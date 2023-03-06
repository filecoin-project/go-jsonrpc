package jsonrpc

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"runtime/pprof"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log/v2"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/propagation"
	"golang.org/x/xerrors"
)

const (
	methodMinRetryDelay = 100 * time.Millisecond
	methodMaxRetryDelay = 10 * time.Minute
)

var (
	errorType   = reflect.TypeOf(new(error)).Elem()
	contextType = reflect.TypeOf(new(context.Context)).Elem()

	log = logging.Logger("rpc")

	_defaultHTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
)

// ErrClient is an error which occurred on the client side the library
type ErrClient struct {
	err error
}

func (e *ErrClient) Error() string {
	return fmt.Sprintf("RPC client error: %s", e.err)
}

// Unwrap unwraps the actual error
func (e *ErrClient) Unwrap() error {
	return e.err
}

type clientResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	ID      interface{}     `json:"id"`
	Error   *respError      `json:"error,omitempty"`
}

type makeChanSink func() (context.Context, func([]byte, bool))

type clientRequest struct {
	req   request
	ready chan clientResponse

	// retCh provides a context and sink for handling incoming channel messages
	retCh makeChanSink
}

// ClientCloser is used to close Client from further use
type ClientCloser func()

// NewClient creates new jsonrpc 2.0 client
//
// handler must be pointer to a struct with function fields
// Returned value closes the client connection
// TODO: Example
func NewClient(ctx context.Context, addr string, namespace string, handler interface{}, requestHeader http.Header) (ClientCloser, error) {
	return NewMergeClient(ctx, addr, namespace, []interface{}{handler}, requestHeader)
}

type client struct {
	namespace     string
	paramEncoders map[reflect.Type]ParamEncoder
	errors        *Errors

	doRequest func(context.Context, clientRequest) (clientResponse, error)
	exiting   <-chan struct{}
	idCtr     int64
}

// NewMergeClient is like NewClient, but allows to specify multiple structs
// to be filled in the same namespace, using one connection
func NewMergeClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, opts ...Option) (ClientCloser, error) {
	config := defaultConfig()
	for _, o := range opts {
		o(&config)
	}

	u, err := url.Parse(addr)
	if err != nil {
		return nil, xerrors.Errorf("parsing address: %w", err)
	}

	switch u.Scheme {
	case "ws", "wss":
		return websocketClient(ctx, addr, namespace, outs, requestHeader, config)
	case "http", "https":
		return httpClient(ctx, addr, namespace, outs, requestHeader, config)
	default:
		return nil, xerrors.Errorf("unknown url scheme '%s'", u.Scheme)
	}

}

func httpClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, config Config) (ClientCloser, error) {
	c := client{
		namespace:     namespace,
		paramEncoders: config.paramEncoders,
		errors:        config.errors,
	}

	stop := make(chan struct{})
	c.exiting = stop

	if requestHeader == nil {
		requestHeader = http.Header{}
	}

	c.doRequest = func(ctx context.Context, cr clientRequest) (clientResponse, error) {
		b, err := json.Marshal(&cr.req)
		if err != nil {
			return clientResponse{}, xerrors.Errorf("marshalling request: %w", err)
		}

		hreq, err := http.NewRequest("POST", addr, bytes.NewReader(b))
		if err != nil {
			return clientResponse{}, &RPCConnectionError{err}
		}

		hreq.Header = requestHeader.Clone()

		if ctx != nil {
			hreq = hreq.WithContext(ctx)
		}

		hreq.Header.Set("Content-Type", "application/json")

		httpResp, err := config.httpClient.Do(hreq)
		if err != nil {
			return clientResponse{}, &RPCConnectionError{err}
		}
		defer httpResp.Body.Close()

		var resp clientResponse
		if cr.req.ID != nil { // non-notification
			if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
				return clientResponse{}, xerrors.Errorf("http status %s unmarshaling response: %w", httpResp.Status, err)
			}

			if resp.ID, err = normalizeID(resp.ID); err != nil {
				return clientResponse{}, xerrors.Errorf("failed to response ID: %w", err)
			}

			if resp.ID != cr.req.ID {
				return clientResponse{}, xerrors.New("request and response id didn't match")
			}
		}

		return resp, nil
	}

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
	}, nil
}

func websocketClient(ctx context.Context, addr string, namespace string, outs []interface{}, requestHeader http.Header, config Config) (ClientCloser, error) {
	connFactory := func() (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial(addr, requestHeader)
		if err != nil {
			return nil, &RPCConnectionError{xerrors.Errorf("cannot dial address %s for %w", addr, err)}
		}
		return conn, nil
	}

	if config.proxyConnFactory != nil {
		// used in tests
		connFactory = config.proxyConnFactory(connFactory)
	}

	conn, err := connFactory()
	if err != nil {
		return nil, err
	}

	if config.noReconnect {
		connFactory = nil
	}

	c := client{
		namespace:     namespace,
		paramEncoders: config.paramEncoders,
		errors:        config.errors,
	}

	requests := c.setupRequestChan()

	stop := make(chan struct{})
	exiting := make(chan struct{})
	c.exiting = exiting

	var hnd reqestHandler
	if len(config.reverseHandlers) > 0 {
		h := makeHandler(defaultServerConfig())
		h.aliasedMethods = config.aliasedHandlerMethods
		for _, reverseHandler := range config.reverseHandlers {
			h.register(reverseHandler.ns, reverseHandler.hnd)
		}
		hnd = h
	}

	wconn := &wsConn{
		conn:             conn,
		connFactory:      connFactory,
		reconnectBackoff: config.reconnectBackoff,
		pingInterval:     config.pingInterval,
		timeout:          config.timeout,
		handler:          hnd,
		requests:         requests,
		stop:             stop,
		exiting:          exiting,
	}

	go func() {
		lbl := pprof.Labels("jrpc-mode", "wsclient", "jrpc-remote", addr, "jrpc-local", conn.LocalAddr().String(), "jrpc-uuid", uuid.New().String())
		pprof.Do(ctx, lbl, func(ctx context.Context) {
			wconn.handleWsConn(ctx)
		})
	}()

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
		<-exiting
	}, nil
}

func (c *client) setupRequestChan() chan clientRequest {
	requests := make(chan clientRequest)

	c.doRequest = func(ctx context.Context, cr clientRequest) (clientResponse, error) {
		select {
		case requests <- cr:
		case <-c.exiting:
			return clientResponse{}, fmt.Errorf("websocket routine exiting")
		}

		var ctxDone <-chan struct{}
		var resp clientResponse

		if ctx != nil {
			ctxDone = ctx.Done()
		}

		// wait for response, handle context cancellation
	loop:
		for {
			select {
			case resp = <-cr.ready:
				break loop
			case <-ctxDone: // send cancel request
				ctxDone = nil

				rp, err := json.Marshal([]param{{v: reflect.ValueOf(cr.req.ID)}})
				if err != nil {
					return clientResponse{}, xerrors.Errorf("marshalling cancel request: %w", err)
				}

				cancelReq := clientRequest{
					req: request{
						Jsonrpc: "2.0",
						Method:  wsCancel,
						Params:  rp,
					},
					ready: make(chan clientResponse, 1),
				}
				select {
				case requests <- cancelReq:
				case <-c.exiting:
					log.Warn("failed to send request cancellation, websocket routing exited")
				}

			}
		}

		return resp, nil
	}

	return requests
}

func (c *client) provide(outs []interface{}) error {
	for _, handler := range outs {
		htyp := reflect.TypeOf(handler)
		if htyp.Kind() != reflect.Ptr {
			return xerrors.New("expected handler to be a pointer")
		}
		typ := htyp.Elem()
		if typ.Kind() != reflect.Struct {
			return xerrors.New("handler should be a struct")
		}

		val := reflect.ValueOf(handler)

		for i := 0; i < typ.NumField(); i++ {
			fn, err := c.makeRpcFunc(typ.Field(i))
			if err != nil {
				return err
			}

			val.Elem().Field(i).Set(fn)
		}
	}

	return nil
}

func (c *client) makeOutChan(ctx context.Context, ftyp reflect.Type, valOut int) (func() reflect.Value, makeChanSink) {
	retVal := reflect.Zero(ftyp.Out(valOut))

	chCtor := func() (context.Context, func([]byte, bool)) {
		// unpack chan type to make sure it's reflect.BothDir
		ctyp := reflect.ChanOf(reflect.BothDir, ftyp.Out(valOut).Elem())
		ch := reflect.MakeChan(ctyp, 0) // todo: buffer?
		retVal = ch.Convert(ftyp.Out(valOut))

		incoming := make(chan reflect.Value, 32)

		// gorotuine to handle buffering of items
		go func() {
			buf := (&list.List{}).Init()

			for {
				front := buf.Front()

				cases := []reflect.SelectCase{
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(ctx.Done()),
					},
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(incoming),
					},
				}

				if front != nil {
					cases = append(cases, reflect.SelectCase{
						Dir:  reflect.SelectSend,
						Chan: ch,
						Send: front.Value.(reflect.Value).Elem(),
					})
				}

				chosen, val, ok := reflect.Select(cases)

				switch chosen {
				case 0:
					ch.Close()
					return
				case 1:
					if ok {
						vvval := val.Interface().(reflect.Value)
						buf.PushBack(vvval)
						if buf.Len() > 1 {
							if buf.Len() > 10 {
								log.Warnw("rpc output message buffer", "n", buf.Len())
							} else {
								log.Debugw("rpc output message buffer", "n", buf.Len())
							}
						}
					} else {
						incoming = nil
					}

				case 2:
					buf.Remove(front)
				}

				if incoming == nil && buf.Len() == 0 {
					ch.Close()
					return
				}
			}
		}()

		return ctx, func(result []byte, ok bool) {
			if !ok {
				close(incoming)
				return
			}

			val := reflect.New(ftyp.Out(valOut).Elem())
			if err := json.Unmarshal(result, val.Interface()); err != nil {
				log.Errorf("error unmarshaling chan response: %s", err)
				return
			}

			if ctx.Err() != nil {
				log.Errorf("got rpc message with cancelled context: %s", ctx.Err())
				return
			}

			select {
			case incoming <- val:
			case <-ctx.Done():
			}
		}
	}

	return func() reflect.Value { return retVal }, chCtor
}

func (c *client) sendRequest(ctx context.Context, req request, chCtor makeChanSink) (clientResponse, error) {
	creq := clientRequest{
		req:   req,
		ready: make(chan clientResponse, 1),

		retCh: chCtor,
	}

	return c.doRequest(ctx, creq)
}

type rpcFunc struct {
	client *client

	ftyp reflect.Type
	name string

	nout   int
	valOut int
	errOut int

	// hasCtx is 1 if the function has a context.Context as its first argument.
	// Used as the number of the first non-context argument.
	hasCtx int

	hasRawParams         bool
	returnValueIsChannel bool

	retry  bool
	notify bool
}

func (fn *rpcFunc) processResponse(resp clientResponse, rval reflect.Value) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = rval
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		if resp.Error != nil {

			out[fn.errOut].Set(resp.Error.val(fn.client.errors))
		}
	}

	return out
}

func (fn *rpcFunc) processError(err error) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = reflect.New(fn.ftyp.Out(fn.valOut)).Elem()
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		out[fn.errOut].Set(reflect.ValueOf(&ErrClient{err}))
	}

	return out
}

func (fn *rpcFunc) handleRpcCall(args []reflect.Value) (results []reflect.Value) {
	var id interface{}
	if !fn.notify {
		id = atomic.AddInt64(&fn.client.idCtr, 1)

		// Prepare the ID to send on the wire.
		// We track int64 ids as float64 in the inflight map (because that's what
		// they'll be decoded to). encoding/json outputs numbers with their minimal
		// encoding, avoding the decimal point when possible, i.e. 3 will never get
		// converted to 3.0.
		var err error
		id, err = normalizeID(id)
		if err != nil {
			return fn.processError(fmt.Errorf("failed to normalize id")) // should probably panic
		}
	}

	var serializedParams json.RawMessage

	if fn.hasRawParams {
		serializedParams = json.RawMessage(args[fn.hasCtx].Interface().(RawParams))
	} else {
		params := make([]param, len(args)-fn.hasCtx)
		for i, arg := range args[fn.hasCtx:] {
			enc, found := fn.client.paramEncoders[arg.Type()]
			if found {
				// custom param encoder
				var err error
				arg, err = enc(arg)
				if err != nil {
					return fn.processError(fmt.Errorf("sendRequest failed: %w", err))
				}
			}

			params[i] = param{
				v: arg,
			}
		}
		var err error
		serializedParams, err = json.Marshal(params)
		if err != nil {
			return fn.processError(fmt.Errorf("marshaling params failed: %w", err))
		}
	}

	var ctx context.Context
	var span *trace.Span
	if fn.hasCtx == 1 {
		ctx = args[0].Interface().(context.Context)
		ctx, span = trace.StartSpan(ctx, "api.call")
		defer span.End()
	}

	retVal := func() reflect.Value { return reflect.Value{} }

	// if the function returns a channel, we need to provide a sink for the
	// messages
	var chCtor makeChanSink
	if fn.returnValueIsChannel {
		retVal, chCtor = fn.client.makeOutChan(ctx, fn.ftyp, fn.valOut)
	}

	req := request{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  fn.name,
		Params:  serializedParams,
	}

	if span != nil {
		span.AddAttributes(trace.StringAttribute("method", req.Method))

		eSC := base64.StdEncoding.EncodeToString(
			propagation.Binary(span.SpanContext()))
		req.Meta = map[string]string{
			"SpanContext": eSC,
		}
	}

	b := backoff{
		maxDelay: methodMaxRetryDelay,
		minDelay: methodMinRetryDelay,
	}

	var err error
	var resp clientResponse
	// keep retrying if got a forced closed websocket conn and calling method
	// has retry annotation
	for attempt := 0; true; attempt++ {
		resp, err = fn.client.sendRequest(ctx, req, chCtor)
		if err != nil {
			return fn.processError(fmt.Errorf("sendRequest failed: %w", err))
		}

		if !fn.notify && resp.ID != req.ID {
			return fn.processError(xerrors.New("request and response id didn't match"))
		}

		if fn.valOut != -1 && !fn.returnValueIsChannel {
			val := reflect.New(fn.ftyp.Out(fn.valOut))

			if resp.Result != nil {
				log.Debugw("rpc result", "type", fn.ftyp.Out(fn.valOut))
				if err := json.Unmarshal(resp.Result, val.Interface()); err != nil {
					log.Warnw("unmarshaling failed", "message", string(resp.Result))
					return fn.processError(xerrors.Errorf("unmarshaling result: %w", err))
				}
			}

			retVal = func() reflect.Value { return val.Elem() }
		}
		retry := resp.Error != nil && resp.Error.Code == eTempWSError && fn.retry
		if !retry {
			break
		}

		time.Sleep(b.next(attempt))
	}

	return fn.processResponse(resp, retVal())
}

const (
	ProxyTagRetry     = "retry"
	ProxyTagNotify    = "notify"
	ProxyTagRPCMethod = "rpc_method"
)

func (c *client) makeRpcFunc(f reflect.StructField) (reflect.Value, error) {
	ftyp := f.Type
	if ftyp.Kind() != reflect.Func {
		return reflect.Value{}, xerrors.New("handler field not a func")
	}

	name := c.namespace + "." + f.Name
	if tag, ok := f.Tag.Lookup(ProxyTagRPCMethod); ok {
		name = tag
	}

	fun := &rpcFunc{
		client: c,
		ftyp:   ftyp,
		name:   name,
		retry:  f.Tag.Get(ProxyTagRetry) == "true",
		notify: f.Tag.Get(ProxyTagNotify) == "true",
	}
	fun.valOut, fun.errOut, fun.nout = processFuncOut(ftyp)

	if fun.valOut != -1 && fun.notify {
		return reflect.Value{}, xerrors.New("notify methods cannot return values")
	}

	fun.returnValueIsChannel = fun.valOut != -1 && ftyp.Out(fun.valOut).Kind() == reflect.Chan

	if ftyp.NumIn() > 0 && ftyp.In(0) == contextType {
		fun.hasCtx = 1
	}
	// note: hasCtx is also the number of the first non-context argument
	if ftyp.NumIn() > fun.hasCtx && ftyp.In(fun.hasCtx) == rtRawParams {
		if ftyp.NumIn() > fun.hasCtx+1 {
			return reflect.Value{}, xerrors.New("raw params can't be mixed with other arguments")
		}
		fun.hasRawParams = true
	}

	return reflect.MakeFunc(ftyp, fun.handleRpcCall), nil
}

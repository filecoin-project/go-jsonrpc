package jsonrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/propagation"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc/metrics"
)

type RawParams json.RawMessage

var rtRawParams = reflect.TypeOf(RawParams{})

// todo is there a better way to tell 'struct with any number of fields'?
func DecodeParams[T any](p RawParams) (T, error) {
	var t T
	err := json.Unmarshal(p, &t)

	// todo also handle list-encoding automagically (json.Unmarshal doesn't do that, does it?)

	return t, err
}

// methodHandler is a handler for a single method
type methodHandler struct {
	paramReceivers []reflect.Type
	nParams        int

	receiver    reflect.Value
	handlerFunc reflect.Value

	hasCtx       int
	hasRawParams bool

	errOut int
	valOut int
}

// Request / response

type request struct {
	Jsonrpc string            `json:"jsonrpc"`
	ID      interface{}       `json:"id,omitempty"`
	Method  string            `json:"method"`
	Params  json.RawMessage   `json:"params"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// Limit request size. Ideally this limit should be specific for each field
// in the JSON request but as a simple defensive measure we just limit the
// entire HTTP body.
// Configured by WithMaxRequestSize.
const DEFAULT_MAX_REQUEST_SIZE = 100 << 20 // 100 MiB

type respError struct {
	Code    ErrorCode       `json:"code"`
	Message string          `json:"message"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

func (e *respError) Error() string {
	if e.Code >= -32768 && e.Code <= -32000 {
		return fmt.Sprintf("RPC error (%d): %s", e.Code, e.Message)
	}
	return e.Message
}

var marshalableRT = reflect.TypeOf(new(marshalable)).Elem()

func (e *respError) val(errors *Errors) reflect.Value {
	if errors != nil {
		t, ok := errors.byCode[e.Code]
		if ok {
			var v reflect.Value
			if t.Kind() == reflect.Ptr {
				v = reflect.New(t.Elem())
			} else {
				v = reflect.New(t)
			}
			if len(e.Meta) > 0 && v.Type().Implements(marshalableRT) {
				_ = v.Interface().(marshalable).UnmarshalJSON(e.Meta)
			}
			if t.Kind() != reflect.Ptr {
				v = v.Elem()
			}
			return v
		}
	}

	return reflect.ValueOf(e)
}

type response struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	ID      interface{} `json:"id"`
	Error   *respError  `json:"error,omitempty"`
}

type handler struct {
	methods map[string]methodHandler
	errors  *Errors

	maxRequestSize int64

	// aliasedMethods contains a map of alias:original method names.
	// These are used as fallbacks if a method is not found by the given method name.
	aliasedMethods map[string]string

	paramDecoders map[reflect.Type]ParamDecoder
}

func makeHandler(sc ServerConfig) *handler {
	return &handler{
		methods: make(map[string]methodHandler),
		errors:  sc.errors,

		aliasedMethods: map[string]string{},
		paramDecoders:  sc.paramDecoders,

		maxRequestSize: sc.maxRequestSize,
	}
}

// Register

func (s *handler) register(namespace string, r interface{}) {
	val := reflect.ValueOf(r)
	// TODO: expect ptr

	for i := 0; i < val.NumMethod(); i++ {
		method := val.Type().Method(i)

		funcType := method.Func.Type()
		hasCtx := 0
		if funcType.NumIn() >= 2 && funcType.In(1) == contextType {
			hasCtx = 1
		}

		hasRawParams := false
		ins := funcType.NumIn() - 1 - hasCtx
		recvs := make([]reflect.Type, ins)
		for i := 0; i < ins; i++ {
			if hasRawParams && i > 0 {
				panic("raw params must be the last parameter")
			}
			if funcType.In(i+1+hasCtx) == rtRawParams {
				hasRawParams = true
			}
			recvs[i] = method.Type.In(i + 1 + hasCtx)
		}

		valOut, errOut, _ := processFuncOut(funcType)

		s.methods[namespace+"."+method.Name] = methodHandler{
			paramReceivers: recvs,
			nParams:        ins,

			handlerFunc: method.Func,
			receiver:    val,

			hasCtx:       hasCtx,
			hasRawParams: hasRawParams,

			errOut: errOut,
			valOut: valOut,
		}
	}
}

// Handle

type rpcErrFunc func(w func(func(io.Writer)), req *request, code ErrorCode, err error)
type chanOut func(reflect.Value, interface{}) error

func (s *handler) handleReader(ctx context.Context, r io.Reader, w io.Writer, rpcError rpcErrFunc) {
	wf := func(cb func(io.Writer)) {
		cb(w)
	}

	// We read the entire request upfront in a buffer to be able to tell if the
	// client sent more than maxRequestSize and report it back as an explicit error,
	// instead of just silently truncating it and reporting a more vague parsing
	// error.
	bufferedRequest := new(bytes.Buffer)
	// We use LimitReader to enforce maxRequestSize. Since it won't return an
	// EOF we can't actually know if the client sent more than the maximum or
	// not, so we read one byte more over the limit to explicitly query that.
	// FIXME: Maybe there's a cleaner way to do this.
	reqSize, err := bufferedRequest.ReadFrom(io.LimitReader(r, s.maxRequestSize+1))
	if err != nil {
		// ReadFrom will discard EOF so any error here is unexpected and should
		// be reported.
		rpcError(wf, nil, rpcParseError, xerrors.Errorf("reading request: %w", err))
		return
	}
	if reqSize > s.maxRequestSize {
		rpcError(wf, nil, rpcParseError,
			// rpcParseError is the closest we have from the standard errors defined
			// in [jsonrpc spec](https://www.jsonrpc.org/specification#error_object)
			// to report the maximum limit.
			xerrors.Errorf("request bigger than maximum %d allowed",
				s.maxRequestSize))
		return
	}

	// Trim spaces to avoid issues with batch request detection.
	bufferedRequest = bytes.NewBuffer(bytes.TrimSpace(bufferedRequest.Bytes()))
	reqSize = int64(bufferedRequest.Len())

	if reqSize == 0 {
		rpcError(wf, nil, rpcInvalidRequest, xerrors.New("Invalid request"))
		return
	}

	if bufferedRequest.Bytes()[0] == '[' && bufferedRequest.Bytes()[reqSize-1] == ']' {
		var reqs []request

		if err := json.NewDecoder(bufferedRequest).Decode(&reqs); err != nil {
			rpcError(wf, nil, rpcParseError, xerrors.New("Parse error"))
			return
		}

		if len(reqs) == 0 {
			rpcError(wf, nil, rpcInvalidRequest, xerrors.New("Invalid request"))
			return
		}

		_, _ = w.Write([]byte("[")) // todo consider handling this error
		for idx, req := range reqs {
			if req.ID, err = normalizeID(req.ID); err != nil {
				rpcError(wf, &req, rpcParseError, xerrors.Errorf("failed to parse ID: %w", err))
				return
			}

			s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)

			if idx != len(reqs)-1 {
				_, _ = w.Write([]byte(",")) // todo consider handling this error
			}
		}
		_, _ = w.Write([]byte("]")) // todo consider handling this error
	} else {
		var req request
		if err := json.NewDecoder(bufferedRequest).Decode(&req); err != nil {
			rpcError(wf, &req, rpcParseError, xerrors.New("Parse error"))
			return
		}

		if req.ID, err = normalizeID(req.ID); err != nil {
			rpcError(wf, &req, rpcParseError, xerrors.Errorf("failed to parse ID: %w", err))
			return
		}

		s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)
	}
}

func doCall(methodName string, f reflect.Value, params []reflect.Value) (out []reflect.Value, err error) {
	defer func() {
		if i := recover(); i != nil {
			err = xerrors.Errorf("panic in rpc method '%s': %s", methodName, i)
			log.Desugar().WithOptions(zap.AddStacktrace(zapcore.ErrorLevel)).Sugar().Error(err)
		}
	}()

	out = f.Call(params)
	return out, nil
}

func (s *handler) getSpan(ctx context.Context, req request) (context.Context, *trace.Span) {
	if req.Meta == nil {
		return ctx, nil
	}

	var span *trace.Span
	if eSC, ok := req.Meta["SpanContext"]; ok {
		bSC := make([]byte, base64.StdEncoding.DecodedLen(len(eSC)))
		_, err := base64.StdEncoding.Decode(bSC, []byte(eSC))
		if err != nil {
			log.Errorf("SpanContext: decode", "error", err)
			return ctx, nil
		}
		sc, ok := propagation.FromBinary(bSC)
		if !ok {
			log.Errorf("SpanContext: could not create span", "data", bSC)
			return ctx, nil
		}
		ctx, span = trace.StartSpanWithRemoteParent(ctx, "api.handle", sc)
	} else {
		ctx, span = trace.StartSpan(ctx, "api.handle")
	}

	span.AddAttributes(trace.StringAttribute("method", req.Method))
	return ctx, span
}

func (s *handler) createError(err error) *respError {
	var code ErrorCode = 1
	if s.errors != nil {
		c, ok := s.errors.byType[reflect.TypeOf(err)]
		if ok {
			code = c
		}
	}

	out := &respError{
		Code:    code,
		Message: err.Error(),
	}

	if m, ok := err.(marshalable); ok {
		meta, err := m.MarshalJSON()
		if err == nil {
			out.Meta = meta
		}
	}

	return out
}

func (s *handler) handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut) {
	// Not sure if we need to sanitize the incoming req.Method or not.
	ctx, span := s.getSpan(ctx, req)
	ctx, _ = tag.New(ctx, tag.Insert(metrics.RPCMethod, req.Method))
	defer span.End()

	handler, ok := s.methods[req.Method]
	if !ok {
		aliasTo, ok := s.aliasedMethods[req.Method]
		if ok {
			handler, ok = s.methods[aliasTo]
		}
		if !ok {
			rpcError(w, &req, rpcMethodNotFound, fmt.Errorf("method '%s' not found", req.Method))
			stats.Record(ctx, metrics.RPCInvalidMethod.M(1))
			done(false)
			return
		}
	}

	outCh := handler.valOut != -1 && handler.handlerFunc.Type().Out(handler.valOut).Kind() == reflect.Chan
	defer done(outCh)

	if chOut == nil && outCh {
		rpcError(w, &req, rpcMethodNotFound, fmt.Errorf("method '%s' not supported in this mode (no out channel support)", req.Method))
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return
	}

	callParams := make([]reflect.Value, 1+handler.hasCtx+handler.nParams)
	callParams[0] = handler.receiver
	if handler.hasCtx == 1 {
		callParams[1] = reflect.ValueOf(ctx)
	}

	if handler.hasRawParams {
		// When hasRawParams is true, there is only one parameter and it is a
		// json.RawMessage.

		callParams[1+handler.hasCtx] = reflect.ValueOf(RawParams(req.Params))
	} else {
		// "normal" param list; no good way to do named params in Golang

		var ps []param
		if len(req.Params) > 0 {
			err := json.Unmarshal(req.Params, &ps)
			if err != nil {
				rpcError(w, &req, rpcParseError, xerrors.Errorf("unmarshaling param array: %w", err))
				stats.Record(ctx, metrics.RPCRequestError.M(1))
				return
			}
		}

		if len(ps) != handler.nParams {
			rpcError(w, &req, rpcInvalidParams, fmt.Errorf("wrong param count (method '%s'): %d != %d", req.Method, len(ps), handler.nParams))
			stats.Record(ctx, metrics.RPCRequestError.M(1))
			done(false)
			return
		}

		for i := 0; i < handler.nParams; i++ {
			var rp reflect.Value

			typ := handler.paramReceivers[i]
			dec, found := s.paramDecoders[typ]
			if !found {
				rp = reflect.New(typ)
				if err := json.NewDecoder(bytes.NewReader(ps[i].data)).Decode(rp.Interface()); err != nil {
					rpcError(w, &req, rpcParseError, xerrors.Errorf("unmarshaling params for '%s' (param: %T): %w", req.Method, rp.Interface(), err))
					stats.Record(ctx, metrics.RPCRequestError.M(1))
					return
				}
				rp = rp.Elem()
			} else {
				var err error
				rp, err = dec(ctx, ps[i].data)
				if err != nil {
					rpcError(w, &req, rpcParseError, xerrors.Errorf("decoding params for '%s' (param: %d; custom decoder): %w", req.Method, i, err))
					stats.Record(ctx, metrics.RPCRequestError.M(1))
					return
				}
			}

			callParams[i+1+handler.hasCtx] = reflect.ValueOf(rp.Interface())
		}
	}

	// /////////////////

	callResult, err := doCall(req.Method, handler.handlerFunc, callParams)
	if err != nil {
		rpcError(w, &req, 0, xerrors.Errorf("fatal error calling '%s': %w", req.Method, err))
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return
	}
	if req.ID == nil {
		return // notification
	}

	// /////////////////

	resp := response{
		Jsonrpc: "2.0",
		ID:      req.ID,
	}

	if handler.errOut != -1 {
		err := callResult[handler.errOut].Interface()
		if err != nil {
			log.Warnf("error in RPC call to '%s': %+v", req.Method, err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))

			resp.Error = s.createError(err.(error))
		}
	}

	var kind reflect.Kind
	var res interface{}
	var nonZero bool
	if handler.valOut != -1 {
		res = callResult[handler.valOut].Interface()
		kind = callResult[handler.valOut].Kind()
		nonZero = !callResult[handler.valOut].IsZero()
	}

	// check error as JSON-RPC spec prohibits error and value at the same time
	if resp.Error == nil {
		if res != nil && kind == reflect.Chan {
			// Channel responses are sent from channel control goroutine.
			// Sending responses here could cause deadlocks on writeLk, or allow
			// sending channel messages before this rpc call returns

			//noinspection GoNilness // already checked above
			err = chOut(callResult[handler.valOut], req.ID)
			if err == nil {
				return // channel goroutine handles responding
			}

			log.Warnf("failed to setup channel in RPC call to '%s': %+v", req.Method, err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))
			resp.Error = &respError{
				Code:    1,
				Message: err.Error(),
			}
		} else {
			resp.Result = res
		}
	}
	if resp.Error != nil && nonZero {
		log.Errorw("error and res returned", "request", req, "r.err", resp.Error, "res", res)
	}

	withLazyWriter(w, func(w io.Writer) {
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Error(err)
			stats.Record(ctx, metrics.RPCResponseError.M(1))
			return
		}
	})
}

// withLazyWriter makes it possible to defer acquiring a writer until the first write.
// This is useful because json.Encode needs to marshal the response fully before writing, which may be
// a problem for very large responses.
func withLazyWriter(withWriterFunc func(func(io.Writer)), cb func(io.Writer)) {
	lw := &lazyWriter{
		withWriterFunc: withWriterFunc,

		done: make(chan struct{}),
	}

	defer close(lw.done)
	cb(lw)
}

type lazyWriter struct {
	withWriterFunc func(func(io.Writer))

	w    io.Writer
	done chan struct{}
}

func (lw *lazyWriter) Write(p []byte) (n int, err error) {
	if lw.w == nil {
		acquired := make(chan struct{})
		go func() {
			lw.withWriterFunc(func(w io.Writer) {
				lw.w = w
				close(acquired)
				<-lw.done
			})
		}()
		<-acquired
	}

	return lw.w.Write(p)
}

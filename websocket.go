package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/xerrors"
)

const wsCancel = "xrpc.cancel"
const chValue = "xrpc.ch.val"
const chClose = "xrpc.ch.close"

var debugTrace = os.Getenv("JSONRPC_ENABLE_DEBUG_TRACE") == "1"

type frame struct {
	// common
	Jsonrpc string            `json:"jsonrpc"`
	ID      interface{}       `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`

	// request
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`

	// response
	Result json.RawMessage `json:"result,omitempty"`
	Error  *respError      `json:"error,omitempty"`
}

type outChanReg struct {
	reqID interface{}

	chID uint64
	ch   reflect.Value
}

type reqestHandler interface {
	handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut)
}

type wsConn struct {
	// outside params
	conn             *websocket.Conn
	connFactory      func() (*websocket.Conn, error)
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration
	handler          reqestHandler
	requests         <-chan clientRequest
	pongs            chan struct{}
	stopPings        func()
	stop             <-chan struct{}
	exiting          chan struct{}

	// incoming messages
	incoming    chan io.Reader
	incomingErr error

	readError chan error

	frameExecQueue chan []byte

	// outgoing messages
	writeLk sync.Mutex

	// ////
	// Client related

	// inflight are requests we've sent to the remote
	inflight   map[interface{}]clientRequest
	inflightLk sync.Mutex

	// chanHandlers is a map of client-side channel handlers
	chanHandlersLk sync.Mutex
	chanHandlers   map[uint64]*chanHandler

	// ////
	// Server related

	// handling are the calls we handle
	handling   map[interface{}]context.CancelFunc
	handlingLk sync.Mutex

	spawnOutChanHandlerOnce sync.Once

	// chanCtr is a counter used for identifying output channels on the server side
	chanCtr uint64

	registerCh chan outChanReg
}

type chanHandler struct {
	// take inside chanHandlersLk
	lk sync.Mutex

	cb func(m []byte, ok bool)
}

//                         //
// WebSocket Message utils //
//                         //

// nextMessage wait for one message and puts it to the incoming channel
func (c *wsConn) nextMessage() {
	c.resetReadDeadline()
	msgType, r, err := c.conn.NextReader()
	if err != nil {
		c.incomingErr = err
		close(c.incoming)
		return
	}
	if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
		c.incomingErr = errors.New("unsupported message type")
		close(c.incoming)
		return
	}
	c.incoming <- r
}

// nextWriter waits for writeLk and invokes the cb callback with WS message
// writer when the lock is acquired
func (c *wsConn) nextWriter(cb func(io.Writer)) {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	wcl, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		log.Error("handle me:", err)
		return
	}

	cb(wcl)

	if err := wcl.Close(); err != nil {
		log.Error("handle me:", err)
		return
	}
}

func (c *wsConn) sendRequest(req request) error {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	if debugTrace {
		log.Debugw("sendRequest", "req", req.Method, "id", req.ID)
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return err
	}
	return nil
}

//                 //
// Output channels //
//                 //

// handleOutChans handles channel communication on the server side
// (forwards channel messages to client)
func (c *wsConn) handleOutChans() {
	regV := reflect.ValueOf(c.registerCh)
	exitV := reflect.ValueOf(c.exiting)

	cases := []reflect.SelectCase{
		{ // registration chan always 0
			Dir:  reflect.SelectRecv,
			Chan: regV,
		},
		{ // exit chan always 1
			Dir:  reflect.SelectRecv,
			Chan: exitV,
		},
	}
	internal := len(cases)
	var caseToID []uint64

	for {
		chosen, val, ok := reflect.Select(cases)

		switch chosen {
		case 0: // registration channel
			if !ok {
				// control channel closed - signals closed connection
				// This shouldn't happen, instead the exiting channel should get closed
				log.Warn("control channel closed")
				return
			}

			registration := val.Interface().(outChanReg)

			caseToID = append(caseToID, registration.chID)
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: registration.ch,
			})

			c.nextWriter(func(w io.Writer) {
				resp := &response{
					Jsonrpc: "2.0",
					ID:      registration.reqID,
					Result:  registration.chID,
				}

				if err := json.NewEncoder(w).Encode(resp); err != nil {
					log.Error(err)
					return
				}
			})

			continue
		case 1: // exiting channel
			if !ok {
				// exiting channel closed - signals closed connection
				//
				// We're not closing any channels as we're on receiving end.
				// Also, context cancellation below should take care of any running
				// requests
				return
			}
			log.Warn("exiting channel received a message")
			continue
		}

		if !ok {
			// Output channel closed, cleanup, and tell remote that this happened

			id := caseToID[chosen-internal]

			n := len(cases) - 1
			if n > 0 {
				cases[chosen] = cases[n]
				caseToID[chosen-internal] = caseToID[n-internal]
			}

			cases = cases[:n]
			caseToID = caseToID[:n-internal]

			rp, err := json.Marshal([]param{{v: reflect.ValueOf(id)}})
			if err != nil {
				log.Error(err)
				continue
			}

			if err := c.sendRequest(request{
				Jsonrpc: "2.0",
				ID:      nil, // notification
				Method:  chClose,
				Params:  rp,
			}); err != nil {
				log.Warnf("closed out channel sendRequest failed: %s", err)
			}
			continue
		}

		// forward message
		rp, err := json.Marshal([]param{{v: reflect.ValueOf(caseToID[chosen-internal])}, {v: val}})
		if err != nil {
			log.Errorw("marshaling params for sendRequest failed", "err", err)
			continue
		}

		if err := c.sendRequest(request{
			Jsonrpc: "2.0",
			ID:      nil, // notification
			Method:  chValue,
			Params:  rp,
		}); err != nil {
			log.Warnf("sendRequest failed: %s", err)
			return
		}
	}
}

// handleChanOut registers output channel for forwarding to client
func (c *wsConn) handleChanOut(ch reflect.Value, req interface{}) error {
	c.spawnOutChanHandlerOnce.Do(func() {
		go c.handleOutChans()
	})
	id := atomic.AddUint64(&c.chanCtr, 1)

	select {
	case c.registerCh <- outChanReg{
		reqID: req,

		chID: id,
		ch:   ch,
	}:
		return nil
	case <-c.exiting:
		return xerrors.New("connection closing")
	}
}

//                          //
// Context.Done propagation //
//                          //

// handleCtxAsync handles context lifetimes for client
// TODO: this should be aware of events going through chanHandlers, and quit
//
//	when the related channel is closed.
//	This should also probably be a single goroutine,
//	Note that not doing this should be fine for now as long as we are using
//	contexts correctly (cancelling when async functions are no longer is use)
func (c *wsConn) handleCtxAsync(actx context.Context, id interface{}) {
	<-actx.Done()

	rp, err := json.Marshal([]param{{v: reflect.ValueOf(id)}})
	if err != nil {
		log.Errorw("marshaling params for sendRequest failed", "err", err)
		return
	}

	if err := c.sendRequest(request{
		Jsonrpc: "2.0",
		Method:  wsCancel,
		Params:  rp,
	}); err != nil {
		log.Warnw("failed to send request", "method", wsCancel, "id", id, "error", err.Error())
	}
}

// cancelCtx is a built-in rpc which handles context cancellation over rpc
func (c *wsConn) cancelCtx(req frame) {
	if req.ID != nil {
		log.Warnf("%s call with ID set, won't respond", wsCancel)
	}

	var params []param
	if err := json.Unmarshal(req.Params, &params); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	var id interface{}
	if err := json.Unmarshal(params[0].data, &id); err != nil {
		log.Error("handle me:", err)
		return
	}

	c.handlingLk.Lock()
	defer c.handlingLk.Unlock()

	cf, ok := c.handling[id]
	if ok {
		cf()
	}
}

//                     //
// Main Handling logic //
//                     //

func (c *wsConn) handleChanMessage(frame frame) {
	var params []param
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	var chid uint64
	if err := json.Unmarshal(params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	c.chanHandlersLk.Lock()
	hnd, ok := c.chanHandlers[chid]
	if !ok {
		c.chanHandlersLk.Unlock()
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	hnd.lk.Lock()
	defer hnd.lk.Unlock()

	c.chanHandlersLk.Unlock()

	hnd.cb(params[1].data, true)
}

func (c *wsConn) handleChanClose(frame frame) {
	var params []param
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	var chid uint64
	if err := json.Unmarshal(params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	c.chanHandlersLk.Lock()
	hnd, ok := c.chanHandlers[chid]
	if !ok {
		c.chanHandlersLk.Unlock()
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	hnd.lk.Lock()
	defer hnd.lk.Unlock()

	delete(c.chanHandlers, chid)

	c.chanHandlersLk.Unlock()

	hnd.cb(nil, false)
}

func (c *wsConn) handleResponse(frame frame) {
	c.inflightLk.Lock()
	req, ok := c.inflight[frame.ID]
	c.inflightLk.Unlock()
	if !ok {
		log.Error("client got unknown ID in response")
		return
	}

	if req.retCh != nil && frame.Result != nil {
		// output is channel
		var chid uint64
		if err := json.Unmarshal(frame.Result, &chid); err != nil {
			log.Errorf("failed to unmarshal channel id response: %s, data '%s'", err, string(frame.Result))
			return
		}

		chanCtx, chHnd := req.retCh()

		c.chanHandlersLk.Lock()
		c.chanHandlers[chid] = &chanHandler{cb: chHnd}
		c.chanHandlersLk.Unlock()

		go c.handleCtxAsync(chanCtx, frame.ID)
	}

	req.ready <- clientResponse{
		Jsonrpc: frame.Jsonrpc,
		Result:  frame.Result,
		ID:      frame.ID,
		Error:   frame.Error,
	}
	c.inflightLk.Lock()
	delete(c.inflight, frame.ID)
	c.inflightLk.Unlock()
}

func (c *wsConn) handleCall(ctx context.Context, frame frame) {
	if c.handler == nil {
		log.Error("handleCall on client with no reverse handler")
		return
	}

	req := request{
		Jsonrpc: frame.Jsonrpc,
		ID:      frame.ID,
		Meta:    frame.Meta,
		Method:  frame.Method,
		Params:  frame.Params,
	}

	ctx, cancel := context.WithCancel(ctx)

	nextWriter := func(cb func(io.Writer)) {
		cb(ioutil.Discard)
	}
	done := func(keepCtx bool) {
		if !keepCtx {
			cancel()
		}
	}
	if frame.ID != nil {
		nextWriter = c.nextWriter

		c.handlingLk.Lock()
		c.handling[frame.ID] = cancel
		c.handlingLk.Unlock()

		done = func(keepctx bool) {
			c.handlingLk.Lock()
			defer c.handlingLk.Unlock()

			if !keepctx {
				cancel()
				delete(c.handling, frame.ID)
			}
		}
	}

	go c.handler.handle(ctx, req, nextWriter, rpcError, done, c.handleChanOut)
}

// handleFrame handles all incoming messages (calls and responses)
func (c *wsConn) handleFrame(ctx context.Context, frame frame) {
	// Get message type by method name:
	// "" - response
	// "xrpc.*" - builtin
	// anything else - incoming remote call
	switch frame.Method {
	case "": // Response to our call
		c.handleResponse(frame)
	case wsCancel:
		c.cancelCtx(frame)
	case chValue:
		c.handleChanMessage(frame)
	case chClose:
		c.handleChanClose(frame)
	default: // Remote call
		c.handleCall(ctx, frame)
	}
}

func (c *wsConn) closeInFlight() {
	c.inflightLk.Lock()
	for id, req := range c.inflight {
		req.ready <- clientResponse{
			Jsonrpc: "2.0",
			ID:      id,
			Error: &respError{
				Message: "handler: websocket connection closed",
				Code:    eTempWSError,
			},
		}
	}
	c.inflight = map[interface{}]clientRequest{}
	c.inflightLk.Unlock()

	c.handlingLk.Lock()
	for _, cancel := range c.handling {
		cancel()
	}
	c.handling = map[interface{}]context.CancelFunc{}
	c.handlingLk.Unlock()

}

func (c *wsConn) closeChans() {
	c.chanHandlersLk.Lock()
	defer c.chanHandlersLk.Unlock()

	for chid := range c.chanHandlers {
		hnd := c.chanHandlers[chid]

		hnd.lk.Lock()

		delete(c.chanHandlers, chid)

		c.chanHandlersLk.Unlock()

		hnd.cb(nil, false)

		hnd.lk.Unlock()
		c.chanHandlersLk.Lock()
	}
}

func (c *wsConn) setupPings() func() {
	if c.pingInterval == 0 {
		return func() {}
	}

	c.conn.SetPongHandler(func(appData string) error {
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})
	c.conn.SetPingHandler(func(appData string) error {
		// treat pings as pongs - this lets us register server activity even if it's too busy to respond to our pings
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-time.After(c.pingInterval):
				c.writeLk.Lock()
				if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					log.Errorf("sending ping message: %+v", err)
				}
				c.writeLk.Unlock()
			case <-stop:
				return
			}
		}
	}()

	var o sync.Once
	return func() {
		o.Do(func() {
			close(stop)
		})
	}
}

// returns true if reconnected
func (c *wsConn) tryReconnect(ctx context.Context) bool {
	if c.connFactory == nil { // server side
		return false
	}

	// connection dropped unexpectedly, do our best to recover it
	c.closeInFlight()
	c.closeChans()
	c.incoming = make(chan io.Reader) // listen again for responses
	go func() {
		c.stopPings()

		attempts := 0
		var conn *websocket.Conn
		for conn == nil {
			time.Sleep(c.reconnectBackoff.next(attempts))
			var err error
			if conn, err = c.connFactory(); err != nil {
				log.Debugw("websocket connection retry failed", "error", err)
			}
			select {
			case <-ctx.Done():
				break
			default:
				continue
			}
			attempts++
		}

		c.writeLk.Lock()
		c.conn = conn
		c.incomingErr = nil

		c.stopPings = c.setupPings()

		c.writeLk.Unlock()

		go c.nextMessage()
	}()

	return true
}

func (c *wsConn) readFrame(ctx context.Context, r io.Reader) {
	// debug util - dump all messages to stderr
	// r = io.TeeReader(r, os.Stderr)

	// json.NewDecoder(r).Decode would read the whole frame as well, so might as well do it
	// with ReadAll which should be much faster
	// use a autoResetReader in case the read takes a long time
	buf, err := io.ReadAll(c.autoResetReader(r)) // todo buffer pool
	if err != nil {
		c.readError <- xerrors.Errorf("reading frame into a buffer: %w", err)
		return
	}

	c.frameExecQueue <- buf
	if len(c.frameExecQueue) > cap(c.frameExecQueue)/2 {
		log.Warnw("frame executor queue is backlogged", "queued", len(c.frameExecQueue), "cap", cap(c.frameExecQueue))
	}

	// got the whole frame, can start reading the next one in background
	go c.nextMessage()
}

func (c *wsConn) frameExecutor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case buf := <-c.frameExecQueue:
			var frame frame
			if err := json.Unmarshal(buf, &frame); err != nil {
				log.Warnw("failed to unmarshal frame", "error", err)
				// todo send invalid request response
				continue
			}

			var err error
			frame.ID, err = normalizeID(frame.ID)
			if err != nil {
				log.Warnw("failed to normalize frame id", "error", err)
				// todo send invalid request response
				continue
			}

			c.handleFrame(ctx, frame)
		}
	}
}

var maxQueuedFrames = 256

func (c *wsConn) handleWsConn(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.incoming = make(chan io.Reader)
	c.readError = make(chan error, 1)
	c.frameExecQueue = make(chan []byte, maxQueuedFrames)
	c.inflight = map[interface{}]clientRequest{}
	c.handling = map[interface{}]context.CancelFunc{}
	c.chanHandlers = map[uint64]*chanHandler{}
	c.pongs = make(chan struct{}, 1)

	c.registerCh = make(chan outChanReg)
	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()

	// setup pings

	c.stopPings = c.setupPings()
	defer c.stopPings()

	var timeoutTimer *time.Timer
	if c.timeout != 0 {
		timeoutTimer = time.NewTimer(c.timeout)
		defer timeoutTimer.Stop()
	}

	// start frame executor
	go c.frameExecutor(ctx)

	// wait for the first message
	go c.nextMessage()
	for {
		var timeoutCh <-chan time.Time
		if timeoutTimer != nil {
			if !timeoutTimer.Stop() {
				select {
				case <-timeoutTimer.C:
				default:
				}
			}
			timeoutTimer.Reset(c.timeout)

			timeoutCh = timeoutTimer.C
		}

		start := time.Now()
		action := ""

		select {
		case r, ok := <-c.incoming:
			action = "incoming"
			err := c.incomingErr

			if ok {
				go c.readFrame(ctx, r)
				break
			}

			if err == nil {
				return // remote closed
			}

			log.Debugw("websocket error", "error", err, "lastAction", action, "time", time.Since(start))
			// only client needs to reconnect
			if !c.tryReconnect(ctx) {
				return // failed to reconnect
			}
		case rerr := <-c.readError:
			action = "read-error"

			log.Debugw("websocket error", "error", rerr, "lastAction", action, "time", time.Since(start))
			if !c.tryReconnect(ctx) {
				return // failed to reconnect
			}
		case req := <-c.requests:
			action = fmt.Sprintf("send-request(%s,%v)", req.req.Method, req.req.ID)

			c.writeLk.Lock()
			if req.req.ID != nil { // non-notification
				if c.incomingErr != nil { // No conn?, immediate fail
					req.ready <- clientResponse{
						Jsonrpc: "2.0",
						ID:      req.req.ID,
						Error: &respError{
							Message: "handler: websocket connection closed",
							Code:    eTempWSError,
						},
					}
					c.writeLk.Unlock()
					break
				}
				c.inflightLk.Lock()
				c.inflight[req.req.ID] = req
				c.inflightLk.Unlock()
			}
			c.writeLk.Unlock()
			serr := c.sendRequest(req.req)
			if serr != nil {
				log.Errorf("sendReqest failed (Handle me): %s", serr)
			}
			if req.req.ID == nil { // notification, return immediately
				resp := clientResponse{
					Jsonrpc: "2.0",
				}
				if serr != nil {
					resp.Error = &respError{
						Code:    eTempWSError,
						Message: fmt.Sprintf("sendRequest: %s", serr),
					}
				}
				req.ready <- resp
			}

		case <-c.pongs:
			action = "pong"

			c.resetReadDeadline()
		case <-timeoutCh:
			if c.pingInterval == 0 {
				// pings not running, this is perfectly normal
				continue
			}

			c.writeLk.Lock()
			if err := c.conn.Close(); err != nil {
				log.Warnw("timed-out websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			log.Errorw("Connection timeout", "remote", c.conn.RemoteAddr(), "lastAction", action)
			// The server side does not perform the reconnect operation, so need to exit
			if c.connFactory == nil {
				return
			}
			// The client performs the reconnect operation, and if it exits it cannot start a handleWsConn again, so it does not need to exit
			continue
		case <-c.stop:
			c.writeLk.Lock()
			cmsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			if err := c.conn.WriteMessage(websocket.CloseMessage, cmsg); err != nil {
				log.Warn("failed to write close message: ", err)
			}
			if err := c.conn.Close(); err != nil {
				log.Warnw("websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			return
		}

		if c.pingInterval > 0 && time.Since(start) > c.pingInterval*2 {
			log.Warnw("websocket long time no response", "lastAction", action, "time", time.Since(start))
		}
		if debugTrace {
			log.Debugw("websocket action", "lastAction", action, "time", time.Since(start))
		}
	}
}

var onReadDeadlineResetInterval = 5 * time.Second

// autoResetReader wraps a reader and resets the read deadline on if needed when doing large reads.
func (c *wsConn) autoResetReader(reader io.Reader) io.Reader {
	return &deadlineResetReader{
		r:     reader,
		reset: c.resetReadDeadline,

		lastReset: time.Now(),
	}
}

type deadlineResetReader struct {
	r     io.Reader
	reset func()

	lastReset time.Time
}

func (r *deadlineResetReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	if time.Since(r.lastReset) > onReadDeadlineResetInterval {
		log.Warnw("slow/large read, resetting deadline while reading the frame", "since", time.Since(r.lastReset), "n", n, "err", err, "p", len(p))

		r.reset()
		r.lastReset = time.Now()
	}
	return
}

func (c *wsConn) resetReadDeadline() {
	if c.timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			log.Error("setting read deadline", err)
		}
	}
}

// Takes an ID as received on the wire, validates it, and translates it to a
// normalized ID appropriate for keying.
func normalizeID(id interface{}) (interface{}, error) {
	switch v := id.(type) {
	case string, float64, nil:
		return v, nil
	case int64: // clients sending int64 need to normalize to float64
		return float64(v), nil
	default:
		return nil, xerrors.Errorf("invalid id type: %T", id)
	}
}

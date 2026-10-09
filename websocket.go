package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/xerrors"
)

const wsCancel = "xrpc.cancel"
const chValue = "xrpc.ch.val"
const chClose = "xrpc.ch.close"

const maxSelectCases = 1 << 16
const defaultMaxSubscriptions = 16384

var debugTrace = os.Getenv("JSONRPC_ENABLE_DEBUG_TRACE") == "1"

type frame struct {
	// common
	Jsonrpc string            `json:"jsonrpc"`
	ID      any               `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`

	// request
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`

	// response
	Result json.RawMessage `json:"result,omitempty"`
	Error  *JSONRPCError   `json:"error,omitempty"`
}

type outChanReg struct {
	reqID any

	chID uint64
	ch   reflect.Value

	accepted chan error
	cleanup  func()
}

type serverRequest struct {
	cancel context.CancelFunc
}

type reqestHandler interface {
	handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut *chanOut)
}

type wsConn struct {
	// outside params
	conn             *websocket.Conn
	connMu           sync.Mutex // protects connection replacement against shutdown
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
	failed           chan struct{}
	failOnce         sync.Once
	maxSubscriptions int
	maxRequestSize   *int64 // nil leaves client reads unlimited

	// incoming messages
	incoming    chan io.Reader
	incomingErr error
	errLk       sync.Mutex

	readError chan error

	frameExecQueue chan []byte

	// outgoing messages
	writeLk sync.Mutex

	// ////
	// Client related

	// inflight are requests we've sent to the remote
	inflight   map[any]clientRequest
	inflightLk sync.Mutex

	// chanHandlers is a map of client-side channel handlers
	chanHandlersLk sync.Mutex
	chanHandlers   map[uint64]*chanHandler

	// ////
	// Server related

	// handling are the calls we handle
	handling   map[any]*serverRequest
	handlingLk sync.Mutex

	spawnOutChanHandlerOnce sync.Once
	// Counts channel-returning calls from invocation through stream cleanup.
	subscriptions atomic.Int64

	// chanCtr is used for identifying output channels on the server side.
	chanCtr atomic.Uint64

	registerCh   chan outChanReg
	outChansDone chan struct{}
}

type chanHandler struct {
	// take inside chanHandlersLk
	lk sync.Mutex

	cb func(m []byte, ok bool)
}

// fail terminates this connection even if another goroutine is blocked in a
// write. Closing the transport must not wait for writeLk.
func (c *wsConn) fail() {
	c.failOnce.Do(func() {
		if c.failed != nil {
			close(c.failed)
		}
		c.closeTransport()
	})
}

func (c *wsConn) closeTransport() {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *wsConn) recoverPanic(activity string) {
	if r := recover(); r != nil {
		c.fail()
		log.Desugar().WithOptions(zap.AddStacktrace(zapcore.ErrorLevel)).Sugar().Errorf("panic in websocket %s: %v", activity, r)
	}
}

func logWebsocketEncodePanic(method string, r any) {
	err := xerrors.Errorf("panic encoding websocket message for '%s': %v", method, r)
	log.Desugar().WithOptions(zap.AddStacktrace(zapcore.ErrorLevel)).Sugar().Error(err)
}

func encodeWebsocketResponse(w io.Writer, resp *response) {
	defer func() {
		if r := recover(); r != nil {
			err := xerrors.Errorf("panic encoding websocket response: %v", r)
			log.Desugar().WithOptions(zap.AddStacktrace(zapcore.ErrorLevel)).Sugar().Error(err)

			fallback := response{
				Jsonrpc: "2.0",
				ID:      resp.ID,
				Error: &JSONRPCError{
					Code:    0,
					Message: err.Error(),
				},
			}
			if err := json.NewEncoder(w).Encode(fallback); err != nil {
				log.Errorw("failed to encode websocket error response", "error", err)
			}
		}
	}()

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error(err)
	}
}

func marshalWebsocketParams(method string, params []param) (out []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			logWebsocketEncodePanic(method, r)
			out = nil
			ok = false
		}
	}()

	out, err := json.Marshal(params)
	if err != nil {
		log.Errorw("failed to marshal websocket params", "method", method, "error", err)
		return nil, false
	}

	return out, true
}

//                         //
// WebSocket Message utils //
//                         //

// nextMessage wait for one message and puts it to the incoming channel
func (c *wsConn) nextMessage() {
	c.resetReadDeadline()
	msgType, r, err := c.conn.NextReader()
	if err != nil {
		c.errLk.Lock()
		c.incomingErr = err
		c.errLk.Unlock()
		close(c.incoming)
		return
	}
	if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
		c.errLk.Lock()
		c.incomingErr = errors.New("unsupported message type")
		c.errLk.Unlock()
		close(c.incoming)
		return
	}
	select {
	case c.incoming <- r:
	case <-c.failed:
	case <-c.exiting:
	}
}

// nextWriter waits for writeLk and invokes the cb callback with a WS message
// writer when the lock is acquired. The callback must always be invoked so
// that callers waiting on synchronization channels are unblocked (see
// lazyWriter). On failure, cb receives io.Discard so the write is harmlessly
// discarded.
func (c *wsConn) nextWriter(cb func(io.Writer)) {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	wcl, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		log.Errorw("websocket NextWriter failed", "error", err)
		cb(io.Discard)
		return
	}

	cb(wcl)

	if err := wcl.Close(); err != nil {
		log.Errorw("websocket writer close failed", "error", err)
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

func (c *wsConn) reserveSubscription(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.failed:
		return nil, xerrors.New("connection closing")
	case <-c.exiting:
		return nil, xerrors.New("connection closing")
	default:
	}
	limit := int64(c.maxSubscriptions)
	for {
		count := c.subscriptions.Load()
		if count >= limit {
			return nil, xerrors.New("subscription limit exceeded")
		}
		if c.subscriptions.CompareAndSwap(count, count+1) {
			var once sync.Once
			return func() {
				once.Do(func() { c.subscriptions.Add(-1) })
			}, nil
		}
	}
}

// handleOutChans handles channel communication on the server side
// (forwards channel messages to client)
func (c *wsConn) handleOutChans() {
	defer c.recoverPanic("output channels")
	defer close(c.outChansDone)
	// An output loop that exits early cannot be restarted safely.
	defer c.fail()
	regV := reflect.ValueOf(c.registerCh)
	exitV := reflect.ValueOf(c.failed)

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
	limit := c.maxSubscriptions
	var registrations []outChanReg
	defer func() {
		for _, registration := range registrations {
			registration.cleanup()
		}
	}()

	for {
		chosen, val, ok := reflect.Select(cases)

		switch chosen {
		case 0: // registration channel
			if !ok {
				// control channel closed - signals closed connection
				// This shouldn't happen; shutdown closes failed instead.
				log.Warn("control channel closed")
				return
			}

			registration := val.Interface().(outChanReg)
			if len(registrations) >= limit {
				registration.accepted <- xerrors.New("subscription limit exceeded")
				continue
			}

			registrations = append(registrations, registration)
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: registration.ch,
			})
			registration.accepted <- nil

			c.nextWriter(func(w io.Writer) {
				resp := &response{
					Jsonrpc: "2.0",
					ID:      registration.reqID,
					Result:  registration.chID,
				}

				encodeWebsocketResponse(w, resp)
			})

			continue
		case 1: // connection shutting down
			return
		}

		if !ok {
			// Output channel closed, cleanup, and tell remote that this happened

			registration := registrations[chosen-internal]
			id := registration.chID

			n := len(cases) - 1
			if n > 0 {
				cases[chosen] = cases[n]
				registrations[chosen-internal] = registrations[n-internal]
			}

			cases[n] = reflect.SelectCase{}
			registrations[n-internal] = outChanReg{}
			cases = cases[:n]
			registrations = registrations[:n-internal]
			registration.cleanup()

			rp, ok := marshalWebsocketParams(chClose, []param{{v: reflect.ValueOf(id)}})
			if !ok {
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
		rp, ok := marshalWebsocketParams(chValue, []param{{v: reflect.ValueOf(registrations[chosen-internal].chID)}, {v: val}})
		if !ok {
			continue
		}

		if err := c.sendRequest(request{
			Jsonrpc: "2.0",
			ID:      nil, // notification
			Method:  chValue,
			Params:  rp,
		}); err != nil {
			log.Warnf("sendRequest failed: %s", err)
			if c.connFactory == nil {
				return
			}
			// A reconnecting client still needs this loop for reverse calls.
			// Reconnect cancels the old requests, allowing their cases to close.
		}
	}
}

// handleChanOut registers output channel for forwarding to client and the cleanup function for the request
func (c *wsConn) handleChanOut(ch reflect.Value, req any, cleanup func()) error {
	c.spawnOutChanHandlerOnce.Do(func() {
		go c.handleOutChans()
	})
	id := c.chanCtr.Add(1)
	accepted := make(chan error, 1)

	select {
	case c.registerCh <- outChanReg{
		reqID: req,

		chID:     id,
		ch:       ch,
		accepted: accepted,
		cleanup:  cleanup,
	}:
		// After handoff, wait for the ownership decision or completed cleanup.
		// Returning on connection shutdown alone could release an active slot.
		select {
		case err := <-accepted:
			return err
		case <-c.outChansDone:
		}
	case <-c.outChansDone:
	case <-c.failed:
	case <-c.exiting:
	}
	return xerrors.New("connection closing")
}

//                          //
// Context.Done propagation //
//                          //

// handleCtxAsync forwards client cancellation until the stream or connection ends.
func (c *wsConn) handleCtxAsync(actx context.Context, id any) {
	if actx == nil {
		return
	}

	select {
	case <-actx.Done():
	case <-c.failed:
		return
	case <-c.exiting:
		return
	}
	if context.Cause(actx) == errChannelClosed {
		return
	}

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

func wsControlParams(method string, raw json.RawMessage, min int) ([]param, bool) {
	var params []param
	if err := json.Unmarshal(raw, &params); err != nil {
		log.Warnw("failed to unmarshal websocket control params", "method", method, "error", err)
		return nil, false
	}

	if len(params) < min {
		log.Warnw("invalid websocket control params", "method", method, "params", len(params), "min", min)
		return nil, false
	}

	return params, true
}

func wsControlValue[T any](method, name string, raw []byte) (T, bool) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Warnw("failed to unmarshal websocket control value", "method", method, "value", name, "error", err)
		return out, false
	}

	return out, true
}

// cancelCtx is a built-in rpc which handles context cancellation over rpc
func (c *wsConn) cancelCtx(req frame) {
	if req.ID != nil {
		log.Warnf("%s call with ID set, won't respond", wsCancel)
	}

	params, ok := wsControlParams(wsCancel, req.Params, 1)
	if !ok {
		return
	}
	id, ok := wsControlValue[any](wsCancel, "id", params[0].data)
	if !ok {
		return
	}
	id, err := normalizeID(id)
	if err != nil {
		log.Warnw("invalid websocket control id", "method", wsCancel, "error", err)
		return
	}

	c.handlingLk.Lock()
	call, ok := c.handling[id]
	c.handlingLk.Unlock()
	if ok {
		call.cancel()
	}
}

//                     //
// Main Handling logic //
//                     //

func (c *wsConn) handleChanMessage(frame frame) {
	params, ok := wsControlParams(chValue, frame.Params, 2)
	if !ok {
		return
	}

	chid, ok := wsControlValue[uint64](chValue, "channel", params[0].data)
	if !ok {
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
	params, ok := wsControlParams(chClose, frame.Params, 1)
	if !ok {
		return
	}

	chid, ok := wsControlValue[uint64](chClose, "channel", params[0].data)
	if !ok {
		return
	}

	c.chanHandlersLk.Lock()
	hnd, ok := c.chanHandlers[chid]
	if !ok {
		c.chanHandlersLk.Unlock()
		log.Errorf("xrpc.ch.close: handler %d not found", chid)
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

	var chid uint64
	var chanCtx context.Context
	var chHnd func([]byte, bool)
	if req.retCh != nil && frame.Result != nil {
		// output is channel
		if err := json.Unmarshal(frame.Result, &chid); err != nil {
			log.Errorf("failed to unmarshal channel id response: %s, data '%s'", err, string(frame.Result))
			return
		}

		chanCtx, chHnd = req.retCh()
	}

	// Channel construction can overlap reconnect cleanup. Keep the request
	// check and handler publication together so cleanup cannot pass between them.
	c.inflightLk.Lock()
	current, ok := c.inflight[frame.ID]
	if !ok || current.ready != req.ready {
		c.inflightLk.Unlock()
		if chHnd != nil {
			chHnd(nil, false)
		}
		return
	}

	if chHnd != nil {
		c.chanHandlersLk.Lock()
		select {
		case <-c.failed:
			c.chanHandlersLk.Unlock()
			c.inflightLk.Unlock()
			chHnd(nil, false)
			return
		default:
		}
		c.chanHandlers[chid] = &chanHandler{cb: chHnd}
		c.chanHandlersLk.Unlock()
	}

	req.respond(clientResponse{
		Jsonrpc: frame.Jsonrpc,
		Result:  frame.Result,
		ID:      frame.ID,
		Error:   frame.Error,
	})
	delete(c.inflight, frame.ID)
	c.inflightLk.Unlock()

	if chHnd != nil {
		go c.handleCtxAsync(chanCtx, frame.ID)
	}
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

	// Cleanup after handling the request
	ctx, cancel := context.WithCancel(ctx)
	call := &serverRequest{cancel: cancel}
	var release func()
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			cancel()
			if release != nil {
				release()
			}
			if frame.ID != nil {
				c.handlingLk.Lock()
				if c.handling[frame.ID] == call {
					delete(c.handling, frame.ID)
				}
				c.handlingLk.Unlock()
			}
		})
	}

	nextWriter := func(cb func(io.Writer)) {
		cb(io.Discard)
	}
	done := func(keepCtx bool) {
		if !keepCtx {
			finish()
		}
	}
	if frame.ID != nil {
		nextWriter = c.nextWriter

		c.handlingLk.Lock()
		select {
		case <-c.failed:
			c.handlingLk.Unlock()
			cancel()
			return
		default:
		}
		previous := c.handling[frame.ID]
		c.handling[frame.ID] = call
		c.handlingLk.Unlock()
		// If caller accidentally used the same ID
		if previous != nil {
			previous.cancel()
		}
	}

	go func() {
		defer c.recoverPanic("request handler")
		c.handler.handle(ctx, req, nextWriter, rpcError, done, &chanOut{
			reserve: func() error {
				var err error
				release, err = c.reserveSubscription(ctx)
				return err
			},
			register: func(ch reflect.Value, reqID any) error {
				return c.handleChanOut(ch, reqID, finish)
			},
		})
	}()
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
	inflight := c.inflight
	c.inflight = map[any]clientRequest{}
	c.inflightLk.Unlock()
	for id, req := range inflight {
		req.respond(clientResponse{
			Jsonrpc: "2.0",
			ID:      id,
			Error: &JSONRPCError{
				Message: "handler: websocket connection closed",
				Code:    eTempWSError,
			},
		})
	}

	c.handlingLk.Lock()
	handling := c.handling
	c.handling = map[any]*serverRequest{}
	c.handlingLk.Unlock()
	for _, call := range handling {
		call.cancel()
	}
}

// ready is buffered; a response racing with shutdown must not block teardown.
func (req clientRequest) respond(resp clientResponse) {
	select {
	case req.ready <- resp:
	default:
	}
}

func (c *wsConn) closeChans() {
	c.chanHandlersLk.Lock()
	handlers := c.chanHandlers
	c.chanHandlers = map[uint64]*chanHandler{}
	c.chanHandlersLk.Unlock()
	for _, hnd := range handlers {
		func() {
			hnd.lk.Lock()
			defer hnd.lk.Unlock()
			hnd.cb(nil, false)
		}()
	}
}

func (c *wsConn) setupPings() func() {
	c.conn.SetPongHandler(func(appData string) error {
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})
	pingHandler := c.conn.PingHandler()
	c.conn.SetPingHandler(func(appData string) error {
		// Record activity before delegating to the websocket ping handler. This
		// preserves the historical "treat pings as pongs" behavior while still
		// replying with a protocol-level pong.
		select {
		case c.pongs <- struct{}{}:
		default:
		}

		return pingHandler(appData)
	})

	if c.pingInterval == 0 {
		return func() {}
	}

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-time.After(c.pingInterval):
				func() {
					c.writeLk.Lock()
					defer c.writeLk.Unlock()
					if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
						log.Errorf("sending ping message: %+v", err)
					}
				}()
			case <-stop:
				return
			case <-c.failed:
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
	select {
	case <-c.failed:
		return false
	case <-ctx.Done():
		return false
	default:
	}
	if c.connFactory == nil { // server side
		return false
	}

	// connection dropped unexpectedly, do our best to recover it
	// Close the previous transport before retrying, including any pending writes.
	c.closeTransport()
	c.closeInFlight()
	c.closeChans()
	select {
	case <-c.failed:
		return false
	default:
	}
	c.incoming = make(chan io.Reader) // listen again for responses
	go func() {
		c.stopPings()

		attempts := 0
		var conn *websocket.Conn
		for conn == nil {
			timer := time.NewTimer(c.reconnectBackoff.next(attempts))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			case <-c.failed:
				timer.Stop()
				return
			}
			var err error
			if conn, err = c.connFactory(); err != nil {
				log.Debugw("websocket connection retry failed", "error", err)
			}
			select {
			case <-ctx.Done():
				if conn != nil {
					_ = conn.Close()
				}
				return
			case <-c.failed:
				if conn != nil {
					_ = conn.Close()
				}
				return
			default:
			}
			attempts++
		}

		func() {
			c.writeLk.Lock()
			defer c.writeLk.Unlock()
			c.connMu.Lock()
			select {
			case <-c.failed:
				c.connMu.Unlock()
				_ = conn.Close()
				return
			case <-ctx.Done():
				c.connMu.Unlock()
				_ = conn.Close()
				return
			default:
			}
			c.conn = conn
			c.connMu.Unlock()
			c.errLk.Lock()
			c.incomingErr = nil
			c.errLk.Unlock()

			c.stopPings = c.setupPings()
			go c.nextMessage()
		}()
	}()

	return true
}

func (c *wsConn) readFrame(ctx context.Context, r io.Reader) {
	// debug util - dump all messages to stderr
	// r = io.TeeReader(r, os.Stderr)

	// json.NewDecoder(r).Decode would read the whole frame as well, so might as well do it
	// with ReadAll which should be much faster
	// use a autoResetReader in case the read takes a long time
	reader := c.autoResetReader(r)
	if c.maxRequestSize != nil && *c.maxRequestSize < math.MaxInt64 {
		// Read one extra byte to distinguish a full frame from an oversized one.
		reader = io.LimitReader(reader, *c.maxRequestSize+1)
	}
	buf, err := io.ReadAll(reader) // todo buffer pool
	if err == nil && c.maxRequestSize != nil && int64(len(buf)) > *c.maxRequestSize {
		err = xerrors.Errorf("websocket frame exceeds maximum size of %d bytes", *c.maxRequestSize)
	}
	if err != nil {
		select {
		case c.readError <- xerrors.Errorf("reading frame into a buffer: %w", err):
		case <-ctx.Done():
		case <-c.failed:
		}
		return
	}

	select {
	case c.frameExecQueue <- buf:
	case <-ctx.Done():
		return
	case <-c.failed:
		return
	}
	if len(c.frameExecQueue) > 2*cap(c.frameExecQueue)/3 { // warn at 2/3 capacity
		log.Warnw("frame executor queue is backlogged", "queued", len(c.frameExecQueue), "cap", cap(c.frameExecQueue))
	}

	// got the whole frame, can start reading the next one in background
	go c.nextMessage()
}

func (c *wsConn) frameExecutor(ctx context.Context) {
	defer c.recoverPanic("frame executor")
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.failed:
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

	if c.maxSubscriptions <= 0 {
		c.maxSubscriptions = defaultMaxSubscriptions
	}
	// Reserve two select cases for registration and connection shutdown.
	if c.maxSubscriptions > maxSelectCases-2 {
		c.maxSubscriptions = maxSelectCases - 2
	}

	c.incoming = make(chan io.Reader)
	c.readError = make(chan error, 1)
	c.frameExecQueue = make(chan []byte, maxQueuedFrames)
	c.inflight = map[any]clientRequest{}
	c.handling = map[any]*serverRequest{}
	c.chanHandlers = map[uint64]*chanHandler{}
	c.pongs = make(chan struct{}, 1)

	c.registerCh = make(chan outChanReg)
	c.outChansDone = make(chan struct{})
	c.failed = make(chan struct{})
	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()
	defer func() {
		cancel()
		c.fail()
	}()

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
		case <-c.failed:
			return
		case r, ok := <-c.incoming:
			action = "incoming"
			c.errLk.Lock()
			err := c.incomingErr
			c.errLk.Unlock()

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
		case <-ctx.Done():
			log.Debugw("context cancelled", "error", ctx.Err(), "lastAction", action, "time", time.Since(start))
			return
		case req := <-c.requests:
			action = fmt.Sprintf("send-request(%s,%v)", req.req.Method, req.req.ID)

			ready := func() bool {
				c.writeLk.Lock()
				defer c.writeLk.Unlock()
				if req.req.ID == nil {
					return true
				}
				c.errLk.Lock()
				hasErr := c.incomingErr != nil
				c.errLk.Unlock()
				if hasErr { // No conn?, immediate fail
					req.respond(clientResponse{
						Jsonrpc: "2.0",
						ID:      req.req.ID,
						Error: &JSONRPCError{
							Message: "handler: websocket connection closed",
							Code:    eTempWSError,
						},
					})
					return false
				}
				c.inflightLk.Lock()
				defer c.inflightLk.Unlock()
				c.inflight[req.req.ID] = req
				return true
			}()
			if !ready {
				break
			}
			serr := c.sendRequest(req.req)
			if serr != nil {
				log.Errorw("sendRequest failed", "method", req.req.Method, "id", req.req.ID, "error", serr)

				if req.req.ID != nil {
					c.inflightLk.Lock()
					delete(c.inflight, req.req.ID)
					c.inflightLk.Unlock()
				}

				req.respond(clientResponse{
					Jsonrpc: "2.0",
					ID:      req.req.ID,
					Error: &JSONRPCError{
						Code:    eTempWSError,
						Message: fmt.Sprintf("sendRequest: %s", serr),
					},
				})
				break
			}
			if req.req.ID == nil { // notification, return immediately
				req.respond(clientResponse{
					Jsonrpc: "2.0",
				})
			}

		case <-c.pongs:
			action = "pong"

			c.resetReadDeadline()
		case <-timeoutCh:
			if c.pingInterval == 0 {
				// pings not running, this is perfectly normal
				continue
			}

			c.closeTransport()
			log.Errorw("Connection timeout", "lastAction", action)
			// The server side does not perform the reconnect operation, so need to exit
			if c.connFactory == nil {
				return
			}
			// The client performs the reconnect operation, and if it exits it cannot start a handleWsConn again, so it does not need to exit
			continue
		case <-c.stop:
			c.connMu.Lock()
			conn := c.conn
			c.connMu.Unlock()
			cmsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			if err := conn.WriteControl(websocket.CloseMessage, cmsg, time.Now().Add(time.Second)); err != nil {
				log.Warn("failed to write close message: ", err)
			}
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
func normalizeID(id any) (any, error) {
	switch v := id.(type) {
	case string, float64, nil:
		return v, nil
	case int64: // clients sending int64 need to normalize to float64
		return float64(v), nil
	default:
		return nil, xerrors.Errorf("invalid id type: %T", id)
	}
}

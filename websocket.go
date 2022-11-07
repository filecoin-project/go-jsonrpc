package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
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

type frame struct {
	// common
	Jsonrpc string            `json:"jsonrpc"`
	ID      interface{}       `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`

	// request
	Method string  `json:"method,omitempty"`
	Params []param `json:"params,omitempty"`

	// response
	Result json.RawMessage `json:"result,omitempty"`
	Error  *respError      `json:"error,omitempty"`
}

type outChanReg struct {
	reqID interface{}

	chID uint64
	ch   reflect.Value
}

type wsConn struct {
	// outside params
	conn             *websocket.Conn
	connFactory      func() (*websocket.Conn, error)
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration
	handler          *RPCServer
	requests         <-chan clientRequest
	pongs            chan struct{}
	stopPings        func()
	stop             <-chan struct{}
	exiting          chan struct{}

	// incoming messages
	incoming    chan io.Reader
	incomingErr error

	// outgoing messages
	writeLk sync.Mutex

	// ////
	// Client related

	// inflight are requests we've sent to the remote
	inflight map[interface{}]clientRequest

	// chanHandlers is a map of client-side channel handlers
	chanHandlers map[uint64]func(m []byte, ok bool)

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

//                         //
// WebSocket Message utils //
//                         //

// nextMessage wait for one message and puts it to the incoming channel
func (c *wsConn) nextMessage() {
	if c.timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			log.Error("setting read deadline", err)
		}
	}
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

			if err := c.sendRequest(request{
				Jsonrpc: "2.0",
				ID:      nil, // notification
				Method:  chClose,
				Params:  []param{{v: reflect.ValueOf(id)}},
			}); err != nil {
				log.Warnf("closed out channel sendRequest failed: %s", err)
			}
			continue
		}

		// forward message
		if err := c.sendRequest(request{
			Jsonrpc: "2.0",
			ID:      nil, // notification
			Method:  chValue,
			Params:  []param{{v: reflect.ValueOf(caseToID[chosen-internal])}, {v: val}},
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
//  when the related channel is closed.
//  This should also probably be a single goroutine,
//  Note that not doing this should be fine for now as long as we are using
//  contexts correctly (cancelling when async functions are no longer is use)
func (c *wsConn) handleCtxAsync(actx context.Context, id interface{}) {
	<-actx.Done()

	if err := c.sendRequest(request{
		Jsonrpc: "2.0",
		Method:  wsCancel,
		Params:  []param{{v: reflect.ValueOf(id)}},
	}); err != nil {
		log.Warnw("failed to send request", "method", wsCancel, "id", id, "error", err.Error())
	}
}

// cancelCtx is a built-in rpc which handles context cancellation over rpc
func (c *wsConn) cancelCtx(req frame) {
	if req.ID != nil {
		log.Warnf("%s call with ID set, won't respond", wsCancel)
	}

	var id interface{}
	if err := json.Unmarshal(req.Params[0].data, &id); err != nil {
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
	var chid uint64
	if err := json.Unmarshal(frame.Params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	hnd, ok := c.chanHandlers[chid]
	if !ok {
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	hnd(frame.Params[1].data, true)
}

func (c *wsConn) handleChanClose(frame frame) {
	var chid uint64
	if err := json.Unmarshal(frame.Params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	hnd, ok := c.chanHandlers[chid]
	if !ok {
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	delete(c.chanHandlers, chid)

	hnd(nil, false)
}

func (c *wsConn) handleResponse(frame frame) {
	req, ok := c.inflight[frame.ID]
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

		var chanCtx context.Context
		chanCtx, c.chanHandlers[chid] = req.retCh()
		go c.handleCtxAsync(chanCtx, frame.ID)
	}

	req.ready <- clientResponse{
		Jsonrpc: frame.Jsonrpc,
		Result:  frame.Result,
		ID:      frame.ID,
		Error:   frame.Error,
	}
	delete(c.inflight, frame.ID)
}

func (c *wsConn) handleCall(ctx context.Context, frame frame) {
	if c.handler == nil {
		log.Error("handleCall on client")
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

	c.handlingLk.Lock()
	for _, cancel := range c.handling {
		cancel()
	}
	c.handlingLk.Unlock()

	c.inflight = map[interface{}]clientRequest{}
	c.handling = map[interface{}]context.CancelFunc{}
}

func (c *wsConn) closeChans() {
	for chid := range c.chanHandlers {
		hnd := c.chanHandlers[chid]
		delete(c.chanHandlers, chid)
		hnd(nil, false)
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

func (c *wsConn) handleWsConn(ctx context.Context) {
	c.incoming = make(chan io.Reader)
	c.inflight = map[interface{}]clientRequest{}
	c.handling = map[interface{}]context.CancelFunc{}
	c.chanHandlers = map[uint64]func(m []byte, ok bool){}
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

		select {
		case r, ok := <-c.incoming:
			err := c.incomingErr

			if ok {
				// debug util - dump all messages to stderr
				// r = io.TeeReader(r, os.Stderr)

				var frame frame
				if err = json.NewDecoder(r).Decode(&frame); err == nil {
					if frame.ID, err = normalizeID(frame.ID); err == nil {
						c.handleFrame(ctx, frame)
						go c.nextMessage()
						continue
					}
				}
			}

			if err == nil {
				return // remote closed
			}

			log.Debugw("websocket error", "error", err)
			// only client needs to reconnect
			if !c.tryReconnect(ctx) {
				return // failed to reconnect
			}
		case req := <-c.requests:
			c.writeLk.Lock()
			if req.req.ID != nil {
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
				c.inflight[req.req.ID] = req
			}
			c.writeLk.Unlock()
			if err := c.sendRequest(req.req); err != nil {
				log.Errorf("sendReqest failed (Handle me): %s", err)
			}
		case <-c.pongs:
			if c.timeout > 0 {
				if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
					log.Error("setting read deadline", err)
				}
			}
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
			log.Errorw("Connection timeout", "remote", c.conn.RemoteAddr())
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

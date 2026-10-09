package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type subscriptionTestCall struct {
	name   string
	ctx    context.Context
	values chan int
}

func (c *subscriptionTestCall) send(t *testing.T, value int) {
	t.Helper()
	select {
	case c.values <- value:
	case <-time.After(5 * time.Second):
		t.Fatal("output loop did not receive the event")
	}
}

type subscriptionTestService struct {
	calls chan *subscriptionTestCall
	gate  <-chan struct{}
}

func (s *subscriptionTestService) Subscribe(ctx context.Context, name string) <-chan int {
	call := &subscriptionTestCall{name: name, ctx: ctx, values: make(chan int)}
	s.calls <- call
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
		}
	}
	// Tests close the producer explicitly, including after cancellation, so
	// they can distinguish cancelling a request from releasing its select case.
	return call.values
}

func (*subscriptionTestService) Ping() string { return "pong" }

func (*subscriptionTestService) SubscribeError() (<-chan int, error) {
	return nil, errors.New("subscription unavailable")
}

type subscriptionTestHandler struct {
	reqestHandler
	finished chan any
	entered  chan subscriptionTestRequest
	writing  chan any
}

type subscriptionTestRequest struct {
	id  any
	ctx context.Context
}

func (h *subscriptionTestHandler) handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(bool), chOut *chanOut) {
	h.entered <- subscriptionTestRequest{id: req.ID, ctx: ctx}
	h.reqestHandler.handle(ctx, req, func(cb func(io.Writer)) {
		// Observe an error response before it waits for the transport lock.
		h.writing <- req.ID
		w(cb)
	}, rpcError, done, chOut)
	// The response may arrive before deferred request cleanup. A completion
	// signal lets tests inspect the handling map after that cleanup finishes.
	h.finished <- req.ID
}

type subscriptionTestConnection struct {
	client   *websocket.Conn
	server   *wsConn
	finished <-chan any
	entered  <-chan subscriptionTestRequest
	writing  <-chan any
}

type subscriptionTestServer struct {
	url         string
	service     *subscriptionTestService
	connections chan subscriptionTestConnection
}

func newSubscriptionTestServer(t *testing.T, limit int, gate <-chan struct{}) *subscriptionTestServer {
	t.Helper()
	h := &subscriptionTestServer{
		service:     &subscriptionTestService{calls: make(chan *subscriptionTestCall, 64), gate: gate},
		connections: make(chan subscriptionTestConnection, 4),
	}
	s := NewServer(WithMaxSubscriptions(limit), WithServerPingInterval(0), func(config *ServerConfig) {
		config.reverseClientBuilder = func(ctx context.Context, conn *wsConn) (context.Context, error) {
			handler := &subscriptionTestHandler{
				reqestHandler: conn.handler,
				finished:      make(chan any, 64),
				entered:       make(chan subscriptionTestRequest, 64),
				writing:       make(chan any, 64),
			}
			conn.handler = handler
			h.connections <- subscriptionTestConnection{
				server: conn, finished: handler.finished, entered: handler.entered, writing: handler.writing,
			}
			return ctx, nil
		}
	})
	s.Register("Subscriptions", h.service)
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	h.url = "ws" + strings.TrimPrefix(server.URL, "http")
	return h
}

func (h *subscriptionTestServer) connect(t *testing.T) subscriptionTestConnection {
	t.Helper()
	client, _, err := websocket.DefaultDialer.Dial(h.url, nil)
	require.NoError(t, err)
	conn := subscriptionTestReceive(t, h.connections)
	conn.client = client
	t.Cleanup(func() {
		_ = client.Close()
		subscriptionTestReceive(t, conn.server.exiting)
	})
	return conn
}

func subscriptionTestReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	var value T
	select {
	case value = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for subscription lifecycle event")
	}
	return value
}

func (c subscriptionTestConnection) send(t *testing.T, id any, method string, params ...any) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	require.NoError(t, c.client.SetWriteDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, c.client.WriteJSON(frame{Jsonrpc: "2.0", ID: id, Method: method, Params: raw}))
}

func (c subscriptionTestConnection) read(t *testing.T) frame {
	t.Helper()
	require.NoError(t, c.client.SetReadDeadline(time.Now().Add(5*time.Second)))
	var f frame
	require.NoError(t, c.client.ReadJSON(&f))
	return f
}

func (c subscriptionTestConnection) handlingIDs() []string {
	c.server.handlingLk.Lock()
	defer c.server.handlingLk.Unlock()
	ids := make([]string, 0, len(c.server.handling))
	for id := range c.server.handling {
		ids = append(ids, id.(string))
	}
	return ids
}

func (h *subscriptionTestServer) subscribe(t *testing.T, c subscriptionTestConnection, id string) (*subscriptionTestCall, frame) {
	t.Helper()
	c.send(t, id, "Subscriptions.Subscribe", id)
	require.Equal(t, id, subscriptionTestReceive(t, c.entered).id)
	call := subscriptionTestReceive(t, h.service.calls)
	require.Equal(t, id, call.name)
	resp := c.read(t)
	require.Equal(t, id, resp.ID)
	require.Equal(t, id, subscriptionTestReceive(t, c.finished))
	return call, resp
}

func subscriptionTestChannelID(t *testing.T, resp frame) uint64 {
	t.Helper()
	require.Nil(t, resp.Error)
	var id uint64
	require.NoError(t, json.Unmarshal(resp.Result, &id))
	require.NotZero(t, id)
	return id
}

func subscriptionTestRejected(t *testing.T, resp frame) {
	t.Helper()
	require.NotNil(t, resp.Error)
	require.Equal(t, ErrorCode(1), resp.Error.Code)
	require.Equal(t, "subscription limit exceeded", resp.Error.Message)
	require.Empty(t, resp.Result)
}

func (h *subscriptionTestServer) reject(t *testing.T, c subscriptionTestConnection, id string) {
	t.Helper()
	c.send(t, id, "Subscriptions.Subscribe", id)
	entry := subscriptionTestReceive(t, c.entered)
	require.Equal(t, id, entry.id)
	resp := c.read(t)
	require.Equal(t, id, resp.ID)
	subscriptionTestRejected(t, resp)
	require.Equal(t, id, subscriptionTestReceive(t, c.finished))
	subscriptionTestReceive(t, entry.ctx.Done())
	require.Empty(t, h.service.calls, "rejected subscriptions must not invoke the producer")
}

func subscriptionTestNotification(t *testing.T, resp frame, method string, params string) {
	t.Helper()
	require.Nil(t, resp.ID)
	require.Equal(t, method, resp.Method)
	require.JSONEq(t, params, string(resp.Params))
}

func TestWebsocketSubscriptionLimitAndCleanup(t *testing.T) {
	h := newSubscriptionTestServer(t, 2, nil)
	c := h.connect(t)
	first, resp := h.subscribe(t, c, "first")
	firstID := subscriptionTestChannelID(t, resp)
	second, resp := h.subscribe(t, c, "second")
	subscriptionTestChannelID(t, resp)
	require.NoError(t, first.ctx.Err())
	require.NoError(t, second.ctx.Err())

	for i := 0; i < 3; i++ {
		h.reject(t, c, fmt.Sprintf("rejected-%d", i))
		require.ElementsMatch(t, []string{"first", "second"}, c.handlingIDs())
	}

	// Exhausting one connection's capacity leaves other connections usable.
	other := h.connect(t)
	independent, resp := h.subscribe(t, other, "independent")
	subscriptionTestChannelID(t, resp)
	require.NoError(t, independent.ctx.Err())

	c.send(t, "ping", "Subscriptions.Ping")
	require.Equal(t, "ping", subscriptionTestReceive(t, c.entered).id)
	resp = c.read(t)
	require.Equal(t, "ping", resp.ID)
	require.Nil(t, resp.Error)
	require.JSONEq(t, `"pong"`, string(resp.Result))
	require.Equal(t, "ping", subscriptionTestReceive(t, c.finished))

	first.send(t, 17)
	subscriptionTestNotification(t, c.read(t), chValue, fmt.Sprintf("[%d,17]", firstID))
	close(first.values)
	subscriptionTestNotification(t, c.read(t), chClose, fmt.Sprintf("[%d]", firstID))
	subscriptionTestReceive(t, first.ctx.Done())
	require.Equal(t, []string{"second"}, c.handlingIDs())
	require.NoError(t, second.ctx.Err())

	replacement, resp := h.subscribe(t, c, "replacement")
	subscriptionTestChannelID(t, resp)
	require.NoError(t, replacement.ctx.Err())
	require.ElementsMatch(t, []string{"second", "replacement"}, c.handlingIDs())
}

func TestWebsocketSubscriptionMethodErrorReleasesCapacity(t *testing.T) {
	h := newSubscriptionTestServer(t, 1, nil)
	c := h.connect(t)
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("error-%d", i)
		c.send(t, id, "Subscriptions.SubscribeError")
		entry := subscriptionTestReceive(t, c.entered)
		require.Equal(t, id, entry.id)
		resp := c.read(t)
		require.Equal(t, id, resp.ID)
		require.NotNil(t, resp.Error)
		require.Equal(t, "subscription unavailable", resp.Error.Message)
		require.Equal(t, id, subscriptionTestReceive(t, c.finished))
		subscriptionTestReceive(t, entry.ctx.Done())
		require.Empty(t, c.handlingIDs())
	}

	call, resp := h.subscribe(t, c, "replacement")
	subscriptionTestChannelID(t, resp)
	require.NoError(t, call.ctx.Err())
	h.reject(t, c, "rejected")
}

func TestWebsocketSubscriptionCancellationWaitsForChannelClose(t *testing.T) {
	h := newSubscriptionTestServer(t, 1, nil)
	c := h.connect(t)
	first, resp := h.subscribe(t, c, "first")
	firstID := subscriptionTestChannelID(t, resp)

	c.send(t, nil, wsCancel, "first")
	subscriptionTestReceive(t, first.ctx.Done())
	h.reject(t, c, "rejected")
	require.Equal(t, []string{"first"}, c.handlingIDs())

	close(first.values)
	subscriptionTestNotification(t, c.read(t), chClose, fmt.Sprintf("[%d]", firstID))
	require.Empty(t, c.handlingIDs())
	replacement, resp := h.subscribe(t, c, "replacement")
	subscriptionTestChannelID(t, resp)
	require.NoError(t, replacement.ctx.Err())
}

func TestWebsocketSubscriptionReusedRequestID(t *testing.T) {
	h := newSubscriptionTestServer(t, 2, nil)
	c := h.connect(t)
	first, resp := h.subscribe(t, c, "same")
	firstID := subscriptionTestChannelID(t, resp)
	second, resp := h.subscribe(t, c, "same")
	secondID := subscriptionTestChannelID(t, resp)
	require.NotEqual(t, firstID, secondID)
	subscriptionTestReceive(t, first.ctx.Done())
	require.NoError(t, second.ctx.Err())

	close(first.values)
	subscriptionTestNotification(t, c.read(t), chClose, fmt.Sprintf("[%d]", firstID))
	require.Equal(t, []string{"same"}, c.handlingIDs())
	require.NoError(t, second.ctx.Err())

	// Cleanup of the old subscription must preserve cancellation of the new one.
	c.send(t, nil, wsCancel, "same")
	subscriptionTestReceive(t, second.ctx.Done())
	close(second.values)
	subscriptionTestNotification(t, c.read(t), chClose, fmt.Sprintf("[%d]", secondID))
	require.Empty(t, c.handlingIDs())
}

func TestWebsocketSubscriptionConcurrentRegistration(t *testing.T) {
	const limit = 3
	const attempts = 12
	gate := make(chan struct{})
	h := newSubscriptionTestServer(t, limit, gate)
	c := h.connect(t)
	for i := 0; i < attempts; i++ {
		id := fmt.Sprintf("concurrent-%d", i)
		c.send(t, id, "Subscriptions.Subscribe", id)
	}
	entries := make(map[string]context.Context, attempts)
	for i := 0; i < attempts; i++ {
		entry := subscriptionTestReceive(t, c.entered)
		entries[entry.id.(string)] = entry.ctx
	}
	calls := make(map[string]*subscriptionTestCall, limit)
	for i := 0; i < limit; i++ {
		call := subscriptionTestReceive(t, h.service.calls)
		calls[call.name] = call
	}
	// Accepted producers still occupy reservations inside their methods. All
	// excess calls must already be rejected, without entering those methods.
	replies := make(map[string]bool, attempts)
	for i := 0; i < attempts-limit; i++ {
		resp := c.read(t)
		id, ok := resp.ID.(string)
		require.True(t, ok)
		require.Contains(t, entries, id)
		require.NotContains(t, calls, id)
		require.False(t, replies[id], "duplicate response for %s", id)
		replies[id] = true
		subscriptionTestRejected(t, resp)
		subscriptionTestReceive(t, entries[id].Done())
	}
	require.Empty(t, h.service.calls, "pending producers must count toward the subscription limit")
	close(gate)
	accepted := make(map[string]uint64, limit)
	for i := 0; i < limit; i++ {
		resp := c.read(t)
		id, ok := resp.ID.(string)
		require.True(t, ok)
		require.Contains(t, calls, id)
		require.False(t, replies[id], "duplicate response for %s", id)
		replies[id] = true
		accepted[id] = subscriptionTestChannelID(t, resp)
	}
	for i := 0; i < attempts; i++ {
		subscriptionTestReceive(t, c.finished)
	}
	require.Len(t, accepted, limit)
	require.Len(t, c.handlingIDs(), limit)
	for id, chID := range accepted {
		require.NoError(t, calls[id].ctx.Err())
		calls[id].send(t, 42)
		subscriptionTestNotification(t, c.read(t), chValue, fmt.Sprintf("[%d,42]", chID))
	}
}

func TestWebsocketSubscriptionAdmissionWithStalledWriter(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			const rejectedCount = 8
			h := newSubscriptionTestServer(t, limit, nil)
			c := h.connect(t)
			first, resp := h.subscribe(t, c, "first")
			firstID := subscriptionTestChannelID(t, resp)

			// Hold the transport lock after the first registration has finished.
			// The unbuffered send synchronizes with the output loop receiving the
			// event; it then cannot register more channels until writes resume.
			c.server.writeLk.Lock()
			var unlock sync.Once
			defer unlock.Do(c.server.writeLk.Unlock)
			first.send(t, 99)

			var pending *subscriptionTestCall
			if limit == 2 {
				c.send(t, "pending", "Subscriptions.Subscribe", "pending")
				require.Equal(t, "pending", subscriptionTestReceive(t, c.entered).id)
				pending = subscriptionTestReceive(t, h.service.calls)
				require.Equal(t, "pending", pending.name)
				// This producer has acquired a slot, but the blocked output loop
				// cannot yet receive its channel registration.
			}

			for i := 0; i < rejectedCount; i++ {
				id := fmt.Sprintf("blocked-%d", i)
				c.send(t, id, "Subscriptions.Subscribe", id)
			}
			entries := make(map[string]context.Context, rejectedCount)
			for i := 0; i < rejectedCount; i++ {
				entry := subscriptionTestReceive(t, c.entered)
				entries[entry.id.(string)] = entry.ctx
			}
			// Each excess handler has reached its response write, proving its
			// admission check has run even though no response can be delivered.
			for i := 0; i < rejectedCount; i++ {
				id := subscriptionTestReceive(t, c.writing)
				require.Contains(t, entries, id)
			}
			require.Empty(t, h.service.calls, "stalled writes must not allow additional producers")
			require.NoError(t, first.ctx.Err())
			if pending != nil {
				require.NoError(t, pending.ctx.Err())
			}

			unlock.Do(c.server.writeLk.Unlock)
			seen := make(map[any]bool, rejectedCount+limit)
			for i := 0; i < rejectedCount+limit; i++ {
				resp := c.read(t)
				require.False(t, seen[resp.ID], "duplicate response for %v", resp.ID)
				seen[resp.ID] = true
				switch resp.ID {
				case nil:
					subscriptionTestNotification(t, resp, chValue, fmt.Sprintf("[%d,99]", firstID))
				case "pending":
					require.Equal(t, 2, limit)
					subscriptionTestChannelID(t, resp)
				default:
					require.Contains(t, entries, resp.ID)
					subscriptionTestRejected(t, resp)
				}
			}
			for i := 0; i < rejectedCount+limit-1; i++ {
				subscriptionTestReceive(t, c.finished)
			}
			for _, ctx := range entries {
				subscriptionTestReceive(t, ctx.Done())
			}
			require.Len(t, c.handlingIDs(), limit)
			require.Empty(t, h.service.calls)
		})
	}
}

type subscriptionParameter int

type subscriptionParamsService struct{}

func (*subscriptionParamsService) Subscribe(value subscriptionParameter) <-chan int {
	values := make(chan int, 1)
	values <- int(value)
	close(values)
	return values
}

func (*subscriptionParamsService) NilChannel() <-chan int { return nil }

func (*subscriptionParamsService) NilChannelWithError() (<-chan int, error) { return nil, nil }

func newSubscriptionParamsConnection(t *testing.T, decoder ParamDecoder) subscriptionTestConnection {
	t.Helper()
	opts := []ServerOption{WithMaxSubscriptions(1), WithServerPingInterval(0)}
	if decoder != nil {
		opts = append(opts, WithParamDecoder(new(subscriptionParameter), decoder))
	}
	s := NewServer(opts...)
	s.Register("Parameters", &subscriptionParamsService{})
	connections := make(chan subscriptionTestConnection, 1)
	s.reverseClientBuilder = func(ctx context.Context, c *wsConn) (context.Context, error) {
		h := &subscriptionTestHandler{
			reqestHandler: c.handler,
			finished:      make(chan any, 16),
			entered:       make(chan subscriptionTestRequest, 16),
			writing:       make(chan any, 16),
		}
		c.handler = h
		connections <- subscriptionTestConnection{server: c, finished: h.finished, entered: h.entered, writing: h.writing}
		return ctx, nil
	}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	c := subscriptionTestReceive(t, connections)
	c.client = conn
	t.Cleanup(func() {
		_ = conn.Close()
		subscriptionTestReceive(t, c.server.exiting)
	})
	return c
}

func finishSubscriptionParamsCall(t *testing.T, c subscriptionTestConnection, id string, value int) {
	t.Helper()
	resp := c.read(t)
	require.Equal(t, id, resp.ID)
	channelID := subscriptionTestChannelID(t, resp)
	subscriptionTestNotification(t, c.read(t), chValue, fmt.Sprintf("[%d,%d]", channelID, value))
	subscriptionTestNotification(t, c.read(t), chClose, fmt.Sprintf("[%d]", channelID))
	require.Equal(t, id, subscriptionTestReceive(t, c.finished))
	require.Zero(t, c.server.subscriptions.Load())
	require.Empty(t, c.handlingIDs())
}

func TestSubscriptionAdmissionBeforeParameterDecoding(t *testing.T) {
	entered := make(chan struct{}, 2)
	gate := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(unblock)
	c := newSubscriptionParamsConnection(t, func(ctx context.Context, b []byte) (reflect.Value, error) {
		entered <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			return reflect.Value{}, ctx.Err()
		}
		var value subscriptionParameter
		if err := json.Unmarshal(b, &value); err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(value), nil
	})

	c.send(t, "first", "Parameters.Subscribe", 17)
	first := subscriptionTestReceive(t, c.entered)
	subscriptionTestReceive(t, entered)
	require.Equal(t, int64(1), c.server.subscriptions.Load())

	c.send(t, "second", "Parameters.Subscribe", 23)
	second := subscriptionTestReceive(t, c.entered)
	resp := c.read(t)
	require.Equal(t, "second", resp.ID)
	subscriptionTestRejected(t, resp)
	require.Equal(t, "second", subscriptionTestReceive(t, c.finished))
	subscriptionTestReceive(t, second.ctx.Done())
	require.Empty(t, entered, "rejected calls must not enter the parameter decoder")
	require.Equal(t, []string{"first"}, c.handlingIDs())

	unblock()
	finishSubscriptionParamsCall(t, c, "first", 17)
	subscriptionTestReceive(t, first.ctx.Done())
}

func TestSubscriptionParameterFailureReleasesCapacity(t *testing.T) {
	for _, test := range []struct {
		name    string
		decoder ParamDecoder
	}{
		{name: "standard decoder"},
		{
			name: "custom decoder",
			decoder: func(_ context.Context, b []byte) (reflect.Value, error) {
				var value subscriptionParameter
				if err := json.Unmarshal(b, &value); err != nil {
					return reflect.Value{}, errors.New("invalid subscription parameter")
				}
				return reflect.ValueOf(value), nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newSubscriptionParamsConnection(t, test.decoder)
			c.send(t, "invalid", "Parameters.Subscribe", "invalid")
			entry := subscriptionTestReceive(t, c.entered)
			resp := c.read(t)
			require.Equal(t, "invalid", resp.ID)
			require.NotNil(t, resp.Error)
			require.Equal(t, ErrorCode(rpcParseError), resp.Error.Code)
			require.Equal(t, "invalid", subscriptionTestReceive(t, c.finished))
			subscriptionTestReceive(t, entry.ctx.Done())
			require.Zero(t, c.server.subscriptions.Load())
			require.Empty(t, c.handlingIDs())

			c.send(t, "valid", "Parameters.Subscribe", 29)
			entry = subscriptionTestReceive(t, c.entered)
			finishSubscriptionParamsCall(t, c, "valid", 29)
			subscriptionTestReceive(t, entry.ctx.Done())
		})
	}
}

func TestSubscriptionNilChannelReleasesCapacity(t *testing.T) {
	for _, method := range []string{"Parameters.NilChannel", "Parameters.NilChannelWithError"} {
		t.Run(method, func(t *testing.T) {
			c := newSubscriptionParamsConnection(t, nil)
			c.send(t, "nil-channel", method)
			entry := subscriptionTestReceive(t, c.entered)
			resp := c.read(t)
			require.Equal(t, "nil-channel", resp.ID)
			require.NotNil(t, resp.Error)
			require.Equal(t, ErrorCode(1), resp.Error.Code)
			require.Contains(t, resp.Error.Message, "returned a nil channel")
			require.Equal(t, "nil-channel", subscriptionTestReceive(t, c.finished))
			subscriptionTestReceive(t, entry.ctx.Done())
			require.Zero(t, c.server.subscriptions.Load())
			require.Empty(t, c.handlingIDs())

			c.send(t, "valid", "Parameters.Subscribe", 31)
			entry = subscriptionTestReceive(t, c.entered)
			finishSubscriptionParamsCall(t, c, "valid", 31)
			subscriptionTestReceive(t, entry.ctx.Done())
		})
	}
}

type panicTestParam int

type panicTestRPC struct {
	started chan context.Context
}

func (*panicTestRPC) Echo(n int) int { return n }

func (*panicTestRPC) Decode(n panicTestParam) int { return int(n) }

func (h *panicTestRPC) Subscribe(ctx context.Context) <-chan int {
	h.started <- ctx
	return make(chan int)
}

func receivePanicTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	var value T
	select {
	case value = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket worker")
	}
	return value
}

func newPanicTestServer(t *testing.T, configure func(*wsConn), opts ...ServerOption) (string, <-chan *wsConn, *panicTestRPC) {
	t.Helper()
	handler := &panicTestRPC{started: make(chan context.Context, 8)}
	server := NewServer(opts...)
	server.Register("Probe", handler)
	connections := make(chan *wsConn, 8)
	server.reverseClientBuilder = func(ctx context.Context, c *wsConn) (context.Context, error) {
		if configure != nil {
			configure(c)
		}
		connections <- c
		return ctx, nil
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return "ws" + strings.TrimPrefix(httpServer.URL, "http"), connections, handler
}

func readPanicTestFrame(t *testing.T, conn *websocket.Conn) frame {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var result frame
	require.NoError(t, conn.ReadJSON(&result))
	return result
}

func openPanicTestConnection(t *testing.T, url string, connections <-chan *wsConn) (*websocket.Conn, *wsConn) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	c := receivePanicTest(t, connections)
	t.Cleanup(func() {
		_ = conn.Close()
		receivePanicTest(t, c.exiting)
	})
	// The response also synchronizes with initialization of wsConn's fields.
	require.NoError(t, conn.WriteJSON(request{Jsonrpc: "2.0", ID: 1, Method: "Probe.Echo", Params: []byte(`[42]`)}))
	response := readPanicTestFrame(t, conn)
	require.Nil(t, response.Error)
	require.JSONEq(t, `42`, string(response.Result))
	return conn, c
}

func requirePanicTestDisconnected(t *testing.T, conn *websocket.Conn, c *wsConn) {
	t.Helper()
	receivePanicTest(t, c.failed)
	receivePanicTest(t, c.exiting)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err)
	var netErr net.Error
	if errors.As(err, &netErr) {
		require.False(t, netErr.Timeout(), "expected connection closure, not a read timeout")
	}
}

func TestWebsocketRequestCallbackRecovery(t *testing.T) {
	quietRPCLogsForTest(t)
	for _, test := range []struct {
		name   string
		option ServerOption
	}{
		{
			name: "parameter decoder",
			option: WithParamDecoder(new(panicTestParam), func(context.Context, []byte) (reflect.Value, error) {
				panic("parameter decoder failed")
			}),
		},
		{
			name: "tracer",
			option: WithTracer(func(method string, _, _ []reflect.Value, _ error) {
				if method == "Probe.Decode" {
					panic("tracer failed")
				}
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			url, connections, handler := newPanicTestServer(t, nil, test.option)
			conn, c := openPanicTestConnection(t, url, connections)
			require.NoError(t, conn.WriteJSON(request{Jsonrpc: "2.0", ID: 2, Method: "Probe.Subscribe", Params: []byte(`[]`)}))
			require.Nil(t, readPanicTestFrame(t, conn).Error)
			ctx := receivePanicTest(t, handler.started)
			require.NoError(t, conn.WriteJSON(request{Jsonrpc: "2.0", ID: 3, Method: "Probe.Decode", Params: []byte(`[1]`)}))
			requirePanicTestDisconnected(t, conn, c)
			receivePanicTest(t, ctx.Done())
			receivePanicTest(t, c.outChansDone)
			openPanicTestConnection(t, url, connections)
		})
	}
}

type websocketCancellationCause struct {
	compared atomic.Bool
}

func (*websocketCancellationCause) Error() string { return "caller canceled subscription" }

func (c *websocketCancellationCause) Is(error) bool {
	c.compared.Store(true)
	return true
}

func TestWebsocketContextCompletionControlsRemoteCancellation(t *testing.T) {
	customCause := &websocketCancellationCause{}
	for _, test := range []struct {
		name         string
		cause        error
		remoteCancel bool
	}{
		{name: "stream completed", cause: errChannelClosed},
		{name: "caller canceled", cause: context.Canceled, remoteCancel: true},
		{name: "custom cancellation cause", cause: customCause, remoteCancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			url, connections, _ := newPanicTestServer(t, nil)
			conn, c := openPanicTestConnection(t, url, connections)
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(test.cause)
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.handleCtxAsync(ctx, float64(42))
			}()
			receivePanicTest(t, done)
			require.False(t, customCause.compared.Load(), "cancellation should not invoke the caller's error matching method")
			require.NoError(t, conn.WriteJSON(request{Jsonrpc: "2.0", ID: 2, Method: "Probe.Echo", Params: []byte(`[17]`)}))
			if test.remoteCancel {
				cancellation := readPanicTestFrame(t, conn)
				require.Equal(t, wsCancel, cancellation.Method)
				require.JSONEq(t, `[42]`, string(cancellation.Params))
			}
			// A completed stream must not send a cancel for a possibly reused ID.
			response := readPanicTestFrame(t, conn)
			require.Equal(t, float64(2), response.ID)
			require.JSONEq(t, `17`, string(response.Result))
		})
	}
}

type panicTestBlockedConn struct {
	net.Conn
	block   atomic.Bool
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *panicTestBlockedConn) Write(p []byte) (int, error) {
	if !c.block.Load() {
		return c.Conn.Write(p)
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *panicTestBlockedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestWebsocketReconnectTransportLifecycle(t *testing.T) {
	for _, pendingWrite := range []bool{false, true} {
		name := "idle"
		if pendingWrite {
			name = "pending write"
		}
		t.Run(name, func(t *testing.T) {
			url, _, _ := newPanicTestServer(t, nil, WithServerPingInterval(0))
			var transport *panicTestBlockedConn
			dialer := *websocket.DefaultDialer
			dialer.NetDial = func(network, addr string) (net.Conn, error) {
				conn, err := net.Dial(network, addr)
				if err != nil {
					return nil, err
				}
				transport = &panicTestBlockedConn{
					Conn: conn, entered: make(chan struct{}, 1), closed: make(chan struct{}),
				}
				return transport, nil
			}
			conn, _, err := dialer.Dial(url, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			previousClosed := make(chan bool, 1)
			c := &wsConn{
				conn: conn, failed: make(chan struct{}), exiting: make(chan struct{}),
				stopPings:        func() {},
				reconnectBackoff: backoff{minDelay: time.Millisecond, maxDelay: time.Millisecond},
				connFactory: func() (*websocket.Conn, error) {
					select {
					case <-transport.closed:
						previousClosed <- true
					default:
						previousClosed <- false
					}
					cancel()
					return nil, context.Canceled
				},
			}

			written := make(chan error, 1)
			if pendingWrite {
				transport.block.Store(true)
				go func() {
					written <- c.sendRequest(request{Jsonrpc: "2.0", ID: 1, Method: "Probe.Echo", Params: []byte(`[1]`)})
				}()
				receivePanicTest(t, transport.entered)
			}

			reconnecting := make(chan bool, 1)
			go func() { reconnecting <- c.tryReconnect(ctx) }()
			require.True(t, receivePanicTest(t, reconnecting))
			require.True(t, receivePanicTest(t, previousClosed), "previous transport should close before the next dial")
			if pendingWrite {
				require.Error(t, receivePanicTest(t, written))
			}
			select {
			case <-c.failed:
				t.Fatal("reconnecting should leave the connection lifecycle active")
			default:
			}
		})
	}
}

func TestWebsocketWorkerShutdownUnblocksWriter(t *testing.T) {
	quietRPCLogsForTest(t)
	url, _, _ := newPanicTestServer(t, nil)
	var transport *panicTestBlockedConn
	dialer := *websocket.DefaultDialer
	dialer.NetDial = func(network, addr string) (net.Conn, error) {
		conn, err := net.Dial(network, addr)
		if err != nil {
			return nil, err
		}
		transport = &panicTestBlockedConn{Conn: conn, entered: make(chan struct{}, 1), closed: make(chan struct{})}
		return transport, nil
	}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := &wsConn{
		conn: conn, failed: make(chan struct{}), exiting: make(chan struct{}),
		maxSubscriptions: defaultMaxSubscriptions,
		frameExecQueue:   make(chan []byte, 1),
		chanHandlers:     map[uint64]*chanHandler{7: {cb: func([]byte, bool) { panic("channel decoder failed") }}},
		registerCh:       make(chan outChanReg), outChansDone: make(chan struct{}),
	}
	transport.block.Store(true)
	written := make(chan error, 1)
	go func() {
		written <- c.sendRequest(request{Jsonrpc: "2.0", ID: 1, Method: "Probe.Echo", Params: []byte(`[1]`)})
	}()
	receivePanicTest(t, transport.entered)
	// The first registration is accepted, but its response is blocked behind
	// the writer. A second registration must escape when the connection fails.
	first := make(chan error, 1)
	go func() {
		first <- c.handleChanOut(reflect.ValueOf(make(chan int)), float64(2), func() {})
	}()
	require.NoError(t, receivePanicTest(t, first))
	pending := make(chan error, 1)
	go func() {
		pending <- c.handleChanOut(reflect.ValueOf(make(chan int)), float64(3), func() {})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	executed := make(chan struct{})
	go func() {
		defer close(executed)
		c.frameExecutor(ctx)
	}()
	c.frameExecQueue <- []byte(`{"jsonrpc":"2.0","method":"xrpc.ch.val","params":[7,1]}`)
	receivePanicTest(t, c.failed)
	require.Error(t, receivePanicTest(t, written))
	receivePanicTest(t, executed)
	_ = receivePanicTest(t, pending) // Admission may race shutdown, but cannot hang.
	receivePanicTest(t, c.outChansDone)
	require.True(t, c.writeLk.TryLock(), "writer lock remained held after shutdown")
	c.writeLk.Unlock()
	require.True(t, c.chanHandlers[7].lk.TryLock(), "callback lock remained held after panic")
	c.chanHandlers[7].lk.Unlock()
}

func TestWebsocketChannelCleanup(t *testing.T) {
	closed := make(chan uint64, 3)
	handlers := map[uint64]*chanHandler{}
	for id := uint64(1); id <= 3; id++ {
		handlers[id] = &chanHandler{cb: func(_ []byte, ok bool) {
			if ok {
				t.Error("cleanup unexpectedly delivered a value")
			}
			closed <- id
		}}
	}
	c := &wsConn{failed: make(chan struct{}), chanHandlers: handlers}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.closeChans()
	}()
	receivePanicTest(t, done)
	seen := []uint64{receivePanicTest(t, closed), receivePanicTest(t, closed), receivePanicTest(t, closed)}
	require.ElementsMatch(t, []uint64{1, 2, 3}, seen)
	require.Empty(t, c.chanHandlers)
	require.True(t, c.chanHandlersLk.TryLock())
	c.chanHandlersLk.Unlock()
	for _, handler := range handlers {
		require.True(t, handler.lk.TryLock())
		handler.lk.Unlock()
	}
}

func TestWebsocketResponseDuringShutdownClosesNewChannel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	sinkClosed := make(chan struct{})
	request := clientRequest{
		ready: make(chan clientResponse, 1),
		retCh: func() (context.Context, func([]byte, bool)) {
			close(entered)
			<-release
			return context.Background(), func(_ []byte, ok bool) {
				if !ok {
					close(sinkClosed)
				}
			}
		},
	}
	c := &wsConn{
		failed: make(chan struct{}), exiting: make(chan struct{}),
		inflight:     map[any]clientRequest{float64(1): request},
		chanHandlers: make(map[uint64]*chanHandler),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handleResponse(frame{Jsonrpc: "2.0", ID: float64(1), Result: []byte(`7`)})
	}()
	receivePanicTest(t, entered)
	c.fail()
	c.closeChans()
	c.closeInFlight()
	unblock()
	receivePanicTest(t, done)
	receivePanicTest(t, sinkClosed)
	require.Empty(t, c.chanHandlers)
	require.NotNil(t, receivePanicTest(t, request.ready).Error)
}

func TestWebsocketResponseDuringReconnect(t *testing.T) {
	for _, reuseID := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse-id-%t", reuseID), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			sinkClosed := make(chan struct{})
			request := clientRequest{
				ready: make(chan clientResponse, 1),
				retCh: func() (context.Context, func([]byte, bool)) {
					close(entered)
					<-release
					return context.Background(), func(_ []byte, ok bool) {
						if !ok {
							close(sinkClosed)
						}
					}
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			dialed := make(chan struct{})
			c := &wsConn{
				failed: make(chan struct{}), exiting: make(chan struct{}),
				inflight:         map[any]clientRequest{float64(1): request},
				chanHandlers:     make(map[uint64]*chanHandler),
				stopPings:        func() {},
				reconnectBackoff: backoff{minDelay: time.Millisecond, maxDelay: time.Millisecond},
				connFactory: func() (*websocket.Conn, error) {
					cancel()
					close(dialed)
					return nil, context.Canceled
				},
			}
			t.Cleanup(c.fail)
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.handleResponse(frame{Jsonrpc: "2.0", ID: float64(1), Result: []byte(`7`)})
			}()
			subscriptionTestReceive(t, entered)
			require.True(t, c.tryReconnect(ctx))
			subscriptionTestReceive(t, dialed)
			require.NotNil(t, subscriptionTestReceive(t, request.ready).Error)
			select {
			case <-c.failed:
				t.Fatal("reconnecting should leave the connection lifecycle active")
			default:
			}

			replacement := clientRequest{ready: make(chan clientResponse, 1)}
			replacementHandler := &chanHandler{cb: func([]byte, bool) {}}
			if reuseID {
				c.inflightLk.Lock()
				c.inflight[float64(1)] = replacement
				c.inflightLk.Unlock()
				c.chanHandlersLk.Lock()
				c.chanHandlers[7] = replacementHandler
				c.chanHandlersLk.Unlock()
			}
			unblock()
			subscriptionTestReceive(t, done)
			if reuseID {
				require.Len(t, c.chanHandlers, 1)
				require.Same(t, replacementHandler, c.chanHandlers[7])
				require.Equal(t, replacement.ready, c.inflight[float64(1)].ready)
				require.Empty(t, replacement.ready)
			} else {
				require.Empty(t, c.chanHandlers)
				require.Empty(t, c.inflight)
			}
			subscriptionTestReceive(t, sinkClosed)
			require.Empty(t, request.ready)
		})
	}
}

type frameLimitService struct {
	calls atomic.Int32
}

func (s *frameLimitService) Echo(value string) string {
	s.calls.Add(1)
	return value
}

func newFrameLimitServer(t *testing.T, limit int64, service *frameLimitService) (string, <-chan *wsConn) {
	t.Helper()
	s := NewServer(WithMaxRequestSize(limit), WithServerPingInterval(0))
	s.Register("Frames", service)
	connections := make(chan *wsConn, 1)
	s.reverseClientBuilder = func(ctx context.Context, c *wsConn) (context.Context, error) {
		connections <- c
		return ctx, nil
	}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), connections
}

func TestWebsocketServerFrameLimit(t *testing.T) {
	for _, test := range []struct {
		name   string
		limit  int64
		size   int
		reject bool
	}{
		{name: "below limit", limit: 128, size: 127},
		{name: "at limit", limit: 128, size: 128},
		{name: "over limit", limit: 128, size: 129, reject: true},
		{name: "zero limit", limit: 0, size: 128, reject: true},
		{name: "maximum limit", limit: math.MaxInt64, size: 128},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &frameLimitService{}
			url, connections := newFrameLimitServer(t, test.limit, service)
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			require.NoError(t, err)
			serverConn := subscriptionTestReceive(t, connections)
			t.Cleanup(func() {
				_ = conn.Close()
				subscriptionTestReceive(t, serverConn.exiting)
			})
			require.NotNil(t, serverConn.maxRequestSize)
			require.Equal(t, test.limit, *serverConn.maxRequestSize)

			payload := `{"jsonrpc":"2.0","id":1,"method":"Frames.Echo","params":["ok"]}`
			require.LessOrEqual(t, len(payload), test.size)
			// Padding keeps every boundary case a valid request, even if truncated.
			payload += strings.Repeat(" ", test.size-len(payload))
			require.NoError(t, conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			if test.reject {
				_, _, err = conn.ReadMessage()
				require.Error(t, err)
				var netErr net.Error
				if errors.As(err, &netErr) {
					require.False(t, netErr.Timeout(), "oversized request should close the connection")
				}
				subscriptionTestReceive(t, serverConn.exiting)
				require.Zero(t, service.calls.Load(), "oversized requests must not invoke the handler")
				return
			}

			var resp frame
			require.NoError(t, conn.ReadJSON(&resp))
			require.Equal(t, float64(1), resp.ID)
			require.Nil(t, resp.Error)
			require.JSONEq(t, `"ok"`, string(resp.Result))
			require.Equal(t, int32(1), service.calls.Load())
		})
	}
}

func TestWebsocketClientFrameSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	written := make(chan error, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			written <- err
			return
		}
		defer func() { _ = conn.Close() }()
		err = func() error {
			if err := conn.SetReadDeadline(deadline); err != nil {
				return err
			}
			if err := conn.SetWriteDeadline(deadline); err != nil {
				return err
			}
			var req frame
			if err := conn.ReadJSON(&req); err != nil {
				return err
			}
			writer, err := conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return err
			}
			// Keep the decoded result small while the message exceeds the default server limit.
			padding := strings.Repeat(" ", 64<<10)
			for sent := 0; sent < DEFAULT_MAX_REQUEST_SIZE; sent += len(padding) {
				if _, err := io.WriteString(writer, padding); err != nil {
					return err
				}
			}
			if err := json.NewEncoder(writer).Encode(frame{Jsonrpc: "2.0", ID: req.ID, Result: json.RawMessage(`"ok"`)}); err != nil {
				return err
			}
			return writer.Close()
		}()
		written <- err
		// Keep the connection open until the client has processed the response.
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	var proxy struct {
		Result func(context.Context) (string, error)
	}
	closeClient, err := NewMergeClient(ctx, url, "Frames", []any{&proxy}, nil, WithNoReconnect(), WithPingInterval(0))
	require.NoError(t, err)
	t.Cleanup(closeClient)

	got, err := proxy.Result(ctx)
	require.NoError(t, err)
	require.Equal(t, "ok", got)
	select {
	case err := <-written:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("timed out writing the response")
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	limit := int64(128)
	payload := `{"jsonrpc":"2.0","id":1,"method":"Frames.Echo","params":["ok"]}`
	source := strings.NewReader(payload + strings.Repeat(" ", 256-len(payload)))
	c := &wsConn{
		maxRequestSize: &limit,
		readError:      make(chan error, 1),
		frameExecQueue: make(chan []byte, 1),
		failed:         make(chan struct{}),
	}
	c.readFrame(context.Background(), source)
	select {
	case err := <-c.readError:
		require.EqualError(t, err, "reading frame into a buffer: websocket frame exceeds maximum size of 128 bytes")
	default:
		t.Fatal("oversized frame did not produce a read error")
	}
	require.Equal(t, limit+1, source.Size()-int64(source.Len()), "stop reading as soon as the frame exceeds the limit")
	require.Empty(t, c.frameExecQueue, "oversized frames must not reach the executor")
}

package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"
)

func init() {
	if _, exists := os.LookupEnv("GOLOG_LOG_LEVEL"); !exists {
		if err := logging.SetLogLevel("rpc", "DEBUG"); err != nil {
			panic(err)
		}
	}

	debugTrace = true
}

type SimpleServerHandler struct {
	n int32
}

type TestType struct {
	S string
	I int
}

type TestOut struct {
	TestType
	Ok bool
}

func (h *SimpleServerHandler) Inc() error {
	h.n++

	return nil
}

func (h *SimpleServerHandler) Add(in int) error {
	if in == -3546 {
		return errors.New("test")
	}

	atomic.AddInt32(&h.n, int32(in))

	return nil
}

func (h *SimpleServerHandler) AddGet(in int) int {
	atomic.AddInt32(&h.n, int32(in))
	return int(h.n)
}

func (h *SimpleServerHandler) StringMatch(t TestType, i2 int64) (out TestOut, err error) {
	if strconv.FormatInt(i2, 10) == t.S {
		out.Ok = true
	}
	if i2 != int64(t.I) {
		return TestOut{}, errors.New(":(")
	}
	out.I = t.I
	out.S = t.S
	return
}

func TestRawRequests(t *testing.T) {
	rpcHandler := SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	removeSpaces := func(jsonStr string) (string, error) {
		var jsonObj interface{}
		err := json.Unmarshal([]byte(jsonStr), &jsonObj)
		if err != nil {
			return "", err
		}

		compactJSONBytes, err := json.Marshal(jsonObj)
		if err != nil {
			return "", err
		}

		return string(compactJSONBytes), nil
	}

	tc := func(req, resp string, n int32, statusCode int) func(t *testing.T) {
		return func(t *testing.T) {
			rpcHandler.n = 0

			res, err := http.Post(testServ.URL, "application/json", strings.NewReader(req))
			require.NoError(t, err)

			b, err := io.ReadAll(res.Body)
			require.NoError(t, err)

			expectedResp, err := removeSpaces(resp)
			require.NoError(t, err)

			responseBody, err := removeSpaces(string(b))
			require.NoError(t, err)

			assert.Equal(t, expectedResp, responseBody)
			require.Equal(t, n, rpcHandler.n)
			require.Equal(t, statusCode, res.StatusCode)
		}
	}

	t.Run("inc", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "params": [], "id": 1}`, `{"jsonrpc":"2.0","id":1,"result":null}`, 1, 200))
	t.Run("inc-null", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "params": null, "id": 1}`, `{"jsonrpc":"2.0","id":1,"result":null}`, 1, 200))
	t.Run("inc-noparam", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "id": 2}`, `{"jsonrpc":"2.0","id":2,"result":null}`, 1, 200))
	t.Run("add", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [10], "id": 4}`, `{"jsonrpc":"2.0","id":4,"result":null}`, 10, 200))
	// Batch requests
	t.Run("add", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 5}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`, 0, 500))
	t.Run("add", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 6}]`, `[{"jsonrpc":"2.0","id":6,"result":null}]`, 123, 200))
	t.Run("add", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 7},{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [-122], "id": 8}]`, `[{"jsonrpc":"2.0","id":7,"result":null},{"jsonrpc":"2.0","id":8,"result":null}]`, 1, 200))
	t.Run("add", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 9},{"jsonrpc": "2.0", "params": [-122], "id": 10}]`, `[{"jsonrpc":"2.0","id":9,"result":null},{"error":{"code":-32601,"message":"method '' not found"},"id":10,"jsonrpc":"2.0"}]`, 123, 200))
	t.Run("add", tc(`     [{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [-1], "id": 11}]   `, `[{"jsonrpc":"2.0","id":11,"result":null}]`, -1, 200))
	t.Run("add", tc(``, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`, 0, 400))
}

func TestReconnection(t *testing.T) {
	var rpcClient struct {
		Add func(int) error
	}

	rpcHandler := SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// capture connection attempts for this duration
	captureDuration := 3 * time.Second

	// run the test until the timer expires
	timer := time.NewTimer(captureDuration)

	// record the number of connection attempts during this test
	connectionAttempts := int64(1)

	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", []interface{}{&rpcClient}, nil, func(c *Config) {
		c.proxyConnFactory = func(f func() (*websocket.Conn, error)) func() (*websocket.Conn, error) {
			return func() (*websocket.Conn, error) {
				defer func() {
					atomic.AddInt64(&connectionAttempts, 1)
				}()

				if atomic.LoadInt64(&connectionAttempts) > 1 {
					return nil, errors.New("simulates a failed reconnect attempt")
				}

				c, err := f()
				if err != nil {
					return nil, err
				}

				// closing the connection here triggers the reconnect logic
				_ = c.Close()

				return c, nil
			}
		}
	})
	require.NoError(t, err)
	defer closer()

	// let the JSON-RPC library attempt to reconnect until the timer runs out
	<-timer.C

	// do some math
	attemptsPerSecond := atomic.LoadInt64(&connectionAttempts) / int64(captureDuration/time.Second)

	assert.Less(t, attemptsPerSecond, int64(50))
}

func (h *SimpleServerHandler) ErrChanSub(ctx context.Context) (<-chan int, error) {
	return nil, errors.New("expect to return an error")
}

func TestRPCBadConnection(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
		ErrChanSub  func(context.Context) (<-chan int, error)
	}
	closer, err := NewClient(context.Background(), "http://"+testServ.Listener.Addr().String()+"0", "SimpleServerHandler", &client, nil)
	require.NoError(t, err)
	err = client.Add(2)
	require.True(t, errors.As(err, new(*RPCConnectionError)))

	defer closer()

}

func TestRPC(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
		ErrChanSub  func(context.Context) (<-chan int, error)
	}
	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	require.NoError(t, err)
	defer closer()

	// Add(int) error

	require.NoError(t, client.Add(2))
	require.Equal(t, 2, int(serverHandler.n))

	err = client.Add(-3546)
	require.EqualError(t, err, "test")

	// AddGet(int) int

	n := client.AddGet(3)
	require.Equal(t, 5, n)
	require.Equal(t, 5, int(serverHandler.n))

	// StringMatch

	o, err := client.StringMatch(TestType{S: "0"}, 0)
	require.NoError(t, err)
	require.Equal(t, "0", o.S)
	require.Equal(t, 0, o.I)

	_, err = client.StringMatch(TestType{S: "5"}, 5)
	require.EqualError(t, err, ":(")

	o, err = client.StringMatch(TestType{S: "8", I: 8}, 8)
	require.NoError(t, err)
	require.Equal(t, "8", o.S)
	require.Equal(t, 8, o.I)

	// ErrChanSub
	ctx := context.TODO()
	_, err = client.ErrChanSub(ctx)
	if err == nil {
		t.Fatal("expect an err return, but got nil")
	}

	// Invalid client handlers

	var noret struct {
		Add func(int)
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noret, nil)
	require.NoError(t, err)

	// this one should actually work
	noret.Add(4)
	require.Equal(t, 9, int(serverHandler.n))
	closer()

	var noparam struct {
		Add func()
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noparam, nil)
	require.NoError(t, err)

	// shouldn't panic
	noparam.Add()
	closer()

	var erronly struct {
		AddGet func() (int, error)
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &erronly, nil)
	require.NoError(t, err)

	_, err = erronly.AddGet()
	if err == nil || err.Error() != "RPC error (-32602): wrong param count (method 'SimpleServerHandler.AddGet'): 0 != 1" {
		t.Error("wrong error:", err)
	}
	closer()

	var wrongtype struct {
		Add func(string) error
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &wrongtype, nil)
	require.NoError(t, err)

	err = wrongtype.Add("not an int")
	if err == nil || !strings.Contains(err.Error(), "RPC error (-32700):") || !strings.Contains(err.Error(), "json: cannot unmarshal string into Go value of type int") {
		t.Error("wrong error:", err)
	}
	closer()

	var notfound struct {
		NotThere func(string) error
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &notfound, nil)
	require.NoError(t, err)

	err = notfound.NotThere("hello?")
	if err == nil || err.Error() != "RPC error (-32601): method 'SimpleServerHandler.NotThere' not found" {
		t.Error("wrong error:", err)
	}
	closer()
}

func TestRPCHttpClient(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
	}
	closer, err := NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	require.NoError(t, err)
	defer closer()

	// Add(int) error

	require.NoError(t, client.Add(2))
	require.Equal(t, 2, int(serverHandler.n))

	err = client.Add(-3546)
	require.EqualError(t, err, "test")

	// AddGet(int) int

	n := client.AddGet(3)
	require.Equal(t, 5, n)
	require.Equal(t, 5, int(serverHandler.n))

	// StringMatch

	o, err := client.StringMatch(TestType{S: "0"}, 0)
	require.NoError(t, err)
	require.Equal(t, "0", o.S)
	require.Equal(t, 0, o.I)

	_, err = client.StringMatch(TestType{S: "5"}, 5)
	require.EqualError(t, err, ":(")

	o, err = client.StringMatch(TestType{S: "8", I: 8}, 8)
	require.NoError(t, err)
	require.Equal(t, "8", o.S)
	require.Equal(t, 8, o.I)

	// Invalid client handlers

	var noret struct {
		Add func(int)
	}
	closer, err = NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noret, nil)
	require.NoError(t, err)

	// this one should actually work
	noret.Add(4)
	require.Equal(t, 9, int(serverHandler.n))
	closer()

	var noparam struct {
		Add func()
	}
	closer, err = NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noparam, nil)
	require.NoError(t, err)

	// shouldn't panic
	noparam.Add()
	closer()

	var erronly struct {
		AddGet func() (int, error)
	}
	closer, err = NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &erronly, nil)
	require.NoError(t, err)

	_, err = erronly.AddGet()
	if err == nil || err.Error() != "RPC error (-32602): wrong param count (method 'SimpleServerHandler.AddGet'): 0 != 1" {
		t.Error("wrong error:", err)
	}
	closer()

	var wrongtype struct {
		Add func(string) error
	}
	closer, err = NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &wrongtype, nil)
	require.NoError(t, err)

	err = wrongtype.Add("not an int")
	if err == nil || !strings.Contains(err.Error(), "RPC error (-32700):") || !strings.Contains(err.Error(), "json: cannot unmarshal string into Go value of type int") {
		t.Error("wrong error:", err)
	}
	closer()

	var notfound struct {
		NotThere func(string) error
	}
	closer, err = NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &notfound, nil)
	require.NoError(t, err)

	err = notfound.NotThere("hello?")
	if err == nil || err.Error() != "RPC error (-32601): method 'SimpleServerHandler.NotThere' not found" {
		t.Error("wrong error:", err)
	}
	closer()
}

func TestParallelRPC(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add func(int) error
	}
	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	require.NoError(t, err)
	defer closer()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				require.NoError(t, client.Add(2))
			}
		}()
	}
	wg.Wait()

	require.Equal(t, 20000, int(serverHandler.n))
}

type CtxHandler struct {
	lk sync.Mutex

	cancelled      bool
	i              int
	connectionType ConnectionType
}

func (h *CtxHandler) Test(ctx context.Context) {
	h.lk.Lock()
	defer h.lk.Unlock()
	timeout := time.After(300 * time.Millisecond)
	h.i++
	h.connectionType = GetConnectionType(ctx)

	select {
	case <-timeout:
	case <-ctx.Done():
		h.cancelled = true
	}
}

func TestCtx(t *testing.T) {
	// setup server

	serverHandler := &CtxHandler{}

	rpcServer := NewServer()
	rpcServer.Register("CtxHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Test func(ctx context.Context)
	}
	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "CtxHandler", &client, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	client.Test(ctx)
	serverHandler.lk.Lock()

	if !serverHandler.cancelled {
		t.Error("expected cancellation on the server side")
	}
	if serverHandler.connectionType != ConnectionTypeWS {
		t.Error("wrong connection type")
	}

	serverHandler.cancelled = false

	serverHandler.lk.Unlock()
	closer()

	var noCtxClient struct {
		Test func()
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "CtxHandler", &noCtxClient, nil)
	if err != nil {
		t.Fatal(err)
	}

	noCtxClient.Test()

	serverHandler.lk.Lock()

	if serverHandler.cancelled || serverHandler.i != 2 {
		t.Error("wrong serverHandler state")
	}
	if serverHandler.connectionType != ConnectionTypeWS {
		t.Error("wrong connection type")
	}

	serverHandler.lk.Unlock()
	closer()
}

func TestCtxHttp(t *testing.T) {
	// setup server

	serverHandler := &CtxHandler{}

	rpcServer := NewServer()
	rpcServer.Register("CtxHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Test func(ctx context.Context)
	}
	closer, err := NewClient(context.Background(), "http://"+testServ.Listener.Addr().String(), "CtxHandler", &client, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	client.Test(ctx)
	serverHandler.lk.Lock()

	if !serverHandler.cancelled {
		t.Error("expected cancellation on the server side")
	}
	if serverHandler.connectionType != ConnectionTypeHTTP {
		t.Error("wrong connection type")
	}

	serverHandler.cancelled = false

	serverHandler.lk.Unlock()
	closer()

	var noCtxClient struct {
		Test func()
	}
	closer, err = NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "CtxHandler", &noCtxClient, nil)
	if err != nil {
		t.Fatal(err)
	}

	noCtxClient.Test()

	serverHandler.lk.Lock()

	if serverHandler.cancelled || serverHandler.i != 2 {
		t.Error("wrong serverHandler state")
	}
	// connection type should have switched to WS
	if serverHandler.connectionType != ConnectionTypeWS {
		t.Error("wrong connection type")
	}

	serverHandler.lk.Unlock()
	closer()
}

type UnUnmarshalable int

func (*UnUnmarshalable) UnmarshalJSON([]byte) error {
	return errors.New("nope")
}

type UnUnmarshalableHandler struct{}

func (*UnUnmarshalableHandler) GetUnUnmarshalableStuff() (UnUnmarshalable, error) {
	return UnUnmarshalable(5), nil
}

func TestUnmarshalableResult(t *testing.T) {
	var client struct {
		GetUnUnmarshalableStuff func() (UnUnmarshalable, error)
	}

	rpcServer := NewServer()
	rpcServer.Register("Handler", &UnUnmarshalableHandler{})

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Handler", &client, nil)
	require.NoError(t, err)
	defer closer()

	_, err = client.GetUnUnmarshalableStuff()
	require.EqualError(t, err, "RPC client error: unmarshaling result: nope")
}

type ChanHandler struct {
	wait    chan struct{}
	ctxdone <-chan struct{}
}

func (h *ChanHandler) Sub(ctx context.Context, i int, eq int) (<-chan int, error) {
	out := make(chan int)
	h.ctxdone = ctx.Done()

	wait := h.wait

	log.Warnf("SERVER SUB!")
	go func() {
		defer close(out)
		var n int

		for {
			select {
			case <-ctx.Done():
				fmt.Println("ctxdone1", i, eq)
				return
			case <-wait:
				//fmt.Println("CONSUMED WAIT: ", i)
			}

			n += i

			if n == eq {
				fmt.Println("eq")
				return
			}

			select {
			case <-ctx.Done():
				fmt.Println("ctxdone2")
				return
			case out <- n:
			}
		}
	}()

	return out, nil
}

func TestChan(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	require.NoError(t, err)

	defer closer()

	serverHandler.wait <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	require.NoError(t, err)

	// recv one

	require.Equal(t, 2, <-sub)

	// recv many (order)

	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}

	require.Equal(t, 4, <-sub)
	require.Equal(t, 6, <-sub)
	require.Equal(t, 8, <-sub)

	// close (through ctx)
	cancel()

	_, ok := <-sub
	require.Equal(t, false, ok)

	// sub (again)

	serverHandler.wait = make(chan struct{}, 5)
	serverHandler.wait <- struct{}{}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	log.Warnf("last sub")
	sub, err = client.Sub(ctx, 3, 6)
	require.NoError(t, err)

	log.Warnf("waiting for value now")
	require.Equal(t, 3, <-sub)
	log.Warnf("not equal")

	// close (remote)
	serverHandler.wait <- struct{}{}
	_, ok = <-sub
	require.Equal(t, false, ok)
}

func TestChanClosing(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	require.NoError(t, err)

	defer closer()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())

	// sub

	sub1, err := client.Sub(ctx1, 2, -1)
	require.NoError(t, err)

	sub2, err := client.Sub(ctx2, 3, -1)
	require.NoError(t, err)

	// recv one

	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}

	require.Equal(t, 2, <-sub1)
	require.Equal(t, 3, <-sub2)

	cancel1()

	require.Equal(t, 0, <-sub1)
	time.Sleep(time.Millisecond * 50) // make sure the loop has exited (having a shared wait channel makes this annoying)

	serverHandler.wait <- struct{}{}
	require.Equal(t, 6, <-sub2)

	cancel2()
	require.Equal(t, 0, <-sub2)
}

func TestChanServerClose(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	tctx, tcancel := context.WithCancel(context.Background())

	testServ := httptest.NewUnstartedServer(rpcServer)
	testServ.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return tctx
	}
	testServ.Start()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	require.NoError(t, err)

	defer closer()

	serverHandler.wait <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	require.NoError(t, err)

	// recv one

	require.Equal(t, 2, <-sub)

	// make sure we're blocked

	select {
	case <-time.After(200 * time.Millisecond):
	case <-sub:
		t.Fatal("didn't expect to get anything from sub")
	}

	// close server

	tcancel()
	testServ.Close()

	_, ok := <-sub
	require.Equal(t, false, ok)
}

func TestServerChanLockClose(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)

	var closeConn func() error

	_, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(),
		"ChanHandler",
		[]interface{}{&client}, nil,
		func(c *Config) {
			c.proxyConnFactory = func(f func() (*websocket.Conn, error)) func() (*websocket.Conn, error) {
				return func() (*websocket.Conn, error) {
					c, err := f()
					if err != nil {
						return nil, err
					}

					closeConn = c.UnderlyingConn().Close

					return c, nil
				}
			}
		})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	require.NoError(t, err)

	// recv one

	go func() {
		serverHandler.wait <- struct{}{}
	}()
	require.Equal(t, 2, <-sub)

	for i := 0; i < 100; i++ {
		serverHandler.wait <- struct{}{}
	}

	if err := closeConn(); err != nil {
		t.Fatal(err)
	}

	<-serverHandler.ctxdone
}

type StreamingHandler struct {
}

func (h *StreamingHandler) GetData(ctx context.Context, n int) (<-chan int, error) {
	out := make(chan int)

	go func() {
		defer close(out)

		for i := 0; i < n; i++ {
			out <- i
		}
	}()

	return out, nil
}

func TestChanClientReceiveAll(t *testing.T) {
	var client struct {
		GetData func(context.Context, int) (<-chan int, error)
	}

	serverHandler := &StreamingHandler{}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	tctx, tcancel := context.WithCancel(context.Background())

	testServ := httptest.NewUnstartedServer(rpcServer)
	testServ.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return tctx
	}
	testServ.Start()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	require.NoError(t, err)

	defer closer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// sub

	sub, err := client.GetData(ctx, 100)
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		select {
		case v, ok := <-sub:
			if !ok {
				t.Fatal("channel closed", i)
			}

			if v != i {
				t.Fatal("got wrong value", v, i)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for values")
		}
	}

	tcancel()
	testServ.Close()

}

func TestControlChanDeadlock(t *testing.T) {
	if _, exists := os.LookupEnv("GOLOG_LOG_LEVEL"); !exists {
		_ = logging.SetLogLevel("rpc", "error")
		defer func() {
			_ = logging.SetLogLevel("rpc", "DEBUG")
		}()
	}

	for r := 0; r < 20; r++ {
		testControlChanDeadlock(t)
	}
}

func testControlChanDeadlock(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	n := 5000

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, n),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	require.NoError(t, err)

	defer closer()

	for i := 0; i < n; i++ {
		serverHandler.wait <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := client.Sub(ctx, 1, -1)
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			if <-sub != i+1 {
				panic("bad!")
				// require.Equal(t, i+1, <-sub)
			}
		}
	}()

	// reset this channel so its not shared between the sub requests...
	serverHandler.wait = make(chan struct{}, n)
	for i := 0; i < n; i++ {
		serverHandler.wait <- struct{}{}
	}

	_, err = client.Sub(ctx, 2, -1)
	require.NoError(t, err)
	<-done
}

type InterfaceHandler struct {
}

func (h *InterfaceHandler) ReadAll(ctx context.Context, r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func TestInterfaceHandler(t *testing.T) {
	var client struct {
		ReadAll func(ctx context.Context, r io.Reader) ([]byte, error)
	}

	serverHandler := &InterfaceHandler{}

	rpcServer := NewServer(WithParamDecoder(new(io.Reader), readerDec))
	rpcServer.Register("InterfaceHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "InterfaceHandler", []interface{}{&client}, nil, WithParamEncoder(new(io.Reader), readerEnc))
	require.NoError(t, err)

	defer closer()

	read, err := client.ReadAll(context.TODO(), strings.NewReader("pooooootato"))
	require.NoError(t, err)
	require.Equal(t, "pooooootato", string(read), "potatos weren't equal")
}

var (
	readerRegistery   = map[int]io.Reader{}
	readerRegisteryN  = 31
	readerRegisteryLk sync.Mutex
)

func readerEnc(rin reflect.Value) (reflect.Value, error) {
	reader := rin.Interface().(io.Reader)

	readerRegisteryLk.Lock()
	defer readerRegisteryLk.Unlock()

	n := readerRegisteryN
	readerRegisteryN++

	readerRegistery[n] = reader
	return reflect.ValueOf(n), nil
}

func readerDec(ctx context.Context, rin []byte) (reflect.Value, error) {
	var id int
	if err := json.Unmarshal(rin, &id); err != nil {
		return reflect.Value{}, err
	}

	readerRegisteryLk.Lock()
	defer readerRegisteryLk.Unlock()

	return reflect.ValueOf(readerRegistery[id]), nil
}

type ErrSomethingBad struct{}

func (e ErrSomethingBad) Error() string {
	return "something bad has happened"
}

type ErrMyErr struct{ str string }

var _ error = ErrSomethingBad{}

func (e *ErrMyErr) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &e.str)
}

func (e *ErrMyErr) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.str)
}

func (e *ErrMyErr) Error() string {
	return fmt.Sprintf("this happened: %s", e.str)
}

type ErrHandler struct{}

func (h *ErrHandler) Test() error {
	return ErrSomethingBad{}
}

func (h *ErrHandler) TestP() error {
	return &ErrSomethingBad{}
}

func (h *ErrHandler) TestMy(s string) error {
	return &ErrMyErr{
		str: s,
	}
}

func TestUserError(t *testing.T) {
	// setup server

	serverHandler := &ErrHandler{}

	const (
		EBad = iota + FirstUserCode
		EBad2
		EMy
	)

	errs := NewErrors()
	errs.Register(EBad, new(ErrSomethingBad))
	errs.Register(EBad2, new(*ErrSomethingBad))
	errs.Register(EMy, new(*ErrMyErr))

	rpcServer := NewServer(WithServerErrors(errs))
	rpcServer.Register("ErrHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Test   func() error
		TestP  func() error
		TestMy func(s string) error
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ErrHandler", []interface{}{
		&client,
	}, nil, WithErrors(errs))
	require.NoError(t, err)

	e := client.Test()
	require.True(t, xerrors.Is(e, ErrSomethingBad{}))

	e = client.TestP()
	require.True(t, xerrors.Is(e, &ErrSomethingBad{}))

	e = client.TestMy("some event")
	require.Error(t, e)
	require.Equal(t, "this happened: some event", e.Error())
	require.Equal(t, "this happened: some event", e.(*ErrMyErr).Error())

	closer()
}

// Unit test for request/response ID translation.
func TestIDHandling(t *testing.T) {
	var decoded request

	cases := []struct {
		str       string
		expect    interface{}
		expectErr bool
	}{
		{`{"id":"8116d306-56cc-4637-9dd7-39ce1548a5a0","jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, "8116d306-56cc-4637-9dd7-39ce1548a5a0", false},
		{`{"id":1234,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, float64(1234), false},
		{`{"id":null,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, false},
		{`{"id":1234.0,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, 1234.0, false},
		{`{"id":1.2,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, 1.2, false},
		{`{"id":["1"],"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, true},
		{`{"id":{"a":"b"},"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%v", tc.expect), func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tc.str))
			require.NoError(t, dec.Decode(&decoded))
			if id, err := normalizeID(decoded.ID); !tc.expectErr {
				require.NoError(t, err)
				require.Equal(t, tc.expect, id)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestAliasedCall(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("ServName", &SimpleServerHandler{n: 3})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		WhateverMethodName func(int) (int, error) `rpc_method:"ServName.AddGet"`
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Server", []interface{}{
		&client,
	}, nil)
	require.NoError(t, err)

	// do the call!

	n, err := client.WhateverMethodName(1)
	require.NoError(t, err)

	require.Equal(t, 4, n)

	closer()
}

type NotifHandler struct {
	notified chan struct{}
}

func (h *NotifHandler) Notif() {
	close(h.notified)
}

func TestNotif(t *testing.T) {
	tc := func(proto string) func(t *testing.T) {
		return func(t *testing.T) {
			// setup server

			nh := &NotifHandler{
				notified: make(chan struct{}),
			}

			rpcServer := NewServer()
			rpcServer.Register("Notif", nh)

			// httptest stuff
			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			// setup client
			var client struct {
				Notif func() error `notify:"true"`
			}
			closer, err := NewMergeClient(context.Background(), proto+"://"+testServ.Listener.Addr().String(), "Notif", []interface{}{
				&client,
			}, nil)
			require.NoError(t, err)

			// do the call!

			// this will block if it's not sent as a notification
			err = client.Notif()
			require.NoError(t, err)

			<-nh.notified

			closer()
		}
	}

	t.Run("ws", tc("ws"))
	t.Run("http", tc("http"))
}

type RawParamHandler struct {
}

type CustomParams struct {
	I int
}

func (h *RawParamHandler) Call(ctx context.Context, ps RawParams) (int, error) {
	p, err := DecodeParams[CustomParams](ps)
	if err != nil {
		return 0, err
	}
	return p.I + 1, nil
}

func TestCallWithRawParams(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("Raw", &RawParamHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		Call func(ctx context.Context, ps RawParams) (int, error)
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Raw", []interface{}{
		&client,
	}, nil)
	require.NoError(t, err)

	// do the call!

	// this will block if it's not sent as a notification
	n, err := client.Call(context.Background(), []byte(`{"I": 1}`))
	require.NoError(t, err)
	require.Equal(t, 2, n)

	closer()
}

type RevCallTestServerHandler struct {
}

func (h *RevCallTestServerHandler) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxy](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	r, err := revClient.CallOnClient(7) // multiply by 2 on client
	if err != nil {
		return xerrors.Errorf("call on client: %w", err)
	}

	if r != 14 {
		return fmt.Errorf("unexpected result: %d", r)
	}

	return nil
}

type RevCallTestClientProxy struct {
	CallOnClient func(int) (int, error)
}

type RevCallTestClientHandler struct {
}

func (h *RevCallTestClientHandler) CallOnClient(a int) (int, error) {
	return a * 2, nil
}

func TestReverseCall(t *testing.T) {
	// setup server

	rpcServer := NewServer(WithReverseClient[RevCallTestClientProxy]("Client"))
	rpcServer.Register("Server", &RevCallTestServerHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Server", []interface{}{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}))
	require.NoError(t, err)

	// do the call!

	e := client.Call()
	require.NoError(t, e)

	closer()
}

type RevCallTestServerHandlerAliased struct {
}

func (h *RevCallTestServerHandlerAliased) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxyAliased](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	r, err := revClient.CallOnClient(8) // multiply by 2 on client
	if err != nil {
		return xerrors.Errorf("call on client: %w", err)
	}

	if r != 16 {
		return fmt.Errorf("unexpected result: %d", r)
	}

	return nil
}

type RevCallTestClientProxyAliased struct {
	CallOnClient func(int) (int, error) `rpc_method:"rpc_thing"`
}

func TestReverseCallAliased(t *testing.T) {
	// setup server

	rpcServer := NewServer(WithReverseClient[RevCallTestClientProxyAliased]("Client"))
	rpcServer.Register("Server", &RevCallTestServerHandlerAliased{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Server", []interface{}{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}), WithClientHandlerAlias("rpc_thing", "Client.CallOnClient"))
	require.NoError(t, err)

	// do the call!

	e := client.Call()
	require.NoError(t, e)

	closer()
}

// RevCallDropTestServerHandler attempts to make a client call on a closed connection.
type RevCallDropTestServerHandler struct {
	closeConn func()
	res       chan error
}

func (h *RevCallDropTestServerHandler) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxy](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	h.closeConn()
	time.Sleep(time.Second)

	_, err := revClient.CallOnClient(7)
	h.res <- err

	return nil
}

func TestReverseCallDroppedConn(t *testing.T) {
	// setup server

	hnd := &RevCallDropTestServerHandler{
		res: make(chan error),
	}

	rpcServer := NewServer(WithReverseClient[RevCallTestClientProxy]("Client"))
	rpcServer.Register("Server", hnd)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "Server", []interface{}{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}))
	require.NoError(t, err)

	hnd.closeConn = closer

	// do the call!
	e := client.Call()

	require.Error(t, e)
	require.Contains(t, e.Error(), "websocket connection closed")

	res := <-hnd.res
	require.Error(t, res)
	require.Contains(t, res.Error(), "RPC client error: sendRequest failed: websocket routine exiting")
	time.Sleep(100 * time.Millisecond)
}

type BigCallTestServerHandler struct {
}

type RecRes struct {
	I int
	R []RecRes
}

func (h *BigCallTestServerHandler) Do() (RecRes, error) {
	var res RecRes
	res.I = 123

	for i := 0; i < 15000; i++ {
		var ires RecRes
		ires.I = i

		for j := 0; j < 15000; j++ {
			var jres RecRes
			jres.I = j

			ires.R = append(ires.R, jres)
		}

		res.R = append(res.R, ires)
	}

	fmt.Println("sending result")

	return res, nil
}

func (h *BigCallTestServerHandler) Ch(ctx context.Context) (<-chan int, error) {
	out := make(chan int)

	go func() {
		var i int
		for {
			select {
			case <-ctx.Done():
				fmt.Println("closing")
				close(out)
				return
			case <-time.After(time.Second):
			}
			fmt.Println("sending")
			out <- i
			i++
		}
	}()

	return out, nil
}

// TestBigResult tests that the connection doesn't die when sending a large result,
// and that requests which happen while a large result is being sent don't fail.
func TestBigResult(t *testing.T) {
	if os.Getenv("I_HAVE_A_LOT_OF_MEMORY_AND_TIME") != "1" {
		// needs ~40GB of memory and ~4 minutes to run
		t.Skip("skipping test due to required resources, set I_HAVE_A_LOT_OF_MEMORY_AND_TIME=1 to run")
	}

	// setup server

	serverHandler := &BigCallTestServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Do func() (RecRes, error)
		Ch func(ctx context.Context) (<-chan int, error)
	}
	closer, err := NewClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	require.NoError(t, err)
	defer closer()

	chctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// client.Ch will generate some requests, which will require websocket locks,
	// and before fixes in #97 would cause deadlocks / timeouts when combined with
	// the large result processing from client.Do
	ch, err := client.Ch(chctx)
	require.NoError(t, err)

	prevN := <-ch

	go func() {
		for n := range ch {
			if n != prevN+1 {
				panic("bad order")
			}
			prevN = n
		}
	}()

	_, err = client.Do()
	require.NoError(t, err)

	fmt.Println("done")
}

func TestNewCustomClient(t *testing.T) {
	// Setup server
	serverHandler := &SimpleServerHandler{}
	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// Custom doRequest function
	doRequest := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
		reader := bytes.NewReader(body)
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			rpcServer.HandleRequest(ctx, reader, pw)
		}()
		return pr, nil
	}

	var client struct {
		Add    func(int) error
		AddGet func(int) int
	}

	// Create custom client
	closer, err := NewCustomClient("SimpleServerHandler", []interface{}{&client}, doRequest)
	require.NoError(t, err)
	defer closer()

	// Add(int) error
	require.NoError(t, client.Add(10))
	require.Equal(t, int32(10), serverHandler.n)

	err = client.Add(-3546)
	require.EqualError(t, err, "test")

	// AddGet(int) int
	n := client.AddGet(3)
	require.Equal(t, 13, n)
	require.Equal(t, int32(13), serverHandler.n)
}

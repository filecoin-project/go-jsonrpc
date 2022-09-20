go-jsonrpc
==================

[![go.dev reference](https://img.shields.io/badge/go.dev-reference-007d9c?logo=go&logoColor=white&style=flat-square)](https://pkg.go.dev/github.com/filecoin-project/go-jsonrpc)
[![](https://img.shields.io/badge/made%20by-Protocol%20Labs-blue.svg?style=flat-square)](https://protocol.ai)

> Low Boilerplate JSON-RPC 2.0 library

## Usage examples

### Server

```go
// Have a type with some exported methods
type SimpleServerHandler struct {
    n int
}

func (h *SimpleServerHandler) AddGet(in int) int {
    h.n += in
    return h.n
}

func main() {
    // create a new server instance
    rpcServer := jsonrpc.NewServer()
    
    // create a handler instance and register it
    serverHandler := &SimpleServerHandler{}
    rpcServer.Register("SimpleServerHandler", serverHandler)
    
    // rpcServer is now http.Handler which will serve jsonrpc calls to SimpleServerHandler.AddGet
    // a method with a single int param, and an int response. The server supports both http and websockets.
    
    // serve the api
    testServ := httptest.NewServer(rpcServer)
    defer testServ.Close()
	
    fmt.Println("URL: ", "ws://"+testServ.Listener.Addr().String())
    
    [..do other app stuff / wait..]
}
```

### Client
```go
func start() error {
    // Create a struct where each field is an exported function with signatures matching rpc calls
    var client struct {
        AddGet      func(int) int
    }
	
	// Make jsonrp populate func fields in the struct with JSONRPC calls
    closer, err := jsonrpc.NewClient(context.Background(), rpcURL, "SimpleServerHandler", &client, nil)
    if err != nil {
    	return err
    }
    defer closer()
    
    ...
    
    n := client.AddGet(10)
    // if the server is the one from the example above, n = 10

    n := client.AddGet(2)
    // if the server is the one from the example above, n = 12
}
```

### Supported function signatures

```go
type _ interface {
    // No Params / Return val
    Func1()
    
    // With Params
    // Note: If param types implement json.[Un]Marshaler, go-jsonrpc will use it
    Func2(param1 int, param2 string, param3 struct{A int})
    
    // Returning errors
    // * For some connection errors, go-jsonrpc will return jsonrpc.RPCConnectionError{}.
    // * RPC-returned errors will be constructed with basic errors.New(__"string message"__)
    // * JSON-RPC error codes can be mapped to typed errors with jsonrpc.Errors - https://pkg.go.dev/github.com/filecoin-project/go-jsonrpc#Errors
    // * For typed errors to work, server needs to be constructed with the `WithServerErrors`
    //   option, and the client needs to be constructed with the `WithErrors` option
    Func3() error
    
    // Returning a value
    // Note: The value must be serializable with encoding/json.
    Func4() int
    
    // Returning a value and an error
    // Note: if the handler returns an error and a non-zero value, the value will not
    //       be returned to the client - the client will see a zero value.
    Func4() (int, error)
    
    // With context
    // * Context isn't passed as JSONRPC param, instead it has a number of different uses
    // * When the context is cancelled on the client side, context cancellation should propagate to the server handler
    //   * In http mode the http request will be aborted
    //   * In websocket mode the client will send a `xrpc.cancel` with a single param containing ID of the cancelled request
    // * If the context contains an opencensus trace span, it will be propagated to the server through a
    //   `"Meta": {"SpanContext": base64.StdEncoding.EncodeToString(propagation.Binary(span.SpanContext()))}` field in
    //   the jsonrpc request
    //   
    Func5(ctx context.Context, param1 string) error
    
    // With non-json-serializable (e.g. interface) params
    // * There are client and server options which make it possible to register transformers for types
    //   to make them json-(de)serializable
    // * Server side: jsonrpc.WithParamDecoder(new(io.Reader), func(ctx context.Context, b []byte) (reflect.Value, error) { ... }
    // * Client side: jsonrpc.WithParamEncoder(new(io.Reader), func(value reflect.Value) (reflect.Value, error) { ... }
    // * For io.Reader specifically there's a simple param encoder/decoder implementation in go-jsonrpc/httpio package
    //   which will pass reader data through separate http streams on a different hanhler.
    // * Note: a similar mechanism for return value transformation isn't supported yet
    Func6(r io.Reader)
    
    // Returning a channel
    // * Only supported in websocket mode
    // * If no error is returned, the return value will be an int channelId
    // * When the server handler writes values into the channel, the client will receive `xrpc.ch.val` notifications
    //   with 2 params: [chanID: int, value: any]
    // * When the channel is closed the client will receive `xrpc.ch.close` notification with a single param: [chanId: int]
    // * The client-side channel will be closed when the websocket connection breaks; Server side will discard writes to
    //   the channel. Handlers should rely on the context to know when to stop writing to the returned channel.
    // NOTE: There is no good backpressure mechanism implemented for channels, returning values faster that the client can
    // receive them may cause memory leaks.
    Func7(ctx context.Context, param1 int, param2 string) (<-chan int, error)
}

```

## Contribute

PRs are welcome!

## License

Dual-licensed under [MIT](https://github.com/filecoin-project/go-jsonrpc/blob/master/LICENSE-MIT) + [Apache 2.0](https://github.com/filecoin-project/go-jsonrpc/blob/master/LICENSE-APACHE)

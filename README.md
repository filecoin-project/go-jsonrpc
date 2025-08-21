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

### Custom Transport Feature
The go-jsonrpc library supports creating clients with custom transport mechanisms (e.g. use for IPC). This allows for greater flexibility in how requests are sent and received, enabling the use of custom protocols, special handling of requests, or integration with other systems.

#### Example Usage of Custom Transport

Here is an example demonstrating how to create a custom client with a custom transport mechanism:
    
```go
// Setup server
serverHandler := &SimpleServerHandler{} // some type with methods

rpcServer := jsonrpc.NewServer()
rpcServer.Register("SimpleServerHandler", serverHandler)

// Custom doRequest function
doRequest := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
    reader := bytes.NewReader(body)
    pr, pw := io.Pipe()
    go func() {
        defer pw.Close()
        rpcServer.HandleRequest(ctx, reader, pw) // handle the rpc frame
    }()
    return pr, nil
}

var client struct {
    Add    func(int) error
}

// Create custom client
closer, err := jsonrpc.NewCustomClient("SimpleServerHandler", []interface{}{&client}, doRequest)
if err != nil {
    log.Fatalf("Failed to create client: %v", err)
}
defer closer()

// Use the client
if err := client.Add(10); err != nil {
    log.Fatalf("Failed to call Add: %v", err)
}
fmt.Printf("Current value: %d\n", client.AddGet(5))
```

### Reverse Calling Feature
The go-jsonrpc library also supports reverse calling, where the server can make calls to the client. This is useful in scenarios where the server needs to notify, request data from the client, or for subscriptions (e.g. `eth_subscribe`).

NOTE: Reverse calling only works in websocket mode

#### Example Usage of Reverse Calling

Here is an example demonstrating how to set up reverse calling:

```go
// Define the client handler interface
type ClientHandler struct {
    CallOnClient func(int) (int, error)
}

// Define the server handler
type ServerHandler struct {}

func (h *ServerHandler) Call(ctx context.Context) error {
    revClient, ok := jsonrpc.ExtractReverseClient[ClientHandler](ctx)
    if !ok {
        return fmt.Errorf("no reverse client")
    }

    result, err := revClient.CallOnClient(7) // Multiply by 2 on client
    if err != nil {
        return fmt.Errorf("call on client: %w", err)
    }

    if result != 14 {
        return fmt.Errorf("unexpected result: %d", result)
    }

    return nil
}

// Define client handler
type RevCallTestClientHandler struct {
}

func (h *RevCallTestClientHandler) CallOnClient(a int) (int, error) {
    return a * 2, nil
}

// Setup server with reverse client capability
rpcServer := jsonrpc.NewServer(jsonrpc.WithReverseClient[ClientHandler]("Client"))
rpcServer.Register("ServerHandler", &ServerHandler{})

testServ := httptest.NewServer(rpcServer)
defer testServ.Close()

// Setup client with reverse call handler
var client struct {
    Call func() error
}

closer, err := jsonrpc.NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "ServerHandler", []interface{}{
    &client,
}, nil, jsonrpc.WithClientHandler("Client", &RevCallTestClientHandler{}))
if err != nil {
    log.Fatalf("Failed to create client: %v", err)
}
defer closer()

// Make a call from the client to the server, which will trigger a reverse call
if err := client.Call(); err != nil {
    log.Fatalf("Failed to call server: %v", err)
}
```

## Options

### Using method name formatters

#### Using `WithServerMethodNameFormatter`

`WithServerMethodNameFormatter` allows you to customize a function that formats the JSON-RPC method name, given namespace and method name.

There are four possible out-of-the-box options:
- `jsonrpc.DefaultMethodNameFormatter` - default method name formatter, e.g. `SimpleServerHandler.AddGet`
- `jsonrpc.NewMethodNameFormatter(true, jsonrpc.LowerFirstCharCase)` - method name formatter with namespace, e.g. `SimpleServerHandler.addGet`
- `jsonrpc.NewMethodNameFormatter(false, jsonrpc.OriginalCase)` - method name formatter without namespace, e.g. `AddGet`
- `jsonrpc.NewMethodNameFormatter(false, jsonrpc.LowerFirstCharCase)` - method name formatter without namespace and with the first char lowercased, e.g. `addGet`

> [!NOTE]  
> The default method name formatter concatenates the namespace and method name with a dot.
> Go exported methods are capitalized, so, the method name will be capitalized as well.
> e.g. `SimpleServerHandler.AddGet` (capital "A" in "AddGet")

You can also create your own method name formatter by creating a function that implements the `jsonrpc.MethodNameFormatter` interface.

```go
func main() {
	// create a new server instance with a custom separator
	rpcServer := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(
		func(namespace, method string) string {
			return namespace + "_" + method
		}),
	)

	// create a handler instance and register it
	serverHandler := &SimpleServerHandler{}
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// serve the api
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	fmt.Println("URL: ", "ws://"+testServ.Listener.Addr().String())

	// rpc method becomes SimpleServerHandler_AddGet

    [..do other app stuff / wait..]
}
```

#### Using `WithMethodNameFormatter`

`WithMethodNameFormatter` is the client-side counterpart to `WithServerMethodNameFormatter`.

```go
func main() {
    closer, err := NewMergeClient(
        context.Background(),
        "http://example.com",
        "SimpleServerHandler",
        []any{&client},
        nil,
        WithMethodNameFormatter(jsonrpc.NewMethodNameFormatter(false, OriginalCase)),
    )
    defer closer()
}
```

#### Using `WithClientHandlerFormatter`

Same as `WithMethodNameFormatter`, but for client handlers. Using it you can fully customize the JSON-RPC method name for client handlers,
given namespace and method name.

```go
func main() {
    closer, err := jsonrpc.NewMergeClient(
        context.Background(),
        "http://example.com",
        "SimpleServerHandler",
        []any{&client},
        nil,
        jsonrpc.WithMethodNameFormatter(jsonrpc.NewMethodNameFormatter(false, OriginalCase)),
        jsonrpc.WithClientHandler("Client", &RevCallTestClientHandler{}),
        jsonrpc.WithClientHandlerFormatter(jsonrpc.NewMethodNameFormatter(false, OriginalCase)),
    )
    defer closer()
}
```
### Using method name alias

You can also create an alias for a method name. This is useful if you want to use a different method name in the JSON-RPC
request than the actual method name for a specific method.

#### Usage of method name alias in the server

```go
type SimpleServerHandler struct {}

func (h *SimpleServerHandler) Double(in int) int {
    return in * 2
}

// create a new server instance
rpcServer := jsonrpc.NewServer()

// create a handler instance and register it
serverHandler := &SimpleServerHandler{}
rpcServer.Register("SimpleServerHandler", serverHandler)

// create an alias for the Double method. This will allow you to call the server's Double method
// with the name "rand_myRandomAlias" in the JSON-RPC request.
rpcServer.AliasMethod("rand_myRandomAlias", "SimpleServerHandler.Double")

```

#### Usage of method name alias with client handlers

```go
// setup the client handler
type ReverseHandler struct {}

func (h *ReverseHandler) DoubleOnClient(in int) int {
    return in * 2
}

// create a new client instance with the client handler + method name alias
closer, err := jsonrpc.NewMergeClient(
    context.Background(),
    "http://example.com",
    "SimpleServerHandler",
    []any{&client},
    nil,
    jsonrpc.WithClientHandler("Client", &ReverseHandler{}),
    // this allows the server to call the client's DoubleOnClient method using the name "rand_theClientRandomAlias" in the JSON-RPC request.
    jsonrpc.WithClientHandlerAlias("rand_theClientRandomAlias", "Client.DoubleOnClient"),
)
```

#### Usage of a struct tag to define method name alias

There are two cases where you can also use the `rpc_method` struct tag to define method name alias:
in the client struct and in the reverse handler struct in the server. 

In the client struct:
```go
// setup the client struct
var client struct {
    AddInt func(int) int `rpc_method:"rand_aRandomAlias"`
}

// create a new client instance with the client struct that has the `rpc_method` struct tag
closer, err := jsonrpc.NewMergeClient(
    context.Background(),
    "http://example.com",
    "SimpleServerHandler",
    []any{&client},
    nil,
)

// since we defined the method name alias in the client struct, this will send a JSON-RPC request with "rand_aRandomAlias" as the method name to the
// server instead of "SimpleServerHandler.AddInt".
result, err := client.AddInt(10)

```

In the server's reverse handler struct:

```go
// Define the client handler interface
type ClientHandler struct {
    CallOnClient func(int) (int, error) `rpc_method:"rand_theClientRandomAlias"`
}

// Define the server handler
type ServerHandler struct {}

func (h *ServerHandler) Call(ctx context.Context) (int, error) {
    revClient, _ := jsonrpc.ExtractReverseClient[ClientHandler](ctx)

    // Reverse call to the client.
    // Since we defined the method name alias in the client handler struct tag, this
    // will send a JSON-RPC request with "rand_theClientRandomAlias" as the method name to the
    // client instead of "Client.CallOnClient".
    result, err := revClient.CallOnClient(7)
    
    // ...
}

// Setup server with reverse client capability
rpcServer := jsonrpc.NewServer(jsonrpc.WithReverseClient[ClientHandler]("Client"))
rpcServer.Register("ServerHandler", &ServerHandler{})
```


## Contribute

PRs are welcome!

## License

Dual-licensed under [MIT](https://github.com/filecoin-project/go-jsonrpc/blob/master/LICENSE-MIT) + [Apache 2.0](https://github.com/filecoin-project/go-jsonrpc/blob/master/LICENSE-APACHE)

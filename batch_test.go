package jsonrpc

import (
	"context"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

type op struct {
	Name  string
	Value int
}
type TestServer struct {
	count int
	log   []op
}

func (testServer *TestServer) Add(val int) error {
	testServer.count += val
	testServer.log = append(testServer.log, op{"add", val})
	return nil
}

func (testServer *TestServer) Sub(val int) error {
	testServer.count += val
	testServer.log = append(testServer.log, op{"sub", val})
	return nil
}

func (testServer *TestServer) Get() (int, error) {
	testServer.log = append(testServer.log, op{"get", testServer.count})
	return testServer.count, nil
}

func (testServer *TestServer) Logs() ([]op, error) {
	testServer.log = append(testServer.log, op{"logs", 0})
	return testServer.log, nil
}

func TestBatchRequest_Batch(t *testing.T) {
	rpcHandler := TestServer{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	var rpcClient struct {
		Add   func(int) error
		Batch func(ctx context.Context, batchParams []BatchParams, results *[]*BatchResult) error
	}

	closer, err := NewMergeClient(context.Background(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", []interface{}{&rpcClient}, nil)
	require.NoError(t, err)
	defer closer()
	results := []*BatchResult{
		{},
		{},
		{},
		{},
		{
			Result: new(int),
		},
		{
			Result: new([]op),
		},
	}
	err = rpcClient.Batch(context.Background(), []BatchParams{
		{
			Method: "Add",
			Params: []interface{}{1},
		},
		{
			Method: "Add",
			Params: []interface{}{1},
		},
		{
			Method: "Add",
			Params: []interface{}{1},
		},
		{
			Method: "Sub",
			Params: []interface{}{1},
		},
		{
			Method: "Get",
			Params: []interface{}{},
		},
		{
			Method: "Logs",
			Params: []interface{}{},
		},
	}, &results)
	require.NoError(t, err)
	if len(results) != 6 {
		t.Errorf("expect %d results but got %d", 5, len(results))
	}

	if *results[4].Result.(*int) != 4 {
		t.Errorf("expect result %d but got %d", 5, *results[4].Result.(*int))
	}
	if len(*results[5].Result.(*[]op)) != 6 {
		t.Errorf("expect op number %d but got %d", 6, len(results[5].Result.([]op)))
	}
}

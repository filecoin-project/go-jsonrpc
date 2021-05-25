package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/filecoin-project/go-jsonrpc/auth"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWsBuilder(t *testing.T) {
	nameSpace := "Test"
	sev := NewServer(WithProxyBind(PBField))
	var fullNode FullAdapter
	auth.PermissionedProxyWithMethod(validPerms, defaultPerms, &mockAPI1{}, &fullNode)
	auth.PermissionedProxyWithMethod(validPerms, defaultPerms, &mockAPI2{}, &fullNode)
	sev.register(nameSpace, &fullNode)
	testServ := httptest.NewServer(sev)
	var client FullAdapter
	closer, err := NewClient(
		context.Background(),
		"ws://"+testServ.Listener.Addr().String(),
		nameSpace,
		&client,
		nil)
	require.NoError(t, err)
	defer closer()

	result, err := client.Test1(context.Background())
	require.NoError(t, err)
	assert.Equal(t, result, "test")
}

func TestJsonRPC(t *testing.T) {
	nameSpace := "Test"
	sev := NewServer(WithProxyBind(PBField))
	var fullNode FullAdapter
	auth.PermissionedProxyWithMethod(validPerms, defaultPerms, &mockAPI1{}, &fullNode)
	sev.register(nameSpace, &fullNode)
	testServ := httptest.NewServer(sev)
	defer testServ.Close()

	http.Handle("/rpc/v0", sev)

	req := struct {
		Jsonrpc string            `json:"jsonrpc"`
		ID      int64             `json:"id,omitempty"`
		Method  string            `json:"method"`
		Meta    map[string]string `json:"meta,omitempty"`
	}{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "Test.Test1",
	}
	reqBytes, err := json.Marshal(req)
	require.NoError(t, err)
	httpRes, err := http.Post("http://"+testServ.Listener.Addr().String()+"/rpc/v0", "", bytes.NewReader(reqBytes))
	require.NoError(t, err)
	assert.Equal(t, httpRes.Status, "200 OK")
	result, err := ioutil.ReadAll(httpRes.Body)
	require.NoError(t, err)
	res := struct {
		Result string `json:"result"`
	}{}
	err = json.Unmarshal(result, &res)
	require.NoError(t, err)
	assert.Equal(t, res.Result, "test")
}

var _ MockAPI1 = &mockAPI1{}

type MockAPI1 interface {
	Test1(ctx context.Context) (string, error)
}

type MockAPI2 interface {
	Test2(ctx context.Context) error
}

type mockAPI1 struct {
}

func (m *mockAPI1) Test1(ctx context.Context) (string, error) {
	return "test", nil
}

var _ MockAPI2 = &mockAPI2{}

type mockAPI2 struct {
}

func (m *mockAPI2) Test2(ctx context.Context) error {
	return nil
}

// FullAdapter client struct
// there is no need to implement the MockAPI1 and MockAPI2 interface
type FullAdapter struct {
	CommonAdapter
	Adapter2
}

type CommonAdapter struct {
	Adapter1
}

type Adapter1 struct {
	Test1 func(ctx context.Context) (string, error) `perm:"read"`
}

type Adapter2 struct {
	Test2 func(ctx context.Context) error `perm:"read"`
}

var (
	validPerms   = []auth.Permission{"read", "write", "sign", "admin"}
	defaultPerms = []auth.Permission{"read"}
)

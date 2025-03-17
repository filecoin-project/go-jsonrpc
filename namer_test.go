package jsonrpc

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDifferentMethodNamers(t *testing.T) {
	tests := map[string]struct {
		namer MethodNamer

		requestedMethod string
	}{
		"default namer": {
			namer:           DefaultMethodNamer,
			requestedMethod: "SimpleServerHandler.Inc",
		},
		"no namespace namer": {
			namer:           NoNamespaceMethodNamer,
			requestedMethod: "Inc",
		},
		"no namespace & decapitalized namer": {
			namer:           NoNamespaceDecapitalizedMethodNamer,
			requestedMethod: "inc",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNamer(test.namer))

			serverHandler := &SimpleServerHandler{}
			rpcServer.Register("SimpleServerHandler", serverHandler)

			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			req := fmt.Sprintf(`{"jsonrpc": "2.0", "method": "%s", "params": [], "id": 1}`, test.requestedMethod)

			res, err := http.Post(testServ.URL, "application/json", strings.NewReader(req))
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, res.StatusCode)
			require.Equal(t, int32(1), serverHandler.n)
		})
	}
}

func TestDifferentMethodNamersWithClient(t *testing.T) {
	tests := map[string]struct {
		namer     MethodNamer
		urlPrefix string
	}{
		"default namer & http": {
			namer:     DefaultMethodNamer,
			urlPrefix: "http://",
		},
		"default namer & ws": {
			namer:     DefaultMethodNamer,
			urlPrefix: "ws://",
		},
		"no namespace namer & http": {
			namer:     NoNamespaceMethodNamer,
			urlPrefix: "http://",
		},
		"no namespace namer & ws": {
			namer:     NoNamespaceMethodNamer,
			urlPrefix: "ws://",
		},
		"no namespace & decapitalized namer & http": {
			namer:     NoNamespaceDecapitalizedMethodNamer,
			urlPrefix: "http://",
		},
		"no namespace & decapitalized namer & ws": {
			namer:     NoNamespaceDecapitalizedMethodNamer,
			urlPrefix: "ws://",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNamer(test.namer))

			serverHandler := &SimpleServerHandler{}
			rpcServer.Register("SimpleServerHandler", serverHandler)

			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			var client struct {
				AddGet func(int) int
			}

			closer, err := NewMergeClient(
				context.Background(),
				test.urlPrefix+testServ.Listener.Addr().String(),
				"SimpleServerHandler",
				[]any{&client},
				nil,
				WithHTTPClient(testServ.Client()),
				WithMethodNamer(test.namer),
			)
			require.NoError(t, err)
			defer closer()

			n := client.AddGet(123)
			require.Equal(t, 123, n)
		})
	}
}

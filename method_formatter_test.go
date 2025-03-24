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
		namer MethodNameFormatter

		requestedMethod string
	}{
		"default namer": {
			namer:           DefaultMethodNameFormatter,
			requestedMethod: "SimpleServerHandler.Inc",
		},
		"no namespace namer": {
			namer:           NoNamespaceMethodNameFormatter,
			requestedMethod: "Inc",
		},
		"no namespace & decapitalized namer": {
			namer:           NoNamespaceDecapitalizedMethodNameFormatter,
			requestedMethod: "inc",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNameFormatter(test.namer))

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
		namer     MethodNameFormatter
		urlPrefix string
	}{
		"default namer & http": {
			namer:     DefaultMethodNameFormatter,
			urlPrefix: "http://",
		},
		"default namer & ws": {
			namer:     DefaultMethodNameFormatter,
			urlPrefix: "ws://",
		},
		"no namespace namer & http": {
			namer:     NoNamespaceMethodNameFormatter,
			urlPrefix: "http://",
		},
		"no namespace namer & ws": {
			namer:     NoNamespaceMethodNameFormatter,
			urlPrefix: "ws://",
		},
		"no namespace & decapitalized namer & http": {
			namer:     NoNamespaceDecapitalizedMethodNameFormatter,
			urlPrefix: "http://",
		},
		"no namespace & decapitalized namer & ws": {
			namer:     NoNamespaceDecapitalizedMethodNameFormatter,
			urlPrefix: "ws://",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNameFormatter(test.namer))

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
				WithMethodNameFormatter(test.namer),
			)
			require.NoError(t, err)
			defer closer()

			n := client.AddGet(123)
			require.Equal(t, 123, n)
		})
	}
}

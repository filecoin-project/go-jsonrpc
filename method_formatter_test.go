package jsonrpc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
		"lower fist char": {
			namer:           NewMethodNameFormatter(true, LowerFirstCharCase),
			requestedMethod: "SimpleServerHandler.inc",
		},
		"no namespace namer": {
			namer:           NewMethodNameFormatter(false, OriginalCase),
			requestedMethod: "Inc",
		},
		"no namespace & lower fist char": {
			namer:           NewMethodNameFormatter(false, LowerFirstCharCase),
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
		"lower first char namer & http": {
			namer:     NewMethodNameFormatter(true, LowerFirstCharCase),
			urlPrefix: "http://",
		},
		"lower first char namer & ws": {
			namer:     NewMethodNameFormatter(true, LowerFirstCharCase),
			urlPrefix: "ws://",
		},
		"no namespace namer & http": {
			namer:     NewMethodNameFormatter(false, OriginalCase),
			urlPrefix: "http://",
		},
		"no namespace namer & ws": {
			namer:     NewMethodNameFormatter(false, OriginalCase),
			urlPrefix: "ws://",
		},
		"no namespace & lower first char & http": {
			namer:     NewMethodNameFormatter(false, LowerFirstCharCase),
			urlPrefix: "http://",
		},
		"no namespace & lower first char & ws": {
			namer:     NewMethodNameFormatter(false, LowerFirstCharCase),
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

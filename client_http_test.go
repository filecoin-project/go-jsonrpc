package jsonrpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type trackedHTTPResponseBody struct {
	io.Reader
	reads  int
	closes int
}

func (b *trackedHTTPResponseBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *trackedHTTPResponseBody) Close() error {
	b.closes++
	return nil
}

type httpResponseTransport struct {
	status int
	body   *trackedHTTPResponseBody
}

func (t httpResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	_ = r.Body.Close()
	return &http.Response{
		StatusCode: t.status,
		Status:     fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Header:     make(http.Header),
		Body:       t.body,
		Request:    r,
	}, nil
}

func TestHTTPClientClosesRejectedResponses(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 502, 503} {
		for _, notify := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d_notify_%t", status, notify), func(t *testing.T) {
				body := &trackedHTTPResponseBody{Reader: strings.NewReader("upstream error")}
				var client struct {
					Call   func() error
					Notify func() error `notify:"true"`
				}
				closer, err := NewMergeClient(context.Background(), "http://example.com", "test", []any{&client}, nil,
					WithHTTPClient(&http.Client{Transport: httpResponseTransport{status: status, body: body}}))
				require.NoError(t, err)
				defer closer()

				call := client.Call
				if notify {
					call = client.Notify
				}
				err = call()
				require.ErrorContains(t, err, fmt.Sprintf("request failed, http status %d %s", status, http.StatusText(status)))
				require.Equal(t, 1, body.closes)
				require.Zero(t, body.reads)
			})
		}
	}
}

func TestHTTPClientClosesAcceptedResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		response   string
		notify     bool
		errorText  string
		serverCode ErrorCode
	}{
		{name: "success", status: 200, response: `{"jsonrpc":"2.0","id":1,"result":null}`},
		{name: "notification", status: 204, notify: true},
		{name: "bad_request", status: 400, response: `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"invalid request"}}`, errorText: "invalid request", serverCode: -32600},
		{name: "server_error", status: 500, response: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`, errorText: "internal error", serverCode: -32603},
		{name: "invalid_json", status: 200, response: "not json", errorText: "unmarshaling response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedHTTPResponseBody{Reader: strings.NewReader(tc.response)}
			var client struct {
				Call   func() error
				Notify func() error `notify:"true"`
			}
			closer, err := NewMergeClient(context.Background(), "http://example.com", "test", []any{&client}, nil,
				WithHTTPClient(&http.Client{Transport: httpResponseTransport{status: tc.status, body: body}}))
			require.NoError(t, err)
			defer closer()

			call := client.Call
			if tc.notify {
				call = client.Notify
			}
			err = call()
			if tc.errorText == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.errorText)
				if tc.serverCode != 0 {
					var rpcErr *JSONRPCError
					require.ErrorAs(t, err, &rpcErr)
					require.Equal(t, tc.serverCode, rpcErr.Code)
				}
			}
			require.Equal(t, 1, body.closes)
		})
	}
}

package jsonrpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// RPCServer provides a jsonrpc 2.0 http server handler
type RPCServer struct {
	*handler
	reverseClientBuilder func(context.Context, *wsConn) (context.Context, error)

	pingInterval time.Duration
}

// NewServer creates new RPCServer instance
func NewServer(opts ...ServerOption) *RPCServer {
	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	return &RPCServer{
		handler:              makeHandler(config),
		reverseClientBuilder: config.reverseClientBuilder,

		pingInterval: config.pingInterval,
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (s *RPCServer) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// TODO: allow setting
	// (note that we still are mostly covered by jwt tokens)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Header.Get("Sec-WebSocket-Protocol") != "" {
		w.Header().Set("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Errorw("upgrading connection", "error", err)
		// note that upgrader.Upgrade will set http error if there is an error
		return
	}

	wc := &wsConn{
		conn:         c,
		handler:      s,
		pingInterval: s.pingInterval,
		exiting:      make(chan struct{}),
	}

	if s.reverseClientBuilder != nil {
		ctx, err = s.reverseClientBuilder(ctx, wc)
		if err != nil {
			log.Errorf("failed to build reverse client: %s", err)
			w.WriteHeader(500)
			return
		}
	}

	lbl := pprof.Labels("jrpc-mode", "wsserver", "jrpc-remote", r.RemoteAddr, "jrpc-uuid", uuid.New().String())
	pprof.Do(ctx, lbl, func(ctx context.Context) {
		wc.handleWsConn(ctx)
	})

	if err := c.Close(); err != nil {
		log.Errorw("closing websocket connection", "error", err)
		return
	}
}

// TODO: return errors to clients per spec
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		s.handleWS(ctx, w, r)
		return
	}

	s.handleReader(ctx, r.Body, w, rpcError)
}

func rpcError(wf func(func(io.Writer)), req *request, code ErrorCode, err error) {
	log.Errorf("RPC Error: %s", err)
	wf(func(w io.Writer) {
		if hw, ok := w.(http.ResponseWriter); ok {
			if code == rpcInvalidRequest {
				hw.WriteHeader(400)
			} else {
				hw.WriteHeader(500)
			}
		}

		log.Warnf("rpc error: %s", err)

		if req == nil {
			req = &request{}
		}

		resp := response{
			Jsonrpc: "2.0",
			ID:      req.ID,
			Error: &respError{
				Code:    code,
				Message: err.Error(),
			},
		}

		err = json.NewEncoder(w).Encode(resp)
		if err != nil {
			log.Warnf("failed to write rpc error: %s", err)
			return
		}
	})
}

// Register registers new RPC handler
//
// Handler is any value with methods defined
func (s *RPCServer) Register(namespace string, handler interface{}) {
	s.register(namespace, handler)
}

func (s *RPCServer) AliasMethod(alias, original string) {
	s.aliasedMethods[alias] = original
}

var _ error = &respError{}

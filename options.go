package jsonrpc

import (
	"net/http"
	"reflect"
	"time"

	"github.com/gorilla/websocket"
)

type ParamEncoder func(reflect.Value) (reflect.Value, error)

type clientHandler struct {
	ns  string
	hnd interface{}
}

type Config struct {
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration

	paramEncoders map[reflect.Type]ParamEncoder
	errors        *Errors

	reverseHandlers       []clientHandler
	aliasedHandlerMethods map[string]string

	httpClient *http.Client

	noReconnect      bool
	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing
}

func defaultConfig() Config {
	return Config{
		reconnectBackoff: backoff{
			minDelay: 100 * time.Millisecond,
			maxDelay: 5 * time.Second,
		},
		pingInterval: 5 * time.Second,
		timeout:      30 * time.Second,

		aliasedHandlerMethods: map[string]string{},

		paramEncoders: map[reflect.Type]ParamEncoder{},

		httpClient: _defaultHTTPClient,
	}
}

type Option func(c *Config)

func WithReconnectBackoff(minDelay, maxDelay time.Duration) func(c *Config) {
	return func(c *Config) {
		c.reconnectBackoff = backoff{
			minDelay: minDelay,
			maxDelay: maxDelay,
		}
	}
}

// Must be < Timeout/2
func WithPingInterval(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.pingInterval = d
	}
}

func WithTimeout(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.timeout = d
	}
}

func WithNoReconnect() func(c *Config) {
	return func(c *Config) {
		c.noReconnect = true
	}
}

func WithParamEncoder(t interface{}, encoder ParamEncoder) func(c *Config) {
	return func(c *Config) {
		c.paramEncoders[reflect.TypeOf(t).Elem()] = encoder
	}
}

func WithErrors(es Errors) func(c *Config) {
	return func(c *Config) {
		c.errors = &es
	}
}

func WithClientHandler(ns string, hnd interface{}) func(c *Config) {
	return func(c *Config) {
		c.reverseHandlers = append(c.reverseHandlers, clientHandler{ns, hnd})
	}
}

// WithClientHandlerAlias creates an alias for a client HANDLER method - for handlers created
// with WithClientHandler
func WithClientHandlerAlias(alias, original string) func(c *Config) {
	return func(c *Config) {
		c.aliasedHandlerMethods[alias] = original
	}
}

func WithHTTPClient(h *http.Client) func(c *Config) {
	return func(c *Config) {
		c.httpClient = h
	}
}

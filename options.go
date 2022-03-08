package jsonrpc

import (
	"net"
	"net/http"
	"reflect"
	"time"

	"github.com/gorilla/websocket"
)

var _defaultTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

type ParamEncoder func(reflect.Value) (reflect.Value, error)

type Config struct {
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration

	paramEncoders map[reflect.Type]ParamEncoder

	noReconnect      bool
	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing

	transport *http.Transport
}

func defaultConfig() Config {
	return Config{
		reconnectBackoff: backoff{
			minDelay: 100 * time.Millisecond,
			maxDelay: 5 * time.Second,
		},
		pingInterval: 5 * time.Second,
		timeout:      30 * time.Second,

		paramEncoders: map[reflect.Type]ParamEncoder{},

		transport: _defaultTransport,
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

func WithTransportMaxIdleConns(value int) func(c *Config) {
	return func(c *Config) {
		c.transport.MaxIdleConns = value
	}
}

func WithTransportMaxIdleConnsPerHost(value int) func(c *Config) {
	return func(c *Config) {
		c.transport.MaxIdleConnsPerHost = value
	}
}

func WithTransportIdleConnTimeout(valueSecond time.Duration) func(c *Config) {
	return func(c *Config) {
		c.transport.IdleConnTimeout = valueSecond * time.Second
	}
}

func WithTransportTLSHandshakeTimeout(valueSecond time.Duration) func(c *Config) {
	return func(c *Config) {
		c.transport.TLSHandshakeTimeout = valueSecond * time.Second
	}
}

func WithTransportDialContext(timeoutSecond, keepAliveSecond time.Duration) func(c *Config) {
	return func(c *Config) {
		c.transport.DialContext = (&net.Dialer{
			Timeout:   timeoutSecond * time.Second,
			KeepAlive: keepAliveSecond * time.Second,
			DualStack: true,
		}).DialContext
	}
}

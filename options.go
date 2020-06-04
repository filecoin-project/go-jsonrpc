package jsonrpc

import (
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	reconnectBackoff backoff

	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing
}

var defaultConfig = Config{
	reconnectBackoff: backoff{
		minDelay: 100 * time.Millisecond,
		maxDelay: 5 * time.Second,
	},
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

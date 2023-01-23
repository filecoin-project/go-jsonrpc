package jsonrpc

import (
	"context"
	"reflect"
	"time"
)

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

type ServerConfig struct {
	maxRequestSize int64
	pingInterval   time.Duration

	paramDecoders map[reflect.Type]ParamDecoder
	errors        *Errors
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		paramDecoders:  map[reflect.Type]ParamDecoder{},
		maxRequestSize: DEFAULT_MAX_REQUEST_SIZE,

		pingInterval: 5 * time.Second,
	}
}

func WithParamDecoder(t interface{}, decoder ParamDecoder) ServerOption {
	return func(c *ServerConfig) {
		c.paramDecoders[reflect.TypeOf(t).Elem()] = decoder
	}
}

func WithMaxRequestSize(max int64) ServerOption {
	return func(c *ServerConfig) {
		c.maxRequestSize = max
	}
}

func WithServerErrors(es Errors) ServerOption {
	return func(c *ServerConfig) {
		c.errors = &es
	}
}

func WithServerPingInterval(d time.Duration) ServerOption {
	return func(c *ServerConfig) {
		c.pingInterval = d
	}
}

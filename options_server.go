package jsonrpc

import (
	"context"
	"reflect"
	"time"
)

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

type ServerConfig struct {
	paramDecoders  map[reflect.Type]ParamDecoder
	maxRequestSize int64
	timeout        time.Duration
	pingInterval   time.Duration
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		paramDecoders:  map[reflect.Type]ParamDecoder{},
		maxRequestSize: DEFAULT_MAX_REQUEST_SIZE,
		timeout:        time.Second * 30,
		pingInterval:   time.Second * 5,
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

func WithServerTimeout(timeout time.Duration) ServerOption {
	return func(c *ServerConfig) {
		c.timeout = timeout
	}
}

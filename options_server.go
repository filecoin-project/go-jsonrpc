package jsonrpc

import (
	"context"
	"reflect"
)

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

type ServerConfig struct {
	paramDecoders  map[reflect.Type]ParamDecoder
	maxRequestSize int64
	errors         *Errors
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		paramDecoders:  map[reflect.Type]ParamDecoder{},
		maxRequestSize: DEFAULT_MAX_REQUEST_SIZE,
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

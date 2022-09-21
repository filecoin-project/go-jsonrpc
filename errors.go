package jsonrpc

import (
	"encoding/json"
	"reflect"
)

const eTempWSError = -1111111

type RPCConnectionError struct {
	err error
}

func (e *RPCConnectionError) Error() string {
	return e.err.Error()
}

func (e *RPCConnectionError) Unwrap() error {
	return e.err
}

type Errors struct {
	byType map[reflect.Type]ErrorCode
	byCode map[ErrorCode]reflect.Type
}

type ErrorCode int

const FirstUserCode = 2

func NewErrors() Errors {
	return Errors{
		byType: map[reflect.Type]ErrorCode{},
		byCode: map[ErrorCode]reflect.Type{
			-1111111: reflect.TypeOf(&RPCConnectionError{}),
		},
	}
}

func (e *Errors) Register(c ErrorCode, typ interface{}) {
	rt := reflect.TypeOf(typ).Elem()
	if !rt.Implements(errorType) {
		panic("can't register non-error types")
	}

	e.byType[rt] = c
	e.byCode[c] = rt
}

type marshalable interface {
	json.Marshaler
	json.Unmarshaler
}

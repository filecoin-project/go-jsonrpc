package jsonrpc

import (
	"encoding/json"
	"errors"
	"reflect"
)

const eTempWSError = -1111111

type RPCConnectionError struct {
	err error
}

func (e *RPCConnectionError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "RPCConnectionError"
}

func (e *RPCConnectionError) Unwrap() error {
	if e.err != nil {
		return e.err
	}
	return errors.New("RPCConnectionError")
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

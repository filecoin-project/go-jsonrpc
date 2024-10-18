package jsonrpc

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Define the error types
type SimpleError struct {
	Message string
}

func (e *SimpleError) Error() string {
	return e.Message
}

type DataError struct {
	Message string
	Data    interface{}
}

func (e *DataError) Error() string {
	return e.Message
}

func (e *DataError) ErrorData() interface{} {
	return e.Data
}

type MetaError struct {
	Message string
	Details string
}

func (e *MetaError) Error() string {
	return e.Message
}

func (e *MetaError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Details string `json:"details"`
	}{
		Details: e.Details,
	})
}

func (e *MetaError) UnmarshalJSON(data []byte) error {
	var temp struct {
		Details string `json:"details"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	e.Details = temp.Details
	return nil
}

type ComplexError struct {
	Message string
	Data    interface{}
	Details string
}

func (e *ComplexError) Error() string {
	return e.Message
}

func (e *ComplexError) ErrorData() interface{} {
	return e.Data
}

func (e *ComplexError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Message string      `json:"message"`
		Details string      `json:"details"`
		Data    interface{} `json:"data"`
	}{
		Details: e.Details,
		Message: e.Message,
		Data:    e.Data,
	})
}

func (e *ComplexError) UnmarshalJSON(data []byte) error {
	var temp struct {
		Message string      `json:"message"`
		Details string      `json:"details"`
		Data    interface{} `json:"data"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	e.Details = temp.Details
	e.Message = temp.Message
	e.Data = temp.Data
	return nil
}

func TestRespErrorVal(t *testing.T) {
	// Initialize the Errors struct and register error types
	errorsMap := NewErrors()
	errorsMap.Register(1001, new(*SimpleError))
	errorsMap.Register(1002, new(*DataError))
	errorsMap.Register(1003, new(*MetaError))
	errorsMap.Register(1004, new(*ComplexError))

	// Define test cases
	testCases := []struct {
		name         string
		respError    *respError
		expectedType reflect.Type
		verify       func(err error) error
	}{
		{
			name: "SimpleError",
			respError: &respError{
				Code:    1001,
				Message: "simple error occurred",
			},
			expectedType: reflect.TypeOf(&SimpleError{}),
			verify: func(err error) error {
				require.IsType(t, &SimpleError{}, err)
				require.Equal(t, "simple error occurred", err.Error())
				return nil
			},
		},
		{
			name: "DataError",
			respError: &respError{
				Code:    1002,
				Message: "data error occurred",
				Data:    "additional data",
			},
			expectedType: reflect.TypeOf(&DataError{}),
			verify: func(err error) error {
				require.IsType(t, &DataError{}, err)
				require.Equal(t, "data error occurred", err.Error())
				require.Equal(t, "additional data", err.(*DataError).ErrorData())
				return nil
			},
		},
		{
			name: "MetaError",
			respError: &respError{
				Code:    1003,
				Message: "meta error occurred",
				Meta: func() json.RawMessage {
					me := &MetaError{
						Details: "meta details",
					}
					metaData, _ := me.MarshalJSON()
					return metaData
				}(),
			},
			expectedType: reflect.TypeOf(&MetaError{}),
			verify: func(err error) error {
				require.IsType(t, &MetaError{}, err)
				require.Equal(t, "meta error occurred", err.Error())
				// details will also be included in the error message since it implements the marshable interface
				require.Equal(t, "meta details", err.(*MetaError).Details)
				return nil
			},
		},
		{
			name: "ComplexError",
			respError: &respError{
				Code:    1004,
				Message: "complex error occurred",
				Data:    "complex data",
				Meta: func() json.RawMessage {
					ce := &ComplexError{
						Details: "complex details",
					}
					metaData, _ := ce.MarshalJSON()
					return metaData
				}(),
			},
			expectedType: reflect.TypeOf(&ComplexError{}),
			verify: func(err error) error {
				require.IsType(t, &ComplexError{}, err)
				require.Equal(t, "complex error occurred", err.Error())
				require.Equal(t, "complex data", err.(*ComplexError).ErrorData())
				require.Equal(t, "complex details", err.(*ComplexError).Details)
				return nil
			},
		},
		{
			name: "UnregisteredError",
			respError: &respError{
				Code:    9999,
				Message: "unregistered error occurred",
				Data:    "some data",
			},
			expectedType: reflect.TypeOf(&respError{}),
			verify: func(err error) error {
				require.IsType(t, &respError{}, err)
				require.Equal(t, "unregistered error occurred", err.Error())
				require.Equal(t, "some data", err.(*respError).ErrorData())
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errValue := tc.respError.val(&errorsMap)
			errInterface := errValue.Interface()
			err, ok := errInterface.(error)
			if !ok {
				t.Fatalf("returned value does not implement error interface")
			}

			if reflect.TypeOf(err) != tc.expectedType {
				t.Errorf("expected error type %v, got %v", tc.expectedType, reflect.TypeOf(err))
			}

			err = tc.verify(err)
			require.NoError(t, err, "failed to verify error")
		})
	}
}

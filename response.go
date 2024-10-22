package jsonrpc

import (
	"encoding/json"
	"fmt"
	"reflect"
)

type response struct {
	Jsonrpc string        `json:"jsonrpc"`
	Result  interface{}   `json:"result,omitempty"`
	ID      interface{}   `json:"id"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

func (r response) MarshalJSON() ([]byte, error) {
	// Custom marshal logic as per JSON-RPC 2.0 spec:
	// > `result`:
	// > This member is REQUIRED on success.
	// > This member MUST NOT exist if there was an error invoking the method.
	//
	// > `error`:
	// > This member is REQUIRED on error.
	// > This member MUST NOT exist if there was no error triggered during invocation.
	data := map[string]interface{}{
		"jsonrpc": r.Jsonrpc,
		"id":      r.ID,
	}

	if r.Error != nil {
		data["error"] = r.Error
	} else {
		data["result"] = r.Result
	}
	return json.Marshal(data)
}

type JSONRPCError struct {
	Code    ErrorCode       `json:"code"`
	Message string          `json:"message"`
	Meta    json.RawMessage `json:"meta,omitempty"`
	Data    interface{}     `json:"data,omitempty"`
}

func (e *JSONRPCError) Error() string {
	if e.Code >= -32768 && e.Code <= -32000 {
		return fmt.Sprintf("RPC error (%d): %s", e.Code, e.Message)
	}
	return e.Message
}

var (
	_               error = (*JSONRPCError)(nil)
	marshalableRT         = reflect.TypeOf(new(marshalable)).Elem()
	unmarshalableRT       = reflect.TypeOf(new(ErrorCodec)).Elem()
)

func (e *JSONRPCError) val(errors *Errors) reflect.Value {
	if errors != nil {
		t, ok := errors.byCode[e.Code]
		if ok {
			var v reflect.Value
			if t.Kind() == reflect.Ptr {
				v = reflect.New(t.Elem())
			} else {
				v = reflect.New(t)
			}

			if v.Type().Implements(unmarshalableRT) {
				_ = v.Interface().(ErrorCodec).FromJSONRPCError(*e)
			}

			if len(e.Meta) > 0 && v.Type().Implements(marshalableRT) {
				_ = v.Interface().(marshalable).UnmarshalJSON(e.Meta)
			}

			if t.Kind() != reflect.Ptr {
				v = v.Elem()
			}
			return v
		}
	}

	return reflect.ValueOf(e)
}

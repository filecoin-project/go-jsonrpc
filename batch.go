package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/filecoin-project/go-jsonrpc/metrics"
	"go.opencensus.io/stats"
	"reflect"
)

const BatchName = "Batch"

type ServerBatchParams struct {
	Method string  `json:"method"`
	Params []param `json:"params"`
}

type BatchResult struct {
	Result interface{} `json:"result,omitempty"`
	Error  *respError  `json:"error,omitempty"`
}

type BatchRequest struct {
	methods map[string]rpcHandler

	aliasedMethods map[string]string

	paramDecoders map[reflect.Type]ParamDecoder
}

func (batchRequst *BatchRequest) Batch(ctx context.Context, batchParams []*ServerBatchParams) ([]*BatchResult, error) {
	batchResult := make([]*BatchResult, len(batchParams))
	for index, param := range batchParams {
		batchResult[index] = batchRequst.handleCall(ctx, param)
	}
	return batchResult, nil
}

func (batchRequst *BatchRequest) handleCall(ctx context.Context, param *ServerBatchParams) *BatchResult {
	handler, ok := batchRequst.methods[param.Method]
	if !ok {
		aliasTo, ok := batchRequst.aliasedMethods[param.Method]
		if ok {
			handler, ok = batchRequst.methods[aliasTo]
		}
		if !ok {
			stats.Record(ctx, metrics.RPCInvalidMethod.M(1))
			return &BatchResult{
				Result: nil,
				Error: &respError{
					Code:    rpcMethodNotFound,
					Message: fmt.Sprintf("method '%s' not found", param.Method),
				},
			}
		}
	}

	if len(param.Params) != handler.nParams {
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return &BatchResult{
			Result: nil,
			Error: &respError{
				Code:    rpcInvalidParams,
				Message: fmt.Sprintf("wrong param count (method '%s'): %d != %d", param.Method, len(param.Params), handler.nParams),
			},
		}
	}

	outCh := handler.valOut != -1 && handler.handlerFunc.Type().Out(handler.valOut).Kind() == reflect.Chan
	if outCh {
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return &BatchResult{
			Result: nil,
			Error: &respError{
				Code:    rpcMethodNotFound,
				Message: fmt.Sprintf("method '%s' not supported in this mode (no out channel support)", param.Method),
			},
		}
	}

	callParams := make([]reflect.Value, 1+handler.hasCtx+handler.nParams)
	callParams[0] = handler.receiver
	if handler.hasCtx == 1 {
		callParams[1] = reflect.ValueOf(ctx)
	}

	for i := 0; i < handler.nParams; i++ {
		var rp reflect.Value

		typ := handler.paramReceivers[i]
		dec, found := batchRequst.paramDecoders[typ]
		if !found {
			rp = reflect.New(typ)
			if err := json.NewDecoder(bytes.NewReader(param.Params[i].data)).Decode(rp.Interface()); err != nil {
				stats.Record(ctx, metrics.RPCRequestError.M(1))
				return &BatchResult{
					Result: nil,
					Error: &respError{
						Code:    rpcParseError,
						Message: fmt.Sprintf("unmarshaling params for '%s' (param: %T): %v", param.Method, rp.Interface(), err),
					},
				}
			}
			rp = rp.Elem()
		} else {
			var err error
			rp, err = dec(ctx, param.Params[i].data)
			if err != nil {
				stats.Record(ctx, metrics.RPCRequestError.M(1))
				return &BatchResult{
					Result: nil,
					Error: &respError{
						Code:    rpcParseError,
						Message: fmt.Sprintf("decoding params for '%s' (param: %d; custom decoder): %v", param.Method, i, err),
					},
				}
			}
		}

		callParams[i+1+handler.hasCtx] = reflect.ValueOf(rp.Interface())
	}

	///////////////////

	callResult, err := doCall(param.Method, handler.handlerFunc, callParams)
	if err != nil {
		stats.Record(ctx, metrics.RPCRequestError.M(1))
		return &BatchResult{
			Result: nil,
			Error: &respError{
				Code:    0,
				Message: fmt.Sprintf("fatal error calling '%s': %v", param.Method, err),
			},
		}
	}

	///////////////////

	if handler.errOut != -1 {
		err := callResult[handler.errOut].Interface()
		if err != nil {
			stats.Record(ctx, metrics.RPCResponseError.M(1))
			return &BatchResult{
				Result: nil,
				Error: &respError{
					Code:    0,
					Message: fmt.Sprintf("fatal error calling '%s': %v", param.Method, err),
				},
			}
		}
	}
	if handler.valOut != -1 {
		return &BatchResult{
			Result: callResult[handler.valOut].Interface(),
			Error:  nil,
		}
	}
	return &BatchResult{}
}

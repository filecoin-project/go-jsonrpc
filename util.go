package jsonrpc

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"time"
)

type param struct {
	data []byte // from unmarshal

	v reflect.Value // to marshal
}

func (p *param) UnmarshalJSON(raw []byte) error {
	p.data = make([]byte, len(raw))
	copy(p.data, raw)
	return nil
}

func (p *param) MarshalJSON() ([]byte, error) {
	if p.v.Kind() == reflect.Invalid {
		return p.data, nil
	}

	return json.Marshal(p.v.Interface())
}

// processFuncOut finds value and error Outs in function
func processFuncOut(funcType reflect.Type) (valOut int, errOut int, n int) {
	errOut = -1 // -1 if not found
	valOut = -1
	n = funcType.NumOut()

	switch n {
	case 0:
	case 1:
		if funcType.Out(0) == errorType {
			errOut = 0
		} else {
			valOut = 0
		}
	case 2:
		valOut = 0
		errOut = 1
		if funcType.Out(1) != errorType {
			panic("expected error as second return value")
		}
	default:
		errstr := fmt.Sprintf("too many return values: %s", funcType)
		panic(errstr)
	}

	return
}

type backoff struct {
	minDelay time.Duration
	maxDelay time.Duration
}

func (b *backoff) next(attempt int) time.Duration {
	if attempt < 0 {
		return b.minDelay
	}

	minf := float64(b.minDelay)
	durf := minf * math.Pow(1.5, float64(attempt))
	durf = durf + rand.Float64()*minf

	delay := time.Duration(durf)

	if delay > b.maxDelay || delay < 0 { //overflow
		return b.maxDelay
	}

	return delay
}

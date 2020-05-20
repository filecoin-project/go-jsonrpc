package metrics

import (
	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

// Global Tags
var (
	RPCMethod, _ = tag.NewKey("method")
)

// Measures
var (
	RPCInvalidMethod = stats.Int64("rpc/invalid_method", "Total number of invalid RPC methods called", stats.UnitDimensionless)
	RPCRequestError  = stats.Int64("rpc/request_error", "Total number of request errors handled", stats.UnitDimensionless)
	RPCResponseError = stats.Int64("rpc/response_error", "Total number of responses errors handled", stats.UnitDimensionless)
)

var (
	// All RPC related metrics should at the very least tag the RPCMethod
	RPCInvalidMethodView = &view.View{
		Measure:     RPCInvalidMethod,
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{RPCMethod},
	}
	RPCRequestErrorView = &view.View{
		Measure:     RPCRequestError,
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{RPCMethod},
	}
	RPCResponseErrorView = &view.View{
		Measure:     RPCResponseError,
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{RPCMethod},
	}
)

// DefaultViews is an array of OpenCensus views for metric gathering purposes
var DefaultViews = []*view.View{
	RPCInvalidMethodView,
	RPCRequestErrorView,
	RPCResponseErrorView,
}

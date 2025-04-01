package jsonrpc

import "strings"

// MethodNameFormatter is a function that takes a namespace and a method name and returns the full method name, sent via JSON-RPC.
// This is useful if you want to customize the default behaviour, e.g. send without the namespace or make it lowercase.
type MethodNameFormatter func(namespace, method string) string

// CaseStyle represents the case style for method names.
type CaseStyle int

const (
	OriginalCase CaseStyle = iota
	LowerFirstCharCase
)

// NewMethodNameFormatter creates a new method name formatter based on the provided options.
func NewMethodNameFormatter(includeNamespace bool, nameCase CaseStyle) MethodNameFormatter {
	return func(namespace, method string) string {
		formattedMethod := method
		if nameCase == LowerFirstCharCase && len(method) > 0 {
			formattedMethod = strings.ToLower(method[:1]) + method[1:]
		}
		if includeNamespace {
			return namespace + "." + formattedMethod
		}
		return formattedMethod
	}
}

// DefaultMethodNameFormatter is a pass-through formatter with default options.
var DefaultMethodNameFormatter = NewMethodNameFormatter(true, OriginalCase)

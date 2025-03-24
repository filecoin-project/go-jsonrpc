package jsonrpc

import "strings"

// MethodNameFormatter is a function that takes a namespace and a method name and returns the full method name, sent via JSON-RPC.
// This is useful if you want to customize the default behaviour, e.g. send without the namespace or make it lowercase.
type MethodNameFormatter func(namespace, method string) string

// DefaultMethodNameFormatter joins the namespace and method name with a dot.
func DefaultMethodNameFormatter(namespace, method string) string {
	return namespace + "." + method
}

// NoNamespaceMethodNameFormatter returns the method name as is, without the namespace.
func NoNamespaceMethodNameFormatter(_, method string) string {
	return method
}

// NoNamespaceDecapitalizedMethodNameFormatter returns the method name as is, without the namespace, and decapitalizes the first letter.
// e.g. "Inc" -> "inc"
func NoNamespaceDecapitalizedMethodNameFormatter(_, method string) string {
	if len(method) == 0 {
		return ""
	}
	return strings.ToLower(method[:1]) + method[1:]
}

package jsonrpc

import "strings"

// MethodNamer is a function that takes a namespace and a method name and returns the full method name, sent via JSON-RPC.
// This is useful if you want to customize the default behaviour, e.g. send without the namespace or make it lowercase.
type MethodNamer func(string, string) string

// DefaultMethodNamer joins the namespace and method name with a dot.
func DefaultMethodNamer(namespace, method string) string {
	return namespace + "." + method
}

// NoNamespaceMethodNamer returns the method name as is, without the namespace.
func NoNamespaceMethodNamer(_, method string) string {
	return method
}

// NoNamespaceDecapitalizedMethodNamer returns the method name as is, without the namespace, and decapitalizes the first letter.
func NoNamespaceDecapitalizedMethodNamer(_, method string) string {
	if len(method) == 0 {
		return ""
	}
	return strings.ToLower(method[:1]) + method[1:]
}

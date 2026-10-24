package dsl

import (
	"testing"
)

// FuzzQueryString tests that the DSL parser handles random
// query_string inputs without panicking. The parser should either
// produce a valid AST or return an error — never crash.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.9
func FuzzQueryString(f *testing.F) {
	// Seeds: valid and edge-case query strings.
	f.Add([]byte(`{"query": {"query_string": {"query": "title:hello AND body:world"}}}`))
	f.Add([]byte(`{"query": {"query_string": {"query": "a OR b OR c"}}}`))
	f.Add([]byte(`{"query": {"query_string": {"query": ""}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"query": {"query_string": {"query": "field:value* AND NOT other:thing~2"}}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// ParseSearchBody should not panic on any input.
		_, _, _, err := ParseSearchBody(data)
		_ = err // error is expected for invalid inputs
	})
}

// Package errors defines UniSearch's REST error envelope. Every handler
// must surface failures through APIError so that error bodies stay
// ES-compatible (design 00-overview §6.1); hand-assembled JSON in handlers
// is forbidden.
package errors

import (
	"encoding/json"
	"fmt"
)

// APIError mirrors the Elasticsearch error body; unsupported_capability is
// a UniSearch extension field carrying migration guidance for capabilities
// that are recognized but not implemented yet.
type APIError struct {
	Status      int              `json:"-"`    // HTTP status: 400/404/409/429/500/501
	Type        string           `json:"type"` // ES-aligned type, e.g. illegal_argument
	Reason      string           `json:"reason"`
	Index       string           `json:"index,omitempty"`
	Unsupported *UnsupportedInfo `json:"unsupported_capability,omitempty"`
}

// UnsupportedInfo describes when and how an unsupported capability is
// planned to become available.
type UnsupportedInfo struct {
	Feature    string `json:"feature"`              // e.g. "query.intervals.unordered"
	Milestone  string `json:"milestone"`            // e.g. "v1.2 (M1)"
	Workaround string `json:"workaround,omitempty"` // suggested ES-compatible rewrite
}

// Error implements the error interface with a compact human-readable form.
// It never includes the full response body; details travel via ResponseBody.
func (e *APIError) Error() string {
	if e.Index != "" {
		return fmt.Sprintf("[%s] %s: %s", e.Index, e.Type, e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Reason)
}

// ResponseBody returns the wire-format JSON body for HTTP responses,
// wrapped in the ES-style "error" key:
//
//	{"error":{"type":...,"reason":...,"index":...,"unsupported_capability":...}}
//
// It only fails on structurally invalid APIError values (never on user
// input), in which case a valid fallback body is produced so the HTTP layer
// always has something to send.
func (e *APIError) ResponseBody() []byte {
	body, err := json.Marshal(struct {
		Error *APIError `json:"error"`
	}{Error: e})
	if err != nil {
		return []byte(`{"error":{"type":"internal_error","reason":"failed to serialize error body"}}`)
	}
	return body
}

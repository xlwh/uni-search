package errors

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestR1_ErrorBodyStructure verifies the ES-style error envelope required
// by spec R1: unsupported capabilities serialize with type/reason plus the
// unsupported_capability block and carry HTTP 400; plain errors omit the
// unsupported_capability key entirely.
func TestR1_ErrorBodyStructure(t *testing.T) {
	tests := []struct {
		name              string
		err               *APIError
		wantStatus        int
		wantType          string
		wantFeature       string
		wantMilestone     string
		wantWorkaround    string
		wantNoUnsupported bool
	}{
		{
			name:           "unsupported capability returns 400 with full guidance block",
			err:            NewUnsupported("query.intervals.unordered", "v1.2 (M1)", "rewrite as match_phrase"),
			wantStatus:     400,
			wantType:       "unsupported_capability",
			wantFeature:    "query.intervals.unordered",
			wantMilestone:  "v1.2 (M1)",
			wantWorkaround: "rewrite as match_phrase",
		},
		{
			name:              "illegal argument returns 400 without unsupported block",
			err:               NewIllegalArgument("from + size must be less than or equal to 10000"),
			wantStatus:        400,
			wantType:          "illegal_argument",
			wantNoUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// APIError must satisfy the error interface.
			var err error = tt.err
			assert.NotEmpty(t, err.Error())

			assert.Equal(t, tt.wantStatus, tt.err.Status, "HTTP status must match the registry value")

			// Body parses into the ES envelope {"error":{...}}.
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(tt.err.ResponseBody(), &raw))
			require.Contains(t, raw, "error", "body must wrap the payload in an \"error\" key")

			var body APIError
			require.NoError(t, json.Unmarshal(raw["error"], &body))
			assert.Equal(t, tt.wantType, body.Type)
			assert.NotEmpty(t, body.Reason, "reason must be non-empty")

			if tt.wantNoUnsupported {
				// With omitempty the key must be absent from the raw JSON,
				// not merely null.
				var inner map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw["error"], &inner))
				assert.NotContains(t, inner, "unsupported_capability")
			} else {
				require.NotNil(t, body.Unsupported, "unsupported errors must carry the guidance block")
				assert.Equal(t, tt.wantFeature, body.Unsupported.Feature)
				assert.Equal(t, tt.wantMilestone, body.Unsupported.Milestone)
				assert.Equal(t, tt.wantWorkaround, body.Unsupported.Workaround)
			}
		})
	}
}

// TestAPIErrorRegistry covers the remaining registry constructors so the
// status/type mapping stays pinned.
func TestAPIErrorRegistry(t *testing.T) {
	tests := []struct {
		name       string
		err        *APIError
		wantStatus int
		wantType   string
	}{
		{"index not found", NewIndexNotFound("logs-2026"), 404, "index_not_found_exception"},
		{"document parsing", NewDocumentParsing("failed to parse field [a.b]"), 400, "document_parsing_exception"},
		{"version conflict", NewVersionConflict("logs", "version conflict, current seq_no [8]"), 409, "version_conflict_engine_exception"},
		{"internal", NewInternal("wal fsync failed"), 500, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantStatus, tt.err.Status)
			assert.Equal(t, tt.wantType, tt.err.Type)
			assert.NotEmpty(t, tt.err.Reason)
		})
	}
}

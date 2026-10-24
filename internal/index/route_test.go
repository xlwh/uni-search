package index

import (
	"testing"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.4
func TestRouteForField(t *testing.T) {
	tests := []struct {
		name    string
		esType  meta.FieldType
		want    []IndexKind
	}{
		{"text → inverted", meta.TypeText, []IndexKind{KindInverted}},
		{"dense_vector → hnsw", meta.TypeDenseVector, []IndexKind{KindHNSW}},
		{"keyword → bitmap", meta.TypeKeyword, []IndexKind{KindBitmap}},
		{"long → bkd", meta.TypeLong, []IndexKind{KindBKD}},
		{"date → bkd", meta.TypeDate, []IndexKind{KindBKD}},
		{"double → bkd", meta.TypeDouble, []IndexKind{KindBKD}},
		{"boolean → bitmap", meta.TypeBoolean, []IndexKind{KindBitmap}},
		{"ip → bkd", meta.TypeIP, []IndexKind{KindBKD}},
		{"geo_point → bkd", meta.TypeGeoPoint, []IndexKind{KindBKD}},
		{"unsupported → empty", meta.TypeObject, []IndexKind{}},
		{"unknown → empty", meta.FieldType("nonexistent"), []IndexKind{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RouteForField(tt.esType)
			if !kindsEqual(got, tt.want) {
				t.Errorf("RouteForField(%q) = %v, want %v", tt.esType, got, tt.want)
			}
		})
	}
}

func kindsEqual(a, b []IndexKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

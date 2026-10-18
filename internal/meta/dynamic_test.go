package meta

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/errors"
)

// requireAPIError asserts err is an *errors.APIError and returns it.
func requireAPIError(t *testing.T, err error) *errors.APIError {
	t.Helper()
	require.Error(t, err)
	apiErr, ok := err.(*errors.APIError)
	require.True(t, ok, "expected *errors.APIError, got %T: %v", err, err)
	return apiErr
}

// TestR3_DynamicTypeInference covers the R3 dynamic inference table
// (design 01 §1.3): bool→boolean, integer→long, float→double, ISO date
// string→date, plain string→text+keyword multi-field, object→recursive
// object mapping, array→type of its first element.
func TestR3_DynamicTypeInference(t *testing.T) {
	m := NewMapping()
	require.Equal(t, DynamicTrue, m.Dynamic)

	doc := map[string]any{
		"flag":    true,
		"count":   json.Number("42"),
		"score":   json.Number("3.14"),
		"created": "2026-10-17T10:30:00Z",
		"title":   "hello world",
		"user": map[string]any{
			"name": "alice",
			"vip":  true,
		},
		"tags": []any{json.Number("1"), json.Number("2")},
	}
	require.NoError(t, ApplyDynamic(m, doc))

	assertField := func(path string, want FieldType) *FieldDef {
		t.Helper()
		fd := m.Field(path)
		if fd == nil {
			t.Fatalf("field %q missing after dynamic apply", path)
		}
		assert.Equalf(t, want, fd.Type, "field %q", path)
		assert.Truef(t, fd.Index, "field %q must default to index=true", path)
		assert.Truef(t, fd.DocValues, "field %q must default to doc_values=true", path)
		return fd
	}

	assertField("flag", TypeBoolean)
	assertField("count", TypeLong)
	assertField("score", TypeDouble)
	assertField("created", TypeDate)

	title := assertField("title", TypeText)
	assert.Equal(t, "standard", title.Analyzer, "text fields default to the standard analyzer")
	kw := title.Fields["keyword"]
	require.NotNil(t, kw, "dynamic text fields must get a keyword multi-field")
	assert.Equal(t, TypeKeyword, kw.Type)
	assert.Equal(t, 256, kw.IgnoreAbove)
	assert.True(t, kw.Index)

	// object values recurse into an object mapping
	user := assertField("user", TypeObject)
	require.NotNil(t, user.Properties["name"])
	assert.Equal(t, TypeText, user.Properties["name"].Type)
	assert.NotNil(t, user.Properties["name"].Fields["keyword"], "nested text gets the keyword sub-field too")
	require.NotNil(t, user.Properties["vip"])
	assert.Equal(t, TypeBoolean, user.Properties["vip"].Type)

	// arrays are not a type: the first element decides
	assertField("tags", TypeLong)

	// a second write merges new keys into the existing object mapping
	require.NoError(t, ApplyDynamic(m, map[string]any{
		"user": map[string]any{"age": json.Number("30")},
	}))
	assertField("user.age", TypeLong)

	// arrays of objects merge every element's keys into one object field
	require.NoError(t, ApplyDynamic(m, map[string]any{
		"actors": []any{
			map[string]any{"first": json.Number("1")},
			map[string]any{"second": true},
		},
	}))
	actors := assertField("actors", TypeObject)
	require.NotNil(t, actors.Properties["first"])
	assert.Equal(t, TypeLong, actors.Properties["first"].Type)
	require.NotNil(t, actors.Properties["second"])
	assert.Equal(t, TypeBoolean, actors.Properties["second"].Type)

	// nested arrays flatten to the first scalar's type
	require.NoError(t, ApplyDynamic(m, map[string]any{
		"matrix": []any{[]any{json.Number("1"), json.Number("2")}},
	}))
	assertField("matrix", TypeLong)

	// nulls and empty arrays introduce no fields
	require.NoError(t, ApplyDynamic(m, map[string]any{
		"nothing": nil,
		"empty":   []any{},
	}))
	assert.Nil(t, m.Field("nothing"))
	assert.Nil(t, m.Field("empty"))

	// InferType mirrors the same table for single values
	for _, tc := range []struct {
		value any
		want  FieldType
	}{
		{true, TypeBoolean},
		{json.Number("7"), TypeLong},
		{json.Number("7.5"), TypeDouble},
		{"2026-10-17", TypeDate},
		{"2026-10-17T10:30", TypeDate},
		{"plain string", TypeText},
		{map[string]any{"a": true}, TypeObject},
		{[]any{json.Number("1")}, TypeLong}, // array → first element type
	} {
		got, ok := InferType(tc.value)
		require.Truef(t, ok, "InferType(%v) must infer a type", tc.value)
		assert.Equalf(t, tc.want, got, "InferType(%v)", tc.value)
	}
	if _, ok := InferType(nil); ok {
		t.Error("null must not introduce a field")
	}
	if _, ok := InferType([]any{}); ok {
		t.Error("empty arrays must not introduce a field")
	}
}

// TestR3_MappingConflict covers the R3 conflict rules (design 01 §1.3):
// values incompatible with an existing mapping are rejected with a 400
// document_parsing_exception; dynamic:false keeps unknown fields in
// _source only; dynamic:strict rejects unknown fields outright.
func TestR3_MappingConflict(t *testing.T) {
	longDef := func(name string) *FieldDef {
		return &FieldDef{Name: name, Type: TypeLong, Index: true, DocValues: true}
	}

	t.Run("KnownLongFieldRejectsString", func(t *testing.T) {
		m := NewMapping()
		m.Properties["count"] = longDef("count")
		err := ApplyDynamic(m, map[string]any{"count": "abc"})
		apiErr := requireAPIError(t, err)
		assert.Equal(t, 400, apiErr.Status)
		assert.Equal(t, errors.TypeDocumentParsing, apiErr.Type)
		assert.Contains(t, apiErr.Reason, "count")
		assert.Contains(t, apiErr.Reason, "long")
		// the mapping is untouched by the rejected write
		assert.Nil(t, m.Field("count.fields"))
	})

	t.Run("DynamicFalseKeepsUnknownFieldsSourceOnly", func(t *testing.T) {
		m := NewMapping()
		m.Dynamic = DynamicFalse
		m.Properties["known"] = longDef("known")
		err := ApplyDynamic(m, map[string]any{
			"known":         json.Number("1"),
			"unknown_field": "kept in _source only",
		})
		require.NoError(t, err)
		assert.Len(t, m.Properties, 1, "dynamic:false must not create mappings for unknown fields")
		assert.Nil(t, m.Field("unknown_field"))

		// known fields are still validated under dynamic:false
		err = ApplyDynamic(m, map[string]any{"known": "abc"})
		requireAPIError(t, err)
	})

	t.Run("DynamicStrictRejectsUnknownFields", func(t *testing.T) {
		m := NewMapping()
		m.Dynamic = DynamicStrict
		m.Properties["known"] = longDef("known")
		err := ApplyDynamic(m, map[string]any{
			"known": json.Number("1"),
			"rogue": "not allowed",
		})
		apiErr := requireAPIError(t, err)
		assert.Equal(t, 400, apiErr.Status)
		assert.Equal(t, errors.TypeDocumentParsing, apiErr.Type)
		assert.Contains(t, apiErr.Reason, "strict")
		assert.Contains(t, apiErr.Reason, "rogue")

		// documents that only touch known fields pass
		require.NoError(t, ApplyDynamic(m, map[string]any{"known": json.Number("2")}))

		// strict also applies inside known object subtrees
		m2 := NewMapping()
		m2.Dynamic = DynamicStrict
		user := newFieldDef("user", TypeObject)
		user.Properties["name"] = newFieldDef("user.name", TypeText)
		m2.Properties["user"] = user
		err = ApplyDynamic(m2, map[string]any{"user": map[string]any{"age": json.Number("1")}})
		apiErr = requireAPIError(t, err)
		assert.Contains(t, apiErr.Reason, "user.age")
	})

	t.Run("TypeCompatibilityRejects", func(t *testing.T) {
		cases := []struct {
			name  string
			fd    *FieldDef
			value any
		}{
			{"long rejects float", longDef("f"), json.Number("3.5")},
			{"long rejects bool", longDef("f"), true},
			{"double rejects alpha string", &FieldDef{Name: "f", Type: TypeDouble, Index: true, DocValues: true}, "abc"},
			{"boolean rejects arbitrary string", &FieldDef{Name: "f", Type: TypeBoolean, Index: true, DocValues: true}, "yes"},
			{"date rejects unparseable string", &FieldDef{Name: "f", Type: TypeDate, Index: true, DocValues: true}, "not-a-date"},
			{"date rejects bool", &FieldDef{Name: "f", Type: TypeDate, Index: true, DocValues: true}, true},
			{"ip rejects malformed address", &FieldDef{Name: "f", Type: TypeIP, Index: true, DocValues: true}, "300.1.2.3"},
			{"geo_point rejects garbage", &FieldDef{Name: "f", Type: TypeGeoPoint, Index: true, DocValues: true}, "abc"},
			{"geo_point rejects out-of-range lat", &FieldDef{Name: "f", Type: TypeGeoPoint, Index: true, DocValues: true}, "91.0,10.0"},
			{"dense_vector rejects wrong dims", &FieldDef{Name: "f", Type: TypeDenseVector, Dimensions: 4, Index: true, DocValues: true}, []any{json.Number("1"), json.Number("2")}},
			{"dense_vector rejects non-numeric", &FieldDef{Name: "f", Type: TypeDenseVector, Index: true, DocValues: true}, []any{"x"}},
			{"sparse_vector rejects bare array", &FieldDef{Name: "f", Type: TypeSparseVector, Index: true, DocValues: true}, []any{json.Number("1")}},
			{"sparse_vector rejects mismatched lengths", &FieldDef{Name: "f", Type: TypeSparseVector, Index: true, DocValues: true}, map[string]any{"indices": []any{json.Number("1")}, "values": []any{json.Number("1"), json.Number("2")}}},
			{"object rejects concrete value", newFieldDef("f", TypeObject), json.Number("42")},
			{"text rejects object value", newFieldDef("f", TypeText), map[string]any{"a": json.Number("1")}},
			{"array element must match too", longDef("f"), []any{json.Number("1"), "abc"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := ValidateFieldValue(tc.fd, tc.value)
				apiErr := requireAPIError(t, err)
				assert.Equal(t, 400, apiErr.Status)
				assert.Equal(t, errors.TypeDocumentParsing, apiErr.Type)
			})
		}
	})

	t.Run("TypeCompatibilityAccepts", func(t *testing.T) {
		base := func(t FieldType, extra func(*FieldDef)) *FieldDef {
			fd := newFieldDef("f", t)
			if extra != nil {
				extra(fd)
			}
			return fd
		}
		cases := []struct {
			name  string
			fd    *FieldDef
			value any
		}{
			{"long accepts integer", longDef("f"), json.Number("7")},
			{"long accepts numeric string (coerce)", longDef("f"), "7"},
			{"double accepts int", base(TypeDouble, nil), json.Number("7")},
			{"boolean accepts true string", base(TypeBoolean, nil), "true"},
			{"date accepts epoch millis", base(TypeDate, nil), json.Number("1798000000000")},
			{"date accepts ISO date", base(TypeDate, nil), "2026-10-17"},
			{"keyword accepts number (coerce)", base(TypeKeyword, nil), json.Number("123")},
			{"ip accepts IPv4", base(TypeIP, nil), "192.168.1.1"},
			{"ip accepts IPv6", base(TypeIP, nil), "2001:db8::1"},
			{"geo_point accepts lat,lon string", base(TypeGeoPoint, nil), "41.12,-71.34"},
			{"geo_point accepts lon/lat array", base(TypeGeoPoint, nil), []any{json.Number("-71.34"), json.Number("41.12")}},
			{"dense_vector accepts matching dims", base(TypeDenseVector, func(fd *FieldDef) { fd.Dimensions = 3 }), []any{json.Number("1"), json.Number("2"), json.Number("3")}},
			{"sparse_vector accepts values+indices", base(TypeSparseVector, nil), map[string]any{"indices": []any{json.Number("3"), json.Number("7")}, "values": []any{json.Number("0.5"), json.Number("1.5")}}},
			{"null is always acceptable", base(TypeLong, nil), nil},
			{"multi-valued array of longs", longDef("f"), []any{json.Number("1"), json.Number("2")}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.NoError(t, ValidateFieldValue(tc.fd, tc.value))
			})
		}
	})

	t.Run("KnownObjectTreeRejectsScalar", func(t *testing.T) {
		m := NewMapping()
		m.Properties["user"] = newFieldDef("user", TypeObject)
		err := ApplyDynamic(m, map[string]any{"user": json.Number("42")})
		apiErr := requireAPIError(t, err)
		assert.Equal(t, 400, apiErr.Status)
		assert.Equal(t, errors.TypeDocumentParsing, apiErr.Type)
	})
}

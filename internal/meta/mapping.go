// Package meta holds UniSearch's index metadata model (design
// 01-storage-engine §1): the field-type universe, per-field definitions,
// the mapping tree with its dynamic mode, the index metadata record and
// its single-node JSON persistence. In M0 metadata persists as one JSON
// file per index (atomic write, fsync); M2 swaps this for the Raft state
// machine (05-distributed.md §1) without changing these types.
package meta

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FieldType enumerates the full field-type universe of design
// 01-storage-engine §1.2. Values are ES-compatible wire strings; the two
// meta-fields `_id`/`_source` are implicit and never appear in mappings.
type FieldType string

const (
	TypeKeyword      FieldType = "keyword"
	TypeText         FieldType = "text"
	TypeLong         FieldType = "long"
	TypeDouble       FieldType = "double"
	TypeFloat        FieldType = "float"
	TypeDate         FieldType = "date"
	TypeBoolean      FieldType = "boolean"
	TypeIP           FieldType = "ip"
	TypeGeoPoint     FieldType = "geo_point"
	TypeDenseVector  FieldType = "dense_vector"
	TypeSparseVector FieldType = "sparse_vector"
	TypeObject       FieldType = "object"
	TypeNested       FieldType = "nested"
)

// VectorElement is the storage element type of dense_vector values
// (design 01 §1.3); v1 only exercises float32 but the wire values are
// fixed here so persistence stays forward-compatible.
type VectorElement string

const (
	ElementFloat32  VectorElement = "float32"
	ElementInt8     VectorElement = "int8"
	ElementBFloat16 VectorElement = "bfloat16"
	ElementFloat16  VectorElement = "float16" // @spec: qdrant-features-import F4
)

// DynamicMode controls how writes treat fields absent from the mapping
// (design 01 §1.3): true infers and adds them, false keeps them in
// _source without indexing, strict rejects the write with a 400.
type DynamicMode string

const (
	DynamicTrue   DynamicMode = "true"
	DynamicFalse  DynamicMode = "false"
	DynamicStrict DynamicMode = "strict"
)

// UnmarshalJSON accepts both ES spellings: JSON booleans (true/false) and
// the string form, e.g. {"dynamic": true} and {"dynamic": "strict"}.
func (d *DynamicMode) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		*d = DynamicTrue // explicit null keeps the ES default
	case bool:
		if v {
			*d = DynamicTrue
		} else {
			*d = DynamicFalse
		}
	case string:
		switch DynamicMode(v) {
		case DynamicTrue, DynamicFalse, DynamicStrict:
			*d = DynamicMode(v)
		default:
			return fmt.Errorf("meta: invalid dynamic mode %q", v)
		}
	default:
		return fmt.Errorf("meta: invalid dynamic mode %q", string(b))
	}
	return nil
}

// MarshalJSON emits ES wire form: booleans for true/false, the bare string
// for strict.
func (d DynamicMode) MarshalJSON() ([]byte, error) {
	switch d {
	case DynamicTrue:
		return []byte("true"), nil
	case DynamicFalse:
		return []byte("false"), nil
	case DynamicStrict:
		return json.Marshal(string(DynamicStrict))
	case "":
		return []byte("true"), nil // zero value defaults to dynamic true
	default:
		return nil, fmt.Errorf("meta: invalid dynamic mode %q", string(d))
	}
}

// FieldDef is a single field definition inside a mapping (design
// 01-storage-engine §1.3). Properties (for object/nested parents) and
// Fields (multi-fields, e.g. the automatic keyword sub-field of text) are
// carried on the same struct so the tree stays one recursive type.
type FieldDef struct {
	Name        string               `json:"name"`
	Type        FieldType            `json:"type"`
	Analyzer    string               `json:"analyzer,omitempty"`     // text only, default "standard"
	Dimensions  int                  `json:"dimensions,omitempty"`   // dense_vector only
	Element     VectorElement        `json:"element_type,omitempty"` // dense_vector only, v1: float32
	Similarity  string               `json:"similarity,omitempty"`   // cosine | dot_product | l2_norm | max_inner_product
	Index       bool                 `json:"index"`                  // default true; false = stored but not indexed
	DocValues   bool                 `json:"doc_values"`             // effectively always true (ES-compat accepts false with a warning)
	CopyTo      []string             `json:"copy_to,omitempty"`
	IgnoreAbove int                  `json:"ignore_above,omitempty"` // keyword: overlong values are dropped
	NullValue   any                  `json:"null_value,omitempty"`
	Format      string               `json:"format,omitempty"`     // date format, e.g. strict_date_optional_time||epoch_millis
	Properties  map[string]*FieldDef `json:"properties,omitempty"` // object/nested children
	Fields      map[string]*FieldDef `json:"fields,omitempty"`     // multi-fields, e.g. text.keyword
}

// UnmarshalJSON applies the mapping defaults from design 01 §1.3 before
// decoding, so a field body that omits index/doc_values gets true/true
// instead of Go zero values.
func (fd *FieldDef) UnmarshalJSON(b []byte) error {
	type raw FieldDef // shed methods to avoid recursion
	r := raw{Index: true, DocValues: true}
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*fd = FieldDef(r)
	return nil
}

// Mapping is the root of an index's field mapping tree (design
// 01-storage-engine §1.3). Dynamic governs every object level of the tree;
// Properties maps top-level field names to their definitions.
type Mapping struct {
	Dynamic    DynamicMode          `json:"dynamic"`
	Properties map[string]*FieldDef `json:"properties"`
}

// NewMapping returns an empty mapping with dynamic inference enabled.
func NewMapping() *Mapping {
	return &Mapping{Dynamic: DynamicTrue, Properties: map[string]*FieldDef{}}
}

// UnmarshalJSON defaults Dynamic to true when the body omits it, matching
// ES behavior.
func (m *Mapping) UnmarshalJSON(b []byte) error {
	type raw Mapping
	r := raw{Dynamic: DynamicTrue}
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*m = Mapping(r)
	if m.Properties == nil {
		m.Properties = map[string]*FieldDef{}
	}
	return nil
}

// Field returns the definition at the given dotted path (e.g. "user.name"),
// or nil when any segment is missing or traverses a non-object parent.
func (m *Mapping) Field(path string) *FieldDef {
	props := m.Properties
	var fd *FieldDef
	for _, key := range strings.Split(path, ".") {
		var ok bool
		if fd, ok = props[key]; !ok {
			return nil
		}
		props = fd.Properties
	}
	return fd
}

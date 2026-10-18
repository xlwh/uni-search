package meta

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// Mapping defaults that mirror ES dynamic mapping behavior (design 01
// §1.3 "对齐 ES 动态 mapping").
const (
	defaultAnalyzer            = "standard" // text analyzer default (design 01 §1.3)
	defaultKeywordIgnoreAbove  = 256        // ES default for the dynamic keyword multi-field
	dynamicMultiFieldKey       = "keyword"  // name of the automatic keyword sub-field
	dynamicDateEpochMillisName = "epoch_millis"
	dynamicDateEpochSecondName = "epoch_second"
)

// defaultDateLayouts are the ISO-8601 shapes accepted by dynamic date
// detection and by date fields without an explicit Format (design 01 §1.3
// "字符串（ISO date 可解析）"). They mirror ES strict_date_optional_time.
var defaultDateLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00", // RFC3339 with/without fractional seconds
	"2006-01-02T15:04:05.999999999",       // local time, no timezone
	"2006-01-02T15:04",
	"2006-01-02",
}

// ApplyDynamic walks one document on the write path and updates the mapping
// in place according to its dynamic setting (design 01 §1.3):
//
//   - dynamic true  : unknown fields are inferred and added to the mapping;
//   - dynamic false : unknown fields are skipped — they stay in _source and
//     are never indexed;
//   - dynamic strict: unknown fields fail the write with a 400
//     document_parsing_exception.
//
// Values of already-mapped fields are validated against their FieldDef via
// ValidateFieldValue; incompatible values are rejected with the same 400
// document_parsing_exception. Known object/nested subtrees are recursed
// with the same dynamic rule, so e.g. a new sub-field inside a known object
// honors the root dynamic mode.
func ApplyDynamic(m *Mapping, doc map[string]any) error {
	if m == nil {
		return errors.NewIllegalArgument("meta: mapping must not be nil")
	}
	if m.Properties == nil {
		m.Properties = map[string]*FieldDef{}
	}
	return applyObject(m.Properties, doc, m.Dynamic, "")
}

// InferType reports the field type dynamic mapping assigns to a single
// JSON value (design 01 §1.3 inference table): bool→boolean, integer→long,
// float→double, ISO date string→date, other string→text, object→object,
// array→type of its first element. It returns false when the value must not
// introduce a field at all (null, empty array, non-JSON Go type).
func InferType(v any) (FieldType, bool) {
	fd := inferFieldDef("", "probe", v)
	if fd == nil {
		return "", false
	}
	return fd.Type, true
}

// applyObject processes one object level: known fields are validated (and
// object parents recursed), unknown fields follow the dynamic rule.
func applyObject(props map[string]*FieldDef, obj map[string]any, dyn DynamicMode, path string) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic first-error on multi-conflict documents

	for _, key := range keys {
		v := obj[key]
		fieldPath := key
		if path != "" {
			fieldPath = path + "." + key
		}
		fd, known := props[key]
		if !known {
			switch dyn {
			case DynamicStrict:
				return errors.NewDocumentParsing(
					"mapping set to strict, dynamic introduction of [" + fieldPath + "] is not allowed")
			case DynamicFalse:
				continue // _source only: no field, no index
			default: // DynamicTrue, plus the zero value treated as true
				if nfd := inferFieldDef(path, key, v); nfd != nil {
					props[key] = nfd
				}
			}
			continue
		}
		if fd.Type == TypeObject || fd.Type == TypeNested {
			if err := applyObjectTree(fd, v, dyn, fieldPath); err != nil {
				return err
			}
			continue
		}
		if err := ValidateFieldValue(fd, v); err != nil {
			return err
		}
	}
	return nil
}

// applyObjectTree validates a known object/nested field and applies the
// dynamic rule inside it. A bare object or an array of objects is accepted
// (ES merges object arrays; nested keeps elements but shares one mapping).
func applyObjectTree(fd *FieldDef, v any, dyn DynamicMode, path string) error {
	objs, ok := objectValues(v)
	if !ok {
		return errors.NewDocumentParsing(
			"object mapping for [" + path + "] tried to parse field [" + path +
				"] as object, but found a concrete value")
	}
	if fd.Properties == nil {
		fd.Properties = map[string]*FieldDef{}
	}
	for _, o := range objs {
		if err := applyObject(fd.Properties, o, dyn, path); err != nil {
			return err
		}
	}
	return nil
}

// objectValues extracts the object(s) carried by an object-typed value:
// a single JSON object, or an array whose non-null elements are all JSON
// objects. Scalars inside the array make the whole value invalid.
func objectValues(v any) ([]map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		return []map[string]any{x}, true
	case []any:
		objs := make([]map[string]any, 0, len(x))
		for _, el := range x {
			if el == nil {
				continue
			}
			m, ok := el.(map[string]any)
			if !ok {
				return nil, false
			}
			objs = append(objs, m)
		}
		return objs, true
	default:
		return nil, false
	}
}

// inferFieldDef builds the FieldDef that dynamic mapping creates for an
// unseen value, or nil when the value must not introduce a field (null,
// empty array, non-JSON Go types). path is the dotted parent path (""
// at the tree root) and key the leaf field key.
func inferFieldDef(path, key string, v any) *FieldDef {
	name := key
	if path != "" {
		name = path + "." + key
	}
	arr, isArray := v.([]any)
	if !isArray {
		return inferScalar(name, v)
	}
	// Arrays are not a type (ES semantics): the first non-null element
	// decides, and nested arrays flatten through the recursion below.
	first, ok := firstNonNil(arr)
	if !ok {
		return nil
	}
	fd := inferFieldDef(path, key, first)
	if fd == nil {
		return nil
	}
	if fd.Type == TypeObject {
		// an array of objects merges every element's keys into one mapping
		for _, el := range arr {
			if m, isMap := el.(map[string]any); isMap {
				mergeProperties(fd, name, m)
			}
		}
	}
	return fd
}

// inferScalar builds the FieldDef for one non-array JSON value.
func inferScalar(name string, v any) *FieldDef {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		return newFieldDef(name, TypeBoolean)
	case string:
		if _, isDate := parseDate(x, ""); isDate {
			return newFieldDef(name, TypeDate)
		}
		fd := newFieldDef(name, TypeText)
		fd.Analyzer = defaultAnalyzer
		kw := newFieldDef(name+"."+dynamicMultiFieldKey, TypeKeyword)
		kw.IgnoreAbove = defaultKeywordIgnoreAbove
		fd.Fields = map[string]*FieldDef{dynamicMultiFieldKey: kw}
		return fd
	case map[string]any:
		fd := newFieldDef(name, TypeObject)
		mergeProperties(fd, name, x)
		return fd
	default:
		// json.Number and native Go numbers: integral → long, else double
		if _, isInt := asInt64(v); isInt {
			return newFieldDef(name, TypeLong)
		}
		if _, isFloat := asFloat64(v); isFloat {
			return newFieldDef(name, TypeDouble)
		}
		return nil // not representable in JSON: nothing to infer
	}
}

// mergeProperties infers the children of an object field from obj; fields
// already present keep their first-inferred definition.
func mergeProperties(parent *FieldDef, path string, obj map[string]any) {
	if parent.Properties == nil {
		parent.Properties = map[string]*FieldDef{}
	}
	for k, v := range obj {
		if _, exists := parent.Properties[k]; exists {
			continue
		}
		if sub := inferFieldDef(path, k, v); sub != nil {
			parent.Properties[k] = sub
		}
	}
}

// newFieldDef returns a FieldDef with the design 01 §1.3 defaults applied
// (Index=true, DocValues=true; object-like types get a property map).
func newFieldDef(name string, t FieldType) *FieldDef {
	fd := &FieldDef{Name: name, Type: t, Index: true, DocValues: true}
	if t == TypeObject || t == TypeNested {
		fd.Properties = map[string]*FieldDef{}
	}
	return fd
}

// firstNonNil returns the first non-null element of an array.
func firstNonNil(arr []any) (any, bool) {
	for _, el := range arr {
		if el != nil {
			return el, true
		}
	}
	return nil, false
}

// ValidateFieldValue checks that v is indexable under fd per design 01
// §1.3. Incompatible values return a 400 document_parsing_exception
// APIError from internal/errors. Nulls are always accepted; arrays are
// multi-valued and every element must be compatible; object/nested values
// are shape-checked here while their inner fields are walked by
// ApplyDynamic.
func ValidateFieldValue(fd *FieldDef, v any) error {
	if compatible(fd, v) {
		return nil
	}
	return errors.NewDocumentParsing(fmt.Sprintf(
		"failed to parse field [%s] of type [%s] in document. Preview of field's value: '%s'",
		fd.Name, fd.Type, previewValue(v)))
}

// compatible implements the value/type compatibility matrix. It follows ES
// write-time semantics: coercion of numeric strings (coerce=true default)
// and of scalars into text/keyword is allowed, floats into integer fields
// are rejected, booleans accept only true/false and the strings "true"
// and "false".
func compatible(fd *FieldDef, v any) bool {
	if v == nil {
		return true
	}
	// geo_point and vector types carry their own array/map encodings; the
	// generic multi-value and object branches below must not intercept them.
	switch fd.Type {
	case TypeGeoPoint:
		return validGeoPoint(v)
	case TypeDenseVector:
		return validDenseVector(fd, v)
	case TypeSparseVector:
		return validSparseVector(v)
	}
	if arr, ok := v.([]any); ok {
		for _, el := range arr {
			if !compatible(fd, el) {
				return false
			}
		}
		return true
	}
	if _, isMap := v.(map[string]any); isMap {
		return fd.Type == TypeObject || fd.Type == TypeNested
	}
	switch fd.Type {
	case TypeText, TypeKeyword:
		if _, isStr := v.(string); isStr {
			return true
		}
		if _, isBool := v.(bool); isBool {
			return true
		}
		_, isNum := asFloat64(v) // numbers stringify (ES coercion)
		return isNum
	case TypeLong:
		if _, isInt := asInt64(v); isInt {
			return true
		}
		if s, isStr := v.(string); isStr {
			_, err := strconv.ParseInt(s, 10, 64)
			return err == nil
		}
		return false
	case TypeDouble:
		if _, isFloat := asFloat64(v); isFloat {
			return true
		}
		if s, isStr := v.(string); isStr {
			_, err := strconv.ParseFloat(s, 64)
			return err == nil
		}
		return false
	case TypeBoolean:
		switch x := v.(type) {
		case bool:
			return true
		case string:
			return x == "true" || x == "false"
		}
		return false
	case TypeDate:
		if _, isMillis := asInt64(v); isMillis {
			return true // epoch_millis is part of the ES default date format
		}
		if s, isStr := v.(string); isStr {
			_, ok := parseDate(s, fd.Format)
			return ok
		}
		return false
	case TypeIP:
		s, isStr := v.(string)
		return isStr && net.ParseIP(s) != nil
	case TypeObject, TypeNested:
		return false // maps were handled above; other shapes are invalid
	}
	return false
}

// validGeoPoint accepts the ES geo_point encodings exercised in M0:
// "lat,lon" strings, {lat,lon} objects and GeoJSON [lon,lat] arrays, with
// range-checked coordinates.
func validGeoPoint(v any) bool {
	switch x := v.(type) {
	case string:
		parts := strings.Split(x, ",")
		if len(parts) != 2 {
			return false
		}
		lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lon, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		return err1 == nil && err2 == nil && validLatLon(lat, lon)
	case map[string]any:
		lat, ok1 := asFloat64(x["lat"])
		lon, ok2 := asFloat64(x["lon"])
		return ok1 && ok2 && validLatLon(lat, lon)
	case []any:
		if len(x) != 2 {
			return false
		}
		lon, ok1 := asFloat64(x[0]) // GeoJSON order: [lon, lat]
		lat, ok2 := asFloat64(x[1])
		return ok1 && ok2 && validLatLon(lat, lon)
	}
	return false
}

func validLatLon(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// validDenseVector accepts a dense array of numbers (optionally in the
// sparse {"values": [...], "indices": [...]} encoding) whose length must
// equal fd.Dimensions when it is declared.
func validDenseVector(fd *FieldDef, v any) bool {
	switch x := v.(type) {
	case []any:
		if fd.Dimensions > 0 && len(x) != fd.Dimensions {
			return false
		}
		return allNumeric(x)
	case map[string]any:
		vals, ok1 := x["values"].([]any)
		idxs, ok2 := x["indices"].([]any)
		if !ok1 || !ok2 || len(vals) != len(idxs) {
			return false
		}
		if fd.Dimensions > 0 && len(vals) != fd.Dimensions {
			return false
		}
		return allNumeric(vals) && allNonNegativeInts(idxs)
	}
	return false
}

// validSparseVector accepts {"indices": [...], "values": [...]} with equal
// lengths, uint32-representable indices and numeric values. sparse_vector
// is a first-class citizen in UniSearch (design 01 §1.2).
func validSparseVector(v any) bool {
	m, isMap := v.(map[string]any)
	if !isMap {
		return false
	}
	vals, ok1 := m["values"].([]any)
	idxs, ok2 := m["indices"].([]any)
	if !ok1 || !ok2 || len(vals) != len(idxs) {
		return false
	}
	for _, el := range idxs {
		n, isInt := asInt64(el)
		if !isInt || n < 0 || n > math.MaxUint32 {
			return false
		}
	}
	return allNumeric(vals)
}

func allNumeric(vals []any) bool {
	for _, el := range vals {
		if _, ok := asFloat64(el); !ok {
			return false
		}
	}
	return true
}

func allNonNegativeInts(vals []any) bool {
	for _, el := range vals {
		n, ok := asInt64(el)
		if !ok || n < 0 {
			return false
		}
	}
	return true
}

// parseDate parses s against format (ES "||"-separated alternatives) or,
// when format is empty, against the default ISO layouts. The epoch_millis
// and epoch_second ES format aliases are recognized as well.
func parseDate(s, format string) (time.Time, bool) {
	layouts := defaultDateLayouts
	if format != "" {
		layouts = strings.Split(format, "||")
	}
	for _, f := range layouts {
		switch f {
		case dynamicDateEpochMillisName:
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.UnixMilli(n), true
			}
		case dynamicDateEpochSecondName:
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.Unix(n, 0), true
			}
		default:
			if t, err := time.Parse(f, s); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// asInt64 extracts an int64 from JSON numbers (json.Number) and native Go
// integer types. Floats are never coerced down (ES rejects them for
// integer fields).
func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	}
	return 0, false
}

// asFloat64 extracts a float64 from JSON numbers (json.Number) and all
// native Go numeric types.
func asFloat64(v any) (float64, bool) {
	if n, ok := asInt64(v); ok {
		return float64(n), true
	}
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// previewValue renders a short preview of a rejected value for the error
// reason, matching the ES "Preview of field's value" convention.
func previewValue(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 64 {
		s = s[:64] + "..."
	}
	return s
}

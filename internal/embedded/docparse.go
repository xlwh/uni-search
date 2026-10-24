package embedded

import (
	"fmt"
	"github.com/unisearch-db/unisearch/internal/jsonx"

	"github.com/unisearch-db/unisearch/internal/meta"
	"github.com/unisearch-db/unisearch/internal/storage/memtable"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// applyInsertFn returns a function matching storage.ShardConfig.ApplyInsert
// that parses a JSON document payload into a memtable.Row using the
// indexState's field mapping. Unknown fields are dynamically inferred
// (when mapping.Dynamic is true) and assigned a new FieldID.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.2
func applyInsertFn(idx *indexState) func(payload []byte, seq types.SeqNo) (memtable.Row, error) {
	return func(payload []byte, seq types.SeqNo) (memtable.Row, error) {
		var doc map[string]any
		if err := jsonx.Unmarshal(payload, &doc); err != nil {
			return memtable.Row{}, fmt.Errorf("embedded: parse document: %w", err)
		}

		// Dynamic mapping inference for unknown fields.
		idx.mu.Lock()
		if idx.fieldIDs["_id"] == 0 {
			idx.fieldIDs["_id"] = types.FieldID(1)
			idx.fieldDefs[types.FieldID(1)] = &meta.FieldDef{Name: "_id", Type: meta.TypeKeyword, Index: true}
		}
		// Check for unknown fields and infer types.
		for field := range doc {
			if field == "_id" {
				continue
			}
			if _, known := idx.fieldIDs[field]; known {
				continue
			}
			// Dynamic inference: determine field type from value.
			ft, ok := meta.InferType(doc[field])
			if !ok {
				idx.mu.Unlock()
				return memtable.Row{}, fmt.Errorf("embedded: cannot infer type for field %q", field)
			}
			fd := &meta.FieldDef{Name: field, Type: ft, Index: true}
			// Allocate next FieldID.
			var maxFid types.FieldID
			for fid := range idx.fieldDefs {
				if fid > maxFid {
					maxFid = fid
				}
			}
			nextFid := maxFid + 1
			idx.fieldIDs[field] = nextFid
			idx.fieldDefs[nextFid] = fd
		}
		idx.mu.Unlock()

		// Build the memtable.Row values.
		values := make(map[types.FieldID]any)
		for field, val := range doc {
			fid, fd, ok := idx.resolveField(field)
			if !ok {
				continue // shouldn't happen after inference above
			}
			converted, err := convertValue(val, fd.Type)
			if err != nil {
				return memtable.Row{}, fmt.Errorf("embedded: field %q: %w", field, err)
			}
			if converted != nil {
				values[fid] = converted
			}
		}

		// Extract _id.
		id := ""
		if idVal, ok := doc["_id"]; ok {
			switch v := idVal.(type) {
			case string:
				id = v
			case float64:
				id = fmt.Sprintf("%v", v)
			default:
				id = fmt.Sprintf("%v", v)
			}
		}
		if id == "" {
			id = fmt.Sprintf("auto-%d", seq)
		}

		return memtable.Row{
			ID:     id,
			SeqNo:  seq,
			Source: payload,
			Values: values,
		}, nil
	}
}

// convertValue converts a JSON-decoded Go value to the storage-native
// type for the given field type.
func convertValue(val any, ft meta.FieldType) (any, error) {
	if val == nil {
		return nil, nil
	}
	// Handle nested objects: skip indexing (the _source retains the
	// original object for retrieval).
	if _, ok := val.(map[string]any); ok {
		return nil, nil
	}
	// Defensive: if any scalar field type receives an array value
	// (e.g., dense_vector field whose mapping wasn't properly recovered
	// or a reindex into an unmapped index), convert it to []float32
	// instead of crashing. This handles the case where dynamic inference
	// misidentifies a numeric array as a scalar type.
	if arr, ok := val.([]any); ok {
		switch ft {
		case meta.TypeDenseVector:
			return convertDenseVector(arr)
		case meta.TypeKeyword, meta.TypeText:
			// Array of strings for a keyword/text field: skip indexing
			// (the _source retains the original array for retrieval).
			return nil, nil
		case meta.TypeDouble, meta.TypeFloat, meta.TypeLong,
			meta.TypeBoolean, meta.TypeDate:
			// Array received for a scalar numeric field — treat as dense_vector.
			return convertDenseVector(arr)
		}
	}
	switch ft {
	case meta.TypeKeyword, meta.TypeText:
		switch v := val.(type) {
		case string:
			return v, nil
		case float64:
			return fmt.Sprintf("%v", v), nil
		case int64:
			return fmt.Sprintf("%d", v), nil
		case bool:
			return fmt.Sprintf("%v", v), nil
		case jsonx.Number:
			return v.String(), nil
		default:
			return nil, nil // skip unknown types instead of crashing
		}
	case meta.TypeLong:
		switch v := val.(type) {
		case float64:
			return int64(v), nil
		case int64:
			return v, nil
		case int:
			return int64(v), nil
		case string:
			// Coerce: try parsing string as number.
			var i int64
			if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
				return i, nil
			}
			return nil, nil // skip unparseable
		case jsonx.Number:
			i, err := v.Int64()
			if err != nil {
				return nil, err
			}
			return i, nil
		default:
			return nil, nil // skip instead of crashing
		}
	case meta.TypeDouble, meta.TypeFloat:
		if arr, ok := val.([]any); ok {
			return convertDenseVector(arr)
		}
		switch v := val.(type) {
		case float64:
			return v, nil
		case int64:
			return float64(v), nil
		case int:
			return float64(v), nil
		case string:
			var f float64
			if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
				return f, nil
			}
			return nil, nil
		case jsonx.Number:
			f, err := v.Float64()
			if err != nil {
				return nil, err
			}
			return f, nil
		default:
			return nil, nil // skip instead of crashing
		}
	case meta.TypeBoolean:
		switch v := val.(type) {
		case bool:
			return v, nil
		default:
			return nil, nil // skip instead of crashing
		}
	case meta.TypeDenseVector:
		// JSON arrays decode as []any; convert to []float32.
		arr, ok := val.([]any)
		if !ok {
			return nil, fmt.Errorf("expected array for dense_vector, got %T", val)
		}
		return convertDenseVector(arr)
	case meta.TypeDate:
		// Dates are stored as epoch millis (int64).
		switch v := val.(type) {
		case float64:
			return int64(v), nil
		case string:
			// Let the mapping layer parse date strings; for now store as-is
			// and let the analyzer/index layer handle it.
			return v, nil
		case jsonx.Number:
			i, err := v.Int64()
			if err != nil {
				return nil, err
			}
			return i, nil
		default:
			return nil, fmt.Errorf("expected date value, got %T", val)
		}
	default:
		// For unsupported types (ip, geo_point, etc.), pass through as-is.
		return val, nil
	}
}

// convertDenseVector converts a JSON-decoded []any array to []float32.
func convertDenseVector(arr []any) (any, error) {
	vec := make([]float32, len(arr))
	for i, elem := range arr {
		switch e := elem.(type) {
		case float64:
			vec[i] = float32(e)
		case jsonx.Number:
			f, err := e.Float64()
			if err != nil {
				return nil, err
			}
			vec[i] = float32(f)
		default:
			return nil, fmt.Errorf("dense_vector element %d: expected number, got %T", i, elem)
		}
	}
	return vec, nil
}

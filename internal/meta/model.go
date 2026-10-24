package meta

import (
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// IndexSettings carries the ES-style index-level settings (design
// 01-storage-engine §1.1). JSON keys follow the ES `_settings` wire format
// so the compat layer (04 §2) can serve them verbatim.
type IndexSettings struct {
	NumberOfShards   int       `json:"number_of_shards"`   // logical shards (05 §3, online splittable)
	NumberOfReplicas int       `json:"number_of_replicas"` // v1 informational in single-node mode
	RefreshInterval  string    `json:"refresh_interval"`   // e.g. "1s"; "-1" disables (maps to MemTable flush cadence, 01 §2.2)
	TTL              *IndexTTL `json:"ttl,omitempty"`      // F4: TTL lifecycle config. @spec: 02_spec.md#5.3
	// F6: Projections defined via _settings. Each projection materializes
	// data in a different sort order or pre-computed aggregate state.
	// @spec: 02_spec.md#7.3
	Projections []ProjectionDef `json:"projections,omitempty"`
}

// ProjectionDef defines one projection for an index. @spec: 02_spec.md#7.3
type ProjectionDef struct {
	Name       string    `json:"name"`       // projection name
	Type       string    `json:"type"`       // "normal" | "aggregate"
	SortKey    []string  `json:"sort_key"`   // normal: sort columns
	GroupBy    []string  `json:"group_by"`   // aggregate: grouping columns
	Aggregates []AggSpec `json:"aggregates"` // aggregate: sub-aggregation defs
}

// AggSpec is one sub-aggregation in an aggregate projection.
type AggSpec struct {
	Name  string `json:"name"`
	Type  string `json:"type"`  // "sum", "count", "min", "max", "avg"
	Field string `json:"field"` // source field name
}

// IndexTTL holds one or more TTL rules for an index. @spec: 02_spec.md#5.3
type IndexTTL struct {
	Exprs []TTLExpr `json:"exprs"`
}

// TTLExpr defines one TTL rule: when rows whose timestamp column is older
// than Interval from now are merged, the specified Mode action is applied.
// @spec: 02_spec.md#5.3
type TTLExpr struct {
	Column      string   `json:"column"`       // timestamp column name
	Mode        string   `json:"mode"`         // "delete" | "move" | "group_by" | "recompress"
	Interval    string   `json:"interval"`     // e.g. "30d", "1h"
	GroupBy     []string `json:"group_by"`     // group_by mode: grouping columns
	TargetCodec string   `json:"target_codec"` // recompress mode: target codec
	Destination string   `json:"destination"`  // move mode: target path
}

// IndexMeta is the metadata state-machine record for one index (design
// 01-storage-engine §1.1). Storage layers consume it but never own it;
// metadata persists only down to shard routing — segment inventories live
// on shard nodes (05-distributed.md §1).
type IndexMeta struct {
	ID        types.TenantID  `json:"id"`      // owning tenant
	Name      string          `json:"name"`    // ES index name
	Mapping   *Mapping        `json:"mapping"` // see design 01 §1.3
	Settings  IndexSettings   `json:"settings"`
	Shards    []types.ShardID `json:"shards"` // logical shard list (routing)
	Aliases   []string        `json:"aliases"`
	CreatedAt time.Time       `json:"created_at"`
	// FieldIDs persists the field-name→FieldID mapping so FieldIDs stay
	// stable across restarts. _id is always FieldID(1); other fields are
	// assigned in mapping-property key order, then appended for dynamic
	// fields. @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.1
	FieldIDs map[string]types.FieldID `json:"field_ids,omitempty"`
}

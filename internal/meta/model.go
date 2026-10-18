package meta

import (
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// IndexSettings carries the ES-style index-level settings (design
// 01-storage-engine §1.1). JSON keys follow the ES `_settings` wire format
// so the compat layer (04 §2) can serve them verbatim.
type IndexSettings struct {
	NumberOfShards   int    `json:"number_of_shards"`   // logical shards (05 §3, online splittable)
	NumberOfReplicas int    `json:"number_of_replicas"` // v1 informational in single-node mode
	RefreshInterval  string `json:"refresh_interval"`   // e.g. "1s"; "-1" disables (maps to MemTable flush cadence, 01 §2.2)
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
}

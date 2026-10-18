// Package types holds the identifier and versioning primitives shared by
// every layer of UniSearch (design 01-storage-engine §1.1/§8). Keeping them
// in one dependency-free package lets storage, index, query and the REST
// layer reference the same concrete types without import cycles.
package types

// TenantID identifies a tenant; tenants are the physical isolation unit.
type TenantID uint32

// IndexID identifies an index (ES index equivalent) within a tenant.
type IndexID uint32

// ShardID identifies a logical shard (routing unit, splittable online).
type ShardID uint64

// SegmentID identifies an immutable segment; globally unique across the
// cluster (snowflake: timestamp + shard + sequence).
type SegmentID uint64

// RowID is the 0-based row number inside a segment (max 2^32 rows).
type RowID uint32

// FieldID is the field number inside an index, assigned in mapping order.
type FieldID uint16

// SeqNo is the MVCC version number = Raft commit index (local WAL sequence
// in the M0 single-node mode).
type SeqNo uint64

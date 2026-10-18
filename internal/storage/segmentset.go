package storage

import (
	"sync"
	"sync/atomic"

	"github.com/unisearch-db/unisearch/internal/index"
	"github.com/unisearch-db/unisearch/internal/storage/segment"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// SegmentEntry pairs a segment reader with its loaded per-field indexes
// (currently HNSW for dense_vector columns). Both fields stay immutable
// after publication so queries can read them lock-free.
//
// The Reader is the concrete *segment.Segment type (not the
// SegmentReader interface) so the storage package can call DocID and
// other non-interface methods; the query engine (T15) treats this as
// a SegmentReader through the concrete type's method set.
type SegmentEntry struct {
	Reader  *segment.Segment
	Indexes map[types.FieldID]index.LoadedIndex
}

// SegmentSet is an immutable published view of all live segments for
// one shard (design 01 §5: "atomic.Pointer 无锁切换").
type SegmentSet struct {
	items []*SegmentEntry
}

func newSegmentSet(items []*SegmentEntry) *SegmentSet { return &SegmentSet{items: items} }

// Items returns the live segment entries.
func (s *SegmentSet) Items() []*SegmentEntry {
	if s == nil {
		return nil
	}
	return s.items
}

// Len reports the live segment count.
func (s *SegmentSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.items)
}

// segmentSetHolder is the atomic publisher of SegmentSet for queries.
// Design 01 §5: a single atomic.Pointer swap hands readers the new set.
//
// The embedded RWMutex closes the publish-vs-remove race: a flush worker
// publishes a new segment and removes the now-redundant frozen memtable
// under the W lock; readers Iterate under the R lock so they observe
// either (segment + frozen memtable) together or (segment published +
// frozen memtable gone) together, never (segment published + frozen
// memtable still iterable) which would yield duplicate rows.
type segmentSetHolder struct {
	p  atomic.Pointer[SegmentSet]
	mu sync.RWMutex
}

func (h *segmentSetHolder) load() *SegmentSet {
	if s := h.p.Load(); s != nil {
		return s
	}
	return emptySet
}

func (h *segmentSetHolder) store(s *SegmentSet) { h.p.Store(s) }

var emptySet = newSegmentSet(nil)

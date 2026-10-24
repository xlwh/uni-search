// Package index defines the unified index plugin interface (design
// 02-index-engine.md §1). Every index kind — vector (HNSW), inverted,
// and scalar (BKD/Bitmap/Bloom/FST) — implements Index and registers a
// factory under its IndexKind. Lifecycle is driven by the storage engine
// flush/merge; the index engine and storage engine couple only through
// segment files and the narrow ColumnProvider/IndexInput/IndexOutput
// interfaces (00-overview.md §4).
package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/unisearch-db/unisearch/internal/meta"
)

// ErrUnsupported is returned by Index methods that do not apply to the
// concrete index kind (e.g. Quantize on a non-vector index).
var ErrUnsupported = errors.New("index: operation unsupported by this index kind")

// ErrUnknownKind is returned when looking up an IndexKind that has no
// registered factory.
var ErrUnknownKind = errors.New("index: unknown index kind")

// IndexKind identifies an index implementation inside one process.
type IndexKind string

// Registered index kinds (02-index-engine.md §1.1).
const (
	KindHNSW     IndexKind = "hnsw"     // vector graph index (§2)
	KindInverted IndexKind = "inverted" // term posting lists (§3)
	KindBKD      IndexKind = "bkd"      // numeric/multi-dim range (§5)
	KindBitmap   IndexKind = "bitmap"   // exact set membership
	KindBloom    IndexKind = "bloom"    // probabilistic membership
	KindFST      IndexKind = "fst"      // finite-state term dictionary
)

// QuantLevel is the quantization tier of a vector index (§2.5).
type QuantLevel int8

const (
	// QuantNone keeps raw float32 vectors (hot tier, 4B/dim, 100% recall).
	QuantNone QuantLevel = iota
	// QuantSQ8 is per-dimension int8 scalar quantization (warm tier,
	// 1B/dim, ~98% recall @ ef64 with rerank).
	QuantSQ8
	// QuantPQ is Product Quantization with K-means++ training
	// (warm tier, ~M bytes/vector, ~95% recall with rerank).
	// @spec: ./active_specs/2026-10-22-vsag-algo-integration/02_spec.md#2.3
	QuantPQ
)

// ColumnProvider streams one column of a segment in row order. It is
// implemented by the storage engine on top of the col_*.bin block
// format (01 §4.3); index building consumes it without touching storage
// internals. M0 minimal form: vector columns only.
type ColumnProvider interface {
	// Field returns the field name of the column.
	Field() string
	// Rows returns the total row count of the segment.
	Rows() int64
	// Dim returns the vector dimension; 0 for non-vector columns.
	Dim() int
	// ForEachVector invokes fn once per row in row order with the row's
	// float32 vector. vec is only valid for the duration of the call.
	// Iteration stops on the first error, which is returned verbatim.
	ForEachVector(fn func(rowID int32, vec []float32) error) error
}

// IndexOutput creates writable files inside a segment's index directory
// (e.g. seg_xxx/vec.idx/). Implementations seal the directory on close.
type IndexOutput interface {
	// Create opens the named file (relative, e.g. "hnsw.graph") for
	// writing; existing content is truncated.
	Create(name string) (io.WriteCloser, error)
}

// IndexInput opens read-only files from a sealed segment's index
// directory. Implementations are expected to be mmap-backed and
// immutable, hence safe for concurrent use.
type IndexInput interface {
	// Open opens the named file (relative, e.g. "hnsw.graph") for
	// reading with seeking support.
	Open(name string) (io.ReadSeekCloser, error)
}

// Query describes one search over a loaded index. Which fields are
// honored depends on the index kind; vector indexes consume Vector/K/
// EFSearch/Filter, term indexes consume Term (M1, to be extended).
type Query struct {
	// Vector is the query vector for vector indexes (nil otherwise).
	Vector []float32
	// Term is the query term for term-scanning indexes.
	Term []byte
	// K is the number of results requested; 0 means index default.
	K int
	// EFSearch overrides the search width for vector indexes; 0 means
	// index default (§2.2: ef_search = 64).
	EFSearch int
	// Filter is an optional row-level predicate applied during graph
	// traversal: rows for which it returns false are skipped before
	// entering the candidate heap (§2.4.4 delete handling, §2.6
	// pre-filter). nil means no filtering.
	Filter func(rowID int32) bool
}

// SearchBudget caps the work a single Search call may perform. Zero
// budget fields mean "no cap beyond ef_search"; Search must also honor
// ctx cancellation and deadlines at all times.
type SearchBudget struct {
	// MaxVisited caps the number of graph nodes expanded (popped from
	// the candidate frontier). <=0 means unlimited.
	MaxVisited int64
	// MaxDistComps caps the number of distance computations. <=0 means
	// unlimited.
	MaxDistComps int64
}

// SearchHit is one ranked result row.
type SearchHit struct {
	// RowID is the segment-local row id (== HNSW node id, zero
	// conversion, §2.1).
	RowID int32
	// Score ranks hits: higher is better. For dot/cosine it is the
	// (normalized) dot product; for L2 it is the negated squared
	// distance, so TopK semantics stay uniform.
	Score float32
}

// SearchResult is the outcome of one Search call, ordered by descending
// Score.
type SearchResult struct {
	Hits []SearchHit
}

// LoadedIndex is a searchable, immutable, concurrency-safe handle to a
// built index (typically mmap-backed).
type LoadedIndex interface {
	Search(ctx context.Context, q Query, budget SearchBudget) (*SearchResult, error)
	Close() error
}

// Index is the build/load/merge/quantize trait every index kind
// implements (02-index-engine.md §1.1). Lifecycle is driven by the
// storage engine flush/merge.
type Index interface {
	Kind() IndexKind

	// Build constructs the index files from a segment column and writes
	// them through w (into seg_xxx/<kind>.idx/, 01 §4.1).
	Build(ctx context.Context, col ColumnProvider, w IndexOutput) error
	// Load mmap-opens a built index directory; the returned handle is
	// immutable and safe for concurrent use.
	Load(r IndexInput) (LoadedIndex, error)

	// Merge combines this index with others of the same field/kind into
	// w. Semantics per kind: HNSW = Cross-Link (M0: full rebuild with
	// incremental-shaped format), inverted = posting merge, BKD =
	// rebuild.
	Merge(ctx context.Context, others []LoadedIndex, w IndexOutput) error

	// Quantize moves a vector index to a lower-memory tier. Non-vector
	// indexes return ErrUnsupported.
	Quantize(ctx context.Context, to QuantLevel) error

	// MemEstimate reports current memory usage for TierManager and
	// merge scheduling (00 §3.1).
	MemEstimate() int64
}

// IndexFactory instantiates a fresh, empty Index of its kind. Factories
// must be cheap and side-effect free; all state comes later via
// Build/Load.
type IndexFactory func() Index

var (
	registryMu sync.RWMutex
	registry   = make(map[IndexKind]IndexFactory)
)

// RegisterIndex installs an in-process factory under kind (§1.2).
// Registering the same kind twice panics: it indicates a wiring bug at
// init time, not a runtime condition to handle gracefully.
func RegisterIndex(kind IndexKind, factory IndexFactory) {
	if factory == nil {
		panic(fmt.Sprintf("index: RegisterIndex(%q) with nil factory", kind))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("index: RegisterIndex called twice for kind %q", kind))
	}
	registry[kind] = factory
}

// NewIndex looks up the factory registered for kind and instantiates an
// empty Index.
func NewIndex(kind IndexKind) (Index, error) {
	registryMu.RLock()
	f, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	return f(), nil
}

// FieldTypeRoute declares which ES field types an index Kind handles.
// The translator (embedded/translator.go) consults this table to decide
// which index path to take for each field (D3: Index Registry declarative
// routing).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.4
type FieldTypeRoute struct {
	Kind         IndexKind
	ESFieldTypes []meta.FieldType
}

// fieldRoutes is the declarative routing table. Order matters: the first
// matching Kind wins when multiple kinds could apply (none currently do,
// but the rule guards future additions).
var fieldRoutes = []FieldTypeRoute{
	{Kind: KindHNSW, ESFieldTypes: []meta.FieldType{meta.TypeDenseVector}},
	{Kind: KindInverted, ESFieldTypes: []meta.FieldType{meta.TypeText}},
	{Kind: KindBKD, ESFieldTypes: []meta.FieldType{meta.TypeLong, meta.TypeDate, meta.TypeDouble, meta.TypeIP, meta.TypeGeoPoint}},
	{Kind: KindBitmap, ESFieldTypes: []meta.FieldType{meta.TypeKeyword, meta.TypeBoolean}},
}

// RouteForField returns the index Kinds applicable to the given ES field
// type. Returns an empty slice for unsupported/unknown types (the caller
// falls back to a full scan).
func RouteForField(esType meta.FieldType) []IndexKind {
	var kinds []IndexKind
	for _, r := range fieldRoutes {
		for _, t := range r.ESFieldTypes {
			if t == esType {
				kinds = append(kinds, r.Kind)
				break
			}
		}
	}
	return kinds
}

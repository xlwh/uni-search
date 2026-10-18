package hnsw

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/unisearch-db/unisearch/internal/index"
)

func init() {
	index.RegisterIndex(index.KindHNSW, func() index.Index { return &Index{} })
}

// graphMagic tags hnsw.graph ("HGR1", little-endian).
const graphMagic uint32 = 0x48475231

// Segment index file names (§2.1 layout under seg_xxx/vec.idx/).
// hnsw.vecs is an M0 addition: the (normalized for cosine) vector copy
// the graph searches against. In the full storage layout the vectors
// live in vec_{field_id}.bin (01 §4.3) and the graph looks them up by
// row id; until the storage layer exposes that column through
// IndexInput, the graph keeps its own copy so a loaded index stays
// self-contained.
const (
	fileGraph  = "hnsw.graph"  // CSR neighbors + offsets + header
	fileLevels = "hnsw.levels" // []uint8 per-node level
	fileEntry  = "entry.bin"   // entry node id + global max level
	fileQuant  = "quant.state" // quantization tier (§2.5)
	fileVecs   = "hnsw.vecs"   // M0: normalized vector copy (§2.1 deviation)
)

// Index is the index.Index implementation registered under
// index.KindHNSW (02-index-engine.md §1.1). It carries the build
// configuration and, after Build/Merge, the built graph; the loaded
// artifact handed to callers is loadedIndex.
type Index struct {
	cfg Config
	g   *Graph // built graph; nil until Build or Merge
}

// New returns an HNSW Index with the given build configuration.
func New(cfg Config) *Index { return &Index{cfg: cfg} }

// Kind implements index.Index.
func (ix *Index) Kind() index.IndexKind { return index.KindHNSW }

// Build implements index.Index: it streams the vector column through
// ColumnProvider, builds the graph in memory, and writes the §2.1
// file set through w.
func (ix *Index) Build(ctx context.Context, col index.ColumnProvider, w index.IndexOutput) error {
	dims := col.Dim()
	if dims <= 0 {
		return fmt.Errorf("hnsw: column %q is not a vector column (dim=%d)", col.Field(), dims)
	}
	n := int(col.Rows())
	vecs := make([]float32, n*dims)
	i := 0
	err := col.ForEachVector(func(rowID int32, v []float32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int(rowID) != i {
			return fmt.Errorf("hnsw: column yielded row %d at position %d", rowID, i)
		}
		if len(v) != dims {
			return fmt.Errorf("hnsw: column yielded %d dims at row %d, want %d", len(v), rowID, dims)
		}
		copy(vecs[i*dims:(i+1)*dims], v)
		i++
		return nil
	})
	if err != nil {
		return err
	}
	if i != n {
		return fmt.Errorf("hnsw: column yielded %d vectors, header says %d", i, n)
	}
	g, err := BuildGraph(ix.cfg, n, dims, vecs)
	if err != nil {
		return err
	}
	if err := writeGraphFiles(w, g); err != nil {
		return err
	}
	ix.g = g
	return nil
}

// Load implements index.Index: it reads the §2.1 file set from r and
// returns an immutable, concurrency-safe loadedIndex. When quant.state
// records SQ8, the quantization tier is restored without retraining.
func (ix *Index) Load(r index.IndexInput) (index.LoadedIndex, error) {
	g, err := readGraphFiles(r)
	if err != nil {
		return nil, err
	}
	return &loadedIndex{g: g}, nil
}

// Merge implements index.Index. M0 semantics (§2.4.3): full rebuild —
// the vectors of this index (when built) plus every other graph are
// concatenated and rebuilt into w, with new row ids assigned by
// position (the storage layer owns the old-to-new row mapping). The
// file format stays the incremental-shaped one, so the M2 Cross-Link
// merge swaps in without a format migration.
func (ix *Index) Merge(ctx context.Context, others []index.LoadedIndex, w index.IndexOutput) error {
	var src []*Graph
	if ix.g != nil {
		src = append(src, ix.g)
	}
	for _, o := range others {
		li, ok := o.(*loadedIndex)
		if !ok {
			return fmt.Errorf("hnsw: cannot merge foreign loaded index %T", o)
		}
		src = append(src, li.g)
	}
	if len(src) == 0 {
		return fmt.Errorf("hnsw: merge requires at least one source index")
	}
	dims := src[0].dims
	metric := src[0].cfg.Metric
	total := 0
	for _, g := range src {
		if g.dims != dims {
			return fmt.Errorf("hnsw: dim mismatch in merge: %d vs %d", g.dims, dims)
		}
		if g.cfg.Metric != metric {
			return fmt.Errorf("hnsw: metric mismatch in merge")
		}
		total += g.N()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vecs := make([]float32, total*dims)
	pos := 0
	for _, g := range src {
		copy(vecs[pos:pos+g.N()*dims], g.vecs)
		pos += g.N() * dims
	}
	g, err := BuildGraph(Config{Metric: metric, Seed: ix.cfg.Seed}, total, dims, vecs)
	if err != nil {
		return err
	}
	if err := writeGraphFiles(w, g); err != nil {
		return err
	}
	ix.g = g
	return nil
}

// Quantize implements index.Index: SQ8 installs the warm tier on the
// built graph (§2.5), QuantNone drops back to the float32 hot tier.
// Unbuilt indexes and future tiers return errors.
func (ix *Index) Quantize(ctx context.Context, to index.QuantLevel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ix.g == nil {
		return fmt.Errorf("hnsw: quantize before build")
	}
	switch to {
	case index.QuantSQ8:
		return ix.g.EnableSQ8()
	case index.QuantNone:
		ix.g.DisableSQ8()
		return nil
	default:
		return fmt.Errorf("%w: quant level %d", index.ErrUnsupported, to)
	}
}

// MemEstimate implements index.Index.
func (ix *Index) MemEstimate() int64 {
	if ix.g == nil {
		return 0
	}
	return ix.g.MemBytes()
}

// loadedIndex is the searchable handle over one built graph. It is
// immutable and safe for concurrent use.
type loadedIndex struct {
	g *Graph
}

// Search implements index.LoadedIndex: the query vector is transformed
// per the build metric (NormalizeQuery), then routed to the float32 or
// SQ8 path depending on the active tier.
func (l *loadedIndex) Search(ctx context.Context, q index.Query, budget index.SearchBudget) (*index.SearchResult, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("hnsw: empty query vector")
	}
	qv := NormalizeQuery(l.g.cfg, q.Vector)
	k := q.K
	if k <= 0 {
		k = 10
	}
	ef := q.EFSearch
	if ef <= 0 {
		ef = l.g.cfg.EFSearch
	}
	hits, err := l.g.Search(ctx, qv, k, ef, q.Filter, budget)
	if err != nil {
		return nil, err
	}
	out := &index.SearchResult{Hits: make([]index.SearchHit, len(hits))}
	for i, h := range hits {
		out.Hits[i] = index.SearchHit{RowID: h.RowID, Score: h.Score}
	}
	return out, nil
}

// Close implements index.LoadedIndex. M0 graphs are in-memory, so
// there is nothing to unmap.
func (l *loadedIndex) Close() error { return nil }

// writeGraphFiles serializes a graph into the §2.1 file set (all
// little-endian): hnsw.graph holds a header {magic, n, dims, metric,
// neighborsLen, offsetsLen} followed by the CSR arrays; hnsw.levels
// holds one byte per node; entry.bin holds the entry id and max level;
// quant.state holds the tier byte and, for SQ8, the quantization state.
func writeGraphFiles(w index.IndexOutput, g *Graph) error {
	f, err := w.Create(fileGraph)
	if err != nil {
		return fmt.Errorf("hnsw: create %s: %w", fileGraph, err)
	}
	hdr := []any{
		graphMagic, int32(g.n), int32(g.dims), int8(g.cfg.Metric),
		int32(len(g.neighbors)), int32(len(g.offsets)),
	}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			f.Close()
			return fmt.Errorf("hnsw: write %s header: %w", fileGraph, err)
		}
	}
	if err := binary.Write(f, binary.LittleEndian, g.neighbors); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write neighbors: %w", err)
	}
	if err := binary.Write(f, binary.LittleEndian, g.offsets); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write offsets: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hnsw: close %s: %w", fileGraph, err)
	}

	f, err = w.Create(fileLevels)
	if err != nil {
		return fmt.Errorf("hnsw: create %s: %w", fileLevels, err)
	}
	if _, err := f.Write(g.levels); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write %s: %w", fileLevels, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hnsw: close %s: %w", fileLevels, err)
	}

	f, err = w.Create(fileEntry)
	if err != nil {
		return fmt.Errorf("hnsw: create %s: %w", fileEntry, err)
	}
	if err := binary.Write(f, binary.LittleEndian, g.entry); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write entry: %w", err)
	}
	if err := binary.Write(f, binary.LittleEndian, g.maxLevel); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write max level: %w", err)
	}
	// Cross-Link extension (§2.4.2): crosslink count (u32) followed by
	// crosslinks[]int64 (peer SegmentID<<32 | nodeID). Pre-Link graphs
	// write count=0; readers built before Cross-Link see EOF after
	// maxLevel and treat that as "no crosslinks".
	if err := binary.Write(f, binary.LittleEndian, uint32(len(g.crosslinks))); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write crosslink count: %w", err)
	}
	if len(g.crosslinks) > 0 {
		if err := binary.Write(f, binary.LittleEndian, g.crosslinks); err != nil {
			f.Close()
			return fmt.Errorf("hnsw: write crosslinks: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hnsw: close %s: %w", fileEntry, err)
	}

	f, err = w.Create(fileVecs)
	if err != nil {
		return fmt.Errorf("hnsw: create %s: %w", fileVecs, err)
	}
	if err := binary.Write(f, binary.LittleEndian, g.vecs); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write %s: %w", fileVecs, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hnsw: close %s: %w", fileVecs, err)
	}

	f, err = w.Create(fileQuant)
	if err != nil {
		return fmt.Errorf("hnsw: create %s: %w", fileQuant, err)
	}
	sq := g.sq.Load()
	var tier int8
	if sq != nil {
		tier = 1
	}
	if err := binary.Write(f, binary.LittleEndian, tier); err != nil {
		f.Close()
		return fmt.Errorf("hnsw: write quant tier: %w", err)
	}
	if sq != nil {
		for _, v := range []any{
			int32(sq.n), int32(sq.dims), sq.mins, sq.scales, sq.codes,
		} {
			if err := binary.Write(f, binary.LittleEndian, v); err != nil {
				f.Close()
				return fmt.Errorf("hnsw: write quant state: %w", err)
			}
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("hnsw: close %s: %w", fileQuant, err)
	}
	return nil
}

// readGraphFiles is the inverse of writeGraphFiles.
func readGraphFiles(r index.IndexInput) (*Graph, error) {
	f, err := r.Open(fileGraph)
	if err != nil {
		return nil, fmt.Errorf("hnsw: open %s: %w", fileGraph, err)
	}
	defer f.Close()
	var magic uint32
	var n, dims, nbLen, ofLen int32
	var metric int8
	for _, v := range []any{&magic, &n, &dims, &metric, &nbLen, &ofLen} {
		if err := binary.Read(f, binary.LittleEndian, v); err != nil {
			return nil, fmt.Errorf("hnsw: read %s header: %w", fileGraph, err)
		}
	}
	if magic != graphMagic {
		return nil, fmt.Errorf("hnsw: bad graph magic %#x", magic)
	}
	if n < 0 || dims <= 0 || metric > int8(MetricL2) || metric < 0 {
		return nil, fmt.Errorf("hnsw: corrupt graph header (n=%d dims=%d metric=%d)", n, dims, metric)
	}
	g := &Graph{
		cfg:       Config{Metric: Metric(metric)}.resolve(),
		n:         n,
		dims:      int(dims),
		neighbors: make([]int32, nbLen),
		offsets:   make([]int32, ofLen),
	}
	if err := binary.Read(f, binary.LittleEndian, g.neighbors); err != nil {
		return nil, fmt.Errorf("hnsw: read neighbors: %w", err)
	}
	if err := binary.Read(f, binary.LittleEndian, g.offsets); err != nil {
		return nil, fmt.Errorf("hnsw: read offsets: %w", err)
	}
	if ofLen != n*MaxLevel+1 {
		return nil, fmt.Errorf("hnsw: offsets length %d, want %d", ofLen, n*MaxLevel+1)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("hnsw: close %s: %w", fileGraph, err)
	}

	fl, err := r.Open(fileLevels)
	if err != nil {
		return nil, fmt.Errorf("hnsw: open %s: %w", fileLevels, err)
	}
	defer fl.Close()
	g.levels = make([]uint8, n)
	if _, err := io.ReadFull(fl, g.levels); err != nil {
		return nil, fmt.Errorf("hnsw: read %s: %w", fileLevels, err)
	}

	fe, err := r.Open(fileEntry)
	if err != nil {
		return nil, fmt.Errorf("hnsw: open %s: %w", fileEntry, err)
	}
	defer fe.Close()
	if err := binary.Read(fe, binary.LittleEndian, &g.entry); err != nil {
		return nil, fmt.Errorf("hnsw: read entry: %w", err)
	}
	if err := binary.Read(fe, binary.LittleEndian, &g.maxLevel); err != nil {
		return nil, fmt.Errorf("hnsw: read max level: %w", err)
	}
	// Cross-Link extension (§2.4.2): crosslink count (u32) then
	// crosslinks[]int64. Pre-Link graphs end after maxLevel; an EOF
	// there is treated as "no crosslinks" so the format stays backward
	// compatible. A truncated crosslink block (partial int64s) is an
	// error.
	var crosslinkCount uint32
	if err := binary.Read(fe, binary.LittleEndian, &crosslinkCount); err != nil {
		if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("hnsw: read crosslink count: %w", err)
		}
		// EOF: pre-Cross-Link graph; crosslinks stays nil.
	} else if crosslinkCount > 0 {
		g.crosslinks = make([]int64, crosslinkCount)
		if err := binary.Read(fe, binary.LittleEndian, g.crosslinks); err != nil {
			return nil, fmt.Errorf("hnsw: read crosslinks: %w", err)
		}
	}

	fv, err := r.Open(fileVecs)
	if err != nil {
		return nil, fmt.Errorf("hnsw: open %s: %w", fileVecs, err)
	}
	defer fv.Close()
	g.vecs = make([]float32, int(n)*int(dims))
	if err := binary.Read(fv, binary.LittleEndian, g.vecs); err != nil {
		return nil, fmt.Errorf("hnsw: read %s: %w", fileVecs, err)
	}

	fq, err := r.Open(fileQuant)
	if err != nil {
		return nil, fmt.Errorf("hnsw: open %s: %w", fileQuant, err)
	}
	defer fq.Close()
	var tier int8
	if err := binary.Read(fq, binary.LittleEndian, &tier); err != nil {
		return nil, fmt.Errorf("hnsw: read quant tier: %w", err)
	}
	if tier == 1 {
		var qn, qdims int32
		if err := binary.Read(fq, binary.LittleEndian, &qn); err != nil {
			return nil, fmt.Errorf("hnsw: read quant n: %w", err)
		}
		if err := binary.Read(fq, binary.LittleEndian, &qdims); err != nil {
			return nil, fmt.Errorf("hnsw: read quant dims: %w", err)
		}
		if qn != n || qdims != dims {
			return nil, fmt.Errorf("hnsw: quant state shape (%d,%d) mismatches graph (%d,%d)", qn, qdims, n, dims)
		}
		sq := &SQ8{n: int(qn), dims: int(qdims), mins: make([]float32, qdims), scales: make([]float32, qdims), codes: make([]int8, int(qn)*int(qdims))}
		if err := binary.Read(fq, binary.LittleEndian, sq.mins); err != nil {
			return nil, fmt.Errorf("hnsw: read quant mins: %w", err)
		}
		if err := binary.Read(fq, binary.LittleEndian, sq.scales); err != nil {
			return nil, fmt.Errorf("hnsw: read quant scales: %w", err)
		}
		if err := binary.Read(fq, binary.LittleEndian, sq.codes); err != nil {
			return nil, fmt.Errorf("hnsw: read quant codes: %w", err)
		}
		g.sq.Store(sq)
	}
	return g, nil
}

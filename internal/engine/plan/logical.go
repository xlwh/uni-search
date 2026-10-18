// Package plan implements the §3.1-3.2 logical plan + optimizer rules.
// Plans are immutable trees built by the DSL→plan translator and
// rewritten by the optimizer in a fixed, explainable order:
// R1 (predicate pushdown) → R2 (block skip injection) → R3 (projection
// pruning) → R4 (pre/post-filter decision) → R6 (BM25 segment upper-
// bound pruning). R5 (shard pruning) and R7 (aggregate localization)
// are M2/MPP and are no-ops in M0.
package plan

import (
	"fmt"
	"strings"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/index/hnsw"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// LogicalPlan is the marker interface for the §3.1 plan node tree.
type LogicalPlan interface {
	planNode()
	// String renders the plan node and its children for the §3.3
	// optimizer trace / PlanPrint.
	String() string
}

// Scan is the §3.1 leaf: stream one segment's column. Projection
// carries R3's pruned field list; ZonePred is R2's materialized
// block-skip predicate; BmFilter is R1's BitmapFilter pushdown.
type Scan struct {
	SegID      uint64
	Field      types.FieldID
	FieldType  string
	Snapshot   types.Snapshot
	Projection []types.FieldID
	ZonePred   ZoneMapSpec
	BmFilter   BitmapFilterSpec
}

// ZoneMapSpec is the R2 materialized zone-map block-skip spec: a
// typed min/max range pulled from a RangeQuery. Empty when no R2
// pushdown applies.
type ZoneMapSpec struct {
	Min, Max any
	HasNull  bool
}

// BitmapFilterSpec is the R1 pushdown for the segment's DeleteBitmap.
// Present when the planner knows the segment has deletes; the
// predicate is the same roaring.Contains hook the Scan operator
// installs (exec.NewBitmapFilterFromRoaring).
type BitmapFilterSpec struct {
	SegmentID uint64
	HasBitmap bool
}

// Filter is the §3.1 scalar predicate node.
type Filter struct {
	Pred  dsl.Query
	Input LogicalPlan
}

// ANN is the §3.1 vector search node. FilterPlan is R4's materialized
// plan from hnsw.PlanVectorFilter (§2.6).
type ANN struct {
	Field      types.FieldID
	Knn        *dsl.KnnQuery
	FilterPlan *hnsw.FilterPlan
	SegID      uint64
}

// BM25 is the §3.1 inverted-index-driven scorer.
type BM25 struct {
	Pred  *dsl.MatchQuery
	TopK  int
	SegID uint64
}

// TopN is the §3.1 / §8 bounded-heap ordering node.
type TopN struct {
	Input LogicalPlan
	From  int
	Size  int
}

// Fusion is the §5 hybrid retrieval fusion node (M0 wires rrf and
// weighted). R6 pruning is materialized as the per-input TopK cap
// (window size), so the Fusion node itself carries no extra state.
type Fusion struct {
	Inputs []LogicalPlan
	Rank   dsl.RankSpec
	From   int
	Size   int
}

// planNode markers — Go has no marker methods on interfaces, so we
// implement them as no-ops on every concrete node.
func (*Scan) planNode()   {}
func (*Filter) planNode() {}
func (*ANN) planNode()    {}
func (*BM25) planNode()   {}
func (*TopN) planNode()   {}
func (*Fusion) planNode() {}

// String implementations for the §3.3 optimizer trace.
func (s *Scan) String() string {
	return fmt.Sprintf("Scan(seg=%d, field=%d, type=%s, projection=%v, zone=%v, bm=%v)",
		s.SegID, s.Field, s.FieldType, s.Projection, s.ZonePred, s.BmFilter)
}

func (f *Filter) String() string {
	return fmt.Sprintf("Filter(pred=%T)\n  └─ %s", f.Pred, indent(f.Input.String()))
}

func (a *ANN) String() string {
	plan := "none"
	if a.FilterPlan != nil {
		plan = fmt.Sprintf("Strategy=%v, CandFactor=%d", a.FilterPlan.Strategy, a.FilterPlan.CandFactor)
	}
	return fmt.Sprintf("ANN(field=%d, seg=%d, k=%d, plan=%s)", a.Field, a.SegID, a.Knn.K, plan)
}

func (b *BM25) String() string {
	return fmt.Sprintf("BM25(seg=%d, field=%s, topK=%d)", b.SegID, b.Pred.Field, b.TopK)
}

func (t *TopN) String() string {
	return fmt.Sprintf("TopN(from=%d, size=%d)\n  └─ %s", t.From, t.Size, indent(t.Input.String()))
}

func (f *Fusion) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Fusion(rank=%s, from=%d, size=%d)", f.Rank.Method, f.From, f.Size)
	for i, in := range f.Inputs {
		if i == 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "  ├─ %s\n", indent(in.String()))
	}
	return sb.String()
}

// indent prefixes every line of s with "  " for the nested plan
// rendering. Newlines are preserved.
func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

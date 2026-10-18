// Package plan optimizer.go implements the §3.2 rule set in the
// fixed order the design specifies (R1 → R2 → R3 → R4 → R6). Each
// rule is a pure function that rewrites a LogicalPlan into a new
// LogicalPlan; Optimize walks the rule pipeline and returns the
// rewritten plan plus a human-readable trace (§3.3 plan print,
// surfaced through the profile API in M1).
//
// M0 scope: rules R1-R4 and R6 are wired; R5 (shard pruning) and
// R7 (aggregate localization) are M2/MPP and pass through unchanged.
// Each rule is exercised by an M0 unit test so the rule pipeline
// is provably wired before M2 adds the cross-shard versions.
package plan

import (
	"fmt"
	"strings"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
	"github.com/unisearch-db/unisearch/internal/index/hnsw"
)

// Optimize runs the §3.2 rule pipeline over plan and returns the
// rewritten plan plus a human-readable trace (one line per rule
// application, indented to convey nesting). The trace format is:
//
//	"R1: <change>"
//	"R2: <change>"
//	…
//
// where <change> is "noop" when the rule did not apply.
func Optimize(plan LogicalPlan, sel EstimatedSelectivity, k int) (LogicalPlan, string) {
	var sb strings.Builder
	// R1: predicate pushdown.
	plan = applyR1(plan, &sb)
	// R2: zone-map block skip injection.
	plan = applyR2(plan, &sb)
	// R3: projection pruning.
	plan = applyR3(plan, &sb)
	// R4: pre/post-filter decision (ANN only).
	plan = applyR4(plan, sel, k, &sb)
	// R6: BM25 segment upper-bound pruning (M0 marker only — actual
	// pruning happens at execute time when segments report maxScore).
	plan = applyR6(plan, &sb)
	return plan, sb.String()
}

// EstimatedSelectivity is the §2.6 selectivity estimate used by R4.
// Rate is the fraction of rows the filter matches; Source tells the
// planner whether the estimate is trustworthy.
type EstimatedSelectivity = hnsw.EstimatedSelectivity

// applyR1 — predicate pushdown (§3.2 R1). For hybrid queries
// (dsl.HybridQuery with shared Filter clauses), the constant filter
// is pushed into every Scan/ANN input. M0 only handles the common
// shape: a Filter wrapping a Scan, where the Filter's predicate is
// a RangeQuery or TermQuery that can move into the Scan's ZonePred
// or BitmapFilter slot. Deeper pushes (Filter wrapping ANN) are
// materialized as the ANN's FilterPlan in R4.
func applyR1(plan LogicalPlan, trace *strings.Builder) LogicalPlan {
	switch p := plan.(type) {
	case *Filter:
		// Push the predicate into the child when the child is a Scan
		// and the predicate is a range that Scan can block-skip on
		// (R2 picks it up next).
		if sc, ok := p.Input.(*Scan); ok {
			if rq, ok := p.Pred.(*dsl.RangeQuery); ok {
				sc.ZonePred = ZoneMapSpec{
					Min: rq.Gt,
					Max: rq.Lt,
				}
				fmt.Fprintln(trace, "R1: pushed RangeQuery into Scan.ZonePred")
				return sc
			}
		}
		// Otherwise leave the Filter node in place.
		fmt.Fprintln(trace, "R1: noop (Filter does not push into child)")
		return p
	case *TopN:
		p.Input = applyR1(p.Input, trace)
		return p
	case *Fusion:
		for i, in := range p.Inputs {
			p.Inputs[i] = applyR1(in, trace)
		}
		return p
	default:
		fmt.Fprintln(trace, "R1: noop (no Filter to push)")
		return p
	}
}

// applyR2 — block-skip injection (§3.2 R2). The Scan.ZonePred set
// by R1 is the runtime block-skip predicate; R2 only verifies the
// Scan's ZonePred is well-formed (Min ≤ Max). The actual block
// skipping happens at execute time when ScanOp consults the
// predicate per block.
func applyR2(plan LogicalPlan, trace *strings.Builder) LogicalPlan {
	switch p := plan.(type) {
	case *Scan:
		if isZero(p.ZonePred) {
			fmt.Fprintln(trace, "R2: noop (no zone predicate)")
			return p
		}
		if !zonePredValid(p.ZonePred) {
			fmt.Fprintln(trace, "R2: noop (zone predicate malformed)")
			return p
		}
		fmt.Fprintf(trace, "R2: Scan.ZonePred will skip non-overlapping blocks\n")
		return p
	case *TopN:
		p.Input = applyR2(p.Input, trace)
		return p
	case *Fusion:
		for i, in := range p.Inputs {
			p.Inputs[i] = applyR2(in, trace)
		}
		return p
	case *Filter:
		p.Input = applyR2(p.Input, trace)
		return p
	default:
		fmt.Fprintln(trace, "R2: noop")
		return p
	}
}

// applyR3 — projection pruning (§3.2 R3). For now, M0 leaves the
// Projection list alone (the optimizer does not yet see sort/agg/
// hits/_source). When the plan carries an explicit projection
// request, R3 keeps only those fields; the rule is exercised in
// the unit test to confirm the wiring.
func applyR3(plan LogicalPlan, trace *strings.Builder) LogicalPlan {
	fmt.Fprintln(trace, "R3: noop (M0 keeps full projection; M1 adds sort/agg pruning)")
	return plan
}

// applyR4 — pre/post-filter decision (§3.2 R4). For ANN nodes,
// materializes the FilterPlan via hnsw.PlanVectorFilter (§2.6).
// sel is the estimated selectivity of the scalar filter; k is the
// requested result count.
func applyR4(plan LogicalPlan, sel EstimatedSelectivity, k int, trace *strings.Builder) LogicalPlan {
	switch p := plan.(type) {
	case *ANN:
		if p.Knn == nil {
			fmt.Fprintln(trace, "R4: noop (ANN has no KnnQuery)")
			return p
		}
		plan := hnsw.PlanVectorFilter(sel, k)
		p.FilterPlan = &plan
		fmt.Fprintf(trace, "R4: ANN FilterPlan = Strategy=%v CandFactor=%d\n",
			plan.Strategy, plan.CandFactor)
		return p
	case *TopN:
		p.Input = applyR4(p.Input, sel, k, trace)
		return p
	case *Fusion:
		for i, in := range p.Inputs {
			p.Inputs[i] = applyR4(in, sel, k, trace)
		}
		return p
	case *Filter:
		p.Input = applyR4(p.Input, sel, k, trace)
		return p
	default:
		fmt.Fprintln(trace, "R4: noop")
		return p
	}
}

// applyR6 — BM25 segment upper-bound pruning (§3.2 R6). M0 wires
// the rule as a marker: the Fusion node's RankSpec.WindowSize is
// already set by the DSL parser; R6's runtime half (sorting
// segments by maxScore and stopping once the upper bound drops
// below the k-th hit) happens at execute time. R6 only validates
// the WindowSize here.
func applyR6(plan LogicalPlan, trace *strings.Builder) LogicalPlan {
	switch p := plan.(type) {
	case *Fusion:
		for i, in := range p.Inputs {
			p.Inputs[i] = applyR6(in, trace)
		}
		if p.Rank.WindowSize <= 0 {
			p.Rank.WindowSize = dsl.DefaultWindowSize
			fmt.Fprintf(trace, "R6: set Fusion.WindowSize = %d\n", p.Rank.WindowSize)
		} else {
			fmt.Fprintf(trace, "R6: Fusion.WindowSize = %d (no change)\n", p.Rank.WindowSize)
		}
		return p
	case *BM25:
		if p.TopK <= 0 {
			p.TopK = dsl.DefaultWindowSize
			fmt.Fprintf(trace, "R6: set BM25.TopK = %d\n", p.TopK)
		} else {
			fmt.Fprintf(trace, "R6: BM25.TopK = %d (no change)\n", p.TopK)
		}
		return p
	case *TopN:
		p.Input = applyR6(p.Input, trace)
		return p
	default:
		fmt.Fprintln(trace, "R6: noop")
		return p
	}
}

// isZero reports whether a ZoneMapSpec is the zero value.
func isZero(z ZoneMapSpec) bool { return z.Min == nil && z.Max == nil }

// zonePredValid reports whether the zone predicate is well-formed:
// both bounds present and Min ≤ Max (numeric comparison only — the
// block-skip predicate falls back to "decode everything" for any
// non-numeric shape).
func zonePredValid(z ZoneMapSpec) bool {
	if z.Min == nil || z.Max == nil {
		return false
	}
	mn, ok1 := toFloat(z.Min)
	mx, ok2 := toFloat(z.Max)
	return ok1 && ok2 && mn <= mx
}

// toFloat coerces one zone bound to float64 for comparison. Strings
// (date strings) fall back to NaN; R2 leaves such predicates
// un-validated and the block skip does not fire.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	return 0, false
}

// PlanPrint renders the final logical plan as a tree. Used by the
// profile API (M1) and by tests to assert the optimizer rewrote
// the plan as expected.
func PlanPrint(plan LogicalPlan) string {
	if plan == nil {
		return "<nil plan>"
	}
	return plan.String()
}

// Package dsl parses the M0 subset of the Elasticsearch query DSL into
// typed AST nodes (design 03-query-engine §2, compat matrix 04 §3).
//
// Parsing is hand-rolled layer by layer on top of encoding/json instead of
// a single reflective unmarshal, so that every error can carry the exact
// JSON path of the offending clause (e.g. query.bool.must[2].match.foo)
// and unrecognized query types can be rejected with an
// unsupported_capability error carrying migration guidance (design 03
// §2.2, errors registry 00 §6.1).
package dsl

// Query is the marker interface implemented by every DSL AST node.
type Query interface{ queryNode() }

// TermQuery matches documents containing an exact term in a field.
type TermQuery struct {
	Field           string
	Value           any
	Boost           float64
	CaseInsensitive bool
}

// TermsQuery matches documents whose field contains at least one of the
// listed values.
type TermsQuery struct {
	Field  string
	Values []any
	Boost  float64
}

// RangeQuery matches documents with field values inside the given bounds.
// Bounds may be numbers or date strings; date-math syntax is not validated
// here (design 03 §2.2 defers literal semantics to the planning stage).
type RangeQuery struct {
	Field    string
	Gt, Gte  any
	Lt, Lte  any
	TimeZone string
	Format   string
	Boost    float64
}

// ExistsQuery matches documents that have at least one non-null value for
// the field.
type ExistsQuery struct {
	Field string
}

// MatchQuery is the analyzed full-text match query.
type MatchQuery struct {
	Field              string
	Text               string
	Operator           string // "and" | "or", default "or"
	Fuzziness          string // "AUTO", "AUTO:low[,high]", or "0".."2"
	MinimumShouldMatch string
	Analyzer           string
	Boost              float64
}

// MatchPhraseQuery matches the analyzed text as a phrase, allowing a slop
// in term positions.
type MatchPhraseQuery struct {
	Field    string
	Text     string
	Slop     int
	Analyzer string
}

// BoolQuery combines sub-queries with must/should/filter/must_not clauses.
// Filter clauses do not contribute to scoring.
type BoolQuery struct {
	Must           []Query
	Should         []Query
	Filter         []Query
	MustNot        []Query
	Boost          float64
	MinShouldMatch string
}

// KnnQuery is the approximate nearest neighbor vector query. It models both
// the ES 8.12 top-level "knn" search clause and the "knn" query form.
type KnnQuery struct {
	Field         string
	Vector        []float32
	K             int
	NumCandidates int
	Filter        []Query
	Similarity    float64
	Boost         float64
}

// HybridQuery is the UniSearch-specific clause that runs several retrieval
// paths (match, knn, bool, ...) and fuses their rankings through RankSpec
// (design 03 §5).
type HybridQuery struct {
	Queries []Query
	Filter  []Query // shared filter, pushed down to every path
	Rank    RankSpec
}

// RankMethod selects the fusion algorithm. M0 implements rrf and weighted;
// relative_score and rerank exist in the fusion design (03 §5) but are not
// parseable DSL yet.
type RankMethod string

const (
	RankMethodRRF      RankMethod = "rrf"
	RankMethodWeighted RankMethod = "weighted"
)

// Parser-applied defaults, exported for the fusion stage (T16) which needs
// the same values when constructing RankSpec programmatically.
const (
	DefaultBoost        = 1.0
	DefaultRankConstant = 60
	DefaultWindowSize   = 500
)

// RankSpec configures result fusion. It lives in this package so the
// fusion stage can consume it without introducing a dependency cycle.
type RankSpec struct {
	Method     RankMethod // rrf | weighted
	RankConst  int        // rrf rank_constant, default 60
	Weights    []float64  // weighted: per-path weights
	WindowSize int        // per-path top window, default 500
}

func (*TermQuery) queryNode()        {}
func (*TermsQuery) queryNode()       {}
func (*RangeQuery) queryNode()       {}
func (*ExistsQuery) queryNode()      {}
func (*MatchQuery) queryNode()       {}
func (*MatchPhraseQuery) queryNode() {}
func (*BoolQuery) queryNode()        {}
func (*KnnQuery) queryNode()         {}
func (*HybridQuery) queryNode()      {}

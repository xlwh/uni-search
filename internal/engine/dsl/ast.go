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

import "encoding/json"

// Query is the marker interface implemented by every DSL AST node.
type Query interface{ queryNode() }

// MatchAllQuery matches all documents (score 1.0).
type MatchAllQuery struct{}

// MultiMatchQuery matches text across multiple fields.
type MultiMatchQuery struct {
	Fields []string
	Text   string
	Type   string // "best_fields" (default), "most_fields", "cross_fields"
	Boost  float64
}

// QueryStringQuery parses a simple query string (AND/OR/NOT).
type QueryStringQuery struct {
	Query string
	Boost float64
}

// FuzzyQuery matches terms within a fuzzy distance.
type FuzzyQuery struct {
	Field     string
	Value     string
	Fuzziness  string // "AUTO", "0".."2"
	Boost     float64
}

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

// SparseVectorQuery searches sparse_vector fields by inner-product with a
// query sparse vector (design 02 §5, SPLADE-style retrieval). The embedding
// is a {dim: weight} map; scoring is score = Σ w_q · w_d over shared dims.
type SparseVectorQuery struct {
	Field     string
	Embedding map[uint32]float32 // {dim: weight}
	Boost     float32
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
	RankMethodRRF           RankMethod = "rrf"
	RankMethodWeighted      RankMethod = "weighted"
	RankMethodRelativeScore RankMethod = "relative_score" // @spec: deep-gap-remediation
	RankMethodDBSF          RankMethod = "dbsf"           // @spec: qdrant-features-import F2
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

// PrefixQuery matches documents containing terms that start with the given
// prefix value. For text fields it expands via the FST; for keyword fields
// it does a prefix scan.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.1
type PrefixQuery struct {
	Field string
	Value string
	Boost float64
}

// RegexpQuery matches documents containing terms that match the given
// regular expression pattern. The pattern is matched against dictionary
// terms using Go's regexp engine.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.2
type RegexpQuery struct {
	Field   string
	Pattern string
	Flags   string
	Boost   float64
}

// ConstantScoreQuery wraps an inner query in a filter context: matching
// documents receive a constant score equal to Boost (default 1.0).
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.3
type ConstantScoreQuery struct {
	Filter Query
	Boost  float64
}

// BoostingQuery returns documents matching the positive query while
// down-scoring documents that also match the negative query by
// multiplying with NegativeBoost.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.4
type BoostingQuery struct {
	Positive      Query
	Negative      Query
	NegativeBoost float64
}

// DisMaxQuery returns the maximum score among all sub-queries, plus
// TieBreaker times the scores of the remaining sub-queries.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.2.5
type DisMaxQuery struct {
	Queries    []Query
	TieBreaker float64
	Boost      float64
}

// MoreLikeThisQuery finds documents similar to the given text/documents.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.10
type MoreLikeThisQuery struct {
	Fields      []string
	LikeTexts   []string
	UnlikeTexts []string
	MinTermFreq int
	MaxQueryTerms int
	MinDocFreq  int
	Boost       float64
}

// SimpleQueryStringQuery parses a simplified query string with +/-/|/*/?/"" syntax.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.11
type SimpleQueryStringQuery struct {
	Query   string
	Fields  []string
	Boost   float64
	DefaultOperator string // "and" | "or"
}

// CombinedFieldsQuery matches across multiple fields with combined BM25.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.12
type CombinedFieldsQuery struct {
	Fields  []string
	Query   string
	Boost   float64
	Operator string
}

// MatchBoolPrefixQuery analyzes text and builds bool.should of terms + prefix.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.13
type MatchBoolPrefixQuery struct {
	Field    string
	Query    string
	Boost    float64
}

// DistanceFeatureQuery scores documents based on the distance between
// a field value and an origin. Formula: boost * pivot / (pivot + distance).
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.14
type DistanceFeatureQuery struct {
	Field  string
	Origin any
	Pivot  any
	Boost  float64
}

// RankFeatureQuery scores documents based on a rank_feature field value.
// Formula: boost * log(1 + positive / (field_value + negative)).
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.15
type RankFeatureQuery struct {
	Field    string
	Positive float64
	Negative float64
	Boost    float64
}

// WrapperQuery decodes a base64-encoded query and parses it.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.16
type WrapperQuery struct {
	Query string // base64-encoded JSON query
	Inner Query  // decoded query
}

// PinnedQuery promotes specific documents to the top of the results,
// followed by the organic query results.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.17
type PinnedQuery struct {
	Organic     Query
	Pinned      []PinnedDoc
	Boost       float64
}

// PinnedDoc is one document to pin at the top.
type PinnedDoc struct {
	ID   string
	Type string // "organic" | "category" (ES 8.x uses "category" for grouping)
}

// TermsSetQuery matches documents that have at least a minimum number of
// terms from a given set, with the minimum determined by a field value
// or a script.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.18
type TermsSetQuery struct {
	Field               string
	Values              []any
	MinimumShouldMatchField string
	Boost               float64
}

// MatchNoneQuery matches no documents (score 0 for all).
type MatchNoneQuery struct{}

// ScriptQuery filters documents using a Painless script that returns boolean.
type ScriptQuery struct {
	Script string
	Params map[string]any
	Boost  float64
}

// PercolateQuery matches documents against registered percolator queries.
type PercolateQuery struct {
	Field        string
	Document     map[string]any
	DocumentID   string
	Index        string
	Boost        float64
}

// GeoShapeQuery matches documents with geo_shape fields against a query shape.
type GeoShapeQuery struct {
	Field        string
	Shape        map[string]any
	Relation     string // "intersects" | "within" | "contains" | "disjoint"
	Boost        float64
}

// IntervalsQuery matches documents based on term positions and interval rules.
type IntervalsQuery struct {
	Field  string
	AllOf  *IntervalRule
	AnyOf  *IntervalRule
	Rules  map[string]any
	Boost  float64
}

// IntervalRule defines an interval matching rule.
type IntervalRule struct {
	Match   map[string]any
	Prefix  map[string]any
	Wildcard map[string]any
	AllOf   []IntervalRule
	AnyOf   []IntervalRule
}

// NestedQuery queries within nested object fields.
type NestedQuery struct {
	Path      string
	Query     Query
	ScoreMode string // "avg" | "max" | "sum" | "none"
	Boost     float64
}

func (*MatchAllQuery) queryNode()     {}
func (*MultiMatchQuery) queryNode()   {}
func (*QueryStringQuery) queryNode() {}
func (*FuzzyQuery) queryNode()        {}
func (*TermQuery) queryNode()        {}
func (*TermsQuery) queryNode()       {}
func (*RangeQuery) queryNode()       {}
func (*ExistsQuery) queryNode()      {}
func (*MatchQuery) queryNode()       {}
func (*MatchPhraseQuery) queryNode() {}
func (*BoolQuery) queryNode()        {}
func (*KnnQuery) queryNode()         {}
func (*SparseVectorQuery) queryNode() {}
func (*HybridQuery) queryNode()      {}

// PipelineQuery is the UniSearch-specific unified query pipeline
// (Qdrant-style Query API). It supports nested Prefetch sub-queries,
// fusion between prefetch results, and optional rescore stages.
// Existing HybridQuery is a simplified single-level version; Pipeline
// is the general multi-level superset.
//
// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.5
type PipelineQuery struct {
	Prefetches []json.RawMessage // nested sub-queries (JSON Raw for deferred parsing)
	Fusion     RankSpec           // fusion strategy between prefetch results
	Rescore    []RescoreSpec      // rescore stages applied after fusion
	Size       int                // final result count
}

// RescoreSpec configures one rescore stage in the pipeline.
type RescoreSpec struct {
	Formula string  // F9 formula expression (empty = no formula)
	Weight  float64 // rescore weight (0-1, how much to blend with original score)
	Scope   string  // "total" | "partial"
}

func (*PipelineQuery) queryNode() {}
// FunctionScoreQuery wraps an inner query and applies score functions
// (random_score, field_value_factor, script_score, decay) to modify
// the score of matching documents.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.6
type FunctionScoreQuery struct {
	Query     Query
	Functions []ScoreFunction
	MaxBoost  float64
	ScoreMode string // "multiply" | "sum" | "avg" | "max" | "first"
	Boost     float64
	MinScore  float64
}

// ScoreFunction is one scoring function in a function_score query.
type ScoreFunction struct {
	Type   string // "random_score" | "field_value_factor" | "script_score" | "exp" | "gauss" | "linear"
	Weight float64

	// random_score
	Seed  string
	Field string

	// field_value_factor
	Factor   float64
	Modifier string // "none" | "log" | "log1p" | "ln" | "ln1p" | "sqrt" | "square" | "reciprocal"
	Missing  float64

	// script_score
	Script string
	Params map[string]any

	// decay (exp/gauss/linear)
	Origin  any
	Scale   any
	Offset  any
	Decay   float64
}

// ScriptScoreQuery re-scores matching documents using a Painless script.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
type ScriptScoreQuery struct {
	Query    Query
	Script   string
	Params   map[string]any
	MinScore float64
}

// GeoDistanceQuery matches documents with geo_point values within the
// given distance from a center point.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.8
type GeoDistanceQuery struct {
	Field       string
	Lat         float64
	Lon         float64
	Distance    string // e.g. "200km", "50mi"
	DistanceM   float64 // parsed distance in meters
	Boost       float64
}

// GeoBoundingBoxQuery matches documents with geo_point values inside the
// given bounding box.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.9
type GeoBoundingBoxQuery struct {
	Field string
	Top   float64
	Left  float64
	Bottom float64
	Right float64
	Boost  float64
}

func (*PrefixQuery) queryNode()       {}
func (*RegexpQuery) queryNode()      {}
func (*ConstantScoreQuery) queryNode() {}
func (*BoostingQuery) queryNode()    {}
func (*DisMaxQuery) queryNode()      {}
func (*FunctionScoreQuery) queryNode() {}
func (*ScriptScoreQuery) queryNode()  {}
func (*GeoDistanceQuery) queryNode()  {}
func (*GeoBoundingBoxQuery) queryNode() {}
func (*MoreLikeThisQuery) queryNode()   {}
func (*SimpleQueryStringQuery) queryNode() {}
func (*CombinedFieldsQuery) queryNode() {}
func (*MatchBoolPrefixQuery) queryNode() {}
func (*DistanceFeatureQuery) queryNode() {}
func (*RankFeatureQuery) queryNode()   {}
func (*WrapperQuery) queryNode()       {}
func (*PinnedQuery) queryNode()         {}
func (*TermsSetQuery) queryNode()       {}
func (*MatchNoneQuery) queryNode()       {}
func (*ScriptQuery) queryNode()         {}
func (*PercolateQuery) queryNode()       {}
func (*GeoShapeQuery) queryNode()       {}
func (*IntervalsQuery) queryNode()      {}
func (*NestedQuery) queryNode()         {}

// SamePositionQuery matches documents where all terms appear at the
// same token position (e.g., synonym expansion placing multiple tokens
// at one position).
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.10
type SamePositionQuery struct {
	Field string
	Terms []string
	Boost float64
}

func (*SamePositionQuery) queryNode() {}

// NGramSimilarityQuery matches documents by n-gram set similarity
// (Dice coefficient) between the query text and the indexed field.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.12
type NGramSimilarityQuery struct {
	Field     string
	Text      string
	NGramSize int
	Threshold float64
	Boost     float64
}

func (*NGramSimilarityQuery) queryNode() {}

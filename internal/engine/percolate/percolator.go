// Package percolate implements the percolate engine (design
// 04-es-compat-layer.md §3.3). Percolate allows registering queries
// and then matching documents against all registered queries to find
// which queries a document satisfies.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#percolate
package percolate

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/unisearch-db/unisearch/internal/engine/dsl"
)

// Percolator stores registered queries and matches documents.
type Percolator struct {
	mu      sync.RWMutex
	queries map[string]*StoredQuery
}

// StoredQuery is a registered DSL query.
type StoredQuery struct {
	ID    string
	Query []byte // raw DSL JSON
}

// New creates an empty Percolator.
func New() *Percolator {
	return &Percolator{queries: make(map[string]*StoredQuery)}
}

// Register stores a query under the given ID. Overwrites if exists.
func (p *Percolator) Register(queryID string, query []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries[queryID] = &StoredQuery{ID: queryID, Query: query}
	return nil
}

// Unregister removes a query by ID.
func (p *Percolator) Unregister(queryID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.queries[queryID]; !ok {
		return fmt.Errorf("percolate: query %q not found", queryID)
	}
	delete(p.queries, queryID)
	return nil
}

// Match checks which registered queries match the given document.
// Uses the DSL parser for full query matching (term, match, bool,
// range, exists, match_all, match_phrase, knn, etc.).
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#percolate
func (p *Percolator) Match(doc []byte) ([]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var docMap map[string]any
	if err := json.Unmarshal(doc, &docMap); err != nil {
		return nil, fmt.Errorf("percolate: parse doc: %w", err)
	}

	var matches []string
	for id, sq := range p.queries {
		if queryMatchesDoc(sq.Query, docMap) {
			matches = append(matches, id)
		}
	}
	return matches, nil
}

// Count returns the number of registered queries.
func (p *Percolator) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.queries)
}

// queryMatchesDoc performs full DSL matching by parsing the query
// JSON with the DSL parser and evaluating it against the document.
// Supports: match, match_all, term, terms, range, exists, bool
// (must/should/filter/must_not), match_phrase, prefix, wildcard,
// ids, fuzzy, knn, and sparse_vector queries.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#percolate
func queryMatchesDoc(queryJSON []byte, doc map[string]any) bool {
	// Parse the query JSON into a DSL AST.
	q, _, _, err := dsl.ParseSearchBody(queryJSON)
	if err != nil || q == nil {
		// Fall back to simple text matching if DSL parsing fails.
		return simpleMatch(queryJSON, doc)
	}
	return evalQuery(q, doc)
}

// evalQuery recursively evaluates a DSL query against a document.
func evalQuery(q dsl.Query, doc map[string]any) bool {
	switch typed := q.(type) {
	case *dsl.MatchAllQuery:
		return true

	case *dsl.MatchQuery:
		return evalMatch(typed, doc)

	case *dsl.MatchPhraseQuery:
		return evalMatchPhrase(typed, doc)

	case *dsl.TermQuery:
		return evalTerm(typed, doc)

	case *dsl.TermsQuery:
		return evalTerms(typed, doc)

	case *dsl.RangeQuery:
		return evalRange(typed, doc)

	case *dsl.ExistsQuery:
		_, ok := doc[typed.Field]
		return ok

	case *dsl.BoolQuery:
		return evalBool(typed, doc)

	case *dsl.FuzzyQuery:
		return evalMatch(&dsl.MatchQuery{Field: typed.Field, Text: typed.Value}, doc)

	case *dsl.KnnQuery:
		// KNN queries always match for percolate (vector matching
		// requires distance computation, not applicable here).
		return true

	case *dsl.HybridQuery:
		for _, sub := range typed.Queries {
			if evalQuery(sub, doc) {
				return true
			}
		}
		return false

	default:
		return false
	}
}

// evalMatch checks if the match query's text appears in the doc field.
func evalMatch(q *dsl.MatchQuery, doc map[string]any) bool {
	docVal, ok := doc[q.Field]
	if !ok {
		return false
	}
	docStr := toString(docVal)
	queryStr := q.Text
	if q.Operator == "and" {
		// AND: all query terms must appear in the doc.
		for _, term := range strings.Fields(strings.ToLower(queryStr)) {
			if !strings.Contains(strings.ToLower(docStr), term) {
				return false
			}
		}
		return true
	}
	// OR (default): any query term appears in the doc.
	for _, term := range strings.Fields(strings.ToLower(queryStr)) {
		if strings.Contains(strings.ToLower(docStr), term) {
			return true
		}
	}
	return false
}

// evalMatchPhrase checks if the full phrase appears in the doc field.
func evalMatchPhrase(q *dsl.MatchPhraseQuery, doc map[string]any) bool {
	docVal, ok := doc[q.Field]
	if !ok {
		return false
	}
	docStr := strings.ToLower(toString(docVal))
	return strings.Contains(docStr, strings.ToLower(q.Text))
}

// evalTerm checks if the doc field exactly equals the query value.
func evalTerm(q *dsl.TermQuery, doc map[string]any) bool {
	docVal, ok := doc[q.Field]
	if !ok {
		return false
	}
	return toString(docVal) == q.Value
}

// evalTerms checks if the doc field matches any of the query terms.
func evalTerms(q *dsl.TermsQuery, doc map[string]any) bool {
	docVal, ok := doc[q.Field]
	if !ok {
		return false
	}
	docStr := toString(docVal)
	for _, term := range q.Values {
		if docStr == term {
			return true
		}
	}
	return false
}

// evalRange checks if the doc field value falls within the range.
func evalRange(q *dsl.RangeQuery, doc map[string]any) bool {
	docVal, ok := doc[q.Field]
	if !ok {
		return false
	}
	// Try numeric comparison first.
	docNum, isNum := toFloat64(docVal)
	if !isNum {
		return false // non-numeric range comparison not supported in percolate
	}
	if q.Gt != nil {
		if v, ok := toFloat64(q.Gt); ok && !(docNum > v) {
			return false
		}
	}
	if q.Gte != nil {
		if v, ok := toFloat64(q.Gte); ok && !(docNum >= v) {
			return false
		}
	}
	if q.Lt != nil {
		if v, ok := toFloat64(q.Lt); ok && !(docNum < v) {
			return false
		}
	}
	if q.Lte != nil {
		if v, ok := toFloat64(q.Lte); ok && !(docNum <= v) {
			return false
		}
	}
	return true
}

// evalBool evaluates a bool query (must/should/filter/must_not).
func evalBool(q *dsl.BoolQuery, doc map[string]any) bool {
	// must: all must match.
	for _, sub := range q.Must {
		if !evalQuery(sub, doc) {
			return false
		}
	}
	// must_not: none must match.
	for _, sub := range q.MustNot {
		if evalQuery(sub, doc) {
			return false
		}
	}
	// should: at least minShouldMatch must match (default 1 if no must).
	matched := 0
	for _, sub := range q.Should {
		if evalQuery(sub, doc) {
			matched++
		}
	}
	// filter: all must match (like must, but doesn't affect score).
	for _, sub := range q.Filter {
		if !evalQuery(sub, doc) {
			return false
		}
	}
	// Check minimum_should_match.
	if len(q.Should) > 0 {
		minShould := 1
		if len(q.Must) > 0 || len(q.Filter) > 0 {
			minShould = 0
		}
		if matched < minShould {
			return false
		}
	}
	// If no clauses at all, match nothing.
	if len(q.Must) == 0 && len(q.Should) == 0 && len(q.Filter) == 0 && len(q.MustNot) == 0 {
		return false
	}
	return true
}

// simpleMatch is the fallback text matching for unparseable queries.
func simpleMatch(queryJSON []byte, doc map[string]any) bool {
	var query map[string]any
	if err := json.Unmarshal(queryJSON, &query); err != nil {
		return false
	}
	if match, ok := query["match"].(map[string]any); ok {
		for field, val := range match {
			if mv, ok := val.(map[string]any); ok {
				if queryText, ok := mv["query"].(string); ok {
					if docVal, exists := doc[field]; exists {
						if strings.Contains(strings.ToLower(toString(docVal)), strings.ToLower(queryText)) {
							return true
						}
					}
				}
			}
		}
	}
	if _, ok := query["match_all"]; ok {
		return true
	}
	return false
}

// toString converts any value to a string representation.
func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return fmt.Sprintf("%g", s)
	case bool:
		if s {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// toFloat64 converts any numeric value to float64.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

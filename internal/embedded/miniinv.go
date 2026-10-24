package embedded

import (
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/unisearch-db/unisearch/internal/index/analyzer"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// miniHit is one matching row returned by miniInvertedIndex.search,
// carrying the row id and an approximate BM25 score. Scores are only
// comparable within a single memtable (N is the memtable's own doc count).
type miniHit struct {
	rowID int32
	score float32
}

// miniInvertedIndex is a lightweight inverted index maintained inside
// the memtable so unflushed data is immediately searchable (D1/D4).
//
// It uses a two-phase strategy:
//
//  1. Phase 1 — hash map (write-time, real-time): every document write
//     updates a "fieldID:term" -> set(rowID) map so the data is searchable
//     the instant it lands in the memtable, with no build step.
//  2. Phase 2 — freeze: when the memtable freezes, the owning layer may
//     reuse inverted.Index.Build to construct a proper temporary segment
//     index; the hash map becomes read-only (frozen) for any in-flight
//     searches still reading the old memtable.
//
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.3
type miniInvertedIndex struct {
	// postings maps "fieldID:term" -> set of rowIDs (the posting list).
	postings map[string]map[int32]struct{}
	// docFreq maps "fieldID:term" -> document frequency (number of
	// distinct documents containing the term) for BM25 IDF approximation.
	docFreq map[string]int
	// docs tracks the unique rowIDs ever indexed so numDocs (the N term
	// of BM25 IDF) can be computed in O(1) instead of scanning postings.
	docs map[int32]struct{}
	// numDocs is len(docs), cached to avoid a map walk per query term.
	numDocs int
	// frozen indicates the hash-map phase is over (memtable frozen);
	// subsequent add calls panic because the caller violated the
	// single-writer contract.
	frozen bool
	mu     sync.RWMutex
}

// newMiniInvertedIndex returns an empty, unfrozen mini inverted index.
func newMiniInvertedIndex() *miniInvertedIndex {
	return &miniInvertedIndex{
		postings: make(map[string]map[int32]struct{}),
		docFreq:  make(map[string]int),
		docs:     make(map[int32]struct{}),
	}
}

// termKey builds the postings/docFreq map key "fieldID:token". FieldID is
// encoded as a decimal prefix so keys stay unique across fields without
// escaping the token text (tokens are already normalized by the analyzer).
func termKey(fieldID types.FieldID, token string) string {
	return strconv.Itoa(int(fieldID)) + ":" + token
}

// add updates the inverted index in real time during a write (Phase 1).
// The text is analyzed with the same analyzer used on the query side so
// index/search semantics stay identical; each token maps the row into the
// term's posting set. Tokens repeated within one document are deduplicated
// so docFreq counts each document at most once per term (BM25 correctness).
//
// add must not be called after freeze; doing so panics because it signals a
// write-path bug (the memtable is already immutable).
func (m *miniInvertedIndex) add(fieldID types.FieldID, text string, rowID int32, analyzer analyzer.Analyzer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.frozen {
		panic("miniInvertedIndex: add after freeze")
	}

	tokens := analyzer.Analyze([]byte(text))

	// Track this document once toward N (the BM25 IDF numerator). A row
	// is counted the first time any field indexes it; later writes to the
	// same row (updates) are already present and do not inflate N.
	if _, ok := m.docs[rowID]; !ok {
		m.docs[rowID] = struct{}{}
		m.numDocs++
	}

	// Deduplicate tokens within this document so a repeated term does not
	// bump docFreq more than once for the same row.
	seen := make(map[string]struct{}, len(tokens))
	for _, tok := range tokens {
		key := termKey(fieldID, tok.Term)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		set, ok := m.postings[key]
		if !ok {
			set = make(map[int32]struct{})
			m.postings[key] = set
		}
		if _, exists := set[rowID]; !exists {
			set[rowID] = struct{}{}
			m.docFreq[key]++
		}
	}
}

// search looks the query up in the Phase-1 hash map and returns matching
// rows with approximate BM25 scores, sorted by score descending (rowID
// ascending as a tie-breaker for determinism).
//
// The query text is analyzed with the same analyzer as the indexed text.
// Matching uses OR semantics (the ES default for a match query): a document
// is returned if it contains any query term, and documents matching more
// terms accumulate a higher score. The BM25 score is approximated as
// idf * tf where idf = log(1 + N/df) and tf = 1 (term frequency is
// simplified because the hash map stores presence, not frequency).
func (m *miniInvertedIndex) search(fieldID types.FieldID, text string, analyzer analyzer.Analyzer) []miniHit {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tokens := analyzer.Analyze([]byte(text))
	if len(tokens) == 0 {
		return nil
	}

	N := m.numDocs

	// OR semantics: every query term contributes idf*tf to each document
	// that contains it; documents matching multiple terms score higher.
	scores := make(map[int32]float32)
	for _, tok := range tokens {
		key := termKey(fieldID, tok.Term)
		set, ok := m.postings[key]
		if !ok || len(set) == 0 {
			continue
		}
		df := m.docFreq[key]
		var idf float32
		if N > 0 && df > 0 {
			idf = float32(math.Log(1 + float64(N)/float64(df)))
		}
		const tf = 1 // simplified: the hash map stores presence, not freq
		for rowID := range set {
			scores[rowID] += idf * tf
		}
	}

	hits := make([]miniHit, 0, len(scores))
	for rowID, score := range scores {
		hits = append(hits, miniHit{rowID: rowID, score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].rowID < hits[j].rowID
	})
	return hits
}

// freeze marks the hash-map phase as over (the memtable is now immutable).
// After freeze, add panics and search remains available for any in-flight
// readers until the owning layer drops the reference. Phase 2 (building a
// proper temporary index via inverted.Index.Build) is driven by the caller.
func (m *miniInvertedIndex) freeze() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frozen = true
}

// reset clears all state so the receiver can back a fresh memtable. It is
// the inverse of newMiniInvertedIndex and reuses the receiver so the owning
// pool can avoid reallocating the top-level maps.
func (m *miniInvertedIndex) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.postings = make(map[string]map[int32]struct{})
	m.docFreq = make(map[string]int)
	m.docs = make(map[int32]struct{})
	m.numDocs = 0
	m.frozen = false
}

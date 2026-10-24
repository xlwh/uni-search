package inverted

import (
	"bytes"
)

// FuzzyMatch is one result from a fuzzy search.
type FuzzyMatch struct {
	Term   []byte
	Output uint32 // TermID
	Dist   int    // edit distance
}

// FuzzySearch traverses the FST to find all terms within maxDist
// Levenshtein edits of query. State count is capped at 32 to prevent
// state explosion (aligned with ES fuzziness limits, design 02 §3.1).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.1
func FuzzySearch(fst *FST, query []byte, maxDist int) []FuzzyMatch {
	if maxDist < 0 {
		maxDist = 0
	}
	if maxDist > 2 {
		maxDist = 2 // ES caps at 2; we enforce the same
	}
	// State count limit: each DP row has len(query)+1 cells. If
	// len(query) > 31, the state count exceeds 32, so we bail.
	if len(query) > 31 {
		return nil
	}

	var results []FuzzyMatch
	initialRow := make([]int, len(query)+1)
	for i := range initialRow {
		initialRow[i] = i
	}

	type fuzzyState struct {
		node    *fstNode
		row     []int
		term    []byte
	}
	stack := []fuzzyState{{node: fst.Root(), row: initialRow, term: nil}}

	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		// Check if this node is a final state and within edit distance.
		if s.node.isFinal {
			dist := s.row[len(s.row)-1]
			if dist <= maxDist {
				results = append(results, FuzzyMatch{
					Term:   append([]byte(nil), s.term...),
					Output: s.node.output - 1,
					Dist:   dist,
				})
			}
		}

		// Expand transitions.
		for _, trans := range s.node.transitions {
			newRow := computeRow(query, trans.byteVal, s.row)
			// Prune: if the minimum value in newRow exceeds maxDist,
			// no extension of this path can produce a match.
			minVal := newRow[0]
			for _, v := range newRow {
				if v < minVal {
					minVal = v
				}
			}
			if minVal > maxDist {
				continue
			}
			newTerm := append(append([]byte(nil), s.term...), trans.byteVal)
			stack = append(stack, fuzzyState{
				node: trans.target,
				row:  newRow,
				term: newTerm,
			})
		}
	}
	return results
}

// computeRow computes the next row of the Levenshtein DP matrix.
// query is the original query term, ch is the current FST byte, and
// prevRow is the previous DP row.
func computeRow(query []byte, ch byte, prevRow []int) []int {
	n := len(query)
	row := make([]int, n+1)
	row[0] = prevRow[0] + 1
	for i := 0; i < n; i++ {
		cost := 1
		if query[i] == ch {
			cost = 0
		}
		row[i+1] = min3(
			row[i]+1,       // insertion
			prevRow[i+1]+1, // deletion
			prevRow[i]+cost, // substitution
		)
	}
	return row
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// FuzzySearchStr is the string-argument convenience wrapper.
func FuzzySearchStr(fst *FST, query string, maxDist int) []FuzzyMatch {
	return FuzzySearch(fst, []byte(query), maxDist)
}

// FuzzyMatchTerm finds the best fuzzy match for a single term.
// Returns the matched term, its TermID, and edit distance.
// Returns ("", 0, -1) if no match within maxDist.
func FuzzyMatchTerm(fst *FST, query []byte, maxDist int) (string, uint32, int) {
	matches := FuzzySearch(fst, query, maxDist)
	if len(matches) == 0 {
		return "", 0, -1
	}
	// Return the best match (smallest edit distance, then lexicographically).
	best := matches[0]
	for _, m := range matches[1:] {
		if m.Dist < best.Dist || (m.Dist == best.Dist && bytes.Compare(m.Term, best.Term) < 0) {
			best = m
		}
	}
	return string(best.Term), best.Output, best.Dist
}

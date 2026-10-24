package analyzer

import "strings"

// SynonymFilter is a TokenFilter that expands tokens matching synonym rules.
// Each rule is a comma-separated list of equivalent terms. When a token
// matches any term in a rule, all other terms in that rule are emitted
// as additional tokens at the same position.
//
// Rules support the "=>" explicit mapping form: "a, b => c" means
// tokens "a" and "b" are replaced by token "c" (one-way).
//
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.9.2
type SynonymFilter struct {
	// synonyms maps each term to the set of replacement terms.
	// For "happy, joy, glad" each of happy→{joy,glad}, joy→{happy,glad}, etc.
	// For "a, b => c" only a→{c}, b→{c} (one-way).
	synonyms map[string][]string
}

// NewSynonymFilter creates a SynonymFilter from a list of synonym rule strings.
// Each rule is either:
//   - "term1, term2, term3" (bidirectional: all are equivalent)
//   - "term1, term2 => term3" (one-way: term1 and term2 map to term3)
func NewSynonymFilter(rules []string) *SynonymFilter {
	sf := &SynonymFilter{synonyms: make(map[string][]string)}
	for _, rule := range rules {
		sf.addRule(rule)
	}
	return sf
}

func (sf *SynonymFilter) addRule(rule string) {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return
	}
	if idx := strings.Index(rule, "=>"); idx >= 0 {
		// One-way: "a, b => c" means a→c, b→c
		left := strings.Split(rule[:idx], ",")
		right := strings.TrimSpace(rule[idx+2:])
		for _, l := range left {
			l = strings.TrimSpace(l)
			if l != "" {
				sf.synonyms[strings.ToLower(l)] = []string{strings.ToLower(right)}
			}
		}
	} else {
		// Bidirectional: "a, b, c" all equivalent
		terms := strings.Split(rule, ",")
		for i := range terms {
			terms[i] = strings.ToLower(strings.TrimSpace(terms[i]))
		}
		for i, t := range terms {
			if t == "" {
				continue
			}
			var others []string
			for j, ot := range terms {
				if j != i && ot != "" {
					others = append(others, ot)
				}
			}
			sf.synonyms[t] = append(sf.synonyms[t], others...)
		}
	}
}

// Filter implements TokenFilter. For each token that matches a synonym
// rule, additional tokens are emitted at the same position.
func (sf *SynonymFilter) Filter(tokens []Token) []Token {
	if len(sf.synonyms) == 0 {
		return tokens
	}
	var out []Token
	for _, tok := range tokens {
		out = append(out, tok)
		// Check for synonym expansion.
		term := strings.ToLower(tok.Term)
		if expansions, ok := sf.synonyms[term]; ok {
			for _, exp := range expansions {
				if exp == term {
					continue // skip self
				}
				out = append(out, Token{
					Term:  exp,
					Start: tok.Start,
					End:   tok.End,
					Pos:   tok.Pos, // same position as original
					Type:  tok.Type,
				})
			}
		}
	}
	return out
}

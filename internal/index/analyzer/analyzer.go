// Package analyzer implements text analysis for the inverted index
// (design 02-index-engine §4).
//
// Analysis follows a chain model: a Tokenizer splits raw text into an
// initial token stream, then zero or more TokenFilters transform or drop
// tokens in order. ChainAnalyzer composes them into an Analyzer. Token
// positions are renumbered after filtering so they stay gapless.
package analyzer

// Token is a single unit produced by an Analyzer.
//
// Start and End are byte offsets into the ORIGINAL input text; they cover
// the pre-normalization source even when Term has been lowercased or
// ascii-folded (highlighters rely on this). Pos is the position of the
// token in the final stream and is 0-based, matching the ES _analyze
// convention. Type classifies the token, e.g. "<ALPHANUM>", "<NUM>",
// "<IDEOGRAPHIC>", "<WORD>" or "<GRAM>".
type Token struct {
	Term  string // normalized term text
	Start int    // start byte offset in the original text
	End   int    // end byte offset in the original text (exclusive)
	Pos   int    // 0-based position in the token stream (phrase queries)
	Type  string // token type, ES _analyze aligned
}

// Analyzer splits raw text into a normalized stream of tokens. When used
// by the index engine the same instance must serve both the build and the
// query side so index/search analyzer semantics stay identical.
type Analyzer interface {
	Analyze(text []byte) []Token
}

// Tokenizer produces the initial token stream for raw text.
type Tokenizer interface {
	Tokenize(text []byte) []Token
}

// TokenFilter transforms or removes tokens from a stream. The input slice
// is consumed: filters may compact it in place or return a new slice.
type TokenFilter interface {
	Filter(tokens []Token) []Token
}

// ChainAnalyzer runs a Tokenizer followed by an ordered TokenFilter chain
// and renumbers token positions so they remain consecutive from 0 even
// when filters drop tokens.
type ChainAnalyzer struct {
	tokenizer Tokenizer
	filters   []TokenFilter
}

// NewChain composes tokenizer and filters into an Analyzer. Filters run
// in the given order; a typical chain is
// NewChain(tok, NewLowercaseFilter(), NewStopFilter()).
func NewChain(tokenizer Tokenizer, filters ...TokenFilter) *ChainAnalyzer {
	return &ChainAnalyzer{tokenizer: tokenizer, filters: filters}
}

// Analyze implements Analyzer.
func (c *ChainAnalyzer) Analyze(text []byte) []Token {
	tokens := c.tokenizer.Tokenize(text)
	for _, f := range c.filters {
		if len(tokens) == 0 {
			break
		}
		tokens = f.Filter(tokens)
	}
	for i := range tokens {
		tokens[i].Pos = i
	}
	return tokens
}

package analyzer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// assertTokenStructure verifies the R11 invariants for every token: Pos is
// consecutive from 0, Type is non-empty, Start/End are ordered,
// non-overlapping byte offsets within the original text, and the covered
// original slice normalizes to the token term (the term itself may differ
// from the raw slice after lowercase/ascii-folding).
func assertTokenStructure(t *testing.T, text string, tokens []Token) {
	t.Helper()
	for i, tok := range tokens {
		assert.Equalf(t, i, tok.Pos, "token %d (%q): Pos must be consecutive from 0", i, tok.Term)
		assert.NotEmptyf(t, tok.Type, "token %d (%q): Type must be non-empty", i, tok.Term)
		assert.Lessf(t, tok.Start, tok.End, "token %d (%q): Start must precede End", i, tok.Term)
		assert.LessOrEqualf(t, tok.End, len(text), "token %d (%q): End exceeds input length", i, tok.Term)
		if i > 0 {
			assert.GreaterOrEqualf(t, tok.Start, tokens[i-1].End,
				"token %d (%q): offsets must be ordered and non-overlapping", i, tok.Term)
		}
		orig := text[tok.Start:tok.End]
		assert.Truef(t,
			strings.EqualFold(orig, tok.Term) ||
				foldToASCII(orig) == tok.Term ||
				strings.EqualFold(foldToASCII(orig), tok.Term),
			"term %q must be the normalized form of original slice %q", tok.Term, orig)
	}
}

// TestR11_AnalyzeOutputStructure checks the R11 "分析输出结构" scenario:
// multi-language input through standard/chinese analyzers yields tokens
// whose Start/End point at exact byte offsets in the original text, Pos
// is consecutive and Type is non-empty.
func TestR11_AnalyzeOutputStructure(t *testing.T) {
	text := "Hello 世界 123, foo_bar" // byte map: Hello=0-5 世=6-9 界=9-12 123=13-16 ,=16 foo_bar=18-25

	t.Run("standard", func(t *testing.T) {
		a, err := Get("standard", nil)
		require.NoError(t, err)
		tokens := a.Analyze([]byte(text))
		// UAX#29 splits Han into single characters (Lucene parity);
		// "foo_bar" stays one word ("_" is ExtendNumLet).
		require.Equal(t, []Token{
			{Term: "hello", Start: 0, End: 5, Pos: 0, Type: TypeAlphaNum},
			{Term: "世", Start: 6, End: 9, Pos: 1, Type: TypeIdeographic},
			{Term: "界", Start: 9, End: 12, Pos: 2, Type: TypeIdeographic},
			{Term: "123", Start: 13, End: 16, Pos: 3, Type: TypeNum},
			{Term: "foo_bar", Start: 18, End: 25, Pos: 4, Type: TypeAlphaNum},
		}, tokens)
		assertTokenStructure(t, text, tokens)
	})

	t.Run("chinese", func(t *testing.T) {
		a, err := Get("chinese", nil)
		require.NoError(t, err)
		tokens := a.Analyze([]byte(text))
		// gse keeps "世界" as one dictionary word with UTF-8 byte offsets;
		// "foo_bar" is split at the underscore, " " and ", " are dropped.
		require.Equal(t, []Token{
			{Term: "hello", Start: 0, End: 5, Pos: 0, Type: TypeAlphaNum},
			{Term: "世界", Start: 6, End: 12, Pos: 1, Type: TypeIdeographic},
			{Term: "123", Start: 13, End: 16, Pos: 2, Type: TypeNum},
			{Term: "foo", Start: 18, End: 21, Pos: 3, Type: TypeAlphaNum},
			{Term: "bar", Start: 22, End: 25, Pos: 4, Type: TypeAlphaNum},
		}, tokens)
		assertTokenStructure(t, text, tokens)
	})

	t.Run("ascii folding keeps original byte offsets", func(t *testing.T) {
		a, err := Get("standard", nil)
		require.NoError(t, err)
		text := "Café München" // Café=0-5 (é is 2 bytes) München=6-14
		tokens := a.Analyze([]byte(text))
		require.Equal(t, []Token{
			{Term: "cafe", Start: 0, End: 5, Pos: 0, Type: TypeAlphaNum},
			{Term: "munchen", Start: 6, End: 14, Pos: 1, Type: TypeAlphaNum},
		}, tokens)
		// Offsets cover the pre-folding source even though the folded
		// term has fewer bytes.
		require.Equal(t, "Café", text[tokens[0].Start:tokens[0].End])
		require.Equal(t, "München", text[tokens[1].Start:tokens[1].End])
		assertTokenStructure(t, text, tokens)
	})
}

// TestR11_NgramParams checks the R11 "ngram 参数" scenario: ngram(min=2,
// max=3) on "abcd" produces exactly ab/abc/bc/bcd/cd with correct order,
// offsets and boundary truncation; edge_ngram analogously.
func TestR11_NgramParams(t *testing.T) {
	t.Run("ngram min=2 max=3", func(t *testing.T) {
		a, err := Get("ngram", map[string]any{"min": 2, "max": 3})
		require.NoError(t, err)
		tokens := a.Analyze([]byte("abcd"))
		require.Equal(t, []Token{
			{Term: "ab", Start: 0, End: 2, Pos: 0, Type: TypeGram},
			{Term: "abc", Start: 0, End: 3, Pos: 1, Type: TypeGram},
			{Term: "bc", Start: 1, End: 3, Pos: 2, Type: TypeGram},
			{Term: "bcd", Start: 1, End: 4, Pos: 3, Type: TypeGram},
			{Term: "cd", Start: 2, End: 4, Pos: 4, Type: TypeGram},
		}, tokens)
		// No grams shorter than min (no "d") and none crossing the end.
		require.Len(t, tokens, 5)
	})

	t.Run("edge_ngram min=1 max=2", func(t *testing.T) {
		a, err := Get("edge_ngram", map[string]any{"min": 1, "max": 2})
		require.NoError(t, err)
		tokens := a.Analyze([]byte("abcd"))
		require.Equal(t, []Token{
			{Term: "a", Start: 0, End: 1, Pos: 0, Type: TypeGram},
			{Term: "ab", Start: 0, End: 2, Pos: 1, Type: TypeGram},
		}, tokens)
	})

	t.Run("defaults min=1 max=2", func(t *testing.T) {
		na, err := Get("ngram", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "ab", "b", "bc", "c", "cd", "d"}, terms(na.Analyze([]byte("abcd"))))

		ea, err := Get("edge_ngram", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "ab"}, terms(ea.Analyze([]byte("abcd"))))
	})

	t.Run("input shorter than min yields no tokens", func(t *testing.T) {
		na, err := Get("ngram", map[string]any{"min": 2, "max": 3})
		require.NoError(t, err)
		assert.Empty(t, na.Analyze([]byte("a")))

		ea, err := Get("edge_ngram", map[string]any{"min": 2, "max": 3})
		require.NoError(t, err)
		assert.Empty(t, ea.Analyze([]byte("a")))
	})

	t.Run("multibyte gram offsets are byte based", func(t *testing.T) {
		na, err := Get("ngram", map[string]any{"min": 1, "max": 2})
		require.NoError(t, err)
		tokens := na.Analyze([]byte("世界"))
		require.Equal(t, []Token{
			{Term: "世", Start: 0, End: 3, Pos: 0, Type: TypeGram},
			{Term: "世界", Start: 0, End: 6, Pos: 1, Type: TypeGram},
			{Term: "界", Start: 3, End: 6, Pos: 2, Type: TypeGram},
		}, tokens)
	})
}

// TestR11_SimpleAnalyzer checks the simple analyzer: split on non-letters
// (digits are not letters, so "123" disappears) plus lowercase.
func TestR11_SimpleAnalyzer(t *testing.T) {
	a, err := Get("simple", nil)
	require.NoError(t, err)
	tokens := a.Analyze([]byte("Hello, World! 123 four"))
	require.Equal(t, []Token{
		{Term: "hello", Start: 0, End: 5, Pos: 0, Type: TypeAlphaNum},
		{Term: "world", Start: 7, End: 12, Pos: 1, Type: TypeAlphaNum},
		{Term: "four", Start: 18, End: 22, Pos: 2, Type: TypeAlphaNum},
	}, tokens)
	assertTokenStructure(t, "Hello, World! 123 four", tokens)
}

// TestR11_WhitespaceAnalyzer checks the whitespace analyzer: split on
// whitespace only, punctuation kept, NO lowercase (ES semantics).
func TestR11_WhitespaceAnalyzer(t *testing.T) {
	a, err := Get("whitespace", nil)
	require.NoError(t, err)
	text := "Hello 世界 123, foo_bar"
	tokens := a.Analyze([]byte(text))
	require.Equal(t, []Token{
		{Term: "Hello", Start: 0, End: 5, Pos: 0, Type: TypeAlphaNum},
		{Term: "世界", Start: 6, End: 12, Pos: 1, Type: TypeIdeographic},
		{Term: "123,", Start: 13, End: 17, Pos: 2, Type: TypeAlphaNum},
		{Term: "foo_bar", Start: 18, End: 25, Pos: 3, Type: TypeAlphaNum},
	}, tokens)
	assertTokenStructure(t, text, tokens)
}

// TestR11_KeywordAnalyzer checks the keyword analyzer: the whole input is
// a single untouched token.
func TestR11_KeywordAnalyzer(t *testing.T) {
	a, err := Get("keyword", nil)
	require.NoError(t, err)
	text := "Hello 世界 123"
	tokens := a.Analyze([]byte(text))
	require.Equal(t, []Token{
		{Term: "Hello 世界 123", Start: 0, End: 16, Pos: 0, Type: TypeWord},
	}, tokens)
	assertTokenStructure(t, text, tokens)
}

// TestR11_ChineseAnalyzerUTF8Offsets checks that dictionary words like
// "世界" are cut out as single tokens with exact UTF-8 byte offsets.
func TestR11_ChineseAnalyzerUTF8Offsets(t *testing.T) {
	a, err := Get("chinese", nil)
	require.NoError(t, err)
	text := "你好世界" // 你=0-3 好=3-6 世=6-9 界=9-12
	tokens := a.Analyze([]byte(text))
	require.Equal(t, []Token{
		{Term: "你好", Start: 0, End: 6, Pos: 0, Type: TypeIdeographic},
		{Term: "世界", Start: 6, End: 12, Pos: 1, Type: TypeIdeographic},
	}, tokens)
	assertTokenStructure(t, text, tokens)
}

// TestR11_LowercaseStopChain checks the filter composition model:
// lowercase and stop are TokenFilters combinable with any tokenizer, and
// positions stay consecutive after stop-word removal.
func TestR11_LowercaseStopChain(t *testing.T) {
	t.Run("default english stop list", func(t *testing.T) {
		a := NewChain(NewStandardTokenizer(), NewLowercaseFilter(), NewStopFilter())
		tokens := a.Analyze([]byte("The quick brown fox"))
		require.Equal(t, []Token{
			{Term: "quick", Start: 4, End: 9, Pos: 0, Type: TypeAlphaNum},
			{Term: "brown", Start: 10, End: 15, Pos: 1, Type: TypeAlphaNum},
			{Term: "fox", Start: 16, End: 19, Pos: 2, Type: TypeAlphaNum},
		}, tokens)
	})

	t.Run("custom stop words extend the built-in list", func(t *testing.T) {
		a := NewChain(NewWhitespaceTokenizer(), NewLowercaseFilter(), NewStopFilter("quick"))
		tokens := a.Analyze([]byte("The QUICK brown fox"))
		require.Equal(t, []Token{
			{Term: "brown", Start: 10, End: 15, Pos: 0, Type: TypeAlphaNum},
			{Term: "fox", Start: 16, End: 19, Pos: 1, Type: TypeAlphaNum},
		}, tokens)
	})
}

// TestR11_Factory checks Get: all built-in names build working analyzers,
// unknown names and invalid params produce illegal_argument errors.
func TestR11_Factory(t *testing.T) {
	for _, name := range []string{
		"standard", "simple", "whitespace", "keyword", "chinese", "ngram", "edge_ngram",
	} {
		t.Run(name, func(t *testing.T) {
			a, err := Get(name, nil)
			require.NoError(t, err)
			require.NotNil(t, a)
			tokens := a.Analyze([]byte("Hello 世界 123"))
			assert.NotEmpty(t, tokens)
		})
	}

	t.Run("unknown analyzer name", func(t *testing.T) {
		_, err := Get("bogus", nil)
		require.Error(t, err)
		var apiErr *errors.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, errors.TypeIllegalArgument, apiErr.Type)
	})

	t.Run("float params (JSON decoded)", func(t *testing.T) {
		a, err := Get("ngram", map[string]any{"min": 2.0, "max": 3.0})
		require.NoError(t, err)
		assert.Equal(t, []string{"ab", "abc", "bc", "bcd", "cd"}, terms(a.Analyze([]byte("abcd"))))
	})

	t.Run("invalid params", func(t *testing.T) {
		for _, params := range []map[string]any{
			{"min": "2"},         // wrong type
			{"min": 2.5},         // non-integer float
			{"min": 0},           // below 1
			{"min": 3, "max": 2}, // max < min
			{"min": -1},          // below 1 (edge_ngram path below)
		} {
			_, err := Get("ngram", params)
			require.Errorf(t, err, "params %v must be rejected", params)
			var apiErr *errors.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, errors.TypeIllegalArgument, apiErr.Type)
		}
		_, err := Get("edge_ngram", map[string]any{"min": 3, "max": 2})
		require.Error(t, err)
	})
}

// TestR11_EmptyInput checks that every analyzer handles empty input
// without panicking and produces no tokens.
func TestR11_EmptyInput(t *testing.T) {
	for _, name := range []string{
		"standard", "simple", "whitespace", "keyword", "chinese", "ngram", "edge_ngram",
	} {
		a, err := Get(name, nil)
		require.NoError(t, err)
		assert.Emptyf(t, a.Analyze(nil), "analyzer %s on empty input", name)
		assert.Emptyf(t, a.Analyze([]byte{}), "analyzer %s on empty input", name)
	}
}

// terms extracts the term list from tokens.
func terms(tokens []Token) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = t.Term
	}
	return out
}

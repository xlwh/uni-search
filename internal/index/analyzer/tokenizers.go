package analyzer

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/go-ego/gse"
	"github.com/rivo/uniseg"
)

// Token type constants, aligned with the ES _analyze "type" field.
const (
	TypeAlphaNum    = "<ALPHANUM>"    // letters/digits (Lucene StandardTokenizer)
	TypeNum         = "<NUM>"         // digits only
	TypeIdeographic = "<IDEOGRAPHIC>" // Han / kana / hangul runs
	TypeWord        = "<WORD>"        // keyword or chinese whole words
	TypeGram        = "<GRAM>"        // ngram / edge_ngram
)

// standardTokenizer segments text on UAX#29 word boundaries (via uniseg)
// and emits only tokens containing at least one letter or digit, matching
// Lucene StandardTokenizer semantics: whitespace and punctuation are
// consumed but not emitted.
type standardTokenizer struct{}

// NewStandardTokenizer returns the UAX#29 standard tokenizer.
func NewStandardTokenizer() Tokenizer { return standardTokenizer{} }

// Tokenize implements Tokenizer.
func (standardTokenizer) Tokenize(text []byte) []Token {
	if len(text) == 0 {
		return nil
	}
	var tokens []Token
	rest := text
	state := -1
	for len(rest) > 0 {
		var word []byte
		word, rest, state = uniseg.FirstWord(rest, state)
		if len(word) == 0 {
			break
		}
		if !containsLetterOrDigit(word) {
			continue
		}
		start := len(text) - len(rest) - len(word)
		tokens = append(tokens, Token{
			Term:  string(word),
			Start: start,
			End:   start + len(word),
			Pos:   len(tokens),
			Type:  classifyToken(word),
		})
	}
	return tokens
}

// simpleTokenizer splits text at every non-letter rune (Unicode aware),
// mirroring the Lucene LetterTokenizer used by the ES simple analyzer.
type simpleTokenizer struct{}

// NewSimpleTokenizer returns the letter-only tokenizer.
func NewSimpleTokenizer() Tokenizer { return simpleTokenizer{} }

// Tokenize implements Tokenizer.
func (simpleTokenizer) Tokenize(text []byte) []Token {
	return splitOnFunc(text, func(r rune) bool { return !unicode.IsLetter(r) })
}

// whitespaceTokenizer splits text on Unicode whitespace and keeps
// everything else (including punctuation) verbatim, mirroring the ES
// whitespace analyzer which does not lowercase.
type whitespaceTokenizer struct{}

// NewWhitespaceTokenizer returns the whitespace tokenizer.
func NewWhitespaceTokenizer() Tokenizer { return whitespaceTokenizer{} }

// Tokenize implements Tokenizer.
func (whitespaceTokenizer) Tokenize(text []byte) []Token {
	return splitOnFunc(text, func(r rune) bool { return unicode.IsSpace(r) })
}

// splitOnFunc scans text rune by rune and emits a token for every maximal
// run of runes for which keep returns true. Offsets are byte offsets into
// the original text.
func splitOnFunc(text []byte, split func(r rune) bool) []Token {
	var tokens []Token
	start := -1
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRune(text[i:])
		if split(r) {
			if start >= 0 {
				tokens = append(tokens, Token{
					Term:  string(text[start:i]),
					Start: start,
					End:   i,
					Pos:   len(tokens),
					Type:  classifyToken(text[start:i]),
				})
				start = -1
			}
		} else if start < 0 {
			start = i
		}
		i += size
	}
	if start >= 0 {
		tokens = append(tokens, Token{
			Term:  string(text[start:]),
			Start: start,
			End:   len(text),
			Pos:   len(tokens),
			Type:  classifyToken(text[start:]),
		})
	}
	return tokens
}

// keywordTokenizer emits the entire input as a single token.
type keywordTokenizer struct{}

// NewKeywordTokenizer returns the no-op keyword tokenizer.
func NewKeywordTokenizer() Tokenizer { return keywordTokenizer{} }

// Tokenize implements Tokenizer.
func (keywordTokenizer) Tokenize(text []byte) []Token {
	if len(text) == 0 {
		return nil
	}
	return []Token{{
		Term:  string(text),
		Start: 0,
		End:   len(text),
		Pos:   0,
		Type:  TypeWord,
	}}
}

// chineseSegmenter lazily builds the gse segmenter over the default
// embedded dictionary. Loading (and HMM model init) costs several hundred
// milliseconds, so the instance is cached process-wide via sync.OnceValue
// and shared by every chinese analyzer; it is read-only after load and
// therefore safe for concurrent use.
var chineseSegmenter = sync.OnceValue(func() *gse.Segmenter {
	seg, err := gse.NewEmbed()
	if err != nil {
		// The embedded dictionary cannot fail to load; guard anyway and
		// let Analyze degrade to no tokens instead of panicking.
		return nil
	}
	return &seg
})

// chineseTokenizer segments text with gse in HMM mode (jieba-style precise
// mode) over the default embedded dictionary, providing ik/jieba-like
// semantics. Tokens without any letter or digit (whitespace, punctuation)
// are dropped.
type chineseTokenizer struct{}

// NewChineseTokenizer returns the gse-based chinese tokenizer.
func NewChineseTokenizer() Tokenizer { return chineseTokenizer{} }

// Tokenize implements Tokenizer.
func (chineseTokenizer) Tokenize(text []byte) []Token {
	if len(text) == 0 {
		return nil
	}
	seg := chineseSegmenter()
	if seg == nil {
		return nil
	}
	return cutWithOffsets(seg, text)
}

// cutWithOffsets runs gse's HMM-mode Cut and reconstructs byte offsets.
// Cut returns words without positions and lowercases the input internally
// (gse.ToLower defaults to true), so the words concatenate exactly to
// strings.ToLower(text). We walk the words over the rune stream and map
// rune indexes back to byte offsets in the original text. If lowercasing
// changed the rune count (exotic runes such as 'İ') or the walk
// misaligns, we fall back to seg.Segment, which reports offsets directly.
func cutWithOffsets(seg *gse.Segmenter, text []byte) []Token {
	orig := string(text)
	target := orig
	if gse.ToLower {
		target = strings.ToLower(orig)
	}
	origRunes := utf8.RuneCountInString(orig)
	if utf8.RuneCountInString(target) == origRunes {
		offsets := runeByteOffsets(text, origRunes)
		var tokens []Token
		runeIdx := 0
		aligned := true
		for _, w := range seg.Cut(orig, true) {
			n := utf8.RuneCountInString(w)
			if n == 0 || runeIdx+n > origRunes {
				aligned = false
				break
			}
			start, end := offsets[runeIdx], offsets[runeIdx+n]
			runeIdx += n
			if !containsLetterOrDigit(text[start:end]) {
				continue
			}
			tokens = append(tokens, Token{
				Term:  w,
				Start: start,
				End:   end,
				Pos:   len(tokens),
				Type:  classifyToken([]byte(w)),
			})
		}
		if aligned && runeIdx == origRunes {
			return tokens
		}
	}
	return tokensFromSegments(seg.Segment(text))
}

// tokensFromSegments converts gse Segment results (shortest-path mode,
// which carries precise byte offsets) into Tokens, dropping tokens
// without any letter or digit.
func tokensFromSegments(segs []gse.Segment) []Token {
	var tokens []Token
	for _, s := range segs {
		term := s.Token().Text()
		if !containsLetterOrDigit([]byte(term)) {
			continue
		}
		tokens = append(tokens, Token{
			Term:  term,
			Start: s.Start(),
			End:   s.End(),
			Pos:   len(tokens),
			Type:  classifyToken([]byte(term)),
		})
	}
	return tokens
}

// ngramTokenizer emits, for every rune start position, all grams of rune
// length min..max clamped to the end of the text: "abcd" with min=2,
// max=3 yields ab, abc, bc, bcd, cd (boundary truncation, no grams
// shorter than min).
type ngramTokenizer struct{ min, max int }

// NewNGramTokenizer returns an ngram tokenizer for gram lengths
// [min, max]. It errors when min < 1 or max < min.
func NewNGramTokenizer(min, max int) (Tokenizer, error) {
	if min < 1 {
		return nil, newIllegalArgumentf("ngram tokenizer: min gram size must be >= 1, got %d", min)
	}
	if max < min {
		return nil, newIllegalArgumentf("ngram tokenizer: max gram size (%d) must be >= min gram size (%d)", max, min)
	}
	return &ngramTokenizer{min: min, max: max}, nil
}

// Tokenize implements Tokenizer.
func (t *ngramTokenizer) Tokenize(text []byte) []Token {
	runes := utf8.RuneCount(text)
	if runes < t.min {
		return nil
	}
	offsets := runeByteOffsets(text, runes)
	var tokens []Token
	for start := 0; start < runes; start++ {
		for n := t.min; n <= t.max && start+n <= runes; n++ {
			s, e := offsets[start], offsets[start+n]
			tokens = append(tokens, Token{
				Term:  string(text[s:e]),
				Start: s,
				End:   e,
				Pos:   len(tokens),
				Type:  TypeGram,
			})
		}
	}
	return tokens
}

// edgeNgramTokenizer emits prefix grams only: grams of length min..max
// anchored at the start of the text and clamped to its length ("abcd"
// with min=1, max=2 yields a, ab).
type edgeNgramTokenizer struct{ min, max int }

// NewEdgeNGramTokenizer returns an edge ngram tokenizer for gram lengths
// [min, max]. It errors when min < 1 or max < min.
func NewEdgeNGramTokenizer(min, max int) (Tokenizer, error) {
	if min < 1 {
		return nil, newIllegalArgumentf("edge_ngram tokenizer: min gram size must be >= 1, got %d", min)
	}
	if max < min {
		return nil, newIllegalArgumentf("edge_ngram tokenizer: max gram size (%d) must be >= min gram size (%d)", max, min)
	}
	return &edgeNgramTokenizer{min: min, max: max}, nil
}

// Tokenize implements Tokenizer.
func (t *edgeNgramTokenizer) Tokenize(text []byte) []Token {
	runes := utf8.RuneCount(text)
	if runes < t.min {
		return nil
	}
	offsets := runeByteOffsets(text, runes)
	limit := min(t.max, runes)
	var tokens []Token
	for n := t.min; n <= limit; n++ {
		tokens = append(tokens, Token{
			Term:  string(text[:offsets[n]]),
			Start: 0,
			End:   offsets[n],
			Pos:   len(tokens),
			Type:  TypeGram,
		})
	}
	return tokens
}

// runeByteOffsets returns a slice of runeCount+1 byte offsets where
// offsets[i] is the byte offset of the i-th rune of text (and
// offsets[runeCount] == len(text) for valid UTF-8).
func runeByteOffsets(text []byte, runeCount int) []int {
	offsets := make([]int, runeCount+1)
	i, off := 0, 0
	for off < len(text) {
		_, size := utf8.DecodeRune(text[off:])
		off += size
		i++
		offsets[i] = off
	}
	return offsets
}

// containsLetterOrDigit reports whether text holds at least one letter or
// digit (used to drop whitespace/punctuation tokens).
func containsLetterOrDigit(text []byte) bool {
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRune(text[i:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
		i += size
	}
	return false
}

// classifyToken assigns an ES-like token type: digits-only tokens are
// "<NUM>", tokens containing Han/kana/hangul are "<IDEOGRAPHIC>", and
// everything else is "<ALPHANUM>".
func classifyToken(word []byte) string {
	allDigits := len(word) > 0
	ideographic := false
	for i := 0; i < len(word); {
		r, size := utf8.DecodeRune(word[i:])
		if r < '0' || r > '9' {
			allDigits = false
		}
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			ideographic = true
		}
		i += size
	}
	switch {
	case allDigits:
		return TypeNum
	case ideographic:
		return TypeIdeographic
	default:
		return TypeAlphaNum
	}
}

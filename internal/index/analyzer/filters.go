package analyzer

import "strings"

// lowercaseFilter lowercases every token term (Unicode aware).
type lowercaseFilter struct{}

// NewLowercaseFilter returns the lowercase token filter.
func NewLowercaseFilter() TokenFilter { return lowercaseFilter{} }

// Filter implements TokenFilter.
func (lowercaseFilter) Filter(tokens []Token) []Token {
	for i := range tokens {
		tokens[i].Term = strings.ToLower(tokens[i].Term)
	}
	return tokens
}

// asciiFoldingFilter maps common accented Latin letters to their ASCII
// equivalents (à→a, É→E, æ→ae, ß→ss, ...). It is a simplified variant of
// the ES asciifolding filter, covering Latin-1 Supplement and the common
// Latin Extended-A letters.
type asciiFoldingFilter struct{}

// NewASCIIFoldingFilter returns the ascii-folding token filter.
func NewASCIIFoldingFilter() TokenFilter { return asciiFoldingFilter{} }

// Filter implements TokenFilter.
func (asciiFoldingFilter) Filter(tokens []Token) []Token {
	for i := range tokens {
		tokens[i].Term = foldToASCII(tokens[i].Term)
	}
	return tokens
}

// foldToASCII replaces foldable runes with their ASCII equivalents and
// returns the original string untouched when no fold is needed.
func foldToASCII(s string) string {
	foldable := false
	for _, r := range s {
		if _, ok := asciiFoldTable[r]; ok {
			foldable = true
			break
		}
	}
	if !foldable {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if repl, ok := asciiFoldTable[r]; ok {
			b.WriteString(repl)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// asciiFoldTable maps accented Latin letters to ASCII replacements.
var asciiFoldTable = func() map[rune]string {
	m := make(map[rune]string, 256)
	add := func(repl string, rs ...rune) {
		for _, r := range rs {
			m[r] = repl
		}
	}
	// Latin-1 Supplement.
	add("A", 'À', 'Á', 'Â', 'Ã', 'Ä', 'Å')
	add("a", 'à', 'á', 'â', 'ã', 'ä', 'å')
	add("AE", 'Æ')
	add("ae", 'æ')
	add("C", 'Ç')
	add("c", 'ç')
	add("E", 'È', 'É', 'Ê', 'Ë')
	add("e", 'è', 'é', 'ê', 'ë')
	add("I", 'Ì', 'Í', 'Î', 'Ï')
	add("i", 'ì', 'í', 'î', 'ï')
	add("N", 'Ñ')
	add("n", 'ñ')
	add("O", 'Ò', 'Ó', 'Ô', 'Õ', 'Ö', 'Ø')
	add("o", 'ò', 'ó', 'ô', 'õ', 'ö', 'ø')
	add("ss", 'ß')
	add("U", 'Ù', 'Ú', 'Û', 'Ü')
	add("u", 'ù', 'ú', 'û', 'ü')
	add("Y", 'Ý', 'Ÿ')
	add("y", 'ý', 'ÿ')
	// Latin Extended-A (common letters with diacritics).
	add("A", 'Ā', 'Ă', 'Ą')
	add("a", 'ā', 'ă', 'ą')
	add("C", 'Ć', 'Ĉ', 'Ċ', 'Č')
	add("c", 'ć', 'ĉ', 'ċ', 'č')
	add("D", 'Ď')
	add("d", 'ď')
	add("E", 'Ē', 'Ĕ', 'Ė', 'Ę', 'Ě')
	add("e", 'ē', 'ĕ', 'ė', 'ę', 'ě')
	add("G", 'Ĝ', 'Ğ', 'Ġ', 'Ģ')
	add("g", 'ĝ', 'ğ', 'ġ', 'ģ')
	add("H", 'Ĥ', 'Ħ')
	add("h", 'ĥ', 'ħ')
	add("I", 'Ĩ', 'Ī', 'Ĭ', 'Į', 'İ')
	add("i", 'ĩ', 'ī', 'ĭ', 'į', 'ı')
	add("J", 'Ĵ')
	add("j", 'ĵ')
	add("K", 'Ķ')
	add("k", 'ķ')
	add("L", 'Ĺ', 'Ļ', 'Ľ', 'Ŀ')
	add("l", 'ĺ', 'ļ', 'ľ', 'ŀ')
	add("N", 'Ń', 'Ņ', 'Ň')
	add("n", 'ń', 'ņ', 'ň')
	add("O", 'Ō', 'Ŏ', 'Ő')
	add("o", 'ō', 'ŏ', 'ő')
	add("OE", 'Œ')
	add("oe", 'œ')
	add("R", 'Ŕ', 'Ŗ', 'Ř')
	add("r", 'ŕ', 'ŗ', 'ř')
	add("S", 'Ś', 'Ŝ', 'Š')
	add("s", 'ś', 'ŝ', 'š')
	add("T", 'Ţ', 'Ŧ', 'Ť')
	add("t", 'ţ', 'ŧ', 'ť')
	add("U", 'Ū', 'Ŭ', 'Ů', 'Ű', 'Ų')
	add("u", 'ū', 'ŭ', 'ů', 'ű', 'ų')
	add("W", 'Ŵ')
	add("w", 'ŵ')
	add("Y", 'Ŷ')
	add("y", 'ŷ')
	add("Z", 'Ź', 'Ż', 'Ž')
	add("z", 'ź', 'ż', 'ž')
	return m
}()

// englishStopWords is the built-in English stop word list (the Lucene/ES
// English default set).
var englishStopWords = []string{
	"a", "an", "and", "are", "as", "at", "be", "but", "by",
	"for", "if", "in", "into", "is", "it", "no", "not", "of",
	"on", "or", "such", "that", "the", "their", "then", "there",
	"these", "they", "this", "to", "was", "will", "with",
}

// stopFilter drops tokens whose term appears in the stop word set.
type stopFilter struct {
	stop map[string]struct{}
}

// NewStopFilter returns a stop-word filter seeded with the built-in
// English stop list plus any extra custom words (all compared
// case-insensitively against lowercased terms, so place it after
// NewLowercaseFilter in a chain).
func NewStopFilter(extra ...string) TokenFilter {
	stop := make(map[string]struct{}, len(englishStopWords)+len(extra))
	for _, w := range englishStopWords {
		stop[strings.ToLower(w)] = struct{}{}
	}
	for _, w := range extra {
		stop[strings.ToLower(w)] = struct{}{}
	}
	return &stopFilter{stop: stop}
}

// Filter implements TokenFilter.
func (f *stopFilter) Filter(tokens []Token) []Token {
	out := tokens[:0]
	for _, t := range tokens {
		if _, stopped := f.stop[t.Term]; stopped {
			continue
		}
		out = append(out, t)
	}
	return out
}

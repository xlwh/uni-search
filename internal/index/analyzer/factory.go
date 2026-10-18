package analyzer

import (
	"fmt"

	"github.com/unisearch-db/unisearch/internal/errors"
)

// Built-in analyzer names accepted by Get.
const (
	NameStandard   = "standard"
	NameSimple     = "simple"
	NameWhitespace = "whitespace"
	NameKeyword    = "keyword"
	NameChinese    = "chinese"
	NameNGram      = "ngram"
	NameEdgeNGram  = "edge_ngram"
)

// Default ngram/edge_ngram gram sizes.
const (
	defaultMinGram = 1
	defaultMaxGram = 2
)

// Get builds an analyzer by name (design 02-index-engine §4):
//
//	standard    uniseg UAX#29 word segmentation + ascii-folding + lowercase
//	simple      letter-run splitting + lowercase
//	whitespace  whitespace splitting (no lowercase, ES semantics)
//	keyword     whole input as one token
//	chinese     gse HMM segmentation over the default dictionary + lowercase
//	ngram       sliding-window grams, params "min"/"max" (default 1/2)
//	edge_ngram  prefix grams, params "min"/"max" (default 1/2)
//
// Unknown names and invalid params return an illegal_argument APIError so
// the REST layer can surface them verbatim.
func Get(name string, params map[string]any) (Analyzer, error) {
	switch name {
	case NameStandard:
		return NewChain(NewStandardTokenizer(), NewASCIIFoldingFilter(), NewLowercaseFilter()), nil
	case NameSimple:
		return NewChain(NewSimpleTokenizer(), NewLowercaseFilter()), nil
	case NameWhitespace:
		return NewChain(NewWhitespaceTokenizer()), nil
	case NameKeyword:
		return NewChain(NewKeywordTokenizer()), nil
	case NameChinese:
		return NewChain(NewChineseTokenizer(), NewLowercaseFilter()), nil
	case NameNGram, NameEdgeNGram:
		min, max, err := gramParams(params)
		if err != nil {
			return nil, err
		}
		var tok Tokenizer
		if name == NameNGram {
			tok, err = NewNGramTokenizer(min, max)
		} else {
			tok, err = NewEdgeNGramTokenizer(min, max)
		}
		if err != nil {
			return nil, err
		}
		return NewChain(tok), nil
	default:
		return nil, newIllegalArgumentf("unknown analyzer [%s]", name)
	}
}

// gramParams reads the integer params "min" and "max" for the ngram
// family, defaulting to 1/2. Values may arrive as int, int64 or float64
// (JSON-decoded numbers).
func gramParams(params map[string]any) (min, max int, err error) {
	min, max = defaultMinGram, defaultMaxGram
	if params == nil {
		return min, max, nil
	}
	if v, ok := params["min"]; ok {
		if min, err = paramInt(v, "min"); err != nil {
			return 0, 0, err
		}
	}
	if v, ok := params["max"]; ok {
		if max, err = paramInt(v, "max"); err != nil {
			return 0, 0, err
		}
	}
	return min, max, nil
}

// paramInt converts a loosely typed param value to an int.
func paramInt(v any, name string) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		i := int(n)
		if float64(i) != n {
			return 0, newIllegalArgumentf("analyzer param [%s] must be an integer, got %v", name, n)
		}
		return i, nil
	default:
		return 0, newIllegalArgumentf("analyzer param [%s] must be an integer, got %T", name, v)
	}
}

// newIllegalArgumentf builds an illegal_argument APIError with a
// formatted reason.
func newIllegalArgumentf(format string, args ...any) error {
	return errors.NewIllegalArgument(fmt.Sprintf(format, args...))
}

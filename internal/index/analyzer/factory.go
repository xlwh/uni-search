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
	NameMinHash    = "minhash"
	NameCollation  = "collation"
	NamePipeline   = "pipeline"
	NameFastText   = "fasttext"
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
	case NameMinHash:
		numPerm, err := paramIntOpt(params, "num_perm", 128)
		if err != nil {
			return nil, err
		}
		seedVal, err := paramIntOpt(params, "seed", 42)
		if err != nil {
			return nil, err
		}
		mf, err := NewMinHashFilter(numPerm, int64(seedVal))
		if err != nil {
			return nil, newIllegalArgumentf("minhash: %v", err)
		}
		return NewChain(NewKeywordTokenizer(), mf), nil
	case NameCollation:
		locale := paramStrOpt(params, "locale", "en")
		strength := paramStrOpt(params, "strength", "primary")
		if err := validateStrength(strength); err != nil {
			return nil, newIllegalArgumentf("%v", err)
		}
		cf, err := NewCollationFilter(locale, strength)
		if err != nil {
			return nil, newIllegalArgumentf("collation: %v", err)
		}
		return NewChain(NewKeywordTokenizer(), cf), nil
	case NamePipeline:
		analyzerNames, err := paramStrList(params, "analyzers")
		if err != nil {
			return nil, newIllegalArgumentf("pipeline: %v", err)
		}
		if len(analyzerNames) == 0 {
			return nil, newIllegalArgumentf("pipeline: at least one analyzer required")
		}
		stages := make([]Analyzer, 0, len(analyzerNames))
		for _, an := range analyzerNames {
			if an == NamePipeline {
				return nil, newIllegalArgumentf("pipeline: nested pipeline not supported")
			}
			sub, err := Get(an, nil)
			if err != nil {
				return nil, newIllegalArgumentf("pipeline: analyzer %q: %v", an, err)
			}
			stages = append(stages, sub)
		}
		pa, err := NewPipelineAnalyzer(stages...)
		if err != nil {
			return nil, newIllegalArgumentf("pipeline: %v", err)
		}
		return pa, nil
	case NameFastText:
		modelPath := paramStrOpt(params, "model_path", "")
		modeStr := paramStrOpt(params, "mode", "classification")
		dimVal, _ := paramIntOpt(params, "dim", 5)
		var mode FastTextMode
		switch modeStr {
		case "nearest":
			mode = FastTextNearest
		default:
			mode = FastTextClassification
		}
		fta, _ := NewFastTextAnalyzer(modelPath, mode, dimVal)
		return fta, nil
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

// paramIntOpt reads an optional integer param with a default value.
func paramIntOpt(params map[string]any, name string, def int) (int, error) {
	if params == nil {
		return def, nil
	}
	v, ok := params[name]
	if !ok {
		return def, nil
	}
	return paramInt(v, name)
}

// paramStrOpt reads an optional string param with a default value.
func paramStrOpt(params map[string]any, name string, def string) string {
	if params == nil {
		return def
	}
	v, ok := params[name]
	if !ok {
		return def
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return def
	}
}

// paramStrList reads a required string list param.
func paramStrList(params map[string]any, name string) ([]string, error) {
	if params == nil {
		return nil, fmt.Errorf("missing param [%s]", name)
	}
	v, ok := params[name]
	if !ok {
		return nil, fmt.Errorf("missing param [%s]", name)
	}
	switch val := v.(type) {
	case []any:
		result := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("param [%s] must be a string list, got %T", name, item)
			}
			result = append(result, s)
		}
		return result, nil
	case []string:
		return val, nil
	default:
		return nil, fmt.Errorf("param [%s] must be a string list, got %T", name, v)
	}
}

// newIllegalArgumentf builds an illegal_argument APIError with a
// formatted reason.
func newIllegalArgumentf(format string, args ...any) error {
	return errors.NewIllegalArgument(fmt.Sprintf(format, args...))
}

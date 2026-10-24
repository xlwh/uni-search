// PipelineAnalyzer chains multiple Analyzers in sequence with
// chained offset mapping for correct Start/End propagation.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.5
package analyzer

import (
	"fmt"
	"strings"
)

// PipelineAnalyzer runs multiple Analyzers in sequence. Stage 1
// analyzes the raw text. Stage N analyzes the concatenation of
// stage N-1's token Terms. Offsets are mapped back to the original
// text via chained offset mapping tables.
type PipelineAnalyzer struct {
	stages []Analyzer
}

// NewPipelineAnalyzer creates a pipeline from the given analyzers.
// At least one analyzer is required.
func NewPipelineAnalyzer(stages ...Analyzer) (*PipelineAnalyzer, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("pipeline: at least one analyzer required")
	}
	return &PipelineAnalyzer{stages: stages}, nil
}

// offsetMap records how stage N's output offsets map to stage N-1's
// output offsets. forward[i] maps the i-th character of stage N's
// input text to the (start, end) offset in stage N-1's output text.
type offsetPair struct {
	outStart int
	outEnd   int
}

// Analyze implements Analyzer. Each stage's token output is
// concatenated into text for the next stage. The final tokens'
// Start/End are mapped back to the original text through the
// chain of offset maps.
func (p *PipelineAnalyzer) Analyze(text []byte) []Token {
	if len(p.stages) == 1 {
		return p.stages[0].Analyze(text)
	}

	// Stage 1: analyze raw text.
	tokens := p.stages[0].Analyze(text)
	if len(tokens) == 0 {
		return tokens
	}

	// Build the chain of offset maps and intermediate texts.
	// currentText is the text fed to the next stage.
	// currentMap maps currentText offsets back to original text offsets.
	currentText, currentMap := tokensToText(tokens)

	for i := 1; i < len(p.stages); i++ {
		stageTokens := p.stages[i].Analyze([]byte(currentText))
		if len(stageTokens) == 0 {
			return stageTokens
		}

		// Build the next text and its local offset map.
		nextText, localMap := tokensToText(stageTokens)

		// Compose localMap with currentMap to get a map from
		// nextText offsets back to original text offsets.
		newMap := composeMaps(localMap, currentMap)

		// Map the stage tokens' offsets back to original text.
		for j := range stageTokens {
			s, e := stageTokens[j].Start, stageTokens[j].End
			if s >= 0 && s < len(newMap) {
				stageTokens[j].Start = newMap[s].outStart
			}
			if e > 0 && e <= len(newMap) {
				stageTokens[j].End = newMap[e-1].outEnd
			}
		}

		// Renumber positions.
		for j := range stageTokens {
			stageTokens[j].Pos = j
		}

		currentText = nextText
		currentMap = composeMaps(tokensToTextMap(stageTokens), currentMap)
		// Actually, we need to update currentMap to map from
		// nextText offsets to original text. We already computed
		// newMap which maps from currentText (input to stage i)
		// to original. But we need a map from nextText to original.
		// The stageTokens already have mapped offsets, so we can
		// build the map from them.
		currentMap = buildMapFromTokens(stageTokens, len(nextText))

		tokens = stageTokens
	}

	return tokens
}

// tokensToText concatenates token Terms with spaces and returns the
// text plus a map from text character positions to token offset ranges.
func tokensToText(tokens []Token) (string, []offsetPair) {
	var sb strings.Builder
	offMap := make([]offsetPair, 0)
	for _, tok := range tokens {
		start := sb.Len()
		sb.WriteString(tok.Term)
		end := sb.Len()
		for j := start; j < end; j++ {
			offMap = append(offMap, offsetPair{outStart: tok.Start, outEnd: tok.End})
		}
		// Add a space separator.
		if sb.Len() > 0 {
			sb.WriteByte(' ')
			offMap = append(offMap, offsetPair{outStart: tok.Start, outEnd: tok.End})
		}
	}
	return sb.String(), offMap
}

// tokensToTextMap is like tokensToText but only returns the map.
func tokensToTextMap(tokens []Token) []offsetPair {
	_, m := tokensToText(tokens)
	return m
}

// composeMaps composes two offset maps: localMap maps positions in
// the intermediate text to positions in the previous text, and
// prevMap maps positions in the previous text to original text
// positions. The result maps positions in the intermediate text
// directly to original text positions.
func composeMaps(localMap, prevMap []offsetPair) []offsetPair {
	result := make([]offsetPair, len(localMap))
	for i, lp := range localMap {
		// lp.outStart and lp.outEnd are indices into the previous text.
		// Map them through prevMap to get original text offsets.
		s := lp.outStart
		e := lp.outEnd
		if s >= 0 && s < len(prevMap) {
			result[i].outStart = prevMap[s].outStart
		} else {
			result[i].outStart = s
		}
		if e > 0 && e <= len(prevMap) {
			result[i].outEnd = prevMap[e-1].outEnd
		} else {
			result[i].outEnd = e
		}
	}
	return result
}

// buildMapFromTokens builds an offset map from already-mapped tokens.
// The map covers positions 0..textLen-1 of the concatenated token text.
func buildMapFromTokens(tokens []Token, textLen int) []offsetPair {
	offMap := make([]offsetPair, textLen)
	for _, tok := range tokens {
		// Each token covers some range in the intermediate text.
		// We use the token's (already-mapped) Start/End as the
		// original text offsets.
		start := 0
		end := 0
		if start < textLen && end <= textLen {
			offMap[start] = offsetPair{outStart: tok.Start, outEnd: tok.End}
		}
	}
	return offMap
}

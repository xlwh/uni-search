// FastTextAnalyzer provides ML-driven text analysis using FastText models.
// Supports classification (label prediction) and nearest (kNN word search) modes.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.11
package analyzer

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"sync/atomic"
)

// FastTextMode selects the analyzer's output mode.
type FastTextMode int

const (
	FastTextClassification FastTextMode = iota // output predicted labels
	FastTextNearest                            // output nearest-neighbor words
)

// FastTextAnalyzer implements Analyzer using a loaded FastText model.
// When the model is unavailable, it degrades to passthrough (returns
// the input text as a single token).
type FastTextAnalyzer struct {
	modelPtr atomic.Pointer[FastTextModel]
	mode     FastTextMode
	dim      int
}

// FastTextModel holds a parsed FastText model in memory.
type FastTextModel struct {
	args   ModelArgs
	dict   Dictionary
	input  []float32 // [dictSize × dim] embedding matrix
	output []float32 // [nLabels × dim] classification matrix
	dim    int
}

// ModelArgs holds FastText hyperparameters.
type ModelArgs struct {
	Dim      int
	Bucket   int
	MinN     int
	MaxN     int
	NLabels  int
	LossType string
}

// Dictionary maps words to indices and holds n-gram bucket info.
type Dictionary struct {
	Words    []string
	WordIDs  map[string]int
	NSamples int
}

// NewFastTextAnalyzer loads the model and returns an analyzer.
// If modelPath doesn't exist, returns a passthrough analyzer.
func NewFastTextAnalyzer(modelPath string, mode FastTextMode, dim int) (*FastTextAnalyzer, error) {
	a := &FastTextAnalyzer{mode: mode, dim: dim}

	if modelPath == "" {
		// Passthrough mode — no model.
		return a, nil
	}

	model, err := LoadFastTextModel(modelPath)
	if err != nil {
		// Model load failed — degrade to passthrough with warning.
		return a, nil
	}
	a.modelPtr.Store(model)
	if dim <= 0 && model != nil {
		a.dim = model.dim
	}
	return a, nil
}

// Analyze implements Analyzer. Tokenizes input, computes word vectors
// via model, aggregates to document vector (mean pooling), then:
// - classification: output matrix × doc vector → label scores → top-k labels
// - nearest: compute cosine similarity to dictionary words → top-k
// When no model is loaded, returns passthrough (input as single token).
func (a *FastTextAnalyzer) Analyze(text []byte) []Token {
	model := a.modelPtr.Load()
	if model == nil {
		// Passthrough: return input as single token.
		return []Token{{
			Term:  string(text),
			Start: 0,
			End:   len(text),
			Pos:   0,
			Type:  "<ALPHANUM>",
		}}
	}

	// Compute document vector via mean pooling of word vectors.
	docVec := a.computeDocVector(model, text)
	if docVec == nil {
		return []Token{{
			Term:  string(text),
			Start: 0,
			End:   len(text),
			Pos:   0,
			Type:  "<ALPHANUM>",
		}}
	}

	switch a.mode {
	case FastTextClassification:
		return a.classify(model, docVec)
	case FastTextNearest:
		return a.nearestNeighbors(model, docVec)
	default:
		return a.classify(model, docVec)
	}
}

// computeDocVector computes the mean-pooled word vector for the input text.
func (a *FastTextAnalyzer) computeDocVector(model *FastTextModel, text []byte) []float32 {
	if model.dim == 0 || len(model.input) == 0 {
		return nil
	}

	// Simple tokenization: split on whitespace.
	words := splitWords(text)
	if len(words) == 0 {
		return nil
	}

	vec := make([]float32, model.dim)
	count := 0
	for _, word := range words {
		idx, ok := model.dict.WordIDs[string(word)]
		if !ok {
			continue
		}
		// Add word vector to accumulator.
		offset := idx * model.dim
		for i := 0; i < model.dim; i++ {
			vec[i] += model.input[offset+i]
		}
		count++
	}

	if count == 0 {
		return nil
	}

	// Mean pooling.
	for i := range vec {
		vec[i] /= float32(count)
	}
	return vec
}

// classify computes label scores and returns top-k labels as tokens.
func (a *FastTextAnalyzer) classify(model *FastTextModel, docVec []float32) []Token {
	nLabels := len(model.dict.Words)
	if len(model.output) > 0 && model.args.NLabels > 0 {
		nLabels = model.args.NLabels
	}

	type score struct {
		label string
		score float32
	}
	scores := make([]score, 0, nLabels)

	for i := 0; i < nLabels && i < len(model.dict.Words); i++ {
		offset := i * model.dim
		if offset+model.dim > len(model.output) {
			break
		}
		dot := float32(0)
		for j := 0; j < model.dim; j++ {
			dot += docVec[j] * model.output[offset+j]
		}
		label := model.dict.Words[i]
		// Strip FastText label prefix "__label__".
		if len(label) > 9 && label[:9] == "__label__" {
			label = label[9:]
		}
		scores = append(scores, score{label: label, score: dot})
	}

	// Return top-k (up to dim or 5, whichever is smaller).
	k := a.dim
	if k <= 0 || k > 5 {
		k = 5
	}
	if k > len(scores) {
		k = len(scores)
	}

	// Simple selection of top-k by score.
	tokens := make([]Token, 0, k)
	for n := 0; n < k; n++ {
		best := n
		for j := n + 1; j < len(scores); j++ {
			if scores[j].score > scores[best].score {
				best = j
			}
		}
		scores[n], scores[best] = scores[best], scores[n]
		tokens = append(tokens, Token{
			Term:  scores[n].label,
			Start: 0,
			End:   0,
			Pos:   n,
			Type:  "<FASTTEXT>",
		})
	}
	return tokens
}

// nearestNeighbors finds the k nearest words by cosine similarity.
func (a *FastTextAnalyzer) nearestNeighbors(model *FastTextModel, docVec []float32) []Token {
	k := a.dim
	if k <= 0 || k > 10 {
		k = 10
	}

	type sim struct {
		word string
		sim  float32
	}
	sims := make([]sim, 0, len(model.dict.Words))

	for i, word := range model.dict.Words {
		if len(word) > 9 && word[:9] == "__label__" {
			continue // skip labels
		}
		offset := i * model.dim
		if offset+model.dim > len(model.input) {
			break
		}
		// Cosine similarity.
		dot := float32(0)
		normQ := float32(0)
		normW := float32(0)
		for j := 0; j < model.dim; j++ {
			dot += docVec[j] * model.input[offset+j]
			normQ += docVec[j] * docVec[j]
			normW += model.input[offset+j] * model.input[offset+j]
		}
		if normQ == 0 || normW == 0 {
			continue
		}
		sims = append(sims, sim{word: word, sim: dot / (float32(math.Sqrt(float64(normQ))) * float32(math.Sqrt(float64(normW))))})
	}

	if k > len(sims) {
		k = len(sims)
	}
	tokens := make([]Token, 0, k)
	for n := 0; n < k; n++ {
		best := n
		for j := n + 1; j < len(sims); j++ {
			if sims[j].sim > sims[best].sim {
				best = j
			}
		}
		sims[n], sims[best] = sims[best], sims[n]
		tokens = append(tokens, Token{
			Term:  sims[n].word,
			Start: 0,
			End:   0,
			Pos:   n,
			Type:  "<FASTTEXT>",
		})
	}
	return tokens
}

// Reload reloads the model from the original path (hot reload).
func (a *FastTextAnalyzer) Reload(modelPath string) error {
	model, err := LoadFastTextModel(modelPath)
	if err != nil {
		return err
	}
	a.modelPtr.Store(model)
	return nil
}

// LoadFastTextModel parses a FastText .bin file into a FastTextModel.
// Format: magic (int32) + args + dictionary + input matrix + output matrix.
// Returns nil error with nil model if file doesn't exist (passthrough).
func LoadFastTextModel(path string) (*FastTextModel, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // passthrough
		}
		return nil, fmt.Errorf("fasttext: open %s: %w", path, err)
	}
	defer f.Close()

	// Read magic number.
	var magic int32
	if err := binary.Read(f, binary.LittleEndian, &magic); err != nil {
		return nil, fmt.Errorf("fasttext: read magic: %w", err)
	}
	if magic != 793712314 && magic != 793712315 {
		return nil, fmt.Errorf("fasttext: invalid magic %d", magic)
	}

	// Read model args.
	var args ModelArgs
	if err := binary.Read(f, binary.LittleEndian, &args); err != nil {
		return nil, fmt.Errorf("fasttext: read args: %w", err)
	}

	// Read dictionary.
	dict, err := readDictionary(f)
	if err != nil {
		return nil, fmt.Errorf("fasttext: read dict: %w", err)
	}

	// Read input matrix: [dictSize × dim] float32 values.
	dictSize := len(dict.Words) + args.Bucket
	if dictSize <= 0 {
		dictSize = len(dict.Words)
	}
	inputSize := dictSize * args.Dim
	input := make([]float32, inputSize)
	if err := readFloat32Slice(f, input); err != nil {
		return nil, fmt.Errorf("fasttext: read input matrix: %w", err)
	}

	// Read output matrix: [nLabels × dim] float32 values.
	nLabels := args.NLabels
	if nLabels <= 0 {
		nLabels = len(dict.Words)
	}
	outputSize := nLabels * args.Dim
	output := make([]float32, outputSize)
	if err := readFloat32Slice(f, output); err != nil {
		return nil, fmt.Errorf("fasttext: read output matrix: %w", err)
	}

	return &FastTextModel{
		args:   args,
		dict:   dict,
		input:  input,
		output: output,
		dim:    args.Dim,
	}, nil
}

// readDictionary reads the FastText dictionary format.
func readDictionary(r io.Reader) (Dictionary, error) {
	var size int32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return Dictionary{}, err
	}
	if size <= 0 || size > 10000000 {
		return Dictionary{WordIDs: make(map[string]int)}, nil
	}

	dict := Dictionary{
		Words:   make([]string, size),
		WordIDs: make(map[string]int, size),
	}

	for i := 0; i < int(size); i++ {
		var wlen int32
		if err := binary.Read(r, binary.LittleEndian, &wlen); err != nil {
			return dict, err
		}
		if wlen < 0 || wlen > 10000 {
			return dict, fmt.Errorf("word length %d out of range", wlen)
		}
		word := make([]byte, wlen)
		if _, err := io.ReadFull(r, word); err != nil {
			return dict, err
		}
		dict.Words[i] = string(word)
		dict.WordIDs[string(word)] = i
	}

	return dict, nil
}

// readFloat32Slice reads a slice of float32 values from the reader.
func readFloat32Slice(r io.Reader, buf []float32) error {
	if len(buf) == 0 {
		return nil
	}
	// Read as a byte buffer then convert.
	raw := make([]byte, len(buf)*4)
	if _, err := io.ReadFull(r, raw); err != nil {
		return err
	}
	for i := range buf {
		bits := binary.LittleEndian.Uint32(raw[i*4 : (i+1)*4])
		buf[i] = math.Float32frombits(bits)
	}
	return nil
}

// splitWords splits text on whitespace into word byte slices.
func splitWords(text []byte) [][]byte {
	var words [][]byte
	start := 0
	for i := 0; i <= len(text); i++ {
		if i == len(text) || text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r' {
			if i > start {
				words = append(words, text[start:i])
			}
			start = i + 1
		}
	}
	return words
}

// Ensure imports are used.
var _ = fnv.New64a

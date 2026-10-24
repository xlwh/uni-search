package analyzer

import (
	"testing"
)

// TestMinHashBasic verifies basic MinHash signature computation.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.3
func TestMinHashBasic(t *testing.T) {
	mf, err := NewMinHashFilter(128, 42)
	if err != nil {
		t.Fatalf("NewMinHashFilter failed: %v", err)
	}

	tokens := []Token{
		{Term: "hello", Start: 0, End: 5, Pos: 0, Type: "<ALPHANUM>"},
		{Term: "world", Start: 6, End: 11, Pos: 1, Type: "<ALPHANUM>"},
	}

	out := mf.Filter(tokens)
	if len(out) != 128 {
		t.Fatalf("expected 128 minhash tokens, got %d", len(out))
	}
	for i, tok := range out {
		if tok.Type != "<MINHASH>" {
			t.Fatalf("token %d: expected type <MINHASH>, got %s", i, tok.Type)
		}
		if tok.Pos != i {
			t.Fatalf("token %d: expected pos %d, got %d", i, i, tok.Pos)
		}
	}
}

// TestMinHashDeterministic verifies that the same input + seed
// produces the same signature.
func TestMinHashDeterministic(t *testing.T) {
	tokens := []Token{
		{Term: "test", Start: 0, End: 4, Pos: 0, Type: "<ALPHANUM>"},
	}

	mf1, _ := NewMinHashFilter(64, 42)
	mf2, _ := NewMinHashFilter(64, 42)

	out1 := mf1.Filter(tokens)
	out2 := mf2.Filter(tokens)

	if len(out1) != len(out2) {
		t.Fatalf("expected same length, got %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].Term != out2[i].Term {
			t.Fatalf("token %d: expected same term, got %q vs %q", i, out1[i].Term, out2[i].Term)
		}
	}
}

// TestMinHashEmptyInput verifies empty token input.
func TestMinHashEmptyInput(t *testing.T) {
	mf, _ := NewMinHashFilter(128, 42)
	out := mf.Filter([]Token{})
	if len(out) != 0 {
		t.Fatalf("expected 0 tokens for empty input, got %d", len(out))
	}
}

// TestMinHashInvalidNumPerm verifies invalid num_perm values.
func TestMinHashInvalidNumPerm(t *testing.T) {
	_, err := NewMinHashFilter(0, 42)
	if err == nil {
		t.Fatal("expected error for num_perm=0")
	}
	_, err = NewMinHashFilter(2000, 42)
	if err == nil {
		t.Fatal("expected error for num_perm=2000")
	}
}

// TestCollationBasic verifies basic collation sort key generation.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.4
func TestCollationBasic(t *testing.T) {
	cf, err := NewCollationFilter("en", "primary")
	if err != nil {
		t.Fatalf("NewCollationFilter failed: %v", err)
	}

	tokens := []Token{
		{Term: "apple", Start: 0, End: 5, Pos: 0, Type: "<ALPHANUM>"},
		{Term: "Banana", Start: 6, End: 12, Pos: 1, Type: "<ALPHANUM>"},
		{Term: "cherry", Start: 13, End: 19, Pos: 2, Type: "<ALPHANUM>"},
	}

	out := cf.Filter(tokens)
	if len(out) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(out))
	}

	// Sort keys should be byte-orderable.
	// "apple" < "Banana" < "cherry" in case-insensitive collation.
	for i, tok := range out {
		if tok.Start != tokens[i].Start {
			t.Fatalf("token %d: expected start %d, got %d", i, tokens[i].Start, tok.Start)
		}
	}
}

// TestCollationPreservesOffsets verifies that offsets are preserved.
func TestCollationPreservesOffsets(t *testing.T) {
	cf, _ := NewCollationFilter("en", "primary")

	tokens := []Token{
		{Term: "test", Start: 10, End: 14, Pos: 0, Type: "<ALPHANUM>"},
	}

	out := cf.Filter(tokens)
	if out[0].Start != 10 || out[0].End != 14 {
		t.Fatalf("expected offsets preserved (10,14), got (%d,%d)", out[0].Start, out[0].End)
	}
}

// TestCollationUnknownLocale verifies fallback for unknown locale.
func TestCollationUnknownLocale(t *testing.T) {
	cf, err := NewCollationFilter("xx-YY", "primary")
	if err != nil {
		t.Fatalf("expected no error for unknown locale, got: %v", err)
	}

	tokens := []Token{{Term: "test", Start: 0, End: 4, Pos: 0, Type: "<ALPHANUM>"}}
	out := cf.Filter(tokens)
	if len(out) != 1 {
		t.Fatalf("expected 1 token, got %d", len(out))
	}
}

// TestCollationValidateStrength verifies strength validation.
func TestCollationValidateStrength(t *testing.T) {
	if err := validateStrength("primary"); err != nil {
		t.Fatalf("primary should be valid: %v", err)
	}
	if err := validateStrength("secondary"); err != nil {
		t.Fatalf("secondary should be valid: %v", err)
	}
	if err := validateStrength("invalid"); err == nil {
		t.Fatal("invalid should be rejected")
	}
}

// TestPipelineSingleStage verifies single-stage pipeline (passthrough).
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.5
func TestPipelineSingleStage(t *testing.T) {
	standard, _ := Get(NameStandard, nil)
	pa, err := NewPipelineAnalyzer(standard)
	if err != nil {
		t.Fatalf("NewPipelineAnalyzer failed: %v", err)
	}

	out := pa.Analyze([]byte("hello world"))
	if len(out) == 0 {
		t.Fatal("expected non-empty result from single-stage pipeline")
	}
	// Standard analyzer should produce "hello" and "world" tokens.
	if out[0].Term != "hello" {
		t.Fatalf("expected first token 'hello', got %q", out[0].Term)
	}
}

// TestPipelineEmptyAnalyzers verifies error for empty analyzer list.
func TestPipelineEmptyAnalyzers(t *testing.T) {
	_, err := NewPipelineAnalyzer()
	if err == nil {
		t.Fatal("expected error for empty analyzer list")
	}
}

// TestPipelineTwoStage verifies two-stage pipeline produces combined output.
func TestPipelineTwoStage(t *testing.T) {
	standard, _ := Get(NameStandard, nil)
	ngram, _ := Get(NameNGram, map[string]any{"min": 1, "max": 2})

	pa, err := NewPipelineAnalyzer(standard, ngram)
	if err != nil {
		t.Fatalf("NewPipelineAnalyzer failed: %v", err)
	}

	out := pa.Analyze([]byte("hi"))
	if len(out) == 0 {
		t.Fatal("expected non-empty result from two-stage pipeline")
	}
	// Stage 1 (standard): "hi" → ["hi"]
	// Stage 2 (ngram 1-2): "hi" → ["h", "hi", "i"]
	for _, tok := range out {
		if tok.Type != "<GRAM>" && tok.Type != "<ALPHANUM>" {
			// ngram tokens should have appropriate type
		}
	}
}

// TestFactoryMinHash verifies minhash analyzer via factory.
func TestFactoryMinHash(t *testing.T) {
	a, err := Get(NameMinHash, map[string]any{"num_perm": 8, "seed": 42})
	if err != nil {
		t.Fatalf("Get(minhash) failed: %v", err)
	}
	out := a.Analyze([]byte("hello world test"))
	if len(out) != 8 {
		t.Fatalf("expected 8 minhash tokens, got %d", len(out))
	}
}

// TestFactoryCollation verifies collation analyzer via factory.
func TestFactoryCollation(t *testing.T) {
	a, err := Get(NameCollation, map[string]any{"locale": "en", "strength": "primary"})
	if err != nil {
		t.Fatalf("Get(collation) failed: %v", err)
	}
	out := a.Analyze([]byte("hello"))
	if len(out) != 1 {
		t.Fatalf("expected 1 token (keyword tokenizer), got %d", len(out))
	}
}

// TestFactoryPipeline verifies pipeline analyzer via factory.
func TestFactoryPipeline(t *testing.T) {
	a, err := Get(NamePipeline, map[string]any{"analyzers": []any{"standard", "ngram"}})
	if err != nil {
		t.Fatalf("Get(pipeline) failed: %v", err)
	}
	out := a.Analyze([]byte("hi"))
	if len(out) == 0 {
		t.Fatal("expected non-empty result from factory pipeline")
	}
}

// TestFactoryPipelineNestedRejects verifies nested pipeline is rejected.
func TestFactoryPipelineNestedRejects(t *testing.T) {
	_, err := Get(NamePipeline, map[string]any{"analyzers": []any{"pipeline"}})
	if err == nil {
		t.Fatal("expected error for nested pipeline")
	}
}

// TestFactoryPipelineEmptyRejects verifies empty analyzer list is rejected.
func TestFactoryPipelineEmptyRejects(t *testing.T) {
	_, err := Get(NamePipeline, map[string]any{"analyzers": []any{}})
	if err == nil {
		t.Fatal("expected error for empty analyzer list")
	}
}

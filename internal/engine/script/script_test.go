package script

import (
	"strings"
	"testing"
)

func eval(t *testing.T, src string, ctx *ScriptCtx) any {
	t.Helper()
	scr, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	v, err := scr.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}

func TestParseAndEval_Arithmetic(t *testing.T) {
	doc := map[string]any{"price": float64(10), "qty": float64(3)}
	ctx := &ScriptCtx{Doc: doc}
	cases := []struct {
		src  string
		want float64
	}{
		{"1 + 2", 3},
		{"10 / 4", 2.5},
		{"(1 + 2) * 3", 9},
		{"7 % 3", 1},
		{"-5 + 8", 3},
		{"doc['price'].value * doc['qty'].value", 30},
		{"doc['price'].value + 5", 15},
	}
	for _, c := range cases {
		scr, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.src, err)
		}
		got, err := scr.EvalFloat(ctx)
		if err != nil {
			t.Fatalf("Eval(%q): %v", c.src, err)
		}
		if got != c.want {
			t.Errorf("Eval(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestParseAndEval_LogicAndComparison(t *testing.T) {
	doc := map[string]any{"status": "active", "score": float64(7)}
	ctx := &ScriptCtx{Doc: doc}
	cases := []struct {
		src  string
		want bool
	}{
		{"1 < 2", true},
		{"2 <= 1", false},
		{"3 == 3", true},
		{"3 != 3", false},
		{"true && false", false},
		{"true || false", true},
		{"!false", true},
		{"doc['score'].value > 5", true},
		{"doc['score'].value > 5 && 1 > 0", true},
	}
	for _, c := range cases {
		if got := eval(t, c.src, ctx); got != c.want {
			t.Errorf("Eval(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestParseAndEval_Ternary(t *testing.T) {
	doc := map[string]any{"score": float64(9)}
	ctx := &ScriptCtx{Doc: doc}
	if got := eval(t, "doc['score'].value > 5 ? 100 : 0", ctx); got != float64(100) {
		t.Errorf("ternary = %v, want 100", got)
	}
}

func TestParseAndEval_ScoreAndParams(t *testing.T) {
	ctx := &ScriptCtx{Score: 2.5, Params: map[string]any{"factor": float64(4)}}
	if got := eval(t, "_score * params.factor", ctx); got != float64(10) {
		t.Errorf("_score * params.factor = %v, want 10", got)
	}
}

func TestParseAndEval_MathMethods(t *testing.T) {
	ctx := &ScriptCtx{}
	if got := eval(t, "Math.abs(0 - 3)", ctx); got != float64(3) {
		t.Errorf("Math.abs = %v, want 3", got)
	}
	if got := eval(t, "Math.max(1, 5)", ctx); got != float64(5) {
		t.Errorf("Math.max = %v, want 5", got)
	}
	if got := eval(t, "Math.pow(2, 10)", ctx); got != float64(1024) {
		t.Errorf("Math.pow = %v, want 1024", got)
	}
}

func TestParseAndEval_VectorFunctions(t *testing.T) {
	ctx := &ScriptCtx{Doc: map[string]any{
		"vec": []any{float64(1), float64(0)},
	}}
	if got := eval(t, "cosineSimilarity(params.q, doc['vec'].value)",
		&ScriptCtx{Doc: ctx.Doc, Params: map[string]any{"q": []any{float64(1), float64(0)}}}); got != float64(1) {
		t.Errorf("cosineSimilarity = %v, want 1", got)
	}
	if got := eval(t, "l2norm(doc['vec'].value)", ctx); got != float64(1) {
		t.Errorf("l2norm = %v, want 1", got)
	}
}

func TestEval_StringConcat(t *testing.T) {
	ctx := &ScriptCtx{Doc: map[string]any{"name": "uni"}}
	if got := eval(t, `"hello " + "world"`, ctx); got != "hello world" {
		t.Errorf("string concat = %v", got)
	}
}

func TestParse_ErrorOnGarbage(t *testing.T) {
	if _, err := Parse("this is not painless !!!"); err == nil {
		t.Error("expected parse error for garbage input")
	}
	if _, err := Parse("doc['a'] +"); err == nil {
		t.Error("expected parse error for trailing operator")
	}
}

func TestEval_StepBudget(t *testing.T) {
	scr, err := Parse("1 + 1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &ScriptCtx{Budget: 0} // fills MaxSteps
	if _, err := scr.Eval(ctx); err != nil {
		t.Fatalf("normal eval should not hit budget: %v", err)
	}
	small := &ScriptCtx{Budget: 1}
	scr2, err := Parse("1 + 2 + 3")
	if err != nil {
		t.Fatal(err)
	}
	_, err = scr2.Eval(small)
	if err == nil || !strings.Contains(err.Error(), "script_exception") {
		t.Errorf("expected script_exception budget error, got %v", err)
	}
}

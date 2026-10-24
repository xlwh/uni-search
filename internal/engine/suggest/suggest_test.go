package suggest

import "testing"

func TestTermSuggester(t *testing.T) {
	terms := map[string]int{
		"trying": 10, "tracking": 4, "terrain": 2, "unrelated": 9,
	}
	ts := NewTermSuggester(terms)

	suggs := ts.Suggest("tring", 2)
	if len(suggs) == 0 {
		t.Fatal("expected suggestions for 'tring'")
	}
	if suggs[0].Text != "trying" {
		t.Errorf("top suggestion = %q, want 'trying' (highest freq in distance)", suggs[0].Text)
	}
	for _, s := range suggs {
		if s.Text == "unrelated" {
			t.Error("'unrelated' is beyond edit distance 2, should not be suggested")
		}
	}

	// Exact word returns itself.
	suggs = ts.Suggest("trying", 2)
	found := false
	for _, s := range suggs {
		if s.Text == "trying" {
			found = true
		}
	}
	if !found {
		t.Error("exact word should appear in its own suggestions")
	}

	// Distance clamp: maxDist > 2 behaves as 2.
	if got := ts.Suggest("xyzabc", 10); len(got) != 0 {
		t.Errorf("distance clamp failed, got %v", got)
	}
}

func TestPhraseSuggester(t *testing.T) {
	terms := map[string]int{"hello": 5, "world": 5, "searching": 3}
	ps := NewPhraseSuggester(NewTermSuggester(terms))

	suggs := ps.Suggest("helo wrold", 2)
	if len(suggs) != 1 {
		t.Fatalf("phrase suggester returns %d entries, want 1", len(suggs))
	}
	if suggs[0].Text != "hello world" {
		t.Errorf("corrected phrase = %q, want 'hello world'", suggs[0].Text)
	}
}

func TestCompletionSuggester(t *testing.T) {
	cs := NewCompletionSuggester([]CompletionEntry{
		{Text: "elastic", Score: 3},
		{Text: "elasticsearch", Score: 8},
		{Text: "elbow", Score: 1},
		{Text: "zebra", Score: 100},
	})

	suggs := cs.Suggest("el", 5)
	if len(suggs) != 3 {
		t.Fatalf("prefix 'el' = %d suggestions, want 3", len(suggs))
	}
	// Sorted by score descending.
	if suggs[0].Text != "elasticsearch" {
		t.Errorf("top completion = %q, want 'elasticsearch'", suggs[0].Text)
	}
	for _, s := range suggs {
		if s.Text == "zebra" {
			t.Error("'zebra' should not match prefix 'el'")
		}
	}

	// Size limit.
	if got := len(cs.Suggest("el", 2)); got != 2 {
		t.Errorf("size=2 returned %d, want 2", got)
	}

	// No match.
	if got := cs.Suggest("zz", 5); len(got) != 0 {
		t.Errorf("prefix 'zz' returned %v, want none", got)
	}
}

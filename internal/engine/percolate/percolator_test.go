package percolate

import "testing"

func TestRegisterAndMatch(t *testing.T) {
	p := New()

	if err := p.Register("q1", []byte(`{"term": {"status": "error"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Register("q2", []byte(`{"match": {"message": "connection reset"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Register("q3", []byte(`{"range": {"code": {"gte": 500}}}`)); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", p.Count())
	}

	// Matches term q1 only.
	m, err := p.Match([]byte(`{"status": "error", "message": "ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[0] != "q1" {
		t.Errorf("Match(status=error) = %v, want [q1]", m)
	}

	// Matches range q3 only.
	m, _ = p.Match([]byte(`{"code": 503}`))
	if len(m) != 1 || m[0] != "q3" {
		t.Errorf("Match(code=503) = %v, want [q3]", m)
	}

	// No matches.
	m, _ = p.Match([]byte(`{"status": "ok"}`))
	if len(m) != 0 {
		t.Errorf("Match(status=ok) = %v, want none", m)
	}
}

func TestMatch_BoolQuery(t *testing.T) {
	p := New()
	if err := p.Register("bool-q", []byte(
		`{"bool": {"must": [{"term": {"env": "prod"}}], "must_not": [{"term": {"canary": "true"}}]}}`)); err != nil {
		t.Fatal(err)
	}

	m, _ := p.Match([]byte(`{"env": "prod", "canary": "false"}`))
	if len(m) != 1 || m[0] != "bool-q" {
		t.Errorf("prod non-canary should match: %v", m)
	}
	m, _ = p.Match([]byte(`{"env": "prod", "canary": "true"}`))
	if len(m) != 0 {
		t.Errorf("canary doc should not match: %v", m)
	}
	m, _ = p.Match([]byte(`{"env": "dev"}`))
	if len(m) != 0 {
		t.Errorf("dev doc should not match: %v", m)
	}
}

func TestMatch_MatchAllAndPhrase(t *testing.T) {
	p := New()
	_ = p.Register("all", []byte(`{"match_all": {}}`))
	_ = p.Register("phrase", []byte(`{"match_phrase": {"title": "quick brown fox"}}`))

	m, _ := p.Match([]byte(`{"anything": 1}`))
	if len(m) != 1 || m[0] != "all" {
		t.Errorf("match_all should always match: %v", m)
	}

	m, _ = p.Match([]byte(`{"title": "the quick brown fox jumps"}`))
	if len(m) != 2 { // match_all + phrase
		t.Errorf("phrase doc should match both: %v", m)
	}
}

func TestUnregister(t *testing.T) {
	p := New()
	_ = p.Register("q1", []byte(`{"match_all": {}}`))
	if err := p.Unregister("q1"); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 0 {
		t.Errorf("Count() = %d after unregister, want 0", p.Count())
	}
	if err := p.Unregister("missing"); err == nil {
		t.Error("unregistering a missing id should error")
	}
}

func TestLookup(t *testing.T) {
	p := New()
	_ = p.Register("q1", []byte(`{"term": {"a": "b"}}`))
	sq := p.Lookup("q1")
	if sq == nil || string(sq.Query) != `{"term": {"a": "b"}}` {
		t.Errorf("Lookup(q1) = %v, want stored query", sq)
	}
	if p.Lookup("missing") != nil {
		t.Error("Lookup(missing) should be nil")
	}
}

func TestMatch_InvalidDoc(t *testing.T) {
	p := New()
	_, err := p.Match([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid doc JSON")
	}
}

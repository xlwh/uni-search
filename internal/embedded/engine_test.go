package embedded

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// TestEngine_WriteAndCount tests basic write + count through the real
// engine stack (no HTTP layer).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.1.7
func TestEngine_WriteAndCount(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	ctx := context.Background()

	// Put mapping.
	err = eng.PutMapping(ctx, "test", []byte(`{"mappings":{"properties":{"title":{"type":"text"}}}}`))
	require.NoError(t, err)

	// Write a document.
	_, err = eng.Write(ctx, esapi.IndexDoc{
		Op:     esapi.OpIndex,
		Index:  "test",
		ID:     "d1",
		Source: []byte(`{"title":"hello world"}`),
	}, "")
	require.NoError(t, err)

	// Count before refresh (should find in memtable).
	count, err := eng.Count(ctx, []string{"test"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "count before refresh should be 1")

	// Check fieldIDs.
	idx, ok := eng.manager.getIndex("test")
	require.True(t, ok)
	t.Logf("fieldIDs: %v", idx.fieldIDs)
	t.Logf("fieldDefs: %v", idx.fieldDefs)

	// Refresh (flush to segment).
	err = eng.Refresh(ctx, "test")
	require.NoError(t, err)

	// Check segment set after refresh.
	snap, segSet := idx.shard.Snapshot()
	t.Logf("snapshot: ReadUpTo=%d", snap.ReadUpTo)
	if segSet != nil {
		t.Logf("segment set items: %d", len(segSet.Items()))
		for i, entry := range segSet.Items() {
			if entry.Reader != nil {
				t.Logf("  segment %d: rows=%d, indexes=%d", i, entry.Reader.NumRows(), len(entry.Indexes))
				for fid, li := range entry.Indexes {
					t.Logf("    index field %d: %T", fid, li)
				}
			}
		}
	} else {
		t.Log("segment set is nil after refresh!")
	}

	// Count after refresh (should find in segment).
	count, err = eng.Count(ctx, []string{"test"}, []byte(`{}`))
	require.NoError(t, err)
	t.Logf("count after refresh: %d", count)

	// Search with match query after refresh.
	result, err := eng.Search(ctx, []string{"test"}, []byte(`{"query":{"match":{"title":"hello"}},"size":10}`))
	require.NoError(t, err)
	t.Logf("search result: total=%d, hits=%d", result.Total, len(result.Hits))
	for i, h := range result.Hits {
		t.Logf("  hit %d: id=%s, score=%f", i, h.ID, h.Score)
	}
}

// TestEngine_MatchSearchAfterRefresh tests the full match query path:
// write 3 docs → refresh → search for "hello" → expect 2 hits.
func TestEngine_MatchSearchAfterRefresh(t *testing.T) {
	dir := t.TempDir()
	eng, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	ctx := context.Background()

	// Put mapping with title:text field.
	err = eng.PutMapping(ctx, "docs", []byte(`{"mappings":{"properties":{"title":{"type":"text"}}}}`))
	require.NoError(t, err)

	// Write 3 documents.
	for _, d := range []struct{ id, title string }{
		{"d1", "hello world"},
		{"d2", "hello database"},
		{"d3", "goodbye world"},
	} {
		_, err = eng.Write(ctx, esapi.IndexDoc{
			Op:     esapi.OpIndex,
			Index:  "docs",
			ID:     d.id,
			Source: []byte(`{"title":"` + d.title + `"}`),
		}, "")
		require.NoError(t, err)
	}

	// Count before refresh.
	count, err := eng.Count(ctx, []string{"docs"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, int64(3), count, "count before refresh")

	// Refresh.
	err = eng.Refresh(ctx, "docs")
	require.NoError(t, err)

	// Count after refresh.
	count, err = eng.Count(ctx, []string{"docs"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, int64(3), count, "count after refresh")

	// Search for "hello" → should match d1 and d2.
	result, err := eng.Search(ctx, []string{"docs"}, []byte(`{"query":{"match":{"title":"hello"}},"size":10}`))
	require.NoError(t, err)
	t.Logf("search total=%d, hits=%d", result.Total, len(result.Hits))
	for i, h := range result.Hits {
		t.Logf("  hit %d: id=%s, score=%f, source=%s", i, h.ID, h.Score, string(h.Source))
	}
	assert.Equal(t, 2, result.Total, "search should find 2 documents matching 'hello'")
	require.Len(t, result.Hits, 2, "should have 2 hits")
}

// TestEngine_Restart tests write → close → reopen → count.
func TestEngine_Restart(t *testing.T) {
	dir := t.TempDir()

	// First session.
	eng1, err := New(dir)
	require.NoError(t, err)
	ctx := context.Background()
	err = eng1.PutMapping(ctx, "test", []byte(`{"mappings":{"properties":{"title":{"type":"text"}}}}`))
	require.NoError(t, err)
	_, err = eng1.Write(ctx, esapi.IndexDoc{
		Op:     esapi.OpIndex,
		Index:  "test",
		ID:     "d1",
		Source: []byte(`{"title":"hello"}`),
	}, "")
	require.NoError(t, err)
	_, err = eng1.Write(ctx, esapi.IndexDoc{
		Op:     esapi.OpIndex,
		Index:  "test",
		ID:     "d2",
		Source: []byte(`{"title":"world"}`),
	}, "")
	require.NoError(t, err)

	count, err := eng1.Count(ctx, []string{"test"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	require.NoError(t, eng1.Close())

	// Second session.
	eng2, err := New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng2.Close() })

	count, err = eng2.Count(ctx, []string{"test"}, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, int64(2), count, "count after restart should be 2")
}

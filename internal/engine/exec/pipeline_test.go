package exec

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.5

// mockOperator emits a fixed set of scored docs then EOF.
type mockOperator struct {
	docs []ScoredDoc
	idx  int
}

func (m *mockOperator) Next(ctx context.Context) (*ColumnBatch, error) {
	if m.idx >= len(m.docs) {
		return nil, ErrEOF
	}
	batch := NewBatch(len(m.docs) - m.idx)
	for m.idx < len(m.docs) {
		d := m.docs[m.idx]
		batch.AppendRow(d.DocID>>32, uint32(d.DocID))
		batch.AppendScore(d.Score)
		m.idx++
	}
	return batch, nil
}

func (m *mockOperator) Close() {}

func TestPipeline_BasicFusion(t *testing.T) {
	// Two prefetch paths, RRF fusion.
	pathA := &mockOperator{docs: []ScoredDoc{
		{DocID: 1, Score: 1.0},
		{DocID: 2, Score: 0.5},
	}}
	pathB := &mockOperator{docs: []ScoredDoc{
		{DocID: 2, Score: 2.0},
		{DocID: 3, Score: 1.0},
	}}

	// Simple fusion: concatenate and sort by score descending.
	fusion := func(paths [][]ScoredDoc) ([]ScoredDoc, error) {
		var all []ScoredDoc
		for _, p := range paths {
			all = append(all, p...)
		}
		// Sort descending by score.
		for i := 0; i < len(all); i++ {
			for j := i + 1; j < len(all); j++ {
				if all[j].Score > all[i].Score {
					all[i], all[j] = all[j], all[i]
				}
			}
		}
		return all, nil
	}

	pipeline := NewPipelineOperator([]Operator{pathA, pathB}, fusion, 10)
	batch, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch)
	// 4 docs total (2 per path, no dedup in this simple fusion).
	assert.Equal(t, 4, batch.Rows)

	pipeline.Close()
}

func TestPipeline_NoFusion(t *testing.T) {
	pathA := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	pathB := &mockOperator{docs: []ScoredDoc{{DocID: 2, Score: 0.5}}}

	pipeline := NewPipelineOperator([]Operator{pathA, pathB}, nil, 10)
	batch, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch)
	assert.Equal(t, 2, batch.Rows)

	pipeline.Close()
}

func TestPipeline_SizeLimit(t *testing.T) {
	path := &mockOperator{docs: []ScoredDoc{
		{DocID: 1, Score: 1.0},
		{DocID: 2, Score: 0.8},
		{DocID: 3, Score: 0.5},
	}}

	pipeline := NewPipelineOperator([]Operator{path}, nil, 2)
	batch, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch)
	assert.Equal(t, 2, batch.Rows, "should truncate to size=2")

	pipeline.Close()
}

func TestPipeline_EmptyPrefetches(t *testing.T) {
	pipeline := NewPipelineOperator(nil, nil, 10)
	batch, err := pipeline.Next(context.Background())
	assert.ErrorIs(t, err, ErrEOF)
	assert.Nil(t, batch)
}

func TestPipeline_SingleShot(t *testing.T) {
	path := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	pipeline := NewPipelineOperator([]Operator{path}, nil, 10)

	batch1, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch1)

	batch2, err := pipeline.Next(context.Background())
	assert.ErrorIs(t, err, ErrEOF)
	assert.Nil(t, batch2)

	pipeline.Close()
}

func TestPipeline_ParallelExecution(t *testing.T) {
	// Multiple paths should be executed (parallel or sequential).
	paths := make([]Operator, 5)
	for i := range paths {
		paths[i] = &mockOperator{docs: []ScoredDoc{
			{DocID: uint64(i + 1), Score: float32(5-i)},
		}}
	}

	pipeline := NewPipelineOperator(paths, nil, 10)
	batch, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch)
	assert.Equal(t, 5, batch.Rows)

	pipeline.Close()
}

func TestAdaptParallel(t *testing.T) {
	assert.False(t, AdaptParallel(0))
	assert.False(t, AdaptParallel(1))
	assert.True(t, AdaptParallel(2))
	assert.True(t, AdaptParallel(4))
}

// --- Edge case and error path tests ---

// errorOperator returns a non-EOF error on Next.
type errorOperator struct{}

func (e *errorOperator) Next(ctx context.Context) (*ColumnBatch, error) {
	return nil, fmt.Errorf("operator failure")
}
func (e *errorOperator) Close() {}

func TestPipeline_OperatorError(t *testing.T) {
	pipeline := NewPipelineOperator([]Operator{&errorOperator{}}, nil, 10)
	batch, err := pipeline.Next(context.Background())
	assert.Error(t, err)
	assert.Nil(t, batch)
	pipeline.Close()
}

func TestPipeline_FusionError(t *testing.T) {
	path := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	errFusion := func(paths [][]ScoredDoc) ([]ScoredDoc, error) {
		return nil, fmt.Errorf("fusion failure")
	}
	pipeline := NewPipelineOperator([]Operator{path}, errFusion, 10)
	batch, err := pipeline.Next(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "fusion failure")
	assert.Nil(t, batch)
	pipeline.Close()
}

func TestPipeline_SizeZero(t *testing.T) {
	path := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	pipeline := NewPipelineOperator([]Operator{path}, nil, 0)
	batch, err := pipeline.Next(context.Background())
	// size=0 means no truncation, all results returned.
	require.NoError(t, err)
	require.NotNil(t, batch)
	assert.Equal(t, 1, batch.Rows)
	pipeline.Close()
}

func TestPipeline_AllEmptyResults(t *testing.T) {
	empty := &mockOperator{docs: nil}
	pipeline := NewPipelineOperator([]Operator{empty, empty}, nil, 10)
	batch, err := pipeline.Next(context.Background())
	assert.ErrorIs(t, err, ErrEOF)
	assert.Nil(t, batch)
	pipeline.Close()
}

func TestPipeline_NilOperatorInPrefetches(t *testing.T) {
	good := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	pipeline := NewPipelineOperator([]Operator{good, nil}, nil, 10)
	batch, err := pipeline.Next(context.Background())
	require.NoError(t, err)
	require.NotNil(t, batch)
	assert.Equal(t, 1, batch.Rows, "nil operator contributes 0 results")
	pipeline.Close()
}

func TestPipeline_CloseIsIdempotent(t *testing.T) {
	path := &mockOperator{docs: []ScoredDoc{{DocID: 1, Score: 1.0}}}
	pipeline := NewPipelineOperator([]Operator{path}, nil, 10)
	pipeline.Close()
	pipeline.Close() // should not panic
}

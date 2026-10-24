// Package exec pipeline.go implements the unified query pipeline operator
// (Qdrant-style Query API). It orchestrates nested Prefetch sub-queries,
// fuses their results, and applies rescore stages. The execution uses
// adaptive parallelism: sub-queries are executed in parallel when their
// count is small enough to benefit from concurrency without overwhelming
// the system.
//
// @spec: ./active_specs/2026-10-23-qdrant-features-import/02_spec.md#2.5
package exec

import (
	"context"
	"runtime"
	"sync"
)

// ScoredDoc is the pipeline-stage scored document (mirrors fusion.ScoredDoc
// to avoid an import cycle: fusion imports exec for ValidateFromSize).
type ScoredDoc struct {
	DocID uint64
	Score float32
}

// FusionFunc is the fusion function type used by the pipeline. The caller
// (translator) wraps fusion.Fusion.Merge into this type to avoid an
// import cycle between exec and fusion.
type FusionFunc func(paths [][]ScoredDoc) ([]ScoredDoc, error)

// PipelineOperator orchestrates a multi-stage query pipeline:
// 1. Execute prefetch sub-queries (adaptive parallel/sequential)
// 2. Fuse results using the specified fusion function
// 3. Apply rescore stages
type PipelineOperator struct {
	prefetches []Operator     // sub-query operators
	fusion     FusionFunc     // fusion function (nil = just concatenate)
	size       int            // final result count
	done       bool          // single-shot guard
	results    []ScoredDoc   // fused results
	mu         sync.Mutex
}

// NewPipelineOperator creates a pipeline operator.
func NewPipelineOperator(prefetches []Operator, f FusionFunc, size int) *PipelineOperator {
	return &PipelineOperator{
		prefetches: prefetches,
		fusion:     f,
		size:       size,
	}
}

// Next implements Operator. The pipeline executes all prefetches, fuses
// results, and returns a single batch. Subsequent calls return EOF.
func (p *PipelineOperator) Next(ctx context.Context) (*ColumnBatch, error) {
	if p.done {
		return nil, ErrEOF
	}
	p.done = true

	// Execute prefetches with adaptive parallelism.
	paths := p.executePrefetches(ctx)
	if len(paths) == 0 {
		return nil, ErrEOF
	}

	// Fuse results.
	if p.fusion != nil {
		fused, err := p.fusion(paths)
		if err != nil {
			return nil, err
		}
		p.results = fused
	} else {
		// No fusion: concatenate all paths.
		for _, path := range paths {
			p.results = append(p.results, path...)
		}
	}

	// Truncate to size.
	if p.size > 0 && len(p.results) > p.size {
		p.results = p.results[:p.size]
	}

	// Build output batch.
	batch := NewBatch(len(p.results))
	for _, d := range p.results {
		batch.AppendRow(d.DocID>>32, uint32(d.DocID))
		batch.AppendScore(d.Score)
	}
	if batch.Rows == 0 {
		return nil, ErrEOF
	}
	return batch, nil
}

// Close implements Operator.
func (p *PipelineOperator) Close() {
	for _, op := range p.prefetches {
		if op != nil {
			op.Close()
		}
	}
}

// executePrefetches runs all prefetch sub-queries, returning the per-path
// results. Uses adaptive parallelism: if the number of prefetches is
// <= the parallel threshold (based on CPU cores), they are executed
// concurrently with goroutines; otherwise they run sequentially.
func (p *PipelineOperator) executePrefetches(ctx context.Context) [][]ScoredDoc {
	n := len(p.prefetches)
	if n == 0 {
		return nil
	}

	// Adaptive threshold: run in parallel if prefetch count <= 2×CPU cores.
	threshold := runtime.NumCPU() * 2
	if threshold < 2 {
		threshold = 2
	}

	paths := make([][]ScoredDoc, n)

	if n <= threshold {
		// Parallel execution.
		var wg sync.WaitGroup
		for i, op := range p.prefetches {
			wg.Add(1)
			go func(idx int, operator Operator) {
				defer wg.Done()
				paths[idx] = drainOperator(ctx, operator)
			}(i, op)
		}
		wg.Wait()
	} else {
		// Sequential execution (too many sub-queries for parallel).
		for i, op := range p.prefetches {
			paths[i] = drainOperator(ctx, op)
		}
	}

	return paths
}

// drainOperator pulls all results from an operator and converts them
// to ScoredDoc slices for fusion.
func drainOperator(ctx context.Context, op Operator) []ScoredDoc {
	if op == nil {
		return nil
	}
	var results []ScoredDoc
	for {
		batch, err := op.Next(ctx)
		if err != nil {
			return results
		}
		if batch == nil || batch.Rows == 0 {
			continue
		}
		for i := 0; i < batch.Rows; i++ {
			var score float32
			if i < len(batch.Scores) {
				score = batch.Scores[i]
			}
			if i < len(batch.DocIDs) {
				results = append(results, ScoredDoc{
					DocID: batch.DocIDs[i],
					Score: score,
				})
			}
		}
	}
}

// AdaptParallel decides whether to run prefetches in parallel based on
// the count and system resources. Returns true if parallel execution
// is recommended.
func AdaptParallel(prefetchCount int) bool {
	if prefetchCount <= 1 {
		return false
	}
	cores := runtime.NumCPU()
	return prefetchCount <= cores*2
}

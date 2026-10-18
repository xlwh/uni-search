// Package exec operator.go defines the §3.3 Volcano-style Operator
// interface and shared helpers. Each operator exposes a pull-based
// Next that returns one ColumnBatch at a time; io.EOF signals end
// of stream. Operators compose into pipelines; the executor runs
// one goroutine per shard sub-plan (00 §3.3, M0 single-process).
package exec

import (
	"context"
	"errors"
	"io"
)

// ErrEOF is the end-of-stream sentinel returned by Operator.Next.
// The design uses io.EOF directly; this alias keeps callers from
// having to import io just for the comparison.
var ErrEOF = io.EOF

// ErrUnsupported is returned by operators that lack a wired
// dependency (e.g. ANN without a registered index). M0 wires every
// operator; M2 will add sidecar-remote variants that may surface
// this error.
var ErrUnsupported = errors.New("exec: operator unsupported on this path")

// Operator is the §3.3 Volcano pull interface. Next returns the next
// ColumnBatch, or an error: io.EOF on normal end-of-stream, any
// other error on failure. Close releases per-operator state and
// recursive inputs' state; it must be idempotent.
type Operator interface {
	Next(ctx context.Context) (*ColumnBatch, error)
	Close()
}

// noopOperator emits nothing and closes cleanly; used as a sentinel
// for unconfigured plan slots and as a base for embedding.
type noopOperator struct{}

func (noopOperator) Next(context.Context) (*ColumnBatch, error) {
	return nil, ErrEOF
}

func (noopOperator) Close() {}

// Pipeline wraps a chain of operators so callers can drive a whole
// plan with a single Next/Close pair. The chain runs in pull order:
// head is the leaf (Scan / ANN / BM25Score) and tail is the
// terminal operator (TopN). Pipeline does not parallelize; M2
// adds the errgroup-based fan-out across shards.
type Pipeline struct {
	tail Operator
}

// NewPipeline constructs a pipeline whose tail operator is op. The
// caller is responsible for the input wiring of op; Pipeline only
// owns the outer driving loop.
func NewPipeline(op Operator) *Pipeline { return &Pipeline{tail: op} }

// Pull drains the pipeline and returns every batch the tail emits,
// up to maxBatches (≤ 0 = unlimited). Useful for tests; production
// paths stream batches straight to the response writer.
func (p *Pipeline) Pull(ctx context.Context, maxBatches int) ([]*ColumnBatch, error) {
	var out []*ColumnBatch
	for {
		b, err := p.tail.Next(ctx)
		if err != nil {
			if errors.Is(err, ErrEOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, b)
		if maxBatches > 0 && len(out) >= maxBatches {
			return out, nil
		}
	}
}

// Close releases the tail operator's resources. Idempotent.
func (p *Pipeline) Close() {
	if p != nil && p.tail != nil {
		p.tail.Close()
	}
}

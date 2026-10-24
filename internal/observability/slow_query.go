package observability

import (
	"context"
	"log/slog"
	"time"
)

// slow_query.go implements the slow query logger (design 06 §3):
// queries slower than 1 second get a WARN-level log entry with
// took_ms, shards_scanned, rows_read, and plan_summary fields.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.2

// SlowQueryThreshold is the duration above which a query is logged
// as slow (design 06 §3: >1s).
const SlowQueryThreshold = 1 * time.Second

// LogSlowQuery emits a slow-query log entry when the duration exceeds
// the 1-second threshold. Called from the engine's Search method.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#4.2
func LogSlowQuery(ctx context.Context, took time.Duration, shardsScanned int, rowsRead int64, planSummary string) {
	if took < SlowQueryThreshold {
		return
	}
	slog.WarnContext(ctx, "slow query",
		"took_ms", took.Milliseconds(),
		"shards_scanned", shardsScanned,
		"rows_read", rowsRead,
		"plan_summary", planSummary,
	)
}

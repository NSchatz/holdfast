package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// What one published frame COSTS at ledger scale (S0096, performance PB4).
//
// The figures a frame carries scale with the ROW COUNT of `jobs` and not with the rows
// returned: the six aggregates and the two row totals each scan the matching set, while
// the queue and history reads are capped at 500 and 200 rows however large the library
// grows. That is the complexity the spec names, and this is where it is measured, at the
// three sizes it names.
//
// IT IS NOT A GATE AND MUST NEVER BECOME ONE. `make check` does not run it (AC-14) and
// nothing here fails on a number: an absolute threshold on a shared runner
// false-positives at roughly 45% (performance PB5), so the figures are RECORDED beside
// the benchmark in snapshot_cost_baseline.json and compared against the stored previous
// figure taken the same way. `make snapshot-bench` is the invocation.

// The caps and status sets the reporting hub applies, restated here because they live in
// internal/server and a benchmark of the store must not make the store depend on it. They
// are the same values: queue 500 over the non-terminal statuses, history 200 over the
// terminal ones.
var (
	benchQueueStatuses   = []Status{Pending, Probing, Encoding, Verifying}
	benchHistoryStatuses = []Status{Done, Skipped, Failed, WouldTranscode, Indeterminate, AppliedDespiteError}
)

const (
	benchQueueLimit   = 500
	benchHistoryLimit = 200
)

// [AC-10] WHEN the ledger-scale benchmark runs THE SYSTEM SHALL report a per-snapshot cost
// for seeded ledgers of 10k, 100k and 500k rows.
//
// One iteration is ONE SNAPSHOT'S WORTH of store reads - the whole-ledger figure set plus
// the per-frame reads - so ns/op is the per-snapshot cost the spec asks for. The seeding
// is outside the timer.
func BenchmarkSnapshot_AtLedgerScale(b *testing.B) {
	for _, rows := range []int{10_000, 100_000, 500_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			st := seedLedger(b, rows)
			ctx := context.Background()
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				snapshotReads(b, ctx, st)
			}
		})
	}
}

// snapshotReads issues exactly the store reads one published frame makes, in the order the
// hub makes them.
func snapshotReads(b *testing.B, ctx context.Context, st *SQLite) {
	b.Helper()
	a := st.Aggregates(ctx)
	for what, err := range map[string]error{
		"outcomes": a.Outcomes.Err, "skips_by_guard": a.SkipsByGuard.Err,
		"size_ratio": a.SizeRatio.Err, "encode_ms": a.EncodeMs.Err,
		"vmaf_mean": a.VmafMean.Err, "vmaf_min": a.VmafMin.Err,
	} {
		if err != nil {
			b.Fatalf("aggregate %s: %v", what, err)
		}
	}
	if t := st.CountRows(ctx, benchQueueStatuses); t.Err != nil {
		b.Fatalf("queue total: %v", t.Err)
	}
	if t := st.CountRows(ctx, benchHistoryStatuses); t.Err != nil {
		b.Fatalf("history total: %v", t.Err)
	}
	if _, err := st.Summary(ctx); err != nil {
		b.Fatalf("summary: %v", err)
	}
	if _, err := st.List(ctx, benchQueueStatuses, benchQueueLimit); err != nil {
		b.Fatalf("queue list: %v", err)
	}
	if _, err := st.List(ctx, benchHistoryStatuses, benchHistoryLimit); err != nil {
		b.Fatalf("history list: %v", err)
	}
	if _, err := st.HeldByUndoWindow(ctx); err != nil {
		b.Fatalf("held by undo window: %v", err)
	}
}

// seedLedger builds a ledger of n rows shaped like a real one: mostly done rows carrying
// the size, duration and VMAF figures the spreads read, a body of skipped rows carrying
// the guard tokens the breakdown reads, some failures, and a live queue over the cap.
//
// It writes through the database directly rather than through Claim/Finish. Half a
// million transitions through the engine's write path would take longer than the thing
// being measured by orders of magnitude, and what is being measured is the READ side: the
// rows only have to be there, and to be distributed the way a library's are.
func seedLedger(b *testing.B, n int) *SQLite {
	b.Helper()
	st, err := Open(b.TempDir() + "/jobs.db")
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })

	tx, err := st.db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO jobs
		(path, fingerprint, status, fail_count, updated_at, reason, encoder,
		 vmaf_mean, vmaf_min, vmaf_model, source_bytes, output_bytes, encode_ms, schema_version)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	guards := []string{"low-bitrate", "already-target-codec", "hardlinked", "too-small", "excluded-path"}
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/lib/seed/%06d/file%09d.mkv", i%1000, i)
		var (
			status                 Status
			reason, encoder, model sql.NullString
			mean, worst            sql.NullFloat64
			src, out, ms           sql.NullInt64
		)
		switch {
		case i%20 == 0: // a live queue, deliberately over the 500-row cap
			status = Pending
		case i%20 == 1:
			status = Encoding
		case i%20 < 6:
			status = Skipped
			reason = sql.NullString{String: guards[i%len(guards)], Valid: true}
		case i%20 < 8:
			status = Failed
			reason = sql.NullString{String: "simulated: the encode did not complete", Valid: true}
		default:
			status = Done
			encoder = sql.NullString{String: "cpu", Valid: true}
			model = sql.NullString{String: "version=vmaf_v0.6.1", Valid: true}
			// One done row in nine records no measurement at all - a row written
			// before the outcome columns existed. The aggregates have to count it
			// as excluded rather than fold it in as a zero, and the query that does
			// that is the one being measured.
			if i%9 != 0 {
				mean = sql.NullFloat64{Float64: 92 + float64(i%800)/100, Valid: true}
				worst = sql.NullFloat64{Float64: 84 + float64(i%1200)/100, Valid: true}
				src = sql.NullInt64{Int64: int64(3_000_000_000 + i), Valid: true}
				out = sql.NullInt64{Int64: int64(1_100_000_000 + i), Valid: true}
				ms = sql.NullInt64{Int64: int64(600_000 + i%400_000), Valid: true}
			}
		}
		if _, err := stmt.Exec(path, "size:mtime", string(status), int64(1_700_000_000+i),
			reason, encoder, mean, worst, model, src, out, ms, 1); err != nil {
			b.Fatalf("seed row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		b.Fatalf("close stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}

	// A handful of live retentions, so the undo window's SUM has rows to add. It is
	// bounded by what is currently retained and not by ledger history, which is exactly
	// why it stays on every frame and is not in the cached figure set.
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.Retain(ctx, Retained{
			SourcePath:         fmt.Sprintf("/lib/seed/retained%d.mkv", i),
			SwappedPath:        fmt.Sprintf("/lib/seed/retained%d.mkv", i),
			RetainedPath:       fmt.Sprintf("/lib/seed/.holdfast/retained%d.mkv", i),
			SourceBytes:        int64(2_000_000_000 + i),
			SwappedFingerprint: "size:mtime",
			RetainedAt:         1_700_000_000,
			ExpiresAt:          1_700_086_400,
		}); err != nil {
			b.Fatalf("seed retention: %v", err)
		}
	}

	var got int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&got); err != nil {
		b.Fatalf("count seeded rows: %v", err)
	}
	if got != int64(n) {
		b.Fatalf("seeded %d rows, want %d", got, n)
	}
	return st
}

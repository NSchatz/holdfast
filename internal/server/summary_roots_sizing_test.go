package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/diskfree"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestS0169_AC6_EverySizingQuestionIsAnsweredByOneSummaryBody grades [AC-6]: the sizing an
// operator used to do by copying jobs.db off the host and querying it with sqlite is
// answered by ONE GET /api/summary body, and every figure in it equals the answer of the
// equivalent SQL run directly against that jobs.db file.
//
// The ledger is written through the store's own write path (Claim, Finish, Retain,
// MarkRestored), exactly as a dry run, a real run and the undo window write it. The SQL
// below is the operator's: it is written against the schema, opens the file through
// database/sql, and shares no code with the store's reads or the handler.
func TestS0169_AC6_EverySizingQuestionIsAnsweredByOneSummaryBody(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	base := t.TempDir()
	tv, films := filepath.Join(base, "tv"), filepath.Join(base, "films")
	for _, d := range []string{tv, films} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A dry run's candidates across two roots, one of them unsized.
	finishRow(t, st, tv+"/s01e01.mkv", store.WouldTranscode, tv, i64p(900_000_001), nil)
	finishRow(t, st, tv+"/s01e02.mkv", store.WouldTranscode, tv, i64p(750_000_000), nil)
	finishRow(t, st, tv+"/s01e03.mkv", store.WouldTranscode, tv, nil, nil)
	finishRow(t, st, films+"/one.mkv", store.WouldTranscode, films, i64p(2_000_000_000), nil)
	// What a real run already did there: the projection's basis.
	finishRow(t, st, tv+"/s00e01.mkv", store.Done, tv, i64p(800_000_000), i64p(299_999_999))
	finishRow(t, st, tv+"/s00e02.mkv", store.Done, tv, i64p(600_000_000), i64p(250_000_000))
	finishRow(t, st, films+"/zero.mkv", store.Done, films, i64p(1_500_000_000), i64p(550_000_007))
	finishRow(t, st, films+"/skipped.mkv", store.Skipped, films, i64p(1_000_000_000), nil)
	// Rows and a retention no configured root accounts for.
	finishRow(t, st, "/retired/a.mkv", store.WouldTranscode, "/retired", i64p(123_456), nil)
	finishRow(t, st, "/retired/b.mkv", store.WouldTranscode, "/retired", nil, nil)
	// The undo window: live retentions under both roots, one restored, one unattributed.
	retain(t, st, tv+"/s00e01.mkv", 800_000_000)
	retain(t, st, tv+"/s00e02.mkv", 600_000_000)
	retain(t, st, films+"/zero.mkv", 1_500_000_000)
	retain(t, st, films+"/put-back.mkv", 5_000_000_000)
	if err := st.MarkRestored(context.Background(), films+"/put-back.mkv", 150); err != nil {
		t.Fatalf("MarkRestored: %v", err)
	}
	retain(t, st, "/retired/c.mkv", 777)

	// ONE body. Everything below reads this string and the jobs.db file, never the store.
	_, body := newSizing(t, st, []string{tv, films}, "", nil).summary(t)
	var got struct {
		Held  *int64 `json:"bytes_held_by_undo_window"`
		Roots []struct {
			Root      string `json:"root"`
			Files     *int64 `json:"candidate_files"`
			Bytes     *int64 `json:"candidate_bytes"`
			Projected *int64 `json:"projected_savings_bytes"`
			Held      *int64 `json:"bytes_held_by_undo_window"`
			Free      *int64 `json:"free_bytes"`
		} `json:"roots"`
		Unattributed struct {
			Files *int64 `json:"candidate_files"`
			Bytes *int64 `json:"candidate_bytes"`
			Held  *int64 `json:"bytes_held_by_undo_window"`
		} `json:"roots_unattributed"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(got.Roots) != 2 {
		t.Fatalf("roots has %d entries, want 2:\n%s", len(got.Roots), body)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open jobs.db directly: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	query := func(q string, args ...any) int64 {
		t.Helper()
		var v sql.NullInt64
		if err := db.QueryRow(q, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !v.Valid {
			t.Fatalf("%s answered NULL", q)
		}
		return v.Int64
	}

	for i, root := range []string{tv, films} {
		r := got.Roots[i]
		if r.Root != root {
			t.Fatalf("roots[%d].root = %q, want %q", i, r.Root, root)
		}
		const candidates = ` FROM jobs WHERE status = 'would-transcode' AND library_root = ? AND source_bytes IS NOT NULL`
		const basis = ` FROM jobs WHERE status = 'done' AND library_root = ? AND source_bytes IS NOT NULL AND output_bytes IS NOT NULL`
		want(t, root+" candidate_files", r.Files, i64p(query(`SELECT COUNT(*)`+candidates, root)))
		wantBytes := query(`SELECT COALESCE(SUM(source_bytes), 0)`+candidates, root)
		want(t, root+" candidate_bytes", r.Bytes, &wantBytes)
		// Integer division in SQLite truncates, which for these positive figures is the
		// floor; the figures are small enough that the product fits its 64-bit integer.
		want(t, root+" projected_savings_bytes", r.Projected, i64p(query(
			`SELECT (? * SUM(source_bytes - output_bytes)) / SUM(source_bytes)`+basis, wantBytes, root)))
		want(t, root+" bytes_held_by_undo_window", r.Held, i64p(query(
			`SELECT COALESCE(SUM(source_bytes), 0) FROM retained_originals
			 WHERE restored_at IS NULL AND source_path LIKE ? || '/%'`, root)))

		free, err := diskfree.Bytes(root)
		if err != nil {
			t.Fatalf("diskfree.Bytes(%s): %v", root, err)
		}
		if r.Free == nil {
			t.Fatalf("%s free_bytes is null", root)
		}
		if diff := *r.Free - int64(free); diff > 64<<20 || diff < -(64<<20) {
			t.Errorf("%s free_bytes = %d, the filesystem says %d", root, *r.Free, free)
		}
	}

	const unattributed = ` FROM jobs WHERE status = 'would-transcode' AND source_bytes IS NOT NULL
		AND (library_root IS NULL OR library_root NOT IN (?, ?))`
	want(t, "unattributed candidate_files", got.Unattributed.Files, i64p(query(`SELECT COUNT(*)`+unattributed, tv, films)))
	want(t, "unattributed candidate_bytes", got.Unattributed.Bytes,
		i64p(query(`SELECT COALESCE(SUM(source_bytes), 0)`+unattributed, tv, films)))
	want(t, "unattributed bytes_held_by_undo_window", got.Unattributed.Held, i64p(query(
		`SELECT COALESCE(SUM(source_bytes), 0) FROM retained_originals
		 WHERE restored_at IS NULL AND source_path NOT LIKE ? || '/%' AND source_path NOT LIKE ? || '/%'`, tv, films)))
	want(t, "bytes_held_by_undo_window", got.Held, i64p(query(
		`SELECT COALESCE(SUM(source_bytes), 0) FROM retained_originals WHERE restored_at IS NULL`)))

	// The fixture is not vacuous: every figure above is a non-zero number, so a body of
	// zeros and a database of zeros cannot agree their way to a pass.
	for name, v := range map[string]*int64{
		"tv candidates": got.Roots[0].Bytes, "films candidates": got.Roots[1].Bytes,
		"tv projection": got.Roots[0].Projected, "films projection": got.Roots[1].Projected,
		"tv held": got.Roots[0].Held, "films held": got.Roots[1].Held,
		"unattributed candidates": got.Unattributed.Bytes, "unattributed held": got.Unattributed.Held,
	} {
		if v == nil || *v <= 0 {
			t.Errorf("%s is not a positive figure in this fixture", name)
		}
	}
	// Worked by hand for one root, so the SQL and the handler cannot be wrong together:
	// tv candidates 1,650,000,001; basis B = 1,400,000,000 and S = 850,000,001;
	// floor(1,650,000,001 x 850,000,001 / 1,400,000,000) = 1,001,785,716.
	want(t, "tv projected_savings_bytes (by hand)", got.Roots[0].Projected, i64p(1001785716))
	want(t, "tv bytes_held_by_undo_window (by hand)", got.Roots[0].Held, i64p(1_400_000_000))
	want(t, "films bytes_held_by_undo_window (by hand)", got.Roots[1].Held, i64p(1_500_000_000))
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The ledger half of a DRY RUN's recorded decision.
//
// Before this state existed, a dry run decided a file and threw the decision away: the row
// stayed wherever the worker had parked it while deciding, so the ledger reported the run
// as having concluded nothing. Recording it means adding a status to a CLOSED vocabulary
// whose claim path classifies every member explicitly - which is a ledger-semantics change
// and not a label - so the three properties that classification has to have are pinned
// here, each against the way it fails.

// TestClaim_AWouldTranscodeRowIsClaimedByARunAllowedToTranscode is the one that guards the
// quiet destructive reading of "terminal".
//
// done and skipped are permanently terminal: Claim refuses them for ever, which is right,
// because that file HAS been dealt with. A dry-run decision has not dealt with anything -
// nothing was encoded, swapped or deleted - so classifying it the same way would mean every
// file examined during a dry run is silently excluded from every later run. The operator
// turns dry_run off, the library is never transcoded, and nothing on any surface says why.
func TestClaim_AWouldTranscodeRowIsClaimedByARunAllowedToTranscode(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const path, fp = "/lib/candidate.mkv", "1000:9"

	// A dry run decided it: claimed, then finished as the recorded decision.
	if ok, err := s.Claim(ctx, path, fp, "w0", 3); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, path, fp, WouldTranscode, &Outcome{
		SourceCodec: "h264", SourceBytes: i64(1_000_000),
	}, 3); err != nil {
		t.Fatalf("Finish(would-transcode): %v", err)
	}
	if st, _, _, err := s.Get(ctx, path, fp); err != nil || st != WouldTranscode {
		t.Fatalf("after the dry run the row is %q (err=%v), want %q", st, err, WouldTranscode)
	}

	// The same path and the same fingerprint, met by a run that IS allowed to transcode.
	ok, err := s.Claim(ctx, path, fp, "w1", 3)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !ok {
		t.Fatal("a would-transcode row was REFUSED a claim. That treats a dry run's decision as " +
			"work already completed, so every file a dry run examined would be excluded from " +
			"every later run and the library would never be transcoded")
	}
	st, failCount, _, err := s.Get(ctx, path, fp)
	if err != nil {
		t.Fatalf("Get after re-claim: %v", err)
	}
	if st != Probing {
		t.Errorf("after the re-claim the row is %q, want %q - the claim must begin a new attempt", st, Probing)
	}
	// A dry-run decision is not an attempt that went wrong, so it must not consume a retry.
	if failCount != 0 {
		t.Errorf("re-claiming a would-transcode row raised fail_count to %d; a recorded decision "+
			"is not a failed attempt and must never spend a retry", failCount)
	}
	// And the claim cleared the previous attempt's proof, as it does for every other claim.
	rows, err := s.List(ctx, []Status{Probing}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List(probing) returned %d rows, want 1", len(rows))
	}
	if rows[0].Outcome.SourceCodec != "" || rows[0].Outcome.SourceBytes != nil {
		t.Errorf("the re-claimed row still carries the dry run's recorded facts (%+v); claiming "+
			"begins a new attempt and clears the proof of the old one", rows[0].Outcome)
	}

	// The transcode then completes on the ordinary path.
	if err := s.Finish(ctx, path, fp, Done, &Outcome{
		Encoder: "cpu", SourceBytes: i64(1_000_000), OutputBytes: i64(250_000),
	}, 3); err != nil {
		t.Fatalf("Finish(done): %v", err)
	}
	if st, _, _, err := s.Get(ctx, path, fp); err != nil || st != Done {
		t.Fatalf("after the real run the row is %q (err=%v), want %q", st, err, Done)
	}
}

// TestClaim_TwoDryRunsOverAnUnchangedFileReportOneCandidate. A repeated dry run must not
// inflate the candidate count: an operator runs one to SIZE the job, and a figure that
// doubles every time they look at it is a figure they cannot act on.
//
// The row is keyed (path, fingerprint), so an unchanged file is one row - but only if the
// second scan can reach it at all. A state that refused the claim would leave the second
// run unable to re-decide the file, and a state that inserted rather than updated would
// count it twice; this asserts the count from the SUMMARY and the outcomes breakdown, which
// are the two figures an operator actually reads.
func TestClaim_TwoDryRunsOverAnUnchangedFileReportOneCandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const path, fp = "/lib/unchanged.mkv", "2000:7"

	for pass := 1; pass <= 2; pass++ {
		ok, err := s.Claim(ctx, path, fp, "w0", 3)
		if err != nil {
			t.Fatalf("dry-run pass %d: Claim: %v", pass, err)
		}
		if !ok {
			t.Fatalf("dry-run pass %d could not claim the file, so it could not re-decide it", pass)
		}
		if err := s.Finish(ctx, path, fp, WouldTranscode, &Outcome{
			SourceCodec: "h264", SourceBytes: i64(4242),
		}, 3); err != nil {
			t.Fatalf("dry-run pass %d: Finish: %v", pass, err)
		}
	}

	sum, err := s.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got := sum[WouldTranscode]; got != 1 {
		t.Errorf("two dry runs over one unchanged file report %d would-transcode rows, want 1 "+
			"(a repeated dry run must not inflate the candidate count)", got)
	}
	rows, err := s.List(ctx, []Status{WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List(would-transcode) returned %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Outcome.SourceCodec != "h264" || rows[0].Outcome.SourceBytes == nil || *rows[0].Outcome.SourceBytes != 4242 {
		t.Errorf("the surviving row lost what the second pass recorded: %+v", rows[0].Outcome)
	}
	for _, b := range s.Aggregates(ctx).Outcomes.Buckets {
		if b.Key == string(WouldTranscode) && b.Count != 1 {
			t.Errorf("the outcomes breakdown counts %d would-transcode rows, want 1", b.Count)
		}
	}
}

// TestClaim_AnUnrecognisedStatusIsStillRefused. Adding a member to the vocabulary must not
// widen the fail-safe default that catches every non-member.
//
// The claim path classifies each status explicitly and REFUSES anything it does not know,
// naming the status and the path - a row written by a newer build, or by a corruption, is
// never handed to the encoder on the strength of a word this binary cannot read. That
// default is one `default:` arm away from being turned into "claim it", and the change that
// would do it is exactly the shape of this one.
func TestClaim_AnUnrecognisedStatusIsStillRefused(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const path, fp = "/lib/from-the-future.mkv", "3000:1"

	// A row this build's vocabulary does not contain, written straight into the table -
	// which is what a newer binary, or a corrupted value, leaves behind.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at)
		 VALUES (?, ?, 'would-transmogrify', 0, NULL, 1000)`, path, fp); err != nil {
		t.Fatalf("seed the unrecognised row: %v", err)
	}

	ok, err := s.Claim(ctx, path, fp, "w0", 3)
	if ok {
		t.Fatal("a row carrying a status this build does not recognise was CLAIMED. The fail-safe " +
			"default is what stops a word this binary cannot read from handing a file to the encoder")
	}
	if err == nil {
		t.Fatal("claiming an unrecognised status returned no error; the refusal has to SAY it refused")
	}
	if !strings.Contains(err.Error(), "would-transmogrify") {
		t.Errorf("the refusal does not name the status it refused: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the path it refused: %v", err)
	}

	// And the row was left exactly as it was found.
	st, _, exists, err := s.Get(ctx, path, fp)
	if err != nil || !exists {
		t.Fatalf("Get after the refusal: st=%q exists=%v err=%v", st, exists, err)
	}
	if string(st) != "would-transmogrify" {
		t.Errorf("the refused row was rewritten to %q; a refusal changes nothing", st)
	}
}

// ---- the migration, against a database written by the PREVIOUS schema ----------

// v7Schema is the schema EXACTLY as it shipped BEFORE this change: v1's table, v2's
// outcome columns, v3's indexes, GATE-4's three columns, UNDO-6's retention table,
// LEDGER-5's totals and index, FILESYSTEM-1's guard columns and incidents table, and the
// v7 version stamp.
//
// Frozen, for the reason every seed here is frozen: it is the shape of every jobs.db in
// the world at the moment this change lands, and a test that migrates from a schema nobody
// ever ran proves nothing. Do NOT update it when the schema changes.
const v7Schema = v4Schema + `
CREATE TABLE IF NOT EXISTS retained_originals (
	source_path         TEXT NOT NULL PRIMARY KEY,
	swapped_path        TEXT NOT NULL,
	retained_path       TEXT NOT NULL,
	source_bytes        INTEGER NOT NULL,
	swapped_fingerprint TEXT NOT NULL,
	retained_at         INTEGER NOT NULL,
	expires_at          INTEGER NOT NULL,
	restored_at         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_retained_swapped ON retained_originals(swapped_path);
CREATE INDEX IF NOT EXISTS idx_retained_expires ON retained_originals(restored_at, expires_at);
CREATE TABLE IF NOT EXISTS ledger_totals (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	reclaimed_pruned INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO ledger_totals (id, reclaimed_pruned) VALUES (1, 0);
CREATE INDEX IF NOT EXISTS idx_jobs_status_updated ON jobs(status, updated_at);
ALTER TABLE jobs ADD COLUMN guard_attributes       TEXT;
ALTER TABLE jobs ADD COLUMN guard_time_resolution  TEXT;
ALTER TABLE jobs ADD COLUMN guard_residual_window  TEXT;
ALTER TABLE jobs ADD COLUMN swap_cause             TEXT;

CREATE TABLE IF NOT EXISTS swap_incidents (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	source_path        TEXT NOT NULL,
	source_fingerprint TEXT NOT NULL,
	replacement_path   TEXT NOT NULL,
	source_attrs       TEXT NOT NULL,
	replacement_attrs  TEXT NOT NULL,
	observed_attrs     TEXT,
	outcome            TEXT NOT NULL,
	swap_error         TEXT,
	swap_cause         TEXT,
	storage_class      TEXT,
	storage_type       TEXT,
	created_at         INTEGER NOT NULL,
	resolution         TEXT,
	resolved_by        TEXT,
	resolved_at        INTEGER,
	observed_source      TEXT,
	observed_replacement TEXT,
	disposition_source      TEXT,
	disposition_replacement TEXT,
	removal_error      TEXT
);
CREATE INDEX IF NOT EXISTS idx_incidents_parked ON swap_incidents(outcome, resolution);
CREATE INDEX IF NOT EXISTS idx_incidents_excluded ON swap_incidents(replacement_path)
	WHERE disposition_replacement IS NULL OR disposition_replacement = 'retained-excluded';
PRAGMA user_version = 7;
`

// seedV7 writes a real pre-change database at path, with rows that CARRY OUTCOMES and a
// retention and an incident beside them. A fixture of empty rows could not tell "the
// migration kept what those rows recorded" apart from "the migration kept a row".
func seedV7(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v7 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(v7Schema); err != nil {
		t.Fatalf("create v7 schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			encoder, vmaf_mean, vmaf_min, vmaf_model, vmaf_pix_fmt, vmaf_chroma, vmaf_chroma_metric,
			source_bytes, output_bytes, encode_ms,
			guard_attributes, guard_time_resolution, guard_residual_window)
		 VALUES ('/lib/old-done.mkv', '10:100', 'done', 0, NULL, 1000,
			'cpu', 97.25, 88.5, 'version=vmaf_v0.6.1', 'yuv420p10le', 41.5, 'psnr_cb/psnr_cr min (dB)',
			5000000, 2000000, 12345,
			'size,mtime', '1s', 'residual-window-local')`); err != nil {
		t.Fatalf("seed done row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-skipped.mkv', '20:200', 'skipped', 0, NULL, 1001, 'interlaced')`); err != nil {
		t.Fatalf("seed skipped row: %v", err)
	}
	// A PARKED job: the one row in the whole ledger that is waiting for a human, and the
	// one whose loss would be least recoverable.
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-parked.mkv', '30:300', 'indeterminate', 0, NULL, 1002, 'rename failed: EIO')`); err != nil {
		t.Fatalf("seed parked row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO swap_incidents (source_path, source_fingerprint, replacement_path,
			source_attrs, replacement_attrs, observed_attrs, outcome, swap_error, created_at)
		 VALUES ('/lib/old-parked.mkv', '30:300', '/lib/old-parked.__holdfast-replacement__',
			'300:9', '120:9', '', 'indeterminate', 'rename failed: EIO', 1002)`); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO retained_originals (source_path, swapped_path, retained_path, source_bytes,
			swapped_fingerprint, retained_at, expires_at, restored_at)
		 VALUES ('/lib/old-done.mkv', '/lib/old-done.mkv', '/lib/.holdfast-undo/old-done', 5000000,
			'2000000:1000', 1000, 9000, NULL)`); err != nil {
		t.Fatalf("seed retention: %v", err)
	}
	if _, err := db.Exec(`UPDATE ledger_totals SET reclaimed_pruned = 777 WHERE id = 1`); err != nil {
		t.Fatalf("seed carry-forward: %v", err)
	}

	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read seeded user_version: %v", err)
	}
	if ver != 7 {
		t.Fatalf("seeded database is at version %d, want 7 - it is not a pre-change database", ver)
	}
	if hasColumn(t, db, "source_codec") {
		t.Fatal("seeded v7 database already has source_codec - the fixture is wrong")
	}
}

// TestMigrate_PreSourceCodecDatabaseOnDiskGainsTheColumnUnbackfilled is the anti-vacuity
// proof for this change's schema step, and it is the test that matters.
//
// A FRESH-DATABASE test would pass against the very bug: v1's `CREATE TABLE IF NOT EXISTS`
// matches on the table's NAME, so a fresh file gets every column either way while a
// database that already exists silently gets none of them - and the process then dies
// later, on a live install, on the first query naming a column that is not there. So the
// fixture is a REAL on-disk database written by the previous schema, with rows in it.
//
// Three things are asserted, and each is a way the migration could destroy evidence:
//
//  1. every recorded fact on every existing row survives, including the parked job and its
//     incident, which is the ledger's only record of a swap nobody can account for;
//  2. the new fact reads as NOT RECORDED rather than backfilled. Every one of those rows
//     was written by a build that probed a codec and stored none, so a DEFAULT here would
//     put a codec on rows about files that may no longer exist;
//  3. the migrated database is WRITABLE through the new column - which is exactly what a
//     silent no-op is not.
func TestMigrate_PreSourceCodecDatabaseOnDiskGainsTheColumnUnbackfilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV7(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v7 database must migrate it in place, not fail: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version after migration = %d, want %d", got, want)
	}
	if !hasColumn(t, s.db, "source_codec") {
		t.Fatal("the migrated database has no source_codec column - the migration was a silent no-op, " +
			"and this build would die on the first query naming it")
	}

	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List after migration: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("migration lost rows: got %d, want 3 (%+v)", len(rows), rows)
	}
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}

	// 1. Nothing recorded was dropped or rewritten.
	done := byPath["/lib/old-done.mkv"]
	if done.Status != Done || done.Outcome.Encoder != "cpu" || done.Outcome.VmafModel != "version=vmaf_v0.6.1" ||
		done.Outcome.VmafPixFmt != "yuv420p10le" || done.Outcome.VmafChromaMetric != "psnr_cb/psnr_cr min (dB)" {
		t.Errorf("the pre-existing done row was mangled: %+v", done)
	}
	if done.Outcome.VmafMean == nil || *done.Outcome.VmafMean != 97.25 ||
		done.Outcome.VmafMin == nil || *done.Outcome.VmafMin != 88.5 ||
		done.Outcome.VmafChroma == nil || *done.Outcome.VmafChroma != 41.5 {
		t.Errorf("the pre-existing measurements were lost: %+v", done.Outcome)
	}
	if done.Outcome.SourceBytes == nil || *done.Outcome.SourceBytes != 5000000 ||
		done.Outcome.OutputBytes == nil || *done.Outcome.OutputBytes != 2000000 {
		t.Errorf("the pre-existing sizes were lost: %+v", done.Outcome)
	}
	if done.Outcome.GuardResidualWindow != ResidualWindowLocal || done.Outcome.GuardAttributes != "size,mtime" {
		t.Errorf("the pre-existing guard record was lost: %+v", done.Outcome)
	}
	if got := byPath["/lib/old-skipped.mkv"]; got.Outcome.Reason != "interlaced" {
		t.Errorf("the pre-existing skip reason was lost: %+v", got.Outcome)
	}
	if got := byPath["/lib/old-parked.mkv"]; got.Status != Indeterminate {
		t.Errorf("the parked job is now %q - that row is the ledger's only record of a swap "+
			"nobody can account for", got.Status)
	}
	parked, err := s.ParkedIncidents(ctx)
	if err != nil {
		t.Fatalf("ParkedIncidents: %v", err)
	}
	if len(parked) != 1 || parked[0].SourcePath != "/lib/old-parked.mkv" ||
		parked[0].ReplacementPath != "/lib/old-parked.__holdfast-replacement__" {
		t.Errorf("the parked incident did not survive the migration: %+v", parked)
	}
	if held, err := s.HeldByUndoWindow(ctx); err != nil {
		t.Fatalf("HeldByUndoWindow: %v", err)
	} else if held != 5000000 {
		t.Errorf("the retained original's bytes read as %d after the migration, want 5000000", held)
	}
	// The lifetime total is the live rows plus the carry-forward, both untouched.
	if total, err := s.ReclaimedTotal(ctx); err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	} else if total != 3000000+777 {
		t.Errorf("the lifetime reclaimed total reads %d after the migration, want %d", total, 3000000+777)
	}

	// 2. The NEW fact is NOT RECORDED on every pre-existing row, never backfilled.
	for _, j := range rows {
		if j.Outcome.SourceCodec != "" {
			t.Errorf("%s was BACKFILLED with a source codec (%q). Nobody recorded one for that row, "+
				"and the file it describes may not exist any more", j.Path, j.Outcome.SourceCodec)
		}
	}

	// 3. And the migrated database is WRITABLE through the new column.
	if ok, err := s.Claim(ctx, "/lib/fresh.mkv", "50:500", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim on a migrated database: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/fresh.mkv", "50:500", WouldTranscode, &Outcome{
		SourceCodec: "h264", SourceBytes: i64(987654),
	}, 3); err != nil {
		t.Fatalf("Finish on a migrated database: %v", err)
	}
	after, err := s.List(ctx, []Status{WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("List(would-transcode) returned %d rows, want 1", len(after))
	}
	if after[0].Outcome.SourceCodec != "h264" ||
		after[0].Outcome.SourceBytes == nil || *after[0].Outcome.SourceBytes != 987654 {
		t.Errorf("a row written AFTER the migration lost its recorded facts: %+v", after[0].Outcome)
	}
}

// TestWouldTranscode_IsTerminalAndIsCountedAsOne. The state is a partition member on every
// surface that partitions the vocabulary, so the two questions the rest of the program asks
// of a status have to answer consistently: it is terminal, and it is not active.
func TestWouldTranscode_IsTerminalAndIsCountedAsOne(t *testing.T) {
	if !WouldTranscode.Terminal() {
		t.Error("would-transcode does not report as terminal; the decision is taken and the row is " +
			"its record, so it belongs with the finished states and not with the work in hand")
	}
	if WouldTranscode.Active() {
		t.Error("would-transcode reports as ACTIVE, which would make RecoverStale reset it to " +
			"pending on the next startup and throw the recorded decision away")
	}
}

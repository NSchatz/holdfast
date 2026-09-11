package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The TRANSCODE-13 migration proof.
//
// The defect being fixed is a SILENT one, and that shapes these tests. The old schema
// was a bare `CREATE TABLE IF NOT EXISTS jobs (...)` with no version stamp, so adding a
// column to it is a no-op against any database that already exists — the table name
// matches, the shape does not, and nobody is told. A test that only ever opens a FRESH
// database would pass vacuously against exactly that bug: a fresh file gets the new
// columns either way, because the CREATE TABLE names them.
//
// So the load-bearing test here (TestMigrate_V0DatabaseOnDiskGainsTheOutcomeColumns)
// seeds a REAL pre-migration database — the literal v0 DDL, with rows in it — and
// proves that opening it with this build migrates it in place and keeps every row. That
// is the only test that would have caught the bug.

// v0Schema is the schema EXACTLY as it shipped in TRANSCODE-5, before versioning
// existed. Frozen here on purpose: it is the shape of every jobs.db in the world at the
// moment this phase lands, and a test that "migrates" from a schema nobody ever ran is
// proving nothing. Do not update it when the schema changes — that is the point of it.
const v0Schema = `
CREATE TABLE IF NOT EXISTS jobs (
	path        TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	status      TEXT NOT NULL,
	fail_count  INTEGER NOT NULL DEFAULT 0,
	worker      TEXT,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (path, fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
`

// seedV0 writes a database at path carrying the v0 schema, the v0 user_version (0 — the
// default, never stamped) and some rows, then closes it. This is a real on-disk legacy
// database, not a mock of one.
func seedV0(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v0 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(v0Schema); err != nil {
		t.Fatalf("create v0 schema: %v", err)
	}
	for _, r := range []struct {
		path, fp, status string
		failCount        int
		updatedAt        int64
	}{
		{"/lib/done.mkv", "10:100", "done", 0, 1000},
		{"/lib/skipped.mkv", "20:200", "skipped", 0, 1001},
		{"/lib/failed.mkv", "30:300", "failed", 2, 1002},
		{"/lib/pending.mkv", "40:400", "pending", 0, 1003},
	} {
		if _, err := db.Exec(
			`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at) VALUES (?, ?, ?, ?, NULL, ?)`,
			r.path, r.fp, r.status, r.failCount, r.updatedAt); err != nil {
			t.Fatalf("seed row %s: %v", r.path, err)
		}
	}

	// Sanity: the seeded database really is at version 0 and really lacks the columns.
	// Without this the test could pass by accident against a database that was already
	// migrated, which would make it vacuous in exactly the way it exists to avoid.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read seeded user_version: %v", err)
	}
	if ver != 0 {
		t.Fatalf("seeded database is at version %d, want 0 — it is not a v0 database", ver)
	}
	if hasColumn(t, db, "reason") {
		t.Fatal("seeded v0 database already has a `reason` column — the fixture is wrong")
	}
}

// hasColumn reports whether jobs has a column of that name, read from SQLite itself
// rather than from our own belief about the schema.
func hasColumn(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('jobs')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if got == name {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pragma_table_info rows: %v", err)
	}
	return false
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// THE test for this phase. An EXISTING on-disk database — the pre-versioning schema,
// with real rows in it — must actually gain the new columns when this build opens it,
// and must not lose a single row doing so.
//
// A fresh-schema test would pass even with a no-op migration, which is precisely the
// bug: `CREATE TABLE IF NOT EXISTS` with an extra column silently does nothing to a
// database that already has the table, and the process then dies on the first query
// naming the column that isn't there.
func TestMigrate_V0DatabaseOnDiskGainsTheOutcomeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV0(t, path)

	// Open with the real production path — this is what an upgraded install does.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v0 database must migrate it, not fail: %v", err)
	}
	defer func() { _ = s.Close() }()

	// 1. It is now at the current version.
	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version after migration = %d, want %d", got, want)
	}

	// 2. Every outcome column actually exists — asked of SQLite, not assumed.
	for _, col := range headColumns {
		if !hasColumn(t, s.db, col) {
			t.Errorf("migrated database is missing column %q - the migration was a silent no-op", col)
		}
	}

	// 3. Every seeded row survived, with its values intact.
	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List after migration: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("migration lost rows: got %d, want 4 (%+v)", len(rows), rows)
	}
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}
	if j := byPath["/lib/failed.mkv"]; j.Status != Failed || j.FailCount != 2 || j.Fingerprint != "30:300" || j.UpdatedAt != 1002 {
		t.Errorf("pre-migration row mangled: %+v", j)
	}
	if j := byPath["/lib/done.mkv"]; j.Status != Done {
		t.Errorf("pre-migration done row mangled: %+v", j)
	}

	// 4. And the pre-existing rows read as NOT RECORDED — nil, not a fabricated zero.
	// This is the fail-safe from the roadmap: a row written before the columns existed
	// has no fidelity data, and inventing a 0 for it would be inventing evidence about
	// a swap nobody measured.
	for _, j := range rows {
		o := j.Outcome
		if o.VmafMean != nil || o.VmafMin != nil || o.VmafChroma != nil ||
			o.SourceBytes != nil || o.OutputBytes != nil || o.EncodeMs != nil {
			t.Errorf("a pre-migration row must read as not-recorded (nil), got %+v for %s", o, j.Path)
		}
		if o.Reason != "" || o.Encoder != "" || o.VmafModel != "" ||
			o.VmafPixFmt != "" || o.VmafChromaMetric != "" {
			t.Errorf("a pre-migration row must carry no reason/encoder/model/format/metric, got %+v for %s", o, j.Path)
		}
	}

	// 5. And the migrated database is WRITABLE through the new columns — the migration
	// is not merely cosmetic. (Without the ALTER this would fail with "no such column",
	// which is exactly how the silent-no-op bug surfaces on a live install: not at
	// startup, but later, on a query.)
	if ok, err := s.Claim(ctx, "/lib/fresh.mkv", "50:500", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim on a migrated database: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/fresh.mkv", "50:500", Done, &Outcome{
		Encoder: "cpu", VmafMean: f64(97.0), VmafMin: f64(90.0), VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: i64(1000), OutputBytes: i64(400), EncodeMs: i64(999),
	}, 3); err != nil {
		t.Fatalf("Finish on a migrated database: %v", err)
	}
	got, err := s.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, j := range got {
		if j.Path != "/lib/fresh.mkv" {
			continue
		}
		if j.Outcome.VmafMin == nil || *j.Outcome.VmafMin != 90.0 || j.Outcome.SourceBytes == nil {
			t.Errorf("a row written AFTER the migration lost its proof: %+v", j.Outcome)
		}
	}
}

// Migrating is idempotent: opening an already-migrated database runs nothing, changes
// nothing, and loses nothing. (A daemon restarts; a migration that only worked once
// would be a migration that broke the second boot.)
func TestMigrate_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV0(t, path)

	for i := 1; i <= 3; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		rows, err := s.List(context.Background(), nil, 0)
		if err != nil {
			t.Fatalf("List #%d: %v", i, err)
		}
		if len(rows) != 4 {
			t.Fatalf("Open #%d: row count = %d, want 4 — a re-run of the migration is destroying data", i, len(rows))
		}
		if got, want := userVersion(t, path), schemaVersion(); got != want {
			t.Fatalf("Open #%d: user_version = %d, want %d", i, got, want)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
}

// applyMigration must be RE-ENTRANT: a migration another writer already applied is a
// no-op, not an error.
//
// This is what the version re-read INSIDE the transaction buys. migrate() reads
// user_version before it takes the write lock, so between that read and the lock the
// step may already have run — and then our ALTER would die on "duplicate column name",
// turning a benign race into a startup failure. Re-reading under the lock makes
// check-and-apply atomic, so losing the race just means the work is already done.
//
// Driven directly rather than through a goroutine race, because a test that only fails
// sometimes is a test that gets deleted: here the "other writer" has demonstrably
// already run (the database is fully migrated), and we re-apply the same step on top.
func TestApplyMigration_IsANoOpWhenAlreadyApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV0(t, path)

	s, err := Open(path) // migrates to the current version
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	// Re-apply EVERY shipped migration on top of an already-migrated database, exactly
	// as a racing process would. Without the in-transaction re-read, migration 2's
	// `ALTER TABLE ... ADD COLUMN reason` fails with "duplicate column name".
	for i, m := range migrations {
		if err := applyMigration(ctx, s.db, i+1, m); err != nil {
			t.Fatalf("re-applying migration %d (%s) must be a no-op, got: %v", i+1, m.name, err)
		}
	}

	// And nothing was disturbed: same version, same rows.
	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version = %d, want %d", got, want)
	}
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 4 {
		t.Errorf("row count = %d, want 4 — a re-applied migration disturbed the data", len(rows))
	}
}

// A fresh database gets the full schema and the version stamp in one go — the other
// half of the "v1 must be a no-op on a legacy database AND a real create on a new one"
// contract.
func TestMigrate_FreshDatabaseIsStampedAndComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("fresh user_version = %d, want %d", got, want)
	}
	for _, col := range headColumns {
		if !hasColumn(t, s.db, col) {
			t.Errorf("fresh database is missing column %q", col)
		}
	}
}

// headColumns is every outcome column the current schema carries, in one place so a
// migration that appends a column has exactly one list to extend rather than three
// to keep in step.
var headColumns = []string{
	"reason", "encoder", "vmaf_mean", "vmaf_min", "vmaf_model",
	"vmaf_pix_fmt", "vmaf_chroma", "vmaf_chroma_metric",
	"source_codec", "source_bytes", "output_bytes", "encode_ms",
	"decision_inputs",
}

// A database from the FUTURE is a startup REFUSAL, not a silent downgrade. Finish writes
// the full outcome column set, so an older binary running against a newer schema would
// happily overwrite outcomes it cannot even see.
func TestMigrate_RefusesADatabaseFromTheFuture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Stamp it one version past what this build knows.
	future := schemaVersion() + 1
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("Open must REFUSE a database newer than this build (a silent downgrade would discard data it cannot see)")
	}
	// And it must not have "helpfully" rewritten the version on its way out.
	if got := userVersion(t, path); got != future {
		t.Errorf("a refused open must not touch the database; user_version = %d, want %d", got, future)
	}
}

// A migration that fails must leave the version UNCHANGED — the version and the shape
// move together or not at all. Otherwise a database claims a schema it does not have,
// which is the one way a versioned schema can still lie to you.
func TestApplyMigration_FailureLeavesVersionUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	bad := migration{name: "deliberately broken", sql: `THIS IS NOT SQL;`}
	if err := applyMigration(context.Background(), db, 1, bad); err == nil {
		t.Fatal("a broken migration must return an error")
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 0 {
		t.Errorf("user_version = %d after a FAILED migration, want 0 — the stamp escaped its transaction", v)
	}
}

// ---- GATE-4: the comparison-format and chroma columns -------------------------

// v3Schema is the schema EXACTLY as it shipped BEFORE GATE-4 - v1's table, v2's
// outcome columns, v3's indexes, and the v3 version stamp. Frozen here for the same
// reason v0Schema is: it is the shape of every jobs.db in the world at the moment this
// phase lands, and a migration test that starts from a schema nobody ever ran proves
// nothing. Do NOT update it when the schema changes.
const v3Schema = `
CREATE TABLE IF NOT EXISTS jobs (
	path        TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	status      TEXT NOT NULL,
	fail_count  INTEGER NOT NULL DEFAULT 0,
	worker      TEXT,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (path, fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
ALTER TABLE jobs ADD COLUMN reason       TEXT;
ALTER TABLE jobs ADD COLUMN encoder      TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_mean    REAL;
ALTER TABLE jobs ADD COLUMN vmaf_min     REAL;
ALTER TABLE jobs ADD COLUMN vmaf_model   TEXT;
ALTER TABLE jobs ADD COLUMN source_bytes INTEGER;
ALTER TABLE jobs ADD COLUMN output_bytes INTEGER;
ALTER TABLE jobs ADD COLUMN encode_ms    INTEGER;
CREATE INDEX IF NOT EXISTS idx_jobs_status_reason ON jobs(status, reason);
CREATE INDEX IF NOT EXISTS idx_jobs_outcome ON jobs(status, source_bytes, output_bytes, encode_ms, vmaf_mean, vmaf_min);
PRAGMA user_version = 3;
`

// seedV3 writes a real pre-GATE-4 database at path: the frozen v3 schema, its version
// stamp, and rows that CARRY OUTCOMES - a done row with a real VMAF pair, and a
// skipped row with a guard token. The outcomes matter: the criterion is that migrating
// keeps what those rows recorded while leaving the NEW fields unrecorded, and a
// fixture of empty rows could not tell those two apart.
func seedV3(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(v3Schema); err != nil {
		t.Fatalf("create v3 schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			encoder, vmaf_mean, vmaf_min, vmaf_model, source_bytes, output_bytes, encode_ms)
		 VALUES ('/lib/old-done.mkv', '10:100', 'done', 0, NULL, 1000,
			'cpu', 97.25, 88.5, 'version=vmaf_v0.6.1', 5000000, 2000000, 12345)`); err != nil {
		t.Fatalf("seed done row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-skipped.mkv', '20:200', 'skipped', 0, NULL, 1001, 'already-target-codec')`); err != nil {
		t.Fatalf("seed skipped row: %v", err)
	}

	// Sanity: the fixture really is a v3 database that really lacks the new columns.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read seeded user_version: %v", err)
	}
	if ver != 3 {
		t.Fatalf("seeded database is at version %d, want 3 - it is not a pre-GATE-4 database", ver)
	}
	for _, col := range []string{"vmaf_pix_fmt", "vmaf_chroma", "vmaf_chroma_metric"} {
		if hasColumn(t, db, col) {
			t.Fatalf("seeded v3 database already has %q - the fixture is wrong", col)
		}
	}
}

// TestMigrate_PreGate4DatabaseGainsTheNewColumnsUnbackfilled is GATE-4's thirteenth
// acceptance criterion. A jobs.db written by the SHIPPED build (v3) must migrate
// forward in place, keep every outcome it already recorded, and read as NOT RECORDED
// for the three new fields - never backfilled with a value.
//
// The backfill half is the one that matters, and it is why the migration adds the
// columns NULLABLE with NO DEFAULT. A `DEFAULT 0` on vmaf_chroma would hand every
// pre-existing done row a chroma measurement nobody took, about swaps that already
// happened and cannot be re-examined, in the one table whose entire job is to be
// evidence. A `DEFAULT ”` on vmaf_pix_fmt would be the same lie about which pixels
// were compared.
func TestMigrate_PreGate4DatabaseGainsTheNewColumnsUnbackfilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV3(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v3 database must migrate it, not fail: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version after migration = %d, want %d", got, want)
	}
	for _, col := range headColumns {
		if !hasColumn(t, s.db, col) {
			t.Errorf("migrated database is missing column %q - the migration was a silent no-op", col)
		}
	}

	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List after migration: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("migration lost rows: got %d, want 2 (%+v)", len(rows), rows)
	}
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}

	// 1. What the v3 rows already recorded survived, exactly.
	done := byPath["/lib/old-done.mkv"]
	if done.Status != Done || done.Outcome.Encoder != "cpu" || done.Outcome.VmafModel != "version=vmaf_v0.6.1" {
		t.Errorf("the pre-existing done row was mangled: %+v", done)
	}
	if done.Outcome.VmafMean == nil || *done.Outcome.VmafMean != 97.25 ||
		done.Outcome.VmafMin == nil || *done.Outcome.VmafMin != 88.5 {
		t.Errorf("the pre-existing VMAF pair was lost: mean=%v min=%v",
			done.Outcome.VmafMean, done.Outcome.VmafMin)
	}
	if skipped := byPath["/lib/old-skipped.mkv"]; skipped.Outcome.Reason != "already-target-codec" {
		t.Errorf("the pre-existing skip reason was lost: %+v", skipped.Outcome)
	}

	// 2. And the NEW fields read as not recorded on every pre-existing row.
	for _, j := range rows {
		o := j.Outcome
		if o.VmafChroma != nil {
			t.Errorf("%s was BACKFILLED with a chroma value (%v) - that is a measurement "+
				"nobody took, about a swap that already happened", j.Path, *o.VmafChroma)
		}
		if o.VmafPixFmt != "" || o.VmafChromaMetric != "" {
			t.Errorf("%s was BACKFILLED with a comparison format (%q) or metric (%q)",
				j.Path, o.VmafPixFmt, o.VmafChromaMetric)
		}
	}

	// 3. The migrated database is WRITABLE through the new columns. Without the ALTER
	// this fails with "no such column" - which is how the silent-no-op bug surfaces on
	// a live install: not at startup, but later, on a query.
	if ok, err := s.Claim(ctx, "/lib/fresh.mkv", "50:500", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim on a migrated database: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/fresh.mkv", "50:500", Done, &Outcome{
		Encoder: "cpu", VmafMean: f64(98.4), VmafMin: f64(96.1), VmafModel: "version=vmaf_v0.6.1",
		VmafPixFmt: "yuv420p10le", VmafChroma: f64(41.2), VmafChromaMetric: "psnr_cb/psnr_cr min (dB)",
	}, 3); err != nil {
		t.Fatalf("Finish on a migrated database: %v", err)
	}
	after, err := s.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, j := range after {
		if j.Path != "/lib/fresh.mkv" {
			continue
		}
		if j.Outcome.VmafChroma == nil || *j.Outcome.VmafChroma != 41.2 || j.Outcome.VmafPixFmt != "yuv420p10le" {
			t.Errorf("a row written AFTER the migration lost its new proof: %+v", j.Outcome)
		}
	}
}

// ---- UNDO-6: the retained-originals table -------------------------------------

// v4Schema is the schema EXACTLY as it shipped BEFORE UNDO-6 - v1's table, v2's
// outcome columns, v3's indexes, GATE-4's three columns, and the v4 version stamp.
// Frozen for the same reason v0Schema and v3Schema are: it is the shape of every
// jobs.db in the world at the moment this phase lands. Do NOT update it when the
// schema changes.
const v4Schema = v3Schema + `
ALTER TABLE jobs ADD COLUMN vmaf_pix_fmt       TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_chroma        REAL;
ALTER TABLE jobs ADD COLUMN vmaf_chroma_metric TEXT;
PRAGMA user_version = 4;
`

// seedV4 writes a real pre-UNDO-6 database at path, with rows that CARRY OUTCOMES. A
// fixture of empty rows could not tell "the migration kept what was recorded" apart
// from "the migration kept a row".
func seedV4(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(v4Schema); err != nil {
		t.Fatalf("create v4 schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			encoder, vmaf_mean, vmaf_min, vmaf_model, vmaf_pix_fmt, vmaf_chroma, vmaf_chroma_metric,
			source_bytes, output_bytes, encode_ms)
		 VALUES ('/lib/old-done.mkv', '10:100', 'done', 0, NULL, 1000,
			'cpu', 97.25, 88.5, 'version=vmaf_v0.6.1', 'yuv420p10le', 41.5, 'psnr_cb/psnr_cr min (dB)',
			5000000, 2000000, 12345)`); err != nil {
		t.Fatalf("seed done row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-skipped.mkv', '20:200', 'skipped', 0, NULL, 1001, 'hardlinked')`); err != nil {
		t.Fatalf("seed skipped row: %v", err)
	}

	// Sanity: the fixture really is a v4 database that really lacks the new table.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read seeded user_version: %v", err)
	}
	if ver != 4 {
		t.Fatalf("seeded database is at version %d, want 4 - it is not a pre-UNDO-6 database", ver)
	}
	if hasTable(t, db, "retained_originals") {
		t.Fatal("seeded v4 database already has retained_originals - the fixture is wrong")
	}
}

// hasTable reports whether a table of that name exists, read from SQLite itself
// rather than from our own belief about the schema.
func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	return found == name
}

// TestMigrate_PreUndoDatabaseGainsTheRetentionTableWithNoFabricatedRetentions is
// UNDO-6's sixteenth criterion. A jobs.db written by the SHIPPED build (v4) must
// migrate forward in place, keep every row and every outcome those rows recorded, and
// read as having NO retained original - never a fabricated one.
//
// The second half is the one that matters. Every swap recorded in a pre-UNDO-6 ledger
// happened with no undo window at all: its original was destroyed by the rename, and
// nothing anywhere retains it. A row that read back as "something is retained for this
// path" would be the ledger promising a restore that is physically impossible, which
// is why the retention lives in its own table with no backfill rather than as columns
// with defaults on the jobs row.
func TestMigrate_PreUndoDatabaseGainsTheRetentionTableWithNoFabricatedRetentions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV4(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v4 database must migrate it, not fail: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version after migration = %d, want %d", got, want)
	}
	if !hasTable(t, s.db, "retained_originals") {
		t.Fatal("migrated database has no retained_originals table - the migration was a silent no-op")
	}

	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List after migration: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("migration lost rows: got %d, want 2 (%+v)", len(rows), rows)
	}
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}

	// 1. Nothing was rewritten: every value the v4 rows carried is still there.
	done := byPath["/lib/old-done.mkv"]
	if done.Status != Done || done.Outcome.Encoder != "cpu" || done.Outcome.VmafModel != "version=vmaf_v0.6.1" ||
		done.Outcome.VmafPixFmt != "yuv420p10le" {
		t.Errorf("the pre-existing done row was mangled: %+v", done)
	}
	if done.Outcome.VmafMean == nil || *done.Outcome.VmafMean != 97.25 ||
		done.Outcome.VmafMin == nil || *done.Outcome.VmafMin != 88.5 ||
		done.Outcome.VmafChroma == nil || *done.Outcome.VmafChroma != 41.5 {
		t.Errorf("the pre-existing measurements were lost: %+v", done.Outcome)
	}
	if done.Outcome.SourceBytes == nil || *done.Outcome.SourceBytes != 5000000 {
		t.Errorf("the pre-existing sizes were lost: %+v", done.Outcome)
	}
	if skipped := byPath["/lib/old-skipped.mkv"]; skipped.Outcome.Reason != "hardlinked" {
		t.Errorf("the pre-existing skip reason was lost: %+v", skipped.Outcome)
	}

	// 2. And NO row has a retained original invented for it.
	for _, j := range rows {
		if _, ok, err := s.GetRetained(ctx, j.Path); err != nil {
			t.Fatalf("GetRetained(%s): %v", j.Path, err)
		} else if ok {
			t.Errorf("%s reads as having a retained original - that swap happened before the undo "+
				"window existed and its original is gone", j.Path)
		}
	}
	if held, err := s.HeldByUndoWindow(ctx); err != nil {
		t.Fatalf("HeldByUndoWindow: %v", err)
	} else if held != 0 {
		t.Errorf("a migrated pre-UNDO-6 ledger reports %d bytes held by a window it never had", held)
	}
	if live, err := s.ListRetained(ctx); err != nil {
		t.Fatal(err)
	} else if len(live) != 0 {
		t.Errorf("a migrated pre-UNDO-6 ledger lists retentions: %+v", live)
	}

	// 3. The migrated database is WRITABLE through the new table. Without the CREATE
	// this fails with "no such table" - which is how a silent no-op surfaces on a live
	// install: not at startup, but later, on the first swap that tries to retain.
	r := Retained{
		SourcePath: "/lib/fresh.mkv", SwappedPath: "/lib/fresh.mkv",
		RetainedPath: "/lib/.holdfast-undo/fresh", SourceBytes: 4242,
		SwappedFingerprint: "9:9", RetainedAt: 5000, ExpiresAt: 6000,
	}
	if err := s.Retain(ctx, r); err != nil {
		t.Fatalf("Retain on a migrated database: %v", err)
	}
	got, ok, err := s.GetRetained(ctx, "/lib/fresh.mkv")
	if err != nil || !ok {
		t.Fatalf("GetRetained after Retain: ok=%v err=%v", ok, err)
	}
	if got != r {
		t.Errorf("a retention written AFTER the migration did not round-trip:\n  got  %+v\n  want %+v", got, r)
	}
}

// ---- the decision inputs a terminal row was taken under -----------------------

// v9Schema is the schema EXACTLY as it shipped BEFORE the decision-inputs column - every
// step from v1 to v9, in the order they shipped, with the v9 version stamp. Frozen for
// the same reason v0Schema, v3Schema and v4Schema are, and it is the fixture this phase's
// migration proof actually needs: a FRESH database gains the new column either way,
// because the migration list is replayed from nothing, so a fresh-schema test would pass
// over a migration that did nothing at all to a database that already exists. Do NOT
// update it when the schema changes.
const v9Schema = v4Schema + `
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
ALTER TABLE jobs ADD COLUMN source_codec TEXT;
ALTER TABLE jobs ADD COLUMN failure_class TEXT;
PRAGMA user_version = 9;
`

// seedV9 writes a real pre-decision-inputs database at path: the frozen v9 schema, its
// version stamp, and rows that CARRY the outcomes the shipped build recorded - a done row
// with its sizes and its VMAF pair, and two skipped rows carrying the guard tokens whose
// verdicts this phase makes re-derivable. The outcomes matter: the criterion is that
// migrating keeps every value while leaving the new column unrecorded, and a fixture of
// empty rows could not tell those two apart.
func seedV9(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open v9 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(v9Schema); err != nil {
		t.Fatalf("create v9 schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			encoder, vmaf_mean, vmaf_min, vmaf_model, vmaf_pix_fmt, vmaf_chroma, vmaf_chroma_metric,
			source_codec, source_bytes, output_bytes, encode_ms)
		 VALUES ('/lib/old-done.mkv', '10:100', 'done', 0, NULL, 1000,
			'cpu', 97.25, 88.5, 'version=vmaf_v0.6.1', 'yuv420p10le', 41.5, 'psnr_cb/psnr_cr min (dB)',
			'h264', 5000000, 2000000, 12345)`); err != nil {
		t.Fatalf("seed done row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-low-bitrate.mkv', '20:200', 'skipped', 0, NULL, 1001, 'low-bitrate')`); err != nil {
		t.Fatalf("seed low-bitrate row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES ('/lib/old-at-codec.mkv', '30:300', 'skipped', 0, NULL, 1002, 'already-at-target-codec')`); err != nil {
		t.Fatalf("seed already-at-target-codec row: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason, failure_class)
		 VALUES ('/lib/old-failed.mkv', '40:400', 'failed', 2, NULL, 1003, 'ffmpeg died', 'transient')`); err != nil {
		t.Fatalf("seed failed row: %v", err)
	}

	// Sanity: the fixture really is a v9 database that really lacks the new column.
	// Without this the test could pass against a database that was already migrated,
	// which would make it vacuous in exactly the way it exists to avoid.
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatalf("read seeded user_version: %v", err)
	}
	if ver != 9 {
		t.Fatalf("seeded database is at version %d, want 9 - it is not a pre-decision-inputs database", ver)
	}
	if hasColumn(t, db, "decision_inputs") {
		t.Fatal("seeded v9 database already has a `decision_inputs` column - the fixture is wrong")
	}
}

// TestMigrate_APreMigrationDatabaseOnDiskRecordsNoDecisionInputs is the anti-vacuity
// proof this phase owes.
//
// A jobs.db written by the SHIPPED build must migrate forward in place, keep every row
// and every value those rows carry, and read as recording NO decision inputs - which is
// what makes each of them re-openable exactly once, after which the decision they reach
// records what it read. The alternative - a backfill - would claim those rows were taken
// under whatever is configured now, and they would then MATCH and stay excluded for ever,
// which is the silent no-op the whole phase exists to end.
func TestMigrate_APreMigrationDatabaseOnDiskRecordsNoDecisionInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV9(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v9 database must migrate it, not fail: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version after migration = %d, want %d", got, want)
	}
	if !hasColumn(t, s.db, "decision_inputs") {
		t.Fatal("migrated database has no decision_inputs column - the migration was a silent no-op")
	}

	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List after migration: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("migration lost rows: got %d, want 4 (%+v)", len(rows), rows)
	}
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}

	// 1. Every value the v9 rows carried is still there.
	done := byPath["/lib/old-done.mkv"]
	if done.Status != Done || done.Outcome.Encoder != "cpu" || done.Outcome.VmafModel != "version=vmaf_v0.6.1" ||
		done.Outcome.VmafPixFmt != "yuv420p10le" || done.Outcome.SourceCodec != "h264" {
		t.Errorf("the pre-existing done row was mangled: %+v", done)
	}
	if done.Outcome.VmafMean == nil || *done.Outcome.VmafMean != 97.25 ||
		done.Outcome.SourceBytes == nil || *done.Outcome.SourceBytes != 5000000 ||
		done.Outcome.OutputBytes == nil || *done.Outcome.OutputBytes != 2000000 {
		t.Errorf("the pre-existing measurements were lost: %+v", done.Outcome)
	}
	if got := byPath["/lib/old-low-bitrate.mkv"].Outcome.Reason; got != "low-bitrate" {
		t.Errorf("the pre-existing skip reason was lost: %q", got)
	}
	if j := byPath["/lib/old-failed.mkv"]; j.FailCount != 2 || j.Outcome.FailureClass != FailureTransient {
		t.Errorf("the pre-existing failure accounting was lost: %+v", j)
	}

	// 2. And every pre-existing row reads as recording NO inputs - not an empty set,
	// which would always match, and not a fabricated record of the configuration in
	// front of this build, which would match too.
	for _, j := range rows {
		if j.Outcome.DecisionInputs.Recorded() {
			t.Errorf("%s was BACKFILLED with decision inputs (%q) - that is a claim about the "+
				"configuration a decision nobody recorded was taken under",
				j.Path, j.Outcome.DecisionInputs.Encode())
		}
	}

	// 3. Which is exactly what makes them re-openable: a row recording nothing cannot be
	// re-derived, so Claim offers the file to the pipeline rather than skipping it for
	// ever. This is the whole point of reading them as unrecorded, so it is asserted
	// here and not only in the Claim suite.
	current := InputsRead(map[string]string{"min_bitrate_kbps": "1200"})
	if ok, err := s.Claim(ctx, "/lib/old-low-bitrate.mkv", "20:200", "w0", 3, current); err != nil || !ok {
		t.Fatalf("a migrated row recording no inputs must be re-opened: ok=%v err=%v", ok, err)
	}

	// 4. The migrated database is WRITABLE through the new column. Without the ALTER this
	// fails with "no such column" - which is how the silent-no-op bug surfaces on a live
	// install: not at startup, but later, on a query.
	if err := s.Finish(ctx, "/lib/old-low-bitrate.mkv", "20:200", Skipped,
		&Outcome{Reason: "low-bitrate", DecisionInputs: current}, 3); err != nil {
		t.Fatalf("Finish on a migrated database: %v", err)
	}
	after, err := s.List(ctx, []Status{Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, j := range after {
		if j.Path != "/lib/old-low-bitrate.mkv" {
			continue
		}
		if got, ok := j.Outcome.DecisionInputs.Value("min_bitrate_kbps"); !ok || got != "1200" {
			t.Errorf("a row written AFTER the migration lost its decision inputs: %+v", j.Outcome.DecisionInputs)
		}
	}
}

// TestMigrate_AFailedMigrationRefusesToOpenTheStore. A half-migrated schema must never be
// run against: the engine would be recording its proof into columns that may or may not
// be there, and the failure would surface later, on a live install, on a query. So a
// migration that cannot complete is a REFUSAL to open, naming the step that failed, and
// it leaves the version where it was - the version and the shape move together or not at
// all.
func TestMigrate_AFailedMigrationRefusesToOpenTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV9(t, path)

	// A step this build cannot apply, driven through the real migrate() that Open calls.
	// Appended rather than substituted, so every shipped step still runs first and the
	// failure is genuinely a half-way one.
	restore := migrations
	migrations = append(append([]migration{}, migrations...),
		migration{name: "deliberately broken", sql: `THIS IS NOT SQL;`})
	t.Cleanup(func() { migrations = restore })

	s, err := Open(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("Open must REFUSE a database whose migration could not complete, not run against a half-migrated schema")
	}
	if !strings.Contains(err.Error(), "deliberately broken") {
		t.Errorf("the refusal must name the step that failed, got: %v", err)
	}

	// The stamp never escaped the failed step's transaction: the database still claims
	// the last version it actually has.
	if got, want := userVersion(t, path), schemaVersion()-1; got != want {
		t.Errorf("user_version after a failed migration = %d, want %d", got, want)
	}
	// And the steps that DID apply are intact, with every row still there - a refusal is
	// not a rollback of the whole file.
	if got := columnCount(t, path); got == 0 {
		t.Fatal("the database is unreadable after a refused open")
	}
}

// columnCount reads how many columns jobs has, from SQLite itself, through a handle this
// test opens directly - the store refused to open, so there is no store to ask.
func columnCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs')`).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	return n
}

// TestMigrations_ShippedTextIsNeverEdited pins the SQL of every migration that has
// already shipped, by content hash.
//
// It enforces the rule migrate.go states and cannot enforce by itself: never edit,
// reorder or delete a shipped migration. A database in the field has ALREADY RUN the
// old text, so rewriting it changes only what a FRESH database gets - and silently
// forks the two shapes apart, giving you a bug that reproduces on exactly one of them
// and is invisible in review, because the diff looks like a correction.
//
// Adding a migration? APPEND it and add its hash here. If one of these reds for a
// migration you did not mean to touch, you have just edited history: revert and append.
func TestMigrations_ShippedTextIsNeverEdited(t *testing.T) {
	shipped := []struct{ name, sha256 string }{
		{"jobs table", "4cad37da548ce37283d6cac5a4ba448e1ebeeffb3b4a5ba7bc9d2236316257df"},
		{"outcome columns", "f32c589da2867b079b6601be9073cc18c89311af1dd2734dd3f58d38423c2fc5"},
		{"aggregate indexes", "f37a79b51a174ce3fa9eee7dd10780f75c8d6cb0eaa70277cdbc54be65d29188"},
		{"comparison format and chroma columns", "78f72aa49e83685be32c26550f09f4190203719480a5e262c24ef3dd483b1522"},
		{"retained originals", "27b4e3a4954ff0b060797b229f1d0ec6f60c7c4a08b0fb571c479a7ab88c25b7"},
		{"ledger retention totals", "44635e1577347481b3322bc04f2a1ac54a2560b452b59c1f5e3a9d7cb392d4a5"},
		{"swap guard record + swap incidents", "1a2162b7e4061ca5a90f53adc9916d85969e05705baf232fc1e5431a13125dbf"},
		{"source codec", "4f8cf21fb8743b51e8609fef458308f65e4e36782d98213cff15833aacc3b164"},
		{"failure class", "1401872cf991a88581c43c24470de409ef06683ca3aaa9cfbd728220bed2b53a"},
		{"decision inputs", "285adec2f24e51f0fdecec3c20a36388fb68b0b9b0da5730774361c82b8a5600"},
	}
	if len(migrations) < len(shipped) {
		t.Fatalf("migrations has %d entries, fewer than the %d that have shipped - an entry was "+
			"DELETED, which rewrites the history of every database in the field",
			len(migrations), len(shipped))
	}
	for i, want := range shipped {
		got := migrations[i]
		if got.name != want.name {
			t.Errorf("migration %d is now named %q, want %q - migrations were reordered or "+
				"replaced; append instead", i+1, got.name, want.name)
			continue
		}
		if h := fmt.Sprintf("%x", sha256.Sum256([]byte(got.sql))); h != want.sha256 {
			t.Errorf("migration %d (%s) has been EDITED.\n  got sha256  %s\n  want        %s\n"+
				"A database in the field has already run the old text, so this changes only what a "+
				"FRESH database gets and forks the two shapes apart. Revert the edit and APPEND a "+
				"new migration instead.", i+1, got.name, h, want.sha256)
		}
	}
}

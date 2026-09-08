package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
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
	if ok, err := s.Claim(ctx, "/lib/fresh.mkv", "50:500", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim on a migrated database: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/fresh.mkv", "50:500", Done, &Outcome{
		Encoder: "cpu", VmafMean: f64(97.0), VmafMin: f64(90.0), VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: i64(1000), OutputBytes: i64(400), EncodeMs: i64(999),
	}); err != nil {
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
	"source_bytes", "output_bytes", "encode_ms",
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
	if ok, err := s.Claim(ctx, "/lib/fresh.mkv", "50:500", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim on a migrated database: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/fresh.mkv", "50:500", Done, &Outcome{
		Encoder: "cpu", VmafMean: f64(98.4), VmafMin: f64(96.1), VmafModel: "version=vmaf_v0.6.1",
		VmafPixFmt: "yuv420p10le", VmafChroma: f64(41.2), VmafChromaMetric: "psnr_cb/psnr_cr min (dB)",
	}); err != nil {
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

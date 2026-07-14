package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
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
	for _, col := range []string{
		"reason", "encoder", "vmaf_mean", "vmaf_min", "vmaf_model",
		"source_bytes", "output_bytes", "encode_ms",
	} {
		if !hasColumn(t, s.db, col) {
			t.Errorf("migrated database is missing column %q — the migration was a silent no-op", col)
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
		if o.VmafMean != nil || o.VmafMin != nil || o.SourceBytes != nil || o.OutputBytes != nil || o.EncodeMs != nil {
			t.Errorf("a pre-migration row must read as not-recorded (nil), got %+v for %s", o, j.Path)
		}
		if o.Reason != "" || o.Encoder != "" || o.VmafModel != "" {
			t.Errorf("a pre-migration row must carry no reason/encoder/model, got %+v for %s", o, j.Path)
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

// Two processes opening the same UN-MIGRATED database at once must both come up. This
// is the real first-start-after-upgrade shape: a `serve` daemon and an operator running
// `holdfast run` against the same jobs.db, neither of which has migrated it yet.
//
// It is easy to get two ways wrong at once, and both were:
//   - a DEFERRED transaction takes no write lock until its first write, so both
//     processes begin, both try to upgrade, and the loser gets SQLITE_BUSY — which
//     busy_timeout will NOT retry, because the deadlock is already established.
//     Hence BEGIN IMMEDIATE.
//   - checking the version OUTSIDE the transaction lets both processes read 0, and then
//     the loser's ALTER dies on "duplicate column name". Hence the re-read under the
//     lock.
//
// Migrating must not introduce a startup failure the old lock-free schema init did not
// have, so this runs the real Open concurrently and demands every one of them succeed.
func TestMigrate_ConcurrentFirstOpenOfAV0Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedV0(t, path)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = s.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Open #%d failed — a first-start race must not refuse to boot: %v", i, err)
		}
	}

	// Exactly one migration actually ran: the schema is at the current version, and the
	// rows are all still there (nobody applied it twice or half-applied it).
	if got, want := userVersion(t, path), schemaVersion(); got != want {
		t.Errorf("user_version = %d, want %d", got, want)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after the race: %v", err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 4 {
		t.Errorf("row count after a concurrent migration = %d, want 4", len(rows))
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
	for _, col := range []string{"reason", "encoder", "vmaf_mean", "vmaf_min", "vmaf_model", "source_bytes", "output_bytes", "encode_ms"} {
		if !hasColumn(t, s.db, col) {
			t.Errorf("fresh database is missing column %q", col)
		}
	}
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

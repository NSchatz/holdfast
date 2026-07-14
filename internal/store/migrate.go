package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Schema versioning (TRANSCODE-13).
//
// Why this exists at all. The pre-TRANSCODE-13 schema was a bare
// `CREATE TABLE IF NOT EXISTS jobs (...)` run on every Open, with no version stamp
// anywhere. That is not a schema — it is a schema for a database that never changes.
// `IF NOT EXISTS` matches on the table's NAME, not its SHAPE, so the moment anyone
// adds a column to that statement it becomes a **silent no-op against every database
// that already exists**: the file keeps its old columns, Open reports success, the
// process comes up believing the column is there, and it dies on the first query that
// names it. The failure is not at the migration, it is later, on a live install, on a
// query — the worst possible place.
//
// So the columns TRANSCODE-13 needs cannot be added until there is a real migration
// mechanism, and the mechanism has to go in NOW, while the only jobs.db in the world
// is a developer's. That is the whole ordering argument for this phase.
//
// The mechanism is SQLite's `PRAGMA user_version` — a 32-bit integer SQLite stores in
// the database header and otherwise ignores entirely, which is exactly what a schema
// version wants to be. `migrations` below is the schema's history; the version is its
// length; a database is migrated by running the entries it has not run yet.

// migration is one forward step in the schema's history.
type migration struct {
	name string
	sql  string
}

// migrations is APPEND-ONLY, and its order IS the schema history. Never edit,
// reorder, or delete an entry that has shipped: a database in the field has already
// run the old text, so rewriting it changes only what a FRESH database gets — which
// silently forks the two shapes apart and gives you a bug that reproduces on exactly
// one of them. To change the schema, append a new entry.
var migrations = []migration{
	{
		// v1 — the original TRANSCODE-5 schema, exactly as it shipped.
		//
		// `IF NOT EXISTS` here is load-bearing, not laziness. A database created before
		// versioning existed already HAS this table and still reports user_version = 0,
		// so it is indistinguishable from a fresh file by the version alone. v1 must
		// therefore be a no-op on the former and a real create on the latter — after
		// which both are at v1 with the identical shape and continue into v2 together.
		// This is the one migration allowed to be shaped by that history; every future
		// one starts from a known version.
		name: "jobs table",
		sql: `
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
`,
	},
	{
		// v2 — TRANSCODE-13: the outcome columns. The engine computed every one of
		// these while deciding the swap was safe, and then threw all of them away.
		//
		// Every column is NULLABLE with NO DEFAULT, deliberately. NULL means "not
		// recorded" and has to stay distinguishable from a recorded zero, because 0 is
		// a legal value for all of them: a VMAF of 0.0 is a destroyed frame, not a
		// missing measurement. A `DEFAULT 0` here would backfill every pre-existing row
		// with a fabricated perfect-looking outcome — inventing evidence about swaps
		// nobody measured, in the one table whose entire job is to be evidence.
		name: "outcome columns",
		sql: `
ALTER TABLE jobs ADD COLUMN reason       TEXT;
ALTER TABLE jobs ADD COLUMN encoder      TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_mean    REAL;
ALTER TABLE jobs ADD COLUMN vmaf_min     REAL;
ALTER TABLE jobs ADD COLUMN vmaf_model   TEXT;
ALTER TABLE jobs ADD COLUMN source_bytes INTEGER;
ALTER TABLE jobs ADD COLUMN output_bytes INTEGER;
ALTER TABLE jobs ADD COLUMN encode_ms    INTEGER;
`,
	},
}

// schemaVersion is the version this build expects a database to be at. It IS the
// migration count — there is no second place to bump, so the two can never disagree.
func schemaVersion() int { return len(migrations) }

// migrate brings db up to schemaVersion(), running only the migrations it has not
// already run. It is idempotent: on an up-to-date database it reads one PRAGMA and
// returns.
//
// Every failure here is returned, and Open turns it into a refusal to start (see
// cmd/holdfast: a store that will not open is a non-zero exit). That is the fail-safe
// the phase requires — a half-migrated database must never be run against, because
// the engine would then be recording the proof of its swaps into columns that may or
// may not exist.
func migrate(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	want := schemaVersion()

	// A database from the FUTURE is a refusal, never a silent downgrade. An older
	// binary against a newer schema does not "just work": it reads rows through a
	// narrower SELECT and — because Finish writes the full outcome column set — would
	// overwrite outcomes it cannot even see. Refusing to open is the safe move; the
	// operator rolls the binary forward (or the database back) and loses nothing
	// meanwhile.
	if have > want {
		return fmt.Errorf("store: database schema version %d is newer than this build supports (%d) — "+
			"refusing to open (running an older binary against a newer schema would silently discard data it cannot see; "+
			"upgrade holdfast, or restore an older database)", have, want)
	}

	for i := have; i < want; i++ {
		if err := applyMigration(ctx, db, i+1, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d (%s): %w", i+1, migrations[i].name, err)
		}
	}
	return nil
}

// applyMigration runs one migration's DDL and stamps the new user_version in the SAME
// transaction, so both land or neither does. SQLite has transactional DDL and journals
// the header write that `PRAGMA user_version =` performs, so a crash or an error part
// way through rolls the whole step back — the database can never end up claiming a
// version whose columns it does not have, which is the one way a versioned schema can
// still lie to you.
//
// The transaction is BEGIN IMMEDIATE, not the default DEFERRED, and that matters. A
// deferred transaction takes no write lock until its first write, so two processes
// opening the same un-migrated database (a `serve` daemon and an operator's `holdfast
// run`, on the first start after an upgrade) both begin, both try to upgrade to a write
// lock, and one gets SQLITE_BUSY — which busy_timeout will NOT retry, because the
// deadlock is already established. IMMEDIATE takes the write lock up front, where the
// busy handler CAN wait on it, so the second process simply blocks until the first has
// migrated and then finds nothing left to do. Migrating is not the place to introduce a
// startup failure the old (idempotent, lock-free) schema init did not have.
func applyMigration(ctx context.Context, db *sql.DB, version int, m migration) error {
	// A dedicated connection: BEGIN/COMMIT are statements here rather than
	// database/sql's tx API (which offers no way to ask for IMMEDIATE), so they must
	// all land on the same connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	// Roll back on any failure below. Uses a background context deliberately: if ctx is
	// what failed (cancelled), a rollback on ctx would fail too and leak the write lock
	// for as long as the connection lives.
	rollback := func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }

	// RE-READ the version under the write lock. migrate() read it before calling us,
	// but between that read and our acquiring this lock another process may have run
	// this very migration — and then our ALTER would die on "duplicate column name",
	// turning a concurrent first-open into a startup failure for whoever lost the race.
	// The check and the apply have to be atomic TOGETHER, which means the check belongs
	// inside the transaction that does the applying. Losing the race is now a no-op: the
	// work is already done.
	var cur int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&cur); err != nil {
		rollback()
		return fmt.Errorf("re-read user_version: %w", err)
	}
	if cur >= version {
		rollback() // nothing to do; another process applied it while we waited
		return nil
	}

	if _, err := conn.ExecContext(ctx, m.sql); err != nil {
		rollback()
		return err
	}
	// PRAGMA takes no bound parameters, so the version is formatted into the text. It
	// is an int derived from len(migrations) — never anything a caller supplies — so
	// there is no injection surface here, only an API limitation.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		rollback()
		return fmt.Errorf("stamp user_version: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback()
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

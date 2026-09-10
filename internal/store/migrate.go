package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Schema versioning.
//
// `CREATE TABLE IF NOT EXISTS` matches on a table's NAME, not its SHAPE, so adding a column
// to such a statement is a silent no-op against every database that already exists: the
// file keeps its old columns, Open reports success, and the process dies on the first query
// that names the new one. The failure lands later, on a live install, on a query, which is
// the worst possible place.
//
// The mechanism is SQLite's `PRAGMA user_version`, a 32-bit integer SQLite stores in the
// database header and otherwise ignores entirely. `migrations` below is the schema's
// history, the version is its length, and a database is migrated by running the entries it
// has not run yet.

// migration is one forward step in the schema's history.
type migration struct {
	name string
	sql  string
}

// migrations is APPEND-ONLY, and its order IS the schema history. Never edit, reorder or
// delete an entry that has shipped: a database in the field has already run the old text,
// so rewriting it changes only what a FRESH database gets, which forks the two shapes apart
// silently and gives you a bug that reproduces on exactly one of them. To change the
// schema, append. A step written while another branch was open moves to the END of the
// history rather than contesting an ordinal that is already spent.
//
// Every column added here is NULLABLE with NO DEFAULT. NULL means "not recorded" and has to
// stay distinguishable from a recorded zero, because 0 is a legal value for all of them: a
// VMAF of 0.0 is a destroyed frame, not a missing measurement. A DEFAULT would backfill
// every pre-existing row with a fabricated outcome, inventing evidence about swaps nobody
// measured, in the one table whose entire job is to be evidence.
var migrations = []migration{
	{
		// v1 - the original schema, exactly as it shipped. `IF NOT EXISTS` here is
		// load-bearing: a database created before versioning existed already HAS this table
		// and still reports user_version = 0, so v1 must be a no-op on it and a real create
		// on a fresh file, after which both are at v1 with the identical shape. It is the
		// one migration shaped by that history; every future one starts from a known version.
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
		// v2 - the outcome columns. The engine computed every one of these while deciding
		// the swap was safe, and then threw all of them away.
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
	{
		// v3 - the indexes the whole-ledger aggregates read through. The published figures
		// are computed over EVERY matching row and recomputed on the snapshot path, which
		// shares one serialized connection with the engine's writes, so a full table scan of
		// a 300,000-row ledger on every snapshot would be paid for in encode throughput.
		//
		// idx_jobs_status_reason serves the skip breakdown and the terminal counts;
		// idx_jobs_outcome carries the four numeric columns the done-row spreads read, so
		// each is an index-only scan of the done partition instead of a walk over the whole
		// table. Both cost a little write amplification per transition, microseconds against
		// an encode measured in minutes.
		name: "aggregate indexes",
		sql: `
CREATE INDEX IF NOT EXISTS idx_jobs_status_reason ON jobs(status, reason);
CREATE INDEX IF NOT EXISTS idx_jobs_outcome ON jobs(status, source_bytes, output_bytes, encode_ms, vmaf_mean, vmaf_min);
`,
	},
	{
		// v4 - what the quality gate actually compared, and what it found in the colour
		// planes. vmaf_pix_fmt is the single format both streams were converted to before
		// scoring, which until this step nothing recorded and nothing chose. vmaf_chroma and
		// vmaf_chroma_metric are the chroma measurement and the name of the metric that
		// produced it, and they travel together because a bare dB figure with no metric
		// attached is not something an operator can act on.
		name: "comparison format and chroma columns",
		sql: `
ALTER TABLE jobs ADD COLUMN vmaf_pix_fmt       TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_chroma        REAL;
ALTER TABLE jobs ADD COLUMN vmaf_chroma_metric TEXT;
`,
	},
	{
		// v5 - the retained originals the undo window can put back.
		//
		// A SEPARATE TABLE rather than columns on jobs, forced by the lifetimes: a jobs row
		// is keyed (path, fingerprint) and the swap DELETES the pre-swap row, so a retention
		// recorded there would be pruned by the very swap it exists to undo. This is keyed by
		// the library PATH, which is what an operator asks to restore.
		//
		// source_path is where the original goes BACK and swapped_path is what the swap
		// produced; both are recorded because a restore puts one back and removes the other,
		// and deriving either from the other would be guessing at configuration that may
		// since have changed. swapped_fingerprint is the size:mtime of what the swap left,
		// taken immediately after it, and is what makes a restore refuse to overwrite content
		// this tool did not put there. restored_at is NULL until an operator restores and a
		// released retention is DELETED outright, so "is there anything to restore for this
		// path" is exactly "a row exists with restored_at IS NULL", with no third state.
		name: "retained originals",
		sql: `
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
`,
	},
	{
		// v6 - the durable carry-forward a prune needs, and the index it reads the oldest
		// rows through.
		//
		// ledger_totals carries the ONE fact a pruned row would otherwise take with it. The
		// published lifetime reclaimed total is a SUM over the done rows that recorded both
		// sizes, so deleting such a row lowers it - not immediately, since the server reads
		// the baseline once at startup, but at the next restart, which is precisely how a
		// wrong total ships unnoticed. Prune therefore ADDS the rows' contribution here in
		// the same transaction that deletes them, and ReclaimedTotal reads live rows plus
		// this, so the total can never run backwards.
		//
		// One row, enforced by the CHECK: a second would silently split the total in two.
		// INSERT OR IGNORE seeds it so every later UPDATE has something to update.
		//
		// idx_jobs_status_updated makes "the oldest terminal rows" an index scan rather than
		// a sort of the operator's entire library, on the same serialized connection the
		// engine writes through.
		name: "ledger retention totals",
		sql: `
CREATE TABLE IF NOT EXISTS ledger_totals (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	reclaimed_pruned INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO ledger_totals (id, reclaimed_pruned) VALUES (1, 0);
CREATE INDEX IF NOT EXISTS idx_jobs_status_updated ON jobs(status, updated_at);
`,
	},
	{
		// v7 - the source-mutation guard's achieved granularity, and the durable record of
		// a swap that did not complete cleanly. Two independent additions, in one step
		// because they ship together.
		//
		// (1) Four more outcome columns on jobs. Three record what the guard actually did
		// for that job: which attributes it compared, the resolution of the timestamp it
		// compared, and WHICH residual window applies to the storage it ran against. The
		// fourth names the CAUSE of a swap failure when holdfast reports it distinctly. A
		// job that never reached the guard has no window, and a fabricated one would be a
		// claim about a check that never ran.
		//
		// (2) A SEPARATE swap_incidents table, for lifecycle rather than tidiness: Claim
		// CLEARS the outcome columns and a successful transcode PRUNES the pre-swap row, so
		// a fact carried there has an expiry date. The record that a replacement holdfast
		// wrote is still sitting in a library root has to outlive both, because the FILE
		// does.
		//
		// The partial index is the read the scan makes on every run: which recorded
		// replacement paths must not be enumerated. Partial, so it indexes only the rows
		// that can still exclude something.
		name: "swap guard record + swap incidents",
		sql: `
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
`,
	},
	{
		// v8 - the source codec a dry-run decision records: the one fact the outcome columns
		// did not already carry. The size is source_bytes, which v2 added and which means
		// the same thing on this row as on a done row.
		name: "source codec",
		sql: `
ALTER TABLE jobs ADD COLUMN source_codec TEXT;
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
	have, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	want := schemaVersion()

	// A database from the FUTURE is a refusal, never a silent downgrade. An older
	// binary against a newer schema does not "just work": it reads rows through a
	// narrower SELECT and — because Finish writes the full outcome column set — would
	// overwrite outcomes it cannot even see. Refusing to open is the safe move; the
	// operator rolls the binary forward (or the database back) and loses nothing
	// meanwhile.
	if have > want {
		return errSchemaFromTheFuture(have, want)
	}

	for i := have; i < want; i++ {
		if err := applyMigration(ctx, db, i+1, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d (%s): %w", i+1, migrations[i].name, err)
		}
	}
	return nil
}

// readSchemaVersion reads the stamp SQLite keeps in the database header. It is the one
// place either door reads it, so a reader and a writer can never disagree about what
// they are looking at.
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var have int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&have); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return have, nil
}

// errSchemaFromTheFuture is the refusal both doors give a database this build cannot see
// all of. Shared so the two cannot drift apart into two different explanations of the
// same fact.
func errSchemaFromTheFuture(have, want int) error {
	return fmt.Errorf("store: database schema version %d is newer than this build supports (%d) — "+
		"refusing to open (running an older binary against a newer schema would silently discard data it cannot see; "+
		"upgrade holdfast, or restore an older database)", have, want)
}

// requireCurrentSchema is migrate's read-only counterpart (LEDGER-5): it CHECKS the
// version and never moves it. OpenReadOnly is its only caller.
//
// Behind this build is a refusal here, where migrate would upgrade. That is the whole
// point of the read-only door. Reading an older ledger would mean querying columns the
// file may not have, and the only way to give it those columns is to migrate it — which
// stamps a user_version the holdfast that wrote the file will then refuse. A reader that
// did that would break the running daemon it was trying not to disturb. So it refuses and
// names the deliberate act instead: no rows are lost, and migrating a ledger stays
// something the operator does by starting holdfast, not something a read does behind
// their back.
func requireCurrentSchema(ctx context.Context, db *sql.DB) error {
	have, err := readSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	want := schemaVersion()
	switch {
	case have > want:
		return errSchemaFromTheFuture(have, want)
	case have < want:
		return fmt.Errorf("store: database schema version %d is older than this build's (%d) — "+
			"refusing to open it read-only, because a read must not migrate the ledger it is reading "+
			"(that would stamp a version the holdfast which wrote this file would then refuse to open). "+
			"Run `holdfast run` or `holdfast serve` once with this build to migrate the store, then read it again", have, want)
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

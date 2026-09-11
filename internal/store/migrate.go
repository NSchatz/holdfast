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
	{
		// v3 - DASH-7: the indexes the whole-ledger aggregates read through.
		//
		// The published figures are computed over EVERY matching row rather than over
		// the few hundred the queue/history views ship, and they are recomputed on the
		// snapshot path - which shares one serialized connection with the engine's
		// writes. An aggregate that costs a full table scan of a 300,000-row ledger on
		// every snapshot would therefore be paid for in encode throughput, and "the
		// dashboard slowed the transcoder down" is not a trade this tool gets to make.
		//
		// idx_jobs_status_reason serves the skip breakdown (GROUP BY reason within the
		// skipped rows) and the terminal counts. idx_jobs_outcome carries the four
		// numeric columns the done-row spreads read, so each of those is an index-only
		// scan of the done partition instead of a walk over the whole table. Both cost
		// a little write amplification per job transition - a few microseconds against
		// an encode measured in minutes.
		name: "aggregate indexes",
		sql: `
CREATE INDEX IF NOT EXISTS idx_jobs_status_reason ON jobs(status, reason);
CREATE INDEX IF NOT EXISTS idx_jobs_outcome ON jobs(status, source_bytes, output_bytes, encode_ms, vmaf_mean, vmaf_min);
`,
	},
	{
		// v4 - GATE-4: what the quality gate actually compared, and what it found in
		// the colour planes.
		//
		// vmaf_pix_fmt is the single format both streams were converted to before
		// scoring. Until this phase nothing recorded it and nothing chose it: the two
		// inputs disagree on the default path (pixel_format: auto floors output depth
		// at 10, so an 8-bit source meets a 10-bit output) and libavfilter negotiated
		// the conversion unobserved. vmaf_chroma + vmaf_chroma_metric are the chroma
		// measurement and the name of the metric that produced it, which have to
		// travel together: a bare dB figure with no metric attached is not something
		// an operator can act on, exactly as vmaf_model established for the score.
		//
		// NULLABLE with NO DEFAULT, as v2 established. Every row written before this
		// migration was scored by a gate that measured none of these things, and it
		// must READ as not recorded rather than be backfilled with a value that would
		// claim a chroma measurement nobody took. A DEFAULT here would invent evidence
		// about swaps that already happened, in the one table whose whole job is to be
		// evidence. 0.0 is legal for vmaf_chroma too - it is an obliterated plane.
		name: "comparison format and chroma columns",
		sql: `
ALTER TABLE jobs ADD COLUMN vmaf_pix_fmt       TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_chroma        REAL;
ALTER TABLE jobs ADD COLUMN vmaf_chroma_metric TEXT;
`,
	},
	{
		// v5 - UNDO-6: the retained originals the undo window can put back.
		//
		// A SEPARATE TABLE rather than columns on jobs, and that is forced by the
		// lifetimes. A jobs row is keyed (path, fingerprint) and the swap DELETES the
		// pre-swap row (ProcessFile prunes it once the done row lands under the final
		// file's new key), so a retention recorded on that row would be pruned by the
		// very swap it exists to undo. The retention outlives the row: it is keyed by
		// the library PATH, which is the thing an operator asks to restore.
		//
		// source_path is where the original goes BACK; swapped_path is what the swap
		// produced (the same path for an in-place rename, a different one when the
		// container extension changed). Both are recorded because a restore has to put
		// one back and remove the other, and deriving either from the other after the
		// fact would be guessing at configuration that may since have changed.
		//
		// swapped_fingerprint is the size:mtime of the file the swap left at
		// swapped_path, taken immediately after the swap. It is what makes a restore
		// refuse to overwrite content that is not what this tool put there.
		//
		// restored_at is NULL until an operator restores, and a released retention is
		// DELETED outright - so "is there anything to restore for this path" is exactly
		// "a row exists with restored_at IS NULL", with no third state to get wrong.
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
		// v6 - LEDGER-5: the durable carry-forward a prune needs, and the index it reads
		// the oldest rows through.
		//
		// It is v6 and NOT v4 or v5, which is the whole of what this slice's append-only
		// rule is for. GATE-4's columns shipped as v4 and UNDO-6's retained_originals as
		// v5 while this branch was open; a database in the field has already run both
		// texts under those versions. Two different steps claiming one version would
		// silently fork the schema in two - so this one moves to the end of the history
		// rather than contesting an ordinal that is already spent.
		//
		// ledger_totals carries the ONE fact a pruned row would otherwise take with it.
		// The published lifetime reclaimed total is a SUM over the done rows that recorded
		// both sizes, so deleting such a row lowers it - not immediately (the server reads
		// the baseline once, at startup) but at the next restart, which is precisely how a
		// wrong total ships unnoticed. Prune therefore ADDS the rows' contribution here, in
		// the same transaction that deletes them, and ReclaimedTotal reads live rows plus
		// this. The row can only ever grow, so the total can never run backwards.
		//
		// One row, enforced by the CHECK: this is a singleton counter, not a table of
		// them, and a second row would silently split the total in two. INSERT OR IGNORE
		// seeds it so every later UPDATE has something to update - and re-running the
		// migration (which cannot happen, but the whole mechanism is built on it being
		// safe if it did) changes nothing.
		//
		// idx_jobs_status_updated is what makes "the oldest terminal rows" an index scan
		// rather than a sort of the operator's entire library: the prune orders terminal
		// rows by updated_at, on the same serialized connection the engine writes through.
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
		// v7 - FILESYSTEM-1 (the swap half): the source-mutation guard's achieved
		// granularity, and the durable record of a swap that did not complete cleanly.
		//
		// It is v7 and NOT v4, which is this slice's append-only rule doing exactly the
		// job LEDGER-5 records above it. This step was written as v4 while the branch was
		// open; GATE-4 then shipped v4, UNDO-6 v5 and LEDGER-5 v6, and a database in the
		// field has already run all three of those texts under those versions. Two
		// different steps claiming one version would silently fork the schema in two, so
		// this one moves to the end of the history rather than contesting an ordinal that
		// is already spent. Nothing about the SQL changes; only where it sits.
		//
		// Two independent additions, in one step because they ship together.
		//
		// (1) Four more nullable outcome columns on jobs. Three record what the
		// source-mutation guard actually did for that job (which attributes it
		// compared, the resolution of the timestamp it compared, and WHICH residual
		// window applies to the storage it ran against). The fourth names the CAUSE of
		// a swap failure when the cause is one holdfast reports distinctly. NULL is
		// "not recorded" here exactly as it is for every other outcome column: a job
		// that never reached the guard has no window, and a fabricated one would be a
		// claim about a check that never ran.
		//
		// (2) A SEPARATE swap_incidents table. It is not more columns on jobs, and the
		// reason is lifecycle, not tidiness: Claim CLEARS the outcome columns (it
		// begins a new attempt) and a successful transcode PRUNES the pre-swap row, so
		// a fact carried there is a fact with an expiry date. The record that a
		// replacement holdfast wrote is still sitting in a library root has to outlive
		// both of those, because the FILE does. Its own table, keyed by its own id and
		// referring to the job by (source_path, source_fingerprint), is what gives it
		// that lifetime.
		//
		// The partial index is the read the scan makes on every run: which recorded
		// replacement paths must not be enumerated. Partial, so it indexes only the
		// rows that can still exclude something.
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
		// v8 - the source codec a dry-run decision records.
		//
		// The dry-run branch decided a file and threw the decision away, so nothing in the
		// ledger said which files a real run would transcode. Recording that decision needs
		// one fact the outcome columns did not already carry: what the SOURCE is in. The
		// size is source_bytes, which v2 added and which means the same thing on this row
		// as on a done row - the size of the file that was examined.
		//
		// NULLABLE with NO DEFAULT, which is the rule v2 set and every step since has kept.
		// Every row already in the field was written by a build that probed a codec and
		// never stored one, so it must READ AS NOT RECORDED. A DEFAULT here - '' or
		// 'unknown' or anything else - would put a codec on rows nobody recorded one for,
		// including rows about sources that have since been deleted, in the one table whose
		// whole job is to be evidence.
		name: "source codec",
		sql: `
ALTER TABLE jobs ADD COLUMN source_codec TEXT;
`,
	},
	{
		// v9 - the class of a terminal failure: whether a re-attempt could differ.
		//
		// One nullable TEXT column holding a two-value vocabulary (store.FailureClass).
		// It is a column on jobs rather than a table of its own because its lifetime IS
		// the row's: it describes THIS attempt's verdict, Claim clears it when a new
		// attempt begins, and a successful transcode prunes it with everything else the
		// attempt recorded.
		//
		// NULLABLE with NO DEFAULT, which is the rule v2 set and every step since has
		// kept - but here the reason is the opposite of the usual one. Elsewhere a
		// DEFAULT would invent evidence; here NULL is not "unknown" at all, because
		// there is no unknown class to represent: an absent class READS as transient
		// (FailureClass.Class), which is the retry direction and the only fail-safe one.
		// Every failure row already in the field was written by a build that retried
		// every failure alike, so reading them as transient is not a fallback, it is
		// exactly what those rows mean. A backfill would be a rewrite of history with
		// nothing to gain: the read already answers correctly, and no row's behaviour
		// under Claim changes.
		//
		// No index. Nothing queries BY the class: Claim keys on (path, fingerprint) and
		// decides on fail_count alone, and every other reader has the row in hand.
		name: "failure class",
		sql: `
ALTER TABLE jobs ADD COLUMN failure_class TEXT;
`,
	},
	{
		// v10 - the decision inputs a terminal row was taken under.
		//
		// One nullable TEXT column holding what the decision that wrote the row actually
		// READ from the configuration (store.DecisionInputs). It is a column on jobs
		// rather than a table of its own for the reason the failure class is: its
		// lifetime IS the row's. It describes THIS decision, Claim clears it when a new
		// attempt begins, and a successful transcode prunes it with everything else the
		// attempt recorded.
		//
		// NULLABLE with NO DEFAULT, the rule v2 set and every step since has kept, and
		// here it is the whole point rather than a convention. A row already in the field
		// was written by a build that recorded no inputs, and it must READ as not
		// recorded - which the re-opening rule treats as "this verdict cannot be
		// re-derived", so the row is offered to the pipeline once and the decision it
		// then reaches records what it read. A DEFAULT - '' or 'none' or anything else -
		// would claim those rows were taken under a configuration nobody recorded, and
		// they would then MATCH whatever is current and stay excluded for ever, which is
		// precisely the silent no-op this column exists to end.
		//
		// An index on (status, decision_inputs) serves the survey the startup report and
		// `validate` read: how many done/skipped rows were taken under a configuration
		// that has since moved. That question is answered by grouping the DISTINCT
		// recorded values within those two statuses - a handful of groups over a
		// 300,000-row ledger - rather than by decoding every row, and the index is what
		// keeps it off the single serialized connection the engine writes through.
		name: "decision inputs",
		sql: `
ALTER TABLE jobs ADD COLUMN decision_inputs TEXT;
CREATE INDEX IF NOT EXISTS idx_jobs_status_inputs ON jobs(status, decision_inputs);
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

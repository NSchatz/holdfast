package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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

	// rows is what this step DECLARES it means to do to the row counts. It is metadata
	// BESIDE the step and never part of it: the sql above is what a database in the field
	// has already run, and it is pinned by content hash, so a declaration can be added to
	// a shipped step without touching a byte of what that step does.
	rows rowChanges
}

// rowChanges is a step's declared per-table row-count intent, keyed by table name. A table
// the map does not name is declared UNCHANGED, and a table absent from the database at one
// of the two moments holds no rows and counts zero there - so a step that creates a table
// and seeds a row into it declares +1, and one that creates empty tables, adds columns or
// builds indexes declares nothing.
type rowChanges map[string]int64

// noRowChange is the declaration of a step that moves no row. It is spelled out beside
// every such step rather than left off, so a step carrying NO declaration is a step
// somebody forgot rather than one that meant zero.
var noRowChange = rowChanges{}

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
		// Nothing is inserted, and the create is a no-op on the pre-versioning database
		// that already has the table, so the count is the same on both of the two shapes
		// this one step meets.
		rows: noRowChange,
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
		rows: noRowChange,
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
		rows: noRowChange,
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
		rows: noRowChange,
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
		// An empty table and two indexes: a retention is written by a swap, never by a
		// migration, and a fabricated one would be a second link to bytes nobody kept.
		rows: noRowChange,
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
		// THE ONE SHIPPED STEP THAT MOVES A ROW COUNT, and it moves it by exactly one.
		// The INSERT OR IGNORE seeds the singleton counter into the table this same step
		// creates, so ledger_totals goes from absent (no rows) to holding its one row on
		// every database this step runs against - a fresh file and a v5 ledger alike.
		// Declaring a flat zero here would make the guard refuse a legitimate first open,
		// which for a daemon is total.
		rows: rowChanges{"ledger_totals": 1},
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
		// Four columns, an empty table and two indexes. The AUTOINCREMENT key brings
		// SQLite's own sqlite_sequence table into existence with it, and that table gains
		// a row on the first INSERT into swap_incidents rather than here, so it too is
		// declared unchanged.
		rows: noRowChange,
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
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN source_codec TEXT;
`,
	},
	{
		// v9 - the class of a terminal failure: whether a re-attempt could differ. One
		// nullable TEXT column holding a two-value vocabulary (store.FailureClass), on jobs
		// rather than in a table of its own because its lifetime IS the row's.
		//
		// Here NULL is not "unknown": an absent class READS as transient
		// (FailureClass.Class), the retry direction and the only fail-safe one, and every
		// failure row already in the field was written by a build that retried every failure
		// alike. No backfill is owed, and no index: nothing queries BY the class.
		name: "failure class",
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN failure_class TEXT;
`,
	},
	{
		// v10 - the decision inputs a terminal row was taken under: one nullable TEXT column
		// holding what the decision that wrote the row actually READ from the configuration
		// (store.DecisionInputs), on jobs for the reason the failure class is.
		//
		// A row already in the field recorded no inputs and must READ as not recorded, which
		// the re-opening rule treats as "this verdict cannot be re-derived": the row is
		// offered to the pipeline once and the decision it then reaches records what it read.
		// A DEFAULT would make those rows MATCH whatever is current and stay excluded for
		// ever, precisely the silent no-op this column exists to end.
		//
		// The index on (status, decision_inputs) serves the survey the startup report and
		// `validate` read: it bounds that read to the two terminal statuses rather than
		// letting it walk a 300,000-row ledger on the engine's own serialized connection.
		name: "decision inputs",
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN decision_inputs TEXT;
CREATE INDEX IF NOT EXISTS idx_jobs_status_inputs ON jobs(status, decision_inputs);
`,
	},
	{
		// v11 - WHICH video stream the quality gate compared. v4 recorded the format both
		// streams were converted to; this records which streams those were, on a source that
		// can carry more than one.
		//
		// NO BACKFILL, and 'v:0' is the tempting default precisely because it is what this
		// build would now write: it would put a stream on rows nobody recorded one for, and
		// on rows whose VMAF gate never ran at all. No index either.
		name: "scored video stream",
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN vmaf_stream TEXT;
`,
	},
	{
		// v12 - which library profile decided the file. A library root now carries its own
		// encoder, crf, bitrate floor and VMAF floors, so the configuration has several
		// answers and the row has none. library_root is the cleaned root the file was
		// enumerated under and profile_digest identifies that root's resolved values; both,
		// because the path alone stops being interpretable the moment the profile is edited.
		//
		// Rows already in the field were decided by a build with one global profile and must
		// READ AS NOT RECORDED: a DEFAULT would attribute them to whichever root happens to
		// be configured now, or to a digest of a profile that did not exist when they were
		// written.
		name: "deciding library profile",
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN library_root   TEXT;
ALTER TABLE jobs ADD COLUMN profile_digest TEXT;
`,
	},
	{
		// v13 - the schema version each record was written under.
		//
		// Until this column a reader holding a row could not tell which build's semantics
		// filled it. It inferred the answer from ABSENT FIELDS, one column at a time - no
		// chroma means pre-GATE-4, no profile digest means pre-profiles - which is a guess
		// built on a coincidence, because every one of those columns is also legitimately
		// empty on a row the current build wrote. The stamp answers it outright, in the
		// three tables that hold a record: the job ledger, the retained originals an undo
		// window can put back, and the swap incidents.
		//
		// NULLABLE with NO DEFAULT, which is the rule v2 set and every step since has kept,
		// and here it is the whole point. There is NO BACKFILL: every record already in the
		// field was written by a build that stamped nothing, and it must READ as written
		// before the stamp existed. A DEFAULT - this build's version above all, because it
		// is the value the writer would now put there and therefore the tempting one -
		// would claim that this build wrote rows it never saw, in the tables whose whole
		// job is to be evidence about files that have since been deleted.
		//
		// No index. Nothing queries BY the stamp: every reader has the record in hand.
		name: "record version stamp",
		// Three columns and no row: an ALTER TABLE ADD COLUMN rewrites no row and creates
		// none, so every table's count is what it was.
		rows: noRowChange,
		sql: `
ALTER TABLE jobs               ADD COLUMN schema_version INTEGER;
ALTER TABLE retained_originals ADD COLUMN schema_version INTEGER;
ALTER TABLE swap_incidents     ADD COLUMN schema_version INTEGER;
`,
	},
	{
		// v14 - TRANSCODE-PROFILES: which ENCODE profile supplied a job's settings.
		//
		// Not the same fact as v12. That one names the library root a file was enumerated
		// under and digests the knobs that root resolved to; this one names the
		// pattern-matched profile, if any, whose overrides were then laid over them. A row
		// can carry both, one, or neither, and each answers a question the other cannot:
		// which tree judged this file, and which named set of overrides decided what its
		// encoder produced.
		//
		// It sits at the END of the history and not at the v8 it was written as. The dry-run
		// decision, the failure classifier, the re-opening rule, the scored stream, the
		// per-library profile and the record stamp have all shipped under the ordinals in
		// between, and a database in the field has already run those texts. Two steps
		// claiming one version fork the schema in two. Nothing about the SQL changes, only
		// where it sits.
		//
		// NULL is "no encode profile matched", which is the true answer for every row
		// written before they existed rather than a measurement nobody took. So NULL and ""
		// are one statement for this field, and no DEFAULT is needed to make an old row
		// honest.
		name: "transcode profile column",
		// One nullable column and no row: ADD COLUMN rewrites no row and creates none.
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN profile TEXT;
`,
	},
	{
		// v15 - the paths an operator has withheld from the pipeline.
		//
		// A SEPARATE TABLE, and the lifetimes force it exactly as they forced the retained
		// originals: a jobs row is keyed (path, fingerprint), Claim clears its outcome and a
		// successful transcode prunes it, while a withholding is an instruction about a PATH
		// and has to outlive every decision anybody takes about the bytes at it.
		//
		// It is RUNTIME STATE this daemon holds and nothing else. No configuration key is
		// added by this step or by anything that reads it: the accepted top-level key set is
		// closed, a reader of an unknown key refuses to start, and a path-withholding list in
		// the configuration file is its own feature rather than a profile of one that is not
		// there. Nothing here ever writes the configuration file.
		//
		// The path is the primary key, so recording the same path twice is one row and not
		// two, and the index is on created_at because the one read that is not a point lookup
		// is "every withheld path, oldest first".
		name: "withheld paths",
		// An empty table and one index. A withholding is recorded by an operator, never by a
		// migration, and a fabricated one would silently stop work on a file nobody named.
		rows: noRowChange,
		sql: `
CREATE TABLE IF NOT EXISTS path_exclusions (
	path           TEXT NOT NULL PRIMARY KEY,
	created_at     INTEGER NOT NULL,
	schema_version INTEGER
);
CREATE INDEX IF NOT EXISTS idx_path_exclusions_created ON path_exclusions(created_at);
`,
	},
	{
		// v16 - the path a replacement WOULD have been written to, which a dry-run decision
		// records and nothing else does.
		//
		// The other three facts such a decision carries were already storable: the source's
		// size is v2's source_bytes, its codec is v8's source_codec, and the target codec and
		// the encoder that would have run are v10's decision_inputs, where they belong because
		// the guard chain READ them. The target path is none of those - it is derived from the
		// source's own name and the container extension in force, so it is neither a
		// configuration value to compare nor a measurement anybody took - and until this step
		// it existed only in a log line, which is not a row.
		//
		// NULLABLE with NO DEFAULT, the rule every step since v2 has kept. Every row already
		// in the field records no target path and must READ as not recorded; deriving one for
		// them from the configuration now in force would name a path that build never chose.
		// No index: nothing queries BY it, every reader has the row in hand.
		name: "dry-run target path",
		// One nullable column: ADD COLUMN rewrites no row and creates none.
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN target_path TEXT;
`,
	},
	{
		// v17 - what a stream selection did: which source streams the job dropped, which
		// part of the selection it did NOT apply, and why the perceptual gate did not run.
		//
		// dropped_streams is the only record the dropped bytes leave. They are not
		// recoverable from the replacement, and the row outlives the source.
		//
		// NULLABLE with NO DEFAULT, the rule every step since v2 has kept, and here the
		// distinction is the whole point: a row written by an earlier build recorded no
		// stream-selection facts, and it must read as NOT RECORDED rather than as
		// "dropped nothing". A DEFAULT of '' or of the empty-set token would put that
		// fabricated claim on every existing row at once - a statement about jobs nobody
		// measured, in the one table whose entire job is to be evidence. "Dropped nothing"
		// is itself representable and is spelled by the writer, never by the schema (see
		// DroppedStreams.Encode).
		//
		// No index: nothing queries BY any of them, and every reader has the row in hand.
		name: "stream selection record",
		// Three nullable columns: ADD COLUMN rewrites no row and creates none.
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN dropped_streams        TEXT;
ALTER TABLE jobs ADD COLUMN selection_not_applied  TEXT;
ALTER TABLE jobs ADD COLUMN vmaf_skipped           TEXT;
`,
	},
	{
		// v18 - the pixel dimensions either side of a job: the source's, and the output's
		// where one was produced and measured.
		//
		// Resolution used to enter exactly one decision in this build - which VMAF model to
		// load - and no other, so nothing about it was worth a column. Per-band rules change
		// that: a file is now judged against the thresholds its source height selects, and a
		// row that does not say how tall its source was leaves an operator inferring the band
		// from the verdict, which is the wrong direction to read a ledger in.
		//
		// It sits at the END of the history and not at the v13 the spec that asked for it
		// named. Five steps have shipped under the ordinals in between and a database in the
		// field has already run their text, so two steps claiming one version would fork the
		// schema in two. Nothing about the SQL changes, only where it sits.
		//
		// NULLABLE with NO DEFAULT, the rule every step since v2 has kept, and here it is the
		// whole point: 0 is a legal pixel dimension for nothing. Every row already in the
		// field was written by a build that measured no dimension and must read as NOT
		// RECORDED; a DEFAULT of 0 would put a fabricated resolution on every one of them at
		// once, and a row whose source has since been deleted is the only record there is.
		//
		// No index: nothing queries BY a dimension, and every reader has the row in hand.
		name: "source and output resolution",
		// Four nullable columns: ADD COLUMN rewrites no row and creates none, so every
		// table's count is what it was. Declared rather than left off, which is what makes
		// applyMigration compare the counts either side of this step inside its own
		// transaction and roll the whole step back if one moved.
		rows: noRowChange,
		sql: `
ALTER TABLE jobs ADD COLUMN source_width  INTEGER;
ALTER TABLE jobs ADD COLUMN source_height INTEGER;
ALTER TABLE jobs ADD COLUMN output_width  INTEGER;
ALTER TABLE jobs ADD COLUMN output_height INTEGER;
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
//
// It RETURNS what it did: one record per step it applied, carrying the version that step
// stamped and every table's row count on either side of it. An up-to-date database
// returns none, which is the honest report of a migration that ran nothing.
func migrate(ctx context.Context, db *sql.DB) ([]MigrationStep, error) {
	have, err := readSchemaVersion(ctx, db)
	if err != nil {
		return nil, err
	}
	want := schemaVersion()

	// A database from the FUTURE is a refusal, never a silent downgrade. An older
	// binary against a newer schema does not "just work": it reads rows through a
	// narrower SELECT and — because Finish writes the full outcome column set — would
	// overwrite outcomes it cannot even see. Refusing to open is the safe move; the
	// operator rolls the binary forward (or the database back) and loses nothing
	// meanwhile.
	if have > want {
		return nil, errSchemaFromTheFuture(have, want)
	}

	var applied []MigrationStep
	for i := have; i < want; i++ {
		rec, err := applyMigration(ctx, db, i+1, migrations[i])
		if err != nil {
			return nil, fmt.Errorf("store: migration %d (%s): %w", i+1, migrations[i].name, err)
		}
		// A nil record is a step another writer had already applied while this one waited
		// for the lock. It moved nothing here, so there is nothing to report about it.
		if rec != nil {
			applied = append(applied, *rec)
		}
	}
	return applied, nil
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
//
// The ROW COUNTS ride in that same transaction, before and after the step, and so does
// the comparison against what the step declared. A count taken outside it would be a
// count of a database another writer could have moved in between, and a comparison made
// after the commit could only report a loss it had already made permanent. Inside, a step
// that moved a row it never declared is rolled back with the version it would have
// stamped, and the open is refused - which for a ledger whose terminal rows decide
// whether a source file may be deleted is the only safe direction.
func applyMigration(ctx context.Context, db *sql.DB, version int, m migration) (*MigrationStep, error) {
	// A dedicated connection: BEGIN/COMMIT are statements here rather than
	// database/sql's tx API (which offers no way to ask for IMMEDIATE), so they must
	// all land on the same connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("begin immediate: %w", err)
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
		return nil, fmt.Errorf("re-read user_version: %w", err)
	}
	if cur >= version {
		rollback() // nothing to do; another process applied it while we waited
		return nil, nil
	}

	before, err := tableRowCounts(ctx, conn)
	if err != nil {
		rollback()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, m.sql); err != nil {
		rollback()
		return nil, err
	}
	after, err := tableRowCounts(ctx, conn)
	if err != nil {
		rollback()
		return nil, err
	}
	rec, err := checkRowChanges(version, m, before, after)
	if err != nil {
		rollback()
		return nil, err
	}

	// PRAGMA takes no bound parameters, so the version is formatted into the text. It
	// is an int derived from len(migrations) — never anything a caller supplies — so
	// there is no injection surface here, only an API limitation.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		rollback()
		return nil, fmt.Errorf("stamp user_version: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback()
		return nil, fmt.Errorf("commit: %w", err)
	}
	return rec, nil
}

// TableRows is one table's row count immediately before and immediately after one
// migration step. A table that does not exist at one of those two moments holds no rows
// and counts zero there, which is what makes a created table and a dropped one
// comparable against a declaration in the same arithmetic as an altered one.
type TableRows struct {
	Table  string
	Before int64
	After  int64
}

// Changed is how far this table's row count moved across the step.
func (t TableRows) Changed() int64 { return t.After - t.Before }

// MigrationStep is the record of one step an open APPLIED: which step it was, the version
// it stamped, and every table's row count on either side of it, table-sorted.
//
// It exists because a migration that dropped or duplicated rows used to commit in
// silence and stamp a version claiming all was well. The counts are taken whether or not
// anything is wrong with them, so the record an operator reads after a clean upgrade is
// the same record that would have shown them the loss.
type MigrationStep struct {
	Version int
	Name    string
	Tables  []TableRows
}

// UndeclaredRowChangeError is the refusal a step earns by moving a table's row count by
// something it never declared. It is exported because the process that opened the store
// reports it distinctly from every other reason an open can fail: this one says the
// ledger was about to lose or gain evidence, and the step was rolled back rather than
// committed.
type UndeclaredRowChangeError struct {
	Version  int
	Step     string
	Table    string
	Declared int64
	Observed int64
	Before   int64
	After    int64
}

func (e *UndeclaredRowChangeError) Error() string {
	return fmt.Sprintf("table %q went from %d row(s) to %d row(s), a change of %+d, "+
		"where the step declares %+d - the step has been rolled back and the store will not open. "+
		"A step that moves rows it did not declare may have dropped or duplicated ledger "+
		"evidence, and a terminal row is what says a source file was already handled",
		e.Table, e.Before, e.After, e.Observed, e.Declared)
}

// checkRowChanges compares what the step DID to what it DECLARED, and returns the step's
// record when the two agree.
//
// Every table either side knows about is judged, and so is every table the declaration
// names: a table created by the step, a table it dropped, and a declaration naming a
// table that is not there are all row-count changes somebody has to have meant, and the
// first two are precisely the shapes a union of only the surviving tables would miss.
func checkRowChanges(version int, m migration, before, after map[string]int64) (*MigrationStep, error) {
	rec := &MigrationStep{Version: version, Name: m.name}
	for _, table := range judgedTables(before, after, m.rows) {
		t := TableRows{Table: table, Before: before[table], After: after[table]}
		declared := m.rows[table]
		if t.Changed() != declared {
			return nil, &UndeclaredRowChangeError{
				Version: version, Step: m.name, Table: table,
				Declared: declared, Observed: t.Changed(),
				Before: t.Before, After: t.After,
			}
		}
		// A table named ONLY by the declaration is not part of the record: it holds no
		// rows at either moment, so there is nothing about it to record.
		if _, wasThere := before[table]; wasThere {
			rec.Tables = append(rec.Tables, t)
			continue
		}
		if _, isThere := after[table]; isThere {
			rec.Tables = append(rec.Tables, t)
		}
	}
	return rec, nil
}

// judgedTables is the sorted union of the tables present before the step, present after
// it, and named by its declaration. Sorted so one step reads the same way on every run.
func judgedTables(before, after map[string]int64, declared rowChanges) []string {
	seen := make(map[string]struct{}, len(before)+len(after)+len(declared))
	for _, m := range []map[string]int64{before, after, declared} {
		for name := range m {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tableRowCounts counts the rows in every table the database holds, THROUGH conn, so the
// count is taken inside whatever transaction that connection is running - which is the
// only way a before-count and an after-count can bracket a step rather than bracket a
// step plus whatever else happened meanwhile.
//
// Every table means every table, SQLite's own sqlite_sequence and sqlite_stat1 included:
// a count that quietly skipped a table would be a count that could not notice a row
// appearing in it. Names come from sqlite_master, never from a caller, and an identifier
// takes no bound parameter, so each is quoted (with any embedded quote doubled) rather
// than bound.
func tableRowCounts(ctx context.Context, conn *sql.Conn) (map[string]int64, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("list tables: %w", err)
	}
	// Closed before the counts run: these share one connection, and a cursor still open
	// on it is a query the next statement would be interleaved with.
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}

	counts := make(map[string]int64, len(names))
	for _, name := range names {
		var n int64
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoted).Scan(&n); err != nil {
			return nil, fmt.Errorf("count rows in %q: %w", name, err)
		}
		counts[name] = n
	}
	return counts, nil
}

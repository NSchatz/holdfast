package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO; registers as "sqlite"
)

// SQLite is the production Store, backed by a single-writer SQLite/WAL database
// file. It implements Store.
type SQLite struct {
	db *sql.DB

	// applied is what the open that produced this handle did to the database: one record
	// per migration step it ran. It is written once, here, and read through
	// MigrationReport.
	applied []MigrationStep
}

// MigrationReport is what the open this handle came from ACTUALLY DID to the database:
// one record per step it applied, in the order it applied them, each carrying the version
// that step stamped and every table's row count immediately before and immediately after
// it.
//
// It is returned to whoever opened the store, rather than logged inside the package,
// because a migration is the one moment this ledger's shape changes underneath the
// evidence an operator audits after their originals have been deleted - so the process
// that opened it decides how that is reported, exactly as it does for a prune.
//
// It is EMPTY for a store that was already at this build's schema version, and empty for
// one opened read-only: that door applies no step, so it counts nothing.
func (s *SQLite) MigrationReport() []MigrationStep { return s.applied }

var _ Store = (*SQLite)(nil)

// uriPath renders a filesystem path for the `file:` DSN every door in this package opens
// through. It is NOT cosmetic and it is not a driver quirk: the DSN is a URI, and in a URI
// a '#' opens a fragment and a '?' opens a query, so a path containing either is silently
// TRUNCATED at it.
//
// What that costs without this: `state_dir: /srv/media#2` opens `/srv/media`, creates
// nothing at the path the operator wrote, and two installs whose state directories differ
// only after the '#' share ONE ledger - each reading the other's rows as its own. On a tool
// that deletes originals, the ledger is the record of what was retained and what was
// removed, so a handle that silently resolves somewhere else is a data-safety fault rather
// than an inconvenience.
//
// SQLite's own rule for a URI filename is that '?' and '#' are percent-encoded; '%' has to
// be encoded FIRST, or an operator's literal "%23" would come back as '#'. strings.Replacer
// makes one pass and never rescans what it wrote, which is exactly that ordering.
func uriPath(path string) string {
	return uriPathEscaper.Replace(path)
}

var uriPathEscaper = strings.NewReplacer("%", "%25", "#", "%23", "?", "%3F")

// Open creates the parent directory (if needed), opens (creating on first use) a
// WAL-mode SQLite database at path, and initializes the schema. dsn enables WAL +
// a busy timeout + foreign keys.
//
// db.SetMaxOpenConns(1) is the key line: it serializes every access to the
// database through a single connection, which is what actually prevents "database
// is locked" errors under concurrent workers — WAL allows concurrent readers, but
// with only ever one open connection there is never a second connection to
// contend with in the first place, so every Claim/Advance/Finish call is
// naturally atomic without needing an explicit transaction.
func Open(path string) (*SQLite, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %q: %w", dir, err)
		}
	}

	// synchronous=NORMAL is the documented-safe pairing with WAL mode: a commit is
	// still crash-safe (the WAL is fsynced at checkpoint, and SQLite guarantees the
	// database is never corrupted), it only relaxes the guarantee that the very
	// latest commit(s) survive an OS-level power loss immediately after they
	// return. The job store is a resumability/dedup aid, not the safety invariant
	// itself (that's the atomic rename in internal/engine) — losing the last
	// in-flight job's state on a hard power-cut just means it's reprocessed next
	// run, which is always safe. Default (FULL) fsyncs every single commit, which
	// under concurrent workers serializes on disk latency badly enough to make the
	// worker pool pointless.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", uriPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &SQLite{db: db}
	applied, err := migrate(context.Background(), db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.applied = applied
	return s, nil
}

// OpenReadOnly opens an EXISTING ledger for reading and never changes it (LEDGER-5).
//
// Open above MIGRATES: it is the daemon's door, and a daemon that is about to write
// through the current schema must have the current schema. A READER is the opposite
// case, and `holdfast export` is the reader this exists for. An export that opened the
// store the daemon's way would silently upgrade the operator's ledger as a side effect
// of reading it — and migrate() then REFUSES that file to the older holdfast that wrote
// it, because its user_version is ahead of that build. So the one command whose whole
// job is to preserve the record would be the command that made the record unreadable to
// the daemon still running against it.
//
// Two things stop that, and both are needed:
//
//   - mode=ro makes it a physical property of the handle rather than a promise about
//     the code above it: SQLite itself refuses every write, so no future caller can
//     quietly reintroduce one. Nothing is created either — a path that does not exist
//     is an error, never a fresh empty database.
//   - requireCurrentSchema refuses a version mismatch in BOTH directions instead of
//     repairing one. Ahead of this build was already a refusal (migrate's rule, for the
//     same reason: a narrower SELECT would not see every column). Behind it is now a
//     refusal too, because the alternative is either reading through a schema the file
//     does not have or migrating it, and migrating is exactly what a read must not do.
//     Upgrading the ledger stays a deliberate act: run `holdfast run` or `holdfast
//     serve` once, which is the path that has always owned the schema.
//
// SQLite may still create its own -wal/-shm sidecars beside the database while reading
// one in WAL mode, as any reader does; the database file itself — its rows, its schema
// and its version — is left exactly as it was found.
func OpenReadOnly(path string) (*SQLite, error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return nil, err
	}
	if err := requireCurrentSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// OpenSnapshot opens an EXISTING ledger for reading and CREATES NOTHING AT ALL, not even
// the -wal/-shm sidecars every ordinary reader leaves beside a WAL database.
//
// That is the one thing OpenReadOnly cannot promise, and it is what a command whose
// contract is "the state directory is byte-for-byte what it was" needs: `holdfast
// analyze` reports a census an operator may run before they have ever let this tool
// touch a file, and a reader that added two files to their state directory would have
// mutated the very thing it claims not to. SQLite's `immutable=1` is what buys that - it
// tells SQLite the file cannot change underneath it, so no shared-memory index and no
// write-ahead log are opened, and locking is skipped entirely.
//
// THE COST, which is why this is NOT the default reader: a database a daemon is writing
// to right now has content in its -wal that this handle does not see, so what comes back
// is the last CHECKPOINTED state rather than the newest. That is acceptable for exactly
// one kind of caller - one that reports on the filesystem and reads the ledger only to
// say which paths a record holds back - and unacceptable for any caller whose answer
// decides a mutation. `export` and `validate` keep OpenReadOnly for that reason: their
// job is the record itself, and a record read one checkpoint short is the wrong record.
func OpenSnapshot(path string) (*SQLite, error) {
	// Deliberately not openReadOnlyDB's DSN: immutable is the whole point here, and a
	// shared helper that sometimes sets it would make "did this open create a file?"
	// a question about an argument rather than about which function was called.
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1&_pragma=query_only(1)", uriPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q as an unchanging snapshot: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := requireCurrentSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// openReadOnlyDB is the read-only handle itself, with no schema rule attached. It is
// shared so the physical read-only property has ONE definition: every reader in this
// package gets mode=ro whatever it then decides about the schema it is looking at.
func openReadOnlyDB(path string) (*sql.DB, error) {
	// No MkdirAll and no create: a read that finds nothing to read says so. The
	// pragmas are deliberately not Open's — journal_mode and synchronous are writes to
	// the header, and a reader has no business setting either. query_only is belt and
	// braces beside mode=ro, and it costs nothing.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)", uriPath(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// New wraps an already-open *sql.DB (test seam — e.g. an in-memory database) and
// migrates the schema up to date. The caller is responsible for any connection-limit
// pragmas it wants (Open sets MaxOpenConns(1); New leaves db as given).
func New(db *sql.DB) (*SQLite, error) {
	s := &SQLite{db: db}
	applied, err := migrate(context.Background(), db)
	if err != nil {
		return nil, err
	}
	s.applied = applied
	return s, nil
}

// Close releases the underlying database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// now is a seam for tests; production always uses time.Now — the no-Date rule
// applies to workflow scripts, not normal Go program code.
var now = func() int64 { return time.Now().Unix() }

// RecoverStale resets any job left in an active state back to pending. Call once
// at startup before any scan — an active row is only ever left behind by a worker
// that crashed or was killed mid-job; the source file itself was never touched
// (the swap is the only mutation and runs strictly after verify).
func (s *SQLite) RecoverStale(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, worker = NULL, updated_at = ?, schema_version = ?
		 WHERE status IN (?, ?, ?)`,
		string(Pending), now(), currentStamp(), string(Probing), string(Encoding), string(Verifying))
	if err != nil {
		return 0, fmt.Errorf("store: recover stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: recover stale rows affected: %w", err)
	}
	return int(n), nil
}

// Claim is documented on the Store interface. The read-then-write is wrapped in an
// explicit transaction: MaxOpenConns(1) guarantees there is only ever one
// connection to the database, but database/sql can still interleave separate
// pooled Query/Exec calls from different goroutines onto that one connection
// between a plain SELECT and a follow-up INSERT/UPDATE — without a transaction two
// concurrent Claim calls on the SAME fresh key can both observe "no row" and both
// attempt to INSERT (one wins, one gets a UNIQUE-constraint error; worse, on an
// existing row both could observe "pending" and both attempt to UPDATE, which
// would hand the same job to two workers). The transaction (SQLite's default
// isolation locks the database for its duration) makes the whole read-modify-write
// atomic, which is what actually delivers the "exactly one claimant" guarantee.
func (s *SQLite) Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int,
	current DecisionInputs, supersede ...string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: claim begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if already committed

	var status string
	var failCount int
	// The reason and the recorded inputs are read INSIDE the transaction with the status,
	// because the re-opening decision is taken from all three together and a second read
	// outside it would be the read-modify-write race this transaction exists to close.
	var reason, inputs sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT status, fail_count, reason, decision_inputs FROM jobs WHERE path = ? AND fingerprint = ?`,
		path, fingerprint).Scan(&status, &failCount, &reason, &inputs)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Never seen before: claim it fresh.
		return claimFresh(ctx, tx, path, fingerprint, worker)
	case err != nil:
		return false, fmt.Errorf("store: claim select: %w", err)
	}

	st := Status(status)
	switch {
	case st == Skipped && supersededBy(reason.String, supersede):
		// A MUTABLE guard's own row, re-evaluated. The caller reached this call without
		// that guard firing, which is the guard saying its condition no longer holds, so
		// the row goes and the file re-enters the pipeline exactly as an unseen one does.
		//
		// It is HERE rather than in a ClearSkip beside the call because the status and the
		// reason are already read, in this transaction, to decide whether the file is
		// claimable at all. Asking a DELETE the same question outside it costs a write
		// statement for every file of every scan - on a processed library, one per mutable
		// guard per file per pass, for ever, to match no row - and it is a write the
		// caller can only issue by guessing, since it does not know what the row says.
		// Folded in, the write is issued exactly where there is a row to write.
		//
		// Deleting and re-inserting, rather than falling through to the update below, is
		// what keeps this identical to the clear-then-claim it replaces: the attempt count
		// goes back to zero with the row, as it does for any file this build has not seen.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM jobs WHERE path = ? AND fingerprint = ?`, path, fingerprint); err != nil {
			return false, fmt.Errorf("store: claim clear superseded skip: %w", err)
		}
		return claimFresh(ctx, tx, path, fingerprint, worker)
	case st == Done || st == Skipped:
		if !reopens(st, reason.String, ParseDecisionInputs(inputs.String), current) {
			return false, nil // terminal, and its decision still re-derives the same way
		}
		// RE-OPENED. The configuration this verdict was taken under has moved, or the row
		// records nothing to re-derive it from, so the file goes back to the pipeline
		// exactly as an unseen file does. What that costs is one pass of the guards: a
		// file that would reach the same verdict reaches it before anything is encoded.
		//
		// A DONE row's contribution to the lifetime reclaimed total is carried forward
		// first, because the claim below clears the sizes it was computed from. That is
		// the same carry the prune makes for the same reason (see PruneTerminal): the
		// published total is a sum over live done rows, so a row that stops being one
		// would take its bytes out of a figure an operator uses to judge whether the tool
		// was worth running - and it would do so at the next restart, not at the claim.
		if err := carryReclaimed(ctx, tx, path, fingerprint); err != nil {
			return false, err
		}
		// fall through to claim
	case st == Indeterminate:
		// PARKED. Whether the swap was applied is exactly what is unknown, so
		// re-claiming would mean re-encoding and re-swapping a path that may already
		// hold the replacement. Nothing here moves until an operator records a
		// determination, which RELEASES the job by removing this row (see
		// ResolveIncident) rather than by making it claimable again.
		return false, nil
	case st == AppliedDespiteError:
		// The rename took effect: the file at this path is the replacement, and this
		// row describes an attempt that is over. It is never re-attempted ON THE
		// STRENGTH OF THIS JOB. The path itself is not held back - the file there now
		// has a different size/mtime, so a later scan keys it as NEW work and the
		// ordinary already-at-target-codec guard is what skips it.
		return false, nil
	case st == WouldTranscode:
		// A DRY RUN's recorded decision, and it is RE-CLAIMABLE. The row says "a run
		// allowed to transcode would take this file"; the run that is allowed to must
		// therefore be able to take it, or turning dry_run off would leave every file the
		// dry run examined excluded for ever - the library never transcoded and nothing on
		// the dashboard saying why. A LATER DRY RUN re-claims it for the same reason, which
		// is also what keeps two successive dry runs reporting ONE candidate row for a file
		// rather than two.
		//
		// Nothing about the file was touched to get here, so there is nothing to undo: the
		// update below clears the outcome columns exactly as it does for a Failed retry and
		// the new attempt writes its own. fail_count is untouched, deliberately - a dry-run
		// decision is not an attempt that went wrong, so it must never consume a retry.
		//
		// fall through to claim
	case st == Failed:
		if failCount >= maxFailures {
			return false, nil // parked
		}
		// fall through to claim (retry)
	case st.Active():
		return false, nil // held by another worker, or stale (awaiting RecoverStale)
	case st == Pending:
		// fall through to claim
	default:
		// Unrecognized status: fail safe — do not claim.
		return false, fmt.Errorf("store: claim: unrecognized status %q for %s", status, path)
	}

	// Claiming BEGINS A NEW ATTEMPT, so it clears the outcome columns. Only a retry of
	// a Failed row can reach here with an outcome already on it, and that outcome
	// describes the PREVIOUS attempt — an encode that was rejected and whose temp was
	// deleted. Leaving it in place would mean a job sitting in probing/encoding/
	// verifying (for hours) still carrying the failed attempt's reason and, worse, its
	// VMAF score: /api/queue projects the same columns as /api/history, so an in-flight
	// file would be served with a fidelity number belonging to an encode that no longer
	// exists. A fabricated score is exactly what this schema exists to prevent, and the
	// rule in Finish's doc — a row's proof always describes its CURRENT status — has to
	// hold on the way IN as well as on the way out.
	//
	// The failure class is cleared with the rest of them, and note what that means: the
	// class is NEVER read by this method. Every decision above is taken on the status
	// and the attempt count alone, so a failed row carrying the deterministic class and
	// a count below the bound is claimed exactly like any other retry - the class is not
	// a claim blocker, and whatever lowers a count to restore an exhausted failure
	// therefore restores a parked deterministic one by the same act, with no second
	// block to clear.
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, worker = ?, updated_at = ?, schema_version = ?,
			reason = NULL, encoder = NULL, vmaf_mean = NULL, vmaf_min = NULL, vmaf_model = NULL,
			vmaf_pix_fmt = NULL, vmaf_chroma = NULL, vmaf_chroma_metric = NULL, vmaf_stream = NULL,
			source_codec = NULL, source_bytes = NULL, output_bytes = NULL, encode_ms = NULL,
			target_path = NULL,
			guard_attributes = NULL, guard_time_resolution = NULL, guard_residual_window = NULL,
			swap_cause = NULL, failure_class = NULL, decision_inputs = NULL,
			library_root = NULL, profile_digest = NULL, profile = NULL,
			dropped_streams = NULL, selection_not_applied = NULL, vmaf_skipped = NULL,
			source_width = NULL, source_height = NULL, output_width = NULL, output_height = NULL,
			deinterlaced = NULL, deinterlace_filter = NULL,
			downscaled = NULL, downscale_scaler = NULL,
			vmaf_scored_width = NULL, vmaf_scored_height = NULL
		 WHERE path = ? AND fingerprint = ?`,
		string(Probing), worker, now(), currentStamp(), path, fingerprint); err != nil {
		return false, fmt.Errorf("store: claim update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: claim commit: %w", err)
	}
	return true, nil
}

// claimFresh writes the row a file this build has not seen gets, and commits. It is the
// one spelling of that INSERT: a claim reaches it both for a path with no row at all and
// for one whose mutable-guard skip was just superseded, and those two must stay the same
// row or the second would quietly carry something forward from the verdict it replaced.
func claimFresh(ctx context.Context, tx *sql.Tx, path, fingerprint, worker string) (bool, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, schema_version)
		 VALUES (?, ?, ?, 0, ?, ?, ?)`,
		path, fingerprint, string(Probing), worker, now(), currentStamp()); err != nil {
		return false, fmt.Errorf("store: claim insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: claim commit: %w", err)
	}
	return true, nil
}

// supersededBy reports whether a skipped row's reason is one this claim was told it may
// clear. An empty reason is never one: a row carrying no reason at all records no guard,
// so there is no mutable condition behind it to have resolved.
//
// A `restored-original` row is never one either, WHATEVER the caller names, and that
// refusal is here rather than left to the caller's list on purpose. It is the row an
// operator wrote by deliberately putting their file back through the undo window, and
// re-opening it feeds those rescued bytes to the very gates that passed the encode they
// rejected. reopens refuses it first and without reading anything else; this branch runs
// before reopens, so it owes the same refusal or it is a way around it.
func supersededBy(reason string, supersede []string) bool {
	if reason == "" || reason == GuardRestoredOriginal {
		return false
	}
	for _, r := range supersede {
		if r == reason {
			return true
		}
	}
	return false
}

// reopens decides whether a DONE or SKIPPED row goes back to the pipeline. It is the
// whole of the re-opening rule, in one place, so Claim reads as the sequence of cases it
// always was.
//
// A `restored-original` row is refused FIRST and without reading anything else. It is a
// file an operator deliberately put back through the undo window, and re-opening it would
// feed their rescued bytes to the very gates that passed the encode they rejected. It is
// written by RecordSkip, which records no inputs, so the inputs rule alone would re-open
// it on the next scan - this is the check that stops that, and it is one of two (Reopen
// refuses the same row, so requeue cannot reach it either).
//
// Everything else is the inputs rule: a record that still matches holds the file out
// exactly as it did before this column existed, and anything else - a value that moved, a
// key this build no longer offers, a row that records nothing at all - is a verdict that
// cannot be re-derived, so the file is offered to the pipeline and the guards decide it
// again.
func reopens(st Status, reason string, recorded, current DecisionInputs) bool {
	if st == Skipped && reason == GuardRestoredOriginal {
		return false
	}
	return !recorded.StillMatches(current)
}

// carryReclaimed moves one row's contribution to the lifetime reclaimed total into the
// durable carry-forward, BEFORE a claim clears the sizes it is computed from. It is a
// no-op for every row that is not a done row recording both sizes, which is every row
// but the one case it exists for.
//
// The arithmetic is spelled in SQL rather than read-then-written in Go so it is atomic
// against the claim it rides with, and clamped at zero so a future bug in the
// strictly-smaller gate can never make a lifetime total run backwards - the same clamp
// ReclaimedTotal already applies on the way out.
//
// It does NOT check that the ledger_totals row was there to carry into, where the prune
// does. The singleton is seeded by the migration that creates it, so its absence is not a
// state this build can produce - and the two failures are not comparable: a prune that
// cannot carry must not delete, which is one pass of bookkeeping declined, while a claim
// that refused would stop every file in the library entering the pipeline.
func carryReclaimed(ctx context.Context, tx *sql.Tx, path, fingerprint string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE ledger_totals SET reclaimed_pruned = reclaimed_pruned + (
			SELECT MAX(COALESCE(SUM(source_bytes - output_bytes), 0), 0) FROM jobs
			WHERE path = ? AND fingerprint = ? AND status = ?
			  AND source_bytes IS NOT NULL AND output_bytes IS NOT NULL
		 ) WHERE id = 1`,
		path, fingerprint, string(Done)); err != nil {
		return fmt.Errorf("store: claim carry reclaimed total: %w", err)
	}
	return nil
}

// Reopen is documented on the Store interface.
func (s *SQLite) Reopen(ctx context.Context, path, fingerprint string, clearFailures bool) (bool, error) {
	q := `UPDATE jobs SET decision_inputs = NULL, updated_at = ?, schema_version = ?`
	if clearFailures {
		q += `, fail_count = 0`
	}
	// The three refusals are in the statement itself, not in the caller. Requeue widens
	// the set of files the encoder may touch, which in this repository is the single most
	// destructive thing that can be done, so the rows that must never be re-opened are
	// refused where the write happens as well as where it is decided.
	q += ` WHERE path = ? AND fingerprint = ?
		AND status NOT IN (?, ?)
		AND NOT (status = ? AND reason = ?)`
	res, err := s.db.ExecContext(ctx, q, now(), currentStamp(), path, fingerprint,
		string(Indeterminate), string(AppliedDespiteError), string(Skipped), GuardRestoredOriginal)
	if err != nil {
		return false, fmt.Errorf("store: reopen: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: reopen rows affected: %w", err)
	}
	return n > 0, nil
}

// TerminalHolds is documented on the Store interface.
//
// It answers with the SAME two predicates Claim decides by - supersededBy and reopens - and
// that is the whole reason it lives here rather than in the caller. A second reading of the
// re-opening rule anywhere else in this repository would be a second answer to "is this row
// still terminal", and the two would drift on the first change to either.
//
// It is a pure read on the ordinary (path, fingerprint) index's leading column, and it reads
// every row at the path rather than one: a file that was re-downloaded leaves the old row
// behind at the old key, so the question "which fingerprints would be refused here" has more
// than one possible answer and the caller holds the one that decides it.
func (s *SQLite) TerminalHolds(ctx context.Context, path string, current DecisionInputs,
	supersede ...string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, status, reason, decision_inputs FROM jobs WHERE path = ?`, path)
	if err != nil {
		return nil, fmt.Errorf("store: terminal holds: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var held []string
	for rows.Next() {
		var fingerprint, status string
		var reason, inputs sql.NullString
		if err := rows.Scan(&fingerprint, &status, &reason, &inputs); err != nil {
			return nil, fmt.Errorf("store: terminal holds scan: %w", err)
		}
		// Only the two statuses a DECISION INPUT can re-open. Every other refusal Claim
		// makes - a parked failure, an active row, an indeterminate incident - is left to
		// Claim, because none of them is what this read exists to see coming and each is a
		// state the caller must not learn to reason about for itself.
		st := Status(status)
		if st != Done && st != Skipped {
			continue
		}
		if supersededBy(reason.String, supersede) {
			continue
		}
		if reopens(st, reason.String, ParseDecisionInputs(inputs.String), current) {
			continue
		}
		held = append(held, fingerprint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: terminal holds rows: %w", err)
	}
	return held, nil
}

// SurveyDecisionInputs is documented on the Store interface.
//
// It reads a row at a time rather than grouping by the stored value, and it has to: the
// configuration in force resolves per PATH, so two rows carrying the identical recorded
// text can be one still-matching row and one moved row. Grouping would answer a question
// about the ledger that no row was decided by.
//
// The cost that buys back is bounded on both sides. The scan is over the terminal
// partition through the same index the group-by used, the decode of each stored value is
// memoized on its text (a library is decided under a handful of distinct records, not one
// per row), and the resolver memoizes its own side. What is left is a comparison of a few
// keys per row, which is microseconds against the encode a re-opened row might cost.
//
// A `restored-original` row is left out of all the counts. It records no inputs, so it
// would otherwise arrive in NotRecorded - and this figure is read as "what the next scan
// will re-open", which that row never is, under any configuration.
func (s *SQLite) SurveyDecisionInputs(ctx context.Context, current InputsForPath) (DecisionInputsSurvey, error) {
	where, args := surveyedRows(true)
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, decision_inputs FROM jobs WHERE `+where, args...)
	if err != nil {
		return DecisionInputsSurvey{}, fmt.Errorf("store: survey decision inputs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return classifyRecordedInputs(rows, current)
}

// surveyedRows is the WHERE both surveys count over, and the reason they must agree: the
// two figures a run announces and the two `validate` prints are the same statement about
// the same ledger, so one of them reading a wider or narrower row set than the other would
// be a disagreement an operator has no way to resolve.
//
// hasReason is false only for a ledger older than the outcome columns, which cannot hold a
// restored-original row at all - that guard is many migrations younger than they are - so
// leaving the clause off such a file excludes nothing that is there.
func surveyedRows(hasReason bool) (string, []any) {
	where := `status IN (?, ?)`
	args := []any{string(Done), string(Skipped)}
	if hasReason {
		where += ` AND NOT (status = ? AND reason = ?)`
		args = append(args, string(Skipped), GuardRestoredOriginal)
	}
	return where, args
}

// classifyRecordedInputs turns (path, decision_inputs) rows into the survey. It is the one
// place the three-way reading of a stored record lives, so the startup report and
// `validate` cannot classify the same row differently - and it is the one place the survey
// resolves the configuration in force, so neither of them can classify a row differently
// from the claim that will meet it.
//
// EVERY row's path is resolved, whatever that row recorded. What a record says and where
// its file is are two questions: a row that recorded nothing is re-opened once by the rule,
// and a row under no configured library root is never enumerated by a scan, so a row that is
// both is a re-open that will never happen. Saying so needs the annotation taken for that
// row too - and that row is not an edge, it is every row of a ledger an earlier build wrote,
// which is the population these figures are read for. The row's own classification is
// untouched by the resolution: not recorded stays not recorded.
func classifyRecordedInputs(rows *sql.Rows, current InputsForPath) (DecisionInputsSurvey, error) {
	var out DecisionInputsSurvey
	parsed := map[string]DecisionInputs{}
	for rows.Next() {
		var path string
		var recorded sql.NullString
		if err := rows.Scan(&path, &recorded); err != nil {
			return DecisionInputsSurvey{}, fmt.Errorf("store: survey decision inputs scan: %w", err)
		}
		in, ok := parsed[recorded.String]
		if !ok {
			in = ParseDecisionInputs(recorded.String)
			parsed[recorded.String] = in
		}
		now, rooted := current(path)
		if !rooted {
			out.Unrooted++
			if out.UnrootedExample == "" || path < out.UnrootedExample {
				out.UnrootedExample = path
			}
		}
		switch {
		case !in.Recorded():
			out.NotRecorded++
		case in.StillMatches(now):
			out.Matching++
		default:
			out.Moved++
		}
	}
	if err := rows.Err(); err != nil {
		return DecisionInputsSurvey{}, fmt.Errorf("store: survey decision inputs rows: %w", err)
	}
	return out, nil
}

// Advance records a non-terminal state transition for a job the caller already
// holds.
func (s *SQLite) Advance(ctx context.Context, path, fingerprint string, st Status) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, updated_at = ?, schema_version = ?
		 WHERE path = ? AND fingerprint = ?`,
		string(st), now(), currentStamp(), path, fingerprint); err != nil {
		return fmt.Errorf("store: advance: %w", err)
	}
	return nil
}

// Finish is documented on the Store interface. Failed increments fail_count.
//
// The outcome columns are written UNCONDITIONALLY from o (nil o => all NULL), never
// merged into whatever was there before. See the interface doc: a retried job that
// finally succeeds must not carry the previous attempt's failure reason next to its
// "done", and the only way to guarantee that without a special case per column is to
// let every Finish fully define the row's proof.
func (s *SQLite) Finish(ctx context.Context, path, fingerprint string, st Status, o *Outcome, maxFailures int) error {
	if o == nil {
		o = &Outcome{}
	}
	// A "" string is stored as NULL, not as an empty string, so "not recorded" has ONE
	// representation in the column rather than two the readers would both have to know
	// about.
	if _, err := s.db.ExecContext(ctx, finishQuery(st, o, maxFailures),
		finishArgs(st, o, path, fingerprint)...); err != nil {
		return fmt.Errorf("store: finish: %w", err)
	}
	return nil
}

// finishQuery and finishArgs are shared by Finish and by RecordSwapIncident's
// transaction, so the "every Finish fully defines the row's proof" rule cannot hold on
// one path and quietly lapse on the other.
//
// The attempt count is where a deterministic failure is parked, and it is parked IN THIS
// WRITE - the same statement that records the status and the proof - so there is no
// window in which the row says "failed, final" while still offering attempts. Two
// details of the arithmetic are load-bearing:
//
//   - MAX(fail_count + 1, ?) and not `= ?`. The count only ever RISES: a bound
//     misconfigured to 0 or below is refused before this (maxFailures > 0), and a row
//     that had somehow already passed the bound is not reduced to it. A park may never
//     hand a file back to the encoder.
//   - It is spelled in SQL rather than read-then-written in Go, so it is atomic against
//     the increment itself.
//
// A transient failure takes the ordinary +1, and a non-Failed status does not touch the
// count at all, exactly as before. The bound is formatted into the statement rather than
// bound as a parameter only because finishArgs is shared with the incident path and its
// argument list has to stay fixed; it is an int the caller passed, never text, so there
// is no injection surface - the same reasoning applyMigration's PRAGMA already rests on.
func finishQuery(st Status, o *Outcome, maxFailures int) string {
	q := `UPDATE jobs SET status = ?, updated_at = ?, schema_version = ?,
		reason = ?, encoder = ?, vmaf_mean = ?, vmaf_min = ?, vmaf_model = ?,
		vmaf_pix_fmt = ?, vmaf_chroma = ?, vmaf_chroma_metric = ?, vmaf_stream = ?,
		source_codec = ?, source_bytes = ?, output_bytes = ?, encode_ms = ?,
		target_path = ?,
		guard_attributes = ?, guard_time_resolution = ?, guard_residual_window = ?,
		swap_cause = ?, failure_class = ?, decision_inputs = ?,
		library_root = ?, profile_digest = ?, profile = ?,
		dropped_streams = ?, selection_not_applied = ?, vmaf_skipped = ?,
		source_width = ?, source_height = ?, output_width = ?, output_height = ?,
		deinterlaced = ?, deinterlace_filter = ?,
		downscaled = ?, downscale_scaler = ?,
		vmaf_scored_width = ?, vmaf_scored_height = ?`
	switch {
	case st != Failed:
	case o.FailureClass.Final() && maxFailures > 0:
		q += fmt.Sprintf(`, fail_count = MAX(fail_count + 1, %d)`, maxFailures)
	default:
		q += `, fail_count = fail_count + 1`
	}
	return q + ` WHERE path = ? AND fingerprint = ?`
}

// finishArgs binds the values finishQuery's placeholders expect, in that order.
//
// The class is written ONLY on a failed row, and it is written through Class() so what
// lands in the column is always a member of the closed vocabulary: no build of holdfast
// can store a third token, and a done or skipped row carries none at all rather than a
// meaningless "transient". Reading is normalised too (outcomeScan.outcome), which is what
// covers the rows this code did not write - one from an older build, or one an operator's
// repair script edited.
func finishArgs(st Status, o *Outcome, path, fingerprint string) []any {
	class := ""
	if st == Failed {
		class = string(o.FailureClass.Class())
	}
	return []any{
		string(st), now(), currentStamp(),
		nullString(o.Reason), nullString(o.Encoder),
		nullFloat(o.VmafMean), nullFloat(o.VmafMin), nullString(o.VmafModel),
		nullString(o.VmafPixFmt), nullFloat(o.VmafChroma), nullString(o.VmafChromaMetric),
		nullString(o.VmafStream),
		nullString(o.SourceCodec), nullInt(o.SourceBytes), nullInt(o.OutputBytes), nullInt(o.EncodeMs),
		nullString(o.TargetPath),
		nullString(o.GuardAttributes), nullString(o.GuardTimeResolution),
		nullString(o.GuardResidualWindow), nullString(o.SwapCause), nullString(class),
		nullString(o.DecisionInputs.Encode()),
		nullString(o.LibraryRoot), nullString(o.ProfileDigest), nullString(o.Profile),
		nullString(o.DroppedStreams.Encode()),
		nullString(o.SelectionNotApplied), nullString(o.VmafSkipped),
		nullPixels(o.SourceWidth), nullPixels(o.SourceHeight),
		nullPixels(o.OutputWidth), nullPixels(o.OutputHeight),
		nullBool(o.Deinterlaced), nullString(o.DeinterlaceFilter),
		nullBool(o.Downscaled), nullString(o.DownscaleScaler),
		nullPixels(o.VmafScoredWidth), nullPixels(o.VmafScoredHeight),
		path, fingerprint,
	}
}

// --- NULL helpers -------------------------------------------------------------
//
// "Not recorded" is NULL in the column, and NULL only. These four keep that mapping
// in one place instead of scattering sql.Null* literals through the queries.

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func nullInt(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}

// nullBool maps a tri-state boolean to the column: NULL for "nobody measured this", and 0
// or 1 for a measurement. It is its own helper rather than a cast at the call site because
// the distinction is the whole reason the field is a pointer - a false written where nil
// was meant is a claim about a job nobody ran.
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// nullPixels is nullInt for a pixel dimension. It is its own helper rather than a cast at
// each call site because the four dimensions are *int - a dimension is a count of pixels,
// not a file size - and a nil one must reach the column as NULL: 0 is a legal dimension
// for nothing, so a zero written here would be a resolution nobody measured.
func nullPixels(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// jobColumns is the projection every reader of a WHOLE job row uses, and scanJob below
// is the only thing that reads it. The two exist together: a hand-written column list
// beside a hand-written scan is the shape that silently mis-binds the next time a column
// is appended, and the export's promise (never a narrower row than the API publishes)
// rests on both readers projecting the same thing.
const jobColumns = `path, fingerprint, status, fail_count, worker, updated_at, schema_version,
	` + outcomeColumns

// scanJob reads one row in jobColumns order.
//
// worker is NULL for a pending or recovered row, every outcome column is nullable
// ("not recorded" must never scan into a bare 0 or "" that a reader would take for a
// measurement), and the version stamp is read through stampScan, which tolerates whatever
// is in the column rather than failing the whole record over it.
func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var status string
	var worker sql.NullString
	var stamp stampScan
	var oc outcomeScan
	dest := append([]any{
		&j.Path, &j.Fingerprint, &status, &j.FailCount, &worker, &j.UpdatedAt, stamp.dest(),
	}, oc.dest()...)
	if err := sc.Scan(dest...); err != nil {
		return Job{}, err
	}
	j.Status = Status(status)
	j.Worker = worker.String
	j.Stamp = stamp.stamp()
	j.Outcome = oc.outcome()
	return j, nil
}

// outcomeColumns is the column list every reader of the outcome projects, in ONE
// place, so the SELECT text and the scan destinations cannot drift apart when a
// column is appended. Its order is the order outcomeScan expects.
const outcomeColumns = `reason, encoder, vmaf_mean, vmaf_min, vmaf_model,
	vmaf_pix_fmt, vmaf_chroma, vmaf_chroma_metric, vmaf_stream,
	source_codec, source_bytes, output_bytes, encode_ms, target_path,
	guard_attributes, guard_time_resolution, guard_residual_window, swap_cause,
	failure_class, decision_inputs,
	library_root, profile_digest, profile,
	dropped_streams, selection_not_applied, vmaf_skipped,
	source_width, source_height, output_width, output_height,
	deinterlaced, deinterlace_filter,
	downscaled, downscale_scaler, vmaf_scored_width, vmaf_scored_height`

// outcomeScan holds one row's outcome columns on the way out of the driver. Every
// field is a sql.Null* because every column is nullable: NULL is "not recorded" and
// must not be scanned into a bare 0/"" a reader would mistake for a measurement.
//
// It exists as a struct rather than a list of locals because the column set grows
// (v2 added eight, GATE-4 three, FILESYSTEM-1 four) and a positional scan is the
// shape that silently mis-binds when it does - swap two same-typed columns in the
// argument list and the compiler is happy while a VMAF score arrives in the chroma
// field, or a residual window in the swap cause.
type outcomeScan struct {
	reason, encoder, model    sql.NullString
	pixFmt, chromaMetric      sql.NullString
	mean, worst, chroma       sql.NullFloat64
	srcBytes, outBytes, encMs sql.NullInt64

	// Which video stream the comparison was made against. Nullable like every other
	// outcome column: a row written before it existed, and any row whose VMAF gate never
	// ran, reads as not recorded rather than as the stream this build would have scored.
	stream sql.NullString

	// The source's own video codec, recorded by a dry-run decision. Nullable like every
	// other outcome column: a row written before it existed, and any row that never
	// probed a codec, reads as not recorded.
	srcCodec sql.NullString

	// The path a replacement WOULD have been written to, recorded by a dry-run decision.
	// Nullable like every other outcome column, and here the distinction is the whole of it:
	// a row that recorded none must never read as a path, because no file was ever written
	// at one.
	targetPath sql.NullString

	// The source-mutation guard's achieved granularity and the distinctly-reported
	// cause of a failed swap (FILESYSTEM-1). All four are strings and all four are
	// nullable: a job that never reached the guard recorded no window, and a swap that
	// never failed has no cause.
	guardAttrs, guardRes, guardWindow, swapCause sql.NullString

	// The class of a terminal failure. Nullable like the rest, and the ONE column whose
	// NULL is not surfaced as "not recorded": there is no unclassified failure, so it
	// resolves to the transient class on the way out (see outcome).
	failClass sql.NullString

	// What the decision that wrote this row read from the configuration. Nullable, and
	// here NULL is the state the whole column exists to keep distinguishable: a row
	// written before it existed recorded nothing, and nothing is not an empty set.
	inputs sql.NullString

	// Which library profile decided the row. Nullable like every other outcome column:
	// a row written before per-library profiles existed was decided by a build that had
	// one global profile and recorded neither fact, and must read as not recorded.
	libraryRoot, profileDigest sql.NullString

	// The ENCODE profile that supplied this job's settings - the pattern-matched
	// overrides laid over the library profile above, not the library profile itself.
	// Nullable like the rest, and here NULL and "" say the same thing: none matched.
	profile sql.NullString

	// What the stream selection did. Nullable like the rest, and for dropped_streams the
	// NULL is the state the column exists to keep distinguishable: a row written before
	// it existed recorded nothing about what it dropped, and nothing is not "dropped
	// nothing".
	dropped, notApplied, vmafSkipped sql.NullString

	// The pixel dimensions either side of the job. Nullable like the rest, and here the
	// NULL is load-bearing twice over: a row written before these columns existed measured
	// nothing, and a row whose guard fired before any probe - or whose job produced no
	// output - measured nothing either. Scanning one into a bare 0 would hand a reader a
	// resolution nobody took.
	srcWidth, srcHeight, outWidth, outHeight sql.NullInt64

	// Whether this job deinterlaced its source, and with which filter. Nullable like the
	// rest, and here the NULL is the whole of it: a row written by a build that could not
	// deinterlace at all measured nothing, and scanning one into a bare false would say that
	// somebody looked and found no deinterlace on a job nobody ran.
	deinterlaced sql.NullBool
	deintFilter  sql.NullString

	// Whether this job scaled its picture down, with which resampler, and the resolution the
	// perceptual comparison was then made at. Nullable for the reasons the pair above is,
	// with one extra: the scored resolution is NULL on every job that scaled nothing, whose
	// comparison was made at the size outWidth and outHeight already carry - so here NULL
	// means "not a separate fact" as well as "not recorded", and both read back as nothing.
	downscaled       sql.NullBool
	downscaleScaler  sql.NullString
	vmafScoredWidth  sql.NullInt64
	vmafScoredHeight sql.NullInt64
}

// dest returns the scan destinations in outcomeColumns order.
func (s *outcomeScan) dest() []any {
	return []any{
		&s.reason, &s.encoder, &s.mean, &s.worst, &s.model,
		&s.pixFmt, &s.chroma, &s.chromaMetric, &s.stream,
		&s.srcCodec, &s.srcBytes, &s.outBytes, &s.encMs, &s.targetPath,
		&s.guardAttrs, &s.guardRes, &s.guardWindow, &s.swapCause,
		&s.failClass, &s.inputs,
		&s.libraryRoot, &s.profileDigest, &s.profile,
		&s.dropped, &s.notApplied, &s.vmafSkipped,
		&s.srcWidth, &s.srcHeight, &s.outWidth, &s.outHeight,
		&s.deinterlaced, &s.deintFilter,
		&s.downscaled, &s.downscaleScaler,
		&s.vmafScoredWidth, &s.vmafScoredHeight,
	}
}

// outcome maps the scanned columns back to an Outcome, turning SQL NULL into the nil
// pointer / empty string that means "not recorded". The inverse of the null* helpers
// above; the round-trip is asserted by the store tests.
//
// The failure class is the one column that does not round-trip through "not recorded",
// and it is the read side of the vocabulary being CLOSED. It is resolved through Class(),
// so a NULL - every row written before the column existed - and any value that is not one
// of the two tokens both read back as the transient class. That is the retry direction,
// and it is what makes an unrecognised value cost CPU rather than a file nobody revisits.
func (s *outcomeScan) outcome() Outcome {
	o := Outcome{
		Reason: s.reason.String, Encoder: s.encoder.String, VmafModel: s.model.String,
		VmafPixFmt: s.pixFmt.String, VmafChromaMetric: s.chromaMetric.String,
		VmafStream:      s.stream.String,
		SourceCodec:     s.srcCodec.String,
		TargetPath:      s.targetPath.String,
		GuardAttributes: s.guardAttrs.String, GuardTimeResolution: s.guardRes.String,
		GuardResidualWindow: s.guardWindow.String, SwapCause: s.swapCause.String,
		FailureClass:   FailureClass(s.failClass.String).Class(),
		DecisionInputs: ParseDecisionInputs(s.inputs.String),
		Profile:        s.profile.String,
		// NULL parses to the zero DroppedStreams, which reads as NOT RECORDED - never as
		// an empty set, which would claim a job dropped nothing when nothing was ever
		// recorded about it.
		DroppedStreams:      ParseDroppedStreams(s.dropped.String),
		SelectionNotApplied: s.notApplied.String,
		VmafSkipped:         s.vmafSkipped.String,
		DeinterlaceFilter:   s.deintFilter.String,
		DownscaleScaler:     s.downscaleScaler.String,
		Decision: Decision{
			LibraryRoot:   s.libraryRoot.String,
			ProfileDigest: s.profileDigest.String,
		},
	}
	o.VmafMean = nullableFloat(s.mean)
	o.VmafMin = nullableFloat(s.worst)
	o.VmafChroma = nullableFloat(s.chroma)
	o.SourceBytes = nullableInt(s.srcBytes)
	o.OutputBytes = nullableInt(s.outBytes)
	o.EncodeMs = nullableInt(s.encMs)
	o.SourceWidth = nullablePixels(s.srcWidth)
	o.SourceHeight = nullablePixels(s.srcHeight)
	o.OutputWidth = nullablePixels(s.outWidth)
	o.OutputHeight = nullablePixels(s.outHeight)
	o.Deinterlaced = nullableBool(s.deinterlaced)
	o.Downscaled = nullableBool(s.downscaled)
	o.VmafScoredWidth = nullablePixels(s.vmafScoredWidth)
	o.VmafScoredHeight = nullablePixels(s.vmafScoredHeight)
	return o
}

// nullableBool is the read side of nullBool: SQL NULL becomes nil, which is NOT RECORDED,
// and a stored 0 becomes an explicit false, which is a job that ran and deinterlaced
// nothing. The two must not collapse - the rows that predate the column describe sources
// this tool has already deleted.
func nullableBool(n sql.NullBool) *bool {
	if !n.Valid {
		return nil
	}
	v := n.Bool
	return &v
}

func nullableFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

func nullableInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// nullablePixels is nullableInt for a pixel dimension, which the Outcome holds as an *int.
// A column that is NULL, or that holds a value no dimension can be, reads as NOT RECORDED
// rather than as a number: this is the read half of the rule nullPixels keeps on the way
// in, and it also covers a row some repair script edited.
func nullablePixels(n sql.NullInt64) *int {
	if !n.Valid || n.Int64 <= 0 {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// Delete removes the row for path+fingerprint (a no-op if absent).
func (s *SQLite) Delete(ctx context.Context, path, fingerprint string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE path = ? AND fingerprint = ?`, path, fingerprint); err != nil {
		return fmt.Errorf("store: delete: %w", err)
	}
	return nil
}

// List is documented on the Store interface. It builds a parameterized query — the
// status filter is expanded to a placeholder list so a Status value can never be
// interpolated into SQL text (the values are a closed internal vocabulary anyway,
// but parameterizing keeps the read injection-proof by construction).
func (s *SQLite) List(ctx context.Context, statuses []Status, limit int) ([]Job, error) {
	q := `SELECT ` + jobColumns + `
		FROM jobs`
	args := make([]any, 0, len(statuses)+1)
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		q += " WHERE status IN (" + strings.Join(ph, ", ") + ")"
	}
	// Newest transition first — the API/UI shows the most recent activity at the top.
	q += " ORDER BY updated_at DESC, path ASC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rows: %w", err)
	}
	return out, nil
}

// Summary is documented on the Store interface.
func (s *SQLite) Summary(ctx context.Context) (map[Status]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[Status]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: summary scan: %w", err)
		}
		out[Status(status)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: summary rows: %w", err)
	}
	return out, nil
}

// ReclaimedTotal is documented on the Store interface. The WHERE clause requires
// BOTH sizes to be non-NULL, so a pre-outcome-columns Done row (no sizes recorded)
// contributes nothing rather than reading a NULL as 0. COALESCE turns the no-rows
// case into 0. The result is clamped at 0 for the same reason Event.BytesReclaimed
// is: the strictly-smaller gate precludes output > source, but a defensive clamp
// means a future bug there can never make a lifetime total run backwards.
//
// The second term is the retention carry-forward (LEDGER-5): what rows a prune has
// already removed contributed, moved into ledger_totals in the same transaction that
// deleted them. Without it, bounding the ledger would quietly shrink the one figure an
// operator uses to judge whether the tool was worth running - and it would do so at the
// NEXT RESTART rather than at the prune, because the server reads this once as a baseline.
// The two terms cannot double-count: a row is in exactly one of them, and it moves from
// the first to the second atomically.
func (s *SQLite) ReclaimedTotal(ctx context.Context) (int64, error) {
	var live, pruned int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(source_bytes - output_bytes), 0) FROM jobs
		 WHERE status = ? AND source_bytes IS NOT NULL AND output_bytes IS NOT NULL`,
		string(Done)).Scan(&live); err != nil {
		return 0, fmt.Errorf("store: reclaimed total: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(reclaimed_pruned), 0) FROM ledger_totals`).Scan(&pruned); err != nil {
		return 0, fmt.Errorf("store: reclaimed total (pruned carry-forward): %w", err)
	}
	if live < 0 {
		live = 0
	}
	if pruned < 0 {
		pruned = 0
	}
	return live + pruned, nil
}

// HeldByUndoWindow is documented on the Store interface. It sums only LIVE retentions
// (restored_at IS NULL); a released one is not in the table at all, and a restored one
// gave its bytes back to the library rather than to the filesystem. COALESCE turns the
// no-rows case into 0, which here is the truth and not a fabrication: nothing retained
// is nothing held.
func (s *SQLite) HeldByUndoWindow(ctx context.Context) (int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(source_bytes), 0) FROM retained_originals WHERE restored_at IS NULL`).
		Scan(&total); err != nil {
		return 0, fmt.Errorf("store: held by undo window: %w", err)
	}
	return total, nil
}

// retainedColumns is the column list every reader of a retention projects, in ONE
// place so the SELECT text and the scan destinations cannot drift apart.
const retainedColumns = `source_path, swapped_path, retained_path, source_bytes,
	swapped_fingerprint, retained_at, expires_at, restored_at, schema_version`

// scanRetained reads one row in retainedColumns order. restored_at is the only
// nullable column: NULL means "not restored", which is a state and not a missing
// measurement, so it maps to a nil pointer exactly as the outcome columns do.
//
// The version stamp is read here rather than left to the callers for the same reason the
// column list is a constant: a retention read through one path and not the other would be
// a record whose stamp depends on which reader you asked.
func scanRetained(sc interface{ Scan(...any) error }) (Retained, error) {
	var r Retained
	var restoredAt sql.NullInt64
	var stamp stampScan
	if err := sc.Scan(&r.SourcePath, &r.SwappedPath, &r.RetainedPath, &r.SourceBytes,
		&r.SwappedFingerprint, &r.RetainedAt, &r.ExpiresAt, &restoredAt, stamp.dest()); err != nil {
		return Retained{}, err
	}
	r.RestoredAt = nullableInt(restoredAt)
	r.Stamp = stamp.stamp()
	return r, nil
}

// Retain is documented on the Store interface. The upsert replaces an earlier record
// for the same source path and CLEARS restored_at with it: the row now describes the
// swap that has just happened, not the one an operator undid before it.
func (s *SQLite) Retain(ctx context.Context, r Retained) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO retained_originals
			(source_path, swapped_path, retained_path, source_bytes, swapped_fingerprint, retained_at, expires_at, restored_at,
			 schema_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?)
		 ON CONFLICT(source_path) DO UPDATE SET
			swapped_path = excluded.swapped_path,
			retained_path = excluded.retained_path,
			source_bytes = excluded.source_bytes,
			swapped_fingerprint = excluded.swapped_fingerprint,
			retained_at = excluded.retained_at,
			expires_at = excluded.expires_at,
			restored_at = NULL,
			schema_version = excluded.schema_version`,
		r.SourcePath, r.SwappedPath, r.RetainedPath, r.SourceBytes,
		r.SwappedFingerprint, r.RetainedAt, r.ExpiresAt, currentStamp()); err != nil {
		return fmt.Errorf("store: retain: %w", err)
	}
	return nil
}

// GetRetained is documented on the Store interface. The source_path match is tried
// FIRST so an exact answer always wins over the swapped-path convenience lookup.
func (s *SQLite) GetRetained(ctx context.Context, path string) (Retained, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+retainedColumns+` FROM retained_originals
		 WHERE source_path = ? OR swapped_path = ?
		 ORDER BY (source_path = ?) DESC LIMIT 1`, path, path, path)
	r, err := scanRetained(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Retained{}, false, nil
	}
	if err != nil {
		return Retained{}, false, fmt.Errorf("store: get retained: %w", err)
	}
	return r, true, nil
}

// ListRetained is documented on the Store interface.
func (s *SQLite) ListRetained(ctx context.Context) ([]Retained, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+retainedColumns+` FROM retained_originals
		 WHERE restored_at IS NULL ORDER BY expires_at ASC, source_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list retained: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Retained
	for rows.Next() {
		r, err := scanRetained(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list retained scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list retained rows: %w", err)
	}
	return out, nil
}

// MarkRestored is documented on the Store interface.
func (s *SQLite) MarkRestored(ctx context.Context, sourcePath string, at int64) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE retained_originals SET restored_at = ?, schema_version = ? WHERE source_path = ?`,
		at, currentStamp(), sourcePath); err != nil {
		return fmt.Errorf("store: mark restored: %w", err)
	}
	return nil
}

// DropRetained is documented on the Store interface.
func (s *SQLite) DropRetained(ctx context.Context, sourcePath string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM retained_originals WHERE source_path = ?`, sourcePath); err != nil {
		return fmt.Errorf("store: drop retained: %w", err)
	}
	return nil
}

// RecordSkip is documented on the Store interface. The ON CONFLICT DO UPDATE is
// gated by `WHERE jobs.status = 'pending'`, which is what keeps a mutable guard from
// clobbering a real outcome: on a fresh key the INSERT runs (1 row); on a pending
// row it converts to skipped (1 row); on a row that is already skipped/done/failed
// the DO UPDATE's WHERE excludes it and nothing changes (0 rows). RowsAffected is
// therefore exactly "did this call newly record the skip", which the caller uses to
// emit — and count — the skip once, not once per scan. The outcome columns are
// cleared so a converted row carries no stale proof (the same discipline as Claim).
//
// The library profile and the encode profile are the ONLY outcome columns this write
// carries values for rather than clearing, and that is the same discipline rather than
// an exception to it. What the clearing rule excludes is proof about an ENCODE - a score,
// a size, a duration - which this row has none of. Neither profile is proof: they are
// what the guard was decided against, resolved at the moment it fired, so they belong to
// THIS attempt exactly as the reason does. Clearing the encode profile would record
// "nothing matched this file" on a row a profile decided, which is a false statement and
// not an absence - for that column NULL and "" say the same thing.
func (s *SQLite) RecordSkip(ctx context.Context, path, fingerprint, reason string, by Decision, profile string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason,
			library_root, profile_digest, schema_version, profile)
		 VALUES (?, ?, ?, 0, NULL, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path, fingerprint) DO UPDATE SET
			status = excluded.status, reason = excluded.reason, worker = NULL, updated_at = excluded.updated_at,
			library_root = excluded.library_root, profile_digest = excluded.profile_digest,
			schema_version = excluded.schema_version,
			encoder = NULL, vmaf_mean = NULL, vmaf_min = NULL, vmaf_model = NULL,
			vmaf_pix_fmt = NULL, vmaf_chroma = NULL, vmaf_chroma_metric = NULL, vmaf_stream = NULL,
			source_codec = NULL, source_bytes = NULL, output_bytes = NULL, encode_ms = NULL,
			target_path = NULL,
			guard_attributes = NULL, guard_time_resolution = NULL, guard_residual_window = NULL,
			swap_cause = NULL, decision_inputs = NULL, profile = excluded.profile,
			dropped_streams = NULL, selection_not_applied = NULL, vmaf_skipped = NULL,
			source_width = NULL, source_height = NULL, output_width = NULL, output_height = NULL,
			deinterlaced = NULL, deinterlace_filter = NULL,
			downscaled = NULL, downscale_scaler = NULL,
			vmaf_scored_width = NULL, vmaf_scored_height = NULL
		 WHERE jobs.status = ?`,
		path, fingerprint, string(Skipped), now(), nullString(reason),
		nullString(by.LibraryRoot), nullString(by.ProfileDigest), currentStamp(), nullString(profile),
		string(Pending))
	if err != nil {
		return false, fmt.Errorf("store: record skip: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: record skip rows affected: %w", err)
	}
	return n > 0, nil
}

// ClearSkip is documented on the Store interface. The status+reason match in the
// WHERE is the safety: it can only ever delete the specific skipped row the mutable
// guard itself wrote, never a done/failed/other-skip row.
func (s *SQLite) ClearSkip(ctx context.Context, path, fingerprint, reason string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE path = ? AND fingerprint = ? AND status = ? AND reason = ?`,
		path, fingerprint, string(Skipped), nullString(reason)); err != nil {
		return fmt.Errorf("store: clear skip: %w", err)
	}
	return nil
}

// --- the ledger-wide path search ----------------------------------------------

// likeEscape turns a caller's TERM into the body of a LIKE pattern that matches those
// characters literally.
//
// It is not a nicety: `_` matches any single character in LIKE and is in a large share of
// real media filenames, so an unescaped search for `the_wire` would also match `the-wire`
// and `the wire`, and the operator would be told a row exists for a file that does not.
// The backslash is escaped FIRST, or escaping the wildcards would then escape their own
// escape character.
func likeEscape(term string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term)
}

// SearchPath is documented on the Store interface.
//
// The rows and the total are two separate reads over the same WHERE, for the reason the
// aggregates are their own reads: a total that cannot be computed must not take the rows
// with it. The statuses are expanded to a placeholder list exactly as List does, so a
// Status can never be interpolated into SQL text.
func (s *SQLite) SearchPath(ctx context.Context, statuses []Status, term string, limit int) ([]Job, RowTotal, error) {
	where, args := pathSearchWhere(statuses, term)
	total := s.searchTotal(ctx, statuses, term, where, args)

	q := `SELECT ` + jobColumns + ` FROM jobs WHERE ` + where + ` ORDER BY updated_at DESC, path ASC`
	rowArgs := args
	if limit > 0 {
		q += ` LIMIT ?`
		rowArgs = append(append([]any{}, args...), limit)
	}
	rows, err := s.db.QueryContext(ctx, q, rowArgs...)
	if err != nil {
		return nil, total, fmt.Errorf("store: search path: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, total, fmt.Errorf("store: search path scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, total, fmt.Errorf("store: search path rows: %w", err)
	}
	return out, total, nil
}

// pathSearchWhere is the one clause both halves of the search read through, so the rows
// and the count can never describe different sets.
func pathSearchWhere(statuses []Status, term string) (string, []any) {
	args := make([]any, 0, len(statuses)+1)
	where := `path LIKE ? ESCAPE '\'`
	args = append(args, "%"+likeEscape(term)+"%")
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where += ` AND status IN (` + strings.Join(ph, ", ") + `)`
	}
	return where, args
}

// searchTotal counts every matching row in the ledger. It states the SET it counted over,
// because a figure whose set is unstated is one an operator reads as covering everything
// they own - and this one deliberately covers more than the response ships.
func (s *SQLite) searchTotal(ctx context.Context, statuses []Status, term, where string, args []any) RowTotal {
	cov := Coverage{Set: "every matching row in the ledger"}
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE `+where, args...).Scan(&n); err != nil {
		return RowTotal{Coverage: cov, Err: fmt.Errorf("store: search total: %w", err)}
	}
	return RowTotal{Coverage: cov, Count: n}
}

// --- withheld paths -------------------------------------------------------------

// exclusionColumns is the projection every reader of a withholding uses, in ONE place, so
// the SELECT text and the scan destinations cannot drift apart.
const exclusionColumns = `path, created_at, schema_version`

func scanExclusion(sc interface{ Scan(...any) error }) (PathExclusion, error) {
	var e PathExclusion
	var stamp stampScan
	if err := sc.Scan(&e.Path, &e.CreatedAt, stamp.dest()); err != nil {
		return PathExclusion{}, err
	}
	e.Stamp = stamp.stamp()
	return e, nil
}

// ExcludePath is documented on the Store interface. DO NOTHING on conflict rather than an
// upsert: re-recording a withholding must not move its created_at, which is the only thing
// that says how long a file has been held out.
func (s *SQLite) ExcludePath(ctx context.Context, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO path_exclusions (path, created_at, schema_version) VALUES (?, ?, ?)
		 ON CONFLICT(path) DO NOTHING`,
		path, now(), currentStamp())
	if err != nil {
		return false, fmt.Errorf("store: exclude path: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: exclude path rows affected: %w", err)
	}
	return n > 0, nil
}

// UnexcludePath is documented on the Store interface.
func (s *SQLite) UnexcludePath(ctx context.Context, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM path_exclusions WHERE path = ?`, path)
	if err != nil {
		return false, fmt.Errorf("store: unexclude path: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unexclude path rows affected: %w", err)
	}
	return n > 0, nil
}

// ExcludedPaths is documented on the Store interface.
func (s *SQLite) ExcludedPaths(ctx context.Context) ([]PathExclusion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+exclusionColumns+` FROM path_exclusions ORDER BY created_at ASC, path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: excluded paths: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PathExclusion
	for rows.Next() {
		e, err := scanExclusion(rows)
		if err != nil {
			return nil, fmt.Errorf("store: excluded paths scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: excluded paths rows: %w", err)
	}
	return out, nil
}

// PathIsExcluded is documented on the Store interface. A point lookup on the primary key,
// because the engine asks it once per file per scan.
func (s *SQLite) PathIsExcluded(ctx context.Context, path string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM path_exclusions WHERE path = ? LIMIT 1`, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: path is excluded: %w", err)
	}
	return true, nil
}

// Get returns the current status and fail_count for path+fingerprint.
func (s *SQLite) Get(ctx context.Context, path, fingerprint string) (Status, int, bool, error) {
	var status string
	var failCount int
	err := s.db.QueryRowContext(ctx,
		`SELECT status, fail_count FROM jobs WHERE path = ? AND fingerprint = ?`,
		path, fingerprint).Scan(&status, &failCount)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("store: get: %w", err)
	}
	return Status(status), failCount, true, nil
}

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
}

var _ Store = (*SQLite)(nil)

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
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &SQLite{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// New wraps an already-open *sql.DB (test seam — e.g. an in-memory database) and
// initializes the schema. The caller is responsible for any connection-limit
// pragmas it wants (Open sets MaxOpenConns(1); New leaves db as given).
func New(db *sql.DB) (*SQLite, error) {
	s := &SQLite{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLite) init() error {
	const schema = `
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
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("store: init schema: %w", err)
	}
	return nil
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
		`UPDATE jobs SET status = ?, worker = NULL, updated_at = ? WHERE status IN (?, ?, ?)`,
		string(Pending), now(), string(Probing), string(Encoding), string(Verifying))
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
func (s *SQLite) Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: claim begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if already committed

	var status string
	var failCount int
	err = tx.QueryRowContext(ctx,
		`SELECT status, fail_count FROM jobs WHERE path = ? AND fingerprint = ?`,
		path, fingerprint).Scan(&status, &failCount)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Never seen before: claim it fresh.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at) VALUES (?, ?, ?, 0, ?, ?)`,
			path, fingerprint, string(Probing), worker, now()); err != nil {
			return false, fmt.Errorf("store: claim insert: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("store: claim commit: %w", err)
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("store: claim select: %w", err)
	}

	st := Status(status)
	switch {
	case st == Done || st == Skipped:
		return false, nil // permanent terminal state
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

	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, worker = ?, updated_at = ? WHERE path = ? AND fingerprint = ?`,
		string(Probing), worker, now(), path, fingerprint); err != nil {
		return false, fmt.Errorf("store: claim update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: claim commit: %w", err)
	}
	return true, nil
}

// Advance records a non-terminal state transition for a job the caller already
// holds.
func (s *SQLite) Advance(ctx context.Context, path, fingerprint string, st Status) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, updated_at = ? WHERE path = ? AND fingerprint = ?`,
		string(st), now(), path, fingerprint); err != nil {
		return fmt.Errorf("store: advance: %w", err)
	}
	return nil
}

// Finish records a terminal outcome. Failed increments fail_count.
func (s *SQLite) Finish(ctx context.Context, path, fingerprint string, st Status) error {
	var err error
	if st == Failed {
		_, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, fail_count = fail_count + 1, updated_at = ? WHERE path = ? AND fingerprint = ?`,
			string(st), now(), path, fingerprint)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status = ?, updated_at = ? WHERE path = ? AND fingerprint = ?`,
			string(st), now(), path, fingerprint)
	}
	if err != nil {
		return fmt.Errorf("store: finish: %w", err)
	}
	return nil
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
	q := `SELECT path, fingerprint, status, fail_count, worker, updated_at FROM jobs`
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
		var j Job
		var status string
		var worker sql.NullString // worker is NULL for a pending/recovered row
		if err := rows.Scan(&j.Path, &j.Fingerprint, &status, &j.FailCount, &worker, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		j.Status = Status(status)
		j.Worker = worker.String
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

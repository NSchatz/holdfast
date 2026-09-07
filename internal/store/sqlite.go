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
	if err := migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
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
	// No MkdirAll and no create: a read that finds nothing to read says so. The
	// pragmas are deliberately not Open's — journal_mode and synchronous are writes to
	// the header, and a reader has no business setting either. query_only is belt and
	// braces beside mode=ro, and it costs nothing.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := requireCurrentSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// New wraps an already-open *sql.DB (test seam — e.g. an in-memory database) and
// migrates the schema up to date. The caller is responsible for any connection-limit
// pragmas it wants (Open sets MaxOpenConns(1); New leaves db as given).
func New(db *sql.DB) (*SQLite, error) {
	s := &SQLite{db: db}
	if err := migrate(context.Background(), db); err != nil {
		return nil, err
	}
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
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, worker = ?, updated_at = ?,
			reason = NULL, encoder = NULL, vmaf_mean = NULL, vmaf_min = NULL, vmaf_model = NULL,
			vmaf_pix_fmt = NULL, vmaf_chroma = NULL, vmaf_chroma_metric = NULL,
			source_bytes = NULL, output_bytes = NULL, encode_ms = NULL
		 WHERE path = ? AND fingerprint = ?`,
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

// Finish is documented on the Store interface. Failed increments fail_count.
//
// The outcome columns are written UNCONDITIONALLY from o (nil o => all NULL), never
// merged into whatever was there before. See the interface doc: a retried job that
// finally succeeds must not carry the previous attempt's failure reason next to its
// "done", and the only way to guarantee that without a special case per column is to
// let every Finish fully define the row's proof.
func (s *SQLite) Finish(ctx context.Context, path, fingerprint string, st Status, o *Outcome) error {
	if o == nil {
		o = &Outcome{}
	}
	// A "" string is stored as NULL, not as an empty string, so "not recorded" has ONE
	// representation in the column rather than two the readers would both have to know
	// about.
	q := `UPDATE jobs SET status = ?, updated_at = ?,
		reason = ?, encoder = ?, vmaf_mean = ?, vmaf_min = ?, vmaf_model = ?,
		vmaf_pix_fmt = ?, vmaf_chroma = ?, vmaf_chroma_metric = ?,
		source_bytes = ?, output_bytes = ?, encode_ms = ?`
	if st == Failed {
		q += `, fail_count = fail_count + 1`
	}
	q += ` WHERE path = ? AND fingerprint = ?`

	if _, err := s.db.ExecContext(ctx, q,
		string(st), now(),
		nullString(o.Reason), nullString(o.Encoder),
		nullFloat(o.VmafMean), nullFloat(o.VmafMin), nullString(o.VmafModel),
		nullString(o.VmafPixFmt), nullFloat(o.VmafChroma), nullString(o.VmafChromaMetric),
		nullInt(o.SourceBytes), nullInt(o.OutputBytes), nullInt(o.EncodeMs),
		path, fingerprint,
	); err != nil {
		return fmt.Errorf("store: finish: %w", err)
	}
	return nil
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

// outcomeColumns is the column list every reader of the outcome projects, in ONE
// place, so the SELECT text and the scan destinations cannot drift apart when a
// column is appended. Its order is the order outcomeScan expects.
const outcomeColumns = `reason, encoder, vmaf_mean, vmaf_min, vmaf_model,
	vmaf_pix_fmt, vmaf_chroma, vmaf_chroma_metric, source_bytes, output_bytes, encode_ms`

// outcomeScan holds one row's outcome columns on the way out of the driver. Every
// field is a sql.Null* because every column is nullable: NULL is "not recorded" and
// must not be scanned into a bare 0/"" a reader would mistake for a measurement.
//
// It exists as a struct rather than a list of locals because the column set grows
// (v2 added eight, v4 added three) and a positional scan is the shape that silently
// mis-binds when it does - swap two same-typed columns in the argument list and the
// compiler is happy while a VMAF score arrives in the chroma field.
type outcomeScan struct {
	reason, encoder, model    sql.NullString
	pixFmt, chromaMetric      sql.NullString
	mean, worst, chroma       sql.NullFloat64
	srcBytes, outBytes, encMs sql.NullInt64
}

// dest returns the scan destinations in outcomeColumns order.
func (s *outcomeScan) dest() []any {
	return []any{
		&s.reason, &s.encoder, &s.mean, &s.worst, &s.model,
		&s.pixFmt, &s.chroma, &s.chromaMetric, &s.srcBytes, &s.outBytes, &s.encMs,
	}
}

// outcome maps the scanned columns back to an Outcome, turning SQL NULL into the nil
// pointer / empty string that means "not recorded". The inverse of the null* helpers
// above; the round-trip is asserted by the store tests.
func (s *outcomeScan) outcome() Outcome {
	o := Outcome{
		Reason: s.reason.String, Encoder: s.encoder.String, VmafModel: s.model.String,
		VmafPixFmt: s.pixFmt.String, VmafChromaMetric: s.chromaMetric.String,
	}
	o.VmafMean = nullableFloat(s.mean)
	o.VmafMin = nullableFloat(s.worst)
	o.VmafChroma = nullableFloat(s.chroma)
	o.SourceBytes = nullableInt(s.srcBytes)
	o.OutputBytes = nullableInt(s.outBytes)
	o.EncodeMs = nullableInt(s.encMs)
	return o
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
	q := `SELECT path, fingerprint, status, fail_count, worker, updated_at,
		` + outcomeColumns + `
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
		var j Job
		var status string
		var worker sql.NullString // worker is NULL for a pending/recovered row
		// Every outcome column is nullable: NULL is "not recorded" and must not be
		// scanned into a bare 0/"" that a reader would mistake for a measurement.
		var oc outcomeScan
		dest := append([]any{&j.Path, &j.Fingerprint, &status, &j.FailCount, &worker, &j.UpdatedAt}, oc.dest()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		j.Status = Status(status)
		j.Worker = worker.String
		j.Outcome = oc.outcome()
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

// RecordSkip is documented on the Store interface. The ON CONFLICT DO UPDATE is
// gated by `WHERE jobs.status = 'pending'`, which is what keeps a mutable guard from
// clobbering a real outcome: on a fresh key the INSERT runs (1 row); on a pending
// row it converts to skipped (1 row); on a row that is already skipped/done/failed
// the DO UPDATE's WHERE excludes it and nothing changes (0 rows). RowsAffected is
// therefore exactly "did this call newly record the skip", which the caller uses to
// emit — and count — the skip once, not once per scan. The outcome columns are
// cleared so a converted row carries no stale proof (the same discipline as Claim).
func (s *SQLite) RecordSkip(ctx context.Context, path, fingerprint, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, reason)
		 VALUES (?, ?, ?, 0, NULL, ?, ?)
		 ON CONFLICT(path, fingerprint) DO UPDATE SET
			status = excluded.status, reason = excluded.reason, worker = NULL, updated_at = excluded.updated_at,
			encoder = NULL, vmaf_mean = NULL, vmaf_min = NULL, vmaf_model = NULL,
			vmaf_pix_fmt = NULL, vmaf_chroma = NULL, vmaf_chroma_metric = NULL,
			source_bytes = NULL, output_bytes = NULL, encode_ms = NULL
		 WHERE jobs.status = ?`,
		path, fingerprint, string(Skipped), now(), nullString(reason), string(Pending))
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

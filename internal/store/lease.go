package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The worker-node lease ledger (docs/design/nodes.md#leases).
//
// A lease is one GRANT of one job's encode to one node. The row is the durable half of the
// fencing contract: a restarted server reads it to tell a live node's work from a stale
// one's, and a later grant of the same path counts its epoch up from the rows kept here.
//
// The ledger decides nothing about a lease. It reads the row, or the rows, INSIDE one
// transaction, hands them to the caller's decision, and writes what the decision returns in
// that same transaction - so the check of lease id, epoch and liveness and the act it
// licenses can never be separated by another writer. The decisions themselves are
// internal/node's, where they are pure functions tested against a clock.

// LeaseState is where a lease stands. The vocabulary is closed.
type LeaseState string

const (
	// LeaseGranted is a lease a node holds and has not yet uploaded an output for.
	LeaseGranted LeaseState = "granted"
	// LeaseUploaded is a lease whose output was admitted - its length and digest matched -
	// and whose node has not yet reported completion.
	LeaseUploaded LeaseState = "uploaded"
	// LeaseCompleted is a lease whose node reported completion of an admitted output. It
	// is terminal: what follows is the server's own gates, which are not the lease's.
	LeaseCompleted LeaseState = "completed"
	// LeaseFailed is a lease that ended on a stated failure. Terminal.
	LeaseFailed LeaseState = "failed"
	// LeaseExpired is a lease whose time ran out, or that the server ended. Terminal.
	LeaseExpired LeaseState = "expired"
)

// Live reports whether the state is one a node may still act on: granted or uploaded.
// It says nothing about the clock; a live state past its expiry is still due to expire.
func (s LeaseState) Live() bool { return s == LeaseGranted || s == LeaseUploaded }

// Valid reports whether s is a member of the closed vocabulary.
func (s LeaseState) Valid() bool {
	return s.Live() || s == LeaseCompleted || s == LeaseFailed || s == LeaseExpired
}

// Lease is one lease row.
type Lease struct {
	// ID is random and unguessable, and names exactly one grant.
	ID string
	// Path is the source as the server names it, and Key its store key (the fingerprint).
	Path, Key string
	// Node is the name the node gave when it asked for work.
	Node string
	// Epoch is the fencing token: one more than the highest epoch any earlier grant of the
	// same Path carried. The ledger assigns it; no caller chooses it.
	Epoch int64
	State LeaseState
	// ExpiresAt is the server's clock, to the second.
	ExpiresAt time.Time
	// Temp is the working file the server named for this grant.
	Temp string
	// ReservedBytes is what the server's free-space hold reserved for the job.
	ReservedBytes int64
	// ArgsDigest is a digest of the leased argument list, so a restarted server can tell
	// whether the job it re-derived is still the one that was leased.
	ArgsDigest string
	// SourceSize and SourceModTime are the source as it stood at the grant.
	SourceSize    int64
	SourceModTime time.Time
	// OutputDigest and OutputBytes are the admitted upload's, empty and zero until one is
	// admitted. SourceDigest is what the node reported it read, empty until completion.
	OutputDigest string
	OutputBytes  int64
	SourceDigest string
	// UploadAttempts counts the uploads refused for a digest mismatch.
	UploadAttempts int
	// Reason says why a failed or expired lease ended. Empty on every other state.
	Reason string
	// GrantedAt and UpdatedAt are the server's clock; EndedAt is zero until the lease is
	// terminal.
	GrantedAt, UpdatedAt, EndedAt time.Time
}

// ErrNoLease is returned by UpdateLease when no row carries the id.
var ErrNoLease = errors.New("store: no lease has that id")

// LeaseLedger is the lease half of the store. internal/node is its one caller.
//
// A decide callback runs INSIDE the write transaction, on the single connection every
// write shares: it must be quick and must not call the store.
type LeaseLedger interface {
	// GrantLease inserts a lease row for path. In one transaction it reads every lease in
	// a live state (any path) and the highest epoch any lease of this path ever carried (0
	// when there is none), hands both to decide, and inserts the row decide returns with
	// Path set to path and Epoch set to that highest epoch plus one. An error from decide
	// is returned as it is and nothing is written.
	GrantLease(ctx context.Context, path string, decide func(live []Lease, lastEpoch int64) (Lease, error)) (Lease, error)
	// UpdateLease reads the row with that id and hands it to decide in one transaction,
	// then writes the row decide returns. ID, Path, Key, Node, Epoch and GrantedAt are the
	// grant's and are never rewritten. An error from decide is returned as it is, with the
	// row as it was read, and nothing is written. ErrNoLease when no row carries the id.
	UpdateLease(ctx context.Context, id string, decide func(cur Lease) (Lease, error)) (Lease, error)
	// GetLease reads one row.
	GetLease(ctx context.Context, id string) (Lease, bool, error)
	// LiveLeases lists every lease in a live state, oldest grant first.
	LiveLeases(ctx context.Context) ([]Lease, error)
	// PruneLeases deletes terminal rows that ended before olderThan, except the newest row
	// of each path, and reports how many it deleted. The newest row of a path is what the
	// next grant counts its epoch up from, so it is never pruned, and a live row never is.
	PruneLeases(ctx context.Context, olderThan time.Time) (int64, error)
	// RecentLeases lists at most limit leases, live and ended alike, newest grant first
	// (then path ascending, then epoch descending). A pure read, for reporting: it is on no
	// grant's path and writes nothing (lease_list.go).
	RecentLeases(ctx context.Context, limit int) ([]Lease, error)
	// CountLeases counts every lease row: the total a RecentLeases listing was capped
	// against. A count that could not be read carries its own error, never a zero.
	CountLeases(ctx context.Context) RowTotal
}

var _ LeaseLedger = (*SQLite)(nil)

const leaseColumns = `id, path, job_key, node, epoch, state, expires_at, temp_path, reserved_bytes,
	args_digest, source_size, source_mtime_ns, output_digest, output_bytes, source_digest,
	upload_attempts, reason, granted_at, updated_at, ended_at`

func scanLease(sc interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	var state string
	var expires, mtime, granted, updated int64
	var outDigest, srcDigest sql.NullString
	var outBytes, ended sql.NullInt64
	if err := sc.Scan(&l.ID, &l.Path, &l.Key, &l.Node, &l.Epoch, &state, &expires, &l.Temp,
		&l.ReservedBytes, &l.ArgsDigest, &l.SourceSize, &mtime, &outDigest, &outBytes, &srcDigest,
		&l.UploadAttempts, &l.Reason, &granted, &updated, &ended); err != nil {
		return Lease{}, err
	}
	l.State = LeaseState(state)
	l.ExpiresAt = time.Unix(expires, 0)
	l.SourceModTime = time.Unix(0, mtime)
	l.OutputDigest, l.OutputBytes, l.SourceDigest = outDigest.String, outBytes.Int64, srcDigest.String
	l.GrantedAt, l.UpdatedAt = time.Unix(granted, 0), time.Unix(updated, 0)
	if ended.Valid {
		l.EndedAt = time.Unix(ended.Int64, 0)
	}
	return l, nil
}

// nullUnix is a time as unix seconds, and NULL for the zero time.
func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// nullPositive is n, and NULL when n is zero: an output of zero bytes is never admitted,
// so a zero here only ever means "not recorded".
func nullPositive(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func queryLeases(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]Lease, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+leaseColumns+` FROM node_leases `+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

const liveLeasesWhere = `WHERE state IN ('granted', 'uploaded') ORDER BY granted_at, id`

// GrantLease is documented on LeaseLedger.
func (s *SQLite) GrantLease(ctx context.Context, path string, decide func(live []Lease, lastEpoch int64) (Lease, error)) (Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("store: grant lease begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if already committed

	live, err := queryLeases(ctx, tx, liveLeasesWhere)
	if err != nil {
		return Lease{}, fmt.Errorf("store: grant lease read live: %w", err)
	}
	var last int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(epoch), 0) FROM node_leases WHERE path = ?`, path).Scan(&last); err != nil {
		return Lease{}, fmt.Errorf("store: grant lease read epoch: %w", err)
	}
	l, err := decide(live, last)
	if err != nil {
		return Lease{}, err
	}
	l.Path, l.Epoch = path, last+1
	if l.ID == "" || !l.State.Valid() {
		return Lease{}, fmt.Errorf("store: grant lease: a lease needs an id and a known state, got id %q state %q", l.ID, l.State)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_leases (`+leaseColumns+`, schema_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Path, l.Key, l.Node, l.Epoch, string(l.State), l.ExpiresAt.Unix(), l.Temp, l.ReservedBytes,
		l.ArgsDigest, l.SourceSize, l.SourceModTime.UnixNano(), nullString(l.OutputDigest),
		nullPositive(l.OutputBytes), nullString(l.SourceDigest), l.UploadAttempts, l.Reason,
		l.GrantedAt.Unix(), l.UpdatedAt.Unix(), nullUnix(l.EndedAt), currentStamp()); err != nil {
		return Lease{}, fmt.Errorf("store: grant lease insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("store: grant lease commit: %w", err)
	}
	return roundLease(l), nil
}

// roundLease is l as a later read of its row returns it: every time to the second.
func roundLease(l Lease) Lease {
	l.ExpiresAt = time.Unix(l.ExpiresAt.Unix(), 0)
	l.GrantedAt, l.UpdatedAt = time.Unix(l.GrantedAt.Unix(), 0), time.Unix(l.UpdatedAt.Unix(), 0)
	if !l.EndedAt.IsZero() {
		l.EndedAt = time.Unix(l.EndedAt.Unix(), 0)
	}
	return l
}

// UpdateLease is documented on LeaseLedger.
func (s *SQLite) UpdateLease(ctx context.Context, id string, decide func(cur Lease) (Lease, error)) (Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("store: update lease begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if already committed

	cur, err := scanLease(tx.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM node_leases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoLease
	}
	if err != nil {
		return Lease{}, fmt.Errorf("store: update lease read: %w", err)
	}
	next, err := decide(cur)
	if err != nil {
		return cur, err
	}
	if !next.State.Valid() {
		return cur, fmt.Errorf("store: update lease: %q is not a lease state", next.State)
	}
	// The grant's own columns are not in the SET list: whatever decide returned for them,
	// the row keeps what it was granted with.
	next.ID, next.Path, next.Key, next.Node, next.Epoch, next.GrantedAt = cur.ID, cur.Path, cur.Key, cur.Node, cur.Epoch, cur.GrantedAt
	if _, err := tx.ExecContext(ctx, `UPDATE node_leases SET state = ?, expires_at = ?, temp_path = ?,
		reserved_bytes = ?, args_digest = ?, source_size = ?, source_mtime_ns = ?, output_digest = ?,
		output_bytes = ?, source_digest = ?, upload_attempts = ?, reason = ?, updated_at = ?, ended_at = ?,
		schema_version = ? WHERE id = ?`,
		string(next.State), next.ExpiresAt.Unix(), next.Temp, next.ReservedBytes, next.ArgsDigest,
		next.SourceSize, next.SourceModTime.UnixNano(), nullString(next.OutputDigest),
		nullPositive(next.OutputBytes), nullString(next.SourceDigest), next.UploadAttempts, next.Reason,
		next.UpdatedAt.Unix(), nullUnix(next.EndedAt), currentStamp(), id); err != nil {
		return cur, fmt.Errorf("store: update lease write: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return cur, fmt.Errorf("store: update lease commit: %w", err)
	}
	return roundLease(next), nil
}

// GetLease is documented on LeaseLedger.
func (s *SQLite) GetLease(ctx context.Context, id string) (Lease, bool, error) {
	l, err := scanLease(s.db.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM node_leases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("store: read lease: %w", err)
	}
	return l, true, nil
}

// LiveLeases is documented on LeaseLedger.
func (s *SQLite) LiveLeases(ctx context.Context) ([]Lease, error) {
	out, err := queryLeases(ctx, s.db, liveLeasesWhere)
	if err != nil {
		return nil, fmt.Errorf("store: read live leases: %w", err)
	}
	return out, nil
}

// PruneLeases is documented on LeaseLedger. The rule is bounded by construction: what is
// kept is the live rows, one terminal row per path, and the terminal rows younger than the
// cut, so the table grows with the number of paths ever leased and not with the number of
// grants.
func (s *SQLite) PruneLeases(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM node_leases
		WHERE state NOT IN ('granted', 'uploaded')
		  AND ended_at IS NOT NULL AND ended_at < ?
		  AND epoch < (SELECT MAX(n.epoch) FROM node_leases n WHERE n.path = node_leases.path)`,
		olderThan.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune leases: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune leases rows affected: %w", err)
	}
	return n, nil
}

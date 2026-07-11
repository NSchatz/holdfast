// Package store is the persistent, crash-safe job ledger for the transcoder. It
// replaces the flat-file internal/ledger with a SQLite/WAL table so a worker pool
// can safely claim files across goroutines (and, later, processes) with no risk of
// two workers encoding the same source concurrently.
//
// The data-safety invariant is unchanged: the store only ever records job STATE —
// it never touches the filesystem. The only filesystem mutation anywhere in the
// program remains the atomic same-directory rename in internal/engine, which runs
// solely after verifyOutput passes. A crash mid-encode leaves a job "stuck" in an
// active state (probing/encoding/verifying); RecoverStale resets it to pending on
// the next startup so it is safely retried — the source itself was never touched.
package store

import "context"

// Status is a job's lifecycle state.
type Status string

// The full set of statuses. Pending is the implicit initial state (a path+
// fingerprint with no row is treated as pending). Probing/Encoding/Verifying are
// the "active" sub-states a worker moves through while it holds the claim.
// Done/Skipped/Failed are terminal for that path+fingerprint (Failed is retryable
// up to a configured bound; see Claim).
const (
	Pending   Status = "pending"
	Probing   Status = "probing"
	Encoding  Status = "encoding"
	Verifying Status = "verifying"
	Done      Status = "done"
	Skipped   Status = "skipped"
	Failed    Status = "failed"
)

// Terminal reports whether s is a terminal status (done/skipped/failed) — no
// further processing will happen for that path+fingerprint (failed may still be
// retried by Claim, but the row itself is a terminal record of an attempt).
func (s Status) Terminal() bool {
	switch s {
	case Done, Skipped, Failed:
		return true
	default:
		return false
	}
}

// Active reports whether s is an in-progress sub-state a worker is actively
// holding (probing/encoding/verifying). An active row left behind by a crashed
// worker is what RecoverStale resets to pending.
func (s Status) Active() bool {
	switch s {
	case Probing, Encoding, Verifying:
		return true
	default:
		return false
	}
}

// Job is a read-only snapshot of one row in the job ledger, returned by List. It
// is a reporting view (the API/UI in TRANSCODE-7 renders it) — never a handle the
// engine writes back through, so exposing it cannot affect file handling.
type Job struct {
	Path        string
	Fingerprint string
	Status      Status
	FailCount   int
	Worker      string // "" when the row carries no worker (e.g. a terminal row)
	UpdatedAt   int64  // unix seconds of the last state transition
}

// Store is the persistent job ledger. Every method is safe for concurrent use by
// multiple workers (goroutines) within one process.
type Store interface {
	// RecoverStale resets any job left in an active state (probing/encoding/
	// verifying) back to pending — the mark of a prior crashed/killed run, since a
	// live worker holds its claim only for the duration of one in-process call.
	// Returns the number of jobs reset. Call once at startup, before any scan.
	RecoverStale(ctx context.Context) (int, error)

	// Claim atomically attempts to take ownership of path+fingerprint for worker.
	// Returns (true, nil) if the caller now owns the job (row moved to probing) and
	// (false, nil) if it does not: the job is done/skipped (permanent), failed and
	// already at/over maxFailures (parked), or currently active (held by another
	// worker, or stale — see RecoverStale). A fresh path+fingerprint with no row
	// yields a claim.
	Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int) (bool, error)

	// Advance records a non-terminal state transition for a job the caller already
	// holds (e.g. probing -> encoding -> verifying).
	Advance(ctx context.Context, path, fingerprint string, s Status) error

	// Finish records a terminal outcome for path+fingerprint. Failed increments
	// fail_count (retry accounting); Done/Skipped do not.
	Finish(ctx context.Context, path, fingerprint string, s Status) error

	// Delete removes the row for path+fingerprint (a no-op if absent). Used to prune
	// a job row that has been superseded — after a successful transcode the pre-swap
	// (path, old-fingerprint) row is deleted, so the table doesn't accumulate one
	// dangling row per transcoded file.
	Delete(ctx context.Context, path, fingerprint string) error

	// Get returns the current status and fail_count for path+fingerprint, and
	// whether a row exists at all (exists=false + status="" means never seen).
	Get(ctx context.Context, path, fingerprint string) (status Status, failCount int, exists bool, err error)

	// List returns job rows for reporting (TRANSCODE-7's API/UI), newest-updated
	// first. If statuses is non-empty only rows in that set are returned; an empty
	// statuses returns every row. limit > 0 caps the result to that many rows
	// (0 or negative = no cap). It is a pure read: it never mutates a row, so no
	// amount of API traffic can alter file handling.
	List(ctx context.Context, statuses []Status, limit int) ([]Job, error)

	// Summary returns a count of rows per status (only statuses with at least one
	// row appear). Used by the API/UI for at-a-glance queue/history totals.
	Summary(ctx context.Context) (map[Status]int, error)

	// Close releases the underlying database handle.
	Close() error
}

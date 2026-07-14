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

// Outcome is the durable PROOF of a terminal job's result — the facts the engine
// computed while deciding whether a swap was safe (TRANSCODE-13). Before this phase
// every one of them was computed and then thrown away, which is precisely why the
// ledger could not show fidelity, why a "reclaimed" total reset to zero on every
// restart, and why the API documented a failure `reason` field that did not exist.
//
// Absence is REPRESENTABLE, and must stay that way. Every numeric field is a POINTER
// for one reason: 0 is a legal value for all of them, so a plain zero cannot mean
// "nobody measured this". A VMAF of 0.0 is a destroyed frame, not a missing
// measurement. nil means NOT RECORDED, and a reader (the API, the UI) is required to
// render it as such — never as 0, never as a fabricated score. The string fields use
// "" for the same purpose, unambiguously: an empty reason/encoder/model carries no
// meaning of its own.
type Outcome struct {
	// Reason is WHY the job reached this status. For Failed it is the error text (the
	// encode error, or the gate that rejected the output). For Skipped it is the name
	// of the GUARD that fired — a stable token from internal/engine, not prose, so a
	// UI can key off it. Done needs no excuse and leaves it "".
	Reason string

	// Encoder is the encoder key (cpu / svtav1 / nvenc / …) the job actually ran, set
	// on every row that reached the encoder at all — a failure is as worth attributing
	// to its encoder as a success is.
	Encoder string

	// VmafMean and VmafMin are the pooled harmonic-mean and the worst-frame VMAF, and
	// VmafModel names the libvmaf model that produced them. All are nil/"" when the
	// VMAF gate did not run (disabled). The model is NOT decoration: a VMAF score
	// without the model and pooling that produced it is not a number anyone can
	// interpret, and displaying one without the other is the exact overclaim the
	// fidelity work exists to prevent.
	VmafMean  *float64
	VmafMin   *float64
	VmafModel string

	// SourceBytes and OutputBytes are the file sizes either side of the swap (Done).
	// BOTH are persisted rather than only their difference, which is what makes a
	// lifetime reclaimed total survive a restart (see Reclaimed) and lets a UI show
	// "before → after" instead of a bare delta.
	SourceBytes *int64
	OutputBytes *int64

	// EncodeMs is the wall-clock encode duration in milliseconds (Done).
	EncodeMs *int64
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

	// Outcome is the recorded proof for a terminal row (TRANSCODE-13). Its fields are
	// all zero/nil on a non-terminal row, and on a terminal row written before this
	// phase existed — "not recorded", which a reader must show as such.
	Outcome Outcome
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
	//
	// o is the proof of that outcome (TRANSCODE-13); nil records none. Finish always
	// writes the FULL outcome column set, so a nil o — or a nil field within it —
	// CLEARS the corresponding column. That is deliberate: a row's proof must always
	// describe its CURRENT status. A file that failed (reason recorded), was retried,
	// and then succeeded must not sit in the ledger as "done" with the old failure's
	// reason still attached to it.
	Finish(ctx context.Context, path, fingerprint string, s Status, o *Outcome) error

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

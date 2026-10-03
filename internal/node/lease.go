package node

import (
	"errors"
	"fmt"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// The lease state machine (docs/design/nodes.md#leases).
//
// Every function here is a DECISION: it takes the row as the ledger read it, the epoch the
// caller presented and the server's clock, and returns the row to write or a refusal. None
// of them reads a clock, a file or the store, so each is tested against a clock the test
// holds. The ledger runs a decision inside the transaction that read the row and writes
// what it returns in that same transaction, which is what keeps the check of lease id,
// epoch and liveness and the act it licenses from ever being separated.

// Lease is one lease row.
type Lease = store.Lease

// Reason says why a lease ended without a completed output. The vocabulary is closed.
type Reason string

const (
	// ReasonExpired is a lease whose heartbeats stopped for one TTL.
	ReasonExpired Reason = "expired"
	// ReasonNodeFailed is a lease its node ended with a stated failure.
	ReasonNodeFailed Reason = "node_failed"
	// ReasonDigestMismatch is a lease whose uploads failed the digest check as many times
	// as the bound allows.
	ReasonDigestMismatch Reason = "digest_mismatch"
	// ReasonSourceMismatch is a lease whose node reported a source digest that is not the
	// digest of the bytes the server streamed to it on that lease.
	ReasonSourceMismatch Reason = "source_digest_mismatch"
	// ReasonCanceled is a lease the server ended because the job waiting on it ended.
	ReasonCanceled Reason = "canceled"
	// ReasonRestart is a lease whose output was uploaded and not completed when the
	// server restarted: it is re-run, never gated after the restart.
	ReasonRestart Reason = "server_restart"
	// ReasonNotAdopted is a recovered lease the restarted server did not take back: the
	// job it re-derived is not the leased one, or it abandoned the lease.
	ReasonNotAdopted Reason = "not_adopted"
	// ReasonPollGone is a lease granted to a poll that had left before it was answered.
	ReasonPollGone Reason = "poll_gone"
)

// ReasonSourceWithdrawn is the typed reason a WORKER fails an http-mode lease with when the
// server itself stopped offering the lease's source (409 source_not_offered after a restart,
// or 409 source_changed). It says nothing about the node, so Hub.Report does not count it
// toward the node's cool-off, and nothing about the file, so the engine does not charge it.
const ReasonSourceWithdrawn = "source_withdrawn"

// The refusals a decision returns.
var (
	// ErrGone is a call on a lease that is not live at the epoch presented: it ended, its
	// time ran out, or a later grant superseded it. Answered 410.
	ErrGone = errors.New("node: the lease is not live at that epoch")
	// ErrDigestConflict is a digest or a length that differs from the one an accepted
	// upload recorded. Answered 409.
	ErrDigestConflict = errors.New("node: a different output was already accepted on this lease")
	// ErrNotUploaded is a completion reported for a lease no output was admitted on.
	ErrNotUploaded = errors.New("node: no output has been admitted on this lease")
	// ErrLeaseHeld is a grant of a path that already has a live lease.
	ErrLeaseHeld = errors.New("node: the path already has a live lease")
	// ErrTempHeld is a grant whose working file is a live lease's working file or source.
	ErrTempHeld = errors.New("node: the working file belongs to a live lease")
	// ErrGlobalCap and ErrNodeCap are a grant refused by node_max_leases and by
	// node_max_leases_per_node.
	ErrGlobalCap = errors.New("node: the cap on live leases is reached")
	ErrNodeCap   = errors.New("node: the node holds as many leases as it may")
	// ErrNotSameJob is a recovered lease whose job, re-derived after a restart, is not the
	// one that was leased.
	ErrNotSameJob = errors.New("node: the re-derived job is not the leased one")

	// errAlready is a repeat of a call whose effect the row already records. The caller
	// answers it with the recorded result and writes nothing.
	errAlready = errors.New("node: already recorded")
	// errNotDue is an expiry asked of a lease that is not due to expire.
	errNotDue = errors.New("node: the lease is not due to expire")
)

// checkLive is the liveness rule every node call is held to: the epoch presented is the
// row's, the state is one a node may act on, and the server's clock has not reached the
// expiry. A lease AT its expiry instant is expired.
func checkLive(cur Lease, epoch int64, now time.Time) error {
	if cur.Epoch != epoch {
		return fmt.Errorf("%w: the lease is at epoch %d, not %d", ErrGone, cur.Epoch, epoch)
	}
	if !cur.State.Live() {
		return fmt.Errorf("%w: it is %s", ErrGone, cur.State)
	}
	if !now.Before(cur.ExpiresAt) {
		return fmt.Errorf("%w: it ran out at %s", ErrGone, cur.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// caps are the two lease caps a grant is held to.
type caps struct {
	global, perNode int
}

// decideGrant builds the row for a new grant, or refuses it. live is every lease in a live
// state, whatever its clock says: a lease past its expiry and not yet swept still holds
// its path and its place under the caps, so a grant never races the sweep that ends it.
func decideGrant(l Lease, live []Lease, c caps, now time.Time, ttl time.Duration) (Lease, error) {
	onNode := 0
	for _, o := range live {
		if o.Path == l.Path {
			return Lease{}, fmt.Errorf("%w: lease %s of node %s at epoch %d", ErrLeaseHeld, o.ID, o.Node, o.Epoch)
		}
		// Two live leases never share a working file, and a working file is never a
		// leased source: an upload would otherwise be written over another job's bytes.
		if o.Temp == l.Temp || o.Path == l.Temp {
			return Lease{}, fmt.Errorf("%w: %q is held by lease %s of node %s", ErrTempHeld, l.Temp, o.ID, o.Node)
		}
		if o.Node == l.Node {
			onNode++
		}
	}
	if len(live) >= c.global {
		return Lease{}, fmt.Errorf("%w: %d live, the cap is %d", ErrGlobalCap, len(live), c.global)
	}
	if onNode >= c.perNode {
		return Lease{}, fmt.Errorf("%w: %s holds %d, the cap is %d", ErrNodeCap, l.Node, onNode, c.perNode)
	}
	l.State = store.LeaseGranted
	l.ExpiresAt = now.Add(ttl)
	l.GrantedAt, l.UpdatedAt, l.EndedAt = now, now, time.Time{}
	l.OutputDigest, l.OutputBytes, l.SourceDigest, l.UploadAttempts, l.Reason = "", 0, "", 0, ""
	return l, nil
}

// decideRenew is a heartbeat. A live lease is renewed to one TTL from now when extend is
// true; with extend false it is confirmed live and left as it is, which is how a lease the
// restarted server has not taken back keeps exactly the grace it was given.
func decideRenew(cur Lease, epoch int64, now time.Time, ttl time.Duration, extend bool) (Lease, error) {
	if err := checkLive(cur, epoch, now); err != nil {
		return cur, err
	}
	if extend {
		cur.ExpiresAt, cur.UpdatedAt = now.Add(ttl), now
	}
	return cur, nil
}

// uploadAction is what an upload request is answered with before a byte of it is read.
type uploadAction int

const (
	// uploadProceed: the lease is granted and live, and the declared length fits.
	uploadProceed uploadAction = iota
	// uploadDuplicate: the lease already records exactly this digest and length. 200, and
	// nothing is rewritten.
	uploadDuplicate
	// uploadConflict: the lease records a different output. 409.
	uploadConflict
	// uploadGone: the lease is not live at that epoch. 410.
	uploadGone
	// uploadTooLarge: the declared length is past MaxOutputBytes. 413.
	uploadTooLarge
)

// classifyUpload decides an upload request from the row, the epoch, the clock and what the
// request declared. An accepted output is answered from the record whether the lease is
// still uploaded or already completed; every other lease that is not live is gone.
func classifyUpload(cur Lease, epoch int64, now time.Time, digest string, declared int64) uploadAction {
	if cur.Epoch != epoch {
		return uploadGone
	}
	accepted := cur.State == store.LeaseCompleted ||
		(cur.State == store.LeaseUploaded && now.Before(cur.ExpiresAt))
	if accepted {
		if cur.OutputDigest == digest && cur.OutputBytes == declared {
			return uploadDuplicate
		}
		return uploadConflict
	}
	if checkLive(cur, epoch, now) != nil {
		return uploadGone
	}
	if declared > MaxOutputBytes(cur.SourceSize) {
		return uploadTooLarge
	}
	return uploadProceed
}

// decideAdmit admits an upload whose length and digest matched what it declared: a live,
// granted lease becomes uploaded and records both figures. It is the second liveness check
// of an upload, taken in the transaction that admits it, so a lease that ran out or was
// superseded while the body arrived is refused here however the first check answered.
func decideAdmit(cur Lease, epoch int64, now time.Time, digest string, size int64) (Lease, error) {
	if err := checkLive(cur, epoch, now); err != nil {
		return cur, err
	}
	if cur.State != store.LeaseGranted {
		return cur, fmt.Errorf("%w: an output was admitted while this one arrived", ErrDigestConflict)
	}
	cur.State, cur.OutputDigest, cur.OutputBytes, cur.UpdatedAt = store.LeaseUploaded, digest, size, now
	return cur, nil
}

// decideMismatch records an upload refused for a digest mismatch. The node may send the
// body again within the lease; at the bound the lease fails.
func decideMismatch(cur Lease, epoch int64, now time.Time, bound int) (Lease, error) {
	if err := checkLive(cur, epoch, now); err != nil {
		return cur, err
	}
	cur.UploadAttempts++
	cur.UpdatedAt = now
	if cur.UploadAttempts >= bound {
		cur.State, cur.EndedAt = store.LeaseFailed, now
		cur.Reason = string(ReasonDigestMismatch)
	}
	return cur, nil
}

// decideComplete records a node's completion of an admitted output. The digest and length
// it reports must be the ones the upload recorded. A repeat that reports the recorded
// figures is errAlready, answered from the record; one that reports others is a conflict.
func decideComplete(cur Lease, epoch int64, now time.Time, outDigest string, outBytes int64, srcDigest string) (Lease, error) {
	if cur.Epoch != epoch {
		return cur, fmt.Errorf("%w: the lease is at epoch %d, not %d", ErrGone, cur.Epoch, epoch)
	}
	if cur.State == store.LeaseCompleted {
		if cur.OutputDigest == outDigest && cur.OutputBytes == outBytes && cur.SourceDigest == srcDigest {
			return cur, errAlready
		}
		return cur, ErrDigestConflict
	}
	if err := checkLive(cur, epoch, now); err != nil {
		return cur, err
	}
	if cur.State != store.LeaseUploaded {
		return cur, ErrNotUploaded
	}
	if cur.OutputDigest != outDigest || cur.OutputBytes != outBytes {
		return cur, ErrDigestConflict
	}
	cur.State, cur.SourceDigest, cur.UpdatedAt, cur.EndedAt = store.LeaseCompleted, srcDigest, now, now
	return cur, nil
}

// decideCompleteStreamed is decideComplete held to what the server itself streamed.
// streamed is the sha-256 of the bytes the server sent on this lease, and "" where it sent
// no whole source (mapped mode, a ranged request, a restart since). A completion whose
// source digest is not streamed is a source that changed between the server and the node's
// encoder: the lease FAILS, in the transaction that would have completed it, and the row
// keeps the digest the node reported.
func decideCompleteStreamed(cur Lease, epoch int64, now time.Time, outDigest string, outBytes int64, srcDigest, streamed string) (Lease, error) {
	next, err := decideComplete(cur, epoch, now, outDigest, outBytes, srcDigest)
	if err != nil || streamed == "" || srcDigest == streamed {
		return next, err
	}
	next.State, next.Reason = store.LeaseFailed, string(ReasonSourceMismatch)
	return next, nil
}

// decideFail ends a live lease on its node's stated failure. A repeat on a lease that
// already failed is errAlready.
func decideFail(cur Lease, epoch int64, now time.Time, reason string) (Lease, error) {
	if cur.Epoch == epoch && cur.State == store.LeaseFailed {
		return cur, errAlready
	}
	if err := checkLive(cur, epoch, now); err != nil {
		return cur, err
	}
	cur.State, cur.Reason, cur.UpdatedAt, cur.EndedAt = store.LeaseFailed, reason, now, now
	return cur, nil
}

// decideEnd is the SERVER ending a lease, whatever the clock says: the job waiting on it
// ended, the restart found it uploaded, or the re-derived job is not the leased one. Only
// a lease still in a live state is ended; a terminal row is never rewritten.
func decideEnd(cur Lease, now time.Time, reason Reason) (Lease, error) {
	if !cur.State.Live() {
		return cur, fmt.Errorf("%w: it is %s", ErrGone, cur.State)
	}
	cur.State, cur.Reason, cur.UpdatedAt, cur.EndedAt = store.LeaseExpired, string(reason), now, now
	return cur, nil
}

// decideExpire expires a lease whose time ran out: a live state, and the server's clock at
// or past the expiry.
func decideExpire(cur Lease, now time.Time) (Lease, error) {
	if !cur.State.Live() || now.Before(cur.ExpiresAt) {
		return cur, errNotDue
	}
	cur.State, cur.Reason, cur.UpdatedAt, cur.EndedAt = store.LeaseExpired, string(ReasonExpired), now, now
	return cur, nil
}

// decideGrace gives a lease that was live when the server stopped one TTL from now,
// whatever its expiry said: the server's own downtime is not the node's silence.
func decideGrace(cur Lease, now time.Time, ttl time.Duration) (Lease, error) {
	if !cur.State.Live() {
		return cur, fmt.Errorf("%w: it is %s", ErrGone, cur.State)
	}
	cur.ExpiresAt, cur.UpdatedAt = now.Add(ttl), now
	return cur, nil
}

// decideAdopt re-attaches a recovered lease to the job the restarted server re-derived. It
// is the leased job only when the path, the argument-list digest and the source's size and
// modification time are all the lease's; the working file and the reservation become the
// re-derived job's. The expiry is left as the grace set it.
func decideAdopt(cur Lease, job Job, now time.Time) (Lease, error) {
	if err := checkLive(cur, cur.Epoch, now); err != nil {
		return cur, err
	}
	if cur.State != store.LeaseGranted {
		return cur, fmt.Errorf("%w: it is %s", ErrGone, cur.State)
	}
	switch {
	case cur.Path != job.Path:
		return cur, fmt.Errorf("%w: the lease is on %q", ErrNotSameJob, cur.Path)
	case cur.ArgsDigest != ArgsDigest(job.Encoder, job.Pre, job.Body):
		return cur, fmt.Errorf("%w: the argument list moved", ErrNotSameJob)
	case cur.SourceSize != job.SourceSize:
		return cur, fmt.Errorf("%w: the source is %d bytes, the lease was granted on %d", ErrNotSameJob, job.SourceSize, cur.SourceSize)
	case cur.SourceModTime.UnixNano() != job.SourceModTime.UnixNano():
		return cur, fmt.Errorf("%w: the source's modification time moved", ErrNotSameJob)
	}
	cur.Temp, cur.ReservedBytes, cur.UpdatedAt = job.Temp, job.ReservedBytes, now
	return cur, nil
}

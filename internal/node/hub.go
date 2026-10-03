// Package node is the worker-node lease protocol's server side and its pure rules
// (docs/design/nodes.md#leases).
//
// A node only ever ENCODES. The Hub leases one job's encode to one node, takes the node's
// output into the working file the SERVER named, and hands it back to the engine as a
// candidate: every gate and the rename stay the engine's. What the Hub guarantees about a
// candidate is narrow and exact - it arrived on a live lease at the current epoch, and it
// is exactly the bytes whose length and sha-256 the node declared.
package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// The protocol's own defaults. Every one is ASSUMED: nobody has measured a node deployment.
const (
	// DefaultLongPoll bounds how long an acquire waits for work before it answers 204.
	DefaultLongPoll = 30 * time.Second
	// DefaultRetryAfter is the Retry-After a 204 and a 503 carry.
	DefaultRetryAfter = 5 * time.Second
	// DefaultMinUploadRate is the slowest upload, in bytes a second, the read deadline
	// allows for, and DefaultUploadGrace the time it allows on top of that.
	DefaultMinUploadRate = 64 << 10
	DefaultUploadGrace   = 30 * time.Second
	// DefaultDigestRetries is how many uploads of one lease may fail the digest check
	// before the lease fails.
	DefaultDigestRetries = 3
	// DefaultSweepEvery is how often expired leases are looked for.
	DefaultSweepEvery = time.Second
	// DefaultCoolOffAfter is how many leases of one node may end in a row, with none
	// succeeding between, before the node is offered nothing for DefaultCoolOff.
	DefaultCoolOffAfter = 3
	DefaultCoolOff      = 5 * time.Minute
	// DefaultLeaseRetention is how long a terminal lease row is kept before the prune may
	// take it. The newest row of every path is kept regardless.
	DefaultLeaseRetention = 7 * 24 * time.Hour
)

// Errors the engine-facing calls return.
var (
	// ErrNoRoom is the refusal the engine hands Release when its free-space reservation
	// for the job was refused, and the reason an upload is refused when the working
	// file's filesystem cannot take it.
	ErrNoRoom = errors.New("node: not enough free space")
	// ErrPollGone is a ticket whose node stopped waiting before it could be answered.
	ErrPollGone = errors.New("node: the node stopped waiting for work")
	// ErrTicketUsed is a ticket that was already released or encoded on.
	ErrTicketUsed = errors.New("node: the ticket was already used")
	// ErrBadJob is a job the Hub cannot lease as described.
	ErrBadJob = errors.New("node: the job cannot be leased")
	// ErrEncoderUnsupported is a job whose encoder the ticket's node did not report.
	ErrEncoderUnsupported = errors.New("node: the node did not report the job's encoder")
	// ErrNotRecovered is a call made before Recover has run.
	ErrNotRecovered = errors.New("node: Recover has not run")
	// ErrClosed is a call that ended because the Hub's base context did.
	ErrClosed = errors.New("node: the hub is shutting down")
)

// Job is one job's encode as the engine offers it at its encode seam.
type Job struct {
	// Path is the source as the server names it, and Key its store key.
	Path, Key string
	// Temp is the server-named working file the upload must land in. The engine picked it
	// and owns it; no request can name another.
	Temp string
	// Pre and Body are the ffmpeg options before -i and between the input and the output,
	// built from the server's plan. Encoder is the registry key of the encoder it names.
	Pre, Body []string
	Encoder   string
	// SourceSize and SourceModTime are the source as the engine found it.
	SourceSize    int64
	SourceModTime time.Time
	// ReservedBytes is what the engine's free-space hold reserved for this job.
	ReservedBytes int64
}

// validate refuses a job the Hub cannot lease as described.
func (j Job) validate() error {
	switch {
	case !cleanAbs(j.Path):
		return fmt.Errorf("%w: the source path %q is not absolute and clean", ErrBadJob, j.Path)
	case !cleanAbs(j.Temp):
		return fmt.Errorf("%w: the working file %q is not absolute and clean", ErrBadJob, j.Temp)
	case j.Temp == j.Path:
		return fmt.Errorf("%w: the working file is the source itself", ErrBadJob)
	case j.Key == "":
		return fmt.Errorf("%w: it has no store key", ErrBadJob)
	case j.Encoder == "":
		return fmt.Errorf("%w: it names no encoder", ErrBadJob)
	case MaxOutputBytes(j.SourceSize) < 1:
		return fmt.Errorf("%w: a %d-byte source admits no strictly smaller output", ErrBadJob, j.SourceSize)
	case j.ReservedBytes < 0:
		return fmt.Errorf("%w: a reservation of %d bytes", ErrBadJob, j.ReservedBytes)
	case j.SourceModTime.IsZero():
		return fmt.Errorf("%w: it carries no source modification time", ErrBadJob)
	}
	return nil
}

func cleanAbs(p string) bool { return strings.HasPrefix(p, "/") && filepath.Clean(p) == p }

// Result is what a completed lease hands the engine. Temp then holds exactly OutputBytes
// bytes whose sha-256 is OutputDigest, fsynced. SourceDigest is the sha-256 of the source
// bytes the NODE read, as it reported them: the engine compares it with its own hash of the
// source before the gates, which is what catches a wrong path map or a stale mount.
type Result struct {
	LeaseID string
	Node    string
	Epoch   int64
	// OutputDigest and SourceDigest are in the recorded form, `sha-256=:<base64>:`.
	OutputDigest string
	OutputBytes  int64
	SourceDigest string
	// StreamedDigest is the sha-256 of the bytes the SERVER streamed on this lease, in the
	// recorded form, where it streamed the whole source in one unranged response; "" where
	// it did not. A completed lease that carries one has SourceDigest equal to it: the hub
	// fails any other completion (ReasonSourceMismatch).
	StreamedDigest string
	// EncodeSeconds is the encode time the node reported, 0 when it reported none.
	EncodeSeconds float64
}

// LeaseError is a lease that ended without a completed output. The working file the lease
// recorded has been removed.
type LeaseError struct {
	LeaseID string
	Node    string
	Epoch   int64
	Reason  Reason
	// Detail is the node's own typed reason for a ReasonNodeFailed, and otherwise empty.
	Detail string
}

func (e *LeaseError) Error() string {
	msg := fmt.Sprintf("node %s: lease %s at epoch %d ended: %s", e.Node, e.LeaseID, e.Epoch, e.Reason)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

// leaseErrorOf is the typed error a terminal row that is not completed stands for.
func leaseErrorOf(l Lease) *LeaseError {
	e := &LeaseError{LeaseID: l.ID, Node: l.Node, Epoch: l.Epoch, Reason: Reason(l.Reason)}
	if l.State == store.LeaseFailed && e.Reason != ReasonDigestMismatch && e.Reason != ReasonSourceMismatch {
		e.Reason, e.Detail = ReasonNodeFailed, l.Reason
	}
	return e
}

// Options configures a Hub. Ledger is required; a zero in any other field means its
// default.
type Options struct {
	// Ledger holds the lease rows.
	Ledger store.LeaseLedger
	// BaseCtx bounds every long-poll and every wait: cancelling it releases them, so a
	// graceful drain is not held up by a node waiting for work.
	BaseCtx context.Context
	// Version is this build's version. A node at any other is refused.
	Version string
	// TTL is how long a lease lives without a heartbeat (node_lease_ttl_sec). A node is
	// told to heartbeat every quarter of it.
	TTL time.Duration
	// MaxLeases, MaxLeasesPerNode and MaxTransfers are node_max_leases,
	// node_max_leases_per_node and node_max_transfers.
	MaxLeases, MaxLeasesPerNode, MaxTransfers int
	// LongPoll, RetryAfter, MinUploadRate, UploadGrace, DigestRetries, SweepEvery and
	// LeaseRetention default to the constants above.
	LongPoll, RetryAfter time.Duration
	MinUploadRate        int64
	UploadGrace          time.Duration
	DigestRetries        int
	SweepEvery           time.Duration
	LeaseRetention       time.Duration
	// CoolOffAfter is how many leases of one node may end in a row without one succeeding
	// before the node is offered nothing for CoolOff. Both default to the constants above.
	CoolOffAfter int
	CoolOff      time.Duration
	// Now is the server's clock, the only one a lease's liveness is judged by.
	Now func() time.Time
	// FreeSpace reports the free bytes on the filesystem holding a directory
	// (internal/diskfree.Bytes in production). nil skips the upload's free-space check.
	FreeSpace func(dir string) (uint64, error)
	Log       *slog.Logger
}

// Hub is the server side of the lease protocol: the queue of nodes waiting for work, the
// leases, and the uploads. It is safe for concurrent use.
type Hub struct {
	o Options

	// life orders every change to a lease's lifecycle against the working file it owns:
	// a grant, the creation of a working file, and every terminal transition together
	// with the removal of the working file that transition licenses. Held across the
	// ledger transaction and the filesystem call, so a lease that ended can never have
	// its working file removed after a later grant of the same path created it again.
	life sync.Mutex

	// mu guards the in-memory state below and is never held across I/O.
	mu        sync.Mutex
	recovered bool
	ready     bool
	polls     []*poll
	tickets   map[*Ticket]struct{}
	// live is every lease in a live state this process knows of, by id, with its node.
	live map[string]string
	// waits is the engine call waiting on each attached lease.
	waits map[string]*wait
	// uploads is the upload writing each working file, by path.
	uploads map[string]*upload
	// transfers counts the source streams and the uploads in flight.
	transfers int
	// sources is, per lease granted by this process, the mode it was granted in and what
	// the server streamed on it. It is memory only: a lease a restart recovered has no
	// entry, so its mode is unknown and its source is not served (serveSource).
	sources map[string]*leaseSource
	// cooling is, per node, the leases that ended in a row with none succeeding between,
	// and the instant until which the node is offered nothing.
	cooling map[string]*coolState
	// changed is closed and replaced whenever something WaitDemand waits on moves.
	changed chan struct{}
}

// New builds a Hub. Recover must run before it grants anything, and Ready before it
// answers an acquire.
func New(o Options) *Hub {
	if o.BaseCtx == nil {
		o.BaseCtx = context.Background()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	defInt := func(n *int, v int) {
		if *n <= 0 {
			*n = v
		}
	}
	def(&o.TTL, 60*time.Second)
	def(&o.LongPoll, DefaultLongPoll)
	def(&o.RetryAfter, DefaultRetryAfter)
	def(&o.UploadGrace, DefaultUploadGrace)
	def(&o.SweepEvery, DefaultSweepEvery)
	def(&o.LeaseRetention, DefaultLeaseRetention)
	defInt(&o.MaxLeases, 4)
	defInt(&o.MaxLeasesPerNode, 1)
	defInt(&o.MaxTransfers, 2)
	defInt(&o.DigestRetries, DefaultDigestRetries)
	defInt(&o.CoolOffAfter, DefaultCoolOffAfter)
	def(&o.CoolOff, DefaultCoolOff)
	if o.MinUploadRate <= 0 {
		o.MinUploadRate = DefaultMinUploadRate
	}
	return &Hub{
		o: o, tickets: map[*Ticket]struct{}{}, live: map[string]string{},
		waits: map[string]*wait{}, uploads: map[string]*upload{}, changed: make(chan struct{}),
		cooling: map[string]*coolState{}, sources: map[string]*leaseSource{},
	}
}

// leaseSource is what the hub remembers about one lease's source: the mode the lease was
// granted in, and the sha-256 of the bytes the server streamed where it streamed the whole
// source in one unranged response.
type leaseSource struct {
	mode     string
	streamed string
}

// streamedDigest is the digest of the source the server streamed on the lease, or "".
func (h *Hub) streamedDigest(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sources[id]; s != nil {
		return s.streamed
	}
	return ""
}

// wait is one engine call blocked on a lease.
type wait struct {
	once sync.Once
	done chan outcome

	mu       sync.Mutex
	progress func(float64)
	closed   bool
	// encodeSec is the encode time the node's completion reported.
	encodeSec float64
}

type outcome struct {
	res Result
	err error
}

func newWait(progress func(float64)) *wait {
	return &wait{done: make(chan outcome, 1), progress: progress}
}

// finish delivers the outcome, once.
func (w *wait) finish(o outcome) { w.once.Do(func() { w.done <- o }) }

// report hands a heartbeat's progress to the engine, unless its call has returned.
func (w *wait) report(fraction float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed && w.progress != nil {
		w.progress(fraction)
	}
}

func (w *wait) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// upload is one request writing a working file. disowned is set, under Hub.mu, when the
// lease it writes for ended: the file at that path is then no longer this upload's to
// remove.
type upload struct {
	lease    string
	disowned bool
}

// pollState is where an acquire request stands.
type pollState int

const (
	pollQueued pollState = iota
	pollReserved
	pollAnswered
	pollGone
)

// poll is one acquire request waiting for work.
type poll struct {
	node string
	// mode is the mode the request asked for work in.
	mode     string
	encoders []string
	reply    chan pollReply
	state    pollState
}

// pollReply is what an acquire request is answered with: a grant, or a refusal.
type pollReply struct {
	lease *Lease
	job   Job
	// mode is the mode a grant was made in.
	mode   string
	status int
	reason string
	detail string
	// retry is the Retry-After of a refusal that states its own, and 0 for the default.
	retry time.Duration
}

// Ticket is one waiting node's poll, reserved for one job. It is used exactly once: by
// Encode, or by Release.
type Ticket struct {
	p    *poll
	used bool
}

// Node is the name of the node the ticket's poll came from.
func (t *Ticket) Node() string { return t.p.node }

// Mode is the mode that node asked for work in.
func (t *Ticket) Mode() string { return t.p.mode }

// Encoders is the encoders that node's own probe found, as it reported them.
func (t *Ticket) Encoders() []string { return append([]string(nil), t.p.encoders...) }

// Supports reports whether the node reported that encoder.
func (t *Ticket) Supports(encoder string) bool {
	for _, e := range t.p.encoders {
		if e == encoder {
			return true
		}
	}
	return false
}

// notifyLocked wakes every WaitDemand. h.mu is held.
func (h *Hub) notifyLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// nodeLoadLocked counts the live leases and the reserved tickets of one node, and of every
// node. h.mu is held.
func (h *Hub) nodeLoadLocked(node string) (onNode, all int) {
	for _, n := range h.live {
		if n == node {
			onNode++
		}
	}
	for t := range h.tickets {
		if t.p.node == node {
			onNode++
		}
	}
	return onNode, len(h.live) + len(h.tickets)
}

// Ready lets the acquire endpoint grant work. The engine calls it once it has taken back,
// or abandoned, every lease Recover returned. Every recovered lease that is still live is
// given one TTL from NOW first: the grace Recover gave started before the engine's own
// start-up work, and a node must not lose its lease to the time that work took.
func (h *Hub) Ready() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.live))
	for id := range h.live {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	now := h.o.Now()
	for _, id := range ids {
		if _, err := h.apply(ctx, id, func(cur Lease) (Lease, error) {
			return decideGrace(cur, now, h.o.TTL)
		}); err != nil && !errors.Is(err, ErrGone) {
			h.o.Log.Warn("a recovered node lease could not be given its grace again", "lease", id, "err", err)
		}
	}
	h.mu.Lock()
	h.ready = true
	h.mu.Unlock()
}

// pollWaiting reports whether the ticket's node is still waiting on its poll.
func (h *Hub) pollWaiting(t *Ticket) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return t.p.state == pollReserved
}

// coolState is one node's run of leases that ended without one succeeding.
type coolState struct {
	streak  int
	reasons []string
	until   time.Time
}

// ErrCoolingOff is a node that is offered nothing for a while: its leases kept ending
// without one succeeding.
var ErrCoolingOff = errors.New("node: the node is cooling off")

// Report is the engine saying how a lease of that node ended. An empty reason is a lease
// whose output the server took; it clears the node's run. Any other is a lease that ended
// without one - the node could not run it, or it failed - and after CoolOffAfter of those
// in a row the node is offered nothing for CoolOff: its queued polls are answered 503
// `node_cooling_off`, and so is every poll it sends meanwhile. One broken node - a wrong
// path map, a missing mount - therefore stops taking jobs instead of failing every file
// the feed offers it.
func (h *Hub) Report(node, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if reason == "" {
		delete(h.cooling, node)
		return
	}
	if reason == ReasonSourceWithdrawn {
		// The server withdrew the source; the node did nothing wrong. The run is neither
		// lengthened nor cleared.
		return
	}
	c := h.cooling[node]
	if c == nil {
		c = &coolState{}
		h.cooling[node] = c
	}
	c.streak++
	c.reasons = append(c.reasons, reason)
	if c.streak < h.o.CoolOffAfter {
		return
	}
	c.until = h.o.Now().Add(h.o.CoolOff)
	h.o.Log.Warn("node cooling off: its leases kept ending without an output, so it is offered no work for a while",
		"node", node, "ended_in_a_row", c.streak, "reasons", strings.Join(c.reasons, " "), "cool_off", h.o.CoolOff.String())
	c.streak, c.reasons = 0, nil
	kept := h.polls[:0]
	for _, p := range h.polls {
		if p.node != node {
			kept = append(kept, p)
			continue
		}
		p.state = pollAnswered
		p.reply <- h.coolingReplyLocked(c)
	}
	h.polls = kept
	h.notifyLocked()
}

// coolingLocked is the node's cool-off while it is in force, and nil otherwise. h.mu is held.
func (h *Hub) coolingLocked(node string) *coolState {
	c := h.cooling[node]
	if c == nil || !h.o.Now().Before(c.until) {
		return nil
	}
	return c
}

// coolingReplyLocked is the refusal a cooling node's poll is answered with. h.mu is held.
func (h *Hub) coolingReplyLocked(c *coolState) pollReply {
	return pollReply{status: statusUnavailable, reason: errCoolingOff, detail: refusalDetail[errCoolingOff],
		retry: c.until.Sub(h.o.Now())}
}

// WaitDemand blocks until a node is long-polling for work under both lease caps, and
// reserves that poll. The ticket must then be handed to Encode or to Release.
func (h *Hub) WaitDemand(ctx context.Context) (*Ticket, error) {
	for {
		h.mu.Lock()
		for i, p := range h.polls {
			onNode, all := h.nodeLoadLocked(p.node)
			if onNode >= h.o.MaxLeasesPerNode || all >= h.o.MaxLeases {
				continue
			}
			h.polls = append(h.polls[:i], h.polls[i+1:]...)
			p.state = pollReserved
			t := &Ticket{p: p}
			h.tickets[t] = struct{}{}
			h.mu.Unlock()
			return t, nil
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.o.BaseCtx.Done():
			return nil, ErrClosed
		case <-changed:
		}
	}
}

// Release gives an unused ticket back. With a nil refusal the poll goes back to the front
// of the queue and may be matched to another job; a poll that was answered or left while
// it was reserved is simply let go. A refusal that is ErrNoRoom answers the
// poll 503 `no_room`; any other answers it 503 `refused`. Releasing a used ticket does
// nothing.
func (h *Hub) Release(t *Ticket, refusal error) {
	reason := ""
	switch {
	case refusal == nil:
	case errors.Is(refusal, ErrNoRoom):
		reason = errNoRoom
	case errors.Is(refusal, ErrGlobalCap):
		reason = errGlobalCap
	case errors.Is(refusal, ErrNodeCap):
		reason = errNodeCap
	default:
		reason = errRefused
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.takeLocked(t) {
		return
	}
	p := t.p
	switch {
	case p.state != pollReserved:
	case reason != "":
		p.state = pollAnswered
		p.reply <- pollReply{status: statusUnavailable, reason: reason, detail: refusalDetail[reason]}
	default:
		p.state = pollQueued
		h.polls = append([]*poll{p}, h.polls...)
	}
	h.notifyLocked()
}

// takeLocked marks a ticket used and reports whether it was this call that did. h.mu is
// held.
func (h *Hub) takeLocked(t *Ticket) bool {
	if t == nil || t.used {
		return false
	}
	t.used = true
	delete(h.tickets, t)
	return true
}

// Encode grants the lease for job to the ticket's node - the durable row first, then the
// poll is answered - and blocks until the lease ends. A nil error means the output was
// admitted and completed: job.Temp holds exactly the bytes whose length and sha-256 the
// node declared, fsynced, and the Result carries the figures; a nil error never comes with
// the working file removed. Otherwise the error says why:
// a *LeaseError names the node, the epoch and the reason of a lease that ended (and the
// working file has been removed), and any other error means no lease was granted. Either
// way the ticket is used.
//
// progress receives the fraction each heartbeat reports, never after Encode has returned.
func (h *Hub) Encode(ctx context.Context, t *Ticket, job Job, progress func(fraction float64)) (Result, error) {
	h.mu.Lock()
	fresh := t != nil && !t.used
	recovered := h.recovered
	h.mu.Unlock()
	if !fresh {
		return Result{}, ErrTicketUsed
	}
	if !recovered {
		h.Release(t, nil)
		return Result{}, ErrNotRecovered
	}
	if !h.pollWaiting(t) {
		// The node is no longer waiting on this poll: its long-poll ran out and was
		// answered with no work, or its request ended. Nothing is granted - no lease
		// row, nothing recorded against the job - and the ticket is spent.
		h.Release(t, nil)
		return Result{}, fmt.Errorf("%w: node %s", ErrPollGone, t.Node())
	}
	if err := job.validate(); err != nil {
		h.Release(t, nil)
		return Result{}, err
	}
	if !t.Supports(job.Encoder) {
		h.Release(t, nil)
		return Result{}, fmt.Errorf("%w: %s on node %s", ErrEncoderUnsupported, job.Encoder, t.Node())
	}
	id, err := newLeaseID()
	if err != nil {
		h.Release(t, nil)
		return Result{}, err
	}

	// A lease past its expiry still holds its path and its place under the caps until it
	// is swept, so the sweep runs first: the grant is then judged against what is really
	// live.
	h.sweep(ctx)
	w := newWait(progress)
	defer w.close()
	h.life.Lock()
	lease, err := h.o.Ledger.GrantLease(ctx, job.Path, func(live []Lease, _ int64) (Lease, error) {
		return decideGrant(Lease{
			ID: id, Path: job.Path, Key: job.Key, Node: t.Node(), Temp: job.Temp,
			ReservedBytes: job.ReservedBytes, ArgsDigest: ArgsDigest(job.Encoder, job.Pre, job.Body),
			SourceSize: job.SourceSize, SourceModTime: job.SourceModTime,
		}, live, caps{global: h.o.MaxLeases, perNode: h.o.MaxLeasesPerNode}, h.o.Now(), h.o.TTL)
	})
	if err != nil {
		h.life.Unlock()
		if errors.Is(err, ErrGlobalCap) || errors.Is(err, ErrNodeCap) {
			h.Release(t, err)
		} else {
			h.Release(t, nil)
		}
		return Result{}, err
	}
	h.mu.Lock()
	h.takeLocked(t)
	h.live[lease.ID] = lease.Node
	h.waits[lease.ID] = w
	h.sources[lease.ID] = &leaseSource{mode: t.p.mode}
	gone := t.p.state != pollReserved
	if !gone {
		t.p.state = pollAnswered
		granted := lease
		t.p.reply <- pollReply{lease: &granted, job: job, mode: t.p.mode}
	}
	h.notifyLocked()
	h.mu.Unlock()
	h.life.Unlock()
	h.o.Log.Info("node lease granted", "node", lease.Node, "lease", lease.ID, "epoch", lease.Epoch, "path", lease.Path,
		"mode", t.p.mode)
	if gone {
		// The node stopped waiting between the reservation and the grant. Nobody holds
		// this lease's id, so it is ended now rather than left to run out.
		h.end(ctx, lease.ID, ReasonPollGone)
	}
	return h.await(ctx, lease, w)
}

// Adopt re-attaches a lease Recover returned to the job the engine re-derived after a
// restart, then blocks exactly as Encode does. The job is the leased one only when its
// path, its argument list and its source's size and modification time are the lease's;
// otherwise the lease is ended and a *LeaseError with ReasonNotAdopted returned. The
// lease's working file becomes job.Temp. Adopt removes no file: whatever sits at the path
// the lease recorded before the restart is the engine's startup sweep's, as a killed local
// encode's working file is. Before Recover has run it returns ErrNotRecovered.
func (h *Hub) Adopt(ctx context.Context, leaseID string, job Job, progress func(fraction float64)) (Result, error) {
	if !h.isRecovered() {
		return Result{}, ErrNotRecovered
	}
	if err := job.validate(); err != nil {
		return Result{}, err
	}
	w := newWait(progress)
	defer w.close()
	h.life.Lock()
	if h.attached(leaseID) {
		h.life.Unlock()
		return Result{}, fmt.Errorf("%w: lease %s is already attached to a job", ErrBadJob, leaseID)
	}
	lease, err := h.o.Ledger.UpdateLease(ctx, leaseID, func(cur Lease) (Lease, error) {
		return decideAdopt(cur, job, h.o.Now())
	})
	if err != nil {
		h.life.Unlock()
		reason := ReasonExpired
		switch {
		case errors.Is(err, ErrNotSameJob):
			reason = ReasonNotAdopted
		case !errors.Is(err, ErrGone):
			return Result{}, err
		}
		// Not the leased job, or a lease whose grace ran out before the engine came back
		// for it: it is ended here. Nothing was attached, so no file is removed.
		ended, endErr := h.end(ctx, leaseID, reason)
		if endErr != nil || ended.State.Live() || ended.State == store.LeaseCompleted {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("%w: %w", leaseErrorOf(ended), err)
	}
	h.mu.Lock()
	h.live[lease.ID] = lease.Node
	h.waits[lease.ID] = w
	h.mu.Unlock()
	h.life.Unlock()
	h.o.Log.Info("node lease adopted after a restart", "node", lease.Node, "lease", lease.ID, "epoch", lease.Epoch)
	return h.await(ctx, lease, w)
}

// attached reports whether an engine call is waiting on the lease. A lease nothing waits
// on - one a restart recovered and the engine has not taken back - can be heartbeated and
// nothing else.
func (h *Hub) attached(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.waits[id]
	return ok
}

// Abandon ends a lease Recover returned that the engine will not take back. It removes no
// file: the path the lease recorded before the restart is the engine's startup sweep's,
// and by now may hold the engine's own next attempt. An engine that neither adopts nor
// abandons a recovered lease leaves it to run out after its one TTL of grace, which
// removes nothing either.
func (h *Hub) Abandon(ctx context.Context, leaseID string) error {
	_, err := h.end(ctx, leaseID, ReasonNotAdopted)
	return err
}

// Recover is called once at start, before any grant. It returns the leases still live in
// the ledger, having given each one TTL of grace from now, and ends every lease that was
// uploaded but not completed: its output is never gated after the restart, so Recover
// removes its recorded working file itself and the job is leased again from the start. A
// granted lease's working file is left where it is. Until Recover has
// run every lease endpoint answers 503, and until Ready the acquire endpoint does.
func (h *Hub) Recover(ctx context.Context) ([]Lease, error) {
	h.life.Lock()
	defer h.life.Unlock()
	live, err := h.o.Ledger.LiveLeases(ctx)
	if err != nil {
		return nil, err
	}
	now := h.o.Now()
	var kept []Lease
	for _, l := range live {
		if l.State == store.LeaseUploaded {
			ended, err := h.endLocked(ctx, l.ID, ReasonRestart)
			if err != nil {
				return nil, err
			}
			// The one file a restart removes itself. The row is the record that this
			// path holds a node's complete, never-gated upload, and Recover runs before
			// any grant or any engine work, so nothing else can have written there. Left
			// in place, the startup sweep would find a full-length file in the target
			// codec with no job record and hold it back as a stranded replacement.
			if ended.State == store.LeaseExpired {
				h.removeTemp(ended.Temp)
			}
			continue
		}
		graced, err := h.o.Ledger.UpdateLease(ctx, l.ID, func(cur Lease) (Lease, error) {
			return decideGrace(cur, now, h.o.TTL)
		})
		if err != nil {
			return nil, err
		}
		kept = append(kept, graced)
	}
	h.mu.Lock()
	for _, l := range kept {
		h.live[l.ID] = l.Node
	}
	h.recovered = true
	h.mu.Unlock()
	return kept, nil
}

// Run looks for expired leases every SweepEvery, and prunes old terminal rows, until ctx
// ends. It expires nothing until Recover has run. A lease an engine call is waiting on is also swept by that call, so Run is what
// expires the leases nothing waits on: the ones a restart recovered and nobody adopted.
func (h *Hub) Run(ctx context.Context) {
	sweep := time.NewTicker(h.o.SweepEvery)
	defer sweep.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			h.sweep(ctx)
		case <-prune.C:
			h.Prune(ctx)
		}
	}
}

// Prune deletes the terminal lease rows older than the retention, keeping the newest row
// of every path (store.LeaseLedger.PruneLeases).
func (h *Hub) Prune(ctx context.Context) {
	n, err := h.o.Ledger.PruneLeases(ctx, h.o.Now().Add(-h.o.LeaseRetention))
	if err != nil {
		h.o.Log.Warn("pruning node leases failed", "err", err)
		return
	}
	if n > 0 {
		h.o.Log.Info("pruned old node lease rows", "rows", n)
	}
}

// await blocks until the lease ends or ctx does.
func (h *Hub) await(ctx context.Context, lease Lease, w *wait) (Result, error) {
	tick := time.NewTicker(h.o.SweepEvery)
	defer tick.Stop()
	for {
		select {
		case out := <-w.done:
			return out.res, out.err
		case <-tick.C:
			h.sweep(ctx)
		case <-ctx.Done():
			return h.cancel(lease, w)
		case <-h.o.BaseCtx.Done():
			return h.cancel(lease, w)
		}
	}
}

// cancel ends a lease whose engine call is ending, and returns what the lease came to: a
// lease that completed in the same instant is still a completed lease.
func (h *Hub) cancel(lease Lease, w *wait) (Result, error) {
	// The caller's context is done, and the row must still be ended.
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	row, err := h.end(ctx, lease.ID, ReasonCanceled)
	if err == nil {
		w.finish(h.outcomeOf(row, w, nil))
		out := <-w.done
		return out.res, out.err
	}
	// The ledger could not end the row. The engine is leaving all the same.
	h.life.Lock()
	defer h.life.Unlock()
	// Every settlement runs under this lock, so one that got in between the failed
	// ending and here has already delivered its outcome: a lease that completed is still
	// a completed lease, and its working file is the engine's to gate.
	select {
	case out := <-w.done:
		return out.res, out.err
	default:
	}
	// Still unsettled: the lease is detached - no upload is taken on a lease nothing
	// waits on, its heartbeats stop extending it, and whenever its row does end, that
	// ending removes nothing - and the working file it owned until now is removed here.
	h.mu.Lock()
	delete(h.waits, lease.ID)
	delete(h.sources, lease.ID)
	h.mu.Unlock()
	h.removeTemp(lease.Temp)
	return Result{}, fmt.Errorf("%w: %w", &LeaseError{LeaseID: lease.ID, Node: lease.Node,
		Epoch: lease.Epoch, Reason: ReasonCanceled}, err)
}

// outcomeOf is what a terminal row means to the engine call waiting on it.
func (h *Hub) outcomeOf(l Lease, w *wait, src *leaseSource) outcome {
	if l.State != store.LeaseCompleted {
		return outcome{err: leaseErrorOf(l)}
	}
	res := Result{LeaseID: l.ID, Node: l.Node, Epoch: l.Epoch, OutputDigest: l.OutputDigest,
		OutputBytes: l.OutputBytes, SourceDigest: l.SourceDigest}
	if src != nil {
		res.StreamedDigest = src.streamed
	}
	if w != nil {
		w.mu.Lock()
		res.EncodeSeconds = w.encodeSec
		w.mu.Unlock()
	}
	return outcome{res: res}
}

// apply runs one decision on one lease under the lifecycle lock and, when the row it wrote
// is terminal, settles the lease: a lease that did not complete has its recorded working
// file removed, and the engine call waiting on it is released either way.
func (h *Hub) apply(ctx context.Context, id string, decide func(Lease) (Lease, error)) (Lease, error) {
	h.life.Lock()
	defer h.life.Unlock()
	return h.applyLocked(ctx, id, decide)
}

func (h *Hub) applyLocked(ctx context.Context, id string, decide func(Lease) (Lease, error)) (Lease, error) {
	row, err := h.o.Ledger.UpdateLease(ctx, id, decide)
	if err != nil {
		return row, err
	}
	if !row.State.Live() {
		h.settleLocked(row)
	}
	return row, nil
}

// settleLocked is what follows a lease's terminal transition. h.life is held.
//
// The working file is removed only while the lease is ATTACHED: an engine call in this
// process is waiting on it, so the file at that path is this lease's and nothing else's.
// A lease nothing waits on - one a restart recovered and the engine did not take back, or
// one whose engine call left while the ledger could not end the row - no longer owns the
// path: the engine may have written its next attempt there, so its late ending unlinks
// nothing.
func (h *Hub) settleLocked(row Lease) {
	h.mu.Lock()
	w := h.waits[row.ID]
	src := h.sources[row.ID]
	delete(h.waits, row.ID)
	delete(h.live, row.ID)
	delete(h.sources, row.ID)
	h.notifyLocked()
	h.mu.Unlock()
	if row.State != store.LeaseCompleted {
		if w != nil {
			h.removeTemp(row.Temp)
		}
		h.o.Log.Warn("node lease ended without an output", "node", row.Node, "lease", row.ID,
			"epoch", row.Epoch, "state", string(row.State), "reason", row.Reason, "attached", w != nil)
	}
	if w != nil {
		w.finish(h.outcomeOf(row, w, src))
	}
}

// removeTemp removes one lease's recorded working file, and disowns the upload writing it
// if there is one. It is the only removal the Hub makes outside an upload's own cleanup,
// it is only ever handed the path a lease row recorded, and its callers call it only for a
// lease that still owns that path. h.life is held.
func (h *Hub) removeTemp(temp string) {
	h.mu.Lock()
	if u := h.uploads[temp]; u != nil {
		u.disowned = true
		delete(h.uploads, temp)
	}
	h.mu.Unlock()
	if err := os.Remove(temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.o.Log.Warn("could not remove an ended lease's working file", "path", temp, "err", err)
	}
}

// end is the server ending a lease. It returns the row as it now stands: the ended row, or
// - when the lease had already ended some other way - the row as it was read.
func (h *Hub) end(ctx context.Context, id string, reason Reason) (Lease, error) {
	h.life.Lock()
	defer h.life.Unlock()
	return h.endLocked(ctx, id, reason)
}

func (h *Hub) endLocked(ctx context.Context, id string, reason Reason) (Lease, error) {
	row, err := h.applyLocked(ctx, id, func(cur Lease) (Lease, error) {
		return decideEnd(cur, h.o.Now(), reason)
	})
	if errors.Is(err, ErrGone) {
		return row, nil
	}
	return row, err
}

// sweep expires every live lease whose time ran out.
func (h *Hub) sweep(ctx context.Context) {
	h.life.Lock()
	defer h.life.Unlock()
	// Before Recover has given the restart's grace, every expiry in the ledger is one the
	// server's own downtime ran out. Nothing is expired on it.
	if !h.isRecovered() {
		return
	}
	live, err := h.o.Ledger.LiveLeases(ctx)
	if err != nil {
		if ctx.Err() == nil {
			h.o.Log.Warn("reading live node leases failed", "err", err)
		}
		return
	}
	now := h.o.Now()
	for _, l := range live {
		if now.Before(l.ExpiresAt) {
			continue
		}
		if _, err := h.applyLocked(ctx, l.ID, func(cur Lease) (Lease, error) {
			return decideExpire(cur, now)
		}); err != nil && !errors.Is(err, errNotDue) && ctx.Err() == nil {
			h.o.Log.Warn("expiring a node lease failed", "lease", l.ID, "err", err)
		}
	}
}

func (h *Hub) isRecovered() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recovered
}

// newLeaseID is 16 random bytes in hex: unguessable, and unique per grant.
func newLeaseID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("node: no random lease id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// validLeaseID reports whether id has the shape newLeaseID produces.
func validLeaseID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/probe"
)

// Worker nodes at the encode seam (docs/design/nodes.md#seam).
//
// A node job is an ordinary ProcessFile whose ENCODE STEP is "lease it to a node and wait
// for the upload into the working file this job already named". Everything before the seam
// - the guards, the claim, the server-named working file, the free-space hold, the owner
// record - and everything after it - every gate, the play hold, the copy beside the source,
// the durable rename - is ProcessFile's own code, run on the server, unchanged. What a node
// hands back is a candidate and nothing more: no verdict of a node licenses a swap.

// Nodes is the lease protocol's server side as the encode seam uses it. *node.Hub is the
// production implementation. A nil Engine.Nodes is nodes OFF: the pool, every command line,
// every decision and every log record are then exactly what they are without this file.
type Nodes interface {
	// WaitDemand blocks until a node is asking for work, and reserves its poll.
	WaitDemand(ctx context.Context) (*node.Ticket, error)
	// Release gives an unused ticket back; a used one is left alone.
	Release(t *node.Ticket, refusal error)
	// Encode leases job to the ticket's node and blocks until the lease ends.
	Encode(ctx context.Context, t *node.Ticket, job node.Job, progress func(fraction float64)) (node.Result, error)
	// Adopt re-attaches a lease a restart recovered to the job re-derived for it.
	Adopt(ctx context.Context, leaseID string, job node.Job, progress func(fraction float64)) (node.Result, error)
	// Abandon ends a recovered lease the engine will not take back.
	Abandon(ctx context.Context, leaseID string) error
	// Report says how a lease of that node ended: "" for an output the server took, and
	// otherwise why it ended without one. The hub cools a node off on a run of those.
	Report(node, reason string)
}

// DefaultNodeRedemand is how long a job whose node stopped waiting before the grant waits
// for another node to ask for work before the server encodes it itself: two long-poll
// bounds. ASSUMED, like the long-poll it is taken from.
const DefaultNodeRedemand = 2 * node.DefaultLongPoll

// defaultFeederIdle is how long a feeder holds a node's poll with no file arriving from the
// feed before it lets the poll go and asks again. ASSUMED.
const defaultFeederIdle = time.Second

// The stages of an adoption: its job has not reached the seam, has reached it and taken the
// lease back, or the lease was abandoned first.
const (
	adoptPending int32 = iota
	adoptReached
	adoptAbandoned
)

// errNodeInterrupted is a lease the SERVER ended because it is stopping. It is an
// interruption, exactly as a cancelled local encode is: nothing is recorded against the file.
var errNodeInterrupted = errors.New("the node lease was ended because the server is stopping")

// nodeJob is what a job offered to a node carries from the feeder (or from the restart's
// adoption) to the seam, through its context: the ticket of the node that asked for work, or
// the id of the lease to take back. Everything but reached is touched only by the job's own
// goroutine.
type nodeJob struct {
	// ticket is the poll this job may be leased to; nil on an adoption.
	ticket *node.Ticket
	// adopt is the id of the recovered lease this job takes back; "" on a feeder's job.
	adopt string
	// reached is closed once an adoption has re-taken its holds and is about to re-attach
	// its lease, or has ended without doing so. nil on a feeder's job.
	reached     chan struct{}
	reachedOnce sync.Once
	// stage is where an adoption stands (adoptPending, adoptReached, adoptAbandoned): the
	// job's own goroutine and the start-up deadline race for it, and exactly one wins.
	stage atomic.Int32
	// spent says the seam has run once for this job: a second encode of the same job (the
	// hardware fallback's) is the server's own.
	spent bool
	// releaseSlot gives back the node gate slot this job holds; nil while it holds none.
	releaseSlot func()
}

type nodeJobKey struct{}

func withNodeJob(ctx context.Context, nj *nodeJob) context.Context {
	return context.WithValue(ctx, nodeJobKey{}, nj)
}

func nodeJobFrom(ctx context.Context) *nodeJob {
	nj, _ := ctx.Value(nodeJobKey{}).(*nodeJob)
	return nj
}

func (nj *nodeJob) markReached() {
	if nj.reached != nil {
		nj.reachedOnce.Do(func() { close(nj.reached) })
	}
}

// nodeGate is the node gate slots (node_gate_slots): how many node jobs the server does its
// own heavy work for at once - the source hash, every gate, the copy and the swap of a node's
// output, or the encode of a job that turned out not to be leasable - beside the `workers`
// local jobs. A slot is held from the moment a node's output is handed back (or the local
// encode starts) until ProcessFile returns.
type nodeGate struct {
	mu            sync.Mutex
	slots         int
	held, waiting int
	// parked is how many feeders are held in waitRoom right now.
	parked  int
	changed chan struct{}
}

func newNodeGate(slots int) *nodeGate {
	if slots < 1 {
		slots = 1
	}
	return &nodeGate{slots: slots, changed: make(chan struct{})}
}

func (g *nodeGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquire takes a slot, waiting for one while every slot is held.
func (g *nodeGate) acquire(ctx context.Context) (release func(), err error) {
	g.mu.Lock()
	g.waiting++
	g.notifyLocked()
	for g.held >= g.slots {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.notifyLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
		g.mu.Lock()
	}
	g.waiting--
	g.held++
	g.notifyLocked()
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.held--
			g.notifyLocked()
			g.mu.Unlock()
		})
	}, nil
}

// full reports whether the gate queue is full: every slot is held AND as many jobs again
// are already waiting for one. While it is, no feeder asks for demand, so a node's poll is
// answered with no work rather than with a job whose output would only queue.
func (g *nodeGate) full() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held >= g.slots && g.waiting >= g.slots
}

// waitRoom blocks while the gate queue is full.
func (g *nodeGate) waitRoom(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.held < g.slots || g.waiting < g.slots {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.parked++
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
		g.parked--
		g.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// gate is this engine's node gate, built on first use from node_gate_slots.
func (e *Engine) gate() *nodeGate {
	e.nodeGateOnce.Do(func() { e.nodeGate = newNodeGate(e.Cfg.EffectiveNodeGateSlots()) })
	return e.nodeGate
}

// takeSlot takes this job's node gate slot, once.
func (e *Engine) takeSlot(ctx context.Context, nj *nodeJob) error {
	if nj.releaseSlot != nil {
		return nil
	}
	release, err := e.gate().acquire(ctx)
	if err != nil {
		return err
	}
	nj.releaseSlot = release
	return nil
}

// settle is every way out of a node job: an unused ticket goes back to its node's poll, a
// recovered lease that never reached the hub is abandoned, and the gate slot is released.
func (e *Engine) settle(ctx context.Context, nj *nodeJob) {
	if nj.ticket != nil {
		// Releasing a used ticket does nothing, so this is safe on every way out.
		e.Nodes.Release(nj.ticket, nil)
	}
	if nj.adopt != "" && ctx.Err() == nil && nj.stage.CompareAndSwap(adoptPending, adoptAbandoned) {
		// The job never reached the seam - the guards now skip the file, it is gone - so
		// the lease is ended rather than left to run out. A shutdown is not that: the lease
		// stays for the next start to recover.
		e.abandon(ctx, nj.adopt)
	}
	nj.markReached()
	if nj.releaseSlot != nil {
		nj.releaseSlot()
		nj.releaseSlot = nil
	}
}

// refuseNoRoom answers a node job's poll 503 `no_room`: the free-space reservation for the
// job it was offered was refused. A job that carries no ticket is left alone.
func (e *Engine) refuseNoRoom(ctx context.Context) {
	if nj := nodeJobFrom(ctx); nj != nil && nj.ticket != nil {
		e.Nodes.Release(nj.ticket, node.ErrNoRoom)
	}
}

// startFeeders starts the node feeders of one pass: node_max_leases goroutines beside the
// local workers, each of which takes a file from the feed ONLY while it holds a demand
// ticket. With no node asking for work a feeder consumes nothing, so the local workers see
// the feed exactly as they do with nodes off. The returned stop is called once the feed has
// closed: it releases every feeder still waiting for demand, and wg then joins them, so no
// feeder outlives the pass and no ticket is left reserved.
func (e *Engine) startFeeders(ctx context.Context, wg *sync.WaitGroup, feed <-chan string,
	process func(ctx context.Context, workerID, f string) (done bool)) (stop func()) {
	wctx, stop := context.WithCancel(ctx)
	for i := 0; i < e.Cfg.EffectiveNodeMaxLeases(); i++ {
		workerID := "n" + strconv.Itoa(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// The server's own gate queue is full: ask no node for more work.
				if e.gate().waitRoom(wctx) != nil {
					return
				}
				t, err := e.Nodes.WaitDemand(wctx)
				if err != nil {
					return
				}
				idle := time.NewTimer(e.feederIdle())
				select {
				case f, ok := <-feed:
					idle.Stop()
					if !ok {
						e.Nodes.Release(t, nil)
						return
					}
					nj := &nodeJob{ticket: t}
					done := process(withNodeJob(ctx, nj), workerID, f)
					e.settle(ctx, nj)
					if done {
						return
					}
				case <-idle.C:
					// Nothing to offer - the feed is paused, or every file is with a local
					// worker. The poll is not held for a file that may never come: it goes
					// back, so the node is answered by its own long-poll bound.
					e.Nodes.Release(t, nil)
				case <-ctx.Done():
					idle.Stop()
					e.Nodes.Release(t, nil)
					return
				}
			}
		}()
	}
	return stop
}

// feederIdle is how long a feeder holds a poll with no file arriving.
func (e *Engine) feederIdle() time.Duration {
	if e.nodeFeederIdle > 0 {
		return e.nodeFeederIdle
	}
	return defaultFeederIdle
}

// abandon ends a recovered lease the engine will not take back.
func (e *Engine) abandon(ctx context.Context, leaseID string) {
	if err := e.Nodes.Abandon(ctx, leaseID); err != nil {
		e.Log.Warn("node: a recovered lease could not be abandoned; it runs out after its grace",
			"lease", leaseID, "err", err)
	}
}

// AdoptLeases takes back the leases a restart recovered (node.Hub.Recover), before anything
// new is granted: each lease's path is run through ProcessFile again - all of them at once -
// which claims the row, re-takes the free-space hold and the working file through its
// ordinary code, re-derives the plan, and at the seam re-attaches the lease (Adopt), which
// refuses a job that is not the leased one. A lease whose job ends before the seam is
// abandoned. It returns once EVERY lease has re-taken its holds and reached Adopt, or has
// been abandoned; the caller then lets the hub grant (Ready). The jobs themselves run on, and
// wait joins them.
//
// It is bounded by within (one lease TTL in production): a lease whose job has not reached
// the seam by then is abandoned, and that job carries on as the server's own. So a job held
// up before its seam - waiting for room, probing a slow mount - cannot hold the listener.
//
// The rows a dead process left active are reset first, so each recovered path is claimable.
func (e *Engine) AdoptLeases(ctx context.Context, leases []node.Lease, within time.Duration) (wait func()) {
	if e.Nodes == nil || len(leases) == 0 {
		return func() {}
	}
	e.recoverStale(ctx)
	var wg sync.WaitGroup
	jobs := make([]*nodeJob, len(leases))
	for i, l := range leases {
		nj := &nodeJob{adopt: l.ID, reached: make(chan struct{})}
		jobs[i] = nj
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.ProcessFile(withNodeJob(ctx, nj), "adopt"+strconv.Itoa(i), l.Path)
			e.settle(ctx, nj)
			if err != nil && ctx.Err() == nil {
				e.Log.Warn("node: taking a recovered lease back failed", "file", l.Path, "lease", l.ID, "err", err)
			}
		}()
	}
	late := time.NewTimer(within)
	defer late.Stop()
	for i, nj := range jobs {
		select {
		case <-nj.reached:
			continue
		case <-ctx.Done():
			return wg.Wait
		case <-late.C:
		}
		// The bound ran out. Every lease whose job is not at the seam yet is abandoned now;
		// a job that gets there afterwards finds its lease gone and encodes on the server.
		for k, rest := range jobs[i:] {
			if rest.stage.CompareAndSwap(adoptPending, adoptAbandoned) {
				l := leases[i+k]
				e.Log.Warn("node: a recovered lease was not taken back within the start-up bound, so it is "+
					"abandoned and its job carries on as the server's own", "file", l.Path, "lease", l.ID,
					"node", l.Node, "bound", within.String())
				e.abandon(ctx, l.ID)
				rest.markReached()
			}
		}
		break
	}
	return wg.Wait
}

// leaseArgs is the leased command line: the plan's own options with NO libx265 parallelism
// (the node's libx265 uses its own defaults; the server's pool figures describe the server)
// and the container's options, exactly as runCarrying appends them. The fixed leading
// options, -i <source>, the muxer-queue bounds and -- <output> are added on the node by
// RunLeased, which is the function every local encode's command line goes through too.
func (p *EncodePlan) leaseArgs() (pre, body []string, err error) {
	pre, body, err = p.args(encoder.X265Parallelism{})
	if err != nil {
		return nil, nil, err
	}
	return pre, append(append([]string(nil), body...), p.container.args()...), nil
}

// leasable decides whether this plan's command line is self-contained on another host, and
// returns it when it is. why is "" exactly then. A plan is leasable only when it names a
// software encoder the node reported, decodes in software, opens no device, normalises no
// audio track (the loudness gate reads the encoder's own report channel), carries no dynamic
// HDR (its pre-pass files sit beside the working file) and attaches no picture (copied out
// beside the working file first). Every other job is encoded by the server, as it always was.
func (p *EncodePlan) leasable(supports func(encoder string) bool) (pre, body []string, why string) {
	switch {
	case p.Video.Copy:
		return nil, nil, "the video is stream-copied, which the server does itself"
	case p.Video.Encoder.Hardware:
		return nil, nil, "the encoder is a hardware encoder"
	case p.Video.HardwareDecode():
		return nil, nil, "the source is decoded on a device"
	case p.Video.Device != "" || p.Video.DecodeDevice != "":
		return nil, nil, "the plan opens a device"
	case len(p.AudioTracks.Normalised()) > 0:
		return nil, nil, "an audio track is loudness-normalised"
	case p.dynamic != nil:
		return nil, nil, "the plan carries dynamic HDR metadata"
	case len(p.coverArt.attached) > 0:
		return nil, nil, "a picture is carried as a Matroska attachment"
	case !supports(p.Video.Encoder.Key):
		return nil, nil, "the node did not report the encoder " + p.Video.Encoder.Key
	}
	pre, body, err := p.leaseArgs()
	if err != nil {
		return nil, nil, err.Error()
	}
	if len(pre) > 0 {
		return nil, nil, "the command line carries a device argument"
	}
	for _, a := range body {
		// Nothing of a leased command line may name a file of this host: the node
		// substitutes its own source and output and nothing else.
		if strings.Contains(a, p.Output) || strings.Contains(a, p.Source) {
			return nil, nil, "the command line names a file beside the working file"
		}
	}
	return pre, body, ""
}

// NodeVerdictError is a node's completed lease the server refused before its gates: the
// source the node read is not the server's, or the working file is not the output the lease
// recorded. The working file is removed and the job fails at the encode gate.
type NodeVerdictError struct {
	Node    string
	LeaseID string
	Epoch   int64
	Why     string
}

func (e *NodeVerdictError) Error() string {
	return fmt.Sprintf("node %s: lease %s at epoch %d: %s", e.Node, e.LeaseID, e.Epoch, e.Why)
}

// encodeAt is the encode seam as ProcessFile calls it. A job that carries no node job - every
// job with nodes off, and every local worker's job with them on - is Engine.encode, untouched.
//
// A node job is leased where it can be. Where it cannot - its plan is not leasable, no lease
// was granted, or the node could not run the lease it was given - the SERVER encodes it, in
// this same attempt, inside a node gate slot, and nothing is recorded against the file: none
// of those is a fact about the file.
func (e *Engine) encodeAt(ctx context.Context, worker, key string, fi os.FileInfo, in, out string,
	props *probe.VideoProps, job *EncodePlan) error {
	nj := nodeJobFrom(ctx)
	if nj == nil {
		return e.encode(ctx, worker, in, out, props, job)
	}
	first := !nj.spent
	nj.spent = true
	if first {
		handled, err := e.encodeOnNode(ctx, nj, worker, key, fi, in, out, props, job)
		if handled {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := e.takeSlot(ctx, nj); err != nil {
		return err
	}
	return e.encode(ctx, worker, in, out, props, job)
}

// nodeCouldNotRun are the typed reasons a worker fails a lease with when IT could not run
// the job: its path map, its mount, its encoders, its own refusal of the command line, its
// own shutdown - and, in http mode, a source it could not download as media or a work
// directory with no room for it. They say something about the node and nothing about the
// file.
var nodeCouldNotRun = map[string]bool{
	"unmapped_source": true, "source_mismatch": true, "source_unreadable": true,
	"unsupported_encoder": true, "refused_plan": true, "worker_stopping": true,
	"source_download_failed": true, "work_dir_full": true,
}

// encodeOnNode leases the job (or takes its recovered lease back) and waits for the node's
// output to be admitted and completed into the working file. handled reports whether the
// node path concluded the encode step: with a nil error the working file holds a candidate
// that passed the two checks below; with an error the lease was really attempted and the job
// fails. handled false means the server encodes the job itself, and the one record saying
// why has been written.
//
// How a lease that ended without an output is read (docs/design/nodes.md#endings):
//
//   - the node stopped waiting before the grant: the job waits for another node to ask, for
//     up to NodeRedemand, and is leased to that one;
//   - the node could not run it (nodeCouldNotRun), the lease was not the re-derived job's, or
//     the hub granted nothing: the server encodes it. Not charged to the file;
//   - the lease was really attempted - it expired, the node's encode failed, its uploads
//     failed the digest bound: the job fails, transient, and max_failures counts it.
//
// A nil error from the hub is a CANDIDATE, and two things about it are checked before any
// gate: the server hashes ITS OWN copy of the source and compares it with the digest of the
// bytes the node read - a mismatch is a wrong worker_path_map entry or a stale mount on the
// node, or in http mode a source that changed after it was streamed, and the job fails -
// and the working file is the length the lease recorded. In http mode the hub has already
// held the node's digest to the digest of what it streamed (node.ReasonSourceMismatch); this
// check is taken in both modes all the same, so a ranged download is never unchecked.
func (e *Engine) encodeOnNode(ctx context.Context, nj *nodeJob, worker, key string, fi os.FileInfo, in, out string,
	props *probe.VideoProps, job *EncodePlan) (handled bool, err error) {
	server := func(why string, args ...any) (bool, error) {
		e.Log.Info("node: this job is encoded by the server", append([]any{"file", in, "worker", worker, "why", why}, args...)...)
		return false, nil
	}
	if nj.adopt != "" && !nj.stage.CompareAndSwap(adoptPending, adoptReached) {
		return server("its recovered lease was abandoned before the job came back for it", "lease", nj.adopt)
	}
	var redemandUntil time.Time
	for {
		supports := func(string) bool { return true }
		if nj.ticket != nil {
			supports = nj.ticket.Supports
		}
		pre, body, why := job.leasable(supports)
		if why != "" {
			// The poll goes back to its node, or the recovered lease is ended.
			if nj.ticket != nil {
				e.Nodes.Release(nj.ticket, nil)
			} else {
				e.abandon(ctx, nj.adopt)
				nj.markReached()
			}
			return server(why)
		}
		lj := node.Job{Path: in, Key: key, Temp: out, Pre: pre, Body: body, Encoder: job.Video.Encoder.Key,
			SourceSize: fi.Size(), SourceModTime: fi.ModTime(), ReservedBytes: fi.Size()}
		// The plan the leased command line was built from is announced exactly where a local
		// encode announces its own, so "the command line and the gates read one plan" is as
		// checkable for a node's job as for the server's.
		e.observePlan(planStageEncode, job.Streams)
		e.observeEncodePlan(planStageEncode, job)

		progress := e.nodeProgress(worker, in, props)
		var res node.Result
		if nj.adopt != "" {
			// The holds are re-taken: the working file is held, the free-space reservation is
			// held, the row is claimed. The hub may grant again once every recovered lease is
			// here.
			nj.markReached()
			res, err = e.Nodes.Adopt(ctx, nj.adopt, lj, progress)
		} else {
			res, err = e.Nodes.Encode(ctx, nj.ticket, lj, progress)
		}
		if err == nil {
			return true, e.acceptNodeOutput(ctx, nj, worker, in, lj, res)
		}
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		var ended *node.LeaseError
		leaseEnded := errors.As(err, &ended)
		switch {
		case errors.Is(err, node.ErrPollGone) || (leaseEnded && ended.Reason == node.ReasonPollGone):
			// The node's request ended, or its long-poll ran out, between the reservation
			// and the grant. Nothing is wrong with the file and no node saw it: the job waits
			// for a node that is asking.
			if nj.ticket == nil {
				return server("no node holds its recovered lease", "err", err.Error())
			}
			if redemandUntil.IsZero() {
				redemandUntil = time.Now().Add(e.nodeRedemand())
			}
			wctx, stop := context.WithDeadline(ctx, redemandUntil)
			t, werr := e.Nodes.WaitDemand(wctx)
			stop()
			if werr != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return server("the node it was offered to stopped waiting, and no other node asked for work "+
					"within "+e.nodeRedemand().String(), "node", nj.ticket.Node())
			}
			nj.ticket = t
			continue
		case leaseEnded && (ended.Reason == node.ReasonCanceled || ended.Reason == node.ReasonRestart):
			// The server itself ended it: it is stopping.
			return true, fmt.Errorf("%w: %w", errNodeInterrupted, err)
		case leaseEnded && ended.Reason == node.ReasonNotAdopted:
			return server("the job re-derived after the restart is not the one that was leased",
				"node", ended.Node, "lease", ended.LeaseID, "epoch", ended.Epoch)
		case leaseEnded && ended.Reason == node.ReasonNodeFailed && nodeCouldNotRun[ended.Detail]:
			// The NODE could not run it. That is a fact about the node - the hub counts it
			// towards the node's cool-off - and not about the file.
			e.Nodes.Report(ended.Node, ended.Detail)
			return server("the node could not run the lease", "node", ended.Node, "lease", ended.LeaseID,
				"epoch", ended.Epoch, "reason", ended.Detail)
		case leaseEnded:
			// The lease was really attempted and ended without an output.
			reason := string(ended.Reason)
			if ended.Detail != "" {
				reason = ended.Detail
			}
			e.Nodes.Report(ended.Node, reason)
			return true, err
		}
		// Not a lease's ending: the hub granted nothing (a cap, a held path), or could not
		// take the recovered lease back. That lease is ended rather than left to run out.
		if nj.adopt != "" {
			e.abandon(ctx, nj.adopt)
		}
		return server("no lease was granted", "err", err.Error())
	}
}

// nodeRedemand is how long a job whose node stopped waiting waits for another.
func (e *Engine) nodeRedemand() time.Duration {
	if e.NodeRedemand > 0 {
		return e.NodeRedemand
	}
	return DefaultNodeRedemand
}

// acceptNodeOutput is the server's own two checks of a completed lease, before any gate,
// taken inside a node gate slot. It tells the hub how the lease ended either way.
func (e *Engine) acceptNodeOutput(ctx context.Context, nj *nodeJob, worker, in string, lj node.Job, res node.Result) error {
	// The server's own heavy work for this output starts here.
	if err := e.takeSlot(ctx, nj); err != nil {
		return err
	}
	e.Log.Info("node encode", "file", in, "worker", worker, "node", res.Node, "lease", res.LeaseID,
		"epoch", res.Epoch, "output_bytes", res.OutputBytes, "output_digest", res.OutputDigest,
		"source_digest", res.SourceDigest, "node_encode_sec", res.EncodeSeconds,
		"source_streamed_digest", res.StreamedDigest)
	refuse := func(reason, why string) error {
		e.Nodes.Report(res.Node, reason)
		return &NodeVerdictError{Node: res.Node, LeaseID: res.LeaseID, Epoch: res.Epoch, Why: why}
	}
	own, err := sourceDigest(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return refuse("source_unhashed", "the server could not hash its own copy of the source to compare with the node's: "+err.Error())
	}
	if own != res.SourceDigest {
		return refuse("source_digest_mismatch", fmt.Sprintf("the source the node read is not the server's (the node read %s, the "+
			"server's copy is %s): a wrong worker_path_map entry or a stale mount on the node, or a source that "+
			"changed after it was streamed; nothing was gated and the source is untouched", res.SourceDigest, own))
	}
	st, err := os.Stat(lj.Temp)
	if err != nil {
		return refuse("output_unreadable", "the working file the lease completed into could not be read: "+err.Error())
	}
	if st.Size() != res.OutputBytes {
		return refuse("output_size_mismatch", fmt.Sprintf("the working file is %d bytes and the lease recorded %d", st.Size(), res.OutputBytes))
	}
	e.Nodes.Report(res.Node, "")
	return nil
}

// sourceDigest is the sha-256 of the file at path in the form a lease records one, read
// sequentially and abandoned as soon as ctx ends: a source is as large as a film, and a
// stopping server does not finish reading one.
func sourceDigest(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			return node.FormatDigest(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// nodeProgress turns a heartbeat's fraction into the live progress event a local encode
// emits. nil where nobody observes.
func (e *Engine) nodeProgress(worker, path string, props *probe.VideoProps) func(float64) {
	if e.Observer == nil {
		return nil
	}
	dur := sourceDuration(props)
	sink := e.progressSink(worker, path, dur)
	return func(fraction float64) {
		var p Progress
		if dur != nil {
			p.PositionSec = fraction * *dur
		}
		sink(p)
	}
}

// nodeFailure is what the encode-failure branch does for a failure that came from a node:
// it reports whether err is one, and when it is, remembers the attempt so the record that
// parks the file can name every node that was tried.
func (e *Engine) nodeFailure(path string, err error) bool {
	var ended *node.LeaseError
	var verdict *NodeVerdictError
	var attempt string
	switch {
	case errors.As(err, &ended):
		attempt = fmt.Sprintf("%s@%d:%s", ended.Node, ended.Epoch, ended.Reason)
		if ended.Detail != "" {
			attempt += "(" + ended.Detail + ")"
		}
	case errors.As(err, &verdict):
		attempt = fmt.Sprintf("%s@%d:refused_before_gates", verdict.Node, verdict.Epoch)
	default:
		return false
	}
	e.nodeAttemptsMu.Lock()
	if e.nodeAttempts == nil {
		e.nodeAttempts = map[string][]string{}
	}
	e.nodeAttempts[path] = append(e.nodeAttempts[path], attempt)
	if e.nodeAttemptsKeep == nil {
		e.nodeAttemptsKeep = map[string]bool{}
	}
	// The failed row this attempt is about to write is the one terminal outcome that keeps
	// what was remembered (forgetNodeAttempts).
	e.nodeAttemptsKeep[path] = true
	e.nodeAttemptsMu.Unlock()
	return true
}

// reportNodePark says, once, that the retry bound has parked a file whose failures came from
// nodes, naming each node attempt this process saw. The park itself is the existing
// max_failures machinery's: Claim holds the row out from here on.
func (e *Engine) reportNodePark(ctx context.Context, path, key string) {
	_, fails, exists, err := e.Store.Get(ctx, path, key)
	if err != nil || !exists || e.Cfg.MaxFailures <= 0 || fails < e.Cfg.MaxFailures {
		return
	}
	e.nodeAttemptsMu.Lock()
	attempts := e.nodeAttempts[path]
	delete(e.nodeAttempts, path)
	e.nodeAttemptsMu.Unlock()
	e.Log.Warn("node: the retry bound parked this file (source untouched); it is held out until "+
		"`holdfast requeue` offers it again", "file", path, "fail_count", fails, "max_failures", e.Cfg.MaxFailures,
		"node_attempts", strings.Join(attempts, " "))
}

// forgetNodeAttempts runs at every terminal outcome of a file: what was remembered about its
// node attempts is dropped, unless the outcome is the node failure that was just remembered.
// So the map holds an entry only for a file whose LAST outcome was a failed node attempt, and
// that entry goes when the file is parked, swapped, skipped or failed some other way.
func (e *Engine) forgetNodeAttempts(path string) {
	e.nodeAttemptsMu.Lock()
	defer e.nodeAttemptsMu.Unlock()
	if e.nodeAttemptsKeep[path] {
		delete(e.nodeAttemptsKeep, path)
		return
	}
	delete(e.nodeAttempts, path)
}

// RunLeased runs one leased encode on a node: ffmpeg over in, writing out, with the options
// the lease carried before the input (pre) and between the input and the output (body). The
// command line is assembled by the one function every encode of this build goes through
// (runFFmpegWith) - the fixed leading options, -i, the muxer-queue bounds, -- and the output
// - so a node's command line and the server's cannot drift apart.
func (e FFmpegEncoder) RunLeased(ctx context.Context, in, out string, pre, body []string, sink ProgressSink) error {
	return e.runFFmpeg(ctx, in, out, sink, pre, body)
}

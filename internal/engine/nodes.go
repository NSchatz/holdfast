package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

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
}

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
	// spent says the seam has run once for this job: a second encode of the same job (the
	// hardware fallback's) is the server's own.
	spent bool
	// leased says the lease reached the hub, by Encode or by Adopt.
	leased bool
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
	changed       chan struct{}
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
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
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
	if nj.adopt != "" && !nj.leased && ctx.Err() == nil {
		// The job never reached the seam - the guards now skip the file, it is gone, its plan
		// is no longer one a node can run - so the lease is ended rather than left to run out.
		// A shutdown is not that: the lease stays for the next start to recover.
		if err := e.Nodes.Abandon(ctx, nj.adopt); err != nil {
			e.Log.Warn("node: a recovered lease could not be abandoned; it runs out after its grace",
				"lease", nj.adopt, "err", err)
		}
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
				select {
				case f, ok := <-feed:
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
				case <-ctx.Done():
					e.Nodes.Release(t, nil)
					return
				}
			}
		}()
	}
	return stop
}

// AdoptLeases takes back the leases a restart recovered (node.Hub.Recover), before anything
// new is granted: each lease's path is run through ProcessFile again, which claims the row,
// re-takes the free-space hold and the working file through its ordinary code, re-derives the
// plan, and at the seam re-attaches the lease (Adopt) - which refuses a job that is not the
// leased one. A lease whose job never reaches the seam is abandoned. It returns once EVERY
// lease has re-taken its holds and reached Adopt, or has been abandoned; the caller then lets
// the hub grant (Ready). The jobs themselves run on, and wait joins them.
//
// The rows a dead process left active are reset first, so each recovered path is claimable.
func (e *Engine) AdoptLeases(ctx context.Context, leases []node.Lease) (wait func()) {
	if e.Nodes == nil || len(leases) == 0 {
		return func() {}
	}
	e.recoverStale(ctx)
	var wg sync.WaitGroup
	for i, l := range leases {
		nj := &nodeJob{adopt: l.ID, reached: make(chan struct{})}
		wg.Add(1)
		go func() {
			defer wg.Done()
			jctx := withNodeJob(ctx, nj)
			err := e.ProcessFile(jctx, "adopt"+strconv.Itoa(i), l.Path)
			e.settle(ctx, nj)
			if err != nil && ctx.Err() == nil {
				e.Log.Warn("node: taking a recovered lease back failed", "file", l.Path, "lease", l.ID, "err", err)
			}
		}()
		select {
		case <-nj.reached:
		case <-ctx.Done():
		}
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
func (e *Engine) encodeAt(ctx context.Context, worker, key string, fi os.FileInfo, in, out string,
	props *probe.VideoProps, job *EncodePlan) error {
	nj := nodeJobFrom(ctx)
	if nj == nil {
		return e.encode(ctx, worker, in, out, props, job)
	}
	first := !nj.spent
	nj.spent = true
	if first {
		supports := func(string) bool { return true }
		if nj.ticket != nil {
			supports = nj.ticket.Supports
		}
		pre, body, why := job.leasable(supports)
		if why == "" {
			leased, err := e.encodeOnNode(ctx, nj, worker, in, props, job, node.Job{
				Path: in, Key: key, Temp: out, Pre: pre, Body: body, Encoder: job.Video.Encoder.Key,
				SourceSize: fi.Size(), SourceModTime: fi.ModTime(), ReservedBytes: fi.Size(),
			})
			if leased || err == nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			why = "no lease was granted: " + err.Error()
		}
		e.Log.Info("node: this job is encoded by the server", "file", in, "worker", worker, "why", why)
		// The poll goes back to its node, or the recovered lease is ended: the server encodes.
		if nj.ticket != nil {
			e.Nodes.Release(nj.ticket, nil)
		} else if !nj.leased {
			nj.leased = true
			if err := e.Nodes.Abandon(ctx, nj.adopt); err != nil {
				e.Log.Warn("node: a recovered lease could not be abandoned; it runs out after its grace",
					"lease", nj.adopt, "err", err)
			}
		}
		nj.markReached()
	}
	if err := e.takeSlot(ctx, nj); err != nil {
		return err
	}
	return e.encode(ctx, worker, in, out, props, job)
}

// encodeOnNode leases the job (or takes its recovered lease back) and waits for the node's
// output to be admitted and completed into the working file. leased reports whether a lease
// reached the hub: false with an error means nothing was granted, and the server encodes.
//
// A nil error is a CANDIDATE in the working file, and two things about it are then checked
// here, before any gate: the server hashes ITS OWN copy of the source and compares it with
// the digest of the bytes the node read - a mismatch is a wrong worker_path_map entry or a
// stale mount on the node, and the job fails - and the working file is the length the lease
// recorded.
func (e *Engine) encodeOnNode(ctx context.Context, nj *nodeJob, worker, in string, props *probe.VideoProps,
	job *EncodePlan, lj node.Job) (leased bool, err error) {
	// The plan the leased command line was built from is announced exactly where a local
	// encode announces its own, so "the command line and the gates read one plan" is as
	// checkable for a node's job as for the server's.
	e.observePlan(planStageEncode, job.Streams)
	e.observeEncodePlan(planStageEncode, job)

	progress := e.nodeProgress(worker, in, props)
	var res node.Result
	if nj.adopt != "" {
		nj.leased = true
		// The holds are re-taken: the working file is held, the free-space reservation is
		// held, the row is claimed. The hub may grant again once every recovered lease is here.
		nj.markReached()
		res, err = e.Nodes.Adopt(ctx, nj.adopt, lj, progress)
	} else {
		res, err = e.Nodes.Encode(ctx, nj.ticket, lj, progress)
		var ended *node.LeaseError
		if err != nil && !errors.As(err, &ended) {
			return false, err
		}
		nj.leased = true
	}
	if err != nil {
		return true, err
	}
	// The server's own heavy work for this output starts here, inside a node gate slot.
	if err := e.takeSlot(ctx, nj); err != nil {
		return true, err
	}
	e.Log.Info("node encode", "file", in, "worker", worker, "node", res.Node, "lease", res.LeaseID,
		"epoch", res.Epoch, "output_bytes", res.OutputBytes, "output_digest", res.OutputDigest,
		"source_digest", res.SourceDigest, "node_encode_sec", res.EncodeSeconds)
	refuse := func(why string) (bool, error) {
		return true, &NodeVerdictError{Node: res.Node, LeaseID: res.LeaseID, Epoch: res.Epoch, Why: why}
	}
	sum, _, err := fileDigest(in)
	if err != nil {
		return refuse("the server could not hash its own copy of the source to compare with the node's: " + err.Error())
	}
	raw, _ := hex.DecodeString(sum)
	if own := node.FormatDigest(raw); own != res.SourceDigest {
		return refuse(fmt.Sprintf("the source the node read is not the server's (the node read %s, the server's "+
			"copy is %s): a wrong worker_path_map entry or a stale mount on the node; nothing was gated and "+
			"the source is untouched", res.SourceDigest, own))
	}
	st, err := os.Stat(lj.Temp)
	if err != nil {
		return refuse("the working file the lease completed into could not be read: " + err.Error())
	}
	if st.Size() != res.OutputBytes {
		return refuse(fmt.Sprintf("the working file is %d bytes and the lease recorded %d", st.Size(), res.OutputBytes))
	}
	return true, nil
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

// forgetNodeAttempts drops what was remembered about a file whose job ended some other way.
func (e *Engine) forgetNodeAttempts(path string) {
	e.nodeAttemptsMu.Lock()
	delete(e.nodeAttempts, path)
	e.nodeAttemptsMu.Unlock()
}

// RunLeased runs one leased encode on a node: ffmpeg over in, writing out, with the options
// the lease carried before the input (pre) and between the input and the output (body). The
// command line is assembled by the one function every encode of this build goes through
// (runFFmpegWith) - the fixed leading options, -i, the muxer-queue bounds, -- and the output
// - so a node's command line and the server's cannot drift apart.
func (e FFmpegEncoder) RunLeased(ctx context.Context, in, out string, pre, body []string, sink ProgressSink) error {
	return e.runFFmpeg(ctx, in, out, sink, pre, body)
}

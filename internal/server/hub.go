package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// Reporting caps. A library can hold hundreds of thousands of rows; the API never
// ships an unbounded payload. The queue (pending+active) and history (terminal)
// views are capped — documented as a known limitation (the UI shows the most
// recent activity, not the entire ledger).
const (
	queueLimit   = 500
	historyLimit = 200
)

// activeAndPending are the non-terminal statuses shown in the "queue" view.
var activeAndPending = []store.Status{store.Pending, store.Probing, store.Encoding, store.Verifying}

// terminal are the statuses shown in the "history" view.
//
// The two FILESYSTEM-1 outcomes are here for a reason that is not cosmetic: a job
// parked indeterminate, or one applied despite an error, must be reported AS THE STATE
// IT IS IN. Leaving them out would have made a parked job - the one job on the whole
// dashboard that is actually waiting for a human - the only job that never appears
// anywhere, which is the same failure as reporting it as a success.
var terminal = []store.Status{
	store.Done, store.Skipped, store.Failed,
	store.Indeterminate, store.AppliedDespiteError,
}

// jobDTO is the wire shape of one job row (a reporting projection of store.Job —
// fingerprint is intentionally omitted; it is an internal dedup key, not UI data).
//
// The outcome fields (TRANSCODE-13) are what makes `GET /api/history` honest: the
// README has always documented it as returning terminal jobs "with reason", and until
// now there was no reason field to return.
//
// The numeric outcome fields are POINTERS and are deliberately NOT `omitempty`: they
// serialize as an explicit JSON `null` when the fact was never recorded, so a client
// can tell "not recorded" from a real 0 and render it as such. Dropping the key (or
// emitting 0) would hand the UI a fabricated fidelity score — precisely the overclaim
// the whole fidelity track exists to prevent.
type jobDTO struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Worker    string `json:"worker,omitempty"`
	FailCount int    `json:"fail_count"`
	UpdatedAt int64  `json:"updated_at"`

	// Reason: the error text for a failed job, or WHICH GUARD skipped a skipped one
	// (a stable token — see internal/engine's Skip* constants).
	Reason string `json:"reason,omitempty"`
	// Encoder that ran (cpu / svtav1 / nvenc / …).
	Encoder string `json:"encoder,omitempty"`
	// The VMAF pair and the model that produced it. A score is meaningless without its
	// model, so they travel together or not at all.
	VmafMean  *float64 `json:"vmaf_mean"`
	VmafMin   *float64 `json:"vmaf_min"`
	VmafModel string   `json:"vmaf_model,omitempty"`
	// The sizes either side of the swap, and how long the encode took.
	SourceBytes *int64 `json:"source_bytes"`
	OutputBytes *int64 `json:"output_bytes"`
	EncodeMs    *int64 `json:"encode_ms"`

	// Live progress of a RUNNING encode (S0030). These are the only fields here that do
	// not come from the store: progress is state about a process, not a ledger fact, so
	// it is never persisted and a terminal row never carries it.
	//
	// They follow the same pointer discipline as the outcome fields above, for the same
	// reason and with a sharper edge: an encoder that has reported nothing yet, and a
	// source whose container reports no duration, are both UNRECORDED and go out as an
	// explicit null. A zero here would say "0% encoded" — a figure nobody measured, on
	// the one page an operator uses to decide whether the tool is working.
	//
	// They are carried for a job in `encoding` and for no other state. A row that has
	// moved on to verifying carries three nulls, because the encoder whose position this
	// was has exited and nothing is measuring the verify phase (that is out of scope by
	// the spec, and elapsed covers it).
	//
	// ProgressSeconds is the encoder's position in the source timeline; ProgressDuration
	// is the length it is measured against; ProgressFraction is the two divided, clamped
	// into [0,1], and present only when BOTH of the others are.
	ProgressSeconds  *float64 `json:"progress_seconds"`
	ProgressDuration *float64 `json:"progress_duration_seconds"`
	ProgressFraction *float64 `json:"progress_fraction"`

	// The source-mutation guard's achieved granularity for this job (FILESYSTEM-1):
	// which attributes it compared, the resolution of the timestamp it compared, and
	// which of the two documented residual windows applies to the storage it ran
	// against. The window is a CLASS LABEL naming a statement in the shipped
	// documentation, never a duration - the network one belongs to the client's
	// attribute cache and has no value anyone can honestly state.
	GuardAttributes     string `json:"guard_attributes,omitempty"`
	GuardTimeResolution string `json:"guard_time_resolution,omitempty"`
	GuardResidualWindow string `json:"guard_residual_window,omitempty"`
	// SwapCause names a swap failure's cause when it is one holdfast reports
	// distinctly - today only "cross-filesystem". Absent for every other failure.
	SwapCause string `json:"swap_cause,omitempty"`
}

func toDTOs(jobs []store.Job) []jobDTO {
	out := make([]jobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobDTO{
			Path:      j.Path,
			Status:    string(j.Status),
			Worker:    j.Worker,
			FailCount: j.FailCount,
			UpdatedAt: j.UpdatedAt,

			Reason:      j.Outcome.Reason,
			Encoder:     j.Outcome.Encoder,
			VmafMean:    j.Outcome.VmafMean,
			VmafMin:     j.Outcome.VmafMin,
			VmafModel:   j.Outcome.VmafModel,
			SourceBytes: j.Outcome.SourceBytes,
			OutputBytes: j.Outcome.OutputBytes,
			EncodeMs:    j.Outcome.EncodeMs,

			GuardAttributes:     j.Outcome.GuardAttributes,
			GuardTimeResolution: j.Outcome.GuardTimeResolution,
			GuardResidualWindow: j.Outcome.GuardResidualWindow,
			SwapCause:           j.Outcome.SwapCause,
		})
	}
	return out
}

// snapshot is the full state the SSE stream pushes and the read endpoints compose.
//
// BytesReclaimedLifetime is the DURABLE total the dashboard leads with (TRANSCODE-14):
// it survives restarts, where BytesReclaimedSession — a per-PROCESS counter — resets to
// 0. It is computed WITHOUT the unbounded per-snapshot table scan the phase warned
// against: a one-time baseline (SUM over done rows, read once when the Hub is built) plus
// the session counter the engine already maintains with atomics. Because the baseline is
// read before this process encodes anything, and the session counter accrues exactly this
// process's Done reclaims, baseline + session is the true lifetime total with no double
// counting and O(1) per snapshot. BytesReclaimedSession is kept for continuity (the old
// header figure) and because it is the honest "this run" number.
//
// Aggregates carries the whole-ledger figures (DASH-7) - the ones Queue and History
// arithmetically cannot give, because those ship at most queueLimit / historyLimit rows
// while the table holds the operator's entire library.
//
// Now is the server's wall clock when the frame was built, in unix seconds. It is the
// BASIS for every in-state elapsed figure the page shows (S0030): a client derives
// elapsed from `now - updated_at` plus the time since the frame arrived, rather than
// ticking a counter of its own. That matters twice over — a browser throttles a
// background tab's timers under policies with no normative guarantee, so an accumulated
// counter drifts while a derived one cannot; and a client whose clock is skewed against
// the server's would otherwise render nonsense (or a negative age) from updated_at alone.
type snapshot struct {
	Summary                map[string]int `json:"summary"`
	Queue                  []jobDTO       `json:"queue"`
	History                []jobDTO       `json:"history"`
	BytesReclaimedSession  int64          `json:"bytes_reclaimed_session"`
	BytesReclaimedLifetime int64          `json:"bytes_reclaimed_lifetime"`
	Paused                 bool           `json:"paused"`
	Scanning               bool           `json:"scanning"`
	Now                    int64          `json:"now"`
	Aggregates             aggregatesDTO  `json:"aggregates"`
}

// aggregateUnavailable is the ONE text an unreadable figure ships, and it is a fixed
// string rather than the store's error on purpose. The read endpoints need no
// authorization and already return full media paths; they must not additionally start
// leaking whatever a driver put in an error message. The real error is logged, where an
// operator with server access can read it - the page is told only that the figure could
// not be read, which is the whole of what it needs to render honestly.
const aggregateUnavailable = "this figure could not be read from the ledger"

// bucketDTO is one keyed count - a status, or a skip guard's token (which /api/history
// already ships per row as `reason`, so the breakdown exposes no new kind of fact).
type bucketDTO struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// The two aggregate wire shapes. Their field names FREEZE at the first published tag,
// so they are chosen to say what they mean without a legend:
//
//	available   - false when the figure could not be read; the page renders it as
//	              unavailable and keeps drawing everything else.
//	unavailable - why, in fixed words (see aggregateUnavailable); "" when available.
//	covers      - the SET the figure is over, always stated, never implied.
//	window      - "" for a figure over that whole set; otherwise the bound narrowing it.
//	counted     - rows that contributed a value.
//	excluded    - matching rows that recorded none, reported rather than dropped.
//
// min/mean/max are pointers and deliberately NOT omitempty: they serialize as explicit
// null when nothing was recorded, the same discipline the per-row outcome fields keep,
// so a client can tell "no data" from a real 0 and can never render an average of zero
// for a library nobody measured.
type spreadDTO struct {
	Available   bool     `json:"available"`
	Unavailable string   `json:"unavailable"`
	Covers      string   `json:"covers"`
	Window      string   `json:"window"`
	Counted     int64    `json:"counted"`
	Excluded    int64    `json:"excluded"`
	Min         *float64 `json:"min"`
	Mean        *float64 `json:"mean"`
	Max         *float64 `json:"max"`
}

type breakdownDTO struct {
	Available   bool        `json:"available"`
	Unavailable string      `json:"unavailable"`
	Covers      string      `json:"covers"`
	Window      string      `json:"window"`
	Counted     int64       `json:"counted"`
	Excluded    int64       `json:"excluded"`
	Buckets     []bucketDTO `json:"buckets"`
}

// aggregatesDTO is the published set. Every member is an aggregate - a count, a
// breakdown or a spread - and never a per-file datum: the figures widen what the
// unauthenticated read surface says about the LIBRARY, and nothing about a file that
// /api/queue and /api/history did not already say.
type aggregatesDTO struct {
	Outcomes     breakdownDTO `json:"outcomes"`
	SkipsByGuard breakdownDTO `json:"skips_by_guard"`
	SizeRatio    spreadDTO    `json:"size_ratio"`
	EncodeMs     spreadDTO    `json:"encode_ms"`
	VmafMean     spreadDTO    `json:"vmaf_mean"`
	VmafMin      spreadDTO    `json:"vmaf_min"`
}

// Hub is the engine.Observer and the SSE fan-out. Engine workers call Observe
// (non-blocking); a single Run goroutine coalesces events, rebuilds the snapshot
// from the store (the source of truth), and broadcasts to subscribers. Decoupling
// this way is a hard requirement: an engine worker must NEVER block on a slow HTTP
// client, or the API could stall an encode.
type Hub struct {
	store store.Store
	ctrl  *Controller
	log   *slog.Logger

	// events is the non-blocking hand-off from engine workers to Run. Buffered and
	// coalesced: if it is full the event is dropped, because the next snapshot
	// re-reads full state anyway — granularity is lost, never correctness.
	events chan engine.Event

	// bytesReclaimed accumulates the reclaimed-space total for this PROCESS, and
	// resets when the daemon does. Updated in Observe with atomics so it is never lost
	// even when the event is coalesced.
	bytesReclaimed atomic.Int64

	// reclaimedBaseline is the lifetime reclaimed total AS OF Hub construction — a
	// single SUM over the store's done rows, read once (never per snapshot). The
	// durable lifetime total the dashboard shows is reclaimedBaseline + bytesReclaimed:
	// the baseline is everything reclaimed before this process started, the counter is
	// what this process has reclaimed since, and the two never overlap because the
	// baseline is frozen at startup. Immutable after NewHub, so it needs no lock.
	reclaimedBaseline int64

	// progMu guards progress. It is deliberately NOT mu: mu is held across the whole
	// subscriber fan-out in broadcast, and an engine worker reporting progress must
	// never queue behind that.
	progMu sync.Mutex
	// progress is the live position of each RUNNING encode, keyed by job path. It is the
	// one piece of hub state that is not derived from the store, because it describes a
	// process rather than a row — so it is never persisted, it is dropped the moment the
	// job leaves `encoding` for ANY state (see Observe), and it is pruned against the
	// store's own encoding rows on every snapshot (a Done event names the POST-swap path,
	// so the pre-swap key would otherwise linger).
	progress map[string]engine.Progress

	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub builds a Hub over the store and controller. It reads the durable lifetime
// reclaimed baseline ONCE here (a single SUM over done rows) — before the engine has
// encoded anything this process, so it captures exactly "everything reclaimed before
// now". A read error is non-fatal: the baseline falls back to 0 and the lifetime
// figure degrades to this-process-only until the next restart, which is a display
// nicety, never correctness — so a store hiccup must not stop the server coming up.
func NewHub(st store.Store, ctrl *Controller, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	baseline, err := st.ReclaimedTotal(context.Background())
	if err != nil {
		log.Warn("reclaimed baseline read failed (lifetime total starts from this run only)", "err", err)
		baseline = 0
	}
	return &Hub{
		store:             st,
		ctrl:              ctrl,
		log:               log,
		events:            make(chan engine.Event, 256),
		subs:              make(map[chan []byte]struct{}),
		progress:          make(map[string]engine.Progress),
		reclaimedBaseline: baseline,
	}
}

// ReclaimedLifetime is the durable lifetime reclaimed total: the startup baseline plus
// everything this process has reclaimed since. See snapshot for why this is O(1) and
// double-count-free.
func (h *Hub) ReclaimedLifetime() int64 { return h.reclaimedBaseline + h.bytesReclaimed.Load() }

// Observe implements engine.Observer. It runs on an engine worker goroutine — and, for a
// progress report, on the goroutine draining a running encoder's progress pipe — so it
// only does cheap, non-blocking things: bump the reclaimed-bytes counter, record or drop
// the job's live progress, and hand the event to Run (dropping it if the buffer is full).
//
// The live-progress rule is deliberately narrow: an entry exists WHILE THE ENCODER IS
// RUNNING and at no other time. So any transition that is not into `encoding` drops it —
// not merely a terminal one. A position measured by a process that has exited describes
// nothing that is still happening, and the page would render it as a frozen percentage
// beside a state it does not describe: for the whole verify/VMAF phase, which on a
// feature-length source is the longest stretch an operator watches, and equally for a job
// RecoverStale returns to `pending`. The spec puts a verify figure out of scope ("the
// verifying state is covered by elapsed alone") and the page promises "no carry-over of
// the last figure"; both are the same rule, enforced here.
//
// Note the drop happens BEFORE the hand-off below, which may be dropped when the buffer
// is full. Losing an event costs granularity; losing this would leave a stale figure on
// the wire, which is correctness.
func (h *Hub) Observe(ev engine.Event) {
	if n := ev.BytesReclaimed(); n > 0 {
		h.bytesReclaimed.Add(n)
	}
	switch {
	case ev.Progress != nil:
		h.setProgress(ev.Path, *ev.Progress)
	case ev.Status == store.Encoding:
		// Still encoding: whatever its encoder last reported still describes it.
	default:
		h.clearProgress(ev.Path)
	}
	select {
	case h.events <- ev:
	default: // buffer full: coalesce — the next broadcast re-reads full state
	}
}

func (h *Hub) setProgress(path string, p engine.Progress) {
	if path == "" {
		return
	}
	h.progMu.Lock()
	h.progress[path] = p
	h.progMu.Unlock()
}

func (h *Hub) clearProgress(path string) {
	if path == "" {
		return // Trigger's synthetic event names no job
	}
	h.progMu.Lock()
	delete(h.progress, path)
	h.progMu.Unlock()
}

// liveProgressFor prunes the live progress table down to the jobs the STORE still has in
// `encoding` and returns what remains. The prune is what makes the live table's rule hold
// on its own rather than depending on every transition event arriving: the events channel
// drops when it is full, a Done event names the POST-swap path (so a container-changing
// swap would strand the source path's entry), and a job can leave `encoding` in the store
// without this process seeing the event at all.
//
// It prunes against `encoding` specifically, not against the whole active list, for the
// reason Observe spells out: the figure is a measurement taken by a process, and it is
// live only while that process is running. A verifying or re-pended row is an active row
// with NO progress figure — which the wire states as an explicit null, never as the
// finished encode's last percentage frozen in place.
func (h *Hub) liveProgressFor(jobs []store.Job) map[string]engine.Progress {
	encoding := make(map[string]struct{}, len(jobs))
	for _, j := range jobs {
		if j.Status == store.Encoding {
			encoding[j.Path] = struct{}{}
		}
	}
	h.progMu.Lock()
	defer h.progMu.Unlock()
	for path := range h.progress {
		if _, ok := encoding[path]; !ok {
			delete(h.progress, path)
		}
	}
	out := make(map[string]engine.Progress, len(h.progress))
	for path, p := range h.progress {
		out[path] = p
	}
	return out
}

// liveProgressCount is the number of jobs currently reporting live progress. Test-only
// accessor: "publishes no live progress entries" is otherwise invisible from outside,
// since an empty queue trivially carries no rows to hang a figure on.
func (h *Hub) liveProgressCount() int {
	h.progMu.Lock()
	defer h.progMu.Unlock()
	return len(h.progress)
}

// queueDTOs projects active jobs and annotates each with whatever live progress its
// encoder has reported. Both the SSE snapshot and GET /api/queue go through here, so the
// stream and the read endpoint cannot disagree about a running job.
func (h *Hub) queueDTOs(jobs []store.Job) []jobDTO {
	dtos := toDTOs(jobs)
	live := h.liveProgressFor(jobs)
	for i := range dtos {
		p, ok := live[dtos[i].Path]
		if !ok {
			continue
		}
		applyProgress(&dtos[i], p)
	}
	return dtos
}

// applyProgress writes one live report onto a job's wire shape, clamping it into a range
// that can be rendered honestly.
//
// The clamps are not defensive noise. An encoder legitimately reports a position at or
// slightly past the source duration on its final report (a container's duration is
// itself an approximation), and an unclamped divide would publish "103% encoded"; a
// negative position — ffmpeg emits negative timestamps during pre-roll on some inputs —
// would publish a negative one. Neither is a figure an operator can act on. The fraction
// is published ONLY when the duration is known and positive: with no length to measure
// against there is no fraction to compute, and inventing one is the failure mode this
// whole surface exists to avoid.
func applyProgress(d *jobDTO, p engine.Progress) {
	pos := p.PositionSec
	if !(pos > 0) { // also catches NaN
		pos = 0
	}
	if p.DurationSec == nil || !(*p.DurationSec > 0) {
		// No length to measure against: the position stands alone and there is no
		// fraction, because there is nothing to divide by.
		d.ProgressSeconds = &pos
		return
	}
	dur := *p.DurationSec
	if pos > dur {
		// At or past the end: report it as fully encoded. The position is clamped as
		// well as the fraction, so nothing on the wire reads past the length beside it.
		pos = dur
	}
	frac := pos / dur
	d.ProgressSeconds = &pos
	d.ProgressDuration = &dur
	d.ProgressFraction = &frac
}

// Trigger forces a broadcast without an engine event (used by the controller's
// onChange so a pause/scan-state flip reaches SSE clients promptly). Non-blocking.
func (h *Hub) Trigger() { h.Observe(engine.Event{}) }

// BytesReclaimed returns the session reclaimed-space total.
func (h *Hub) BytesReclaimed() int64 { return h.bytesReclaimed.Load() }

// Run coalesces events and broadcasts snapshots until ctx is cancelled. Start it in
// a goroutine before serving.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.events:
			// Coalesce: drain everything queued so a burst of transitions produces
			// one broadcast, not one per event.
			drained := true
			for drained {
				select {
				case <-h.events:
				default:
					drained = false
				}
			}
			h.broadcast(ctx)
		}
	}
}

// broadcast builds the current snapshot and pushes it to every subscriber, dropping
// for any subscriber whose buffer is full (a slow client just gets the next one).
func (h *Hub) broadcast(ctx context.Context) {
	snap, err := h.buildSnapshot(ctx)
	if err != nil {
		h.log.Warn("snapshot build failed (skipping broadcast)", "err", err)
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		h.log.Warn("snapshot marshal failed", "err", err)
		return
	}
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- data:
		default: // slow subscriber: drop this frame, it will get the next
		}
	}
	h.mu.Unlock()
}

// Subscribe registers a new SSE subscriber and returns its channel plus a cancel
// func to unregister it (call on connection close).
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, cancel
}

// SnapshotJSON returns the current snapshot as marshaled JSON (used for the initial
// SSE frame and reusable by handlers/tests).
func (h *Hub) SnapshotJSON(ctx context.Context) ([]byte, error) {
	snap, err := h.buildSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(snap)
}

// buildSnapshot reads the store (the source of truth) plus the live control/counter
// state into a snapshot. Pure reads — it never mutates a row.
func (h *Hub) buildSnapshot(ctx context.Context) (snapshot, error) {
	sum, err := h.store.Summary(ctx)
	if err != nil {
		return snapshot{}, err
	}
	queue, err := h.store.List(ctx, activeAndPending, queueLimit)
	if err != nil {
		return snapshot{}, err
	}
	hist, err := h.store.List(ctx, terminal, historyLimit)
	if err != nil {
		return snapshot{}, err
	}
	counts := make(map[string]int, len(sum))
	for st, n := range sum {
		counts[string(st)] = n
	}
	return snapshot{
		Summary: counts,
		Queue:   h.queueDTOs(queue),
		// History rows are terminal, so they are projected WITHOUT live progress — a
		// finished file carries the proof its swap was safe, never a running figure.
		History:                toDTOs(hist),
		BytesReclaimedSession:  h.bytesReclaimed.Load(),
		BytesReclaimedLifetime: h.ReclaimedLifetime(),
		Paused:                 h.ctrl.Paused(),
		Scanning:               h.ctrl.Scanning(),
		Now:                    time.Now().Unix(),
		Aggregates:             h.aggregates(ctx),
	}, nil
}

// aggregates reads the whole-ledger figures for a snapshot. It CANNOT fail the
// snapshot, and that is the point: buildSnapshot returns an error on any store read
// failure and broadcast then skips the frame entirely, so an aggregate read on that
// all-or-nothing path would let one unreadable figure blank the live page for every
// subscriber. Each figure carries its own error out of the store instead, and an
// unreadable one is marked unavailable while the summary, the queue and the history
// still ship and the broadcast still fires.
func (h *Hub) aggregates(ctx context.Context) aggregatesDTO {
	a := h.store.Aggregates(ctx)
	return aggregatesDTO{
		Outcomes:     h.breakdown("outcomes", a.Outcomes),
		SkipsByGuard: h.breakdown("skips_by_guard", a.SkipsByGuard),
		SizeRatio:    h.spread("size_ratio", a.SizeRatio),
		EncodeMs:     h.spread("encode_ms", a.EncodeMs),
		VmafMean:     h.spread("vmaf_mean", a.VmafMean),
		VmafMin:      h.spread("vmaf_min", a.VmafMin),
	}
}

// breakdown projects one keyed aggregate, logging the real error where an operator can
// read it and shipping only the fixed unavailable text. A failed figure carries NO
// counts and NO buckets: an unreadable breakdown that reported zeros would be
// indistinguishable from a ledger in which nothing has happened yet.
func (h *Hub) breakdown(name string, b store.Breakdown) breakdownDTO {
	out := breakdownDTO{
		Available: b.Err == nil,
		Covers:    b.Coverage.Set,
		Window:    b.Coverage.Window,
	}
	if b.Err != nil {
		h.log.Warn("aggregate unavailable (the rest of the snapshot still ships)", "aggregate", name, "err", b.Err)
		out.Unavailable = aggregateUnavailable
		return out
	}
	out.Counted, out.Excluded = b.Counted, b.Excluded
	out.Buckets = make([]bucketDTO, 0, len(b.Buckets))
	for _, x := range b.Buckets {
		out.Buckets = append(out.Buckets, bucketDTO{Key: x.Key, Count: x.Count})
	}
	return out
}

// spread projects one numeric aggregate. See breakdown - and note that min/mean/max stay
// nil on a failure as well as on an empty set, so a figure that could not be read never
// goes out as a number.
func (h *Hub) spread(name string, s store.Spread) spreadDTO {
	out := spreadDTO{
		Available: s.Err == nil,
		Covers:    s.Coverage.Set,
		Window:    s.Coverage.Window,
	}
	if s.Err != nil {
		h.log.Warn("aggregate unavailable (the rest of the snapshot still ships)", "aggregate", name, "err", s.Err)
		out.Unavailable = aggregateUnavailable
		return out
	}
	out.Counted, out.Excluded = s.Counted, s.Excluded
	out.Min, out.Mean, out.Max = s.Min, s.Mean, s.Max
	return out
}

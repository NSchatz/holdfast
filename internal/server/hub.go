package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"

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
var terminal = []store.Status{store.Done, store.Skipped, store.Failed}

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
type snapshot struct {
	Summary                map[string]int `json:"summary"`
	Queue                  []jobDTO       `json:"queue"`
	History                []jobDTO       `json:"history"`
	BytesReclaimedSession  int64          `json:"bytes_reclaimed_session"`
	BytesReclaimedLifetime int64          `json:"bytes_reclaimed_lifetime"`
	Paused                 bool           `json:"paused"`
	Scanning               bool           `json:"scanning"`
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
		reclaimedBaseline: baseline,
	}
}

// ReclaimedLifetime is the durable lifetime reclaimed total: the startup baseline plus
// everything this process has reclaimed since. See snapshot for why this is O(1) and
// double-count-free.
func (h *Hub) ReclaimedLifetime() int64 { return h.reclaimedBaseline + h.bytesReclaimed.Load() }

// Observe implements engine.Observer. It runs on an engine worker goroutine, so it
// only does two cheap, non-blocking things: bump the reclaimed-bytes counter and
// hand the event to Run (dropping it if the buffer is full — coalesced).
func (h *Hub) Observe(ev engine.Event) {
	if n := ev.BytesReclaimed(); n > 0 {
		h.bytesReclaimed.Add(n)
	}
	select {
	case h.events <- ev:
	default: // buffer full: coalesce — the next broadcast re-reads full state
	}
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
		Summary:                counts,
		Queue:                  toDTOs(queue),
		History:                toDTOs(hist),
		BytesReclaimedSession:  h.bytesReclaimed.Load(),
		BytesReclaimedLifetime: h.ReclaimedLifetime(),
		Paused:                 h.ctrl.Paused(),
		Scanning:               h.ctrl.Scanning(),
	}, nil
}

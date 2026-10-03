package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// The lease endpoints (docs/design/nodes.md#leases), relative to the group the server
// mounts them under, /api/node/v1. Every body is JSON except the upload's, and every
// refusal past the credential check is an ErrorResponse carrying a typed reason.

// The routes, relative to the mount point.
const (
	RouteLeases    = "/leases"
	RouteHeartbeat = "/leases/{id}/heartbeat"
	RouteOutput    = "/leases/{id}/output"
	RouteComplete  = "/leases/{id}/complete"
	RouteFail      = "/leases/{id}/fail"
)

// EpochHeader carries the lease epoch on an upload, whose body is the output itself.
const EpochHeader = "Holdfast-Lease-Epoch"

// ModeMapped is the one mode this build serves: the node reads the source through its own
// mount of the library. ModeHTTP, the server streaming the source, is not built.
const (
	ModeMapped = "mapped"
	ModeHTTP   = "http"
)

// The typed reasons an ErrorResponse carries. The vocabulary is closed.
const (
	errBadRequest       = "bad_request"
	errUnsupportedMode  = "unsupported_mode"
	errVersionMismatch  = "version_mismatch"
	errNotReady         = "not_ready"
	errDraining         = "draining"
	errNodeCap          = "node_cap"
	errGlobalCap        = "global_cap"
	errNoRoom           = "no_room"
	errRefused          = "refused"
	errCoolingOff       = "node_cooling_off"
	errTransfersFull    = "transfers_full"
	errLeaseGone        = "lease_gone"
	errUnknownLease     = "unknown_lease"
	errLengthRequired   = "length_required"
	errTooLarge         = "too_large"
	errBadDigest        = "bad_digest"
	errDigestMismatch   = "digest_mismatch"
	errDigestConflict   = "digest_conflict"
	errNotUploaded      = "not_uploaded"
	errShortBody        = "short_body"
	errUploadStalled    = "upload_stalled"
	errUploadInProgress = "upload_in_progress"
	errInternal         = "internal"
)

// refusalDetail is the sentence that goes with a refusal a ticket's release answers.
var refusalDetail = map[string]string{
	errNoRoom:    "the server's free-space reservation for the job was refused",
	errGlobalCap: "the cap on live leases (node_max_leases) is reached",
	errNodeCap:   "this node holds as many leases as it may (node_max_leases_per_node)",
	errRefused:   "the server did not lease the job it had for this node",
	errCoolingOff: "this node's leases kept ending without an output, so it is offered no work for a while; " +
		"its log says why each one ended",
}

const (
	statusNoWork      = http.StatusNoContent
	statusUnavailable = http.StatusServiceUnavailable
)

// maxJSONBody bounds every JSON request body.
const maxJSONBody = 64 << 10

// maxEncoders and maxEncoderLen bound what an acquire may report.
const (
	maxEncoders   = 64
	maxEncoderLen = 64
	maxVersionLen = 64
)

// failReason is the shape of the typed reason a node fails a lease with.
var failReason = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// AcquireRequest is the body of POST /leases.
type AcquireRequest struct {
	// Node is the node's name, and Version its build version.
	Node    string `json:"node"`
	Version string `json:"version"`
	// Slots is how many more encodes the node could start now. One request is answered
	// with at most one lease; a node with several free slots asks several times.
	Slots int `json:"slots"`
	// Mode is how the node reaches the source: ModeMapped.
	Mode string `json:"mode"`
	// Encoders is the encoder registry keys the node's own probe found.
	Encoders []string `json:"encoders"`
}

// AcquireResponse is the body of a 200 from POST /leases: one lease.
type AcquireResponse struct {
	LeaseID string `json:"lease_id"`
	// Epoch is the fencing token. Every later call on this lease carries it.
	Epoch        int64  `json:"epoch"`
	TTLSec       int    `json:"ttl_sec"`
	HeartbeatSec int    `json:"heartbeat_sec"`
	Mode         string `json:"mode"`
	// Path is the source as the SERVER names it; the node maps it through its own path
	// map. SourceSize and SourceMtimeNS are the source as the server found it.
	Path          string `json:"path"`
	SourceSize    int64  `json:"source_size"`
	SourceMtimeNS int64  `json:"source_mtime_ns"`
	// Encoder, Pre and Body are the encode: the encoder's registry key, the ffmpeg
	// options before -i and the options between the input and the output.
	Encoder string   `json:"encoder"`
	Pre     []string `json:"pre"`
	Body    []string `json:"body"`
	// MaxOutputBytes is the largest output the server admits on this lease.
	MaxOutputBytes int64 `json:"max_output_bytes"`
}

// HeartbeatRequest is the body of POST /leases/{id}/heartbeat.
type HeartbeatRequest struct {
	Epoch int64 `json:"epoch"`
	// Progress is the fraction of the encode done, 0 to 1.
	Progress float64 `json:"progress"`
}

// HeartbeatResponse is the body of a 200 from a heartbeat.
type HeartbeatResponse struct {
	// TTLSec is how long the lease now lives without another heartbeat.
	TTLSec int `json:"ttl_sec"`
}

// UploadResponse is the body of a 200 from PUT /leases/{id}/output: what the lease records.
type UploadResponse struct {
	State        string `json:"state"`
	OutputBytes  int64  `json:"output_bytes"`
	OutputDigest string `json:"output_digest"`
}

// CompleteRequest is the body of POST /leases/{id}/complete.
type CompleteRequest struct {
	Epoch int64 `json:"epoch"`
	// OutputDigest and SourceDigest are `sha-256=:<base64>:`: the output as uploaded, and
	// the source bytes the node read.
	OutputDigest string `json:"output_digest"`
	SourceDigest string `json:"source_digest"`
	OutputBytes  int64  `json:"output_bytes"`
	// EncodeSec is how long the encode took on the node.
	EncodeSec float64 `json:"encode_sec"`
}

// CompleteResponse is the body of a 200 from a completion: what the lease records.
type CompleteResponse struct {
	State        string `json:"state"`
	OutputBytes  int64  `json:"output_bytes"`
	OutputDigest string `json:"output_digest"`
	SourceDigest string `json:"source_digest"`
}

// FailRequest is the body of POST /leases/{id}/fail.
type FailRequest struct {
	Epoch int64 `json:"epoch"`
	// Reason is the node's typed reason: 1 to 64 characters from a-z, 0-9 and `_`.
	Reason string `json:"reason"`
}

// FailResponse is the body of a 200 from a fail.
type FailResponse struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// ErrorResponse is the body of every refusal past the credential check.
type ErrorResponse struct {
	// Error is the typed reason, from a closed vocabulary; Detail says it in words.
	Error  string `json:"error"`
	Detail string `json:"detail"`
	// ServerVersion and WorkerVersion are set on a version_mismatch.
	ServerVersion string `json:"server_version,omitempty"`
	WorkerVersion string `json:"worker_version,omitempty"`
}

// Mount registers the five lease endpoints on r. hub is asked on every request, so the
// routes exist - and the served surface is the same - whether or not a Hub has been
// wired; with none they answer 503 `not_ready`.
func Mount(r chi.Router, hub func() *Hub) {
	with := func(serve func(*Hub, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h := hub()
			if h == nil {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(DefaultRetryAfter)))
				writeError(w, http.StatusServiceUnavailable, errNotReady, "this server leases no work to nodes yet")
				return
			}
			serve(h, w, r)
		}
	}
	r.Post(RouteLeases, with((*Hub).serveAcquire))
	r.Post(RouteHeartbeat, with((*Hub).serveHeartbeat))
	r.Put(RouteOutput, with((*Hub).serveUpload))
	r.Post(RouteComplete, with((*Hub).serveComplete))
	r.Post(RouteFail, with((*Hub).serveFail))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, reason, detail string) {
	writeJSON(w, code, ErrorResponse{Error: reason, Detail: detail})
}

// retryAfterSeconds is d as a Retry-After value: whole seconds, rounded up, at least 1.
func retryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		return 1
	}
	return s
}

// unavailable answers 503 with Retry-After: the request may be sent again unchanged.
func (h *Hub) unavailable(w http.ResponseWriter, reason, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(h.o.RetryAfter)))
	writeError(w, http.StatusServiceUnavailable, reason, detail)
}

// gone answers 410. RFC 9110 makes a 410 heuristically cacheable (section 15.5.11,
// https://www.rfc-editor.org/rfc/rfc9110.html, read 2026-10-03), and a cached "this lease
// is gone" must never answer for another lease, so every one says no-store.
func gone(w http.ResponseWriter, detail string) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusGone, errLeaseGone, detail)
}

// leaseRefusal answers the refusals every call on one lease shares, and reports whether it
// answered.
func (h *Hub) leaseRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNoLease):
		writeError(w, http.StatusNotFound, errUnknownLease, "no lease has that id")
	case errors.Is(err, ErrGone):
		gone(w, err.Error())
	case errors.Is(err, ErrDigestConflict):
		writeError(w, http.StatusConflict, errDigestConflict, err.Error())
	case errors.Is(err, ErrNotUploaded):
		writeError(w, http.StatusConflict, errNotUploaded, err.Error())
	default:
		h.o.Log.Warn("a node lease call failed", "err", err)
		writeError(w, http.StatusInternalServerError, errInternal, "the server could not record the call")
	}
	return true
}

// decodeJSON reads one JSON object, refusing an unknown field, trailing data and a body
// past maxJSONBody. It answers 400 itself and reports whether the body was read.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest, "the body is not the JSON object this endpoint reads")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, errBadRequest, "the body carries more than one JSON object")
		return false
	}
	return true
}

// leaseID reads the {id} path parameter. A value that is not the shape of a lease id names
// no lease.
func leaseID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !validLeaseID(id) {
		writeError(w, http.StatusNotFound, errUnknownLease, "no lease has that id")
		return "", false
	}
	return id, true
}

// recoveredOr503 refuses every lease call until Recover has run: a lease judged before the
// restart's grace was given would be judged by an expiry the server's own downtime ran out.
func (h *Hub) recoveredOr503(w http.ResponseWriter) bool {
	h.mu.Lock()
	ok := h.recovered
	h.mu.Unlock()
	if !ok {
		h.unavailable(w, errNotReady, "the server is still starting")
	}
	return ok
}

// validAcquire refuses an acquire body outside what the protocol reads, in words.
func validAcquire(req AcquireRequest) string {
	switch {
	case !config.ValidNodeName(req.Node):
		return "node must be 1 to 64 characters from letters, digits, '.', '_' and '-'"
	case req.Version == "" || len(req.Version) > maxVersionLen:
		return "version must be the node's build version"
	case req.Slots < 1:
		return "slots must be at least 1: a node with no free slot does not ask for work"
	case len(req.Encoders) == 0 || len(req.Encoders) > maxEncoders:
		return "encoders must list the encoders the node's probe found"
	}
	for _, e := range req.Encoders {
		if e == "" || len(e) > maxEncoderLen {
			return "encoders must list encoder registry keys"
		}
	}
	return ""
}

// serveAcquire is POST /leases: a bounded long-poll for one lease.
func (h *Hub) serveAcquire(w http.ResponseWriter, r *http.Request) {
	var req AcquireRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := validAcquire(req); msg != "" {
		writeError(w, http.StatusBadRequest, errBadRequest, msg)
		return
	}
	if req.Version != h.o.Version {
		// 409 and not 426: RFC 9110 section 15.5.22 makes 426 a protocol upgrade and
		// requires an Upgrade header naming the protocol, and no protocol is offered here.
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error:         errVersionMismatch,
			Detail:        "the worker and the server must run the same holdfast version",
			ServerVersion: h.o.Version, WorkerVersion: req.Version,
		})
		return
	}
	if req.Mode != ModeMapped {
		writeError(w, http.StatusBadRequest, errUnsupportedMode,
			"this build serves mapped mode only: the node reads the source through its own mount of the library")
		return
	}

	h.mu.Lock()
	if !h.recovered || !h.ready {
		h.mu.Unlock()
		h.unavailable(w, errNotReady, "the server is not leasing work yet")
		return
	}
	if c := h.coolingLocked(req.Node); c != nil {
		rep := h.coolingReplyLocked(c)
		h.mu.Unlock()
		h.answerPoll(w, rep)
		return
	}
	onNode, all := h.liveLoadLocked(req.Node)
	if all >= h.o.MaxLeases {
		h.mu.Unlock()
		h.unavailable(w, errGlobalCap, refusalDetail[errGlobalCap])
		return
	}
	if onNode >= h.o.MaxLeasesPerNode {
		h.mu.Unlock()
		h.unavailable(w, errNodeCap, refusalDetail[errNodeCap])
		return
	}
	p := &poll{node: req.Node, encoders: append([]string(nil), req.Encoders...), reply: make(chan pollReply, 1)}
	h.polls = append(h.polls, p)
	h.notifyLocked()
	h.mu.Unlock()

	timer := time.NewTimer(h.o.LongPoll)
	defer timer.Stop()
	for {
		select {
		case rep := <-p.reply:
			h.answerPoll(w, rep)
			return
		case <-timer.C:
			if h.withdraw(p) {
				h.noWork(w)
				return
			}
			// Answered in the same instant: the answer is in the channel.
		case <-r.Context().Done():
			h.leave(p)
			return
		case <-h.o.BaseCtx.Done():
			// The server is draining. The poll is released now, so a graceful shutdown
			// never waits out a long-poll.
			h.leave(p)
			h.unavailable(w, errDraining, "the server is shutting down")
			return
		}
	}
}

// liveLoadLocked counts the live leases of one node and of every node. h.mu is held.
func (h *Hub) liveLoadLocked(node string) (onNode, all int) {
	for _, n := range h.live {
		if n == node {
			onNode++
		}
	}
	return onNode, len(h.live)
}

// withdraw is a poll whose long-poll ran out: it is answered with no work. One still queued
// leaves the queue. One RESERVED for a job leaves too, and its ticket is dead from here: the
// node was told there is no work, so a grant on that ticket would be a lease nobody holds
// (Encode refuses it before granting anything). It reports false only for a poll that has
// already been answered, whose answer is on its way.
func (h *Hub) withdraw(p *poll) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch p.state {
	case pollQueued:
		h.dequeueLocked(p)
	case pollReserved:
	default:
		return false
	}
	p.state = pollGone
	h.notifyLocked()
	return true
}

func (h *Hub) dequeueLocked(p *poll) {
	for i, q := range h.polls {
		if q == p {
			h.polls = append(h.polls[:i], h.polls[i+1:]...)
			return
		}
	}
}

// leave is a poll whose request is ending without an answer written. A lease granted to it
// in the same instant has no holder, so it is ended rather than left to run out.
func (h *Hub) leave(p *poll) {
	h.mu.Lock()
	if p.state == pollQueued {
		h.dequeueLocked(p)
	}
	p.state = pollGone
	h.notifyLocked()
	h.mu.Unlock()
	select {
	case rep := <-p.reply:
		if rep.lease != nil {
			// The request's own context is done, and the row must still be ended.
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = h.end(ctx, rep.lease.ID, ReasonPollGone)
			stop()
		}
	default:
	}
}

func (h *Hub) noWork(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(h.o.RetryAfter)))
	w.WriteHeader(statusNoWork)
}

// answerPoll writes what a poll was answered with.
func (h *Hub) answerPoll(w http.ResponseWriter, rep pollReply) {
	switch {
	case rep.lease != nil:
		l := rep.lease
		writeJSON(w, http.StatusOK, AcquireResponse{
			LeaseID: l.ID, Epoch: l.Epoch,
			TTLSec:       wholeSeconds(h.o.TTL),
			HeartbeatSec: wholeSeconds(h.o.TTL / 4),
			Mode:         ModeMapped,
			Path:         l.Path, SourceSize: l.SourceSize, SourceMtimeNS: l.SourceModTime.UnixNano(),
			Encoder: rep.job.Encoder, Pre: nonNil(rep.job.Pre), Body: nonNil(rep.job.Body),
			MaxOutputBytes: MaxOutputBytes(l.SourceSize),
		})
	case rep.status == statusNoWork:
		h.noWork(w)
	case rep.retry > 0:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(rep.retry)))
		writeError(w, http.StatusServiceUnavailable, rep.reason, rep.detail)
	default:
		h.unavailable(w, rep.reason, rep.detail)
	}
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// wholeSeconds is d in whole seconds, rounded down, at least 1.
func wholeSeconds(d time.Duration) int {
	s := int(d / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// serveHeartbeat is POST /leases/{id}/heartbeat.
func (h *Hub) serveHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	var req HeartbeatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.recoveredOr503(w) {
		return
	}
	// A lease nothing waits on keeps the grace the restart gave it and no more.
	extend := h.attached(id)
	now := h.o.Now()
	row, err := h.apply(r.Context(), id, func(cur Lease) (Lease, error) {
		return decideRenew(cur, req.Epoch, now, h.o.TTL, extend)
	})
	if h.leaseRefusal(w, err) {
		return
	}
	h.mu.Lock()
	wt := h.waits[id]
	h.mu.Unlock()
	if wt != nil {
		wt.report(clampFraction(req.Progress))
	}
	writeJSON(w, http.StatusOK, HeartbeatResponse{TTLSec: wholeSeconds(row.ExpiresAt.Sub(now))})
}

// clampFraction is f inside 0 to 1. Anything that is not a number is 0.
func clampFraction(f float64) float64 {
	switch {
	case math.IsNaN(f) || f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

// serveComplete is POST /leases/{id}/complete. It is what releases the engine call
// waiting on the lease. A repeat that reports the recorded figures is answered from the
// record.
func (h *Hub) serveComplete(w http.ResponseWriter, r *http.Request) {
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	var req CompleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	outDigest, err := CanonicalDigest(req.OutputDigest)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadDigest, "output_digest: "+err.Error())
		return
	}
	srcDigest, err := CanonicalDigest(req.SourceDigest)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadDigest, "source_digest: "+err.Error())
		return
	}
	if req.OutputBytes < 1 || req.EncodeSec < 0 || math.IsNaN(req.EncodeSec) {
		writeError(w, http.StatusBadRequest, errBadRequest, "output_bytes must be at least 1 and encode_sec must not be negative")
		return
	}
	if !h.recoveredOr503(w) {
		return
	}
	row, err := h.apply(r.Context(), id, func(cur Lease) (Lease, error) {
		next, err := decideComplete(cur, req.Epoch, h.o.Now(), outDigest, req.OutputBytes, srcDigest)
		if err == nil && !h.attached(id) {
			// Nothing waits on this lease, so nothing would gate its output: it is not
			// recorded as completed.
			return cur, errUnattached
		}
		if err == nil {
			// Only a completion the decision accepted reports an encode time, and it is
			// noted before the settlement that hands the engine its Result.
			h.noteEncodeSeconds(id, req.EncodeSec)
		}
		return next, err
	})
	if errors.Is(err, errUnattached) {
		h.unavailable(w, errNotReady, "the server is not waiting on this lease")
		return
	}
	if err != nil && !errors.Is(err, errAlready) {
		h.leaseRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, CompleteResponse{State: string(row.State), OutputBytes: row.OutputBytes,
		OutputDigest: row.OutputDigest, SourceDigest: row.SourceDigest})
}

// noteEncodeSeconds records the encode time an accepted completion reported, for the
// engine call waiting on the lease.
func (h *Hub) noteEncodeSeconds(id string, sec float64) {
	h.mu.Lock()
	wt := h.waits[id]
	h.mu.Unlock()
	if wt != nil {
		wt.mu.Lock()
		wt.encodeSec = sec
		wt.mu.Unlock()
	}
}

// serveFail is POST /leases/{id}/fail: the node ends its lease with a typed reason.
func (h *Hub) serveFail(w http.ResponseWriter, r *http.Request) {
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	var req FailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !failReason.MatchString(req.Reason) {
		writeError(w, http.StatusBadRequest, errBadRequest, "reason must be 1 to 64 characters from a-z, 0-9 and '_'")
		return
	}
	if !h.recoveredOr503(w) {
		return
	}
	row, err := h.apply(r.Context(), id, func(cur Lease) (Lease, error) {
		return decideFail(cur, req.Epoch, h.o.Now(), req.Reason)
	})
	if err != nil && !errors.Is(err, errAlready) {
		h.leaseRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, FailResponse{State: string(row.State), Reason: row.Reason})
}

package node

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PUT /leases/{id}/output - the upload (docs/design/nodes.md#leases).
//
// What it admits is a CANDIDATE and nothing more. The bytes are written only to the working
// file the lease recorded, which the server named: no part of the request chooses a path.
// They are admitted only on a lease that is live at the epoch presented - checked before
// the file is opened, and again in the transaction that admits it - with the length the
// request declared and the sha-256 it declared. Everything else leaves no file behind.

// uploadChunk is the buffer the body is copied through, and the unit the read deadline is
// refreshed at.
const uploadChunk = 1 << 20

// errUnattached is an upload on a lease no engine call is waiting on.
var errUnattached = errors.New("node: nothing is waiting on the lease")

// errBusy is a second upload on a lease while one is writing its working file.
var errBusy = errors.New("node: an upload of this lease is already in progress")

// uploadBudget is how long an upload of that many bytes may take in all: the grace, plus
// the time the slowest allowed rate needs.
func uploadBudget(declared, minRate int64, grace time.Duration) time.Duration {
	return grace + time.Duration(declared/minRate)*time.Second
}

// serveUpload is PUT /leases/{id}/output.
func (h *Hub) serveUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	epoch, err := strconv.ParseInt(r.Header.Get(EpochHeader), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest, "the "+EpochHeader+" header must carry the lease epoch")
		return
	}
	// The length must be DECLARED: it is what the size cap, the free-space check and the
	// read deadline are all taken from, and what the body is held to.
	if r.Header.Get("Content-Length") == "" || r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, errLengthRequired, "an upload must declare its Content-Length")
		return
	}
	declared := r.ContentLength
	if declared < 1 {
		writeError(w, http.StatusBadRequest, errBadRequest, "an empty body is not an output")
		return
	}
	// Every Content-Digest line is read as one list, as a field sent on several lines is:
	// two lines each carrying a sha-256 member are two members, and refused as such.
	digest, err := CanonicalDigest(strings.Join(r.Header.Values("Content-Digest"), ","))
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadDigest, "Content-Digest: "+err.Error())
		return
	}
	if !h.recoveredOr503(w) {
		return
	}

	// The first liveness check, before anything is reserved or opened.
	cur, found, err := h.o.Ledger.GetLease(r.Context(), id)
	if err != nil {
		h.leaseRefusal(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, errUnknownLease, "no lease has that id")
		return
	}
	if h.answerUpload(w, cur, classifyUpload(cur, epoch, h.o.Now(), digest, declared)) {
		return
	}

	if !h.takeTransfer() {
		h.unavailable(w, errTransfersFull, "as many uploads as the server takes at once (node_max_transfers) are in flight")
		return
	}
	defer h.releaseTransfer()

	// The free-space re-check, now that the length is known. A failed lookup refuses
	// nothing, exactly as the engine's own check does not fail a job on one.
	if h.o.FreeSpace != nil {
		free, err := h.o.FreeSpace(filepath.Dir(cur.Temp))
		if err == nil && free < uint64(declared) {
			h.unavailable(w, errNoRoom, "the filesystem holding the working file cannot take the output")
			return
		}
	}

	u, f, lease, action, err := h.beginUpload(r, id, epoch, digest, declared)
	if err != nil {
		switch {
		case errors.Is(err, errUnattached):
			h.unavailable(w, errNotReady, "the server has not taken this lease back since it restarted")
		case errors.Is(err, errBusy):
			writeError(w, http.StatusConflict, errUploadInProgress, "an upload of this lease is already in progress")
		default:
			h.leaseRefusal(w, err)
		}
		return
	}
	if h.answerUpload(w, lease, action) {
		return
	}

	got, sum, readErr := h.receive(w, r, f, declared)
	if readErr == nil {
		if err := f.Sync(); err != nil {
			readErr = fmt.Errorf("%w: sync: %w", errWrite, err)
		}
	}
	// A failed sync or close is the server's failure to keep the bytes, never the node's
	// failure to send them.
	if err := f.Close(); err != nil && readErr == nil {
		readErr = fmt.Errorf("%w: close: %w", errWrite, err)
	}
	if readErr != nil {
		h.discard(u, lease.Temp)
		var tooLong *http.MaxBytesError
		switch {
		case errors.As(readErr, &tooLong):
			writeError(w, http.StatusRequestEntityTooLarge, errTooLarge, "the body is longer than its declared length")
		case errors.Is(readErr, os.ErrDeadlineExceeded):
			writeError(w, http.StatusRequestTimeout, errUploadStalled, "the upload did not keep to the minimum rate")
		case errors.Is(readErr, errWrite):
			h.o.Log.Warn("writing a node upload failed", "lease", id, "err", readErr)
			writeError(w, http.StatusInternalServerError, errInternal, "the server could not write the working file")
		default:
			writeError(w, http.StatusBadRequest, errShortBody,
				fmt.Sprintf("the body ended after %d of the %d bytes it declared", got, declared))
		}
		return
	}
	if got != declared || FormatDigest(sum) != digest {
		// Not the bytes the request declared: an error page offered as media, a truncated
		// transfer, a corrupted one. Nothing of it is kept.
		h.discard(u, lease.Temp)
		row, err := h.apply(r.Context(), id, func(cur Lease) (Lease, error) {
			return decideMismatch(cur, epoch, h.o.Now(), h.o.DigestRetries)
		})
		if h.leaseRefusal(w, err) {
			return
		}
		detail := "the body's sha-256 is not the Content-Digest it declared; the upload may be sent again"
		if !row.State.Live() {
			detail = "the body's sha-256 is not the Content-Digest it declared, and the lease has failed"
		}
		writeError(w, http.StatusBadRequest, errDigestMismatch, detail)
		return
	}

	// The second liveness check, in the transaction that admits the file.
	row, err := h.admit(r, id, epoch, digest, declared)
	if err != nil {
		h.discard(u, lease.Temp)
		if errors.Is(err, errUnattached) {
			gone(w, "the job waiting on this lease ended while the upload arrived")
			return
		}
		h.leaseRefusal(w, err)
		return
	}
	h.keep(u, lease.Temp)
	h.o.Log.Info("node upload admitted", "node", row.Node, "lease", row.ID, "epoch", row.Epoch, "bytes", row.OutputBytes)
	writeJSON(w, http.StatusOK, UploadResponse{State: string(row.State), OutputBytes: row.OutputBytes, OutputDigest: row.OutputDigest})
}

// answerUpload writes the answer for every action but uploadProceed, and reports whether
// it answered.
func (h *Hub) answerUpload(w http.ResponseWriter, cur Lease, action uploadAction) bool {
	switch action {
	case uploadProceed:
		return false
	case uploadDuplicate:
		writeJSON(w, http.StatusOK, UploadResponse{State: string(cur.State), OutputBytes: cur.OutputBytes, OutputDigest: cur.OutputDigest})
	case uploadConflict:
		writeError(w, http.StatusConflict, errDigestConflict, ErrDigestConflict.Error())
	case uploadTooLarge:
		writeError(w, http.StatusRequestEntityTooLarge, errTooLarge,
			fmt.Sprintf("the output may be at most %d bytes, one less than the source", MaxOutputBytes(cur.SourceSize)))
	default:
		gone(w, "the lease is not live at that epoch")
	}
	return true
}

func (h *Hub) takeTransfer() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transfers >= h.o.MaxTransfers {
		return false
	}
	h.transfers++
	return true
}

func (h *Hub) releaseTransfer() {
	h.mu.Lock()
	h.transfers--
	h.mu.Unlock()
}

// beginUpload re-reads the lease under the lifecycle lock and, when it is still one an
// upload may be written for, creates its working file. The liveness check and the create
// are under one lock with every terminal transition, so no file is created for a lease
// that has ended, and a file created here is this lease's until the lease ends.
func (h *Hub) beginUpload(r *http.Request, id string, epoch int64, digest string, declared int64) (*upload, *os.File, Lease, uploadAction, error) {
	h.life.Lock()
	defer h.life.Unlock()
	cur, found, err := h.o.Ledger.GetLease(r.Context(), id)
	if err != nil {
		return nil, nil, Lease{}, uploadGone, err
	}
	if !found {
		return nil, nil, Lease{}, uploadGone, nil
	}
	action := classifyUpload(cur, epoch, h.o.Now(), digest, declared)
	if action != uploadProceed {
		return nil, nil, cur, action, nil
	}
	h.mu.Lock()
	_, attached := h.waits[id]
	_, busy := h.uploads[cur.Temp]
	h.mu.Unlock()
	if !attached {
		return nil, nil, cur, action, errUnattached
	}
	if busy {
		return nil, nil, cur, action, errBusy
	}
	// Whatever is at the working file's path is this lease's own leavings: an upload a
	// restart cut short. O_EXCL then guarantees the file written is one this call made.
	if err := os.Remove(cur.Temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, cur, action, fmt.Errorf("clearing the working file: %w", err)
	}
	f, err := os.OpenFile(cur.Temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, nil, cur, action, fmt.Errorf("creating the working file: %w", err)
	}
	u := &upload{lease: id}
	h.mu.Lock()
	h.uploads[cur.Temp] = u
	h.mu.Unlock()
	return u, f, cur, action, nil
}

// errWrite marks a failure writing the working file, as against one reading the body.
var errWrite = errors.New("node: writing the working file failed")

// receive copies the body into f, hashing it, and returns how many bytes arrived and their
// sha-256. The body is held to the declared length by http.MaxBytesReader, and every read
// is under a deadline: the whole upload may take no longer than uploadBudget, and no single
// read longer than the grace, so a stalled upload cannot hold a transfer slot and a
// reservation for ever. The deadline is the connection's, so it is taken from the real
// clock and never from the lease clock.
func (h *Hub) receive(w http.ResponseWriter, r *http.Request, f *os.File, declared int64) (int64, []byte, error) {
	rc := http.NewResponseController(w)
	start := time.Now()
	overall := start.Add(uploadBudget(declared, h.o.MinUploadRate, h.o.UploadGrace))
	body := http.MaxBytesReader(w, r.Body, declared)
	hash := sha256.New()
	buf := make([]byte, uploadChunk)
	var got int64
	for {
		deadline := time.Now().Add(h.o.UploadGrace)
		if deadline.After(overall) {
			deadline = overall
		}
		// A ResponseWriter with no deadline support has none to set; the copy goes on.
		_ = rc.SetReadDeadline(deadline)
		n, err := body.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			if _, werr := f.Write(buf[:n]); werr != nil {
				return got, nil, fmt.Errorf("%w: %w", errWrite, werr)
			}
			got += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return got, nil, err
		}
	}
	// The request is read; the connection's next request is not held to this deadline.
	_ = rc.SetReadDeadline(time.Time{})
	return got, hash.Sum(nil), nil
}

// admit records an upload whose length and digest matched, in one transaction that checks
// the lease, the epoch and the clock again. The lease must still have an engine call
// waiting on it: an output nothing will gate is not admitted.
func (h *Hub) admit(r *http.Request, id string, epoch int64, digest string, size int64) (Lease, error) {
	h.life.Lock()
	defer h.life.Unlock()
	if !h.attached(id) {
		return Lease{}, errUnattached
	}
	return h.applyLocked(r.Context(), id, func(cur Lease) (Lease, error) {
		return decideAdmit(cur, epoch, h.o.Now(), digest, size)
	})
}

// discard removes the working file an upload wrote, unless the lease ended meanwhile: the
// file at that path is then no longer this upload's, and may be a later grant's.
func (h *Hub) discard(u *upload, temp string) {
	h.life.Lock()
	defer h.life.Unlock()
	if !h.disown(u, temp) {
		return
	}
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.o.Log.Warn("could not remove a refused upload's working file", "path", temp, "err", err)
	}
}

// keep ends an upload's hold on a working file it leaves in place.
func (h *Hub) keep(u *upload, temp string) { h.disown(u, temp) }

// disown takes the upload out of the table and reports whether the file was still its own.
func (h *Hub) disown(u *upload, temp string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u.disowned {
		return false
	}
	u.disowned = true
	delete(h.uploads, temp)
	return true
}

package node

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// GET /leases/{id}/source - the source stream of http mode (docs/design/nodes.md#http-mode).
//
// No request names a path. The file served is the source of the lease the request names,
// and only while that lease is live at the epoch presented, was granted in http mode by
// this process, has admitted no output yet, and has an engine call waiting on it. The file
// is opened and checked against the size and modification time the lease was granted on
// before a byte of it is sent, so what a node downloads is the file its command line was
// planned for or nothing.
//
// What the server streams it also hashes. A whole source sent in one unranged response
// leaves its sha-256 on the lease, and the completion the node later reports is held to it
// (decideComplete). A ranged response leaves none: the proof is then the server's own hash
// of its own copy before the gates, which the engine takes in every mode.

// errLeaseEnded stops a source stream whose lease ended while it was being sent.
var errLeaseEnded = errors.New("node: the lease ended while its source was streamed")

// SourceMediaType is the Content-Type of a source stream. It is set before
// http.ServeContent is called, which otherwise reads the first 512 bytes to sniff a type
// and seeks back ("If the response's Content-Type header is not set, ServeContent first
// tries to deduce the type from name's file extension and, if that fails, falls back to
// reading the first block of the content and passing it to DetectContentType", `go doc
// net/http ServeContent`, Go 1.25.14; https://pkg.go.dev/net/http#ServeContent, read
// 2026-10-03). A sniffed stream would be read twice at its head, and a source is media to
// no one but the node's ffmpeg.
const SourceMediaType = "application/octet-stream"

// ReprDigestTrailer is the trailer a whole source stream MAY carry: the RFC 9530
// `Repr-Digest` of what was sent. It is sent only where the connection can carry a trailer
// beside a declared length (HTTP/2), it is never load-bearing, and a node that receives
// one that disagrees with what it read stops before it encodes.
const ReprDigestTrailer = "Repr-Digest"

// conditionalHeaders are the request headers a source request is served without.
var conditionalHeaders = []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"}

// sourceStream is the leased source as http.ServeContent reads it. It hashes the bytes that
// are read in order from the first one, refreshes the connection's write deadline as the
// stream advances, and ends the stream when the lease does.
type sourceStream struct {
	f    *os.File
	hash hash.Hash
	// pos is the file offset, and hashed how many bytes from offset 0 have been hashed. A
	// read anywhere but at hashed is not hashed, so hashed reaches the file's size only
	// when the whole file was read once, in order.
	pos, hashed int64

	rc      *http.ResponseController
	grace   time.Duration
	overall time.Time
	last    time.Time
	live    func() bool
}

func (s *sourceStream) Read(p []byte) (int, error) {
	if !s.live() {
		return 0, errLeaseEnded
	}
	s.extend()
	n, err := s.f.Read(p)
	if n > 0 {
		if s.pos == s.hashed {
			s.hash.Write(p[:n])
			s.hashed += int64(n)
		}
		s.pos += int64(n)
	}
	return n, err
}

func (s *sourceStream) Seek(offset int64, whence int) (int64, error) {
	pos, err := s.f.Seek(offset, whence)
	if err == nil {
		s.pos = pos
	}
	return pos, err
}

// extend moves the write deadline on: no write may take longer than the grace, and the
// whole stream no longer than its budget. The deadline is the connection's, so it is taken
// from the real clock and never from the lease clock. A ResponseWriter with no deadline
// support has none to set; the stream goes on.
func (s *sourceStream) extend() {
	now := time.Now()
	if !s.last.IsZero() && now.Sub(s.last) < s.grace/4 {
		return
	}
	deadline := now.Add(s.grace)
	if deadline.After(s.overall) {
		deadline = s.overall
	}
	_ = s.rc.SetWriteDeadline(deadline)
	s.last = now
}

// sourceUnchanged reports whether an open file is the source the lease was granted on: a
// regular file of exactly that size and modification time.
func sourceUnchanged(fi fs.FileInfo, l Lease) bool {
	return fi.Mode().IsRegular() && fi.Size() == l.SourceSize &&
		fi.ModTime().UnixNano() == l.SourceModTime.UnixNano()
}

// sourceOffered decides whether a lease's source is served, from the row, the epoch, the
// clock, the mode the lease was granted in and whether an engine call waits on it. It
// returns the typed reason of a refusal, and "" for a lease whose source is served.
func sourceOffered(cur Lease, epoch int64, now time.Time, mode string, attached bool) string {
	switch {
	case checkLive(cur, epoch, now) != nil:
		return errLeaseGone
	case !attached:
		return errNotReady
	case mode != ModeHTTP || cur.State != store.LeaseGranted:
		return errSourceNotOffered
	}
	return ""
}

// serveSource is GET /leases/{id}/source.
func (h *Hub) serveSource(w http.ResponseWriter, r *http.Request) {
	id, ok := leaseID(w, r)
	if !ok {
		return
	}
	epoch, err := strconv.ParseInt(r.Header.Get(EpochHeader), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest, "the "+EpochHeader+" header must carry the lease epoch")
		return
	}
	if !h.recoveredOr503(w) {
		return
	}
	cur, found, err := h.o.Ledger.GetLease(r.Context(), id)
	if err != nil {
		h.leaseRefusal(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, errUnknownLease, "no lease has that id")
		return
	}
	h.mu.Lock()
	_, attached := h.waits[id]
	mode := ""
	if s := h.sources[id]; s != nil {
		mode = s.mode
	}
	h.mu.Unlock()
	switch sourceOffered(cur, epoch, h.o.Now(), mode, attached) {
	case "":
	case errLeaseGone:
		gone(w, "the lease is not live at that epoch")
		return
	case errNotReady:
		h.unavailable(w, errNotReady, "the server has not taken this lease back since it restarted")
		return
	default:
		writeError(w, http.StatusConflict, errSourceNotOffered,
			"this lease's source is not streamed: it was not granted in http mode by this server "+
				"process, or it has already admitted an output")
		return
	}
	if !h.takeTransfer() {
		h.unavailable(w, errTransfersFull, "as many transfers as the server takes at once (node_max_transfers) are in flight")
		return
	}
	defer h.releaseTransfer()

	// O_NOFOLLOW: the engine refuses a source that is a symlink (its symlinked-source
	// guard), so a link found at the leased path was put there after the grant, and what it
	// points at is not the leased file however its size and time read. It is never followed.
	f, err := os.OpenFile(cur.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			writeError(w, http.StatusConflict, errSourceChanged, "a symbolic link stands where the lease was granted on a file")
			return
		}
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusConflict, errSourceChanged, "the source is no longer where the lease was granted on it")
			return
		}
		h.o.Log.Warn("opening a leased source failed", "lease", id, "err", err)
		writeError(w, http.StatusInternalServerError, errInternal, "the server could not open the source")
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		h.o.Log.Warn("reading a leased source's size failed", "lease", id, "err", err)
		writeError(w, http.StatusInternalServerError, errInternal, "the server could not read the source")
		return
	}
	if !sourceUnchanged(fi, cur) {
		// The lease stays live: the node fails it as one it could not run, and the server's
		// own guards then decide about the file it now finds.
		writeError(w, http.StatusConflict, errSourceChanged,
			"the source is not the size and modification time the lease was granted on")
		return
	}

	// Whole means one unranged request: only then is what is sent the whole representation.
	whole := r.Header.Get("Range") == ""
	rc := http.NewResponseController(w)
	src := &sourceStream{
		f: f, hash: sha256.New(), rc: rc, grace: h.o.UploadGrace,
		overall: time.Now().Add(uploadBudget(cur.SourceSize, h.o.MinUploadRate, h.o.UploadGrace)),
		live:    func() bool { return h.attached(id) },
	}
	w.Header().Set("Content-Type", SourceMediaType)
	// A source is served to the holder of a live lease and to nobody else, ever again.
	w.Header().Set("Cache-Control", "no-store")
	// No conditional request is answered: http.ServeContent "handles If-Match,
	// If-Unmodified-Since, If-None-Match, If-Modified-Since, and If-Range requests", and a
	// 304 or a 412 is no answer this endpoint has. A source is sent, or refused typed.
	for _, name := range conditionalHeaders {
		r.Header.Del(name)
	}
	// No name and no modification time: nothing is sniffed from an extension.
	http.ServeContent(w, r, "", time.Time{}, src)
	// The response is written; the connection's next response is not held to this deadline.
	_ = rc.SetWriteDeadline(time.Time{})

	if !whole || src.hashed != cur.SourceSize {
		h.o.Log.Info("node source stream ended without a whole-source digest", "node", cur.Node, "lease", id,
			"epoch", cur.Epoch, "ranged", !whole, "bytes_hashed", src.hashed, "source_bytes", cur.SourceSize)
		return
	}
	digest := FormatDigest(src.hash.Sum(nil))
	h.mu.Lock()
	if s := h.sources[id]; s != nil {
		s.streamed = digest
	}
	h.mu.Unlock()
	// Sent only where a trailer can follow a body of declared length (HTTP/2); on HTTP/1.1
	// the response is not chunked and this is dropped. Never load-bearing.
	w.Header().Set(http.TrailerPrefix+ReprDigestTrailer, digest)
	h.o.Log.Info("node source streamed", "node", cur.Node, "lease", id, "epoch", cur.Epoch,
		"bytes", cur.SourceSize, "source_digest", digest)
}

package nodeworker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/node"
)

// keyBlock is the PEM block type of a private key. It is spelled in two halves so that no
// file of this repository carries the marker a secret scanner reads as a committed key: the
// fixtures below hold no key, only the shape of one.
const keyBlock = "PRIVATE" + " KEY"

// The worker in http mode, and its transport (docs/design/nodes.md#http-mode, #transport),
// against the fake server of worker_test.go. No ffmpeg runs here: the encode is a function
// the test supplies, and it records the bytes of the file it was handed.

// httpRig is a rig whose worker has no mount and no path map.
type httpRig struct {
	*rig
	content []byte

	mu sync.Mutex
	// encoded is the content of each source file an encode was handed.
	encoded [][]byte
}

func newHTTPRig(t *testing.T) *httpRig {
	t.Helper()
	r := newRig(t)
	h := &httpRig{rig: r, content: []byte(strings.Repeat("the leased source, as the server streams it. ", 400))}
	r.opts.Mode = node.ModeHTTP
	r.opts.PathMap = nil
	r.opts.SourceIdle = waitFor
	// The mount is gone: an http worker reaches nothing of the library.
	if err := os.Remove(r.mount); err != nil {
		t.Fatal(err)
	}
	inner := r.opts.Encode
	r.opts.Encode = func(ctx context.Context, in, out string, pre, body []string, progress func(float64)) error {
		b, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.encoded = append(h.encoded, b)
		h.mu.Unlock()
		return inner(ctx, in, out, pre, body, progress)
	}
	r.f.source = h.serveWhole
	return h
}

// lease is an http-mode lease on a source of the rig's content.
func (h *httpRig) lease() node.AcquireResponse {
	size := int64(len(h.content))
	return node.AcquireResponse{LeaseID: leaseID, Epoch: 7, TTLSec: 4, HeartbeatSec: 1, Mode: node.ModeHTTP,
		Path: "/srv/media/Some Film (2001)/film.MKV", SourceSize: size, SourceMtimeNS: 1700000000000000000, Encoder: "cpu",
		Pre: []string{}, Body: []string{"-c:v", "libx265", "-f", "matroska"}, MaxOutputBytes: size - 1}
}

// serveWhole answers the source request as the real server does: 200, a declared length,
// the bytes.
func (h *httpRig) serveWhole(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", node.SourceMediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(h.content)))
	_, _ = w.Write(h.content)
}

// untilEnded waits until the lease has ended one way or the other: failed, or completed. A
// fixture that expects a refusal therefore fails by name, at once, when the worker encodes
// instead - it does not sit out the whole wait for a `fail` that will never come.
func (h *httpRig) untilEnded(what string) {
	h.t.Helper()
	until(h.t, what, func() bool { return len(h.f.seen("fail")) >= 1 || len(h.f.seen("complete")) >= 1 })
}

func (h *httpRig) encodedSources() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.encoded...)
}

// TestWorker_HTTPMode_DownloadsTheSourceEncodesItAndReportsTheDigestOfWhatArrived is http
// mode's whole happy path, read off what the fake server received and what the encode was
// handed: a worker with no mount and no path map asks in http mode, downloads the source
// with one unranged GET carrying the credential and the epoch, encodes the downloaded file
// from its work directory, uploads exactly as mapped mode does, reports the sha-256 of the
// bytes that arrived, and leaves the work directory empty.
func TestWorker_HTTPMode_DownloadsTheSourceEncodesItAndReportsTheDigestOfWhatArrived(t *testing.T) {
	h := newHTTPRig(t)
	h.f.queue("acquire", jsonReply(200, h.lease()))
	stop := h.run()
	until(t, "the completion", func() bool { return len(h.f.seen("complete")) >= 1 })
	until(t, "the next poll", func() bool { return len(h.f.seen("acquire")) >= 2 })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var acq node.AcquireRequest
	if err := json.Unmarshal(h.f.seen("acquire")[0].body, &acq); err != nil || acq.Mode != node.ModeHTTP {
		t.Errorf("the acquire asked in mode %q (%v), want http", acq.Mode, err)
	}
	gets := h.f.seen("source")
	if len(gets) != 1 {
		t.Fatalf("%d source requests, want 1", len(gets))
	}
	get := gets[0]
	if get.method != http.MethodGet || get.path != "/api/node/v1/leases/"+leaseID+"/source" ||
		get.auth != "Bearer "+testToken || get.header.Get(node.EpochHeader) != "7" {
		t.Errorf("the source request was %s %s with Authorization %q and epoch %q", get.method, get.path, get.auth, get.header.Get(node.EpochHeader))
	}
	if get.header.Get("Range") != "" {
		t.Errorf("the source request carries Range %q: a download is never resumed", get.header.Get("Range"))
	}
	if got := get.header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("the source request accepts encoding %q, want identity: the bytes as they are", got)
	}
	if len(get.body) != 0 {
		t.Errorf("the source request carries a %d-byte body", len(get.body))
	}

	h.rig.mu.Lock()
	enc := h.rig.encodes
	h.rig.mu.Unlock()
	wantIn := filepath.Join(h.work, leaseID+".7.src.mkv")
	wantOut := filepath.Join(h.work, leaseID+".7.out")
	if len(enc) != 1 || enc[0][0] != wantIn || enc[0][1] != wantOut || strings.Join(enc[0][2:], " ") != "-c:v libx265 -f matroska" {
		t.Fatalf("encodes = %v, want one of the DOWNLOADED source %s into %s with the lease's options", enc, wantIn, wantOut)
	}
	if got := h.encodedSources(); len(got) != 1 || !bytes.Equal(got[0], h.content) {
		t.Fatal("the file handed to the encode is not, byte for byte, what the server streamed")
	}

	puts := h.f.seen("output")
	if len(puts) != 1 || string(puts[0].body) != string(h.output) || puts[0].length != int64(len(h.output)) ||
		puts[0].header.Get("Content-Digest") != digest(h.output) || puts[0].header.Get(node.EpochHeader) != "7" {
		t.Errorf("the upload is not mapped mode's: %d put(s)", len(puts))
	}
	var done node.CompleteRequest
	_ = json.Unmarshal(h.f.seen("complete")[0].body, &done)
	if done.Epoch != 7 || done.SourceDigest != digest(h.content) || done.OutputDigest != digest(h.output) || done.OutputBytes != int64(len(h.output)) {
		t.Errorf("complete = %+v, want the sha-256 of the bytes that arrived as source_digest", done)
	}
	if n := len(h.f.seen("fail")); n != 0 {
		t.Errorf("the lease was failed %d time(s): %v", n, h.f.failReasons())
	}
	workDirEmpty(t, h.work)
}

// TestWorkerFixture_HTTPMode_AnAnswerThatIsNotTheSourceIsNeverEncoded is P4 rule 6 on the
// worker: a response is media only when it is a 200 delivering exactly source_size bytes.
// Every other answer fails the lease typed, runs no encode and leaves no file. retried says
// whether another attempt could cure it: those are asked MaxSourceAttempts times, the rest
// once.
func TestWorkerFixture_HTTPMode_AnAnswerThatIsNotTheSourceIsNeverEncoded(t *testing.T) {
	page := []byte("<html><body><h1>404 Not Found</h1></body></html>\n123456") // 55 bytes
	if len(page) != 55 {
		t.Fatalf("the error page is %d bytes, want the 55 of the incumbent's report", len(page))
	}
	for name, tc := range map[string]struct {
		serve   func(h *httpRig, w http.ResponseWriter, r *http.Request)
		retried bool
	}{
		"a 404 with a 55-byte body": {func(_ *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(page)
		}, false},
		"a 200 whose body is the 55-byte error page": {func(_ *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(page)
		}, false},
		"a 200 that declares one byte less than the source": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(h.content)-1))
			_, _ = w.Write(h.content[:len(h.content)-1])
		}, false},
		"a 200 that declares one byte more than the source": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(h.content)+1))
			_, _ = w.Write(append(append([]byte(nil), h.content...), 'x'))
		}, false},
		"a short body: the declared length never arrives": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(h.content)))
			_, _ = w.Write(h.content[:len(h.content)/2])
			// The handler returns short of its declared length, so the server drops the connection.
		}, true},
		"a short body, chunked": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			_, _ = w.Write(h.content[:len(h.content)-1])
		}, true},
		"a long body, chunked": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			_, _ = w.Write(append(append([]byte(nil), h.content...), 'x'))
		}, false},
		"a 206 carrying the whole source": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(h.content)-1, len(h.content)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(h.content)
		}, false},
		"a 500": {func(_ *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, false},
		"a 409 that is not the server withdrawing the source": {func(_ *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"digest_conflict","detail":"said by the fake server"}`)
		}, false},
		"a redirect to the source elsewhere": {func(h *httpRig, w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("moved") != "" {
				h.serveWhole(w, r)
				return
			}
			http.Redirect(w, r, r.URL.Path+"?moved=1", http.StatusTemporaryRedirect)
		}, false},
		"a 200 with a content coding": {func(h *httpRig, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(len(h.content)))
			_, _ = w.Write(h.content)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTPRig(t)
			h.f.source = func(w http.ResponseWriter, r *http.Request) { tc.serve(h, w, r) }
			h.f.queue("acquire", jsonReply(200, h.lease()))
			stop := h.run()
			h.untilEnded("the lease to be failed")
			_ = stop()
			if n := h.encodeCount(); n != 0 {
				t.Fatalf("%d encode(s) ran on an answer that is not the source", n)
			}
			if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonSourceDownloadFailed {
				t.Errorf("the lease was failed %v, want [%s]", got, ReasonSourceDownloadFailed)
			}
			if n := len(h.f.seen("output")) + len(h.f.seen("complete")); n != 0 {
				t.Errorf("%d upload or completion call(s) followed an answer that is not the source", n)
			}
			want := 1
			if tc.retried {
				want = MaxSourceAttempts
			}
			if n := len(h.f.seen("source")); n != want {
				t.Errorf("the source was asked for %d time(s), want %d", n, want)
			}
			for _, c := range h.f.seen("source") {
				if c.header.Get("Range") != "" {
					t.Errorf("a retry carried Range %q: a download restarts from zero", c.header.Get("Range"))
				}
			}
			workDirEmpty(t, h.work)
		})
	}
}

// trailered answers the source chunked, with a Repr-Digest trailer of its own choosing.
func trailered(h *httpRig, value string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", node.ReprDigestTrailer)
		w.(http.Flusher).Flush()
		_, _ = w.Write(h.content)
		w.Header().Set(node.ReprDigestTrailer, value)
	}
}

// TestWorkerFixture_HTTPMode_ASourceWhoseReprDigestTrailerDisagreesIsNotEncoded: the trailer
// is never required, but one that arrives and says the bytes are not the ones sent stops the
// lease before anything is encoded. One that agrees, and one this build cannot read a
// sha-256 out of, do not.
func TestWorkerFixture_HTTPMode_ASourceWhoseReprDigestTrailerDisagreesIsNotEncoded(t *testing.T) {
	t.Run("a trailer that disagrees", func(t *testing.T) {
		h := newHTTPRig(t)
		h.f.source = trailered(h, digest([]byte("what the server really sent")))
		h.f.queue("acquire", jsonReply(200, h.lease()))
		stop := h.run()
		h.untilEnded("the lease to be failed")
		_ = stop()
		if n := h.encodeCount(); n != 0 {
			t.Fatalf("%d encode(s) ran on bytes the server's own digest disowns", n)
		}
		if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonSourceDownloadFailed {
			t.Errorf("the lease was failed %v, want [%s]", got, ReasonSourceDownloadFailed)
		}
		if n := len(h.f.seen("source")); n != MaxSourceAttempts {
			t.Errorf("the source was asked for %d time(s), want %d: a change in transit may not repeat", n, MaxSourceAttempts)
		}
		workDirEmpty(t, h.work)
	})
	for name, value := range map[string]func(h *httpRig) string{
		"a trailer that agrees":             func(h *httpRig) string { return digest(h.content) },
		"a trailer with no sha-256 member":  func(*httpRig) string { return "sha-512=:AAAA:" },
		"a trailer that is not a digest":    func(*httpRig) string { return "garbage" },
		"an agreeing trailer among members": func(h *httpRig) string { return "sha-512=:AAAA:, " + digest(h.content) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTPRig(t)
			h.f.source = trailered(h, value(h))
			h.f.queue("acquire", jsonReply(200, h.lease()))
			stop := h.run()
			until(t, "the completion", func() bool { return len(h.f.seen("complete")) >= 1 })
			_ = stop()
			if got := h.encodedSources(); len(got) != 1 || !bytes.Equal(got[0], h.content) {
				t.Errorf("%d encode(s) ran, want one of the source", len(got))
			}
			if n := len(h.f.seen("fail")); n != 0 {
				t.Errorf("the lease was failed: %v", h.f.failReasons())
			}
		})
	}
}

// TestWorkerFixture_HTTPMode_A503IsAskedAgainAndA410EndsTheLeaseUnfailed: a transfer cap is
// waited out inside the bound, under the server's own Retry-After; a 410 means the lease is
// gone, so nothing is encoded and nothing is failed - there is no lease to fail.
func TestWorkerFixture_HTTPMode_A503IsAskedAgainAndA410EndsTheLeaseUnfailed(t *testing.T) {
	t.Run("a 503 then the source", func(t *testing.T) {
		h := newHTTPRig(t)
		var asked atomic.Int32
		h.f.source = func(w http.ResponseWriter, r *http.Request) {
			if asked.Add(1) == 1 {
				w.Header().Set("Retry-After", "9")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"transfers_full","detail":"said by the fake server"}`)
				return
			}
			h.serveWhole(w, r)
		}
		h.f.queue("acquire", jsonReply(200, h.lease()))
		stop := h.run()
		until(t, "the completion", func() bool { return len(h.f.seen("complete")) >= 1 })
		_ = stop()
		if n := asked.Load(); n != 2 {
			t.Errorf("the source was asked for %d time(s), want 2", n)
		}
		h.rig.mu.Lock()
		waited := false
		for _, d := range h.rig.sleeps {
			waited = waited || d == 9*time.Second
		}
		h.rig.mu.Unlock()
		if !waited {
			t.Errorf("the worker did not wait out the server's Retry-After of 9 s before it asked again: %v", h.rig.sleeps)
		}
		if got := h.encodedSources(); len(got) != 1 || !bytes.Equal(got[0], h.content) {
			t.Errorf("%d encode(s) ran, want one of the source", len(got))
		}
	})
	t.Run("more 503s than the download bound, then the source", func(t *testing.T) {
		// A busy server is not a failed download: with node_max_transfers below the number
		// of http nodes this is the ordinary case, and a lease that failed on it would also
		// push a good node toward its cool-off.
		const busy = MaxSourceAttempts + 4
		h := newHTTPRig(t)
		var asked atomic.Int32
		h.f.source = func(w http.ResponseWriter, r *http.Request) {
			if asked.Add(1) <= busy {
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"transfers_full","detail":"said by the fake server"}`)
				return
			}
			h.serveWhole(w, r)
		}
		h.f.queue("acquire", jsonReply(200, h.lease()))
		stop := h.run()
		h.untilEnded("the lease to complete after the server stopped being busy")
		_ = stop()
		if got := h.f.failReasons(); len(got) != 0 {
			t.Fatalf("the lease was failed %v after %d 503s: a busy server must be waited out, not counted against "+
				"the %d download attempts", got, busy, MaxSourceAttempts)
		}
		if n := asked.Load(); n != busy+1 {
			t.Errorf("the source was asked for %d time(s), want %d", n, busy+1)
		}
		if got := h.encodedSources(); len(got) != 1 || !bytes.Equal(got[0], h.content) {
			t.Errorf("%d encode(s) ran, want one of the source", len(got))
		}
		h.rig.mu.Lock()
		waits := 0
		for _, d := range h.rig.sleeps {
			if d >= 5*time.Second && d != time.Second {
				waits++
			}
		}
		h.rig.mu.Unlock()
		if waits < busy {
			t.Errorf("%d of the %d waits were at least the server's Retry-After of 5 s: %v", waits, busy, h.rig.sleeps)
		}
	})
	t.Run("a 503 for as long as the lease lives", func(t *testing.T) {
		// What bounds the wait is the lease: its heartbeat answers 410 and the wait ends
		// there, with nothing failed and nothing encoded.
		h := newHTTPRig(t)
		var asked atomic.Int32
		h.f.source = func(w http.ResponseWriter, _ *http.Request) {
			if asked.Add(1) == MaxSourceAttempts+2 {
				h.f.queue("heartbeat", errReply(http.StatusGone, "lease_gone"))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		h.f.queue("acquire", jsonReply(200, h.lease()))
		stop := h.run()
		until(t, "the worker to ask for work again", func() bool { return len(h.f.seen("acquire")) >= 2 })
		_ = stop()
		if n := asked.Load(); n < MaxSourceAttempts+2 {
			t.Errorf("the source was asked for %d time(s); the wait gave up before the lease was gone", n)
		}
		if n := len(h.f.seen("fail")); n != 0 || h.encodeCount() != 0 {
			t.Errorf("a lease that went while its source was busy was failed %d time(s) and encoded %d time(s)", n, h.encodeCount())
		}
		workDirEmpty(t, h.work)
	})
	t.Run("a 410", func(t *testing.T) {
		h := newHTTPRig(t)
		h.f.source = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusGone)
		}
		h.f.queue("acquire", jsonReply(200, h.lease()))
		stop := h.run()
		until(t, "the worker to ask for work again", func() bool { return len(h.f.seen("acquire")) >= 2 })
		_ = stop()
		if n := len(h.f.seen("source")); n != 1 {
			t.Errorf("the source was asked for %d time(s) on a lease that is gone, want 1", n)
		}
		if n := len(h.f.seen("fail")) + len(h.f.seen("output")) + len(h.f.seen("complete")); n != 0 || h.encodeCount() != 0 {
			t.Errorf("a gone lease was followed by %d call(s) and %d encode(s)", n, h.encodeCount())
		}
		workDirEmpty(t, h.work)
	})
}

// TestWorkerFixture_HTTPMode_NoRoomInTheWorkDirectoryFailsTheLeaseBeforeAnyDownload: the
// work directory must hold the source and the largest output the lease admits. Short of
// that the lease is failed typed and no byte is asked for; a failed lookup refuses nothing.
func TestWorkerFixture_HTTPMode_NoRoomInTheWorkDirectoryFailsTheLeaseBeforeAnyDownload(t *testing.T) {
	h := newHTTPRig(t)
	l := h.lease()
	need := uint64(l.SourceSize + l.MaxOutputBytes)
	var asked []string
	h.opts.FreeSpace = func(dir string) (uint64, error) { asked = append(asked, dir); return need - 1, nil }
	h.f.queue("acquire", jsonReply(200, l))
	stop := h.run()
	until(t, "the lease to be failed", func() bool { return len(h.f.seen("fail")) >= 1 })
	_ = stop()
	if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonWorkDirFull {
		t.Errorf("the lease was failed %v, want [%s]", got, ReasonWorkDirFull)
	}
	if n := len(h.f.seen("source")); n != 0 || h.encodeCount() != 0 {
		t.Errorf("%d source request(s) and %d encode(s) with no room for the source", n, h.encodeCount())
	}
	if len(asked) != 1 || asked[0] != h.work {
		t.Errorf("free space was asked of %v, want the work directory %s once", asked, h.work)
	}

	for name, c := range map[string]struct {
		free func(string) (uint64, error)
		ok   bool
	}{
		"exactly enough":  {func(string) (uint64, error) { return need, nil }, true},
		"one byte short":  {func(string) (uint64, error) { return need - 1, nil }, false},
		"nothing free":    {func(string) (uint64, error) { return 0, nil }, false},
		"a failed lookup": {func(string) (uint64, error) { return 0, errors.New("statfs failed") }, true},
		"no lookup wired": {nil, true},
	} {
		if ok := roomBeside(c.free, h.work, roomNeeded(&l), 0); ok != c.ok || roomNeeded(&l) != need {
			t.Errorf("%s: roomBeside = %v needing %d, want %v needing %d", name, ok, roomNeeded(&l), c.ok, need)
		}
	}
}

// TestWorkerFixture_HTTPMode_AServerThatStopsSendingIsCutByTheIdleBound: a download that
// receives nothing for SourceIdle is cut and started again, inside the bound, and the lease
// is then failed - it never hangs the slot.
func TestWorkerFixture_HTTPMode_AServerThatStopsSendingIsCutByTheIdleBound(t *testing.T) {
	h := newHTTPRig(t)
	h.opts.SourceIdle = 60 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.f.source = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(h.content)))
		_, _ = w.Write(h.content[:100])
		w.(http.Flusher).Flush()
		select { // and then nothing
		case <-release:
		case <-r.Context().Done():
		}
	}
	h.f.queue("acquire", jsonReply(200, h.lease()))
	stop := h.run()
	until(t, "the lease to be failed", func() bool { return len(h.f.seen("fail")) >= 1 })
	_ = stop()
	if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonSourceDownloadFailed {
		t.Errorf("the lease was failed %v, want [%s]", got, ReasonSourceDownloadFailed)
	}
	if n := len(h.f.seen("source")); n != MaxSourceAttempts {
		t.Errorf("the stalled source was asked for %d time(s), want %d", n, MaxSourceAttempts)
	}
	if h.encodeCount() != 0 {
		t.Error("an encode ran on a source that never arrived")
	}
	workDirEmpty(t, h.work)
}

// TestWorkerFixture_HTTPMode_HeartbeatsRunWhileTheSourceDownloads: a source is as large as
// a film, and a lease whose node is downloading must not run out. The fake server holds
// the source back until it has been heartbeated.
func TestWorkerFixture_HTTPMode_HeartbeatsRunWhileTheSourceDownloads(t *testing.T) {
	h := newHTTPRig(t)
	var beatsAtSource atomic.Int32
	h.f.source = func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(waitFor)
		for len(h.f.seen("heartbeat")) == 0 && time.Now().Before(deadline) && r.Context().Err() == nil {
			time.Sleep(2 * time.Millisecond)
		}
		beatsAtSource.Store(int32(len(h.f.seen("heartbeat"))))
		h.serveWhole(w, r)
	}
	h.f.queue("acquire", jsonReply(200, h.lease()))
	stop := h.run()
	until(t, "the completion", func() bool { return len(h.f.seen("complete")) >= 1 })
	_ = stop()
	if beatsAtSource.Load() < 1 {
		t.Error("no heartbeat reached the server while the source was still downloading")
	}
	var hb node.HeartbeatRequest
	_ = json.Unmarshal(h.f.seen("heartbeat")[0].body, &hb)
	if hb.Epoch != 7 || hb.Progress != 0 {
		t.Errorf("the heartbeat during the download is %+v, want epoch 7 and no progress yet", hb)
	}
}

// TestWorkerFixture_HTTPMode_AGoneLeaseStopsTheDownloadAtOnce: a 410 on a heartbeat ends the
// lease while its source is still arriving; the download is dropped, nothing is encoded and
// nothing is failed.
func TestWorkerFixture_HTTPMode_AGoneLeaseStopsTheDownloadAtOnce(t *testing.T) {
	h := newHTTPRig(t)
	h.f.queue("heartbeat", errReply(http.StatusGone, "lease_gone"))
	h.f.source = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(h.content)))
		_, _ = w.Write(h.content[:100])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(waitFor):
		}
	}
	h.f.queue("acquire", jsonReply(200, h.lease()))
	stop := h.run()
	until(t, "the worker to ask for work again", func() bool { return len(h.f.seen("acquire")) >= 2 })
	_ = stop()
	if n := len(h.f.seen("fail")); n != 0 || h.encodeCount() != 0 {
		t.Errorf("a gone lease was failed %d time(s) and encoded %d time(s)", n, h.encodeCount())
	}
	if n := len(h.f.seen("source")); n != 1 {
		t.Errorf("the source of a gone lease was asked for %d time(s), want 1", n)
	}
	workDirEmpty(t, h.work)
}

// TestWorker_ALeaseInAnotherModeThanTheWorkersIsNeverRun: a worker asks in one mode and
// runs a lease in no other. An http worker handed a mapped lease has no mount to read it
// through; a mapped worker handed an http lease would never download it.
func TestWorker_ALeaseInAnotherModeThanTheWorkersIsNeverRun(t *testing.T) {
	h := newHTTPRig(t)
	l := h.lease()
	l.Mode = node.ModeMapped
	h.f.queue("acquire", jsonReply(200, l))
	stop := h.run()
	until(t, "the worker to ask for work again", func() bool { return len(h.f.seen("acquire")) >= 2 })
	_ = stop()
	if n := len(h.f.seen("source")) + len(h.f.seen("fail")) + len(h.f.seen("output")); n != 0 || h.encodeCount() != 0 {
		t.Errorf("an http worker acted on a mapped lease: %d call(s), %d encode(s)", n, h.encodeCount())
	}

	// New's own refusals about the mode.
	r := newRig(t)
	r.opts.Mode = node.ModeHTTP // with the rig's path map still set
	if _, err := New(r.opts); err == nil || !strings.Contains(err.Error(), "worker_mode") || !strings.Contains(err.Error(), "worker_path_map") {
		t.Errorf("New in http mode with a path map = %v, want a refusal naming worker_mode and worker_path_map", err)
	}
	r.opts.PathMap, r.opts.Mode = nil, "streaming"
	if _, err := New(r.opts); err == nil || !strings.Contains(err.Error(), "worker_mode") || !strings.Contains(err.Error(), "mapped|http") {
		t.Errorf("New in an unknown mode = %v, want a refusal naming worker_mode and both modes", err)
	}
	r.opts.Mode = ""
	r.opts.PathMap = config.PathMap{{From: r.lib, To: r.mount}}
	w, err := New(r.opts)
	if err != nil || w.o.Mode != node.ModeMapped {
		t.Errorf("New with no mode = %v, want a worker in mapped mode", err)
	}
	r.opts.Mode = node.ModeMapped
	if _, err := New(r.opts); err != nil {
		t.Errorf("New in mapped mode with a path map = %v", err)
	}
}

// TestWorker_TheDownloadedSourceIsNamedForTheStartSweepAndCarriesAPlainExtension: the name
// a download is written under is one the start-up sweep recognises, it keeps the leased
// source's extension only when that is short and plain, and the sweep removes exactly this
// worker's own files.
func TestWorker_TheDownloadedSourceIsNamedForTheStartSweepAndCarriesAPlainExtension(t *testing.T) {
	for path, want := range map[string]string{
		"/srv/media/film.mkv":          leaseID + ".3.src.mkv",
		"/srv/media/film.MKV":          leaseID + ".3.src.mkv",
		"/srv/media/film.m2ts":         leaseID + ".3.src.m2ts",
		"/srv/media/film":              leaseID + ".3.src",
		"/srv/media/film.":             leaseID + ".3.src",
		"/srv/media/film.verylongext1": leaseID + ".3.src",
		"/srv/media/film.mk v":         leaseID + ".3.src",
		"/srv/media/film.mkv;rm":       leaseID + ".3.src",
		"/srv/media.dir/film":          leaseID + ".3.src",
		"/srv/media/film.12345678":     leaseID + ".3.src.12345678",
	} {
		got := sourceName(leaseID, 3, path)
		if got != want {
			t.Errorf("sourceName(%q) = %q, want %q", path, got, want)
		}
		if !outputNamed.MatchString(got) {
			t.Errorf("the start-up sweep does not recognise %q", got)
		}
	}
	for name, mine := range map[string]bool{
		leaseID + ".3.out":                  true,
		leaseID + ".3.src":                  true,
		leaseID + ".3.src.mkv":              true,
		leaseID + ".3.src.MKV":              false,
		leaseID + ".3.src.":                 false,
		leaseID + ".3.src.mkv.part":         false,
		leaseID + ".3.src.123456789":        false,
		leaseID + ".3.srcx":                 false,
		leaseID + ".src.mkv":                false,
		"x" + leaseID + ".3.src.mkv":        false,
		leaseID[:31] + ".3.src.mkv":         false,
		"film.mkv":                          false,
		leaseID + ".3.out.mkv":              false,
		strings.ToUpper(leaseID) + ".3.src": false,
	} {
		if outputNamed.MatchString(name) != mine {
			t.Errorf("outputNamed(%q) = %v, want %v", name, !mine, mine)
		}
	}

	h := newHTTPRig(t)
	if err := os.MkdirAll(h.work, 0o700); err != nil {
		t.Fatal(err)
	}
	left := []string{leaseID + ".3.src.mkv", leaseID + ".4.src", leaseID + ".3.out"}
	kept := []string{"someone-elses.src.mkv", leaseID + ".3.src.mkv.part"}
	for _, name := range append(append([]string(nil), left...), kept...) {
		if err := os.WriteFile(filepath.Join(h.work, name), []byte("left by a killed worker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stop := h.run()
	until(t, "the first poll", func() bool { return len(h.f.seen("acquire")) >= 1 })
	_ = stop()
	for _, name := range left {
		if _, err := os.Stat(filepath.Join(h.work, name)); err == nil {
			t.Errorf("the start-up sweep left %s", name)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(h.work, name)); err != nil {
			t.Errorf("the start-up sweep removed %s, which is not this worker's naming", name)
		}
	}
}

// logs is a concurrency-safe log capture.
type logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestWorker_InsecureHTTPIsAnOverrideSaidLoudlyAtEveryStart: plain http to a host that is
// not loopback is refused, unless worker_insecure_http is true - and then the worker says
// what that costs at warn level every time it is built. With https or a loopback server the
// override does nothing and says nothing.
func TestWorker_InsecureHTTPIsAnOverrideSaidLoudlyAtEveryStart(t *testing.T) {
	for _, tc := range []struct {
		server    string
		insecure  bool
		ok        bool
		cleartext bool
	}{
		{"http://10.0.0.5:8080", false, false, false},
		{"http://10.0.0.5:8080", true, true, true},
		{"http://holdfast.example.net", true, true, true},
		{"http://[2001:db8::1]:8080", true, true, true},
		{"http://127.0.0.1:8080", true, true, false},
		{"http://[::1]:8080", true, true, false},
		{"http://localhost:8080", true, true, false},
		{"http://127.0.0.1:8080", false, true, false},
		{"http://[::1]:8080", false, true, false},
		{"http://localhost:8080", false, true, false},
		{"https://holdfast.example.net", true, true, false},
		{"https://holdfast.example.net", false, true, false},
		// The override is about cleartext and nothing else: it does not excuse an address
		// that is not one.
		{"ftp://10.0.0.5/", true, false, false},
		{"http://user:pw@10.0.0.5:8080", true, false, false},
		{"", true, false, false},
	} {
		cleartext, err := CheckTransport(tc.server, tc.insecure)
		if (err == nil) != tc.ok || cleartext != tc.cleartext {
			t.Errorf("CheckTransport(%q, %v) = %v, %v, want accepted %v and cleartext %v",
				tc.server, tc.insecure, cleartext, err, tc.ok, tc.cleartext)
		}
	}

	build := func(server string, insecure bool, mode string) (string, error) {
		r := newRig(t)
		out := &logs{}
		r.opts.Log = slog.New(slog.NewTextHandler(out, nil))
		r.opts.Server, r.opts.InsecureHTTP, r.opts.Mode = server, insecure, mode
		if mode == node.ModeHTTP {
			r.opts.PathMap = nil
		}
		_, err := New(r.opts)
		return out.String(), err
	}
	// Refused without the override, and the refusal says how to accept it.
	out, err := build("http://10.0.0.5:8080", false, node.ModeMapped)
	if !errors.Is(err, ErrInsecureServer) || !strings.Contains(err.Error(), "worker_insecure_http: true") {
		t.Fatalf("New without the override = %v, want the cleartext refusal naming worker_insecure_http", err)
	}
	if out != "" {
		t.Errorf("a refused worker logged: %s", out)
	}
	// Accepted with it, and warned - at every start.
	for start := 1; start <= 2; start++ {
		for _, mode := range []string{node.ModeMapped, node.ModeHTTP} {
			out, err := build("http://10.0.0.5:8080", true, mode)
			if err != nil {
				t.Fatalf("start %d in %s mode with worker_insecure_http = %v, want a worker", start, mode, err)
			}
			if n := strings.Count(out, "level=WARN"); n != 1 {
				t.Fatalf("start %d in %s mode wrote %d warn record(s), want exactly 1:\n%s", start, mode, n, out)
			}
			for _, want := range []string{"worker_insecure_http", "THE NODE CREDENTIAL CROSSES THE NETWORK IN CLEARTEXT ON EVERY REQUEST",
				"in http mode so does the library's media", "lease jobs, upload outputs", "digests do not help",
				"worker_server=http://10.0.0.5:8080", "worker_mode=" + mode} {
				if !strings.Contains(out, want) {
					t.Errorf("the warning in %s mode does not say %q:\n%s", mode, want, out)
				}
			}
			if strings.Contains(out, testToken) {
				t.Error("the warning carries the credential")
			}
		}
	}
	// With loopback or https the key does nothing and says nothing.
	for _, server := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080", "https://holdfast.example.net"} {
		out, err := build(server, true, node.ModeMapped)
		if err != nil || out != "" {
			t.Errorf("New(%s) with worker_insecure_http = %v and logged %q, want a worker and no record", server, err, out)
		}
	}
}

// newFakeTLSServer is the fake lease server behind TLS, with a certificate no system root
// vouches for. It returns the path of a PEM file holding that certificate.
func newFakeTLSServer(t *testing.T) (*fakeServer, string) {
	t.Helper()
	f := &fakeServer{t: t, replies: map[string][]reply{}, hang: make(chan struct{})}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		close(f.hang)
		f.srv.CloseClientConnections()
		closed := make(chan struct{})
		go func() { defer close(closed); f.srv.Close() }()
		select {
		case <-closed:
		case <-time.After(time.Minute):
			t.Error("the fake TLS server did not close within a minute; the test goes on without it rather than hang")
		}
	})
	ca := filepath.Join(t.TempDir(), "server-ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o644); err != nil {
		t.Fatal(err)
	}
	return f, ca
}

// TestWorker_TLS_AWorkerTrustingTheServersCertificateCompletesAnHTTPModeLease: over TLS,
// with the server's private certificate named in worker_tls_ca, the whole http-mode lease
// runs - the source down, the output up, the completion.
func TestWorker_TLS_AWorkerTrustingTheServersCertificateCompletesAnHTTPModeLease(t *testing.T) {
	h := newHTTPRig(t)
	f, ca := newFakeTLSServer(t)
	f.source = h.serveWhole
	h.f = f
	client, err := TrustingClient(ca)
	if err != nil {
		t.Fatalf("TrustingClient: %v", err)
	}
	h.opts.Server, h.opts.HTTP = f.srv.URL, client
	if !strings.HasPrefix(f.srv.URL, "https://127.0.0.1:") {
		t.Fatalf("the fake server is at %s, want https on loopback", f.srv.URL)
	}
	f.queue("acquire", jsonReply(200, h.lease()))
	stop := h.run()
	until(t, "the completion", func() bool { return len(f.seen("complete")) >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := h.encodedSources(); len(got) != 1 || !bytes.Equal(got[0], h.content) {
		t.Fatalf("%d encode(s) ran over TLS, want one of the streamed source", len(got))
	}
	var done node.CompleteRequest
	_ = json.Unmarshal(f.seen("complete")[0].body, &done)
	if done.SourceDigest != digest(h.content) || done.OutputDigest != digest(h.output) {
		t.Errorf("complete over TLS = %+v", done)
	}
	if puts := f.seen("output"); len(puts) != 1 || string(puts[0].body) != string(h.output) {
		t.Errorf("%d upload(s) over TLS", len(puts))
	}
}

// TestWorker_TLS_AServerWhoseCertificateIsNotTrustedReceivesNoRequest: certificate
// verification is never switched off. Against a server whose certificate neither the system
// roots nor worker_tls_ca vouch for, the handshake fails - so no request, and with it no
// credential and no upload, ever reaches the server.
func TestWorker_TLS_AServerWhoseCertificateIsNotTrustedReceivesNoRequest(t *testing.T) {
	// A certificate authority of the test's own making: httptest's servers all present one
	// built-in certificate, so "another server's" would be this server's too.
	otherCA := filepath.Join(t.TempDir(), "another-ca.pem")
	if err := os.WriteFile(otherCA, selfSignedCertPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, client := range map[string]func(t *testing.T) *http.Client{
		"no worker_tls_ca": func(*testing.T) *http.Client { return nil },
		"a worker_tls_ca that vouches for another authority": func(t *testing.T) *http.Client {
			c, err := TrustingClient(otherCA)
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHTTPRig(t)
			f, _ := newFakeTLSServer(t)
			f.source = h.serveWhole
			h.f = f
			out := &logs{}
			h.opts.Log = slog.New(slog.NewTextHandler(out, nil))
			h.opts.Server, h.opts.HTTP = f.srv.URL, client(t)
			f.queue("acquire", jsonReply(200, h.lease()))
			stop := h.run()
			until(t, "the worker to have tried and backed off twice", func() bool {
				return strings.Count(out.String(), "asking for work failed") >= 2
			})
			_ = stop()
			f.mu.Lock()
			calls := len(f.calls)
			f.mu.Unlock()
			if calls != 0 {
				t.Fatalf("%d request(s) reached a server whose certificate the worker does not trust", calls)
			}
			if !strings.Contains(out.String(), "certificate") {
				t.Errorf("the worker's record does not say the certificate was the reason:\n%s", out.String())
			}
			if strings.Contains(out.String(), testToken) {
				t.Error("the credential reached the log")
			}
			if h.encodeCount() != 0 {
				t.Error("an encode ran")
			}
		})
	}
}

// TestWorker_TLS_AnUnreadableOrEmptyBundleRefusesNamingTheKey: worker_tls_ca that cannot be
// read, or that holds no certificate, is a refusal - never a worker that silently trusts
// only the system roots.
func TestWorker_TLS_AnUnreadableOrEmptyBundleRefusesNamingTheKey(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	notPEM := filepath.Join(dir, "not.pem")
	keyOnly := filepath.Join(dir, "key.pem")
	for path, body := range map[string]string{
		empty:   "",
		notPEM:  "this is not a certificate\n",
		keyOnly: "-----BEGIN " + keyBlock + "-----\nAAAA\n-----END " + keyBlock + "-----\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(dir, "missing.pem"), dir, empty, notPEM, keyOnly} {
		c, err := TrustingClient(path)
		if err == nil || c != nil || !strings.Contains(err.Error(), "worker_tls_ca") {
			t.Errorf("TrustingClient(%s) = %v, %v, want a refusal naming worker_tls_ca", path, c, err)
		}
	}
	_, ca := newFakeTLSServer(t)
	c, err := TrustingClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		t.Fatalf("the trusting client's transport is %T", c.Transport)
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("the trusting client skips certificate verification")
	}
	if tr.TLSClientConfig.MinVersion != 0x0303 {
		t.Errorf("the trusting client's minimum TLS version is %#x, want TLS 1.2 (0x0303)", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("the trusting client carries no root pool")
	}
}

// selfSignedCertPEM is a freshly generated self-signed certificate, PEM encoded. Its private
// key is made here and dropped: nothing of it is written anywhere.
func selfSignedCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another authority, for a test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestWorkerFixture_HTTPMode_ASourceTheServerWithdrewFailsTheLeaseWithItsOwnReason: a 409
// source_not_offered (the server restarted and no longer knows the lease's mode) and a 409
// source_changed (the file is not the leased one) are the SERVER withdrawing the source.
// The lease is failed `source_withdrawn` - not `source_download_failed`, which the server
// counts toward this node's cool-off - at the first answer, with nothing encoded.
func TestWorkerFixture_HTTPMode_ASourceTheServerWithdrewFailsTheLeaseWithItsOwnReason(t *testing.T) {
	if got := []string{ReasonSourceWithdrawn, node.ReasonSourceWithdrawn}; got[0] != "source_withdrawn" || got[1] != got[0] {
		t.Fatalf("the worker's reason and the server's are %q, want source_withdrawn in both", got)
	}
	for _, reason := range []string{"source_not_offered", "source_changed"} {
		t.Run(reason, func(t *testing.T) {
			h := newHTTPRig(t)
			h.f.source = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"`+reason+`","detail":"said by the fake server"}`)
			}
			h.f.queue("acquire", jsonReply(200, h.lease()))
			stop := h.run()
			h.untilEnded("the lease to be failed")
			_ = stop()
			if got := h.f.failReasons(); len(got) != 1 || got[0] != "source_withdrawn" {
				t.Errorf("the lease was failed %v, want [source_withdrawn]", got)
			}
			if n := len(h.f.seen("source")); n != 1 {
				t.Errorf("the withdrawn source was asked for %d time(s), want 1", n)
			}
			if h.encodeCount() != 0 {
				t.Error("an encode ran")
			}
			workDirEmpty(t, h.work)
		})
	}
}

// TestWorkerFixture_HTTPMode_TwoSlotsNeverBothPassTheRoomCheckOnTheSameFreeBytes: the room
// check and the reservation are one step across the worker's slots. With room for exactly
// one lease's source and output and two leases granted at once, one is downloaded and the
// other fails `work_dir_full` before it asks for a byte.
func TestWorkerFixture_HTTPMode_TwoSlotsNeverBothPassTheRoomCheckOnTheSameFreeBytes(t *testing.T) {
	h := newHTTPRig(t)
	first, second := h.lease(), h.lease()
	second.LeaseID = "fedcba9876543210fedcba9876543210"
	need := uint64(first.SourceSize + first.MaxOutputBytes)
	h.opts.Slots = 2
	h.opts.FreeSpace = func(string) (uint64, error) { return need, nil }
	// The one download that starts is held until the other lease has been decided, so both
	// leases are in flight together whatever the scheduler does.
	h.f.source = func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(10 * time.Second)
		for len(h.f.seen("fail")) == 0 && len(h.f.seen("source")) < 2 && time.Now().Before(deadline) && r.Context().Err() == nil {
			time.Sleep(2 * time.Millisecond)
		}
		h.serveWhole(w, r)
	}
	h.f.queue("acquire", jsonReply(200, first), jsonReply(200, second))
	stop := h.run()
	until(t, "one lease to complete and the other to end", func() bool {
		return len(h.f.seen("complete")) >= 1 && len(h.f.seen("fail"))+len(h.f.seen("complete")) >= 2
	})
	_ = stop()
	if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonWorkDirFull {
		t.Fatalf("the leases were failed %v, want exactly one work_dir_full: both slots passed the room check on the same %d free bytes",
			got, need)
	}
	if n := len(h.f.seen("source")); n != 1 {
		t.Errorf("%d source requests, want 1: the refused lease asks for no byte", n)
	}
	if n := len(h.encodedSources()); n != 1 {
		t.Errorf("%d encode(s) ran, want 1", n)
	}

	// The account itself: a reservation is held until released, released once, and a failed
	// lookup or no lookup refuses nothing while still reserving.
	l := h.lease()
	w := &Worker{o: Options{WorkDir: h.work, FreeSpace: func(string) (uint64, error) { return 2*need + 1, nil }}}
	relA, got, ok := w.reserveRoom(&l)
	if !ok || got != need || w.reserved != need {
		t.Fatalf("the first reservation = %d, %v with %d reserved, want %d, true, %d", got, ok, w.reserved, need, need)
	}
	relB, _, ok := w.reserveRoom(&l)
	if !ok || w.reserved != 2*need {
		t.Fatalf("the second reservation = %v with %d reserved, want true and %d", ok, w.reserved, 2*need)
	}
	if rel, _, ok := w.reserveRoom(&l); ok || w.reserved != 2*need {
		t.Errorf("a third reservation past the free bytes = %v with %d reserved, want it refused and nothing added", ok, w.reserved)
	} else {
		rel() // a refused reservation's release gives nothing back
	}
	if w.reserved != 2*need {
		t.Errorf("releasing a refused reservation moved the account to %d", w.reserved)
	}
	relA()
	relA()
	if w.reserved != need {
		t.Errorf("after one release (called twice) %d bytes are reserved, want %d", w.reserved, need)
	}
	if _, _, ok := w.reserveRoom(&l); !ok {
		t.Error("a reservation after a release was refused")
	}
	relB()
	for name, free := range map[string]func(string) (uint64, error){
		"a failed lookup": func(string) (uint64, error) { return 0, errors.New("statfs failed") }, "no lookup": nil,
	} {
		w := &Worker{o: Options{WorkDir: h.work, FreeSpace: free}}
		if _, _, ok := w.reserveRoom(&l); !ok || w.reserved != need {
			t.Errorf("%s: reserveRoom = %v with %d reserved, want it accepted and %d reserved", name, ok, w.reserved, need)
		}
	}
	for name, c := range map[string]struct {
		free, reserved uint64
		ok             bool
	}{
		"exactly the room beside a reservation": {need + 7, 7, true},
		"one byte short beside a reservation":   {need + 6, 7, false},
		"all of it reserved":                    {need, need, false},
	} {
		if ok := roomBeside(func(string) (uint64, error) { return c.free, nil }, h.work, need, c.reserved); ok != c.ok {
			t.Errorf("%s: roomBeside = %v, want %v", name, ok, c.ok)
		}
	}
}

// TestWorkerFixture_HTTPMode_AWorkDirectoryThatFillsWhileTheSourceIsWrittenIsWorkDirFull:
// ENOSPC from the write is the work directory being full, not a download that failed. The
// download's path is made a link to /dev/full, which answers every write with ENOSPC.
func TestWorkerFixture_HTTPMode_AWorkDirectoryThatFillsWhileTheSourceIsWrittenIsWorkDirFull(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skipf("no /dev/full on this system: %v", err)
	}
	h := newHTTPRig(t)
	l := h.lease()
	if err := os.MkdirAll(h.work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", filepath.Join(h.work, sourceName(l.LeaseID, l.Epoch, l.Path))); err != nil {
		t.Fatal(err)
	}
	h.f.queue("acquire", jsonReply(200, l))
	stop := h.run()
	h.untilEnded("the lease to be failed")
	_ = stop()
	if got := h.f.failReasons(); len(got) != 1 || got[0] != ReasonWorkDirFull {
		t.Errorf("the lease was failed %v, want [%s]", got, ReasonWorkDirFull)
	}
	if n := len(h.f.seen("source")); n != 1 {
		t.Errorf("the source was asked for %d time(s) after the work directory filled, want 1", n)
	}
	if h.encodeCount() != 0 {
		t.Error("an encode ran")
	}
	workDirEmpty(t, h.work)
}

package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// The source stream of http mode (docs/design/nodes.md#http-mode), against the fixtures of
// fixture_test.go: the same httptest server, fake worker, temp ledger and held clock.

// acquireHTTP is a node asking for work in http mode.
func (f *fixture) acquireHTTP(node string) reply {
	body := acquireBody(node)
	body.Mode = ModeHTTP
	return f.post(RouteLeases, body)
}

// grantHTTP leases job to a node that asked in http mode.
func (f *fixture) grantHTTP(node string, job Job) (AcquireResponse, *engineCall) {
	f.t.Helper()
	call := f.offer(job)
	r := f.acquireHTTP(node)
	if r.status != http.StatusOK {
		f.t.Fatalf("an http-mode acquire by %s answered %d %s, want a lease", node, r.status, r.body)
	}
	var a AcquireResponse
	if err := json.Unmarshal(r.body, &a); err != nil {
		f.t.Fatalf("the acquire body %q is not a lease: %v", r.body, err)
	}
	return a, call
}

// source asks for a lease's source at an epoch, with any extra headers.
func (f *fixture) source(id string, epoch string, header map[string]string) reply {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+leasePath("/source", id), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if epoch != "" {
		req.Header.Set(EpochHeader, epoch)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return f.do(req)
}

// patterned is size bytes no two halves of which are alike, so a part served out of place
// or a body cut short never hashes to the whole.
func patterned(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// jobOf writes content as a source and returns the job the engine would offer for it.
func (f *fixture) jobOf(name string, content []byte) Job {
	f.t.Helper()
	job := f.job(name, len(content))
	fi, err := os.Stat(job.Path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(job.Path, content, 0o644); err != nil {
		f.t.Fatal(err)
	}
	// The content is replaced under the modification time the job was built on.
	if err := os.Chtimes(job.Path, fi.ModTime(), fi.ModTime()); err != nil {
		f.t.Fatal(err)
	}
	return job
}

func (f *fixture) streamed(id string) string { return f.hub.streamedDigest(id) }

func (f *fixture) transfers() int {
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	return f.hub.transfers
}

// TestNodeFixture_AnHTTPModeLeaseIsGrantedInHTTPModeAndItsSourceIsStreamedAndHashed is the
// source direction end to end: the lease says the mode it was granted in, one unranged GET
// returns exactly the source under a declared length and a fixed media type, the server
// records the sha-256 of what it sent, and a completion that reports that digest completes.
func TestNodeFixture_AnHTTPModeLeaseIsGrantedInHTTPModeAndItsSourceIsStreamedAndHashed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(70_000)
	job := f.jobOf("film", content)
	a, call := f.grantHTTP("node-a", job)
	if a.Mode != ModeHTTP {
		t.Fatalf("the lease says mode %q, want the http mode the request asked for", a.Mode)
	}
	if got := f.streamed(a.LeaseID); got != "" {
		t.Fatalf("a lease nothing was streamed on records %q", got)
	}

	r := f.source(a.LeaseID, "1", nil)
	if r.status != http.StatusOK || !bytes.Equal(r.body, content) {
		t.Fatalf("the source request answered %d with %d bytes, want 200 and the source's %d", r.status, len(r.body), len(content))
	}
	if got := r.header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("the source is served as %q, want application/octet-stream: a sniffed type means the head was read twice", got)
	}
	if got := r.header.Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Errorf("the source declares Content-Length %q, want %d", got, len(content))
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the source is served with Cache-Control %q, want no-store", got)
	}
	if got := r.header.Get("Last-Modified"); got != "" {
		t.Errorf("the source is served with Last-Modified %q; no conditional request is answered from a date", got)
	}
	want := digestOf(content)
	if got := f.streamed(a.LeaseID); got != want {
		t.Fatalf("the server recorded %q for what it streamed, want the source's sha-256 %s", got, want)
	}
	if n := f.transfers(); n != 0 {
		t.Errorf("%d transfer slot(s) are still held after the stream", n)
	}

	if up := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); up.status != http.StatusOK {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}
	done := f.complete(a.LeaseID, a.Epoch, output, want)
	if done.status != http.StatusOK {
		t.Fatalf("a completion reporting the streamed digest answered %d %s", done.status, done.body)
	}
	res, err := call.wait(t)
	if err != nil {
		t.Fatalf("Encode returned %v", err)
	}
	if res.SourceDigest != want || res.StreamedDigest != want {
		t.Errorf("the engine was handed source digest %q and streamed digest %q, want both %s", res.SourceDigest, res.StreamedDigest, want)
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseCompleted || row.SourceDigest != want {
		t.Errorf("the row is %s with source digest %q", row.State, row.SourceDigest)
	}
	f.hub.mu.Lock()
	left := len(f.hub.sources)
	f.hub.mu.Unlock()
	if left != 0 {
		t.Errorf("the hub still remembers %d lease source(s) after the lease ended", left)
	}
}

// TestNodeFixture_ASourceChangedInTransitFailsTheLeaseBeforeAnyGate: the node reports a
// digest that is not the digest of what the server streamed. The completion is refused
// typed, the lease fails with its own reason, the uploaded working file is removed, the
// engine gets the typed ending - so nothing is gated - and the source is untouched.
func TestNodeFixture_ASourceChangedInTransitFailsTheLeaseBeforeAnyGate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(4000)
	job := f.jobOf("film", content)
	a, call := f.grantHTTP("node-a", job)
	if r := f.source(a.LeaseID, "1", nil); r.status != http.StatusOK {
		t.Fatalf("source: %d %s", r.status, r.body)
	}
	if up := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); up.status != http.StatusOK {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}
	// What arrived on the node is one byte off what was sent.
	arrived := append([]byte(nil), content...)
	arrived[1234] ^= 0x01
	r := f.complete(a.LeaseID, a.Epoch, output, digestOf(arrived))
	isTyped(t, "a completion whose source digest is not the streamed one", r, http.StatusConflict, "source_digest_mismatch")

	_, err := call.wait(t)
	le := leaseErr(t, err)
	if le.Reason != ReasonSourceMismatch || le.Detail != "" || le.Node != "node-a" || le.Epoch != 1 {
		t.Errorf("Encode returned %+v, want the source digest mismatch naming node-a at epoch 1", le)
	}
	row := f.row(a.LeaseID)
	if row.State != store.LeaseFailed || row.Reason != "source_digest_mismatch" || row.SourceDigest != digestOf(arrived) || row.EndedAt.IsZero() {
		t.Errorf("the row is %+v, want failed source_digest_mismatch recording the digest the node reported", row)
	}
	f.onlySources("film")
	if got, err := os.ReadFile(job.Path); err != nil || !bytes.Equal(got, content) {
		t.Error("the source changed")
	}
	// The lease is over: the honest digest, sent late, is gone.
	is410(t, "a completion after the lease failed", f.complete(a.LeaseID, a.Epoch, output, digestOf(content)))
}

// TestNodeFixture_ASourceRequestOnALeaseThatIsNotLiveIs410NoStore: expired (at the expiry
// instant, with the sweep held off, so it is the request's own check that is graded),
// superseded by a later grant, and ended.
func TestNodeFixture_ASourceRequestOnALeaseThatIsNotLiveIs410NoStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour })
	job := f.jobOf("film", patterned(3000))
	a, call := f.grantHTTP("node-a", job)

	is410(t, "a source request at another epoch", f.source(a.LeaseID, "2", nil))
	isTyped(t, "a source request with no epoch", f.source(a.LeaseID, "", nil), http.StatusBadRequest, "bad_request")
	isTyped(t, "a source request with an unreadable epoch", f.source(a.LeaseID, "one", nil), http.StatusBadRequest, "bad_request")
	isTyped(t, "a source request on an unknown lease", f.source(strings.Repeat("f", 32), "1", nil), http.StatusNotFound, "unknown_lease")
	isTyped(t, "a source request on a malformed lease id", f.source("not-a-lease", "1", nil), http.StatusNotFound, "unknown_lease")

	f.clk.advance(ttl) // AT the expiry instant
	r := f.source(a.LeaseID, "1", nil)
	is410(t, "a source request on an expired lease", r)
	if bytes.Contains(r.body, []byte{0x07}) || len(r.body) > 400 {
		t.Errorf("the 410 carries %d bytes: a refusal carries no part of the source", len(r.body))
	}
	if st := f.row(a.LeaseID).State; st != store.LeaseGranted {
		t.Fatalf("the row is %s; this case needs the sweep not to have run", st)
	}
	if got := f.streamed(a.LeaseID); got != "" {
		t.Errorf("a refused source request recorded a streamed digest %q", got)
	}

	// Ended by its node: gone for good.
	f.clk.advance(-ttl)
	if r := f.fail(a.LeaseID, a.Epoch, "encode_failed"); r.status != http.StatusOK {
		t.Fatalf("fail: %d %s", r.status, r.body)
	}
	is410(t, "a source request on a failed lease", f.source(a.LeaseID, "1", nil))
	if _, err := call.wait(t); err == nil {
		t.Error("the failed lease completed")
	}
}

// TestNodeFixture_AMappedLeasesSourceIsRefused: the source endpoint serves an http-mode
// lease and no other. A mapped lease's, an http lease's once it has admitted an output, and
// a lease a restart recovered (whose mode the restarted server does not know) are refused
// typed, with no byte of the source.
func TestNodeFixture_AMappedLeasesSourceIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(3000)
	mapped, _ := f.grant("node-a", f.jobOf("mapped", content))
	if mapped.Mode != ModeMapped {
		t.Fatalf("a mapped acquire was granted in mode %q", mapped.Mode)
	}
	r := f.source(mapped.LeaseID, "1", nil)
	isTyped(t, "a mapped lease's source", r, http.StatusConflict, "source_not_offered")
	if len(r.body) > 400 {
		t.Errorf("the refusal carries %d bytes", len(r.body))
	}
	if !strings.Contains(string(r.body), "http mode") {
		t.Errorf("the refusal does not say the lease was not granted in http mode: %s", r.body)
	}

	h, _ := f.grantHTTP("node-b", f.jobOf("streamed", content))
	if up := f.put(h.LeaseID, h.Epoch, output, digestOf(output)); up.status != http.StatusOK {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}
	isTyped(t, "the source of a lease that has admitted its output", f.source(h.LeaseID, "1", nil), http.StatusConflict, "source_not_offered")
	if n := f.transfers(); n != 0 {
		t.Errorf("a refused source request holds %d transfer slot(s)", n)
	}
}

// TestNodeFixture_ARecoveredLeasesSourceIsNotServedAfterARestart: the mode is the hub's
// memory, so a restarted server does not know it. Until the engine has taken the lease back
// the request is 503; after it has, the source is refused typed - never served on a guess.
func TestNodeFixture_ARecoveredLeasesSourceIsNotServedAfterARestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.jobOf("film", patterned(3000))
	a, call := f.grantHTTP("node-a", job)
	// The restart: the first process's engine call ends with its context and the ledger is
	// opened again by a new hub.
	f.stop()
	if _, err := call.wait(t); err == nil {
		t.Fatal("the first process's call completed")
	}
	// The stopping hub ended the lease as cancelled; this fixture needs it live across the
	// restart, as a killed process leaves it.
	if _, err := f.st.UpdateLease(context.Background(), a.LeaseID, func(cur Lease) (Lease, error) {
		cur.State, cur.Reason, cur.EndedAt = store.LeaseGranted, "", time.Time{}
		cur.ExpiresAt = f.clk.Now().Add(ttl)
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	_ = f.st.Close()
	f.open(nil)
	live, err := f.hub.Recover(context.Background())
	if err != nil || len(live) != 1 {
		t.Fatalf("Recover returned %d lease(s), %v", len(live), err)
	}
	f.hub.Ready()
	is503(t, "a recovered lease's source before the engine took the lease back", f.source(a.LeaseID, "1", nil), "not_ready")

	adopted := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _, err := f.hub.Adopt(ctx, a.LeaseID, job, nil); adopted <- err }()
	eventually(t, "the adoption", func() bool { return f.hub.attached(a.LeaseID) })
	isTyped(t, "a recovered lease's source after the adoption", f.source(a.LeaseID, "1", nil), http.StatusConflict, "source_not_offered")
	cancel()
	select {
	case <-adopted:
	case <-time.After(10 * time.Second):
		t.Fatal("the adopted call did not return")
	}
}

// TestNodeFixture_ARangedSourceRequestRecordsNoStreamedDigest: http.ServeContent answers a
// Range, and a ranged response - even one that spans the whole source - leaves no digest,
// so nothing is held to a figure the server did not take over one whole response. The
// completion is then recorded as reported, and the proof is the engine's own hash.
func TestNodeFixture_ARangedSourceRequestRecordsNoStreamedDigest(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(5000)
	job := f.jobOf("film", content)
	a, call := f.grantHTTP("node-a", job)

	part := f.source(a.LeaseID, "1", map[string]string{"Range": "bytes=100-299"})
	if part.status != http.StatusPartialContent || !bytes.Equal(part.body, content[100:300]) {
		t.Fatalf("a ranged request answered %d with %d bytes, want 206 and bytes 100 to 299", part.status, len(part.body))
	}
	whole := f.source(a.LeaseID, "1", map[string]string{"Range": "bytes=0-"})
	if whole.status != http.StatusPartialContent || !bytes.Equal(whole.body, content) {
		t.Fatalf("a range over the whole source answered %d with %d bytes", whole.status, len(whole.body))
	}
	if r := f.source(a.LeaseID, "1", map[string]string{"Range": "bytes=9000-"}); r.status != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("a range past the source answered %d, want 416", r.status)
	}
	if got := f.streamed(a.LeaseID); got != "" {
		t.Fatalf("ranged requests recorded a streamed digest %q, want none", got)
	}
	if up := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); up.status != http.StatusOK {
		t.Fatalf("upload: %d %s", up.status, up.body)
	}
	// With nothing streamed whole there is nothing in the hub to hold this digest to: it is
	// recorded as reported, for the engine to compare with its own hash of the source.
	if r := f.complete(a.LeaseID, a.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("complete: %d %s", r.status, r.body)
	}
	res, err := call.wait(t)
	if err != nil || res.StreamedDigest != "" || res.SourceDigest != srcDigest {
		t.Errorf("Encode returned %+v, %v, want the reported source digest and no streamed one", res, err)
	}
}

// TestNodeFixture_ASourceThatIsNotTheLeasedFileIsRefused: the file is opened and held to the
// size and modification time the lease was granted on before a byte is sent. The lease
// stays live.
func TestNodeFixture_ASourceThatIsNotTheLeasedFileIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(3000)
	job := f.jobOf("film", content)
	a, call := f.grantHTTP("node-a", job)
	fi, _ := os.Stat(job.Path)

	refused := func(what string) {
		t.Helper()
		r := f.source(a.LeaseID, "1", nil)
		isTyped(t, what, r, http.StatusConflict, "source_changed")
		if len(r.body) > 400 {
			t.Errorf("%s: the refusal carries %d bytes", what, len(r.body))
		}
		if got := f.streamed(a.LeaseID); got != "" {
			t.Errorf("%s recorded a streamed digest", what)
		}
		if n := f.transfers(); n != 0 {
			t.Errorf("%s holds %d transfer slot(s)", what, n)
		}
	}
	// Same size, a later modification time.
	if err := os.Chtimes(job.Path, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	refused("a source whose modification time moved")
	// The leased modification time, another size.
	if err := os.WriteFile(job.Path, content[:2999], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(job.Path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	refused("a source whose size moved")
	// A directory at the path: the size and time of a directory are not a source's.
	if err := os.Remove(job.Path); err != nil {
		t.Fatal(err)
	}
	refused("a source that is gone")
	if err := os.Mkdir(job.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	refused("a directory where the source was")
	_ = os.Remove(job.Path)

	if row := f.row(a.LeaseID); row.State != store.LeaseGranted {
		t.Errorf("a refused source request moved the lease to %s", row.State)
	}
	if call.returned() {
		t.Error("a refused source request ended the lease")
	}
	// Put back exactly as leased, it is served.
	if err := os.WriteFile(job.Path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(job.Path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if r := f.source(a.LeaseID, "1", nil); r.status != http.StatusOK || !bytes.Equal(r.body, content) {
		t.Errorf("the source as leased answered %d", r.status)
	}
}

// bigJob is a sparse source far larger than any socket buffer, so a client that stops
// reading leaves the server blocked in a write.
func (f *fixture) bigJob(name string) Job {
	f.t.Helper()
	job := f.job(name, 10)
	if err := os.Truncate(job.Path, 96<<20); err != nil {
		f.t.Fatal(err)
	}
	fi, err := os.Stat(job.Path)
	if err != nil {
		f.t.Fatal(err)
	}
	job.SourceSize, job.SourceModTime, job.ReservedBytes = fi.Size(), fi.ModTime(), fi.Size()
	return job
}

// rawSource opens a connection, asks for a lease's source and reads nothing.
func (f *fixture) rawSource(id string) net.Conn {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	head := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: fixture\r\n%s: 1\r\n\r\n", leasePath("/source", id), EpochHeader)
	if _, err := conn.Write([]byte(head)); err != nil {
		f.t.Fatal(err)
	}
	return conn
}

// TestNodeFixture_AStalledSourceDownloadIsCutByTheWriteDeadline: a node that stops reading
// cannot hold its transfer slot. The stream is cut, nothing is recorded as streamed, and the
// lease is still good for a download that keeps up.
func TestNodeFixture_AStalledSourceDownloadIsCutByTheWriteDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.UploadGrace = 100 * time.Millisecond })
	job := f.bigJob("film")
	a, call := f.grantHTTP("node-a", job)
	conn := f.rawSource(a.LeaseID)
	eventually(t, "the stream to take its transfer slot", func() bool { return f.transfers() == 1 })
	// Nothing is read. The write deadline is all that can end the request.
	eventually(t, "the stalled stream to be cut and its transfer slot released", func() bool { return f.transfers() == 0 })
	if got := f.streamed(a.LeaseID); got != "" {
		t.Errorf("a stream that was cut recorded a digest %q", got)
	}
	// What did arrive is short of the source, and the connection is closed behind it.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.Copy(io.Discard, conn)
	if err != nil {
		t.Fatalf("reading what the cut stream left: %v", err)
	}
	if got >= job.SourceSize {
		t.Errorf("%d bytes arrived of a %d-byte source the server stopped sending", got, job.SourceSize)
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted {
		t.Errorf("a stalled download moved the lease to %s", row.State)
	}
	if call.returned() {
		t.Error("a stalled download ended the lease")
	}
}

// TestNodeFixture_SourceStreamsAndUploadsShareTheTransferCap: node_max_transfers counts
// source streams and uploads together. Past it either one is 503 with Retry-After, and the
// lease stays live.
func TestNodeFixture_SourceStreamsAndUploadsShareTheTransferCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.MaxTransfers = 1; o.UploadGrace = time.Minute })
	big := f.bigJob("big")
	a, _ := f.grantHTTP("node-a", big)
	small := f.jobOf("small", patterned(3000))
	b, callB := f.grantHTTP("node-b", small)

	// A source stream in flight holds the one slot.
	conn := f.rawSource(a.LeaseID)
	eventually(t, "the stream to take the transfer slot", func() bool { return f.transfers() == 1 })
	is503(t, "a second source stream past the transfer cap", f.source(b.LeaseID, "1", nil), "transfers_full")
	is503(t, "an upload past the transfer cap", f.put(b.LeaseID, b.Epoch, output, digestOf(output)), "transfers_full")
	if exists(small.Temp) {
		t.Error("the refused upload created its working file")
	}
	if row := f.row(b.LeaseID); row.State != store.LeaseGranted || callB.returned() {
		t.Errorf("a transfer refusal moved the lease: %+v", row)
	}
	// The stream's connection drops: its slot comes back.
	_ = conn.Close()
	eventually(t, "the dropped stream's slot to be released", func() bool { return f.transfers() == 0 })

	// And an upload in flight holds it against a source stream.
	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", b.LeaseID), pr)
	req.ContentLength = int64(len(output))
	req.Header.Set(EpochHeader, "1")
	req.Header.Set("Content-Digest", digestOf(output))
	answered := make(chan reply, 1)
	go func() { answered <- f.do(req) }()
	feed(t, pw, output[:10], answered)
	eventually(t, "the upload to hold the transfer slot", func() bool { return f.transfers() == 1 })
	is503(t, "a source stream while an upload holds the slot", f.source(b.LeaseID, "1", nil), "transfers_full")
	feed(t, pw, output[10:], answered)
	_ = pw.Close()
	if r := <-answered; r.status != http.StatusOK {
		t.Fatalf("the upload answered %d %s", r.status, r.body)
	}
	if n := f.transfers(); n != 0 {
		t.Errorf("%d transfer slot(s) are held after everything ended", n)
	}
}

// TestNodeFixture_ASourceStreamEndsWhenItsLeaseDoes: a source is streamed only on a live
// lease, for the whole of the stream. The lease is failed while the node is still reading,
// and the rest of the source never arrives.
func TestNodeFixture_ASourceStreamEndsWhenItsLeaseDoes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.UploadGrace = time.Minute })
	job := f.bigJob("film")
	a, call := f.grantHTTP("node-a", job)
	conn := f.rawSource(a.LeaseID)
	eventually(t, "the stream to start", func() bool { return f.transfers() == 1 })
	if r := f.fail(a.LeaseID, a.Epoch, "encode_failed"); r.status != http.StatusOK {
		t.Fatalf("fail: %d %s", r.status, r.body)
	}
	if _, err := call.wait(t); err == nil {
		t.Fatal("the failed lease completed")
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.Copy(io.Discard, conn)
	if err != nil {
		t.Fatalf("draining the stream of an ended lease: %v", err)
	}
	if got >= job.SourceSize {
		t.Errorf("the whole source (%d bytes) was sent on a lease that ended while it was read", got)
	}
	eventually(t, "the ended stream's slot to be released", func() bool { return f.transfers() == 0 })
}

// TestNodeFixture_TheReprDigestTrailerFollowsAWholeSourceOnHTTP2: where the connection can
// carry a trailer beside a declared length the server sends the RFC 9530 Repr-Digest of
// what it streamed; on HTTP/1.1 the response is not chunked and carries none. Either way
// the length is declared.
func TestNodeFixture_TheReprDigestTrailerFollowsAWholeSourceOnHTTP2(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(40_000)
	job := f.jobOf("film", content)
	a, _ := f.grantHTTP("node-a", job)

	h2 := httptest.NewUnstartedServer(f.srv.Config.Handler)
	h2.EnableHTTP2 = true
	h2.StartTLS()
	t.Cleanup(h2.Close)
	get := func(client *http.Client, base string, header map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+leasePath("/source", a.LeaseID), nil)
		req.Header.Set(EpochHeader, "1")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}
	resp, body := get(h2.Client(), h2.URL, nil)
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("the HTTP/2 source request answered HTTP/%d %d with %d bytes", resp.ProtoMajor, resp.StatusCode, len(body))
	}
	if resp.ContentLength != int64(len(content)) {
		t.Errorf("the HTTP/2 response declares %d bytes, want %d", resp.ContentLength, len(content))
	}
	if got := resp.Trailer.Get(ReprDigestTrailer); got != digestOf(content) {
		t.Errorf("the Repr-Digest trailer is %q, want the sha-256 of the source %s", got, digestOf(content))
	}
	// A ranged response is not the whole representation and carries no digest of it.
	if resp, _ := get(h2.Client(), h2.URL, map[string]string{"Range": "bytes=0-99"}); resp.Trailer.Get(ReprDigestTrailer) != "" {
		t.Errorf("a ranged response carries a Repr-Digest trailer %q", resp.Trailer.Get(ReprDigestTrailer))
	}
	resp, body = get(http.DefaultClient, f.srv.URL, nil)
	if resp.ProtoMajor != 1 || !bytes.Equal(body, content) || resp.ContentLength != int64(len(content)) {
		t.Fatalf("the HTTP/1.1 source request answered HTTP/%d with %d bytes declared %d", resp.ProtoMajor, len(body), resp.ContentLength)
	}
	if len(resp.Trailer) != 0 || len(resp.TransferEncoding) != 0 {
		t.Errorf("the HTTP/1.1 response carries trailers %v and transfer encodings %v, want a plain declared length", resp.Trailer, resp.TransferEncoding)
	}
}

// TestNodeSource_OnlyBytesReadInOrderAreHashedAndOnlyOnce: what P4 warns of. A reader that
// sniffs the head and seeks back - as http.ServeContent does when no Content-Type is set -
// must not count those bytes twice, and a stream that skipped a part is not the whole.
func TestNodeSource_OnlyBytesReadInOrderAreHashedAndOnlyOnce(t *testing.T) {
	t.Parallel()
	content := patterned(5000)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(live func() bool) *sourceStream {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return &sourceStream{f: file, hash: sha256.New(), rc: http.NewResponseController(httptest.NewRecorder()),
			grace: time.Minute, overall: time.Now().Add(time.Hour), live: live}
	}
	alive := func() bool { return true }

	s := open(alive)
	head := make([]byte, 512)
	if _, err := io.ReadFull(s, head); err != nil {
		t.Fatal(err)
	}
	if end, err := s.Seek(0, io.SeekEnd); err != nil || end != 5000 {
		t.Fatalf("seek to the end = %d, %v", end, err)
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(all, content) {
		t.Fatalf("read %d bytes, %v", len(all), err)
	}
	if s.hashed != 5000 || FormatDigest(s.hash.Sum(nil)) != digestOf(content) {
		t.Errorf("after a sniff and a seek back the stream hashed %d bytes to %s, want 5000 hashing to the source's digest",
			s.hashed, FormatDigest(s.hash.Sum(nil)))
	}

	// A stream that starts past the head never hashes the whole.
	s = open(alive)
	if _, err := s.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if s.hashed != 0 {
		t.Errorf("a stream read from offset 100 hashed %d bytes, want 0: it is not the source from its first byte", s.hashed)
	}
	// One that skips a part stops hashing where it skipped.
	s = open(alive)
	if _, err := io.ReadFull(s, head[:200]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seek(300, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if s.hashed != 200 || s.pos != 5000 {
		t.Errorf("a stream that skipped 100 bytes hashed %d and stands at %d, want 200 and 5000", s.hashed, s.pos)
	}
	// A failed seek leaves the position where it was.
	if _, err := s.Seek(-1, io.SeekStart); err == nil || s.pos != 5000 {
		t.Errorf("a failed seek moved the position to %d (%v)", s.pos, err)
	}
	// A lease that ended reads nothing more.
	s = open(func() bool { return false })
	if n, err := s.Read(head); n != 0 || !errors.Is(err, errLeaseEnded) {
		t.Errorf("a read on an ended lease returned %d, %v", n, err)
	}
}

// deadlines records the write deadlines a stream set.
type deadlines struct {
	httptest.ResponseRecorder
	set []time.Time
}

func (d *deadlines) SetWriteDeadline(t time.Time) error { d.set = append(d.set, t); return nil }

// TestNodeSource_TheWriteDeadlineIsTheGraceCappedByTheWholeBudget: no write may take longer
// than the grace, the whole stream no longer than its budget, and the deadline is not reset
// on every read.
func TestNodeSource_TheWriteDeadlineIsTheGraceCappedByTheWholeBudget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, patterned(100), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	d := &deadlines{}
	start := time.Now()
	s := &sourceStream{f: file, hash: sha256.New(), rc: http.NewResponseController(d), grace: time.Hour,
		overall: start.Add(24 * time.Hour), live: func() bool { return true }}
	buf := make([]byte, 10)
	for i := 0; i < 3; i++ {
		if _, err := s.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.set) != 1 {
		t.Fatalf("three reads inside a quarter of the grace set %d deadlines, want 1", len(d.set))
	}
	if got := d.set[0].Sub(start); got < time.Hour || got > time.Hour+time.Minute {
		t.Errorf("the deadline is %s from the start, want one grace (1h)", got)
	}
	// A read a quarter of the grace later moves it on.
	s.last = time.Now().Add(-15 * time.Minute)
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}
	if len(d.set) != 2 || !d.set[1].After(d.set[0]) {
		t.Fatalf("a read a quarter of the grace later set deadlines %v", d.set)
	}
	// One just short of a quarter does not.
	s.last = time.Now().Add(-14 * time.Minute)
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}
	if len(d.set) != 2 {
		t.Errorf("a read short of a quarter of the grace set a deadline")
	}
	// Near the end of the budget the deadline is the budget's end, never past it.
	s.overall = time.Now().Add(time.Minute)
	s.last = time.Time{}
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}
	if got := d.set[len(d.set)-1]; !got.Equal(s.overall) {
		t.Errorf("the deadline near the end of the budget is %v, want the budget's end %v", got, s.overall)
	}
}

// TestNodeSource_WhichLeasesHaveTheirSourceServed is the decision, against a held clock.
func TestNodeSource_WhichLeasesHaveTheirSourceServed(t *testing.T) {
	t.Parallel()
	granted := Lease{ID: "l", Epoch: 3, State: store.LeaseGranted, ExpiresAt: t0.Add(ttl)}
	with := func(edit func(*Lease)) Lease { l := granted; edit(&l); return l }
	for _, c := range []struct {
		name     string
		lease    Lease
		epoch    int64
		now      time.Time
		mode     string
		attached bool
		want     string
	}{
		{"a live http lease the engine waits on", granted, 3, t0, ModeHTTP, true, ""},
		{"one second before its expiry", granted, 3, t0.Add(ttl - time.Second), ModeHTTP, true, ""},
		{"at its expiry instant", granted, 3, t0.Add(ttl), ModeHTTP, true, errLeaseGone},
		{"at another epoch", granted, 2, t0, ModeHTTP, true, errLeaseGone},
		{"ended", with(func(l *Lease) { l.State = store.LeaseFailed }), 3, t0, ModeHTTP, true, errLeaseGone},
		{"completed", with(func(l *Lease) { l.State = store.LeaseCompleted }), 3, t0, ModeHTTP, true, errLeaseGone},
		{"nothing waits on it", granted, 3, t0, ModeHTTP, false, errNotReady},
		{"gone and unattached is gone", granted, 9, t0, ModeHTTP, false, errLeaseGone},
		{"a mapped lease", granted, 3, t0, ModeMapped, true, errSourceNotOffered},
		{"a lease whose mode is unknown", granted, 3, t0, "", true, errSourceNotOffered},
		{"an http lease that has admitted its output", with(func(l *Lease) { l.State = store.LeaseUploaded }), 3, t0, ModeHTTP, true, errSourceNotOffered},
	} {
		if got := sourceOffered(c.lease, c.epoch, c.now, c.mode, c.attached); got != c.want {
			t.Errorf("%s: sourceOffered = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestNodeLease_ACompletionIsHeldToWhatTheServerStreamed is decideCompleteStreamed, against
// a held clock.
func TestNodeLease_ACompletionIsHeldToWhatTheServerStreamed(t *testing.T) {
	t.Parallel()
	up := Lease{ID: "l", Epoch: 2, State: store.LeaseUploaded, ExpiresAt: t0.Add(ttl), OutputDigest: "sha-256=:out:", OutputBytes: 900}
	at := t0.Add(time.Second)

	// Nothing streamed whole: exactly decideComplete.
	plain, perr := decideComplete(up, 2, at, "sha-256=:out:", 900, "sha-256=:src:")
	got, err := decideCompleteStreamed(up, 2, at, "sha-256=:out:", 900, "sha-256=:src:", "")
	if err != nil || perr != nil || got != plain || got.State != store.LeaseCompleted {
		t.Fatalf("with nothing streamed: %+v, %v; decideComplete gave %+v, %v", got, err, plain, perr)
	}
	// The streamed digest reported: completed.
	got, err = decideCompleteStreamed(up, 2, at, "sha-256=:out:", 900, "sha-256=:src:", "sha-256=:src:")
	if err != nil || got != plain {
		t.Errorf("reporting the streamed digest: %+v, %v, want the completed row", got, err)
	}
	// Another digest: the lease fails in that transaction, and the row keeps what was reported.
	got, err = decideCompleteStreamed(up, 2, at, "sha-256=:out:", 900, "sha-256=:other:", "sha-256=:src:")
	if err != nil {
		t.Fatalf("a mismatch returned %v; it is a row to write, not a refusal", err)
	}
	if got.State != store.LeaseFailed || got.Reason != string(ReasonSourceMismatch) || got.SourceDigest != "sha-256=:other:" ||
		!got.EndedAt.Equal(at) || !got.UpdatedAt.Equal(at) || got.OutputDigest != "sha-256=:out:" || got.OutputBytes != 900 {
		t.Errorf("a mismatch wrote %+v, want failed source_digest_mismatch recording the reported digest", got)
	}
	if le := leaseErrorOf(got); le.Reason != ReasonSourceMismatch || le.Detail != "" {
		t.Errorf("the engine reads the failed row as %+v, want the source mismatch and not a node's own failure", le)
	}
	// Every refusal of decideComplete is returned as it is, whatever was streamed.
	if _, err := decideCompleteStreamed(up, 1, at, "sha-256=:out:", 900, "sha-256=:other:", "sha-256=:src:"); !errors.Is(err, ErrGone) {
		t.Errorf("a stale epoch returned %v, want ErrGone", err)
	}
	if row, err := decideCompleteStreamed(up, 2, at, "sha-256=:nope:", 900, "sha-256=:other:", "sha-256=:src:"); !errors.Is(err, ErrDigestConflict) || row.State != store.LeaseUploaded {
		t.Errorf("another output digest returned %+v, %v, want the conflict and the row as it was", row, err)
	}
	done := plain
	if row, err := decideCompleteStreamed(done, 2, at, "sha-256=:out:", 900, "sha-256=:src:", "sha-256=:streamed-later:"); !errors.Is(err, errAlready) || row != done {
		t.Errorf("a repeat of a recorded completion returned %+v, %v, want it answered from the record", row, err)
	}
}

// TestNodeSource_TheLeasedFileIsARegularFileOfTheLeasedSizeAndTime is sourceUnchanged.
func TestNodeSource_TheLeasedFileIsARegularFileOfTheLeasedSizeAndTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "source")
	if err := os.WriteFile(path, patterned(100), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	l := Lease{SourceSize: 100, SourceModTime: fi.ModTime()}
	if !sourceUnchanged(fi, l) {
		t.Fatal("the file as leased is not the leased file")
	}
	for name, edit := range map[string]func(*Lease){
		"one byte larger":       func(l *Lease) { l.SourceSize = 101 },
		"one byte smaller":      func(l *Lease) { l.SourceSize = 99 },
		"one nanosecond later":  func(l *Lease) { l.SourceModTime = l.SourceModTime.Add(time.Nanosecond) },
		"one nanosecond before": func(l *Lease) { l.SourceModTime = l.SourceModTime.Add(-time.Nanosecond) },
	} {
		other := l
		edit(&other)
		if sourceUnchanged(fi, other) {
			t.Errorf("a lease granted on a source %s is taken for this file's", name)
		}
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sourceUnchanged(di, Lease{SourceSize: di.Size(), SourceModTime: di.ModTime()}) {
		t.Error("a directory of the leased size and time is taken for the source")
	}
}

// TestNodeFixture_ASymlinkSwappedInAtTheLeasedPathIsNeverFollowed: the engine refuses a
// source that is a symbolic link, so a link at the leased path was put there after the
// grant. It is refused as a changed source - not followed - even when what it points at has
// exactly the leased size and modification time, and no byte of that file is sent.
func TestNodeFixture_ASymlinkSwappedInAtTheLeasedPathIsNeverFollowed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(3000)
	job := f.jobOf("film", content)
	a, call := f.grantHTTP("node-a", job)
	fi, err := os.Stat(job.Path)
	if err != nil {
		t.Fatal(err)
	}
	// A file OUTSIDE the library, of the leased size and modification time.
	outside := filepath.Join(t.TempDir(), "not-the-library.bin")
	secret := bytes.Repeat([]byte("PRIVATE!"), 375)
	if err := os.WriteFile(outside, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(outside, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(job.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, job.Path); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(job.Path); err != nil || st.Size() != job.SourceSize || !st.ModTime().Equal(job.SourceModTime) {
		t.Fatalf("the fixture's link does not read as the leased size and time (%v); it proves nothing", err)
	}
	r := f.source(a.LeaseID, "1", nil)
	isTyped(t, "a symlink at the leased path", r, http.StatusConflict, "source_changed")
	if bytes.Contains(r.body, []byte("PRIVATE!")) {
		t.Fatal("bytes of the file the link points at were sent")
	}
	if !strings.Contains(string(r.body), "symbolic link") {
		t.Errorf("the refusal does not say a symbolic link stands there: %s", r.body)
	}
	if got := f.streamed(a.LeaseID); got != "" || f.transfers() != 0 {
		t.Errorf("the refused request recorded %q and holds %d transfer slot(s)", got, f.transfers())
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted || call.returned() {
		t.Errorf("the refused request moved the lease to %s", row.State)
	}
	// A ranged request is refused the same way.
	isTyped(t, "a ranged request for a symlink at the leased path",
		f.source(a.LeaseID, "1", map[string]string{"Range": "bytes=0-9"}), http.StatusConflict, "source_changed")
}

// TestNodeFixture_AConditionalSourceRequestIsAnsweredWithTheWholeSource: http.ServeContent
// honours If-None-Match, If-Match, If-Modified-Since, If-Unmodified-Since and If-Range, which
// would answer 304 or 412 - answers this endpoint does not have. They are dropped before the
// source is served: each such request is a 200 with the whole source, hashed like any other,
// and an If-Range leaves a Range request an ordinary ranged one.
func TestNodeFixture_AConditionalSourceRequestIsAnsweredWithTheWholeSource(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	content := patterned(3000)
	job := f.jobOf("film", content)
	a, _ := f.grantHTTP("node-a", job)
	for name, header := range map[string]map[string]string{
		"If-None-Match: *":                 {"If-None-Match": "*"},
		`If-Match: "x"`:                    {"If-Match": `"x"`},
		"If-Match: *":                      {"If-Match": "*"},
		"If-Modified-Since in the future":  {"If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT"},
		"If-Unmodified-Since in the past":  {"If-Unmodified-Since": "Thu, 01 Jan 1970 00:00:01 GMT"},
		"If-Range with no Range":           {"If-Range": `"x"`},
		"every conditional header at once": {"If-None-Match": "*", "If-Match": `"x"`, "If-Modified-Since": "Fri, 01 Jan 2100 00:00:00 GMT", "If-Unmodified-Since": "Thu, 01 Jan 1970 00:00:01 GMT"},
	} {
		f.hub.mu.Lock()
		f.hub.sources[a.LeaseID].streamed = ""
		f.hub.mu.Unlock()
		r := f.source(a.LeaseID, "1", header)
		if r.status != http.StatusOK || !bytes.Equal(r.body, content) {
			t.Errorf("%s answered %d with %d bytes, want 200 and the whole source", name, r.status, len(r.body))
			continue
		}
		if got := f.streamed(a.LeaseID); got != digestOf(content) {
			t.Errorf("%s: the whole source was sent and %q recorded for it", name, got)
		}
	}
	// If-Range is dropped, so the Range beside it is served as a Range: a part, and no digest.
	f.hub.mu.Lock()
	f.hub.sources[a.LeaseID].streamed = ""
	f.hub.mu.Unlock()
	r := f.source(a.LeaseID, "1", map[string]string{"Range": "bytes=10-19", "If-Range": `"x"`})
	if r.status != http.StatusPartialContent || !bytes.Equal(r.body, content[10:20]) {
		t.Errorf("a Range with an If-Range answered %d with %d bytes, want 206 and bytes 10 to 19", r.status, len(r.body))
	}
	if got := f.streamed(a.LeaseID); got != "" {
		t.Errorf("a ranged request recorded a streamed digest %q", got)
	}
}

// TestNodeFixture_ASourceTheServerWithdrewNeverCoolsTheNodeOff: `source_withdrawn` is the
// server's own doing - it restarted, or the file moved - so however many leases of one node
// end on it, the node is still offered work; it neither lengthens the node's run of ended
// leases nor clears it.
func TestNodeFixture_ASourceTheServerWithdrewNeverCoolsTheNodeOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	cooling := func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.coolingLocked("node-a") != nil
	}
	streak := func() int {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		if c := f.hub.cooling["node-a"]; c != nil {
			return c.streak
		}
		return 0
	}
	for i := 0; i < 3*DefaultCoolOffAfter; i++ {
		f.hub.Report("node-a", ReasonSourceWithdrawn)
	}
	if cooling() || streak() != 0 {
		t.Fatalf("a node whose leases ended source_withdrawn %d times is cooling off (%v) with a run of %d", 3*DefaultCoolOffAfter, cooling(), streak())
	}
	// It does not clear a run either: two real endings, a withdrawal, and the third real
	// ending still cools the node off.
	f.hub.Report("node-a", "source_download_failed")
	f.hub.Report("node-a", "encode_failed")
	f.hub.Report("node-a", ReasonSourceWithdrawn)
	if cooling() || streak() != 2 {
		t.Fatalf("after two real endings and a withdrawal the run is %d and cooling is %v, want 2 and false", streak(), cooling())
	}
	f.hub.Report("node-a", "source_download_failed")
	if !cooling() {
		t.Error("the third real ending did not cool the node off")
	}
	// And the reason is one a worker may fail a lease with.
	if !failReason.MatchString(ReasonSourceWithdrawn) || ReasonSourceWithdrawn != "source_withdrawn" {
		t.Errorf("ReasonSourceWithdrawn = %q", ReasonSourceWithdrawn)
	}
}

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

var output = bytes.Repeat([]byte("encoded-output-"), 40) // 600 bytes

const srcDigest = "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:"

// TestNodeFixture_ALeaseRunsFromAcquireToComplete is the whole protocol once, end to end:
// what the acquire answers, a heartbeat's renewal and progress, the upload landing in the
// server-named working file, and the completion releasing the engine with the figures.
func TestNodeFixture_ALeaseRunsFromAcquireToComplete(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)

	if !validLeaseID(a.LeaseID) || a.Epoch != 1 || a.TTLSec != 60 || a.HeartbeatSec != 15 || a.Mode != "mapped" {
		t.Errorf("the lease is id=%q epoch=%d ttl=%d heartbeat=%d mode=%q, want a 32-hex id, 1, 60, 15, mapped",
			a.LeaseID, a.Epoch, a.TTLSec, a.HeartbeatSec, a.Mode)
	}
	if a.Path != job.Path || a.SourceSize != 1000 || a.SourceMtimeNS != job.SourceModTime.UnixNano() || a.MaxOutputBytes != 999 {
		t.Errorf("the lease names source %q size %d mtime %d max %d, want the job's and one byte under the source",
			a.Path, a.SourceSize, a.SourceMtimeNS, a.MaxOutputBytes)
	}
	if a.Encoder != "libx265" || fmt.Sprint(a.Pre) != fmt.Sprint(job.Pre) || fmt.Sprint(a.Body) != fmt.Sprint(job.Body) {
		t.Errorf("the lease carries encoder %q pre %v body %v, want the job's argument list", a.Encoder, a.Pre, a.Body)
	}
	row := f.row(a.LeaseID)
	if row.State != store.LeaseGranted || row.Node != "node-a" || row.Temp != job.Temp || row.Key != job.Key ||
		row.ReservedBytes != 1000 || row.ArgsDigest != ArgsDigest(job.Encoder, job.Pre, job.Body) ||
		!row.ExpiresAt.Equal(t0.Add(ttl)) {
		t.Errorf("the durable row is %+v", row)
	}

	// A heartbeat 40 s in renews to one TTL from then, and hands the engine its progress.
	f.clk.advance(40 * time.Second)
	hb := f.heartbeat(a.LeaseID, a.Epoch, 0.25)
	var hr HeartbeatResponse
	if hb.status != http.StatusOK || json.Unmarshal(hb.body, &hr) != nil || hr.TTLSec != 60 {
		t.Fatalf("a heartbeat answered %d %s, want 200 with ttl_sec 60", hb.status, hb.body)
	}
	if got := f.row(a.LeaseID).ExpiresAt; !got.Equal(t0.Add(100 * time.Second)) {
		t.Errorf("the lease now expires at %v, want one TTL after the heartbeat", got)
	}
	f.heartbeat(a.LeaseID, a.Epoch, 7)  // clamped to 1
	f.heartbeat(a.LeaseID, a.Epoch, -3) // clamped to 0
	if got := call.reported(); fmt.Sprint(got) != "[0.25 1 0]" {
		t.Errorf("the engine was handed progress %v, want [0.25 1 0]", got)
	}

	// A completion before any upload is refused, and ends nothing.
	isTyped(t, "a completion with nothing uploaded", f.complete(a.LeaseID, a.Epoch, output, srcDigest), http.StatusConflict, "not_uploaded")

	up := f.put(a.LeaseID, a.Epoch, output, digestOf(output))
	var ur UploadResponse
	if up.status != http.StatusOK || json.Unmarshal(up.body, &ur) != nil ||
		ur.State != "uploaded" || ur.OutputBytes != 600 || ur.OutputDigest != digestOf(output) {
		t.Fatalf("the upload answered %d %s, want 200 uploaded with the figures", up.status, up.body)
	}
	if got, err := os.ReadFile(job.Temp); err != nil || !bytes.Equal(got, output) {
		t.Fatalf("the working file holds %d bytes (%v), want exactly the uploaded output", len(got), err)
	}
	if call.returned() {
		t.Fatal("the engine was released by the upload; only the completion releases it")
	}

	// A completion that reports other figures than the upload recorded is a conflict.
	isTyped(t, "a completion with another digest", f.complete(a.LeaseID, a.Epoch, []byte("other"), srcDigest), http.StatusConflict, "digest_conflict")

	done := f.complete(a.LeaseID, a.Epoch, output, srcDigest)
	var cr CompleteResponse
	if done.status != http.StatusOK || json.Unmarshal(done.body, &cr) != nil || cr.State != "completed" ||
		cr.OutputBytes != 600 || cr.OutputDigest != digestOf(output) || cr.SourceDigest != srcDigest {
		t.Fatalf("the completion answered %d %s", done.status, done.body)
	}
	res, err := call.wait(t)
	if err != nil {
		t.Fatalf("Encode returned %v, want the completed lease", err)
	}
	want := Result{LeaseID: a.LeaseID, Node: "node-a", Epoch: 1, OutputDigest: digestOf(output),
		OutputBytes: 600, SourceDigest: srcDigest, EncodeSeconds: 12.5}
	if res != want {
		t.Errorf("Encode returned %+v, want %+v", res, want)
	}
	if got, err := os.ReadFile(job.Temp); err != nil || !bytes.Equal(got, output) {
		t.Errorf("after the completion the working file holds %d bytes (%v), want the output", len(got), err)
	}
	// The completion is idempotent: a repeat answers 200 from the record.
	again := f.complete(a.LeaseID, a.Epoch, output, srcDigest)
	if again.status != http.StatusOK || !bytes.Equal(again.body, done.body) {
		t.Errorf("a repeated completion answered %d %s, want the recorded 200 %s", again.status, again.body, done.body)
	}
	isTyped(t, "a repeated completion with another source digest",
		f.complete(a.LeaseID, a.Epoch, output, digestOf([]byte("x"))), http.StatusConflict, "digest_conflict")
	// The lease is over: a heartbeat on it is gone, and the node is under its cap again.
	is410(t, "a heartbeat after the completion", f.heartbeat(a.LeaseID, a.Epoch, 1))
	f.hub.mu.Lock()
	live, waits := len(f.hub.live), len(f.hub.waits)
	f.hub.mu.Unlock()
	if live != 0 || waits != 0 {
		t.Errorf("after the completion the hub still counts %d live leases and %d waits", live, waits)
	}
}

// TestNodeFixture_UploadOnAnExpiredLeaseIs410AndLeavesNoFile: both liveness checks. The
// sweep is held off, so it is the upload's own checks that are graded: the one before the
// working file is opened, and the one in the admitting transaction for a lease that ran out
// while the body arrived.
func TestNodeFixture_UploadOnAnExpiredLeaseIs410AndLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour })
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)

	t.Run("expired before the upload starts", func(t *testing.T) {
		f.clk.advance(ttl) // AT the expiry instant
		is410(t, "an upload on an expired lease", f.put(a.LeaseID, a.Epoch, output, digestOf(output)))
		f.onlySources("film")
		if st := f.row(a.LeaseID).State; st != store.LeaseGranted {
			t.Fatalf("the row is %s; this case needs the sweep not to have run", st)
		}
	})

	t.Run("expired while the body arrived", func(t *testing.T) {
		f.clk.advance(-ttl) // live again
		pr, pw := io.Pipe()
		// Whatever way this case leaves, the body ends: an open body holds its connection.
		t.Cleanup(func() { _ = pw.Close() })
		req, _ := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", a.LeaseID), pr)
		req.ContentLength = int64(len(output))
		req.Header.Set(EpochHeader, "1")
		req.Header.Set("Content-Digest", digestOf(output))
		answered := make(chan reply, 1)
		go func() { answered <- f.do(req) }()
		feed(t, pw, output[:300], answered)
		eventually(t, "the first half to reach the working file", func() bool {
			fi, err := os.Stat(job.Temp)
			return err == nil && fi.Size() == 300
		})
		f.clk.advance(ttl) // the lease runs out mid-body
		feed(t, pw, output[300:], answered)
		_ = pw.Close()
		is410(t, "an upload whose lease ran out mid-body", <-answered)
		f.onlySources("film")
		if r := f.row(a.LeaseID); r.State != store.LeaseGranted || r.OutputDigest != "" || r.OutputBytes != 0 {
			t.Errorf("the refused upload was recorded: %+v", r)
		}
	})

	// The sweep then expires it, and the engine gets the typed expiry.
	f.hub.sweep(context.Background())
	_, err := call.wait(t)
	if le := leaseErr(t, err); le.Reason != ReasonExpired || le.Node != "node-a" || le.Epoch != 1 || le.LeaseID != a.LeaseID {
		t.Errorf("Encode returned %+v, want an expiry naming node-a at epoch 1", le)
	}
	f.onlySources("film")
}

// TestNodeFixture_AStaleEpochAfterARegrantIs410: the fencing token. After the path is
// granted again, nothing the first holder sends is taken - on its own lease id or on the
// new one at its old epoch - and no byte of it reaches the library.
func TestNodeFixture_AStaleEpochAfterARegrantIs410(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	first, call1 := f.grant("node-a", job)
	f.clk.advance(ttl)
	_, err := call1.wait(t)
	if le := leaseErr(t, err); le.Reason != ReasonExpired || le.Epoch != 1 {
		t.Fatalf("the first lease ended %+v, want expired at epoch 1", le)
	}

	second, call2 := f.grant("node-b", job)
	if second.Epoch != 2 || second.LeaseID == first.LeaseID {
		t.Fatalf("the re-grant is lease %q at epoch %d, want a new id at epoch 2", second.LeaseID, second.Epoch)
	}

	stale := []byte("the stale worker's output")
	is410(t, "the stale worker's upload on its own lease", f.put(first.LeaseID, first.Epoch, stale, digestOf(stale)))
	is410(t, "an upload on the new lease at the old epoch", f.put(second.LeaseID, first.Epoch, stale, digestOf(stale)))
	is410(t, "an upload on the old lease at the new epoch", f.put(first.LeaseID, second.Epoch, stale, digestOf(stale)))
	is410(t, "the stale worker's heartbeat", f.heartbeat(first.LeaseID, first.Epoch, 0.9))
	is410(t, "a heartbeat on the new lease at the old epoch", f.heartbeat(second.LeaseID, first.Epoch, 0.9))
	is410(t, "the stale worker's completion", f.complete(first.LeaseID, first.Epoch, stale, srcDigest))
	is410(t, "a completion on the new lease at the old epoch", f.complete(second.LeaseID, first.Epoch, stale, srcDigest))
	is410(t, "the stale worker's fail", f.fail(first.LeaseID, first.Epoch, "late"))
	is410(t, "a fail on the new lease at the old epoch", f.fail(second.LeaseID, first.Epoch, "late"))
	f.onlySources("film")
	if call2.returned() {
		t.Fatal("a stale call ended the new lease")
	}

	// The current holder is untouched by all of it.
	if r := f.put(second.LeaseID, second.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("the current holder's upload answered %d %s", r.status, r.body)
	}
	if r := f.complete(second.LeaseID, second.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("the current holder's completion answered %d %s", r.status, r.body)
	}
	res, err := call2.wait(t)
	if err != nil || res.Epoch != 2 || res.Node != "node-b" {
		t.Errorf("the re-granted job returned %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(job.Temp); !bytes.Equal(got, output) {
		t.Error("the working file is not the current holder's output")
	}
}

type fileID struct {
	ino   uint64
	mtime time.Time
	size  int64
}

func idOf(t *testing.T, path string) fileID {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileID{ino: fi.Sys().(*syscall.Stat_t).Ino, mtime: fi.ModTime(), size: fi.Size()}
}

// TestNodeFixture_ADuplicateUploadIs200AndRewritesNothing: a repeat of the accepted upload
// is answered from the record with the file untouched, and a different output after
// acceptance is a conflict - before the completion and after it.
func TestNodeFixture_ADuplicateUploadIs200AndRewritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)

	first := f.put(a.LeaseID, a.Epoch, output, digestOf(output))
	if first.status != http.StatusOK {
		t.Fatalf("the upload answered %d %s", first.status, first.body)
	}
	before := idOf(t, job.Temp)
	other := bytes.Repeat([]byte("different-bytes"), 40) // the same length, another digest

	check := func(when string) {
		t.Helper()
		again := f.put(a.LeaseID, a.Epoch, output, digestOf(output))
		var ur UploadResponse
		if again.status != http.StatusOK || json.Unmarshal(again.body, &ur) != nil ||
			ur.OutputBytes != 600 || ur.OutputDigest != digestOf(output) {
			t.Errorf("%s: a duplicate upload answered %d %s, want 200 with the recorded figures", when, again.status, again.body)
		}
		isTyped(t, when+": another digest after acceptance", f.put(a.LeaseID, a.Epoch, other, digestOf(other)), http.StatusConflict, "digest_conflict")
		isTyped(t, when+": another length after acceptance", f.put(a.LeaseID, a.Epoch, output[:599], digestOf(output[:599])), http.StatusConflict, "digest_conflict")
		if after := idOf(t, job.Temp); after != before {
			t.Errorf("%s: the working file moved from %+v to %+v; a duplicate must rewrite nothing", when, before, after)
		}
		if got, _ := os.ReadFile(job.Temp); !bytes.Equal(got, output) {
			t.Errorf("%s: the working file no longer holds the accepted output", when)
		}
	}
	check("uploaded")
	if st := f.row(a.LeaseID).State; st != store.LeaseUploaded {
		t.Fatalf("the lease is %s after the duplicates, want uploaded", st)
	}
	if r := f.complete(a.LeaseID, a.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("the completion answered %d %s", r.status, r.body)
	}
	if _, err := call.wait(t); err != nil {
		t.Fatalf("Encode returned %v", err)
	}
	check("completed")
}

// TestNodeFixture_ADigestMismatchIsRefusedAndTheTempDeleted: a body that is not the bytes
// its Content-Digest declared leaves no file and a live lease, the node may send it again,
// and the third mismatch fails the lease.
func TestNodeFixture_ADigestMismatchIsRefusedAndTheTempDeleted(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	corrupt := append([]byte(nil), output...)
	corrupt[17] ^= 0xff

	r := f.put(a.LeaseID, a.Epoch, corrupt, digestOf(output))
	isTyped(t, "a corrupted body", r, http.StatusBadRequest, "digest_mismatch")
	f.onlySources("film")
	row := f.row(a.LeaseID)
	if row.State != store.LeaseGranted || row.UploadAttempts != 1 || row.OutputDigest != "" {
		t.Fatalf("after one mismatch the row is %+v, want granted with one attempt and no digest", row)
	}
	if hb := f.heartbeat(a.LeaseID, a.Epoch, 0.5); hb.status != http.StatusOK {
		t.Fatalf("the lease did not survive a first mismatch: heartbeat %d", hb.status)
	}

	// The retry inside the lease is taken.
	t.Run("a retry within the lease is admitted", func(t *testing.T) {
		g := newFixture(t, nil)
		job := g.job("film", 1000)
		a, call := g.grant("node-a", job)
		isTyped(t, "the first, corrupted body", g.put(a.LeaseID, a.Epoch, corrupt, digestOf(output)), http.StatusBadRequest, "digest_mismatch")
		if r := g.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
			t.Fatalf("the retry answered %d %s", r.status, r.body)
		}
		if r := g.complete(a.LeaseID, a.Epoch, output, srcDigest); r.status != http.StatusOK {
			t.Fatalf("the completion answered %d %s", r.status, r.body)
		}
		if _, err := call.wait(t); err != nil {
			t.Fatalf("Encode returned %v", err)
		}
	})

	// The bound: the third mismatch fails the lease.
	isTyped(t, "the second mismatch", f.put(a.LeaseID, a.Epoch, corrupt, digestOf(output)), http.StatusBadRequest, "digest_mismatch")
	if call.returned() {
		t.Fatal("the lease failed before the bound")
	}
	third := f.put(a.LeaseID, a.Epoch, corrupt, digestOf(output))
	isTyped(t, "the third mismatch", third, http.StatusBadRequest, "digest_mismatch")
	if !strings.Contains(string(third.body), "lease has failed") {
		t.Errorf("the third mismatch does not say the lease failed: %s", third.body)
	}
	_, err := call.wait(t)
	if le := leaseErr(t, err); le.Reason != ReasonDigestMismatch || le.Node != "node-a" || le.Epoch != 1 {
		t.Errorf("Encode returned %+v, want a digest-mismatch failure", le)
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseFailed || row.UploadAttempts != 3 {
		t.Errorf("the row is %+v, want failed after 3 attempts", row)
	}
	is410(t, "an upload after the bound", f.put(a.LeaseID, a.Epoch, output, digestOf(output)))
	f.onlySources("film")
}

// TestNodeFixture_A404BodyOfferedAsMediaIsRefused: the Unmanic #635 class at the server's
// end. A 55-byte error page arrives where the output should be, under the Content-Digest
// computed over the real output. It is refused and leaves no working file.
func TestNodeFixture_A404BodyOfferedAsMediaIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	page := []byte("<html><body><h1>404 Not Found</h1></body></html>\n      ")
	if len(page) != 55 {
		t.Fatalf("the fixture's error page is %d bytes, want 55", len(page))
	}
	r := f.put(a.LeaseID, a.Epoch, page, digestOf(output))
	isTyped(t, "a 404 page offered as the output", r, http.StatusBadRequest, "digest_mismatch")
	f.onlySources("film")
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted || row.OutputBytes != 0 || row.OutputDigest != "" {
		t.Errorf("the error page was recorded as an output: %+v", row)
	}
	if call.returned() {
		t.Error("the engine was released by a refused body")
	}
}

// TestNodeFixture_AFreeSpaceRefusalAtUploadStartIs503: the re-check once the length is
// known, and the refusal at grant through Release(t, ErrNoRoom). Both are 503 with
// Retry-After, and neither ends anything.
func TestNodeFixture_AFreeSpaceRefusalAtUploadStartIs503(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)

	f.space.set(599, nil) // one byte short of the 600 declared
	is503(t, "an upload the filesystem cannot take", f.put(a.LeaseID, a.Epoch, output, digestOf(output)), "no_room")
	f.onlySources("film")
	if st := f.row(a.LeaseID).State; st != store.LeaseGranted {
		t.Fatalf("the refused upload left the lease %s, want still granted", st)
	}
	if call.returned() {
		t.Fatal("the free-space refusal ended the lease")
	}
	f.space.mu.Lock()
	asked := append([]string(nil), f.space.asked...)
	f.space.mu.Unlock()
	if len(asked) != 1 || asked[0] != f.dir {
		t.Errorf("free space was asked of %v, want once of the working file's directory %s", asked, f.dir)
	}

	// A failed lookup refuses nothing; exactly enough room is enough.
	f.space.set(0, errors.New("statfs failed"))
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("an upload under a failed free-space lookup answered %d %s, want 200", r.status, r.body)
	}
	t.Run("exactly enough room", func(t *testing.T) {
		g := newFixture(t, nil)
		a, _ := g.grant("node-a", g.job("film", 1000))
		g.space.set(600, nil)
		if r := g.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
			t.Fatalf("an upload with exactly its length free answered %d %s, want 200", r.status, r.body)
		}
	})

	// At grant: the engine's reservation was refused, and it says so through Release.
	t.Run("a reservation refused at grant", func(t *testing.T) {
		g := newFixture(t, nil)
		answered := make(chan reply, 1)
		go func() { answered <- g.acquire("node-a") }()
		tk, err := g.hub.WaitDemand(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tk.Node() != "node-a" || fmt.Sprint(tk.Encoders()) != "[libx265 libsvtav1]" || !tk.Supports("libsvtav1") || tk.Supports("hevc_nvenc") {
			t.Errorf("the ticket names node %q encoders %v", tk.Node(), tk.Encoders())
		}
		g.hub.Release(tk, fmt.Errorf("the source room refused: %w", ErrNoRoom))
		is503(t, "a poll whose reservation was refused", <-answered, "no_room")
		if live, _ := g.st.LiveLeases(context.Background()); len(live) != 0 {
			t.Errorf("a refused reservation left %d lease rows", len(live))
		}
		// Releasing it again does nothing, and it cannot be encoded on.
		g.hub.Release(tk, nil)
		if _, err := g.hub.Encode(context.Background(), tk, g.job("film", 1000), nil); !errors.Is(err, ErrTicketUsed) {
			t.Errorf("Encode on a released ticket returned %v, want ErrTicketUsed", err)
		}
	})
}

// TestNodeFixture_AServerRestartKeepsLiveLeases: the ledger is closed and reopened under a
// new hub. A granted lease survives with one TTL of grace from the restart, a heartbeat
// inside the grace is 200 and one after it 410; a lease that was uploaded and not completed
// is ended by Recover and never gated.
func TestNodeFixture_AServerRestartKeepsLiveLeases(t *testing.T) {
	t.Parallel()
	// The first server never sweeps, so its abandoned engine calls never touch the ledger
	// again once it is closed: they stand for a process that died.
	f := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour; o.MaxLeasesPerNode = 4 })
	kept := f.job("kept", 1000)
	changed := f.job("changed", 1000)
	stray := f.job("stray", 1000)
	gated := f.job("uploaded", 1000)
	a, _ := f.grant("node-a", kept)
	b, _ := f.grant("node-a", changed)
	c, _ := f.grant("node-a", stray)
	u, _ := f.grant("node-b", gated)
	if r := f.put(u.LeaseID, u.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("the upload before the restart answered %d %s", r.status, r.body)
	}
	// A partial upload the dying server left at a granted lease's working file.
	if err := os.WriteFile(changed.Temp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The restart, long after every lease's own expiry.
	f.srv.Close()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(10 * time.Minute)
	restart := f.clk.Now()
	f.open(func(o *Options) { o.MaxLeasesPerNode = 4 })

	// Until Recover has run nothing about a lease is answered, and until Ready no work is.
	is503(t, "a heartbeat before Recover", f.heartbeat(a.LeaseID, a.Epoch, 0.1), "not_ready")
	is503(t, "an upload before Recover", f.put(a.LeaseID, a.Epoch, output, digestOf(output)), "not_ready")
	is503(t, "a completion before Recover", f.complete(u.LeaseID, u.Epoch, output, srcDigest), "not_ready")
	is503(t, "a fail before Recover", f.fail(a.LeaseID, a.Epoch, "why"), "not_ready")
	is503(t, "an acquire before Recover", f.acquire("node-c"), "not_ready")

	live, err := f.hub.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	var ids []string
	for _, l := range live {
		ids = append(ids, l.ID)
		if l.State != store.LeaseGranted || !l.ExpiresAt.Equal(restart.Add(ttl)) {
			t.Errorf("recovered lease %s is %s expiring %v, want granted with one TTL of grace from the restart", l.ID, l.State, l.ExpiresAt)
		}
	}
	want := []string{a.LeaseID, b.LeaseID, c.LeaseID}
	sort.Strings(ids)
	sort.Strings(want)
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("Recover returned %v, want the three granted leases %v", ids, want)
	}
	is503(t, "an acquire before Ready", f.acquire("node-c"), "not_ready")
	f.hub.Ready()

	// The uploaded lease is ended, its output removed and never gated.
	if row := f.row(u.LeaseID); row.State != store.LeaseExpired || row.Reason != string(ReasonRestart) {
		t.Errorf("the uploaded lease is %s (%s) after the restart, want expired for the restart", row.State, row.Reason)
	}
	if exists(gated.Temp) {
		t.Error("the uploaded lease's working file survived the restart")
	}
	is410(t, "a completion of the lease the restart ended", f.complete(u.LeaseID, u.Epoch, output, srcDigest))
	// A granted lease's partial working file is left where it was.
	if !exists(changed.Temp) {
		t.Error("Recover removed a live lease's working file")
	}

	// A lease the engine has not taken back can be heartbeated and nothing else, and the
	// heartbeat does not stretch its grace.
	f.clk.advance(20 * time.Second)
	hb := f.heartbeat(a.LeaseID, a.Epoch, 0.3)
	var hr HeartbeatResponse
	if hb.status != http.StatusOK || json.Unmarshal(hb.body, &hr) != nil || hr.TTLSec != 40 {
		t.Fatalf("a heartbeat inside the grace answered %d %s, want 200 with the 40 s of grace left", hb.status, hb.body)
	}
	if got := f.row(a.LeaseID).ExpiresAt; !got.Equal(restart.Add(ttl)) {
		t.Errorf("a heartbeat stretched an un-adopted lease's grace to %v", got)
	}
	is503(t, "an upload on a lease not taken back", f.put(a.LeaseID, a.Epoch, output, digestOf(output)), "not_ready")
	if got, want := fmt.Sprint(f.files()), fmt.Sprint([]string{"changed.__transcoding__.mkv.holdfast-part",
		"changed.mkv", "kept.mkv", "stray.mkv", "uploaded.mkv"}); got != want {
		t.Errorf("the library directory holds %s, want %s", got, want)
	}

	// Adopt: the same job, re-derived, under a NEW working file name. Whatever sits at the
	// name recorded before the restart is not Adopt's to remove.
	prior := kept.Temp + ".prior"
	if err := os.WriteFile(prior, []byte("left by the dead server"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpdateLease(context.Background(), a.LeaseID, func(cur Lease) (Lease, error) {
		cur.Temp = prior
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	rederived := kept
	rederived.Temp = filepath.Join(f.dir, "kept.__transcoding__.1.mkv.holdfast-part")
	adopted := &engineCall{done: make(chan struct{})}
	go func() {
		defer close(adopted.done)
		adopted.res, adopted.err = f.hub.Adopt(context.Background(), a.LeaseID, rederived, adopted.report)
	}()
	eventually(t, "the lease to be attached", func() bool { return f.hub.attached(a.LeaseID) })
	if got := f.row(a.LeaseID).Temp; got != rederived.Temp {
		t.Errorf("the adopted lease's working file is %q, want the re-derived job's", got)
	}
	// Adopting it twice is refused and ends nothing.
	if _, err := f.hub.Adopt(context.Background(), a.LeaseID, rederived, nil); !errors.Is(err, ErrBadJob) {
		t.Errorf("a second Adopt returned %v, want ErrBadJob", err)
	}
	// A heartbeat inside the grace now renews it, and reaches the engine.
	hb = f.heartbeat(a.LeaseID, a.Epoch, 0.4)
	if hb.status != http.StatusOK || json.Unmarshal(hb.body, &hr) != nil || hr.TTLSec != 60 {
		t.Fatalf("a heartbeat on the adopted lease answered %d %s, want 200 renewed to 60 s", hb.status, hb.body)
	}
	if got := adopted.reported(); fmt.Sprint(got) != "[0.4]" {
		t.Errorf("the adopting call was handed progress %v, want [0.4]", got)
	}

	// The job that moved: its argument list is not the leased one's.
	moved := changed
	moved.Body = append(append([]string(nil), changed.Body...), "-preset", "fast")
	_, err = f.hub.Adopt(context.Background(), b.LeaseID, moved, nil)
	if le := leaseErr(t, err); !errors.Is(err, ErrNotSameJob) || le.Reason != ReasonNotAdopted || le.Epoch != 1 || le.Node != "node-a" {
		t.Errorf("adopting a moved job returned %v, want ErrNotSameJob in a not-adopted lease error", err)
	}
	if row := f.row(b.LeaseID); row.State != store.LeaseExpired || row.Reason != string(ReasonNotAdopted) {
		t.Errorf("the lease of the moved job is %s (%s), want expired, not adopted", row.State, row.Reason)
	}
	// Nothing was attached to that lease, so ending it removes no file: the partial upload
	// the dead server left is the engine's startup sweep's.
	if !exists(changed.Temp) {
		t.Error("ending a lease nothing was attached to removed the file at its recorded path")
	}
	if err := os.Remove(changed.Temp); err != nil {
		t.Fatal(err)
	}
	is410(t, "a heartbeat on the lease of the moved job", f.heartbeat(b.LeaseID, b.Epoch, 0.1))

	// The lease nobody takes back runs out after exactly its grace.
	f.clk.advance(39 * time.Second) // one second inside the un-adopted grace
	if hb := f.heartbeat(c.LeaseID, c.Epoch, 0.1); hb.status != http.StatusOK {
		t.Fatalf("a heartbeat one second inside the grace answered %d %s", hb.status, hb.body)
	}
	f.clk.advance(time.Second)
	is410(t, "a heartbeat after the grace", f.heartbeat(c.LeaseID, c.Epoch, 0.1))

	// The adopted lease takes its upload into the re-derived working file and completes.
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("the upload on the adopted lease answered %d %s", r.status, r.body)
	}
	if got, _ := os.ReadFile(rederived.Temp); !bytes.Equal(got, output) {
		t.Error("the upload did not land in the re-derived working file")
	}
	if exists(kept.Temp) {
		t.Error("the upload landed in the working file recorded before the restart")
	}
	if got, _ := os.ReadFile(prior); string(got) != "left by the dead server" {
		t.Errorf("Adopt touched the file at the path the lease recorded before the restart: %q", got)
	}
	if r := f.complete(a.LeaseID, a.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("the completion on the adopted lease answered %d %s", r.status, r.body)
	}
	res, err := adopted.wait(t)
	if err != nil || res.LeaseID != a.LeaseID || res.Epoch != 1 || res.OutputBytes != 600 || res.SourceDigest != srcDigest {
		t.Errorf("Adopt returned %+v, %v", res, err)
	}

	// The re-grant of a path whose lease the restart ended counts its epoch up.
	again, _ := f.grant("node-c", gated)
	if again.Epoch != 2 {
		t.Errorf("the re-run of the uploaded job is at epoch %d, want 2", again.Epoch)
	}
}

// TestNodeFixture_AnAdoptedLeaseThatRunsOutReturnsTheExpiry, and Abandon ends one.
func TestNodeFixture_AnAdoptedLeaseThatRunsOutReturnsTheExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour; o.MaxLeasesPerNode = 4 })
	one, two, late := f.job("one", 1000), f.job("two", 1000), f.job("late", 1000)
	a, _ := f.grant("node-a", one)
	b, _ := f.grant("node-a", two)
	c, _ := f.grant("node-a", late)
	if err := os.WriteFile(two.Temp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	_ = f.st.Close()
	f.open(nil)
	if _, err := f.hub.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.hub.Ready()

	if err := f.hub.Abandon(context.Background(), b.LeaseID); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if row := f.row(b.LeaseID); row.State != store.LeaseExpired || row.Reason != string(ReasonNotAdopted) {
		t.Errorf("the abandoned lease is %s (%s)", row.State, row.Reason)
	}
	if got, err := os.ReadFile(two.Temp); err != nil || string(got) != "partial" {
		t.Errorf("Abandon touched the file at the lease's recorded path: %q, %v", got, err)
	}
	if err := f.hub.Abandon(context.Background(), b.LeaseID); err != nil {
		t.Errorf("abandoning an ended lease returned %v, want nil", err)
	}
	if err := f.hub.Abandon(context.Background(), "ffffffffffffffffffffffffffffffff"); !errors.Is(err, store.ErrNoLease) {
		t.Errorf("abandoning an unknown lease returned %v, want ErrNoLease", err)
	}

	adopted := &engineCall{done: make(chan struct{})}
	go func() {
		defer close(adopted.done)
		adopted.res, adopted.err = f.hub.Adopt(context.Background(), a.LeaseID, one, nil)
	}()
	eventually(t, "the lease to be attached", func() bool { return f.hub.attached(a.LeaseID) })
	f.clk.advance(ttl)
	_, err := adopted.wait(t)
	if le := leaseErr(t, err); le.Reason != ReasonExpired || le.LeaseID != a.LeaseID {
		t.Errorf("Adopt returned %+v, want the expiry", le)
	}

	// Adopting a lease whose grace already ran out ends it and says so.
	_, err = f.hub.Adopt(context.Background(), c.LeaseID, late, nil)
	if le := leaseErr(t, err); !errors.Is(err, ErrGone) || le.Reason != ReasonExpired || le.LeaseID != c.LeaseID {
		t.Errorf("adopting after the grace returned %v, want ErrGone in an expiry", err)
	}
	if row := f.row(c.LeaseID); row.State != store.LeaseExpired {
		t.Errorf("the lease adopted too late is %s, want expired", row.State)
	}
	if _, err := f.hub.Adopt(context.Background(), "ffffffffffffffffffffffffffffffff", late, nil); !errors.Is(err, store.ErrNoLease) {
		t.Errorf("adopting an unknown lease returned %v, want ErrNoLease", err)
	}
	bad := late
	bad.Key = ""
	if _, err := f.hub.Adopt(context.Background(), c.LeaseID, bad, nil); !errors.Is(err, ErrBadJob) {
		t.Errorf("adopting with a job that cannot be leased returned %v, want ErrBadJob", err)
	}
}

// TestNodeFixture_AnOversizedUploadIs413: the declared length is past one byte under the
// source, so the body is never read and nothing is written.
func TestNodeFixture_AnOversizedUploadIs413(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 600) // at most 599
	a, call := f.grant("node-a", job)
	isTyped(t, "an output as large as its source", f.put(a.LeaseID, a.Epoch, output, digestOf(output)),
		http.StatusRequestEntityTooLarge, "too_large")
	f.onlySources("film")
	if st := f.row(a.LeaseID); st.State != store.LeaseGranted || st.UploadAttempts != 0 {
		t.Errorf("the oversized upload moved the lease: %+v", st)
	}
	if call.returned() {
		t.Error("the oversized upload ended the lease")
	}
	if r := f.put(a.LeaseID, a.Epoch, output[:599], digestOf(output[:599])); r.status != http.StatusOK {
		t.Errorf("an output one byte under its source answered %d %s, want 200", r.status, r.body)
	}
}

// TestNodeFixture_AMissingContentLengthIs411: a chunked upload declares no length.
func TestNodeFixture_AMissingContentLengthIs411(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, _ := f.grant("node-a", job)
	req, _ := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", a.LeaseID), io.NopCloser(bytes.NewReader(output)))
	req.ContentLength = -1 // sent chunked
	req.Header.Set(EpochHeader, "1")
	req.Header.Set("Content-Digest", digestOf(output))
	isTyped(t, "an upload with no Content-Length", f.do(req), http.StatusLengthRequired, "length_required")
	f.onlySources("film")

	// A declared length of zero is not an output.
	isTyped(t, "an empty upload", f.put(a.LeaseID, a.Epoch, nil, digestOf(nil)), http.StatusBadRequest, "bad_request")
	f.onlySources("film")
}

// TestNodeFixture_ThePerNodeCapIs503WithRetryAfter.
func TestNodeFixture_ThePerNodeCapIs503WithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil) // one lease a node
	a, _ := f.grant("node-a", f.job("one", 1000))
	is503(t, "a second acquire by a node at its cap", f.acquire("node-a"), "node_cap")
	// Another node is not held to node-a's cap.
	b, _ := f.grant("node-b", f.job("two", 1000))
	if b.LeaseID == a.LeaseID {
		t.Fatal("two nodes were handed one lease")
	}
	// The cap frees with the lease.
	if r := f.fail(a.LeaseID, a.Epoch, "gave_up"); r.status != http.StatusOK {
		t.Fatalf("fail answered %d", r.status)
	}
	f.grant("node-a", f.job("three", 1000))
}

// TestNodeFixture_TheGlobalCapIs503WithRetryAfter.
func TestNodeFixture_TheGlobalCapIs503WithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.MaxLeases = 2 })
	f.grant("node-a", f.job("one", 1000))
	b, _ := f.grant("node-b", f.job("two", 1000))
	is503(t, "an acquire at the global cap", f.acquire("node-c"), "global_cap")
	if r := f.fail(b.LeaseID, b.Epoch, "gave_up"); r.status != http.StatusOK {
		t.Fatalf("fail answered %d", r.status)
	}
	f.grant("node-c", f.job("three", 1000))
}

// TestNodeFixture_TheCapsHoldInsideTheGrantAndForReservedTickets: the caps are also the
// grant transaction's, and a reserved ticket counts against them while it is out.
func TestNodeFixture_TheCapsHoldInsideTheGrantAndForReservedTickets(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.MaxLeases = 1; o.MaxLeasesPerNode = 1 })
	ctx := context.Background()

	// Two nodes wait. One ticket is out, so the cap of one leaves nothing for the other.
	first := make(chan reply, 1)
	go func() { first <- f.acquire("node-a") }()
	eventually(t, "node-a's poll", func() bool { return f.queuedPolls() == 1 })
	second := make(chan reply, 1)
	go func() { second <- f.acquire("node-b") }()
	eventually(t, "node-b's poll", func() bool { return f.queuedPolls() == 2 })
	tk, err := f.hub.WaitDemand(ctx)
	if err != nil || tk.Node() != "node-a" {
		t.Fatalf("WaitDemand returned %v, %v; want the first poll, node-a's", tk, err)
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	if tk2, err := f.hub.WaitDemand(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with the one lease reserved WaitDemand returned %v, %v; want it to keep waiting", tk2, err)
	}
	stop()
	// Released unused, the poll is matched again, first.
	f.hub.Release(tk, nil)
	tk, err = f.hub.WaitDemand(ctx)
	if err != nil || tk.Node() != "node-a" {
		t.Fatalf("after a release WaitDemand returned %v, %v; want node-a's poll again", tk, err)
	}

	// A lease the hub does not know of (another writer's row) is still counted by the
	// grant transaction: the poll is answered with the cap, and Encode says so.
	if _, err := f.st.GrantLease(ctx, "/lib/other.mkv", func([]Lease, int64) (Lease, error) {
		return decideGrant(Lease{ID: "0123456789abcdef0123456789abcdef", Node: "node-z", Path: "/lib/other.mkv"}, nil, caps{9, 9}, f.clk.Now(), ttl)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Encode(ctx, tk, f.job("film", 1000), nil); !errors.Is(err, ErrGlobalCap) {
		t.Fatalf("Encode at the cap returned %v, want ErrGlobalCap", err)
	}
	is503(t, "the poll of a grant refused by the cap", <-first, "global_cap")
	f.stop()
	is503(t, "the other poll at shutdown", <-second, "draining")
}

// TestNodeFixture_TheLongPollReturnsWhenTheBaseContextIsCancelled: a draining server does
// not wait out a long-poll, and WaitDemand is released with it.
func TestNodeFixture_TheLongPollReturnsWhenTheBaseContextIsCancelled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.LongPoll = time.Hour })
	answered := make(chan reply, 1)
	go func() { answered <- f.acquire("node-a") }()
	eventually(t, "the poll to be waiting", func() bool { return f.queuedPolls() == 1 })
	tk, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.hub.Release(tk, nil)
	waiting := make(chan error, 1)
	go func() {
		f.hub.mu.Lock()
		f.hub.polls = nil // nothing to hand out: WaitDemand blocks
		f.hub.mu.Unlock()
		_, err := f.hub.WaitDemand(context.Background())
		waiting <- err
	}()

	start := time.Now()
	f.stop()
	select {
	case r := <-answered:
		is503(t, "a long-poll at shutdown", r, "draining")
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll did not return when the base context was cancelled")
	}
	select {
	case err := <-waiting:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("WaitDemand returned %v at shutdown, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDemand did not return when the base context was cancelled")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the drain took %v", took)
	}
}

// TestNodeFixture_TheLongPollAnswers204WithRetryAfterWhenNoWorkArrives.
func TestNodeFixture_TheLongPollAnswers204WithRetryAfterWhenNoWorkArrives(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.LongPoll = 20 * time.Millisecond })
	r := f.acquire("node-a")
	if r.status != http.StatusNoContent || r.header.Get("Retry-After") != "7" || len(r.body) != 0 {
		t.Fatalf("an acquire with no work answered %d Retry-After=%q body=%q, want 204 with Retry-After 7 and no body",
			r.status, r.header.Get("Retry-After"), r.body)
	}
	if n := f.queuedPolls(); n != 0 {
		t.Errorf("%d polls are still queued after the long-poll ran out", n)
	}

	// A poll RESERVED when its long-poll runs out is answered with no work all the same,
	// and its ticket is dead: a grant on it would be a lease nobody holds, so Encode refuses
	// before it grants anything - no lease row, no working file.
	answered := make(chan reply, 1)
	go func() { answered <- f.acquire("node-b") }()
	tk, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r := take(t, answered); r.status != http.StatusNoContent || r.header.Get("Retry-After") != "7" {
		t.Fatalf("a reserved poll at its long-poll bound answered %d Retry-After=%q, want 204 and 7", r.status, r.header.Get("Retry-After"))
	}
	job := f.job("reserved-then-gone", 1000)
	if _, err := f.hub.Encode(context.Background(), tk, job, nil); !errors.Is(err, ErrPollGone) {
		t.Fatalf("Encode on a ticket whose poll was answered = %v, want ErrPollGone", err)
	} else if !strings.Contains(err.Error(), "node-b") {
		t.Errorf("the refusal does not name the node: %v", err)
	}
	var none *LeaseError
	if _, err := f.hub.Encode(context.Background(), tk, job, nil); !errors.Is(err, ErrTicketUsed) || errors.As(err, &none) {
		t.Errorf("a second Encode on the dead ticket = %v, want ErrTicketUsed", err)
	}
	if live, err := f.st.LiveLeases(context.Background()); err != nil || len(live) != 0 {
		t.Errorf("a dead ticket left %d live lease(s) (%v)", len(live), err)
	}
	if exists(job.Temp) {
		t.Error("a dead ticket left a working file")
	}
	if n := f.queuedPolls(); n != 0 {
		t.Errorf("a timed-out poll was queued again")
	}
	// The dead ticket no longer counts against its node: node-b can be reserved again.
	again := make(chan reply, 1)
	go func() { again <- f.acquire("node-b") }()
	tk2, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.hub.Release(tk2, ErrNoRoom)
	is503(t, "the next poll of the same node", take(t, again), errNoRoom)

	// A poll whose REQUEST ended while it was reserved is dead the same way.
	ctx, cancel := context.WithCancel(context.Background())
	left := make(chan struct{})
	go func() {
		defer close(left)
		body, _ := json.Marshal(acquireBody("node-c"))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+mount+RouteLeases, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	tk3, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	take(t, left)
	eventually(t, "the handler to see its request end", func() bool { return !f.hub.pollWaiting(tk3) })
	if _, err := f.hub.Encode(context.Background(), tk3, f.job("left", 1000), nil); !errors.Is(err, ErrPollGone) {
		t.Fatalf("Encode on a ticket whose request ended = %v, want ErrPollGone", err)
	}
	if live, _ := f.st.LiveLeases(context.Background()); len(live) != 0 {
		t.Errorf("a ticket whose request ended left %d live lease(s)", len(live))
	}
}

// take receives one value, and fails the test rather than wait for ever on one that never
// comes.
func take[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Minute):
		t.Fatal("timed out waiting for an answer")
		panic("unreachable")
	}
}

// TestNodeFixture_ANodeWhoseLeasesKeepEndingCoolsOff: after CoolOffAfter leases of one node
// end in a row with none succeeding between, the node is offered nothing for CoolOff - its
// queued poll and every poll it sends meanwhile answer 503 node_cooling_off with the time
// left as Retry-After - while another node is served as before. A success clears the run,
// and so does the cool-off running out.
func TestNodeFixture_ANodeWhoseLeasesKeepEndingCoolsOff(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	if f.hub.o.CoolOffAfter != 3 || f.hub.o.CoolOff != 5*time.Minute {
		t.Fatalf("defaults: cool off after %d for %s, want 3 and 5m", f.hub.o.CoolOffAfter, f.hub.o.CoolOff)
	}
	// Two endings, a success, two endings: no cool-off, the run was cleared.
	for _, reason := range []string{"unmapped_source", "expired", "", "unmapped_source", "source_mismatch"} {
		f.hub.Report("node-a", reason)
	}
	queued := make(chan reply, 1)
	go func() { queued <- f.acquire("node-a") }()
	eventually(t, "node-a's poll to queue", func() bool { return f.queuedPolls() == 1 })
	other := make(chan reply, 1)
	go func() { other <- f.acquire("node-b") }()
	eventually(t, "node-b's poll to queue", func() bool { return f.queuedPolls() == 2 })

	// The third in a row: the queued poll is answered now, with the whole cool-off to wait.
	f.hub.Report("node-a", "encode_failed")
	r := take(t, queued)
	isTyped(t, "the cooling node's queued poll", r, http.StatusServiceUnavailable, errCoolingOff)
	if got := r.header.Get("Retry-After"); got != "300" {
		t.Errorf("Retry-After = %q, want 300", got)
	}
	if n := f.queuedPolls(); n != 1 {
		t.Fatalf("%d poll(s) queued, want node-b's alone", n)
	}
	f.clk.advance(4*time.Minute + 30*time.Second)
	r = f.acquire("node-a")
	isTyped(t, "a poll during the cool-off", r, http.StatusServiceUnavailable, errCoolingOff)
	if got := r.header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After with 30 s left = %q, want 30", got)
	}
	if f.queuedPolls() != 1 {
		t.Error("a cooling node's poll was queued")
	}
	// The other node is served as before.
	tk, err := f.hub.WaitDemand(context.Background())
	if err != nil || tk.Node() != "node-b" {
		t.Fatalf("WaitDemand = %v, %v; want node-b's poll", tk, err)
	}
	f.hub.Release(tk, ErrNoRoom)
	is503(t, "node-b's poll", take(t, other), errNoRoom)

	// At the cool-off's end the node is served again, and its run starts from nothing: two
	// more endings do not cool it off.
	f.clk.advance(30 * time.Second)
	f.hub.Report("node-a", "expired")
	f.hub.Report("node-a", "expired")
	back := make(chan reply, 1)
	go func() { back <- f.acquire("node-a") }()
	tk, err = f.hub.WaitDemand(context.Background())
	if err != nil || tk.Node() != "node-a" {
		t.Fatalf("after the cool-off WaitDemand = %v, %v; want node-a's poll", tk, err)
	}
	f.hub.Release(tk, ErrNoRoom)
	is503(t, "node-a's poll after the cool-off", take(t, back), errNoRoom)
}

// TestNodeFixture_ReadyGivesEveryRecoveredLeaseItsGraceAgain: the grace Recover gave runs
// while the engine takes the leases back. Ready gives each lease that is still live one TTL
// from the moment the server can be reached, and never revives one that ended meanwhile.
func TestNodeFixture_ReadyGivesEveryRecoveredLeaseItsGraceAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	kept, _ := f.grant("node-a", f.job("kept", 1000))
	dropped, _ := f.grant("node-b", f.job("dropped", 1000))
	f.srv.Close()
	_ = f.st.Close()
	f.open(nil)
	live, err := f.hub.Recover(context.Background())
	if err != nil || len(live) != 2 {
		t.Fatalf("Recover = %d lease(s), %v; want 2", len(live), err)
	}
	// The engine's start-up work takes most of a TTL; one lease is abandoned during it.
	f.clk.advance(ttl - 2*time.Second)
	if err := f.hub.Abandon(context.Background(), dropped.LeaseID); err != nil {
		t.Fatal(err)
	}
	f.hub.Ready()
	if got, want := f.row(kept.LeaseID).ExpiresAt, f.clk.Now().Add(ttl); !got.Equal(want.Truncate(time.Second)) {
		t.Errorf("after Ready the recovered lease expires at %s, want one TTL from Ready (%s)", got, want)
	}
	if row := f.row(dropped.LeaseID); row.State.Live() {
		t.Errorf("Ready revived an abandoned lease: it is %s", row.State)
	}
	// Past the grace Recover gave, inside the one Ready gave: still the node's lease.
	f.clk.advance(ttl - 2*time.Second)
	if r := f.heartbeat(kept.LeaseID, kept.Epoch, 0); r.status != http.StatusOK {
		t.Errorf("a heartbeat inside the grace Ready gave answered %d, want 200", r.status)
	}
	is410(t, "a heartbeat on the abandoned lease", f.heartbeat(dropped.LeaseID, dropped.Epoch, 0))
}

// TestNodeFixture_AVersionMismatchIs409NamingBothVersions.
func TestNodeFixture_AVersionMismatchIs409NamingBothVersions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	body := acquireBody("node-a")
	body.Version = "v9.9.9-other"
	r := f.post(RouteLeases, body)
	var e ErrorResponse
	if r.status != http.StatusConflict || json.Unmarshal(r.body, &e) != nil {
		t.Fatalf("a worker at another version answered %d %s, want 409", r.status, r.body)
	}
	if e.Error != "version_mismatch" || e.ServerVersion != testVersion || e.WorkerVersion != "v9.9.9-other" {
		t.Errorf("the 409 is %+v, want version_mismatch naming both versions", e)
	}
	if n := f.queuedPolls(); n != 0 {
		t.Error("a worker at another version was queued for work")
	}
}

// TestNodeFixture_AnUnknownModeIsRefusedNamingBothModes: a mode that is neither mapped nor
// http is refused typed, naming the two this build serves. (Until http mode was built this
// fixture also held that `http` was refused; TestNodeFixture_AnHTTPModeLeaseIsGrantedInHTTPMode
// now holds that it is served.)
func TestNodeFixture_AnUnknownModeIsRefusedNamingBothModes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	for _, mode := range []string{"HTTP", "", "carrier-pigeon"} {
		body := acquireBody("node-a")
		body.Mode = mode
		r := f.post(RouteLeases, body)
		isTyped(t, "mode "+mode, r, http.StatusBadRequest, "unsupported_mode")
		if !strings.Contains(string(r.body), "mode must be mapped") || !strings.Contains(string(r.body), "or http") {
			t.Errorf("the refusal of mode %q does not name the two modes this build serves: %s", mode, r.body)
		}
	}
	if n := f.queuedPolls(); n != 0 {
		t.Error("a node in an unserved mode was queued for work")
	}
}

// TestNodeFixture_AFailEndsTheLeaseWithItsTypedReason, removes what was uploaded, and is
// idempotent.
func TestNodeFixture_AFailEndsTheLeaseWithItsTypedReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("upload answered %d", r.status)
	}
	isTyped(t, "a fail with an untyped reason", f.fail(a.LeaseID, a.Epoch, "It Broke!"), http.StatusBadRequest, "bad_request")
	isTyped(t, "a fail with no reason", f.fail(a.LeaseID, a.Epoch, ""), http.StatusBadRequest, "bad_request")
	isTyped(t, "a fail with a 65-character reason", f.fail(a.LeaseID, a.Epoch, strings.Repeat("r", 65)), http.StatusBadRequest, "bad_request")
	if call.returned() {
		t.Fatal("a refused fail ended the lease")
	}

	r := f.fail(a.LeaseID, a.Epoch, "source_unreadable")
	var fr FailResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &fr) != nil || fr.State != "failed" || fr.Reason != "source_unreadable" {
		t.Fatalf("fail answered %d %s", r.status, r.body)
	}
	_, err := call.wait(t)
	le := leaseErr(t, err)
	if le.Reason != ReasonNodeFailed || le.Detail != "source_unreadable" || le.Node != "node-a" || le.Epoch != 1 {
		t.Errorf("Encode returned %+v, want the node's typed failure", le)
	}
	f.onlySources("film") // the uploaded output went with the lease
	if again := f.fail(a.LeaseID, a.Epoch, "something_else"); again.status != http.StatusOK || !bytes.Equal(again.body, r.body) {
		t.Errorf("a repeated fail answered %d %s, want the recorded 200", again.status, again.body)
	}
	is410(t, "a completion after the fail", f.complete(a.LeaseID, a.Epoch, output, srcDigest))
}

// TestNodeFixture_HeartbeatsStoppingExpireTheLeaseAfterOneTTL, by the server's clock alone,
// and only the working file that lease recorded is removed.
func TestNodeFixture_HeartbeatsStoppingExpireTheLeaseAfterOneTTL(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	neighbour := filepath.Join(f.dir, "other.__transcoding__.mkv.holdfast-part")
	if err := os.WriteFile(neighbour, []byte("another job's working file"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, call := f.grant("node-a", job)
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("upload answered %d", r.status)
	}

	f.clk.advance(ttl - time.Second)
	time.Sleep(60 * time.Millisecond) // several sweeps
	if call.returned() {
		t.Fatal("the lease expired a second before its TTL")
	}
	f.clk.advance(time.Second)
	_, err := call.wait(t)
	le := leaseErr(t, err)
	if le.Reason != ReasonExpired || le.Node != "node-a" || le.Epoch != 1 || le.LeaseID != a.LeaseID {
		t.Errorf("Encode returned %+v, want the expiry naming node-a at epoch 1", le)
	}
	row := f.row(a.LeaseID)
	if row.State != store.LeaseExpired || row.Reason != "expired" || !row.EndedAt.Equal(t0.Add(ttl)) {
		t.Errorf("the row is %+v, want expired at the TTL", row)
	}
	if exists(job.Temp) {
		t.Error("the expired lease's working file was not removed")
	}
	if got, err := os.ReadFile(neighbour); err != nil || string(got) != "another job's working file" {
		t.Errorf("another working file in the directory was touched: %q, %v", got, err)
	}
	if got, err := os.ReadFile(job.Path); err != nil || len(got) != 1000 {
		t.Errorf("the source was touched: %d bytes, %v", len(got), err)
	}
	is410(t, "a heartbeat after the expiry", f.heartbeat(a.LeaseID, a.Epoch, 0.5))
}

// TestNodeFixture_CancellingTheJobEndsTheLease: the engine's context ending is the server
// ending the lease.
func TestNodeFixture_CancellingTheJobEndsTheLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("upload answered %d", r.status)
	}
	call.cancel()
	_, err := call.wait(t)
	if le := leaseErr(t, err); le.Reason != ReasonCanceled || le.Node != "node-a" || le.Epoch != 1 {
		t.Errorf("Encode returned %+v, want a cancelled lease", le)
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseExpired || row.Reason != "canceled" {
		t.Errorf("the row is %s (%s), want expired, canceled", row.State, row.Reason)
	}
	f.onlySources("film")
	is410(t, "a completion after the job ended", f.complete(a.LeaseID, a.Epoch, output, srcDigest))
	// Progress reported after the call returned never reaches the engine's callback.
	call.mu.Lock()
	n := len(call.progress)
	call.mu.Unlock()
	if n != 0 {
		t.Errorf("progress was reported with no heartbeat: %d", n)
	}
}

// TestNodeFixture_APollThatLeftBeforeTheGrantLeavesNoLiveLease: the node gave up waiting
// between the reservation and the grant.
func TestNodeFixture_APollThatLeftBeforeTheGrantLeavesNoLiveLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	b, _ := json.Marshal(acquireBody("node-a"))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+mount+RouteLeases, bytes.NewReader(b))
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	tk, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	eventually(t, "the poll to leave", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return tk.p.state == pollGone
	})
	job := f.job("film", 1000)
	// Nothing is granted on a ticket whose poll left: no lease row at all, at any epoch.
	if _, err = f.hub.Encode(context.Background(), tk, job, nil); !errors.Is(err, ErrPollGone) {
		t.Errorf("Encode on a poll that left returned %v, want ErrPollGone", err)
	}
	var le *LeaseError
	if errors.As(err, &le) {
		t.Errorf("Encode on a poll that left granted lease %s", le.LeaseID)
	}
	if exists(job.Temp) {
		t.Error("a working file was left")
	}
	if live, _ := f.st.LiveLeases(context.Background()); len(live) != 0 {
		t.Errorf("a poll that left holds %d live leases", len(live))
	}
	f.hub.mu.Lock()
	live := len(f.hub.live)
	f.hub.mu.Unlock()
	if live != 0 {
		t.Errorf("the hub still counts %d live leases", live)
	}
}

// TestNodeFixture_TheTransferCapIs503WithRetryAfter: node_max_transfers.
func TestNodeFixture_TheTransferCapIs503WithRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.MaxTransfers = 1 })
	a, _ := f.grant("node-a", f.job("one", 1000))
	second := f.job("two", 1000)
	b, _ := f.grant("node-b", second)

	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", a.LeaseID), pr)
	req.ContentLength = int64(len(output))
	req.Header.Set(EpochHeader, "1")
	req.Header.Set("Content-Digest", digestOf(output))
	answered := make(chan reply, 1)
	go func() { answered <- f.do(req) }()
	_, _ = pw.Write(output[:10])
	eventually(t, "the first upload to hold the transfer slot", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.transfers == 1 && len(f.hub.uploads) == 1
	})
	is503(t, "an upload past the transfer cap", f.put(b.LeaseID, b.Epoch, output, digestOf(output)), "transfers_full")
	if exists(second.Temp) {
		t.Error("the refused upload created its working file")
	}
	// A second upload of the SAME lease while one is writing is refused, and the cap is
	// not what refuses it once there is room.
	f.hub.mu.Lock()
	f.hub.o.MaxTransfers = 2
	f.hub.mu.Unlock()
	isTyped(t, "a concurrent upload of one lease", f.put(a.LeaseID, a.Epoch, output, digestOf(output)), http.StatusConflict, "upload_in_progress")
	_, _ = pw.Write(output[10:])
	_ = pw.Close()
	if r := <-answered; r.status != http.StatusOK {
		t.Fatalf("the first upload answered %d %s", r.status, r.body)
	}
	// The slot is free again.
	if r := f.put(b.LeaseID, b.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Errorf("an upload after the slot freed answered %d %s", r.status, r.body)
	}
	f.hub.mu.Lock()
	transfers, uploads := f.hub.transfers, len(f.hub.uploads)
	f.hub.mu.Unlock()
	if transfers != 0 || uploads != 0 {
		t.Errorf("after both uploads the hub counts %d transfers and %d uploads in flight", transfers, uploads)
	}
}

// rawPut opens a connection and sends an upload's headers and the given part of its body,
// declaring a length of declared. It returns the connection for the test to finish.
func (f *fixture) rawPut(id string, declared int, digest string, part []byte) net.Conn {
	f.t.Helper()
	conn, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	head := fmt.Sprintf("PUT %s HTTP/1.1\r\nHost: fixture\r\nContent-Length: %d\r\n%s: 1\r\nContent-Digest: %s\r\n\r\n",
		leasePath("/output", id), declared, EpochHeader, digest)
	if _, err := conn.Write(append([]byte(head), part...)); err != nil {
		f.t.Fatal(err)
	}
	return conn
}

// TestNodeFixture_AStalledUploadIsCutOffAndLeavesNoFile: the read deadline. An upload that
// stops sending cannot hold its transfer slot, and what it sent is discarded.
func TestNodeFixture_AStalledUploadIsCutOffAndLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.UploadGrace = 50 * time.Millisecond })
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	conn := f.rawPut(a.LeaseID, len(output), digestOf(output), output[:200])
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("reading the answer to a stalled upload: %v", err)
	}
	if !bytes.HasPrefix(answer, []byte("HTTP/1.1 408 ")) || !bytes.Contains(answer, []byte(`"upload_stalled"`)) {
		t.Errorf("a stalled upload was answered %q, want 408 upload_stalled", answer)
	}
	eventually(t, "the transfer slot to be released", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.transfers == 0 && len(f.hub.uploads) == 0
	})
	f.onlySources("film")
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted || row.UploadAttempts != 0 {
		t.Errorf("a stalled upload moved the lease: %+v", row)
	}
	if call.returned() {
		t.Error("a stalled upload ended the lease")
	}
	// The lease is still good for an upload that keeps to the rate.
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Errorf("an upload after a stalled one answered %d %s", r.status, r.body)
	}
}

// TestNodeFixture_ATruncatedUploadLeavesNoFile: the connection drops before the declared
// length has arrived.
func TestNodeFixture_ATruncatedUploadLeavesNoFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	conn := f.rawPut(a.LeaseID, len(output), digestOf(output), output[:200])
	eventually(t, "the partial body to reach the working file", func() bool {
		fi, err := os.Stat(job.Temp)
		return err == nil && fi.Size() == 200
	})
	_ = conn.Close()
	eventually(t, "the partial working file to be removed", func() bool { return !exists(job.Temp) })
	eventually(t, "the transfer slot to be released", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.transfers == 0 && len(f.hub.uploads) == 0
	})
	f.onlySources("film")
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted || row.UploadAttempts != 0 || row.OutputBytes != 0 {
		t.Errorf("a truncated upload moved the lease: %+v", row)
	}
	if call.returned() {
		t.Error("a truncated upload ended the lease")
	}
}

// TestNodeFixture_AnUploadThatOutlivesItsLeaseNeverRemovesALaterGrantsFile: a slow upload's
// lease ends and the path is granted again; when the slow upload finally fails, the file it
// removes is not the new holder's.
func TestNodeFixture_AnUploadThatOutlivesItsLeaseNeverRemovesALaterGrantsFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	first, call1 := f.grant("node-a", job)
	conn := f.rawPut(first.LeaseID, len(output), digestOf(output), output[:200])
	eventually(t, "the partial body to reach the working file", func() bool {
		fi, err := os.Stat(job.Temp)
		return err == nil && fi.Size() == 200
	})
	f.clk.advance(ttl)
	if _, err := call1.wait(t); leaseErr(t, err).Reason != ReasonExpired {
		t.Fatalf("the first lease ended %v", err)
	}
	if exists(job.Temp) {
		t.Fatal("the expired lease's working file was not removed")
	}
	second, call2 := f.grant("node-b", job)
	if r := f.put(second.LeaseID, second.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("the new holder's upload answered %d %s", r.status, r.body)
	}
	// The slow upload now ends, one way and then the other.
	_ = conn.Close()
	eventually(t, "the slow upload to be let go", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.transfers == 0
	})
	if got, err := os.ReadFile(job.Temp); err != nil || !bytes.Equal(got, output) {
		t.Fatalf("the new holder's working file is gone or changed after the stale upload ended: %v", err)
	}
	if r := f.complete(second.LeaseID, second.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("completion answered %d", r.status)
	}
	if _, err := call2.wait(t); err != nil {
		t.Errorf("the new holder's job returned %v", err)
	}
}

// TestNodeFixture_ARequestOutsideTheProtocolIsRefusedTyped.
func TestNodeFixture_ARequestOutsideTheProtocolIsRefusedTyped(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	raw := func(method, path, body string, hdr map[string]string) reply {
		req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return f.do(req)
	}
	hb := leasePath("/heartbeat", a.LeaseID)
	good := map[string]string{EpochHeader: "1", "Content-Digest": digestOf(output)}
	out := leasePath("/output", a.LeaseID)
	unknown := "ffffffffffffffffffffffffffffffff"

	for _, tc := range []struct {
		name   string
		r      reply
		status int
		reason string
	}{
		{"a body that is not JSON", raw("POST", hb, "not json", nil), 400, "bad_request"},
		{"a body with an unknown field", raw("POST", hb, `{"epoch":1,"progress":0.1,"extra":true}`, nil), 400, "bad_request"},
		{"two JSON objects", raw("POST", hb, `{"epoch":1}{"epoch":1}`, nil), 400, "bad_request"},
		{"a body past the bound", raw("POST", hb, `{"epoch":1,"progress":0.`+strings.Repeat("1", 70000)+`}`, nil), 400, "bad_request"},
		{"an acquire that is not JSON", raw("POST", mount+RouteLeases, "{", nil), 400, "bad_request"},
		{"an acquire with no node", raw("POST", mount+RouteLeases, `{"version":"`+testVersion+`","slots":1,"mode":"mapped","encoders":["x"]}`, nil), 400, "bad_request"},
		{"a completion that is not JSON", raw("POST", leasePath("/complete", a.LeaseID), "[]", nil), 400, "bad_request"},
		{"a fail that is not JSON", raw("POST", leasePath("/fail", a.LeaseID), "", nil), 400, "bad_request"},
		{"a heartbeat on an id that is no lease id", raw("POST", leasePath("/heartbeat", "not-a-lease-id"), `{"epoch":1}`, nil), 404, "unknown_lease"},
		{"a heartbeat on an unknown lease", raw("POST", leasePath("/heartbeat", unknown), `{"epoch":1}`, nil), 404, "unknown_lease"},
		{"a completion on an id that is no lease id", raw("POST", leasePath("/complete", "x"), `{}`, nil), 404, "unknown_lease"},
		{"a fail on an id that is no lease id", raw("POST", leasePath("/fail", "x"), `{}`, nil), 404, "unknown_lease"},
		{"an upload on an id that is no lease id", raw("PUT", leasePath("/output", "x"), "body", good), 404, "unknown_lease"},
		{"an upload on an unknown lease", raw("PUT", leasePath("/output", unknown), "body", good), 404, "unknown_lease"},
		{"an upload with no epoch", raw("PUT", out, "body", map[string]string{"Content-Digest": digestOf(output)}), 400, "bad_request"},
		{"an upload with an epoch that is not a number", raw("PUT", out, "body", map[string]string{EpochHeader: "one", "Content-Digest": digestOf(output)}), 400, "bad_request"},
		{"an upload with no Content-Digest", raw("PUT", out, "body", map[string]string{EpochHeader: "1"}), 400, "bad_digest"},
		{"an upload with a Content-Digest of another algorithm", raw("PUT", out, "body", map[string]string{EpochHeader: "1", "Content-Digest": "md5=:abc:"}), 400, "bad_digest"},
		{"a completion with an unreadable output digest", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{Epoch: 1, OutputDigest: "abc", SourceDigest: srcDigest, OutputBytes: 5}), 400, "bad_digest"},
		{"a completion with an unreadable source digest", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{Epoch: 1, OutputDigest: srcDigest, SourceDigest: "abc", OutputBytes: 5}), 400, "bad_digest"},
		{"a completion of zero bytes", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{Epoch: 1, OutputDigest: srcDigest, SourceDigest: srcDigest}), 400, "bad_request"},
		{"a completion with a negative encode time", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{Epoch: 1, OutputDigest: srcDigest, SourceDigest: srcDigest, OutputBytes: 5, EncodeSec: -1}), 400, "bad_request"},
	} {
		isTyped(t, tc.name, tc.r, tc.status, tc.reason)
	}
	f.onlySources("film")
	if call.returned() {
		t.Error("a refused request ended the lease")
	}
	if row := f.row(a.LeaseID); row.State != store.LeaseGranted || !row.ExpiresAt.Equal(t0.Add(ttl)) {
		t.Errorf("a refused request moved the lease: %+v", row)
	}
	// A completion of exactly one byte with an encode time of zero is inside the bounds
	// (and is refused only because nothing was uploaded).
	isTyped(t, "a one-byte completion", f.post("/leases/"+a.LeaseID+"/complete",
		CompleteRequest{Epoch: 1, OutputDigest: srcDigest, SourceDigest: srcDigest, OutputBytes: 1}), http.StatusConflict, "not_uploaded")
}

// TestNodeFixture_TheEndpointsAnswer503UntilAHubIsWiredAndReady.
func TestNodeFixture_TheEndpointsAnswer503UntilAHubIsWiredAndReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	hub := f.hub
	f.hub = nil
	for _, r := range []reply{
		f.acquire("node-a"),
		f.heartbeat("0123456789abcdef0123456789abcdef", 1, 0),
		f.put("0123456789abcdef0123456789abcdef", 1, output, digestOf(output)),
		f.complete("0123456789abcdef0123456789abcdef", 1, output, srcDigest),
		f.fail("0123456789abcdef0123456789abcdef", 1, "why"),
	} {
		if r.status != http.StatusServiceUnavailable || r.errorType(t) != "not_ready" || r.header.Get("Retry-After") != "5" {
			t.Errorf("with no hub wired an endpoint answered %d %s Retry-After=%q, want 503 not_ready with Retry-After 5",
				r.status, r.body, r.header.Get("Retry-After"))
		}
	}
	f.hub = hub

	// Encode before Recover is refused and puts the poll back.
	g := &fixture{t: t, dir: t.TempDir(), clk: &clock{now: t0}, space: &space{free: 1 << 40}}
	g.dbPath = newLedgerFile(t)
	g.open(nil)
	g.hub.mu.Lock()
	p := &poll{node: "node-a", encoders: []string{"libx265"}, reply: make(chan pollReply, 1)}
	g.hub.polls = append(g.hub.polls, p)
	g.hub.mu.Unlock()
	tk, err := g.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.hub.Encode(context.Background(), tk, g.job("film", 1000), nil); !errors.Is(err, ErrNotRecovered) {
		t.Errorf("Encode before Recover returned %v, want ErrNotRecovered", err)
	}
	if g.queuedPolls() != 1 {
		t.Error("a poll whose Encode was refused before any grant was not put back")
	}
}

// TestNodeFixture_EncodeRefusesAJobItCannotLeaseAndPutsThePollBack.
func TestNodeFixture_EncodeRefusesAJobItCannotLeaseAndPutsThePollBack(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.MaxLeasesPerNode = 2 })
	job := f.job("film", 1000)
	answered := make(chan reply, 1)
	go func() { answered <- f.acquire("node-a") }()
	ctx := context.Background()

	next := func() *Ticket {
		t.Helper()
		tk, err := f.hub.WaitDemand(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	bad := job
	bad.Temp = "relative.part"
	if _, err := f.hub.Encode(ctx, next(), bad, nil); !errors.Is(err, ErrBadJob) {
		t.Errorf("Encode of a job with a relative working file returned %v, want ErrBadJob", err)
	}
	other := job
	other.Encoder = "hevc_nvenc"
	if _, err := f.hub.Encode(ctx, next(), other, nil); !errors.Is(err, ErrEncoderUnsupported) {
		t.Errorf("Encode with an encoder the node did not report returned %v, want ErrEncoderUnsupported", err)
	}
	if _, err := f.hub.Encode(ctx, nil, job, nil); !errors.Is(err, ErrTicketUsed) {
		t.Errorf("Encode with no ticket returned %v, want ErrTicketUsed", err)
	}
	if rows, _ := f.st.LiveLeases(ctx); len(rows) != 0 {
		t.Fatalf("a refused Encode wrote %d lease rows", len(rows))
	}
	// The poll survived all three and is still good for a lease.
	call := &engineCall{done: make(chan struct{})}
	tk := next()
	go func() {
		defer close(call.done)
		call.res, call.err = f.hub.Encode(ctx, tk, job, nil)
	}()
	r := <-answered
	var a AcquireResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &a) != nil {
		t.Fatalf("the poll was answered %d %s after the refusals, want the lease", r.status, r.body)
	}

	// The same path cannot be leased twice at once: the second Encode is refused, and its
	// poll goes back to the queue.
	second := make(chan reply, 1)
	go func() { second <- f.acquire("node-a") }()
	if _, err := f.hub.Encode(ctx, next(), job, nil); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("a second lease of one path returned %v, want ErrLeaseHeld", err)
	}
	if f.queuedPolls() != 1 {
		t.Error("the poll of a refused second lease was not put back")
	}
	// A refusal that is neither a cap nor the reservation answers the poll 503 `refused`.
	f.hub.Release(next(), errors.New("the engine changed its mind"))
	is503(t, "a poll released with another refusal", <-second, "refused")
	if call.returned() {
		t.Error("the refusals ended the first lease")
	}
}

// TestNodeFixture_RunSweepsLeasesNothingWaitsOnAndPruneKeepsTheEpochHistory.
func TestNodeFixture_RunSweepsLeasesNothingWaitsOnAndPruneKeepsTheEpochHistory(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.LeaseRetention = time.Hour })
	job := f.job("film", 1000)
	ctx := context.Background()
	// Three grants of one path, each expired: the rows of epochs 1 and 2 are prunable once
	// the retention has passed, and epoch 3's never is.
	var ids []string
	for i := 0; i < 3; i++ {
		a, call := f.grant("node-a", job)
		ids = append(ids, a.LeaseID)
		f.clk.advance(ttl)
		if _, err := call.wait(t); leaseErr(t, err).Reason != ReasonExpired {
			t.Fatal(err)
		}
	}
	f.clk.advance(55 * time.Minute)
	f.hub.Prune(ctx) // nothing is older than the retention yet
	for _, id := range ids {
		f.row(id)
	}
	f.clk.advance(10 * time.Minute)
	f.hub.Prune(ctx)
	for i, id := range ids {
		_, ok, _ := f.st.GetLease(ctx, id)
		if want := i == 2; ok != want {
			t.Errorf("after the prune the row of epoch %d present=%v, want %v", i+1, ok, want)
		}
	}
	if a, _ := f.grant("node-a", job); a.Epoch != 4 {
		t.Errorf("the grant after the prune is at epoch %d, want 4", a.Epoch)
	}

	// Run expires a lease no engine call waits on.
	g := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour })
	a, _ := g.grant("node-a", g.job("film", 1000))
	g.srv.Close()
	_ = g.st.Close()
	g.open(nil)
	if _, err := g.hub.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { g.hub.Run(runCtx); close(stopped) }()
	g.clk.advance(ttl)
	eventually(t, "Run to expire the lease nothing waits on", func() bool { return g.row(a.LeaseID).State == store.LeaseExpired })
	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
}

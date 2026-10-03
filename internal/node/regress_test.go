package node

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// flakyLedger is a ledger whose writes to an existing row can be made to fail, standing for
// a database that is locked or gone at the moment the hub tries to end a lease.
type flakyLedger struct {
	store.LeaseLedger
	fail atomic.Bool
}

func (l *flakyLedger) UpdateLease(ctx context.Context, id string, decide func(Lease) (Lease, error)) (Lease, error) {
	if l.fail.Load() {
		return Lease{}, errors.New("database is locked")
	}
	return l.LeaseLedger.UpdateLease(ctx, id, decide)
}

// TestNodeFixture_ALeaseThatEndsLateNeverRemovesALaterAttemptsWorkingFile: the engine's
// call is cancelled while the ledger cannot end the row. The call removes the working file
// and leaves, naming the node and the epoch; the row is still granted. The engine then
// writes its next attempt at the same deterministic name, and when the stale row finally
// expires, that expiry unlinks nothing.
func TestNodeFixture_ALeaseThatEndsLateNeverRemovesALaterAttemptsWorkingFile(t *testing.T) {
	t.Parallel()
	var fl *flakyLedger
	f := newFixture(t, func(o *Options) { fl = &flakyLedger{LeaseLedger: o.Ledger}; o.Ledger = fl })
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("upload answered %d", r.status)
	}
	fl.fail.Store(true)
	call.cancel()
	_, err := call.wait(t)
	le := leaseErr(t, err)
	if le.Reason != ReasonCanceled || le.Node != "node-a" || le.Epoch != 1 || le.LeaseID != a.LeaseID {
		t.Errorf("the cancelled call returned %+v, want a cancelled lease naming node-a at epoch 1", le)
	}
	if exists(job.Temp) {
		t.Fatal("the call returned an error with the working file still there")
	}
	fl.fail.Store(false)
	if row := f.row(a.LeaseID); !row.State.Live() {
		t.Fatalf("this case needs the row still live after the failed ending; it is %s", row.State)
	}
	if f.hub.attached(a.LeaseID) {
		t.Fatal("the lease is still attached after its engine call left")
	}
	// Nothing is taken on the detached lease, and a heartbeat does not stretch it.
	is503(t, "a completion on a lease its engine call left", f.complete(a.LeaseID, a.Epoch, output, srcDigest), "not_ready")
	if row := f.row(a.LeaseID); row.State != store.LeaseUploaded {
		t.Fatalf("the refused completion moved the row to %s", row.State)
	}
	f.clk.advance(30 * time.Second)
	if hb := f.heartbeat(a.LeaseID, a.Epoch, 0.5); hb.status != http.StatusOK {
		t.Fatalf("heartbeat answered %d", hb.status)
	}
	if got := f.row(a.LeaseID).ExpiresAt; !got.Equal(t0.Add(ttl)) {
		t.Errorf("a heartbeat stretched a detached lease to %v", got)
	}

	// The engine's next attempt, at the same name.
	if err := os.WriteFile(job.Temp, []byte("the engine's own next attempt"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(30 * time.Second)
	f.hub.sweep(context.Background())
	if row := f.row(a.LeaseID); row.State != store.LeaseExpired {
		t.Fatalf("the stale row is %s after its TTL, want expired", row.State)
	}
	if got, err := os.ReadFile(job.Temp); err != nil || string(got) != "the engine's own next attempt" {
		t.Errorf("the stale lease's expiry touched a file written after the engine had left it: %q, %v", got, err)
	}
}

// TestNodeFixture_ACompletionThatBeatsAFailedCancelIsStillACompletion: the ending of a
// cancelled call fails in the ledger, and a completion settles the lease before the call
// gets back to it. The call returns that completion, and the working file is untouched.
func TestNodeFixture_ACompletionThatBeatsAFailedCancelIsStillACompletion(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	temp := filepath.Join(f.dir, "film.__transcoding__.mkv.holdfast-part")
	if err := os.WriteFile(temp, output, 0o644); err != nil {
		t.Fatal(err)
	}
	// A lease id the ledger does not hold: ending it fails, as a locked database would.
	lease := Lease{ID: "ffffffffffffffffffffffffffffffff", Node: "node-a", Epoch: 3, Temp: temp}
	w := newWait(nil)
	settled := Result{LeaseID: lease.ID, Node: "node-a", Epoch: 3, OutputBytes: 600, OutputDigest: digestOf(output)}
	w.finish(outcome{res: settled})
	res, err := f.hub.cancel(lease, w)
	if err != nil || res != settled {
		t.Errorf("cancel returned %+v, %v; want the completion that had already settled", res, err)
	}
	if got, _ := os.ReadFile(temp); !bytes.Equal(got, output) {
		t.Error("a nil error came with the working file removed")
	}

	// With nothing settled, the same failed ending removes the file and says who held it.
	w = newWait(nil)
	_, err = f.hub.cancel(lease, w)
	if le := leaseErr(t, err); le.Reason != ReasonCanceled || le.Node != "node-a" || le.Epoch != 3 || !errors.Is(err, store.ErrNoLease) {
		t.Errorf("cancel returned %v, want a cancelled lease naming node-a at epoch 3 and the ledger's error", err)
	}
	if exists(temp) {
		t.Error("an unsettled cancelled lease left its working file")
	}
}

// TestNodeFixture_NothingExpiresBeforeRecoverHasGivenTheGrace: a sweep on a restarted
// server that has not yet recovered its leases would judge them by an expiry the server's
// own downtime ran out. It expires nothing, and Adopt refuses until Recover has run.
func TestNodeFixture_NothingExpiresBeforeRecoverHasGivenTheGrace(t *testing.T) {
	t.Parallel()
	g := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour })
	job := g.job("film", 1000)
	a, _ := g.grant("node-a", job)
	g.srv.Close()
	_ = g.st.Close()
	g.clk.advance(2 * ttl) // the downtime
	g.open(nil)
	g.hub.sweep(context.Background()) // what Run does on its first tick
	if row := g.row(a.LeaseID); row.State != store.LeaseGranted {
		t.Fatalf("before Recover ran, the sweep ended the lease: %s (%s)", row.State, row.Reason)
	}
	if _, err := g.hub.Adopt(context.Background(), a.LeaseID, job, nil); !errors.Is(err, ErrNotRecovered) {
		t.Errorf("Adopt before Recover returned %v, want ErrNotRecovered", err)
	}
	if row := g.row(a.LeaseID); row.State != store.LeaseGranted {
		t.Fatalf("Adopt before Recover ended the lease: %s", row.State)
	}
	live, err := g.hub.Recover(context.Background())
	if err != nil || len(live) != 1 || !live[0].ExpiresAt.Equal(g.clk.Now().Add(ttl)) {
		t.Fatalf("Recover returned %+v, %v; want the lease with one TTL of grace", live, err)
	}
	// Once recovered, the sweep does its job again.
	g.clk.advance(ttl)
	g.hub.sweep(context.Background())
	if row := g.row(a.LeaseID); row.State != store.LeaseExpired {
		t.Errorf("after Recover and the grace the lease is %s, want expired", row.State)
	}
}

// TestNodeFixture_ARecoveredLeaseNobodyTookBackRemovesNoFileWhenItRunsOut: after a restart
// the path a granted lease recorded is the engine's startup sweep's, and may hold the
// engine's own next encode by the time the lease's grace runs out.
func TestNodeFixture_ARecoveredLeaseNobodyTookBackRemovesNoFileWhenItRunsOut(t *testing.T) {
	t.Parallel()
	g := newFixture(t, func(o *Options) { o.SweepEvery = time.Hour })
	job := g.job("film", 1000)
	a, _ := g.grant("node-a", job)
	g.srv.Close()
	_ = g.st.Close()
	g.open(nil)
	if _, err := g.hub.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	g.hub.Ready()
	if err := os.WriteFile(job.Temp, []byte("a local encode after the restart"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.clk.advance(ttl)
	g.hub.sweep(context.Background())
	if row := g.row(a.LeaseID); row.State != store.LeaseExpired {
		t.Fatalf("the lease is %s after its grace, want expired", row.State)
	}
	if got, err := os.ReadFile(job.Temp); err != nil || string(got) != "a local encode after the restart" {
		t.Errorf("the expiry of a lease nobody took back touched the file at its recorded path: %q, %v", got, err)
	}
}

// TestNodeFixture_ARefusedCompletionReportsNoEncodeTime: a completion at a stale epoch is
// 410 and leaves nothing behind, so the figure the engine is handed is the accepted one's.
func TestNodeFixture_ARefusedCompletionReportsNoEncodeTime(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, call := f.grant("node-a", job)
	if r := f.put(a.LeaseID, a.Epoch, output, digestOf(output)); r.status != http.StatusOK {
		t.Fatalf("upload answered %d", r.status)
	}
	is410(t, "a completion at a stale epoch", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{
		Epoch: a.Epoch + 1, OutputDigest: digestOf(output), SourceDigest: srcDigest, OutputBytes: 600, EncodeSec: 999}))
	isTyped(t, "a completion with another digest", f.post("/leases/"+a.LeaseID+"/complete", CompleteRequest{
		Epoch: a.Epoch, OutputDigest: digestOf([]byte("x")), SourceDigest: srcDigest, OutputBytes: 600, EncodeSec: 888}),
		http.StatusConflict, "digest_conflict")
	f.hub.mu.Lock()
	w := f.hub.waits[a.LeaseID]
	f.hub.mu.Unlock()
	w.mu.Lock()
	noted := w.encodeSec
	w.mu.Unlock()
	if noted != 0 {
		t.Errorf("a refused completion left an encode time of %v on the waiting call", noted)
	}
	is410(t, "a duplicate upload at a stale epoch", f.put(a.LeaseID, a.Epoch+1, output, digestOf(output)))
	if r := f.complete(a.LeaseID, a.Epoch, output, srcDigest); r.status != http.StatusOK {
		t.Fatalf("the completion answered %d %s", r.status, r.body)
	}
	res, err := call.wait(t)
	if err != nil || res.EncodeSeconds != 12.5 {
		t.Errorf("Encode returned encode seconds %v (%v), want the accepted completion's 12.5", res.EncodeSeconds, err)
	}
}

// TestNodeFixture_TwoContentDigestLinesAreTwoMembers: a Content-Digest sent on two header
// lines is one list, so two sha-256 members are refused however they were split.
func TestNodeFixture_TwoContentDigestLinesAreTwoMembers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	job := f.job("film", 1000)
	a, _ := f.grant("node-a", job)
	other := []byte("some other bytes")
	req, _ := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", a.LeaseID), bytes.NewReader(output))
	req.Header.Set(EpochHeader, "1")
	req.Header.Add("Content-Digest", digestOf(output))
	req.Header.Add("Content-Digest", digestOf(other))
	isTyped(t, "an upload with two sha-256 digests on two lines", f.do(req), http.StatusBadRequest, "bad_digest")
	f.onlySources("film")
	// A second line for another algorithm is passed over, as a second member is.
	req, _ = http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", a.LeaseID), bytes.NewReader(output))
	req.Header.Set(EpochHeader, "1")
	req.Header.Add("Content-Digest", "md5=:abc:")
	req.Header.Add("Content-Digest", digestOf(output))
	if r := f.do(req); r.status != http.StatusOK {
		t.Errorf("an upload with its sha-256 on a second line answered %d %s, want 200", r.status, r.body)
	}
}

// TestNodeLease_AGrantNeverSharesAWorkingFileWithALiveLease: a working file that is a live
// lease's working file, or a live lease's source, is refused.
func TestNodeLease_AGrantNeverSharesAWorkingFileWithALiveLease(t *testing.T) {
	live := []Lease{{ID: "x1", Path: "/lib/x.mkv", Node: "node-a", Temp: "/lib/x.part"}}
	ask := func(path, temp string) error {
		_, err := decideGrant(Lease{ID: "new", Path: path, Node: "node-b", Temp: temp}, live, caps{9, 9}, t0, ttl)
		return err
	}
	if err := ask("/lib/y.mkv", "/lib/x.part"); !errors.Is(err, ErrTempHeld) {
		t.Errorf("a grant on a live lease's working file returned %v, want ErrTempHeld", err)
	}
	if err := ask("/lib/y.mkv", "/lib/x.mkv"); !errors.Is(err, ErrTempHeld) {
		t.Errorf("a grant whose working file is a leased source returned %v, want ErrTempHeld", err)
	}
	if err := ask("/lib/y.mkv", "/lib/y.part"); err != nil {
		t.Errorf("a grant with a working file of its own was refused: %v", err)
	}
}

// TestNodeFixture_EncodeRefusesAWorkingFileALiveLeaseHolds, and puts the poll back.
func TestNodeFixture_EncodeRefusesAWorkingFileALiveLeaseHolds(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	first := f.job("one", 1000)
	_, call := f.grant("node-a", first)
	second := f.job("two", 1000)
	second.Temp = first.Temp
	answered := make(chan reply, 1)
	go func() { answered <- f.acquire("node-b") }()
	tk, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.Encode(context.Background(), tk, second, nil); !errors.Is(err, ErrTempHeld) {
		t.Errorf("Encode on a live lease's working file returned %v, want ErrTempHeld", err)
	}
	if rows, _ := f.st.LiveLeases(context.Background()); len(rows) != 1 {
		t.Errorf("%d live leases after the refusal, want the first alone", len(rows))
	}
	if f.queuedPolls() != 1 {
		t.Error("the poll of the refused grant was not put back")
	}
	if call.returned() {
		t.Error("the refusal ended the first lease")
	}
	f.stop()
	<-answered
}

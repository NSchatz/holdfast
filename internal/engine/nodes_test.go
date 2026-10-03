package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/nodeworker"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// The worker-node fixtures of the encode seam (docs/design/nodes.md#seam). Each drives the
// REAL engine, the REAL lease hub over the real SQLite ledger and the real lease endpoints on
// a loopback httptest server; the node is the real worker loop where it can be, and a fake
// that speaks the protocol by hand where a real worker cannot misbehave. The sources are a
// two-second 320x240 clip, and the server's own encoder is observed through argvObserver, so
// "the server ran no encode for that file" is read off the command lines it really built.

// lockedBuf is a log sink a test may read while the engine writes it.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// leaseClock is the hub's clock, held by the test.
type leaseClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *leaseClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *leaseClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// nodePrefix is where the server mounts the lease endpoints, and where a worker looks.
const nodePrefix = "/api/node/v1"

type nodeRig struct {
	t               *testing.T
	ffmpeg, ffprobe string
	root            string
	cfg             config.Config
	ts              *testStore
	eng             *Engine
	hub             *node.Hub
	srv             *httptest.Server
	argv            *argvLog
	logs            *lockedBuf
	clock           *leaseClock
	// stages counts the plan announcements by stage, so a test can ask whether the gates
	// ever ran.
	stageMu sync.Mutex
	stages  map[string]int
}

// newNodeRig builds an engine with nodes ON over a fresh library root.
func newNodeRig(t *testing.T, mutate func(*config.Config)) *nodeRig {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	r := &nodeRig{t: t, ffmpeg: ffmpeg, ffprobe: ffprobe, root: root, cfg: cfg, ts: newTestStore(t, root),
		argv: newArgvLog(), logs: &lockedBuf{}, clock: &leaseClock{now: time.Now()}, stages: map[string]int{}}
	log := slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, argvObserver: r.argv.record}
	r.eng = New(cfg, prober, enc, r.ts, log)
	r.eng.encodePlanObserver = func(stage string, _ *EncodePlan) {
		r.stageMu.Lock()
		r.stages[stage]++
		r.stageMu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.hub = node.New(node.Options{
		Ledger: r.ts.SQLite, BaseCtx: ctx, Version: "test", TTL: cfg.NodeLeaseTTL(),
		MaxLeases: cfg.EffectiveNodeMaxLeases(), MaxLeasesPerNode: cfg.EffectiveNodeMaxLeasesPerNode(),
		MaxTransfers: cfg.EffectiveNodeMaxTransfers(), LongPoll: 20 * time.Second, RetryAfter: time.Second,
		SweepEvery: 20 * time.Millisecond, Now: r.clock.Now, Log: log,
	})
	if _, err := r.hub.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	r.hub.Ready()
	r.eng.Nodes = r.hub
	mux := chi.NewRouter()
	mux.Route(nodePrefix, func(sub chi.Router) { node.Mount(sub, func() *node.Hub { return r.hub }) })
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *nodeRig) stage(name string) int {
	r.stageMu.Lock()
	defer r.stageMu.Unlock()
	return r.stages[name]
}

func (r *nodeRig) source(name string) string {
	r.t.Helper()
	p := filepath.Join(r.root, name)
	mkH264(r.t, r.ffmpeg, p, "8M")
	return p
}

// answer is what one fake-node call came back with.
type answer struct {
	status int
	body   []byte
	lease  node.AcquireResponse
	reason string
	err    error
}

func (r *nodeRig) post(route string, in any) answer {
	payload, _ := json.Marshal(in)
	resp, err := http.Post(r.srv.URL+nodePrefix+route, "application/json", bytes.NewReader(payload))
	if err != nil {
		return answer{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode, body: body}
	var e node.ErrorResponse
	if json.Unmarshal(body, &e) == nil {
		a.reason = e.Error
	}
	return a
}

// poll is a fake node asking for work, in the background.
func (r *nodeRig) poll(name string, encoders ...string) <-chan answer {
	if len(encoders) == 0 {
		encoders = []string{"cpu"}
	}
	out := make(chan answer, 1)
	go func() {
		a := r.post(node.RouteLeases, node.AcquireRequest{Node: name, Version: "test", Slots: 1,
			Mode: node.ModeMapped, Encoders: encoders})
		if a.err == nil && a.status == http.StatusOK {
			_ = json.Unmarshal(a.body, &a.lease)
		}
		out <- a
	}()
	return out
}

func (r *nodeRig) put(l node.AcquireResponse, body []byte, digest string) answer {
	req, _ := http.NewRequest(http.MethodPut, r.srv.URL+nodePrefix+leasePath(node.RouteOutput, l.LeaseID), bytes.NewReader(body))
	req.Header.Set("Content-Digest", digest)
	req.Header.Set(node.EpochHeader, strconv.FormatInt(l.Epoch, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	a := answer{status: resp.StatusCode, body: b}
	var e node.ErrorResponse
	if json.Unmarshal(b, &e) == nil {
		a.reason = e.Error
	}
	return a
}

func leasePath(route, id string) string { return strings.Replace(route, "{id}", id, 1) }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return node.FormatDigest(sum[:])
}

func fileDigestOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return digestOf(b)
}

// offer runs src through the pipeline as a node feeder would: it waits for the demand ticket
// of the node that is polling, and carries it to the seam.
func (r *nodeRig) offer(src string) <-chan error {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	tk, err := r.hub.WaitDemand(ctx)
	if err != nil {
		cancel()
		r.t.Fatalf("no node was asking for work: %v\n%s", err, r.logs.String())
	}
	done := make(chan error, 1)
	go func() {
		defer cancel()
		nj := &nodeJob{ticket: tk}
		perr := r.eng.ProcessFile(withNodeJob(ctx, nj), "n0", src)
		r.eng.settle(ctx, nj)
		done <- perr
	}()
	return done
}

func waitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessFile: %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the job did not end")
	}
}

func waitAnswer(t *testing.T, ch <-chan answer) answer {
	t.Helper()
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatalf("node call: %v", a.err)
		}
		return a
	case <-time.After(30 * time.Second):
		t.Fatal("the node's poll was never answered")
		return answer{}
	}
}

// row is the ledger row of the one file at path (whatever its fingerprint).
func (r *nodeRig) row(path string) (store.Job, bool) {
	r.t.Helper()
	jobs, err := r.ts.List(context.Background(), nil, 100)
	if err != nil {
		r.t.Fatalf("List: %v", err)
	}
	for _, j := range jobs {
		if j.Path == path {
			return j, true
		}
	}
	return store.Job{}, false
}

func (r *nodeRig) liveLeases() []node.Lease {
	r.t.Helper()
	live, err := r.ts.LiveLeases(context.Background())
	if err != nil {
		r.t.Fatalf("LiveLeases: %v", err)
	}
	return live
}

// noTempBeside fails the test if any file of the temp construction sits in dir.
func noTempBeside(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if isTempName(e.Name()) {
			t.Errorf("a working file was left in %s: %s", dir, e.Name())
		}
	}
}

// serverEncodes is how many encodes the SERVER's own encoder built for src.
func (r *nodeRig) serverEncodes(src string) int {
	r.argv.mu.Lock()
	defer r.argv.mu.Unlock()
	if _, ok := r.argv.byIn[src]; ok {
		return 1
	}
	return 0
}

// realWorker runs the real worker loop against the rig's server until the test ends. The
// worker reaches the library under a DIFFERENT path - a symlink to the root - so its path
// map is really exercised.
func (r *nodeRig) realWorker(name string, tune func(*nodeworker.Options)) (workDir string) {
	r.t.Helper()
	mount := filepath.Join(r.t.TempDir(), "mount")
	if err := os.Symlink(r.root, mount); err != nil {
		r.t.Fatal(err)
	}
	workDir = filepath.Join(r.t.TempDir(), "work")
	enc := FFmpegEncoder{FFmpeg: r.ffmpeg}
	o := nodeworker.Options{
		Server: r.srv.URL, Token: secret.NewValue("unused-by-this-rig"), Name: name, Version: "test",
		PathMap: config.PathMap{{From: r.root, To: mount}}, WorkDir: workDir, Encoders: []string{"cpu"},
		Encode: func(ctx context.Context, in, out string, pre, body []string, _ func(float64)) error {
			return enc.RunLeased(ctx, in, out, pre, body, nil)
		},
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(r.logs, nil)),
	}
	if tune != nil {
		tune(&o)
	}
	w, err := nodeworker.New(o)
	if err != nil {
		r.t.Fatalf("nodeworker.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = w.Run(ctx) }()
	r.t.Cleanup(func() { cancel(); <-stopped })
	return workDir
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("%s is not empty: %s", dir, e.Name())
	}
}

// TestNodes_OffByDefaultThePoolAndArgvAreUnchanged is I5: with Engine.Nodes nil the pass
// starts the local workers and nothing else, and the command line, the claims and the log
// records of a run are the ones a run with nodes on but no node asking for work produces -
// which in turn starts feeders that consume nothing.
func TestNodes_OffByDefaultThePoolAndArgvAreUnchanged(t *testing.T) {
	type observed struct {
		workers []string
		argv    []string
		msgs    []string
		status  store.Status
	}
	runOnce := func(nodesOn bool) observed {
		r := newNodeRig(t, func(c *config.Config) { c.Workers = 2 })
		if !nodesOn {
			r.eng.Nodes = nil
		}
		src := r.source("movie.mkv")
		var mu sync.Mutex
		var o observed
		r.eng.onClaim = func(worker, _ string) {
			mu.Lock()
			o.workers = append(o.workers, worker)
			mu.Unlock()
		}
		before := runtime.NumGoroutine()
		if err := r.eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if nodesOn {
			// No node asked for work, so no feeder took a file and none outlived the pass.
			settleGoroutines(t, before)
		}
		for _, a := range r.argv.forSource(t, src) {
			o.argv = append(o.argv, strings.ReplaceAll(a, r.root, "<root>"))
		}
		for _, line := range strings.Split(r.logs.String(), "\n") {
			if i := strings.Index(line, "msg="); i >= 0 {
				// The message alone: the values beside it carry this run's own paths.
				msg := line[i+len("msg="):]
				if strings.HasPrefix(msg, `"`) {
					msg = msg[:strings.Index(msg[1:], `"`)+2]
				} else if j := strings.Index(msg, " "); j >= 0 {
					msg = msg[:j]
				}
				o.msgs = append(o.msgs, line[strings.Index(line, "level="):i]+msg)
			}
		}
		j, ok := r.row(src)
		if !ok {
			t.Fatal("no row for the source")
		}
		o.status = j.Status
		if len(r.liveLeases()) != 0 {
			t.Error("a lease was granted with no node asking for work")
		}
		return o
	}
	off, on := runOnce(false), runOnce(true)
	if off.status != store.Done || on.status != store.Done {
		t.Fatalf("status with nodes off = %s, on = %s, want done for both", off.status, on.status)
	}
	if strings.Join(off.workers, ",") != "w0" && strings.Join(off.workers, ",") != "w1" {
		t.Errorf("with nodes off the file was claimed by %v, want one local worker", off.workers)
	}
	for _, w := range append(append([]string(nil), off.workers...), on.workers...) {
		if !strings.HasPrefix(w, "w") {
			t.Errorf("a worker that is not a local worker claimed the file: %s", w)
		}
	}
	if strings.Join(off.argv, "\x00") != strings.Join(on.argv, "\x00") {
		t.Errorf("the command line moved with nodes on and no node asking:\noff: %q\non:  %q", off.argv, on.argv)
	}
	if strings.Join(off.msgs, "\n") != strings.Join(on.msgs, "\n") {
		t.Errorf("the log records moved with nodes on and no node asking:\noff:\n%s\non:\n%s",
			strings.Join(off.msgs, "\n"), strings.Join(on.msgs, "\n"))
	}
	for _, a := range off.argv {
		if strings.Contains(a, "node") {
			t.Errorf("with nodes off the command line carries %q", a)
		}
	}
}

// settleGoroutines waits for the goroutine count to come back to at most base.
func settleGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("%d goroutines, want at most %d: a goroutine outlived the pass\n%s",
				runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNodes_AJobThatIsNotLeasableIsEncodedByTheServer: a job whose plan a node cannot run -
// here, its encoder is one the node did not report - is encoded by the server itself, the
// node's poll goes back unused, and no lease is granted.
func TestNodes_AJobThatIsNotLeasableIsEncodedByTheServer(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	lease := r.poll("nodeA", "svtav1")
	waitDone(t, r.offer(src))

	if r.serverEncodes(src) != 1 {
		t.Error("the server did not encode a job its node could not run")
	}
	if j, _ := r.row(src); j.Status != store.Done {
		t.Fatalf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
	}
	if !strings.Contains(r.logs.String(), "this job is encoded by the server") ||
		!strings.Contains(r.logs.String(), "did not report the encoder cpu") {
		t.Error("no record says why the job was not leased")
	}
	select {
	case a := <-lease:
		t.Fatalf("the node's poll was answered %d %s; its ticket should have gone back unused", a.status, a.body)
	default:
	}
	// The poll is back in the queue: it can be reserved again.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tk, err := r.hub.WaitDemand(ctx)
	if err != nil {
		t.Fatalf("the unused ticket was not given back: %v", err)
	}
	r.hub.Release(tk, node.ErrNoRoom)
	if a := waitAnswer(t, lease); a.status != http.StatusServiceUnavailable {
		t.Errorf("poll answered %d, want 503", a.status)
	}
}

// TestNodes_ThePlansANodeCannotRunAreNotLeasable is D4 as a table: each way a plan stops
// being self-contained on another host refuses the lease, and the plan it started from is
// leasable.
func TestNodes_ThePlansANodeCannotRunAreNotLeasable(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	cfg := baseCfg(d)
	prof := cfg.TopLevelProfile()
	prober := probe.New(ffmpeg, ffprobe)
	derive := func() *EncodePlan {
		props := prober.VideoProps(context.Background(), src)
		plan, err := deriveEncodePlan(planInputs{
			settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: filepath.Join(d, "out.mkv"),
			snapshot: func() (*probe.VideoProps, error) { return props, nil },
		})
		if err != nil {
			t.Fatalf("deriveEncodePlan: %v", err)
		}
		return plan
	}
	all := func(string) bool { return true }
	pre, body, why := derive().leasable(all)
	if why != "" || len(pre) != 0 {
		t.Fatalf("a software, software-decoded plan is not leasable: %q (pre %v)", why, pre)
	}
	if got := strings.Join(body, " "); !strings.Contains(got, "-c:v libx265") || !strings.HasSuffix(got, "-f matroska") ||
		strings.Contains(got, "pools=") || strings.Contains(got, src) {
		t.Errorf("leased body = %q: want the plan's options, the container's last, no pool figure and no path", got)
	}
	for name, mutate := range map[string]func(*EncodePlan){
		"a stream copy":         func(p *EncodePlan) { p.Video.Copy = true },
		"a hardware encoder":    func(p *EncodePlan) { p.Video.Encoder.Hardware = true },
		"a hardware decode":     func(p *EncodePlan) { p.Video.Decode = DecodeCUDA },
		"an encoder device":     func(p *EncodePlan) { p.Video.Device = "/dev/dri/renderD128" },
		"a decode device":       func(p *EncodePlan) { p.Video.DecodeDevice = "/dev/dri/renderD128" },
		"dynamic HDR":           func(p *EncodePlan) { p.dynamic = &dynhdr.Prepared{} },
		"an attached picture":   func(p *EncodePlan) { p.coverArt.attached = []matroskaPicture{{name: "cover"}} },
		"an unreported encoder": nil,
	} {
		p := derive()
		supports := all
		if mutate == nil {
			supports = func(string) bool { return false }
		} else {
			mutate(p)
		}
		if _, _, why := p.leasable(supports); why == "" {
			t.Errorf("a plan with %s was leasable", name)
		}
	}
}

// TestNodes_FeedersLeakNoGoroutineAndNoTicket: through the real pool. A node asks for work
// while a pass runs; the feeder takes a file for it; when the feed closes every feeder is
// gone and no poll is left reserved - the node's next poll is answered, not stranded. And a
// pass cancelled while a feeder holds a ticket ends the same way.
func TestNodes_FeedersLeakNoGoroutineAndNoTicket(t *testing.T) {
	t.Run("a pass that runs to its end", func(t *testing.T) {
		r := newNodeRig(t, nil)
		// Both files are already HEVC, so every job ends at a guard: the feeders and the
		// local worker share the feed, and whichever takes a file leases nothing.
		for _, n := range []string{"a.mkv", "b.mkv", "c.mkv"} {
			mkHevc(t, r.ffmpeg, filepath.Join(r.root, n), "200k")
		}
		lease := r.poll("nodeA")
		waitQueued(t, r)
		before := runtime.NumGoroutine()
		if err := r.eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		settleGoroutines(t, before)
		if n := len(r.liveLeases()); n != 0 {
			t.Errorf("%d lease(s) live after a pass that encoded nothing", n)
		}
		assertPollNotStranded(t, r, lease)
	})
	t.Run("a pass cancelled while a feeder holds a ticket", func(t *testing.T) {
		r := newNodeRig(t, nil)
		lease := r.poll("nodeA")
		waitQueued(t, r)
		before := runtime.NumGoroutine()
		ctx, cancel := context.WithCancel(context.Background())
		feed := make(chan string)
		var wg sync.WaitGroup
		stop := r.eng.startFeeders(ctx, &wg, feed, func(context.Context, string, string) bool {
			t.Error("a feeder processed a file nobody fed")
			return true
		})
		// Every feeder is now waiting: one on the feed with the node's ticket in hand, the
		// rest on demand.
		waitReserved(t, r)
		cancel()
		wg.Wait()
		stop()
		settleGoroutines(t, before)
		assertPollNotStranded(t, r, lease)
	})
	t.Run("the feed closes while a feeder holds a ticket", func(t *testing.T) {
		r := newNodeRig(t, nil)
		lease := r.poll("nodeA")
		waitQueued(t, r)
		feed := make(chan string)
		var wg sync.WaitGroup
		stop := r.eng.startFeeders(context.Background(), &wg, feed, func(context.Context, string, string) bool { return false })
		waitReserved(t, r)
		close(feed)
		stop()
		wg.Wait()
		assertPollNotStranded(t, r, lease)
	})
}

// waitQueued waits until the fake node's poll is in the hub's queue: a ticket can be
// reserved for it. The ticket is given straight back.
func waitQueued(t *testing.T, r *nodeRig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tk, err := r.hub.WaitDemand(ctx)
	if err != nil {
		t.Fatalf("the node's poll never arrived: %v", err)
	}
	r.hub.Release(tk, nil)
}

// waitReserved waits until a feeder has reserved the one poll: nothing is left to reserve.
func waitReserved(t *testing.T, r *nodeRig) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		tk, err := r.hub.WaitDemand(ctx)
		cancel()
		if err != nil {
			return
		}
		r.hub.Release(tk, nil)
		if time.Now().After(deadline) {
			t.Fatal("no feeder ever reserved the node's poll")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertPollNotStranded proves no ticket leaked: the node's poll is still answerable.
func assertPollNotStranded(t *testing.T, r *nodeRig, lease <-chan answer) {
	t.Helper()
	select {
	case a := <-lease:
		t.Fatalf("the poll was answered %d %s before the test released it", a.status, a.body)
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tk, err := r.hub.WaitDemand(ctx)
	if err != nil {
		t.Fatalf("a ticket leaked: the node's poll is still reserved by a feeder that is gone (%v)", err)
	}
	r.hub.Release(tk, node.ErrNoRoom)
	if a := waitAnswer(t, lease); a.status != http.StatusServiceUnavailable || a.reason != "no_room" {
		t.Errorf("poll answered %d %q, want 503 no_room", a.status, a.reason)
	}
}

// TestNodes_TheNodeGateBoundsTheServersOwnWork: node_gate_slots. A slot is held by one job
// at a time, the queue is full only when every slot is held AND as many jobs again wait, and
// a feeder asks for no demand while it is.
func TestNodes_TheNodeGateBoundsTheServersOwnWork(t *testing.T) {
	g := newNodeGate(1)
	if g.full() {
		t.Fatal("an empty gate is full")
	}
	release, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.full() {
		t.Fatal("a gate with its slot held and nobody waiting is full: a node's job may still queue one deep")
	}
	if err := g.waitRoom(context.Background()); err != nil {
		t.Fatalf("waitRoom with nobody waiting: %v", err)
	}
	got := make(chan func(), 1)
	go func() {
		rel, err := g.acquire(context.Background())
		if err == nil {
			got <- rel
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !g.full() {
		if time.Now().After(deadline) {
			t.Fatal("a gate with its slot held and a job waiting never read as full")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if err := g.waitRoom(ctx); err == nil {
		t.Error("waitRoom returned while the gate queue was full")
	}
	cancel()
	select {
	case <-got:
		t.Fatal("a second job took the one slot while it was held")
	default:
	}
	// A cancelled wait for a slot gives its place in the queue back.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := g.acquire(cctx); err == nil {
		t.Fatal("acquire on a cancelled context took a slot")
	}
	release()
	release() // releasing twice gives one slot back, not two
	second := <-got
	if g.full() {
		t.Error("the gate reads full with one slot held and nobody waiting")
	}
	select {
	case rel := <-acquireAsync(g):
		rel()
		t.Fatal("a slot was free while the second job held the only one: release ran twice")
	case <-time.After(50 * time.Millisecond):
	}
	second()

	// And through the engine: while the queue is full no feeder reserves a node's poll.
	r := newNodeRig(t, nil)
	hold, err := r.eng.gate().acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waiter := acquireAsync(r.eng.gate())
	for !r.eng.gate().full() {
		time.Sleep(time.Millisecond)
	}
	lease := r.poll("nodeA")
	waitQueued(t, r)
	ctx, cancel = context.WithCancel(context.Background())
	feed := make(chan string)
	var wg sync.WaitGroup
	stop := r.eng.startFeeders(ctx, &wg, feed, func(context.Context, string, string) bool { return false })
	time.Sleep(100 * time.Millisecond)
	tctx, tcancel := context.WithTimeout(context.Background(), time.Second)
	tk, err := r.hub.WaitDemand(tctx)
	tcancel()
	if err != nil {
		t.Fatal("a feeder reserved a node's poll while the server's gate queue was full")
	}
	r.hub.Release(tk, nil)
	hold()
	(<-waiter)()
	waitReserved(t, r) // the queue has room again, so a feeder asks for demand
	cancel()
	wg.Wait()
	stop()
	assertPollNotStranded(t, r, lease)
}

func acquireAsync(g *nodeGate) <-chan func() {
	out := make(chan func(), 1)
	go func() {
		if rel, err := g.acquire(context.Background()); err == nil {
			out <- rel
		}
	}()
	return out
}

// TestNodes_TheWrongSourceDigestFailsBeforeTheGates is P4 rule 5: the node completes its
// lease honestly about its output and reports a source digest that is not the server's - a
// wrong path map, a stale mount. The job fails at the encode gate naming that, no gate runs,
// the working file is removed and the source is untouched.
func TestNodes_TheWrongSourceDigestFailsBeforeTheGates(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	before := md5f(t, src)
	lease := r.poll("nodeA")
	done := r.offer(src)
	l := waitAnswer(t, lease).lease
	if l.LeaseID == "" {
		t.Fatal("no lease was granted")
	}
	out := bytes.Repeat([]byte("x"), 4096)
	if a := r.put(l, out, digestOf(out)); a.status != http.StatusOK {
		t.Fatalf("upload: %d %s", a.status, a.body)
	}
	if a := r.post(leasePath(node.RouteComplete, l.LeaseID), node.CompleteRequest{Epoch: l.Epoch,
		OutputDigest: digestOf(out), SourceDigest: digestOf([]byte("another file entirely")), OutputBytes: int64(len(out))}); a.status != http.StatusOK {
		t.Fatalf("complete: %d %s", a.status, a.body)
	}
	waitDone(t, done)

	j, _ := r.row(src)
	if j.Status != store.Failed || j.FailCount != 1 {
		t.Fatalf("status = %s, fail_count = %d, want failed and 1", j.Status, j.FailCount)
	}
	for _, want := range []string{"worker_path_map", "stale mount", "nodeA", "epoch 1"} {
		if !strings.Contains(j.Outcome.Reason, want) {
			t.Errorf("the reason does not name %q: %s", want, j.Outcome.Reason)
		}
	}
	if j.Outcome.FailureClass != store.FailureTransient {
		t.Errorf("failure class = %q, want transient", j.Outcome.FailureClass)
	}
	if n := r.stage(planStageVerify); n != 0 {
		t.Errorf("the gates ran %d time(s) on an output whose source digest was wrong", n)
	}
	if md5f(t, src) != before {
		t.Error("the source changed")
	}
	noTempBeside(t, r.root)
	if r.serverEncodes(src) != 0 {
		t.Error("the server encoded the file itself")
	}
}

// TestWorkerFixture_AFreeSpaceReservationRefusalAtGrantIs503AndTheSourceIsUntouched is P4
// rule 8: the engine's own free-space reservation refuses the job a node was offered, so the
// node's poll is answered 503 `no_room`, no lease row exists, and the job fails as it does
// for a local worker.
func TestWorkerFixture_AFreeSpaceReservationRefusalAtGrantIs503AndTheSourceIsUntouched(t *testing.T) {
	for _, mode := range []string{"beside the source", "scratch_dir"} {
		t.Run(mode, func(t *testing.T) {
			scratch := ""
			r := newNodeRig(t, func(c *config.Config) {
				if mode == "scratch_dir" {
					scratch = filepath.Join(t.TempDir(), "scratch")
					if err := os.MkdirAll(scratch, 0o755); err != nil {
						t.Fatal(err)
					}
					c.ScratchDir = scratch
				}
			})
			src := r.source("movie.mkv")
			before := md5f(t, src)
			r.eng.freeBytes = func(string) (uint64, error) { return 10, nil }
			lease := r.poll("nodeA")
			waitDone(t, r.offer(src))

			a := waitAnswer(t, lease)
			if a.status != http.StatusServiceUnavailable || a.reason != "no_room" {
				t.Fatalf("poll answered %d %q, want 503 no_room", a.status, a.reason)
			}
			if n := len(r.liveLeases()); n != 0 {
				t.Errorf("%d lease(s) live after a refused reservation", n)
			}
			j, _ := r.row(src)
			if j.Status != store.Failed || !strings.Contains(j.Outcome.Reason, "not enough room") {
				t.Errorf("status = %s reason = %q, want the free-space failure a local job records", j.Status, j.Outcome.Reason)
			}
			if md5f(t, src) != before {
				t.Error("the source changed")
			}
			noTempBeside(t, r.root)
			if r.serverEncodes(src) != 0 {
				t.Error("the server encoded a job whose reservation was refused")
			}
		})
	}
}

// TestWorkerFixture_TheRetryBoundParksTheFileNamingTheNodeAttempts is P4 rule 3: a job whose
// node fails every lease is offered max_failures times and then held out by the existing
// Claim, with one record naming every node attempt. The source is never touched.
func TestWorkerFixture_TheRetryBoundParksTheFileNamingTheNodeAttempts(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	before := md5f(t, src)
	for attempt := 1; attempt <= r.cfg.MaxFailures; attempt++ {
		lease := r.poll("nodeA")
		done := r.offer(src)
		l := waitAnswer(t, lease).lease
		if l.Epoch != int64(attempt) {
			t.Fatalf("attempt %d was leased at epoch %d", attempt, l.Epoch)
		}
		if a := r.post(leasePath(node.RouteFail, l.LeaseID), node.FailRequest{Epoch: l.Epoch, Reason: "encode_failed"}); a.status != http.StatusOK {
			t.Fatalf("fail: %d %s", a.status, a.body)
		}
		waitDone(t, done)
		j, _ := r.row(src)
		if j.Status != store.Failed || j.FailCount != attempt {
			t.Fatalf("after attempt %d: status = %s, fail_count = %d", attempt, j.Status, j.FailCount)
		}
		if !strings.Contains(j.Outcome.Reason, "nodeA") || !strings.Contains(j.Outcome.Reason, fmt.Sprintf("epoch %d", attempt)) ||
			!strings.Contains(j.Outcome.Reason, "encode_failed") {
			t.Errorf("the reason does not name the node, the epoch and the node's reason: %s", j.Outcome.Reason)
		}
		parked := strings.Contains(r.logs.String(), "the retry bound parked this file")
		if parked != (attempt == r.cfg.MaxFailures) {
			t.Errorf("after attempt %d the park record present = %v", attempt, parked)
		}
	}
	logs := r.logs.String()
	if !strings.Contains(logs, `node_attempts="nodeA@1:node_failed(encode_failed) nodeA@2:node_failed(encode_failed) nodeA@3:node_failed(encode_failed)"`) {
		t.Errorf("the park record does not name the three node attempts:\n%s", grepLines(logs, "retry bound"))
	}

	// Offered once more: the claim holds the row out, nothing is leased, the ticket goes back.
	lease := r.poll("nodeA")
	waitDone(t, r.offer(src))
	select {
	case a := <-lease:
		t.Fatalf("a parked file was leased again: %d %s", a.status, a.body)
	default:
	}
	assertPollNotStranded(t, r, lease)
	if j, _ := r.row(src); j.FailCount != r.cfg.MaxFailures || j.Status != store.Failed {
		t.Errorf("the parked row moved: status = %s, fail_count = %d", j.Status, j.FailCount)
	}
	if md5f(t, src) != before {
		t.Error("the source changed")
	}
	noTempBeside(t, r.root)
	if r.serverEncodes(src) != 0 {
		t.Error("the server encoded the file itself")
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestWorkerFixture_AnExpiredLeaseUploadIsDiscardedAndTheJobIsRetried: a node whose
// heartbeats stopped for one TTL sends its output anyway. The upload is answered 410 and
// writes nothing beside the source; the job fails transient, naming the node and the epoch;
// and the next offer leases it again at the next epoch.
func TestWorkerFixture_AnExpiredLeaseUploadIsDiscardedAndTheJobIsRetried(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	before := md5f(t, src)
	lease := r.poll("nodeA")
	done := r.offer(src)
	l := waitAnswer(t, lease).lease
	r.clock.advance(r.cfg.NodeLeaseTTL() + time.Second)
	out := bytes.Repeat([]byte("late"), 1024)
	if a := r.put(l, out, digestOf(out)); a.status != http.StatusGone {
		t.Fatalf("an upload on an expired lease answered %d %s, want 410", a.status, a.body)
	}
	waitDone(t, done)
	noTempBeside(t, r.root)
	j, _ := r.row(src)
	if j.Status != store.Failed || j.FailCount != 1 || j.Outcome.FailureClass != store.FailureTransient {
		t.Fatalf("status = %s, fail_count = %d, class = %q; want failed, 1, transient", j.Status, j.FailCount, j.Outcome.FailureClass)
	}
	if !strings.Contains(j.Outcome.Reason, "expired") || !strings.Contains(j.Outcome.Reason, "nodeA") {
		t.Errorf("the reason does not say the lease expired on nodeA: %s", j.Outcome.Reason)
	}

	lease = r.poll("nodeA")
	done = r.offer(src)
	again := waitAnswer(t, lease).lease
	if again.Epoch != l.Epoch+1 || again.Path != src {
		t.Fatalf("the retry was leased at epoch %d for %s, want epoch %d for %s", again.Epoch, again.Path, l.Epoch+1, src)
	}
	// The stale node's upload at the old epoch is still refused, and the live lease is not
	// disturbed by it.
	if a := r.put(l, out, digestOf(out)); a.status != http.StatusGone {
		t.Errorf("the stale node's upload answered %d after the re-grant, want 410", a.status)
	}
	if a := r.post(leasePath(node.RouteFail, again.LeaseID), node.FailRequest{Epoch: again.Epoch, Reason: "worker_stopping"}); a.status != http.StatusOK {
		t.Fatalf("fail: %d %s", a.status, a.body)
	}
	waitDone(t, done)
	if md5f(t, src) != before {
		t.Error("the source changed")
	}
	noTempBeside(t, r.root)
}

// TestWorkerFixture_ADigestMismatchIsRetriedWithinTheLeaseThenFails: a body that is not the
// bytes its Content-Digest declared is refused and leaves no file; the lease stays live for
// another try, and at the bound it fails, the job with it.
func TestWorkerFixture_ADigestMismatchIsRetriedWithinTheLeaseThenFails(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	before := md5f(t, src)
	lease := r.poll("nodeA")
	done := r.offer(src)
	l := waitAnswer(t, lease).lease
	body := bytes.Repeat([]byte("y"), 2048)
	wrong := digestOf([]byte("the real output"))
	for attempt := 1; attempt <= node.DefaultDigestRetries; attempt++ {
		a := r.put(l, body, wrong)
		if a.status != http.StatusBadRequest || a.reason != "digest_mismatch" {
			t.Fatalf("mismatch %d answered %d %q, want 400 digest_mismatch", attempt, a.status, a.reason)
		}
		noTempBeside(t, r.root)
		hb := r.post(leasePath(node.RouteHeartbeat, l.LeaseID), node.HeartbeatRequest{Epoch: l.Epoch})
		if live := hb.status == http.StatusOK; live != (attempt < node.DefaultDigestRetries) {
			t.Fatalf("after mismatch %d the lease is live = %v (heartbeat %d)", attempt, live, hb.status)
		}
	}
	waitDone(t, done)
	j, _ := r.row(src)
	if j.Status != store.Failed || !strings.Contains(j.Outcome.Reason, "digest_mismatch") {
		t.Fatalf("status = %s reason = %q, want failed on digest_mismatch", j.Status, j.Outcome.Reason)
	}
	if md5f(t, src) != before {
		t.Error("the source changed")
	}
	noTempBeside(t, r.root)
}

// TestWorkerFixture_A404BodyOfferedAsMediaFailsTheServersGates is the Unmanic #635 class at
// the server's end: a node uploads a 55-byte error page under the honest digest of those 55
// bytes and completes its lease. The bytes are admitted - they are what was declared - and
// are then only a candidate: the server's own gates refuse them, the working file is removed
// and the source is byte for byte what it was.
func TestWorkerFixture_A404BodyOfferedAsMediaFailsTheServersGates(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	before := md5f(t, src)
	lease := r.poll("nodeA")
	done := r.offer(src)
	l := waitAnswer(t, lease).lease
	page := []byte("<html><body><h1>404 Not Found</h1></body></html>\n\n\n\n\n\n\n")
	if len(page) != 55 {
		t.Fatalf("the error page is %d bytes, want 55", len(page))
	}
	if a := r.put(l, page, digestOf(page)); a.status != http.StatusOK {
		t.Fatalf("an honest 55-byte upload answered %d %s; it is admitted as bytes", a.status, a.body)
	}
	if a := r.post(leasePath(node.RouteComplete, l.LeaseID), node.CompleteRequest{Epoch: l.Epoch,
		OutputDigest: digestOf(page), SourceDigest: fileDigestOf(t, src), OutputBytes: 55}); a.status != http.StatusOK {
		t.Fatalf("complete: %d %s", a.status, a.body)
	}
	waitDone(t, done)

	j, _ := r.row(src)
	if j.Status != store.Failed {
		t.Fatalf("status = %s, want failed: a 404 page is not a replacement", j.Status)
	}
	if n := r.stage(planStageVerify); n != 1 {
		t.Errorf("the server's gates ran %d time(s), want 1: they are what refuses the page", n)
	}
	if md5f(t, src) != before {
		t.Error("the source changed")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the source is gone: %v", err)
	}
	noTempBeside(t, r.root)
}

// dropFirstPutAnswer is a transport that performs the first upload and then loses its
// answer, the way a proxy that times out does: the server admitted the body and the worker
// never heard so.
type dropFirstPutAnswer struct {
	dropped atomic.Bool
	puts    atomic.Int32
	after   func()
}

func (d *dropFirstPutAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if req.Method != http.MethodPut || err != nil {
		return resp, err
	}
	d.puts.Add(1)
	if d.dropped.CompareAndSwap(false, true) {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		d.after()
		return nil, errors.New("the answer was lost on the way back")
	}
	return resp, nil
}

// TestWorkerFixture_ADuplicateUploadFromTheRealWorkerRewritesNothing: the REAL worker
// encodes and uploads, the answer to its upload is lost, and it sends the same output again.
// The repeat is answered from the record: the working file's inode and modification time are
// the first upload's, and the server gates and swaps that one file.
func TestWorkerFixture_ADuplicateUploadFromTheRealWorkerRewritesNothing(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	var first, second struct {
		ino   uint64
		mtime time.Time
	}
	temp := tempPath(r.root, "movie", "mkv", 0)
	statTemp := func() (uint64, time.Time) {
		st, err := os.Stat(temp)
		if err != nil {
			t.Errorf("the working file is not at the engine's own construction %s: %v", temp, err)
			return 0, time.Time{}
		}
		return st.Sys().(*syscall.Stat_t).Ino, st.ModTime()
	}
	tr := &dropFirstPutAnswer{after: func() { first.ino, first.mtime = statTemp() }}
	// The swap consumes the working file, so its identity is read again at the last moment
	// it must still be the first upload's: when the gates start.
	r.eng.encodePlanObserver = func(stage string, _ *EncodePlan) {
		if stage == planStageVerify {
			second.ino, second.mtime = statTemp()
		}
	}
	work := r.realWorker("nodeA", func(o *nodeworker.Options) { o.HTTP = &http.Client{Transport: tr} })
	waitDone(t, r.offer(src))

	if n := tr.puts.Load(); n != 2 {
		t.Fatalf("the worker sent %d upload(s), want 2: the first's answer was lost", n)
	}
	if first.ino == 0 || first.ino != second.ino || !first.mtime.Equal(second.mtime) {
		t.Errorf("the repeat rewrote the working file: inode %d -> %d, mtime %s -> %s",
			first.ino, second.ino, first.mtime, second.mtime)
	}
	j, _ := r.row(filepath.Join(r.root, "movie.mkv"))
	if j.Status != store.Done {
		t.Fatalf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
	}
	if got := videoCodecOf(t, r.ffprobe, src); got != "hevc" {
		t.Errorf("the file is %s after the swap, want hevc", got)
	}
	if r.serverEncodes(src) != 0 {
		t.Error("the server encoded the file itself")
	}
	noTempBeside(t, r.root)
	emptyDir(t, work)
}

func videoCodecOf(t *testing.T, ffprobe, path string) string {
	t.Helper()
	return probe.New("ffmpeg", ffprobe).VideoCodec(context.Background(), path)
}

// TestNodes_WithAScratchDirTheUploadLandsThereAndTheCopyBackRuns: scratch_dir set. The
// lease's working file is the scratch working path, nothing appears beside the source until
// the gates accepted, and the existing copy-beside-and-prove-identical step and swap run.
func TestNodes_WithAScratchDirTheUploadLandsThereAndTheCopyBackRuns(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	r := newNodeRig(t, func(c *config.Config) { c.ScratchDir = scratch })
	src := r.source("movie.mkv")
	size := fileSize(t, src)
	var besideAtGates []string
	r.eng.encodePlanObserver = func(stage string, p *EncodePlan) {
		if stage != planStageVerify {
			return
		}
		ents, _ := os.ReadDir(r.root)
		for _, e := range ents {
			besideAtGates = append(besideAtGates, e.Name())
		}
		if !strings.HasPrefix(p.Output, scratch+string(filepath.Separator)) {
			t.Errorf("the gates read %s, want a working file in the scratch directory", p.Output)
		}
	}
	work := r.realWorker("nodeA", nil)
	waitDone(t, r.offer(src))

	if strings.Join(besideAtGates, ",") != "movie.mkv" {
		t.Errorf("while the gates ran the source's directory held %v, want the source alone", besideAtGates)
	}
	if j, _ := r.row(src); j.Status != store.Done {
		t.Fatalf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
	}
	if got := videoCodecOf(t, r.ffprobe, src); got != "hevc" {
		t.Errorf("the file is %s after the swap, want hevc", got)
	}
	if fileSize(t, src) >= size {
		t.Error("the replacement is not smaller than the source was")
	}
	if !strings.Contains(r.logs.String(), "node encode") {
		t.Error("no record names the node that encoded the file")
	}
	if r.serverEncodes(src) != 0 {
		t.Error("the server encoded the file itself")
	}
	noTempBeside(t, r.root)
	emptyDir(t, scratch)
	emptyDir(t, work)
}

// TestNodes_ANodesCommandLineIsTheServersOwn: the command line the real worker runs for a
// leased job is the one the server builds for the same job itself, argument for argument,
// apart from the two paths the node substitutes - so the shared assembly cannot drift.
func TestNodes_ANodesCommandLineIsTheServersOwn(t *testing.T) {
	// The server's own, from a run with nodes off.
	local := newNodeRig(t, nil)
	local.eng.Nodes = nil
	lsrc := local.source("movie.mkv")
	if err := local.eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	own := local.argv.forSource(t, lsrc)

	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	nodeArgv := newArgvLog()
	var mapped string
	r.realWorker("nodeA", func(o *nodeworker.Options) {
		enc := FFmpegEncoder{FFmpeg: r.ffmpeg, argvObserver: nodeArgv.record}
		mapped = o.PathMap.Map(src)
		o.Encode = func(ctx context.Context, in, out string, pre, body []string, _ func(float64)) error {
			return enc.RunLeased(ctx, in, out, pre, body, nil)
		}
	})
	waitDone(t, r.offer(src))
	if j, _ := r.row(src); j.Status != store.Done {
		t.Fatalf("status = %s (%s), want done", j.Status, j.Outcome.Reason)
	}
	if mapped == src {
		t.Fatal("the path map mapped the source to itself; the mapping is not exercised")
	}
	got := nodeArgv.forSource(t, mapped)
	if len(got) != len(own) {
		t.Fatalf("the node's command line has %d arguments, the server's %d:\nnode:   %q\nserver: %q", len(got), len(own), got, own)
	}
	for i := range own {
		switch {
		case own[i] == lsrc:
			if got[i] != mapped {
				t.Errorf("argument %d: the node read %q, want its mapped source %q", i, got[i], mapped)
			}
		case i == len(own)-1:
			if strings.HasPrefix(got[i], r.root) {
				t.Errorf("the node wrote its output into the library: %s", got[i])
			}
		case got[i] != own[i]:
			t.Errorf("argument %d: node %q, server %q", i, got[i], own[i])
		}
	}
}

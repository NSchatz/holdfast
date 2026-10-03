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
	"github.com/NSchatz/holdfast/internal/encoder"
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
	hubMu           sync.Mutex
	hubStop         context.CancelFunc
	hubTune         func(*node.Options)
	ledger          *flakyLedger
	log             *slog.Logger
	srv             *httptest.Server
	argv            *argvLog
	logs            *lockedBuf
	clock           *leaseClock
	// stages counts the plan announcements by stage, so a test can ask whether the gates
	// ever ran.
	stageMu sync.Mutex
	stages  map[string]int
}

// rigWait bounds every wait for something that must happen. It is generous on purpose: the
// gate runs this suite under -race on a loaded host, and a wait that passes returns at once.
const rigWait = 5 * time.Minute

// flakyLedger is a lease ledger whose writes to an existing row can be made to fail. It is
// how a fixture leaves the ledger as a KILLED server leaves it: with it failing, a hub that is
// stopped cannot end its leases, so their rows stay granted.
type flakyLedger struct {
	store.LeaseLedger
	fail atomic.Bool
}

func (l *flakyLedger) UpdateLease(ctx context.Context, id string, decide func(node.Lease) (node.Lease, error)) (node.Lease, error) {
	if l.fail.Load() {
		return node.Lease{}, errors.New("the server was killed")
	}
	return l.LeaseLedger.UpdateLease(ctx, id, decide)
}

// newNodeRig builds an engine with nodes ON over a fresh library root.
func newNodeRig(t *testing.T, mutate func(*config.Config)) *nodeRig {
	t.Helper()
	return newNodeRigHub(t, mutate, nil)
}

// startHub builds the rig's hub over its ledger and hands it to the engine. The lease
// endpoints follow r.hub, so a rig can stand for a server that was restarted.
func (r *nodeRig) startHub() {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	r.hubStop = cancel
	o := node.Options{
		Ledger: r.ledger, BaseCtx: ctx, Version: "test", TTL: r.cfg.NodeLeaseTTL(),
		MaxLeases: r.cfg.EffectiveNodeMaxLeases(), MaxLeasesPerNode: r.cfg.EffectiveNodeMaxLeasesPerNode(),
		MaxTransfers: r.cfg.EffectiveNodeMaxTransfers(), LongPoll: time.Hour, RetryAfter: time.Second,
		SweepEvery: 20 * time.Millisecond, Now: r.clock.Now, Log: r.log,
	}
	if r.hubTune != nil {
		r.hubTune(&o)
	}
	hub := node.New(o)
	r.hubMu.Lock()
	r.hub = hub
	r.hubMu.Unlock()
	r.eng.Nodes = hub
}

func (r *nodeRig) currentHub() *node.Hub {
	r.hubMu.Lock()
	defer r.hubMu.Unlock()
	return r.hub
}

// kill stops the rig's server the way a SIGKILL does as far as the ledger can tell: every
// job in flight ends, and no lease row is ended.
func (r *nodeRig) kill() {
	r.ledger.fail.Store(true)
	r.hubStop()
}

// restart is the next start of the server on the same ledger and library: a new hub, and
// the leases its recovery returns. The caller adopts them and calls Ready.
func (r *nodeRig) restart() []node.Lease {
	r.t.Helper()
	r.ledger.fail.Store(false)
	r.startHub()
	live, err := r.hub.Recover(context.Background())
	if err != nil {
		r.t.Fatalf("Recover: %v", err)
	}
	return live
}

func newNodeRigHub(t *testing.T, mutate func(*config.Config), hubTune func(*node.Options)) *nodeRig {
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
	r.log = log
	r.ledger = &flakyLedger{LeaseLedger: r.ts.SQLite}
	r.hubTune = hubTune
	r.startHub()
	if _, err := r.hub.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	r.hub.Ready()
	mux := chi.NewRouter()
	mux.Route(nodePrefix, func(sub chi.Router) { node.Mount(sub, r.currentHub) })
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
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
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
	case <-time.After(rigWait):
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
	case <-time.After(rigWait):
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
	deadline := time.Now().Add(rigWait)
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
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
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
		r.eng.nodeFeederIdle = time.Hour
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
		r.eng.nodeFeederIdle = time.Hour
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
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
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
	deadline := time.Now().Add(rigWait)
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
	ctx, cancel := context.WithTimeout(context.Background(), rigWait)
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
	deadline := time.Now().Add(rigWait)
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
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d slot(s) held while the second job holds the only one: releasing twice gave two back", held)
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
	r.eng.nodeFeederIdle = time.Hour
	stop := r.eng.startFeeders(ctx, &wg, feed, func(context.Context, string, string) bool { return false })
	// Every feeder is parked at the full gate queue, and none has reserved the node's poll:
	// it is still there to reserve.
	parked := func() int {
		g := r.eng.gate()
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.parked
	}
	for deadline := time.Now().Add(rigWait); parked() != r.cfg.EffectiveNodeMaxLeases(); {
		if time.Now().After(deadline) {
			t.Fatalf("%d feeder(s) parked at the full gate queue, want %d", parked(), r.cfg.EffectiveNodeMaxLeases())
		}
		time.Sleep(time.Millisecond)
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
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

	// Three failed leases in a row with none succeeding: the node is cooling off too.
	if a := waitAnswer(t, r.poll("nodeA")); a.status != http.StatusServiceUnavailable || a.reason != "node_cooling_off" {
		t.Errorf("the failing node's next poll answered %d %q, want 503 node_cooling_off", a.status, a.reason)
	}
	// Offered once more, to another node: the claim holds the row out, nothing is leased, the
	// ticket goes back.
	lease := r.poll("nodeB")
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
	if a := r.post(leasePath(node.RouteFail, again.LeaseID), node.FailRequest{Epoch: again.Epoch, Reason: "encode_failed"}); a.status != http.StatusOK {
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
// apart from the two paths the node substitutes and the ONE intended difference: the server's
// libx265 pool figures describe the server, so the node is sent none.
func TestNodes_ANodesCommandLineIsTheServersOwn(t *testing.T) {
	pools := encoder.X265ParallelismFor(5)
	if pools.Params() == "" {
		t.Fatal("the fixture's libx265 parallelism is empty; the difference it proves would not exist")
	}
	withPools := func(r *nodeRig) {
		r.eng.Enc = FFmpegEncoder{FFmpeg: r.ffmpeg, Cfg: r.cfg, Probe: probe.New(r.ffmpeg, r.ffprobe),
			X265: pools, argvObserver: r.argv.record}
	}
	// The server's own, from a run with nodes off.
	local := newNodeRig(t, nil)
	withPools(local)
	local.eng.Nodes = nil
	lsrc := local.source("movie.mkv")
	if err := local.eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	own := local.argv.forSource(t, lsrc)

	r := newNodeRig(t, nil)
	withPools(r)
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
	if r.serverEncodes(src) != 0 {
		t.Fatal("the server encoded the file itself")
	}
	got := nodeArgv.forSource(t, mapped)
	if len(got) != len(own) {
		t.Fatalf("the node's command line has %d arguments, the server's %d:\nnode:   %q\nserver: %q", len(got), len(own), got, own)
	}
	differences := 0
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
		case i > 0 && own[i-1] == "-x265-params":
			differences++
			if !strings.Contains(own[i], pools.Params()) {
				t.Errorf("the server's own -x265-params %q does not carry its pool figures %q", own[i], pools.Params())
			}
			if want := strings.Replace(own[i], pools.Params(), "", 1); got[i] != want || strings.Contains(got[i], "pools=") {
				t.Errorf("the node's -x265-params = %q, want the server's without its pool figures: %q", got[i], want)
			}
		case got[i] != own[i]:
			t.Errorf("argument %d: node %q, server %q", i, got[i], own[i])
		}
	}
	if differences != 1 {
		t.Errorf("%d -x265-params arguments compared, want 1", differences)
	}
}

// reencode runs the leased command line as a node would, and returns the output's bytes.
func (r *nodeRig) reencode(l node.AcquireResponse) []byte {
	r.t.Helper()
	out := filepath.Join(r.t.TempDir(), "node-output")
	if err := (FFmpegEncoder{FFmpeg: r.ffmpeg}).RunLeased(context.Background(), l.Path, out, l.Pre, l.Body, nil); err != nil {
		r.t.Fatalf("the fake node's encode: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// deliver uploads a real encode of the leased job and completes the lease, honestly.
func (r *nodeRig) deliver(l node.AcquireResponse) {
	r.t.Helper()
	out := r.reencode(l)
	if a := r.put(l, out, digestOf(out)); a.status != http.StatusOK {
		r.t.Fatalf("upload: %d %s", a.status, a.body)
	}
	if a := r.post(leasePath(node.RouteComplete, l.LeaseID), node.CompleteRequest{Epoch: l.Epoch,
		OutputDigest: digestOf(out), SourceDigest: fileDigestOf(r.t, l.Path), OutputBytes: int64(len(out))}); a.status != http.StatusOK {
		r.t.Fatalf("complete: %d %s", a.status, a.body)
	}
}

func (r *nodeRig) failLease(l node.AcquireResponse, reason string) {
	r.t.Helper()
	if a := r.post(leasePath(node.RouteFail, l.LeaseID), node.FailRequest{Epoch: l.Epoch, Reason: reason}); a.status != http.StatusOK {
		r.t.Fatalf("fail: %d %s", a.status, a.body)
	}
}

func (r *nodeRig) leaseRow(id string) node.Lease {
	r.t.Helper()
	l, found, err := r.ts.GetLease(context.Background(), id)
	if err != nil || !found {
		r.t.Fatalf("GetLease(%s): found %v, %v", id, found, err)
	}
	return l
}

func (r *nodeRig) untilLog(what string, subs ...string) {
	r.t.Helper()
	for deadline := time.Now().Add(rigWait); ; time.Sleep(5 * time.Millisecond) {
		for _, line := range strings.Split(r.logs.String(), "\n") {
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(line, s)
			}
			if ok {
				return
			}
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s\n%s", what, r.logs.String())
		}
	}
}

// TestNodes_ANodeThatCannotRunALeaseCostsTheFileNothing: every typed reason a worker fails a
// lease with when IT could not run the job - its path map, its mount, its encoders, its own
// refusal of the command line, its own shutdown. The server encodes the job in the same
// attempt, nothing is recorded against the file, one record names the node and the reason,
// and three such endings in a row cool the node off.
func TestNodes_ANodeThatCannotRunALeaseCostsTheFileNothing(t *testing.T) {
	r := newNodeRig(t, nil)
	reasons := []string{"unmapped_source", "source_mismatch", "source_unreadable",
		"unsupported_encoder", "refused_plan", "worker_stopping"}
	for i, reason := range reasons {
		nodeName := "nodeA"
		if i >= 3 {
			nodeName = "nodeB"
		}
		src := r.source(reason + ".mkv")
		lease := r.poll(nodeName)
		done := r.offer(src)
		l := waitAnswer(t, lease).lease
		if l.LeaseID == "" {
			t.Fatalf("%s: no lease was granted", reason)
		}
		r.failLease(l, reason)
		waitDone(t, done)
		j, _ := r.row(src)
		if j.Status != store.Done || j.FailCount != 0 {
			t.Errorf("%s: status = %s, fail_count = %d (%s); want done and 0: the node's trouble is not the file's",
				reason, j.Status, j.FailCount, j.Outcome.Reason)
		}
		if r.serverEncodes(src) != 1 {
			t.Errorf("%s: the server did not encode the job in the same attempt", reason)
		}
		r.untilLog("the record of "+reason, "this job is encoded by the server", "the node could not run the lease",
			"node="+nodeName, "reason="+reason, "file="+src)
		// The third in a row from one node cools it off; the first two do not.
		cooling := strings.Count(r.logs.String(), "node cooling off")
		if want := (i + 1) / 3; cooling != want {
			t.Errorf("after %d ending(s) %d cool-off record(s), want %d", i+1, cooling, want)
		}
	}
	if !strings.Contains(r.logs.String(), `reasons="unmapped_source source_mismatch source_unreadable"`) {
		t.Errorf("the cool-off record does not name the reasons:\n%s", grepLines(r.logs.String(), "cooling off"))
	}
	for _, n := range []string{"nodeA", "nodeB"} {
		if a := waitAnswer(t, r.poll(n)); a.status != http.StatusServiceUnavailable || a.reason != "node_cooling_off" {
			t.Errorf("%s's next poll answered %d %q, want 503 node_cooling_off", n, a.status, a.reason)
		}
	}
	if strings.Contains(r.logs.String(), "retry bound parked") {
		t.Error("a file was parked for a node's trouble")
	}
	noTempBeside(t, r.root)
}

// TestNodes_OneMisconfiguredWorkerDoesNotParkTheLibrary: through the real pool, with the REAL
// worker and a wrong path map. Before the split of lease endings this worker took files as
// fast as the feed offered them, failed each in milliseconds, and after max_failures passes
// the library was parked. Now every file is done after one pass, none carries a failure, and
// the worker was cooled off after its third lease.
func TestNodes_OneMisconfiguredWorkerDoesNotParkTheLibrary(t *testing.T) {
	r := newNodeRig(t, func(c *config.Config) { c.Workers = 1 })
	var srcs []string
	for i := 0; i < 5; i++ {
		srcs = append(srcs, r.source("movie"+strconv.Itoa(i)+".mkv"))
	}
	r.realWorker("nodeA", func(o *nodeworker.Options) {
		o.PathMap = config.PathMap{{From: "/somewhere/else", To: "/mnt"}}
	})
	waitQueued(t, r)
	if err := r.eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		j, _ := r.row(s)
		if j.Status != store.Done || j.FailCount != 0 {
			t.Errorf("%s: status = %s, fail_count = %d; want done and 0", filepath.Base(s), j.Status, j.FailCount)
		}
	}
	logs := r.logs.String()
	if n := strings.Count(logs, "node lease granted"); n > node.DefaultCoolOffAfter {
		t.Errorf("the misconfigured worker was granted %d lease(s), want at most %d before its cool-off", n, node.DefaultCoolOffAfter)
	}
	if strings.Contains(logs, "retry bound parked") || strings.Contains(logs, "FAIL (node encode failed") {
		t.Errorf("a file was failed for the worker's wrong path map:\n%s", grepLines(logs, "FAIL"))
	}
	noTempBeside(t, r.root)
}

// cancelledPoll is a fake node's poll whose request can be ended by the test.
func (r *nodeRig) cancelledPoll(name string) (end func()) {
	ctx, cancel := context.WithCancel(context.Background())
	payload, _ := json.Marshal(node.AcquireRequest{Node: name, Version: "test", Slots: 1,
		Mode: node.ModeMapped, Encoders: []string{"cpu"}})
	left := make(chan struct{})
	go func() {
		defer close(left)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.srv.URL+nodePrefix+node.RouteLeases, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	return func() { cancel(); <-left }
}

// TestNodes_APollThatLeftCostsTheFileNothing: a node whose request ended - its client's
// timeout, a dropped connection - while a feeder held its poll. Nothing is wrong with the
// file and no node ever saw it, so no failure is recorded and no lease row is written: the
// job waits for another node to ask, and failing that the server encodes it.
func TestNodes_APollThatLeftCostsTheFileNothing(t *testing.T) {
	// take reserves the departed node's poll and ends its request, and returns the ticket
	// once the hub has seen the request end.
	take := func(t *testing.T, r *nodeRig) *node.Ticket {
		end := r.cancelledPoll("nodeA")
		ctx, cancel := context.WithTimeout(context.Background(), rigWait)
		defer cancel()
		tk, err := r.hub.WaitDemand(ctx)
		if err != nil {
			t.Fatal(err)
		}
		end()
		return tk
	}
	run := func(r *nodeRig, tk *node.Ticket, src string) <-chan error {
		done := make(chan error, 1)
		go func() {
			nj := &nodeJob{ticket: tk}
			err := r.eng.ProcessFile(withNodeJob(context.Background(), nj), "n0", src)
			r.eng.settle(context.Background(), nj)
			done <- err
		}()
		return done
	}

	t.Run("no other node asks: the server encodes it", func(t *testing.T) {
		r := newNodeRig(t, nil)
		r.eng.NodeRedemand = 200 * time.Millisecond
		src := r.source("movie.mkv")
		waitDone(t, run(r, take(t, r), src))
		j, _ := r.row(src)
		if j.Status != store.Done || j.FailCount != 0 {
			t.Fatalf("status = %s, fail_count = %d (%s); want done and 0", j.Status, j.FailCount, j.Outcome.Reason)
		}
		if r.serverEncodes(src) != 1 {
			t.Error("the server did not encode the job")
		}
		if strings.Contains(r.logs.String(), "node lease granted") {
			t.Error("a lease was granted to a node that had stopped waiting")
		}
		r.untilLog("the record", "this job is encoded by the server", "stopped waiting", "node=nodeA")
	})
	t.Run("another node asks: it is leased to that one", func(t *testing.T) {
		r := newNodeRig(t, nil)
		r.eng.NodeRedemand = rigWait
		src := r.source("movie.mkv")
		done := run(r, take(t, r), src)
		// The job is now waiting for fresh demand. nodeB asks, and gets it.
		l := waitAnswer(t, r.poll("nodeB")).lease
		if l.Path != src || l.Epoch != 1 {
			t.Fatalf("nodeB was leased %q at epoch %d, want %s at epoch 1: the departed node's ticket must have granted nothing", l.Path, l.Epoch, src)
		}
		r.deliver(l)
		waitDone(t, done)
		j, _ := r.row(src)
		if j.Status != store.Done || j.FailCount != 0 {
			t.Fatalf("status = %s, fail_count = %d (%s); want done and 0", j.Status, j.FailCount, j.Outcome.Reason)
		}
		if r.serverEncodes(src) != 0 {
			t.Error("the server encoded a job a node was asking for")
		}
	})
	t.Run("the default wait is two long-poll bounds", func(t *testing.T) {
		if DefaultNodeRedemand != 2*node.DefaultLongPoll || (&Engine{}).nodeRedemand() != DefaultNodeRedemand {
			t.Errorf("DefaultNodeRedemand = %s, want two long-poll bounds (%s)", DefaultNodeRedemand, 2*node.DefaultLongPoll)
		}
	})
}

// TestNodes_AFeederDoesNotSitOnAPollWhileTheFeedIsIdle: with nothing arriving from the feed -
// it is paused, or every file is with a local worker - a feeder lets the node's poll go after
// a short bound instead of holding it reserved, and the poll is answered with no work at its
// own long-poll bound.
func TestNodes_AFeederDoesNotSitOnAPollWhileTheFeedIsIdle(t *testing.T) {
	if (&Engine{}).feederIdle() != time.Second {
		t.Errorf("the default idle bound is %s, want 1s", (&Engine{}).feederIdle())
	}
	r := newNodeRigHub(t, nil, func(o *node.Options) { o.LongPoll = 2 * time.Second })
	r.eng.nodeFeederIdle = 20 * time.Millisecond
	lease := r.poll("nodeA")
	waitQueued(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	feed := make(chan string)
	var wg sync.WaitGroup
	stop := r.eng.startFeeders(ctx, &wg, feed, func(context.Context, string, string) bool {
		t.Error("a feeder processed a file nobody fed")
		return true
	})
	// The poll is answered 204 at its bound, though feeders reserved it again and again.
	if a := waitAnswer(t, lease); a.status != http.StatusNoContent {
		t.Errorf("the idle poll answered %d %s, want 204", a.status, a.body)
	}
	// And a poll held by an idle feeder comes back to the queue: with a long-poll bound far
	// away, the test itself can reserve it.
	slow := newNodeRig(t, nil)
	slow.eng.nodeFeederIdle = 20 * time.Millisecond
	held := slow.poll("nodeB")
	waitQueued(t, slow)
	sctx, scancel := context.WithCancel(context.Background())
	var swg sync.WaitGroup
	sstop := slow.eng.startFeeders(sctx, &swg, make(chan string), func(context.Context, string, string) bool { return false })
	waitReserved(t, slow)
	wctx, wcancel := context.WithTimeout(context.Background(), rigWait)
	tk, err := slow.hub.WaitDemand(wctx)
	wcancel()
	if err != nil {
		t.Fatal("an idle feeder never let its poll go")
	}
	slow.hub.Release(tk, node.ErrNoRoom)
	if a := waitAnswer(t, held); a.reason != "no_room" {
		t.Errorf("poll answered %d %q", a.status, a.reason)
	}
	cancel()
	scancel()
	wg.Wait()
	swg.Wait()
	stop()
	sstop()
}

// crashWithLeases grants one lease per source, each to its own fake node, then kills the
// server: the jobs end, the lease rows stay granted, the job rows stay active.
func (r *nodeRig) crashWithLeases(srcs ...string) []node.AcquireResponse {
	r.t.Helper()
	var leases []node.AcquireResponse
	var jobs []<-chan error
	for i, src := range srcs {
		poll := r.poll("node" + strconv.Itoa(i))
		jobs = append(jobs, r.offer(src))
		l := waitAnswer(r.t, poll).lease
		if l.Path != src {
			r.t.Fatalf("leased %q, want %s", l.Path, src)
		}
		leases = append(leases, l)
	}
	r.kill()
	for _, done := range jobs {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				r.t.Fatalf("a job of the killed server ended with %v, want the interruption", err)
			}
		case <-time.After(rigWait):
			r.t.Fatal("a job of the killed server did not end")
		}
	}
	for _, l := range leases {
		if row := r.leaseRow(l.LeaseID); row.State != store.LeaseGranted {
			r.t.Fatalf("the fixture needs the lease still granted after the kill; it is %s", row.State)
		}
		if j, _ := r.row(l.Path); j.Status != store.Encoding {
			r.t.Fatalf("the fixture needs the job row left active; it is %s", j.Status)
		}
	}
	return leases
}

// TestNodes_ARestartTakesTwoLiveLeasesBackAtOnce: two leases were live when the server was
// killed. The restart runs both jobs at once - the first is held before its seam until the
// second has taken its lease back, which a serial adoption could never satisfy - re-graces
// both at Ready, and both nodes then finish the encodes they were leased before the restart.
func TestNodes_ARestartTakesTwoLiveLeasesBackAtOnce(t *testing.T) {
	r := newNodeRig(t, nil)
	a, b := r.source("a.mkv"), r.source("b.mkv")
	leases := r.crashWithLeases(a, b)
	live := r.restart()
	if len(live) != 2 {
		t.Fatalf("Recover returned %d lease(s), want 2", len(live))
	}
	secondBack := make(chan struct{})
	r.eng.onClaim = func(worker, _ string) {
		if worker == "adopt0" {
			<-secondBack
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adopted := make(chan func(), 1)
	go func() { adopted <- r.eng.AdoptLeases(ctx, live, time.Hour) }()
	r.untilLog("the second lease's adoption", "node lease adopted after a restart", "lease="+live[1].ID)
	select {
	case <-adopted:
		t.Fatal("AdoptLeases returned while the first lease's job had not reached the seam")
	default:
	}
	close(secondBack)
	wait := <-adopted
	r.untilLog("the first lease's adoption", "node lease adopted after a restart", "lease="+live[0].ID)
	// The engine's start-up took most of a TTL. Ready gives both leases a whole one again.
	r.clock.advance(r.cfg.NodeLeaseTTL() - 2*time.Second)
	r.hub.Ready()
	r.clock.advance(r.cfg.NodeLeaseTTL() - 2*time.Second)
	for _, l := range leases {
		if hb := r.post(leasePath(node.RouteHeartbeat, l.LeaseID), node.HeartbeatRequest{Epoch: l.Epoch}); hb.status != http.StatusOK {
			t.Fatalf("a heartbeat inside the grace Ready gave answered %d %s", hb.status, hb.body)
		}
	}
	for _, l := range leases {
		r.deliver(l)
	}
	wait()
	for _, src := range []string{a, b} {
		if j, _ := r.row(src); j.Status != store.Done || j.FailCount != 0 {
			t.Errorf("%s: status = %s, fail_count = %d (%s); want done", filepath.Base(src), j.Status, j.FailCount, j.Outcome.Reason)
		}
		if r.serverEncodes(src) != 0 {
			t.Errorf("%s: the server encoded a job its node finished", filepath.Base(src))
		}
	}
	noTempBeside(t, r.root)
}

// TestNodes_ARecoveredLeaseWhoseJobDoesNotComeBackIsAbandonedBeforeReady: the three ways a
// recovered lease's job never takes it back. Each lease is ended - not left to run out -
// before the hub may grant again, and the node's next heartbeat is 410.
func TestNodes_ARecoveredLeaseWhoseJobDoesNotComeBackIsAbandonedBeforeReady(t *testing.T) {
	for name, tc := range map[string]struct {
		before func(t *testing.T, r *nodeRig, src string)
		// unchanged says the library must be exactly what it was.
		unchanged bool
	}{
		"the file is now withheld": {func(t *testing.T, r *nodeRig, src string) {
			if _, err := r.ts.ExcludePath(context.Background(), src); err != nil {
				t.Fatal(err)
			}
		}, true},
		"the file is gone": {func(t *testing.T, r *nodeRig, src string) {
			if err := os.Remove(src); err != nil {
				t.Fatal(err)
			}
		}, true},
		"the plan is no longer leasable": {func(_ *testing.T, r *nodeRig, _ string) {
			// An adoption has no ticket to ask about encoders, so the plan itself is changed:
			// the restarted server stream-copies the picture, which it does itself.
			yes := true
			cfg := r.cfg
			cfg.RemuxOnly = &yes
			r.replaceEngine(cfg)
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newNodeRig(t, nil)
			src := r.source("movie.mkv")
			before := md5f(t, src)
			l := r.crashWithLeases(src)[0]
			tc.before(t, r, src)
			live := r.restart()
			if len(live) != 1 {
				t.Fatalf("Recover returned %d lease(s), want 1", len(live))
			}
			wait := r.eng.AdoptLeases(context.Background(), live, time.Hour)
			// BEFORE Ready: the lease is ended already.
			if row := r.leaseRow(l.LeaseID); row.State != store.LeaseExpired || row.Reason != string(node.ReasonNotAdopted) {
				t.Fatalf("before Ready the lease is %s (%s), want it abandoned", row.State, row.Reason)
			}
			r.hub.Ready()
			if hb := r.post(leasePath(node.RouteHeartbeat, l.LeaseID), node.HeartbeatRequest{Epoch: l.Epoch}); hb.status != http.StatusGone {
				t.Errorf("the node's next heartbeat answered %d, want 410", hb.status)
			}
			wait()
			if strings.Contains(r.logs.String(), "node lease adopted") {
				t.Error("the lease was taken back")
			}
			if tc.unchanged {
				if _, err := os.Stat(src); err == nil && md5f(t, src) != before {
					t.Error("the source changed")
				}
				ents, _ := os.ReadDir(r.root)
				for _, e := range ents {
					if e.Name() != "movie.mkv" {
						t.Errorf("the library gained %s", e.Name())
					}
				}
				if r.serverEncodes(src) != 0 {
					t.Error("the server encoded the file")
				}
			} else {
				r.untilLog("the record", "this job is encoded by the server", "stream-copied")
			}
		})
	}
}

// replaceEngine swaps the rig's engine for one built from cfg, over the same store and
// library: the restarted server runs another configuration.
func (r *nodeRig) replaceEngine(cfg config.Config) {
	prober := probe.New(r.ffmpeg, r.ffprobe)
	r.cfg = cfg
	r.eng = New(cfg, prober, FFmpegEncoder{FFmpeg: r.ffmpeg, Cfg: cfg, Probe: prober, argvObserver: r.argv.record}, r.ts, r.log)
}

// TestNodes_ARecoveredLeaseNotTakenBackInTimeIsAbandonedAndStartUpIsNotHeld: a recovered
// lease's job is held up before its seam. AdoptLeases returns at its bound - the listener is
// not held - with the lease abandoned; the job, when it gets there, finds its lease gone and
// is encoded by the server.
func TestNodes_ARecoveredLeaseNotTakenBackInTimeIsAbandonedAndStartUpIsNotHeld(t *testing.T) {
	r := newNodeRig(t, nil)
	src := r.source("movie.mkv")
	l := r.crashWithLeases(src)[0]
	live := r.restart()
	held := make(chan struct{})
	r.eng.onClaim = func(string, string) { <-held }
	wait := r.eng.AdoptLeases(context.Background(), live, 100*time.Millisecond)
	if row := r.leaseRow(l.LeaseID); row.State != store.LeaseExpired || row.Reason != string(node.ReasonNotAdopted) {
		t.Fatalf("at the bound the lease is %s (%s), want it abandoned", row.State, row.Reason)
	}
	r.untilLog("the record", "not taken back within the start-up bound", "lease="+l.LeaseID, "node=node0")
	r.hub.Ready()
	if hb := r.post(leasePath(node.RouteHeartbeat, l.LeaseID), node.HeartbeatRequest{Epoch: l.Epoch}); hb.status != http.StatusGone {
		t.Errorf("the node's next heartbeat answered %d, want 410", hb.status)
	}
	close(held)
	wait()
	j, _ := r.row(src)
	if j.Status != store.Done || j.FailCount != 0 {
		t.Fatalf("status = %s, fail_count = %d (%s); want done and 0", j.Status, j.FailCount, j.Outcome.Reason)
	}
	if r.serverEncodes(src) != 1 {
		t.Error("the server did not encode the job whose lease was abandoned")
	}
	r.untilLog("the record", "this job is encoded by the server", "its recovered lease was abandoned")
	noTempBeside(t, r.root)
}

// TestNodes_TheSourceHashStopsWhenTheServerDoes: the server's own hash of a source is a read
// as long as the film, and a cancelled context ends it.
func TestNodes_TheSourceHashStopsWhenTheServerDoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	body := bytes.Repeat([]byte("holdfast"), 1<<18) // 2 MiB: more than one read
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sourceDigest(context.Background(), path)
	if err != nil || got != digestOf(body) {
		t.Fatalf("sourceDigest = %q, %v; want %q", got, err, digestOf(body))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sourceDigest(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("sourceDigest on a cancelled context = %v, want the cancellation", err)
	}
	if _, err := sourceDigest(context.Background(), path+".missing"); err == nil {
		t.Error("sourceDigest of a missing file returned no error")
	}
}

// TestNodes_NodeAttemptsAreDroppedAtEveryOtherTerminalOutcome: what the engine remembers
// about a file's node attempts lives only while the file's last outcome is a failed node
// attempt.
func TestNodes_NodeAttemptsAreDroppedAtEveryOtherTerminalOutcome(t *testing.T) {
	r := newNodeRig(t, nil)
	remembered := func(path string) int {
		r.eng.nodeAttemptsMu.Lock()
		defer r.eng.nodeAttemptsMu.Unlock()
		return len(r.eng.nodeAttempts[path])
	}
	err := &node.LeaseError{Node: "nodeA", Epoch: 1, Reason: node.ReasonExpired}
	for _, status := range []store.Status{store.Done, store.Skipped, store.Failed} {
		path := filepath.Join(r.root, string(status)+".mkv")
		if !r.eng.nodeFailure(path, err) {
			t.Fatal("a lease error was not read as a node failure")
		}
		// The failed row of the node attempt itself keeps what was remembered.
		r.eng.finishStore(context.Background(), path, "key", store.Failed, &store.Outcome{})
		if remembered(path) != 1 {
			t.Fatalf("the node failure's own row dropped the attempt")
		}
		// Any later terminal outcome of the file drops it.
		r.eng.finishStore(context.Background(), path, "key", status, &store.Outcome{})
		if n := remembered(path); n != 0 {
			t.Errorf("after a %s outcome %d attempt(s) are still remembered", status, n)
		}
	}
	if r.eng.nodeFailure("x", errors.New("an ordinary encode error")) {
		t.Error("an ordinary error was read as a node failure")
	}
}

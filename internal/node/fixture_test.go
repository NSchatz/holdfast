package node

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
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/NSchatz/holdfast/internal/store"
)

// The fixtures: an httptest server mounting the lease endpoints, a fake worker speaking
// HTTP to it, a temp SQLite ledger, a clock the test holds, and a goroutine standing in for
// the engine by calling WaitDemand and Encode.

const (
	testVersion = "v0.0.0-fixture"
	mount       = "/api/node/v1"
)

// clock is the server clock a fixture holds. It only moves when the test moves it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// space is the free-space answer a fixture holds.
type space struct {
	mu    sync.Mutex
	free  uint64
	err   error
	asked []string
}

func (s *space) of(dir string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, dir)
	return s.free, s.err
}

func (s *space) set(free uint64, err error) {
	s.mu.Lock()
	s.free, s.err = free, err
	s.mu.Unlock()
}

type fixture struct {
	t      *testing.T
	dir    string // the library directory: sources, and the working files beside them
	dbPath string
	st     *store.SQLite
	clk    *clock
	space  *space
	hub    *Hub
	srv    *httptest.Server
	stop   context.CancelFunc
}

// newFixture builds a ready hub. tune adjusts its options before it is built.
func newFixture(t *testing.T, tune func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), clk: &clock{now: t0}, space: &space{free: 1 << 40}}
	f.dbPath = newLedgerFile(t)
	f.open(tune)
	if _, err := f.hub.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	f.hub.Ready()
	return f
}

// ledgerTemplate is one migrated, empty ledger, built once: every fixture starts from a
// copy of it rather than running the whole schema history again.
var ledgerTemplate struct {
	once  sync.Once
	bytes []byte
	err   error
}

// newLedgerFile returns the path of a fresh, migrated, empty SQLite ledger.
func newLedgerFile(t *testing.T) string {
	t.Helper()
	ledgerTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "node-ledger-template")
		if err != nil {
			ledgerTemplate.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "jobs.db")
		st, err := store.Open(path)
		if err != nil {
			ledgerTemplate.err = err
			return
		}
		if err := st.Close(); err != nil {
			ledgerTemplate.err = err
			return
		}
		ledgerTemplate.bytes, ledgerTemplate.err = os.ReadFile(path)
	})
	if ledgerTemplate.err != nil {
		t.Fatalf("building the ledger template: %v", ledgerTemplate.err)
	}
	path := filepath.Join(t.TempDir(), "jobs.db")
	if err := os.WriteFile(path, ledgerTemplate.bytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// open opens the ledger and builds a hub and a server on it, neither recovered nor ready.
func (f *fixture) open(tune func(*Options)) {
	f.t.Helper()
	st, err := store.Open(f.dbPath)
	if err != nil {
		f.t.Fatalf("open the ledger: %v", err)
	}
	f.st = st
	base, stop := context.WithCancel(context.Background())
	f.stop = stop
	o := Options{
		Ledger: st, BaseCtx: base, Version: testVersion, TTL: ttl,
		MaxLeases: 4, MaxLeasesPerNode: 1, MaxTransfers: 2,
		LongPoll: 5 * time.Second, RetryAfter: 7 * time.Second, SweepEvery: 15 * time.Millisecond,
		Now: f.clk.Now, FreeSpace: f.space.of,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tune != nil {
		tune(&o)
	}
	f.hub = New(o)
	r := chi.NewRouter()
	r.Route(mount, func(r chi.Router) { Mount(r, func() *Hub { return f.hub }) })
	f.srv = httptest.NewServer(r)
	srv, st2 := f.srv, st
	f.t.Cleanup(func() {
		stop()
		srv.Close()
		_ = st2.Close()
	})
}

// job writes a source of size bytes into the library directory and returns the job the
// engine would offer for it.
func (f *fixture) job(name string, size int) Job {
	f.t.Helper()
	src := filepath.Join(f.dir, name+".mkv")
	if err := os.WriteFile(src, bytes.Repeat([]byte{'s'}, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		f.t.Fatal(err)
	}
	return Job{
		Path: src, Key: "fp-" + name, Temp: filepath.Join(f.dir, name+".__transcoding__.mkv.holdfast-part"),
		Pre: []string{"-nostdin", "-y"}, Body: []string{"-map", "0", "-c:v", "libx265", "-crf", "22"},
		Encoder: "libx265", SourceSize: fi.Size(), SourceModTime: fi.ModTime(), ReservedBytes: fi.Size(),
	}
}

// files lists the library directory.
func (f *fixture) files() []string {
	f.t.Helper()
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// onlySources asserts the library directory holds exactly the named sources and nothing
// else: no working file, no partial upload.
func (f *fixture) onlySources(names ...string) {
	f.t.Helper()
	var want []string
	for _, n := range names {
		want = append(want, n+".mkv")
	}
	sort.Strings(want)
	got := f.files()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		f.t.Errorf("the library directory holds %v, want exactly %v", got, want)
	}
}

// engineCall is the test goroutine standing in for the engine on one job.
type engineCall struct {
	done   chan struct{}
	res    Result
	err    error
	cancel context.CancelFunc

	mu       sync.Mutex
	progress []float64
}

func (c *engineCall) report(f float64) {
	c.mu.Lock()
	c.progress = append(c.progress, f)
	c.mu.Unlock()
}

func (c *engineCall) reported() []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float64(nil), c.progress...)
}

// wait returns the call's result, failing the test if it has not returned in time.
func (c *engineCall) wait(t *testing.T) (Result, error) {
	t.Helper()
	select {
	case <-c.done:
		return c.res, c.err
	case <-time.After(10 * time.Second):
		t.Fatal("the engine's call did not return")
		return Result{}, nil
	}
}

func (c *engineCall) returned() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// offer starts the engine's side for one job: wait for a node, lease it the job.
func (f *fixture) offer(job Job) *engineCall {
	ctx, cancel := context.WithCancel(context.Background())
	c := &engineCall{done: make(chan struct{}), cancel: cancel}
	f.t.Cleanup(cancel)
	go func() {
		defer close(c.done)
		t, err := f.hub.WaitDemand(ctx)
		if err != nil {
			c.err = err
			return
		}
		c.res, c.err = f.hub.Encode(ctx, t, job, c.report)
	}()
	return c
}

// reply is one HTTP answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) errorType(t *testing.T) string {
	t.Helper()
	var e ErrorResponse
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("status %d body %q is not an ErrorResponse: %v", r.status, r.body, err)
	}
	return e.Error
}

func (f *fixture) do(req *http.Request) reply {
	f.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: body}
}

func (f *fixture) post(path string, v any) reply {
	f.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		f.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+mount+path, bytes.NewReader(b))
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(req)
}

func acquireBody(node string) AcquireRequest {
	return AcquireRequest{Node: node, Version: testVersion, Slots: 1, Mode: ModeMapped, Encoders: []string{"libx265", "libsvtav1"}}
}

func (f *fixture) acquire(node string) reply { return f.post(RouteLeases, acquireBody(node)) }

// grant leases job to node: the engine offers, the node asks, and the answer must be a
// lease.
func (f *fixture) grant(node string, job Job) (AcquireResponse, *engineCall) {
	f.t.Helper()
	call := f.offer(job)
	r := f.acquire(node)
	if r.status != http.StatusOK {
		f.t.Fatalf("acquire by %s answered %d %s, want a lease", node, r.status, r.body)
	}
	var a AcquireResponse
	if err := json.Unmarshal(r.body, &a); err != nil {
		f.t.Fatalf("the acquire body %q is not a lease: %v", r.body, err)
	}
	return a, call
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return FormatDigest(sum[:])
}

func leasePath(route, id string) string {
	return mount + "/leases/" + id + route
}

// put uploads body on a lease, declaring digest.
func (f *fixture) put(id string, epoch int64, body []byte, digest string) reply {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPut, f.srv.URL+leasePath("/output", id), bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set(EpochHeader, fmt.Sprint(epoch))
	req.Header.Set("Content-Digest", digest)
	return f.do(req)
}

func (f *fixture) heartbeat(id string, epoch int64, progress float64) reply {
	return f.post("/leases/"+id+"/heartbeat", HeartbeatRequest{Epoch: epoch, Progress: progress})
}

func (f *fixture) complete(id string, epoch int64, out []byte, srcDigest string) reply {
	return f.post("/leases/"+id+"/complete", CompleteRequest{Epoch: epoch, OutputDigest: digestOf(out),
		SourceDigest: srcDigest, OutputBytes: int64(len(out)), EncodeSec: 12.5})
}

func (f *fixture) fail(id string, epoch int64, reason string) reply {
	return f.post("/leases/"+id+"/fail", FailRequest{Epoch: epoch, Reason: reason})
}

func (f *fixture) row(id string) Lease {
	f.t.Helper()
	l, ok, err := f.st.GetLease(context.Background(), id)
	if err != nil || !ok {
		f.t.Fatalf("lease %s: present=%v err=%v", id, ok, err)
	}
	return l
}

// is410 asserts an answer is 410 Gone, typed, and marked no-store.
func is410(t *testing.T, what string, r reply) {
	t.Helper()
	if r.status != http.StatusGone {
		t.Errorf("%s answered %d %s, want 410", what, r.status, r.body)
		return
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s answered 410 with Cache-Control %q, want no-store", what, got)
	}
	if got := r.errorType(t); got != "lease_gone" {
		t.Errorf("%s answered 410 typed %q, want lease_gone", what, got)
	}
}

// is503 asserts an answer is 503 with the fixture's Retry-After and the typed reason.
func is503(t *testing.T, what string, r reply, reason string) {
	t.Helper()
	if r.status != http.StatusServiceUnavailable {
		t.Errorf("%s answered %d %s, want 503", what, r.status, r.body)
		return
	}
	if got := r.header.Get("Retry-After"); got != "7" {
		t.Errorf("%s answered 503 with Retry-After %q, want 7", what, got)
	}
	if got := r.errorType(t); got != reason {
		t.Errorf("%s answered 503 typed %q, want %q", what, got, reason)
	}
}

func isTyped(t *testing.T, what string, r reply, status int, reason string) {
	t.Helper()
	if r.status != status {
		t.Errorf("%s answered %d %s, want %d", what, r.status, r.body, status)
		return
	}
	if got := r.errorType(t); got != reason {
		t.Errorf("%s answered %d typed %q, want %q", what, status, got, reason)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) queuedPolls() int {
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	return len(f.hub.polls)
}

func leaseErr(t *testing.T, err error) *LeaseError {
	t.Helper()
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("the engine's call returned %v, want a *LeaseError", err)
	}
	return le
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

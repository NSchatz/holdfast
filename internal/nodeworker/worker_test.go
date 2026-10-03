package nodeworker

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/secret"
)

// The worker loop against a fake server that speaks the lease protocol by hand, so every
// answer a real server could give - and several it never would - is put to the worker. No
// ffmpeg runs here: the encode is a function the test supplies.

// waitFor bounds every wait for something that must happen. It is generous on purpose: the
// gate runs this suite under -race on a loaded host, and a wait that passes returns at once.
const waitFor = 120 * time.Second

const (
	testToken   = "node-credential-for-tests"
	testVersion = "v-test"
	leaseID     = "0123456789abcdef0123456789abcdef"
)

// reply is one canned HTTP answer.
type reply struct {
	status int
	ctype  string
	body   string
	retry  string
	// location makes the reply a redirect to that address.
	location string
}

func jsonReply(status int, v any) reply {
	b, _ := json.Marshal(v)
	return reply{status: status, ctype: "application/json; charset=utf-8", body: string(b)}
}

func errReply(status int, reason string) reply {
	return jsonReply(status, node.ErrorResponse{Error: reason, Detail: "said by the fake server"})
}

// call is one request the fake server received.
type call struct {
	method, path string
	auth         string
	header       http.Header
	body         []byte
	length       int64
}

// fakeServer answers each route from a queue of replies; the last reply of a queue repeats.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []call
	replies map[string][]reply
	// idle is the acquire answer once the acquire queue has run out: no work.
	hang chan struct{}
	// source, when set, answers the source route itself (source_test.go).
	source http.HandlerFunc
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, replies: map[string][]reply{}, hang: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	// The held polls are let go first and the client connections dropped, so closing the
	// server cannot wait on a request; and the close is bounded all the same.
	t.Cleanup(func() {
		close(f.hang)
		f.srv.CloseClientConnections()
		closed := make(chan struct{})
		go func() { defer close(closed); f.srv.Close() }()
		select {
		case <-closed:
		case <-time.After(time.Minute):
			t.Error("the fake server did not close within a minute; the test goes on without it rather than hang")
		}
	})
	return f
}

func routeOf(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/api/node/v1")
	switch {
	case p == "/leases":
		return "acquire"
	case strings.HasSuffix(p, "/heartbeat"):
		return "heartbeat"
	case strings.HasSuffix(p, "/output"):
		return "output"
	case strings.HasSuffix(p, "/source"):
		return "source"
	case strings.HasSuffix(p, "/complete"):
		return "complete"
	case strings.HasSuffix(p, "/fail"):
		return "fail"
	}
	return "unknown:" + r.URL.Path
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	route := routeOf(r)
	f.mu.Lock()
	f.calls = append(f.calls, call{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		header: r.Header.Clone(), body: body, length: r.ContentLength})
	source := f.source
	if route == "source" && source != nil {
		f.mu.Unlock()
		source(w, r)
		return
	}
	q := f.replies[route]
	var rep reply
	switch {
	case len(q) > 1:
		rep, f.replies[route] = q[0], q[1:]
	case len(q) == 1:
		rep = q[0]
		if route == "acquire" {
			// A lease is granted once; after the queue runs out there is no more work.
			f.replies[route] = nil
		}
	default:
		rep = reply{status: -1}
	}
	f.mu.Unlock()
	if rep.status == -1 {
		if route == "acquire" {
			// No more work: hold the poll until the test ends, as a long-poll would.
			select {
			case <-f.hang:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		rep = jsonReply(http.StatusOK, map[string]string{"state": "ok"})
	}
	if rep.ctype != "" {
		w.Header().Set("Content-Type", rep.ctype)
	}
	if rep.retry != "" {
		w.Header().Set("Retry-After", rep.retry)
	}
	if rep.location != "" {
		w.Header().Set("Location", rep.location)
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (f *fakeServer) queue(route string, r ...reply) {
	f.mu.Lock()
	f.replies[route] = append(f.replies[route], r...)
	f.mu.Unlock()
}

func (f *fakeServer) seen(route string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		r, _ := http.NewRequest(c.method, c.path, nil)
		if routeOf(r) == route {
			out = append(out, c)
		}
	}
	return out
}

// failReasons is the typed reasons the worker failed leases with, in order.
func (f *fakeServer) failReasons() []string {
	var out []string
	for _, c := range f.seen("fail") {
		var req node.FailRequest
		_ = json.Unmarshal(c.body, &req)
		out = append(out, req.Reason)
	}
	return out
}

// rig is one worker under test.
type rig struct {
	t      *testing.T
	f      *fakeServer
	lib    string // the library as the SERVER names it
	mount  string // the same directory as the worker reaches it
	work   string
	src    string // the source, as the server names it
	mapped string
	opts   Options

	mu      sync.Mutex
	encodes [][]string
	sleeps  []time.Duration
	// output is what the fake encode writes.
	output []byte
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	r := &rig{t: t, f: newFakeServer(t), lib: filepath.Join(d, "server", "media"), mount: filepath.Join(d, "mnt"),
		work: filepath.Join(d, "work"), output: []byte("encoded output")}
	if err := os.MkdirAll(r.lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r.lib, r.mount); err != nil {
		t.Fatal(err)
	}
	r.src = filepath.Join(r.lib, "movie.mkv")
	r.mapped = filepath.Join(r.mount, "movie.mkv")
	if err := os.WriteFile(r.src, []byte(strings.Repeat("source bytes ", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.opts = Options{
		Server: r.f.srv.URL, Token: secret.NewValue(testToken), Name: "nodeA", Version: testVersion,
		PathMap: config.PathMap{{From: r.lib, To: r.mount}}, WorkDir: r.work, Encoders: []string{"cpu", "x264"},
		MinBackoff: 40 * time.Millisecond, MaxBackoff: 320 * time.Millisecond,
		Jitter: func() float64 { return 0 },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sleep: func(ctx context.Context, d time.Duration) error {
			r.mu.Lock()
			r.sleeps = append(r.sleeps, d)
			r.mu.Unlock()
			return sleep(ctx, 2*time.Millisecond)
		},
		Encode: func(_ context.Context, in, out string, pre, body []string, progress func(float64)) error {
			r.mu.Lock()
			r.encodes = append(r.encodes, append([]string{in, out}, append(append([]string(nil), pre...), body...)...))
			output := r.output
			r.mu.Unlock()
			progress(5)
			return os.WriteFile(out, output, 0o600)
		},
		Duration: func(context.Context, string) (float64, bool) { return 10, true },
	}
	return r
}

// lease is the acquire answer of a lease on the rig's source.
func (r *rig) lease() node.AcquireResponse {
	st, err := os.Stat(r.src)
	if err != nil {
		r.t.Fatal(err)
	}
	return node.AcquireResponse{LeaseID: leaseID, Epoch: 7, TTLSec: 4, HeartbeatSec: 1, Mode: node.ModeMapped,
		Path: r.src, SourceSize: st.Size(), SourceMtimeNS: st.ModTime().UnixNano(), Encoder: "cpu",
		Pre: []string{}, Body: []string{"-c:v", "libx265", "-f", "matroska"}, MaxOutputBytes: st.Size() - 1}
}

func (r *rig) encodeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.encodes)
}

// run starts the worker and returns a stop that ends it and reports what Run returned.
func (r *rig) run() (stop func() error) {
	r.t.Helper()
	w, err := New(r.opts)
	if err != nil {
		r.t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(waitFor):
				r.t.Error("the worker did not stop; the test goes on without it rather than hang")
			}
		})
		return runErr
	}
	r.t.Cleanup(func() { _ = stop() })
	return stop
}

// until waits for cond.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func workDirEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range ents {
		t.Errorf("the work directory still holds %s", e.Name())
	}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return node.FormatDigest(sum[:])
}

// TestWorker_RefusesPlainHTTPToANonLoopbackServer is D8: the node credential never crosses a
// network in cleartext. https is accepted, plain http only to a loopback host, and a worker
// pointed anywhere else is refused before it sends a byte.
func TestWorker_RefusesPlainHTTPToANonLoopbackServer(t *testing.T) {
	for _, tc := range []struct {
		server   string
		ok       bool
		insecure bool
	}{
		{"https://holdfast.example.net", true, false},
		{"https://10.0.0.5:8443/", true, false},
		{"http://127.0.0.1:8080", true, false},
		{"http://127.8.9.1:8080", true, false},
		{"http://localhost:8080", true, false},
		{"http://[::1]:8080", true, false},
		{" http://127.0.0.1:8080 ", true, false},
		{"http://10.0.0.5:8080", false, true},
		{"http://holdfast.example.net", false, true},
		{"http://localhost.example.net:8080", false, true},
		{"http://[2001:db8::1]:8080", false, true},
		{"http://0.0.0.0:8080", false, true},
		{"ftp://127.0.0.1/", false, false},
		{"127.0.0.1:8080", false, false},
		{"http://", false, false},
		{"", false, false},
	} {
		err := CheckServer(tc.server)
		if (err == nil) != tc.ok {
			t.Errorf("CheckServer(%q) = %v, want accepted = %v", tc.server, err, tc.ok)
		}
		if errors.Is(err, ErrInsecureServer) != tc.insecure {
			t.Errorf("CheckServer(%q) = %v, want the cleartext refusal = %v", tc.server, err, tc.insecure)
		}
		if err != nil && !strings.Contains(err.Error(), "worker_server") {
			t.Errorf("the refusal of %q does not name worker_server: %v", tc.server, err)
		}
	}

	// And through New: a worker pointed at a non-loopback http server is never built, so it
	// never sends its credential.
	r := newRig(t)
	r.opts.Server = "http://10.0.0.5:8080"
	if _, err := New(r.opts); !errors.Is(err, ErrInsecureServer) {
		t.Fatalf("New with a plain-http, non-loopback server = %v, want the cleartext refusal", err)
	}
	if n := len(r.f.seen("acquire")); n != 0 {
		t.Errorf("%d request(s) were sent", n)
	}
}

// TestWorker_NewRefusesWhatAWorkerCannotRunWithout: each missing requirement refuses, naming
// the key an operator sets.
func TestWorker_NewRefusesWhatAWorkerCannotRunWithout(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Options)
		want   string
	}{
		"no server":      {func(o *Options) { o.Server = "  " }, "worker_server is not set"},
		"no token":       {func(o *Options) { o.Token = secret.Value{} }, "node_token is not set"},
		"no name":        {func(o *Options) { o.Name = "" }, "worker_name"},
		"a bad name":     {func(o *Options) { o.Name = "node/one" }, "worker_name"},
		"no encoders":    {func(o *Options) { o.Encoders = nil }, "could encode nothing"},
		"no encode":      {func(o *Options) { o.Encode = nil }, "required"},
		"no version":     {func(o *Options) { o.Version = "" }, "required"},
		"no work dir":    {func(o *Options) { o.WorkDir = "" }, "required"},
		"http elsewhere": {func(o *Options) { o.Server = "http://192.0.2.7" }, "cleartext"},
	} {
		r := newRig(t)
		tc.mutate(&r.opts)
		_, err := New(r.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, want a refusal naming %q", name, err, tc.want)
		}
	}
	// The defaults a zero option takes.
	r := newRig(t)
	r.opts.Slots, r.opts.MinBackoff, r.opts.MaxBackoff, r.opts.HTTP, r.opts.Sleep, r.opts.Jitter, r.opts.Log = 0, 0, 0, nil, nil, nil, nil
	r.opts.Server = r.opts.Server + "/"
	w, err := New(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	if w.o.Slots != 1 || w.o.MinBackoff != DefaultMinBackoff || w.o.MaxBackoff != DefaultMaxBackoff ||
		w.o.HTTP == nil || w.o.Sleep == nil || w.o.Log == nil {
		t.Errorf("defaults: slots %d, backoff %s to %s", w.o.Slots, w.o.MinBackoff, w.o.MaxBackoff)
	}
	if j := w.o.Jitter(); j < 0 || j >= 1 {
		t.Errorf("the default jitter returned %v, want a number in [0, 1)", j)
	}
	if w.base != r.f.srv.URL+"/api/node/v1" {
		t.Errorf("base = %q", w.base)
	}
	// A minimum past the default maximum keeps the two in order.
	r.opts.MinBackoff, r.opts.MaxBackoff = 10*time.Minute, time.Second
	if w, _ := New(r.opts); w.o.MaxBackoff != 10*time.Minute {
		t.Errorf("MaxBackoff = %s with a 10m minimum, want 10m", w.o.MaxBackoff)
	}
	if DefaultMinBackoff != 5*time.Second || DefaultMaxBackoff != 5*time.Minute {
		t.Errorf("the backoff bounds are %s to %s; P4 rule 8 says 5 s to 5 min", DefaultMinBackoff, DefaultMaxBackoff)
	}
}

// TestWorker_BackoffDoublesFromTheMinimumToTheMaximumWithJitter pins the backoff arithmetic.
func TestWorker_BackoffDoublesFromTheMinimumToTheMaximumWithJitter(t *testing.T) {
	jitter := 0.0
	b := &backoff{min: 4 * time.Second, max: 20 * time.Second, jitter: func() float64 { return jitter }}
	// With no jitter each wait is half its step: 4, 8, 16, then capped at 20.
	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		if got := b.next(0); got != want {
			t.Errorf("wait %d = %s, want %s", i, got, want)
		}
	}
	b.reset()
	jitter = 0.5
	if got := b.next(0); got != 3*time.Second {
		t.Errorf("after a reset, with jitter 0.5, the first wait = %s, want 3s (half the 4s step plus half of the rest)", got)
	}
	// A Retry-After the server stated is a floor, never a ceiling.
	if got := b.next(30 * time.Second); got != 30*time.Second {
		t.Errorf("wait with a 30s Retry-After = %s, want 30s", got)
	}
	if got := b.next(time.Second); got != 12*time.Second {
		t.Errorf("wait with a 1s Retry-After at the 16s step = %s, want 12s", got)
	}
	jitter = 0.999999
	b.reset()
	if got := b.next(0); got <= 3*time.Second || got >= 4*time.Second {
		t.Errorf("with jitter just under 1 the first wait = %s, want just under the 4s step", got)
	}
}

// TestWorkerFixture_AnAcquireAnswerThatIsNotALeaseIsNeverEncoded is the Unmanic #635 class at
// the worker's end: whatever an acquire is answered with that is not a 200 carrying the JSON
// of a whole lease - an error page, a 200 that is not JSON, a JSON object that is not a lease
// - the worker encodes nothing, fails nothing, and asks again after backing off.
func TestWorkerFixture_AnAcquireAnswerThatIsNotALeaseIsNeverEncoded(t *testing.T) {
	r := newRig(t)
	good := r.lease()
	with := func(mutate func(*node.AcquireResponse)) reply {
		l := good
		mutate(&l)
		return jsonReply(http.StatusOK, l)
	}
	goodJSON, _ := json.Marshal(good)
	notLeases := map[string]reply{
		"a 404 with a body":             {status: 404, ctype: "text/html", body: "<html><body><h1>404 Not Found</h1></body></html>"},
		"a 404 carrying a lease's JSON": {status: 404, ctype: "application/json", body: string(goodJSON)},
		"a 200 that is not JSON":        {status: 200, ctype: "text/plain", body: "OK"},
		"a 200 HTML page":               {status: 200, ctype: "application/json", body: "<html>welcome to the proxy</html>"},
		"a lease under the wrong type":  {status: 200, ctype: "text/html", body: string(goodJSON)},
		"a lease with no content type":  {status: 200, body: string(goodJSON)},
		"a 201":                         {status: 201, ctype: "application/json", body: string(goodJSON)},
		"a 500":                         {status: 500, ctype: "text/plain", body: "boom"},
		"an empty object":               {status: 200, ctype: "application/json", body: "{}"},
		"an unknown field":              {status: 200, ctype: "application/json", body: strings.Replace(string(goodJSON), "{", `{"surprise":1,`, 1)},
		"no lease id":                   with(func(l *node.AcquireResponse) { l.LeaseID = "" }),
		"a short lease id":              with(func(l *node.AcquireResponse) { l.LeaseID = "abcd" }),
		"a lease id that is not hex":    with(func(l *node.AcquireResponse) { l.LeaseID = strings.Repeat("z", 32) }),
		"no epoch":                      with(func(l *node.AcquireResponse) { l.Epoch = 0 }),
		"http mode":                     with(func(l *node.AcquireResponse) { l.Mode = node.ModeHTTP }),
		"no path":                       with(func(l *node.AcquireResponse) { l.Path = "" }),
		"no encoder":                    with(func(l *node.AcquireResponse) { l.Encoder = "" }),
		"no command line":               with(func(l *node.AcquireResponse) { l.Body = nil }),
		"a one-byte source":             with(func(l *node.AcquireResponse) { l.SourceSize, l.MaxOutputBytes = 1, 0 }),
		"no output cap":                 with(func(l *node.AcquireResponse) { l.MaxOutputBytes = 0 }),
		"a cap at the source's size":    with(func(l *node.AcquireResponse) { l.MaxOutputBytes = l.SourceSize }),
		"no heartbeat interval":         with(func(l *node.AcquireResponse) { l.HeartbeatSec = 0 }),
		"no ttl":                        with(func(l *node.AcquireResponse) { l.TTLSec = 0 }),
	}
	for name, rep := range notLeases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			// The same answer, as often as the worker asks.
			r.f.queue("acquire", rep, rep, rep, rep)
			stop := r.run()
			until(t, "the worker to ask again", func() bool { return len(r.f.seen("acquire")) >= 3 })
			if err := stop(); err != nil {
				t.Errorf("Run = %v, want a clean stop", err)
			}
			if n := r.encodeCount(); n != 0 {
				t.Fatalf("the worker encoded %d time(s) on an answer that is not a lease", n)
			}
			for _, route := range []string{"heartbeat", "output", "complete", "fail"} {
				if n := len(r.f.seen(route)); n != 0 {
					t.Errorf("the worker made %d %s call(s) on an answer that is not a lease", n, route)
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.sleeps) < 2 || r.sleeps[0] != r.opts.MinBackoff/2 || r.sleeps[1] != r.opts.MinBackoff {
				t.Errorf("the worker waited %v before asking again, want a backoff from %s", r.sleeps, r.opts.MinBackoff/2)
			}
			workDirEmpty(t, r.work)
		})
	}
	_ = r
}

// TestWorker_RunsALeaseEndToEndAgainstTheProtocol is the loop's whole happy path, read off
// what the fake server received: the acquire, the heartbeat with progress, the upload with
// its declared length, digest and epoch, the completion with both digests - every call
// carrying the credential - and an empty work directory after.
func TestWorker_RunsALeaseEndToEndAgainstTheProtocol(t *testing.T) {
	r := newRig(t)
	l := r.lease()
	r.f.queue("acquire", jsonReply(200, l))
	gate := make(chan struct{})
	inner := r.opts.Encode
	r.opts.Encode = func(ctx context.Context, in, out string, pre, body []string, progress func(float64)) error {
		err := inner(ctx, in, out, pre, body, progress)
		select { // hold the encode until a heartbeat has gone out
		case <-gate:
		case <-ctx.Done():
		}
		return err
	}
	stop := r.run()
	until(t, "a heartbeat", func() bool { return len(r.f.seen("heartbeat")) >= 1 })
	close(gate)
	until(t, "the completion", func() bool { return len(r.f.seen("complete")) >= 1 })
	until(t, "the next poll", func() bool { return len(r.f.seen("acquire")) >= 2 })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}

	var acq node.AcquireRequest
	first := r.f.seen("acquire")[0]
	if err := json.Unmarshal(first.body, &acq); err != nil {
		t.Fatal(err)
	}
	if acq.Node != "nodeA" || acq.Version != testVersion || acq.Slots != 1 || acq.Mode != node.ModeMapped ||
		strings.Join(acq.Encoders, ",") != "cpu,x264" {
		t.Errorf("acquire = %+v", acq)
	}
	if first.method != http.MethodPost || first.path != "/api/node/v1/leases" {
		t.Errorf("acquire was %s %s", first.method, first.path)
	}
	r.f.mu.Lock()
	for _, c := range r.f.calls {
		if c.auth != "Bearer "+testToken {
			t.Errorf("%s %s carried Authorization %q", c.method, c.path, c.auth)
		}
	}
	r.f.mu.Unlock()

	r.mu.Lock()
	enc := r.encodes
	r.mu.Unlock()
	wantOut := filepath.Join(r.work, leaseID+".7.out")
	if len(enc) != 1 || enc[0][0] != r.mapped || enc[0][1] != wantOut ||
		strings.Join(enc[0][2:], " ") != "-c:v libx265 -f matroska" {
		t.Fatalf("encodes = %v, want one of the MAPPED source %s into %s with the lease's options", enc, r.mapped, wantOut)
	}

	var hb node.HeartbeatRequest
	hbCall := r.f.seen("heartbeat")[0]
	_ = json.Unmarshal(hbCall.body, &hb)
	if hb.Epoch != 7 || hb.Progress != 0.5 {
		t.Errorf("heartbeat = %+v, want epoch 7 and progress 0.5 (5 s of a 10 s source)", hb)
	}
	if hbCall.path != "/api/node/v1/leases/"+leaseID+"/heartbeat" {
		t.Errorf("heartbeat path = %s", hbCall.path)
	}
	r.mu.Lock()
	hasBeat := false
	for _, d := range r.sleeps {
		if d == time.Second {
			hasBeat = true
		}
	}
	r.mu.Unlock()
	if !hasBeat {
		t.Error("the worker did not wait heartbeat_sec between beats")
	}

	puts := r.f.seen("output")
	if len(puts) != 1 {
		t.Fatalf("%d uploads, want 1", len(puts))
	}
	put := puts[0]
	if put.method != http.MethodPut || put.path != "/api/node/v1/leases/"+leaseID+"/output" ||
		string(put.body) != string(r.output) || put.length != int64(len(r.output)) ||
		put.header.Get("Content-Digest") != digest(r.output) || put.header.Get(node.EpochHeader) != "7" {
		t.Errorf("upload: %s %s, %d bytes declared, digest %q, epoch %q", put.method, put.path, put.length,
			put.header.Get("Content-Digest"), put.header.Get(node.EpochHeader))
	}

	var done node.CompleteRequest
	_ = json.Unmarshal(r.f.seen("complete")[0].body, &done)
	src, _ := os.ReadFile(r.src)
	if done.Epoch != 7 || done.OutputDigest != digest(r.output) || done.SourceDigest != digest(src) ||
		done.OutputBytes != int64(len(r.output)) || done.EncodeSec <= 0 {
		t.Errorf("complete = %+v", done)
	}
	if got := r.f.failReasons(); len(got) != 0 {
		t.Errorf("the worker failed a lease it completed: %v", got)
	}
	workDirEmpty(t, r.work)
}

// TestWorker_AnUnmappedSourceFailsTheLeaseAndNeverGuesses: the leased source is a path no
// worker_path_map entry covers. The file EXISTS at that very path on this host, so a worker
// that passed an unmapped path through would encode it; this one fails the lease
// `unmapped_source` and encodes nothing.
func TestWorker_AnUnmappedSourceFailsTheLeaseAndNeverGuesses(t *testing.T) {
	for name, pm := range map[string]config.PathMap{
		"an empty map":                      nil,
		"a map of another tree":             {{From: "/srv/elsewhere", To: "/mnt/elsewhere"}},
		"a sibling with a prefix in common": nil, // filled in below
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			if name == "a sibling with a prefix in common" {
				pm = config.PathMap{{From: r.lib + "-2", To: r.mount}}
			}
			r.opts.PathMap = pm
			if _, err := os.Stat(r.src); err != nil {
				t.Fatalf("the unmapped path must exist on the worker for this to prove anything: %v", err)
			}
			r.f.queue("acquire", jsonReply(200, r.lease()))
			r.run()
			until(t, "the lease to be failed", func() bool { return len(r.f.failReasons()) >= 1 })
			if got := r.f.failReasons(); got[0] != "unmapped_source" || ReasonUnmappedSource != "unmapped_source" {
				t.Errorf("failed with %v, want unmapped_source", got)
			}
			var req node.FailRequest
			c := r.f.seen("fail")[0]
			_ = json.Unmarshal(c.body, &req)
			if req.Epoch != 7 || c.path != "/api/node/v1/leases/"+leaseID+"/fail" {
				t.Errorf("fail = %+v at %s", req, c.path)
			}
			if r.encodeCount() != 0 || len(r.f.seen("output")) != 0 {
				t.Error("the worker encoded or uploaded a source it could not map")
			}
		})
	}
}

// TestWorker_RefusesALeaseItCannotRunWithATypedFail: a source that is not the size or the
// modification time the lease was granted on, one that is not there, an encoder the worker's
// probe did not find, an encode that fails and an output that is not strictly smaller are
// each a typed fail - never an encode of something else, never an upload.
func TestWorker_RefusesALeaseItCannotRunWithATypedFail(t *testing.T) {
	for name, tc := range map[string]struct {
		lease   func(*rig, *node.AcquireResponse)
		want    string
		encodes int
	}{
		"another size":                  {func(_ *rig, l *node.AcquireResponse) { l.SourceSize++; l.MaxOutputBytes++ }, ReasonSourceMismatch, 0},
		"an mtime past the tolerance":   {func(_ *rig, l *node.AcquireResponse) { l.SourceMtimeNS += int64(MtimeTolerance) + 1 }, ReasonSourceMismatch, 0},
		"an mtime before the tolerance": {func(_ *rig, l *node.AcquireResponse) { l.SourceMtimeNS -= int64(MtimeTolerance) + 1 }, ReasonSourceMismatch, 0},
		"options before the input":      {func(_ *rig, l *node.AcquireResponse) { l.Pre = []string{"-init_hw_device", "x"} }, ReasonRefusedPlan, 0},
		"a second input":                {func(_ *rig, l *node.AcquireResponse) { l.Body = append(l.Body, "-i", "other.mkv") }, ReasonRefusedPlan, 0},
		"a missing file":                {func(r *rig, _ *node.AcquireResponse) { _ = os.Remove(r.src) }, ReasonSourceUnreadable, 0},
		"a directory":                   {func(r *rig, l *node.AcquireResponse) { l.Path = r.lib }, ReasonSourceMismatch, 0},
		"another encoder":               {func(_ *rig, l *node.AcquireResponse) { l.Encoder = "nvenc" }, ReasonUnsupportedEncoder, 0},
		"a failed encode": {func(r *rig, _ *node.AcquireResponse) {
			r.opts.Encode = func(context.Context, string, string, []string, []string, func(float64)) error {
				r.mu.Lock()
				r.encodes = append(r.encodes, nil)
				r.mu.Unlock()
				return errors.New("ffmpeg exited 1")
			}
		}, ReasonEncodeFailed, 1},
		"an output as large as the cap allows plus one": {func(r *rig, l *node.AcquireResponse) {
			r.output = make([]byte, l.MaxOutputBytes+1)
		}, ReasonOutputTooLarge, 1},
		"an empty output": {func(r *rig, _ *node.AcquireResponse) { r.output = nil }, ReasonOutputTooLarge, 1},
		"no output at all": {func(r *rig, _ *node.AcquireResponse) {
			r.opts.Encode = func(context.Context, string, string, []string, []string, func(float64)) error {
				r.mu.Lock()
				r.encodes = append(r.encodes, nil)
				r.mu.Unlock()
				return nil
			}
		}, ReasonEncodeFailed, 1},
		"a source rewritten while it was encoded": {func(r *rig, _ *node.AcquireResponse) {
			inner := r.opts.Encode
			r.opts.Encode = func(ctx context.Context, in, out string, pre, body []string, p func(float64)) error {
				if err := os.WriteFile(r.src, []byte("rewritten under the encoder"), 0o644); err != nil {
					return err
				}
				return inner(ctx, in, out, pre, body, p)
			}
		}, ReasonSourceMismatch, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			l := r.lease()
			tc.lease(r, &l)
			r.f.queue("acquire", jsonReply(200, l))
			r.run()
			until(t, "the lease to be failed", func() bool { return len(r.f.failReasons()) >= 1 })
			if got := r.f.failReasons(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("failed with %v, want [%s]", got, tc.want)
			}
			if got := r.encodeCount(); got != tc.encodes {
				t.Errorf("%d encode(s), want %d", got, tc.encodes)
			}
			if n := len(r.f.seen("output")); n != 0 {
				t.Errorf("%d upload(s) of a lease the worker failed", n)
			}
			if n := len(r.f.seen("complete")); n != 0 {
				t.Errorf("%d completion(s) of a lease the worker failed", n)
			}
			until(t, "the worker to ask again", func() bool { return len(r.f.seen("acquire")) >= 2 })
			workDirEmpty(t, r.work)
		})
	}
	// The largest output the cap admits IS uploaded: the bound is the cap itself.
	r := newRig(t)
	l := r.lease()
	r.output = make([]byte, l.MaxOutputBytes)
	r.f.queue("acquire", jsonReply(200, l))
	r.run()
	until(t, "the completion", func() bool { return len(r.f.seen("complete")) >= 1 })
	if got := r.f.failReasons(); len(got) != 0 {
		t.Errorf("an output at exactly the cap was failed: %v", got)
	}
}

// TestWorker_AGoneLeaseStopsTheEncodeAtOnceAndDiscardsTheOutput: a heartbeat answered 410
// cancels the encode in flight. Nothing is uploaded, nothing is completed, and the lease -
// which is no longer this worker's - is not failed either.
func TestWorker_AGoneLeaseStopsTheEncodeAtOnceAndDiscardsTheOutput(t *testing.T) {
	r := newRig(t)
	r.f.queue("acquire", jsonReply(200, r.lease()))
	// A refused beat that is not a 410 does not stop the encode; the 410 after it does.
	r.f.queue("heartbeat", errReply(500, "internal"), errReply(http.StatusGone, "lease_gone"))
	stopped := make(chan struct{})
	r.opts.Encode = func(ctx context.Context, _, out string, _, _ []string, _ func(float64)) error {
		r.mu.Lock()
		r.encodes = append(r.encodes, nil)
		r.mu.Unlock()
		_ = os.WriteFile(out, []byte("half an output"), 0o600)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	r.run()
	select {
	case <-stopped:
	case <-time.After(waitFor):
		t.Fatal("the encode was not stopped by the 410")
	}
	until(t, "the worker to ask again", func() bool { return len(r.f.seen("acquire")) >= 2 })
	if n := len(r.f.seen("heartbeat")); n != 2 {
		t.Errorf("%d heartbeats, want 2: the 500 is tried again, the 410 ends the lease", n)
	}
	for _, route := range []string{"output", "complete", "fail"} {
		if n := len(r.f.seen(route)); n != 0 {
			t.Errorf("%d %s call(s) on a lease that is gone", n, route)
		}
	}
	workDirEmpty(t, r.work)
}

// TestWorker_StoppingFailsTheLeaseAndExitsCleanly: SIGTERM while encoding. The encode stops,
// the lease is failed `worker_stopping`, the output is removed, and Run returns nil.
func TestWorker_StoppingFailsTheLeaseAndExitsCleanly(t *testing.T) {
	r := newRig(t)
	r.f.queue("acquire", jsonReply(200, r.lease()))
	started := make(chan struct{})
	r.opts.Encode = func(ctx context.Context, _, out string, _, _ []string, _ func(float64)) error {
		_ = os.WriteFile(out, []byte("half an output"), 0o600)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	stop := r.run()
	select {
	case <-started:
	case <-time.After(waitFor):
		t.Fatal("the encode never started")
	}
	if err := stop(); err != nil {
		t.Fatalf("Run = %v, want nil: a stop is a clean exit", err)
	}
	if got := r.f.failReasons(); len(got) != 1 || got[0] != ReasonWorkerStopping {
		t.Errorf("failed with %v, want [worker_stopping]", got)
	}
	if n := len(r.f.seen("output")); n != 0 {
		t.Errorf("%d upload(s) from a stopping worker", n)
	}
	workDirEmpty(t, r.work)
}

// TestWorker_UploadIsRetriedOnlyWhereTheServerSaysItMayBe: a digest mismatch and a 503 are
// sent again while the lease lives (the 503 after its Retry-After); a 410 ends the lease
// with nothing more said; a refusal the server will not change its mind about is not sent
// again and fails the lease; and the worker's own bound stops a server that never admits.
func TestWorker_UploadIsRetriedOnlyWhereTheServerSaysItMayBe(t *testing.T) {
	ok := jsonReply(200, node.UploadResponse{State: "uploaded"})
	unavailable := errReply(503, "transfers_full")
	unavailable.retry = "7"
	for name, tc := range map[string]struct {
		replies  []reply
		puts     int
		complete bool
		fail     string
	}{
		"a digest mismatch, then admitted":     {[]reply{errReply(400, "digest_mismatch"), ok}, 2, true, ""},
		"a 503, then admitted":                 {[]reply{unavailable, ok}, 2, true, ""},
		"an upload in progress, then admitted": {[]reply{errReply(409, "upload_in_progress"), ok}, 2, true, ""},
		"a short body, then admitted":          {[]reply{errReply(400, "short_body"), ok}, 2, true, ""},
		"a stalled upload, then admitted":      {[]reply{errReply(408, "upload_stalled"), ok}, 2, true, ""},
		"gone":                                 {[]reply{errReply(410, "lease_gone"), ok}, 1, false, ""},
		"too large":                            {[]reply{errReply(413, "too_large"), ok}, 1, false, ReasonUploadRefused},
		"a conflicting digest":                 {[]reply{errReply(409, "digest_conflict"), ok}, 1, false, ReasonUploadRefused},
		"a bad request":                        {[]reply{errReply(400, "bad_request"), ok}, 1, false, ReasonUploadRefused},
		"an error page":                        {[]reply{{status: 502, ctype: "text/html", body: "<html>bad gateway</html>"}, ok}, 1, false, ReasonUploadRefused},
		"never admitted":                       {[]reply{errReply(400, "digest_mismatch")}, MaxUploadAttempts, false, ReasonUploadRefused},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.queue("acquire", jsonReply(200, r.lease()))
			r.f.queue("output", tc.replies...)
			r.run()
			until(t, "the worker to ask again", func() bool { return len(r.f.seen("acquire")) >= 2 })
			if n := len(r.f.seen("output")); n != tc.puts {
				t.Errorf("%d upload(s), want %d", n, tc.puts)
			}
			if got := len(r.f.seen("complete")) == 1; got != tc.complete {
				t.Errorf("completed = %v, want %v", got, tc.complete)
			}
			got := strings.Join(r.f.failReasons(), ",")
			if got != tc.fail {
				t.Errorf("failed with %q, want %q", got, tc.fail)
			}
			if name == "a 503, then admitted" {
				r.mu.Lock()
				found := false
				for _, d := range r.sleeps {
					found = found || d == 7*time.Second
				}
				r.mu.Unlock()
				if !found {
					t.Errorf("the worker did not wait the server's Retry-After of 7 s before sending again: %v", r.sleeps)
				}
			}
			workDirEmpty(t, r.work)
		})
	}
	if MaxUploadAttempts != 5 {
		t.Errorf("MaxUploadAttempts = %d", MaxUploadAttempts)
	}
}

// TestWorker_CompletionIsRetriedOnA503AndStandsOtherwise.
func TestWorker_CompletionIsRetriedOnA503AndStandsOtherwise(t *testing.T) {
	ok := jsonReply(200, node.CompleteResponse{State: "completed"})
	busy := errReply(503, "not_ready")
	busy.retry = "9"
	for name, tc := range map[string]struct {
		replies []reply
		calls   int
	}{
		"a 503, then recorded": {[]reply{busy, ok}, 2},
		"recorded":             {[]reply{ok}, 1},
		"gone":                 {[]reply{errReply(410, "lease_gone"), ok}, 1},
		"a conflict":           {[]reply{errReply(409, "digest_conflict"), ok}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.queue("acquire", jsonReply(200, r.lease()))
			r.f.queue("complete", tc.replies...)
			r.run()
			until(t, "the worker to ask again", func() bool { return len(r.f.seen("acquire")) >= 2 })
			if n := len(r.f.seen("complete")); n != tc.calls {
				t.Errorf("%d completion call(s), want %d", n, tc.calls)
			}
			if got := r.f.failReasons(); len(got) != 0 {
				t.Errorf("the worker failed the lease after its upload was admitted: %v", got)
			}
			if tc.calls == 2 {
				r.mu.Lock()
				found := false
				for _, d := range r.sleeps {
					found = found || d == 9*time.Second
				}
				r.mu.Unlock()
				if !found {
					t.Errorf("the worker did not wait the Retry-After of 9 s: %v", r.sleeps)
				}
			}
			workDirEmpty(t, r.work)
		})
	}
}

// TestWorker_AcquireBacksOffAsTheServerAsks: no work is asked for again after the server's
// Retry-After, a refusal backs off exponentially and never under its Retry-After, and a
// server that cannot be reached is backed off from too.
func TestWorker_AcquireBacksOffAsTheServerAsks(t *testing.T) {
	t.Run("no work", func(t *testing.T) {
		r := newRig(t)
		noWork := reply{status: http.StatusNoContent, retry: "3"}
		r.f.queue("acquire", noWork, noWork, noWork, reply{status: http.StatusNoContent}, noWork)
		r.run()
		until(t, "five polls", func() bool { return len(r.f.seen("acquire")) >= 5 })
		r.mu.Lock()
		defer r.mu.Unlock()
		// The server's Retry-After each time, never growing; with none stated, half the minimum.
		want := []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second, r.opts.MinBackoff / 2}
		for i, w := range want {
			if r.sleeps[i] != w {
				t.Fatalf("waits after no work = %v, want %v", r.sleeps[:len(want)], want)
			}
		}
	})
	t.Run("a refusal", func(t *testing.T) {
		r := newRig(t)
		capped := errReply(503, "global_cap")
		capped.retry = "0"
		floor := errReply(503, "no_room")
		floor.retry = "11"
		r.f.queue("acquire", capped, capped, capped, capped, capped, floor,
			reply{status: http.StatusNoContent, retry: "1"}, capped, capped)
		r.run()
		until(t, "nine polls", func() bool { return len(r.f.seen("acquire")) >= 9 })
		r.mu.Lock()
		defer r.mu.Unlock()
		m := r.opts.MinBackoff
		// Half of 40, 80, 160, 320 and 320 ms (the maximum); then the Retry-After as a floor;
		// then the no-work wait, which also resets the backoff to its minimum.
		want := []time.Duration{m / 2, m, 2 * m, 4 * m, 4 * m, 11 * time.Second, time.Second, m / 2}
		for i, w := range want {
			if r.sleeps[i] != w {
				t.Fatalf("waits = %v, want %v", r.sleeps[:len(want)], want)
			}
		}
	})
	t.Run("a server that is not there", func(t *testing.T) {
		r := newRig(t)
		r.f.srv.Close()
		r.run()
		until(t, "two waits", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sleeps) >= 2 })
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sleeps[0] != r.opts.MinBackoff/2 || r.sleeps[1] != r.opts.MinBackoff {
			t.Errorf("waits = %v, want a backoff from %s", r.sleeps[:2], r.opts.MinBackoff/2)
		}
		if len(r.encodes) != 0 {
			t.Error("the worker encoded with no server")
		}
	})
}

// TestWorker_AnotherVersionIsFatalAndNamesBoth: a 409 version_mismatch stops the worker, and
// the error names this worker's version and the server's. Any other 409 is a refusal to back
// off from.
func TestWorker_AnotherVersionIsFatalAndNamesBoth(t *testing.T) {
	r := newRig(t)
	r.opts.Slots = 3
	r.f.queue("acquire", jsonReply(409, node.ErrorResponse{Error: "version_mismatch", ServerVersion: "v9.9.9", WorkerVersion: testVersion}),
		reply{status: 409, body: "{}"}, reply{status: 409, body: "{}"}, reply{status: 409, body: "{}"})
	w, err := New(r.opts)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrVersionMismatch) || !strings.Contains(err.Error(), "v9.9.9") || !strings.Contains(err.Error(), testVersion) {
			t.Fatalf("Run = %v, want the version mismatch naming %s and v9.9.9", err, testVersion)
		}
	case <-time.After(waitFor):
		t.Fatal("a version mismatch did not stop the worker")
	}
	if r.encodeCount() != 0 {
		t.Error("the worker encoded")
	}

	other := newRig(t)
	other.f.queue("acquire", errReply(409, "something_else"), errReply(409, "something_else"), errReply(409, "something_else"))
	stop := other.run()
	until(t, "the worker to ask again", func() bool { return len(other.f.seen("acquire")) >= 3 })
	if err := stop(); err != nil {
		t.Errorf("Run = %v after a 409 that is not a version mismatch, want it retried", err)
	}
}

// TestWorker_EverySlotAsksForItsOwnLease: worker_slots polls at once, each for one lease.
func TestWorker_EverySlotAsksForItsOwnLease(t *testing.T) {
	r := newRig(t)
	r.opts.Slots = 3
	stop := r.run()
	until(t, "three polls in flight", func() bool { return len(r.f.seen("acquire")) >= 3 })
	// Each slot is now inside its one poll, which the fake server holds. Stopping the worker
	// ends them, and no slot can have sent a second.
	_ = stop()
	if n := len(r.f.seen("acquire")); n != 3 {
		t.Errorf("%d polls in flight from 3 slots", n)
	}
	for _, c := range r.f.seen("acquire") {
		var req node.AcquireRequest
		_ = json.Unmarshal(c.body, &req)
		if req.Slots != 1 {
			t.Errorf("a poll asked for %d leases; each asks for one", req.Slots)
		}
	}
}

// TestWorker_TheWorkDirectoryIsCreatedAndAnUnusableOneRefuses.
func TestWorker_TheWorkDirectoryIsCreatedAndAnUnusableOneRefuses(t *testing.T) {
	r := newRig(t)
	r.opts.WorkDir = filepath.Join(r.work, "nested", "deeper")
	stop := r.run()
	until(t, "a poll", func() bool { return len(r.f.seen("acquire")) >= 1 })
	_ = stop()
	st, err := os.Stat(r.opts.WorkDir)
	if err != nil || !st.IsDir() {
		t.Fatalf("the work directory was not created: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("the work directory is %o, want 0700: an output is a copy of library media", st.Mode().Perm())
	}

	blocked := newRig(t)
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.opts.WorkDir = filepath.Join(file, "work")
	w, err := New(blocked.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "worker_work_dir") {
		t.Errorf("Run with a work directory that cannot be made = %v, want a refusal naming worker_work_dir", err)
	}
	if n := len(blocked.f.seen("acquire")); n != 0 {
		t.Errorf("%d poll(s) from a worker with nowhere to write", n)
	}
}

// TestWorker_ProgressIsAFractionOfTheSourceAndOneWhileUploading.
func TestWorker_ProgressIsAFractionOfTheSourceAndOneWhileUploading(t *testing.T) {
	for name, tc := range map[string]struct {
		duration func(context.Context, string) (float64, bool)
		pos      float64
		want     string
	}{
		"a quarter":            {func(context.Context, string) (float64, bool) { return 20, true }, 5, `"progress":0.25`},
		"past the end":         {func(context.Context, string) (float64, bool) { return 4, true }, 5, `"progress":1`},
		"an unknown length":    {func(context.Context, string) (float64, bool) { return 0, false }, 5, `"progress":0`},
		"no duration function": {nil, 5, `"progress":0`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.opts.Duration = tc.duration
			r.f.queue("acquire", jsonReply(200, r.lease()))
			gate := make(chan struct{})
			r.opts.Encode = func(ctx context.Context, _, out string, _, _ []string, progress func(float64)) error {
				progress(tc.pos)
				select {
				case <-gate:
				case <-ctx.Done():
				}
				return os.WriteFile(out, r.output, 0o600)
			}
			r.run()
			until(t, "a heartbeat", func() bool { return len(r.f.seen("heartbeat")) >= 1 })
			if body := string(r.f.seen("heartbeat")[0].body); !strings.Contains(body, tc.want+"}") {
				t.Errorf("heartbeat = %s, want %s", body, tc.want)
			}
			close(gate)
			until(t, "the completion", func() bool { return len(r.f.seen("complete")) >= 1 })
		})
	}
}

func TestWorker_RetryAfterReadsWholeSecondsOnly(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"5": 5 * time.Second, "0": 0, "": 0, "-3": 0, "soon": 0, "Wed, 21 Oct 2026 07:28:00 GMT": 0,
	} {
		resp := &http.Response{Header: http.Header{}}
		if value != "" {
			resp.Header.Set("Retry-After", value)
		}
		if got := retryAfter(resp); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", value, got, want)
		}
	}
	if got := fmt.Sprint(leaseRoute(node.RouteFail, "abc")); got != "/leases/abc/fail" {
		t.Errorf("leaseRoute = %s", got)
	}
}

// TestWorker_AModificationTimeWithinTwoSecondsIsTheSameSource: a source read through another
// mount can show another modification time (FAT keeps two-second stamps, SMB and NFS round).
// Up to MtimeTolerance either way the lease is run - the server's own comparison of the source
// digest is the proof - and the size is still exact.
func TestWorker_AModificationTimeWithinTwoSecondsIsTheSameSource(t *testing.T) {
	if MtimeTolerance != 2*time.Second {
		t.Fatalf("MtimeTolerance = %s, want 2s", MtimeTolerance)
	}
	for _, drift := range []time.Duration{0, MtimeTolerance, -MtimeTolerance, time.Nanosecond} {
		r := newRig(t)
		l := r.lease()
		l.SourceMtimeNS += int64(drift)
		r.f.queue("acquire", jsonReply(200, l))
		r.run()
		until(t, "the completion", func() bool { return len(r.f.seen("complete")) >= 1 })
		if got := r.f.failReasons(); len(got) != 0 {
			t.Errorf("a source whose mtime is %s off was failed: %v", drift, got)
		}
	}
	// The size is never tolerated: one byte off is another file.
	r := newRig(t)
	l := r.lease()
	l.SourceSize--
	l.MaxOutputBytes--
	if same, err := sameSource(r.mapped, &l); err != nil || same {
		t.Errorf("sameSource with a size one byte off = %v, %v; want false", same, err)
	}
}

// TestWorker_ALeaseItFailedItselfIsFollowedByABackoff: whatever stopped this worker running
// a lease - its path map, its mount, its encoders - is still there a moment later, so it
// backs off, exponentially, before it asks again. A lease it completed resets the backoff.
func TestWorker_ALeaseItFailedItselfIsFollowedByABackoff(t *testing.T) {
	r := newRig(t)
	r.opts.PathMap = nil // every lease is unmapped
	lease := jsonReply(200, r.lease())
	r.f.queue("acquire", lease, lease, lease, lease)
	r.run()
	until(t, "four failed leases and the poll after them", func() bool { return len(r.f.seen("acquire")) >= 5 })
	r.mu.Lock()
	m := r.opts.MinBackoff
	want := []time.Duration{m / 2, m, 2 * m, 4 * m}
	for i, w := range want {
		if len(r.sleeps) <= i || r.sleeps[i] != w {
			t.Fatalf("waits after each failed lease = %v, want %v: the backoff must grow, not reset", r.sleeps, want)
		}
	}
	r.mu.Unlock()
	if got := r.f.failReasons(); len(got) != 4 {
		t.Errorf("failed %d lease(s), want 4", len(got))
	}

	// A completed lease is followed by no wait at all.
	ok := newRig(t)
	ok.opts.Sleep = func(ctx context.Context, d time.Duration) error {
		if d != time.Second { // the heartbeat's own wait
			ok.mu.Lock()
			ok.sleeps = append(ok.sleeps, d)
			ok.mu.Unlock()
		}
		return sleep(ctx, 2*time.Millisecond)
	}
	ok.f.queue("acquire", jsonReply(200, ok.lease()))
	ok.run()
	until(t, "the poll after a completed lease", func() bool { return len(ok.f.seen("acquire")) >= 2 })
	ok.mu.Lock()
	defer ok.mu.Unlock()
	if len(ok.sleeps) != 0 {
		t.Errorf("the worker waited %v after a lease it completed", ok.sleeps)
	}
}

// TestWorker_ALeasedCommandLineOutsideTheServersShapeIsRefusedUnrun: the worker runs what the
// lease carries, so it holds the lease to the shape the server's leasable plans have. An
// input, an attachment, an overwrite switch, a second progress channel, a filter script, an
// end-of-options marker, an absolute path, a path that climbs: each is failed `refused_plan`
// and never reaches ffmpeg. What the server really emits is run.
func TestWorker_ALeasedCommandLineOutsideTheServersShapeIsRefusedUnrun(t *testing.T) {
	real := []string{"-map", "0", "-map", "-0:d?", "-c", "copy", "-c:v", "libx265", "-preset", "slow", "-crf", "22",
		"-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error:colorprim=bt709", "-vf", "yadif=0:-1:0,scale=-2:720:flags=lanczos",
		"-color_primaries", "bt709", "-metadata:s:v:0", "title=a/b ../c", "-f", "matroska"}
	if why := refusedPlan(&node.AcquireResponse{Body: real}); why != "" {
		t.Fatalf("the server's own command line was refused: %s", why)
	}
	for name, tc := range map[string]struct {
		pre, extra []string
		want       string
	}{
		"options before the input": {[]string{"-f", "lavfi"}, nil, "before the input"},
		"an input":                 {nil, []string{"-i", "x.mkv"}, "-i"},
		"an attachment":            {nil, []string{"-attach", "cover.jpg"}, "-attach"},
		"an attachment dump":       {nil, []string{"-dump_attachment:t", "out"}, "-dump_attachment"},
		"the overwrite switch":     {nil, []string{"-y"}, "-y"},
		"a progress channel":       {nil, []string{"-progress", "pipe:1"}, "-progress"},
		"a filter script":          {nil, []string{"-filter_script:v", "graph.txt"}, "-filter_script"},
		"a complex filter script":  {nil, []string{"-filter_complex_script", "graph.txt"}, "-filter_complex_script"},
		"an end of options":        {nil, []string{"--"}, "--"},
		"an absolute path":         {nil, []string{"-passlogfile", "/etc/cron.d/x"}, "absolute path"},
		"a climbing path":          {nil, []string{"-passlogfile", "../../etc/x"}, "climbs"},
		"a bare parent":            {nil, []string{".."}, "climbs"},
		"a path ending in parent":  {nil, []string{"a/.."}, "climbs"},
		"a path through a parent":  {nil, []string{"a/../b"}, "climbs"},
	} {
		l := &node.AcquireResponse{Pre: tc.pre, Body: append(append([]string(nil), real...), tc.extra...)}
		if why := refusedPlan(l); !strings.Contains(why, tc.want) {
			t.Errorf("%s: refusedPlan = %q, want a refusal naming %q", name, why, tc.want)
		}
	}
	if ReasonRefusedPlan != "refused_plan" {
		t.Errorf("ReasonRefusedPlan = %q", ReasonRefusedPlan)
	}
}

// TestWorker_StartSweepsOnlyItsOwnLeftOutputs: a killed worker leaves its output behind.
// The next start removes it - and only files of its own output naming: the directory may be
// shared, and nothing else in it is this worker's to remove.
func TestWorker_StartSweepsOnlyItsOwnLeftOutputs(t *testing.T) {
	r := newRig(t)
	if err := os.MkdirAll(filepath.Join(r.work, leaseID+".3.out"+".d"), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := []string{leaseID + ".1.out", strings.Repeat("a", 32) + ".12345.out"}
	notMine := []string{"notes.txt", leaseID + ".out", leaseID + ".1.out.bak", "x" + leaseID + ".1.out",
		strings.Repeat("A", 32) + ".1.out", leaseID[:31] + ".1.out", leaseID + ".one.out", leaseID + "..out", "movie.mkv"}
	for _, n := range append(append([]string(nil), mine...), notMine...) {
		if err := os.WriteFile(filepath.Join(r.work, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory under its naming is not an output either.
	dirName := strings.Repeat("b", 32) + ".1.out"
	if err := os.Mkdir(filepath.Join(r.work, dirName), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := r.run()
	until(t, "a poll", func() bool { return len(r.f.seen("acquire")) >= 1 })
	_ = stop()
	for _, n := range mine {
		if _, err := os.Stat(filepath.Join(r.work, n)); err == nil {
			t.Errorf("a left output survived the start: %s", n)
		}
	}
	for _, n := range append(notMine, dirName) {
		if _, err := os.Stat(filepath.Join(r.work, n)); err != nil {
			t.Errorf("the start removed something that is not this worker's output: %s", n)
		}
	}
	if got := outputName(leaseID, 7); got != leaseID+".7.out" || !outputNamed.MatchString(got) {
		t.Errorf("outputName = %q, which the sweep would not recognise", got)
	}
}

// TestWorker_ARedirectIsNeverFollowed: Go's client would carry the Authorization header to a
// redirect's target and send an upload's body again to wherever a 307 points. The worker
// follows none: a 3xx to an acquire is not a lease, and a 3xx to an upload is a refusal.
func TestWorker_ARedirectIsNeverFollowed(t *testing.T) {
	var elsewhere []call
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		elsewhere = append(elsewhere, call{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer target.Close()
	redirect := func(status int) reply { return reply{status: status, location: target.URL + "/elsewhere"} }

	r := newRig(t)
	r.f.queue("acquire", redirect(307), redirect(302), jsonReply(200, r.lease()))
	r.f.queue("output", redirect(307), redirect(308))
	r.run()
	until(t, "the lease to be failed", func() bool { return len(r.f.failReasons()) >= 1 })
	if got := r.f.failReasons(); got[0] != ReasonUploadRefused {
		t.Errorf("an upload answered with a redirect was failed %v, want upload_refused", got)
	}
	if n := len(r.f.seen("output")); n != 1 {
		t.Errorf("%d upload(s), want 1: a redirected upload is not sent again", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(elsewhere) != 0 {
		t.Errorf("the worker followed a redirect and sent %d request(s) elsewhere, the first %s %s with Authorization %q",
			len(elsewhere), elsewhere[0].method, elsewhere[0].path, elsewhere[0].auth)
	}
}

// TestWorker_AServerAddressCarryingMoreThanAnAddressIsRefused: userinfo, a query or a
// fragment in worker_server refuses to start, naming the key and echoing none of it.
func TestWorker_AServerAddressCarryingMoreThanAnAddressIsRefused(t *testing.T) {
	for _, server := range []string{
		"https://alice:hunter2@holdfast.example.net", "https://alice@holdfast.example.net",
		"https://holdfast.example.net/?token=hunter2", "https://holdfast.example.net/?",
		"https://holdfast.example.net/#hunter2", "http://alice:hunter2@127.0.0.1:8080",
	} {
		err := CheckServer(server)
		if err == nil || !strings.Contains(err.Error(), "worker_server") {
			t.Errorf("CheckServer(%q) = %v, want a refusal naming worker_server", server, err)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "alice") {
			t.Errorf("the refusal echoes the address: %v", err)
		}
	}
}

package engine

// The per-root filesystem watch (S0091). Two properties carry the hazard and both are
// graded here before anything else: a file a download client is still writing is never
// offered for processing, and a root that cannot be watched says so out loud and is served
// by the interval scan rather than silently going unwatched.
//
// The event source is stood in for and the clock is stood in for; the WATCH is not, and
// neither is ProcessFile. A test that drove a real kernel queue would be a test that could
// not make one overflow, could not make a platform lack a backend, and could not decide
// anything in under a minute of wall clock.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// ---- harness -----------------------------------------------------------------

// stubBackend is the event source under test control. It is the ONLY double these cases
// use for the watch's own machinery: everything from the event onwards is the real thing.
type stubBackend struct {
	events chan watchEvent
	errs   chan error

	mu     sync.Mutex
	added  []string
	failOn map[string]error
	closed bool
}

func newStubBackend() *stubBackend {
	return &stubBackend{
		events: make(chan watchEvent, 64),
		errs:   make(chan error, 8),
		failOn: map[string]error{},
	}
}

func (s *stubBackend) Add(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, bad := s.failOn[dir]; bad {
		return err
	}
	s.added = append(s.added, dir)
	return nil
}

func (s *stubBackend) Events() <-chan watchEvent { return s.events }
func (s *stubBackend) Errors() <-chan error      { return s.errs }

func (s *stubBackend) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stubBackend) watching() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.added...)
}

func (s *stubBackend) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// fakeClock is the settle period's clock. The delay under test is 60 seconds by default
// and a case that waited for one would be a case nobody runs.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// watchedRoots builds the resolved roots a configuration with the watch turned on
// produces, which is what an Engine reads at construction. A Config assembled in Go
// carries no Roots, and RootProfiles then derives one per library root with the watch OFF
// - which is exactly right for every other case in this package and useless here.
//
// only names the roots that opted in; naming none turns every root on.
func watchedRoots(cfg *config.Config, settle int, only ...string) {
	on := func(p string) bool {
		if len(only) == 0 {
			return true
		}
		for _, o := range only {
			if filepath.Clean(o) == filepath.Clean(p) {
				return true
			}
		}
		return false
	}
	roots := make([]config.Root, 0, len(cfg.LibraryRoots))
	for _, r := range cfg.LibraryRoots {
		root := config.Root{
			Path:    r,
			Clean:   filepath.Clean(r),
			Profile: cfg.TopLevelProfile(),
			Filters: cfg.TopLevelFilters(),
		}
		if on(r) {
			root.Watch = config.Watch{Enabled: true, SettleSec: settle}
		}
		roots = append(roots, root)
	}
	cfg.Roots = roots
}

// waitFor polls until cond holds, failing the case when it does not. The deadline is a
// wall clock on a case that would otherwise hang, never a gate: nothing here passes
// because it expired.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(watchDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen inside %s", what, watchDeadline)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// watchDeadline bounds every poll in this file.
const watchDeadline = 2 * time.Minute

// offeredPaths drains what the watch has offered for processing, without running the pool
// that would consume it. It is how "offered" is asserted separately from "processed".
func offeredPaths(w *Watches) []string {
	var out []string
	for {
		select {
		case p := <-w.ch:
			out = append(out, p)
		default:
			return out
		}
	}
}

// grow appends n bytes to path, which is what a download client writing a large remux
// looks like to a watch: an event, then a file that is bigger every time it is looked at.
func grow(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(bytes.Repeat([]byte("x"), n)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---- AC-5: a file still being written is never offered ------------------------

// TestWatch_DoesNotEnqueueAFileStillGrowing grades AC-5, and it is the case the settle
// delay exists for: a download client writing a 40 GB remux produces an event long before
// the file is complete, and a probe taken then reads a duration and a packet count that
// are not the file's. Every gate downstream is weighed against those numbers, and the
// source is deleted on their say-so.
//
// The clock is driven rather than waited on, and the settle decision is taken
// synchronously, so the case decides on the RULE rather than on a race: an event, a size
// that moves, a size that holds, and exactly one offer at the end of it.
func TestWatch_DoesNotEnqueueAFileStillGrowing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	arriving := filepath.Join(root, "arriving.mkv")
	if err := os.WriteFile(arriving, bytes.Repeat([]byte("a"), 1000), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const settle = 60
	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
		func(c *config.Config) { watchedRoots(c, settle) })
	eng.SetCoverage([]string{root}, nil)

	clk := newFakeClock()
	w := eng.NewWatches()
	w.now = clk.now
	w.mechanism = "inotify"
	stub := newStubBackend()
	w.newBackend = func() (watchBackend, error) { return stub, nil }
	w.start(context.Background())

	// The event. Nothing is offered on an event alone: an event says a file was touched,
	// never that it is finished.
	w.observe(watchEvent{Path: arriving})
	w.settleOnce()
	if got := offeredPaths(w); len(got) != 0 {
		t.Fatalf("offered %v on the event itself, before the file had held still for a moment", got)
	}

	// The file grows, and MORE than a settle period passes while it does. The period is
	// about the size holding still, not about how long ago the event was: a watch that
	// counted from the event would hand the encoder a half-written file.
	grow(t, arriving, 1000)
	clk.advance((settle + 1) * time.Second)
	w.settleOnce()
	if got := offeredPaths(w); len(got) != 0 {
		t.Fatalf("offered %v while the file was still growing (%ds after the event, size moved in between)",
			got, settle+1)
	}

	// The size holds still, but not yet for long enough.
	clk.advance((settle - 1) * time.Second)
	w.settleOnce()
	if got := offeredPaths(w); len(got) != 0 {
		t.Fatalf("offered %v after %ds of a %ds settle period", got, settle-1, settle)
	}

	// And now it has held still for the whole period: offered, once.
	clk.advance(2 * time.Second)
	w.settleOnce()
	got := offeredPaths(w)
	if len(got) != 1 || got[0] != arriving {
		t.Fatalf("offered %v after the settle period, want exactly [%s]", got, arriving)
	}

	// Once. A settled file that is offered on every pass afterwards is a file the pipeline
	// meets again on every tick for as long as it sits in the library.
	clk.advance((settle + 1) * time.Second)
	w.settleOnce()
	if got := offeredPaths(w); len(got) != 0 {
		t.Fatalf("offered %v a second time for one arrival", got)
	}
}

// ---- AC-3: a root that cannot be watched says so, once, at startup ------------

// TestWatch_FallsBackLoudlyWhenTheRootCannotBeWatched grades AC-3. Each way a watch can
// fail to exist is driven for real: a platform this build carries no backend for, a
// backend the platform refused to hand over, and storage the FILESYSTEM-1 check could not
// positively identify as local. Each must record which root, which dependency failed, what
// was tried and that the interval scan covers it - and none may leave behind a record
// naming an event mechanism for that root, because none of them got one.
func TestWatch_FallsBackLoudlyWhenTheRootCannotBeWatched(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, tc := range []struct {
		name string
		// arrange breaks exactly one of the three dependencies.
		arrange func(t *testing.T, eng *Engine, w *Watches)
		// dependency is what the record must name as the thing that failed.
		dependency string
	}{
		{
			name: "this build carries no backend for the platform",
			arrange: func(t *testing.T, _ *Engine, w *Watches) {
				w.mechanism = ""
				var asked atomic.Bool
				w.newBackend = func() (watchBackend, error) {
					asked.Store(true)
					return newStubBackend(), nil
				}
				// A watcher this build could name nothing about is one it must not run
				// under: with no mechanism there is nothing to report, so nothing is
				// asked for either.
				t.Cleanup(func() {
					if asked.Load() {
						t.Error("a watcher was obtained on a platform this build carries no mechanism name for, " +
							"so a watch would have run under a mechanism no record could name")
					}
				})
			},
			dependency: "github.com/fsnotify/fsnotify",
		},
		{
			name: "the platform refused a watcher",
			arrange: func(_ *testing.T, _ *Engine, w *Watches) {
				w.newBackend = func() (watchBackend, error) {
					return nil, errors.New("simulated: too many open files")
				}
			},
			dependency: "github.com/fsnotify/fsnotify",
		},
		{
			name: "the root's storage is not positively local",
			arrange: func(_ *testing.T, eng *Engine, w *Watches) {
				eng.fsLookup = func(string) (string, error) { return "nfs", nil }
				w.newBackend = func() (watchBackend, error) { return newStubBackend(), nil }
			},
			dependency: "FILESYSTEM-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkH264(t, ffmpeg, filepath.Join(root, "film.mkv"), "8M")

			var encodes atomic.Int32
			eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
				func(c *config.Config) { watchedRoots(c, 1) })
			eng.SetCoverage([]string{root}, nil)
			var buf bytes.Buffer
			eng.Log = jsonLogger(&buf)

			w := eng.NewWatches()
			w.mechanism = "inotify"
			tc.arrange(t, eng, w)
			w.start(context.Background())

			if w.Watching(root) {
				t.Errorf("the root is reported as watched, and the watch could not be obtained")
			}
			if n := w.Descriptors(); n != 0 {
				t.Errorf("the watch holds %d descriptors over a root it could not watch", n)
			}

			// The record: at warn, naming the root, the dependency, what was tried, and
			// the interval scan as what covers the root now.
			var fallbacks []map[string]any
			for _, rec := range logRecords(t, &buf) {
				if rec["library_root"] != root {
					continue
				}
				if _, names := rec["mechanism"]; names {
					t.Errorf("a record names an event mechanism for a root that obtained none: %v", rec)
				}
				if rec["dependency"] != nil {
					fallbacks = append(fallbacks, rec)
				}
			}
			if len(fallbacks) != 1 {
				t.Fatalf("want exactly one fallback record for %s, got %d: %s", root, len(fallbacks), buf.String())
			}
			rec := fallbacks[0]
			if rec["level"] != "WARN" {
				t.Errorf("the fallback was recorded at %v, want WARN: the process continued in a degraded "+
					"state, which is what warn means here", rec["level"])
			}
			if dep, _ := rec["dependency"].(string); !strings.Contains(dep, tc.dependency) {
				t.Errorf("the record names dependency %q, want it to name %q", dep, tc.dependency)
			}
			if s, _ := rec["attempted"].(string); strings.TrimSpace(s) == "" {
				t.Errorf("the record does not say what was tried: %v", rec)
			}
			if s, _ := rec["next"].(string); !strings.Contains(s, "interval scan") {
				t.Errorf("the record's next action is %q, which does not name the interval scan", s)
			}

			// And the run CONTINUES, with that root served by the scan alone.
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot after the fallback: %v", err)
			}
			if got := encodes.Load(); got != 1 {
				t.Errorf("the interval scan reached the encoder %d times, want 1: a root that fell back is "+
					"served by the scan, not abandoned", got)
			}
		})
	}
}

// ---- AC-1: the opt-in is per root, and off is what silence means --------------

// TestWatch_OptInIsPerRootAndDefaultsOff grades AC-1 at the engine: a watch starts for the
// root that asked for one and for no other, so every configuration written before this
// feature - none of which carries the key anywhere - behaves exactly as it did.
func TestWatch_OptInIsPerRootAndDefaultsOff(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	watched, unwatched := t.TempDir(), t.TempDir()

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, watched, countingEncoder(&encodes), func(c *config.Config) {
		c.LibraryRoots = []string{watched, unwatched}
		watchedRoots(c, 60, watched)
	})
	eng.SetCoverage([]string{watched, unwatched}, nil)
	var buf bytes.Buffer
	eng.Log = jsonLogger(&buf)

	stub := newStubBackend()
	w := eng.NewWatches()
	w.mechanism = "inotify"
	w.newBackend = func() (watchBackend, error) { return stub, nil }
	w.start(context.Background())

	if !w.Watching(watched) {
		t.Errorf("%s carried the opt-in and is not watched", watched)
	}
	if w.Watching(unwatched) {
		t.Errorf("%s omitted the opt-in and is watched anyway", unwatched)
	}
	for _, dir := range stub.watching() {
		if !strings.HasPrefix(dir, watched) {
			t.Errorf("a descriptor was registered over %s, which no entry asked for", dir)
		}
	}
	// Silence about a root is not a report about it either: an entry that said nothing
	// about the watch produces no record, no fallback and no descriptor.
	for _, rec := range logRecords(t, &buf) {
		if rec["library_root"] == unwatched {
			t.Errorf("a root that asked for no watch was reported on: %v", rec)
		}
	}

	// And an engine whose configuration asks for no watch at all starts nothing, which is
	// every configuration written before this feature existed.
	silent := buildEngine(t, ffmpeg, ffprobe, unwatched, countingEncoder(&encodes), nil)
	silent.SetCoverage([]string{unwatched}, nil)
	quiet := silent.NewWatches()
	quiet.newBackend = func() (watchBackend, error) {
		t.Error("a watcher was obtained for a configuration that asked for no watch")
		return newStubBackend(), nil
	}
	quiet.start(context.Background())
	if n := quiet.Descriptors(); n != 0 {
		t.Errorf("a configuration with no watch holds %d descriptors", n)
	}
}

// ---- AC-2: the mechanism it actually obtained, named once per root ------------

// TestWatch_ReportsTheMechanismItGot grades AC-2. A watch that silently degraded to
// polling would be the same class of false report the rest of this codebase refuses, and
// there is no polling backend to degrade INTO - so what has to be true is narrower and
// checkable: the mechanism named is the one this build carries for this platform, it is
// named once per root, and a root that obtained none has no record naming one.
func TestWatch_ReportsTheMechanismItGot(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
		func(c *config.Config) { watchedRoots(c, 60) })
	eng.SetCoverage([]string{root}, nil)
	var buf bytes.Buffer
	eng.Log = jsonLogger(&buf)

	w := eng.NewWatches()
	stub := newStubBackend()
	w.newBackend = func() (watchBackend, error) { return stub, nil }
	w.start(context.Background())

	named := 0
	for _, rec := range logRecords(t, &buf) {
		mech, ok := rec["mechanism"]
		if !ok {
			continue
		}
		named++
		if rec["library_root"] != root {
			t.Errorf("a mechanism was named for %v, not for the watched root", rec["library_root"])
		}
		if mech != thisMechanism() {
			t.Errorf("the record names mechanism %v, and this build's backend for this platform is %q",
				mech, thisMechanism())
		}
	}
	if named != 1 {
		t.Errorf("the mechanism was named %d times for one root, want once: %s", named, buf.String())
	}

	// The name comes from fsnotify's own supported-platform table, which is what makes it
	// a name rather than a guess. A platform this build carries no backend for has no
	// name, which is what the fallback decision keys off.
	for _, tc := range []struct{ goos, want string }{
		{"linux", "inotify"},
		{"darwin", "kqueue"},
		{"freebsd", "kqueue"},
		{"windows", "ReadDirectoryChangesW"},
		{"illumos", "FEN"},
		{"plan9", ""},
	} {
		if got := watchMechanism(tc.goos); got != tc.want {
			t.Errorf("watchMechanism(%q) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

// ---- AC-4: through the same door, and exactly one claim ----------------------

// TestWatch_EnqueuesThroughTheSameClaimedPath grades AC-4. The watch is a queue and a pool
// and nothing else: what it offers goes to Engine.ProcessFile, so every skip guard, every
// gate and the Store.Claim mutual-exclusion guard reach a watch-found file by construction.
// The second half is the race that matters - a watch offer and a scan over one path must
// produce exactly one claim and exactly one processing of it.
func TestWatch_EnqueuesThroughTheSameClaimedPath(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a settled path reaches ProcessFile", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")

		var encodes atomic.Int32
		eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
			func(c *config.Config) { watchedRoots(c, 0) })
		eng.SetCoverage([]string{root}, nil)
		var claimed sync.Map
		eng.onClaim = func(worker, path string) { claimed.Store(path, worker) }

		stub := newStubBackend()
		w := eng.NewWatches()
		w.mechanism = "inotify"
		w.newBackend = func() (watchBackend, error) { return stub, nil }

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); w.Run(ctx) }()
		waitFor(t, "the watch to register its descriptors", func() bool { return w.Watching(root) })
		stub.events <- watchEvent{Path: film}

		waitFor(t, "the watched file to reach the encoder", func() bool { return encodes.Load() == 1 })
		cancel()
		<-done

		worker, got := claimed.Load(film)
		if !got {
			t.Fatalf("the watch offered %s and nothing claimed it", film)
		}
		if s, _ := worker.(string); !strings.HasPrefix(s, "watch-") {
			t.Errorf("the claim was taken by %q, want a watch worker", s)
		}
	})

	t.Run("a watch offer racing a scan claims once", func(t *testing.T) {
		root := t.TempDir()
		film := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, film, "8M")

		gate := &gatedEncoder{
			inner:   EncoderFunc(func(context.Context, string, string, *probe.VideoProps) error { return errFake }),
			started: make(chan string, 1),
			release: make(chan struct{}),
		}
		eng := buildEngine(t, ffmpeg, ffprobe, root, gate, func(c *config.Config) { watchedRoots(c, 0) })
		eng.SetCoverage([]string{root}, nil)
		var claims atomic.Int32
		eng.onClaim = func(string, string) { claims.Add(1) }

		w := eng.NewWatches()
		w.mechanism = "inotify"
		w.newBackend = func() (watchBackend, error) { return newStubBackend(), nil }
		w.start(context.Background())

		// The scan claims the file and is held inside the encode.
		scan := make(chan error, 1)
		go func() { scan <- eng.RunOneshot(context.Background()) }()
		select {
		case <-gate.started:
		case <-time.After(watchDeadline):
			t.Fatal("the scan never reached the encoder")
		}

		// The watch offers the same path, through its own worker, while the scan holds it.
		w.process(context.Background(), "watch-w0", film)

		close(gate.release)
		if err := <-scan; err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := claims.Load(); got != 1 {
			t.Errorf("%d claims were taken on one path, want exactly 1", got)
		}
		if got := gate.n.Load(); got != 1 {
			t.Errorf("the file was encoded %d times, want exactly 1", got)
		}
	})
}

// ---- AC-6: the periodic scan is still the source of truth --------------------

// TestWatch_PeriodicScanStillRuns grades AC-6. Events are lossy - dropped on queue
// overflow, missed across a restart, absent for a file moved in by a rename the watch did
// not see - so the scan remains the mechanism and the watch is an accelerator. The case
// drives exactly that: a watch is running over the root, a file arrives with NO event at
// all, and the scan finds it.
func TestWatch_PeriodicScanStillRuns(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(root, "present.mkv"), "8M")

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
		func(c *config.Config) { watchedRoots(c, 60) })
	eng.SetCoverage([]string{root}, nil)

	stub := newStubBackend()
	w := eng.NewWatches()
	w.mechanism = "inotify"
	w.newBackend = func() (watchBackend, error) { return stub, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	waitFor(t, "the watch to register its descriptors", func() bool { return w.Watching(root) })

	// The startup scan still runs with the watch enabled on every root.
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if got := encodes.Load(); got != 1 {
		t.Fatalf("the startup scan reached the encoder %d times with the watch running, want 1", got)
	}

	// A file arrives and the watch never hears about it - the rename nobody saw, the
	// overflowed queue, the restart. The reconciliation scan is what finds it.
	mkH264(t, ffmpeg, filepath.Join(root, "arrived-unseen.mkv"), "8M")
	if n := len(stub.events); n != 0 {
		t.Fatalf("the stub event source carries %d events; this case is about a file no event named", n)
	}
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot (the periodic pass): %v", err)
	}
	if got := encodes.Load(); got != 2 {
		t.Errorf("the periodic scan reached the encoder %d times in total, want 2: the file no event named "+
			"is exactly what the scan is still the source of truth for", got)
	}
	if !w.Watching(root) {
		t.Error("the watch stopped watching the root over the course of two scans")
	}
	cancel()
	<-done
	if !stub.isClosed() {
		t.Error("the shutdown left the watch descriptors open")
	}
}

// ---- AC-7: the bound is reported, and exhaustion degrades loudly -------------

// TestWatch_BoundIsReportedAndExhaustionDegradesLoudly grades AC-7. fsnotify is not
// recursive, so a descriptor is one per directory and the count grows with the tree, while
// the host's own limit differs per distro and per available memory. Reaching either must be
// an ANNOUNCED degradation naming the root, never a watch that quietly sees half a tree
// while reporting one over the whole of it.
func TestWatch_BoundIsReportedAndExhaustionDegradesLoudly(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	// covered builds a root with three covered directories, which is what the bound and
	// the host limit are then driven against.
	covered := func(t *testing.T) (root string, dirs []string) {
		t.Helper()
		root = t.TempDir()
		dirs = []string{root}
		for _, name := range []string{"a", "b"} {
			dir := filepath.Join(root, name)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatalf("Mkdir: %v", err)
			}
			dirs = append(dirs, dir)
		}
		return root, dirs
	}

	t.Run("the count is recorded when the whole tree is watched", func(t *testing.T) {
		root, dirs := covered(t)
		var encodes atomic.Int32
		eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
			func(c *config.Config) { watchedRoots(c, 60) })
		eng.SetCoverage(dirs, nil)
		var buf bytes.Buffer
		eng.Log = jsonLogger(&buf)

		w := eng.NewWatches()
		w.mechanism = "inotify"
		w.newBackend = func() (watchBackend, error) { return newStubBackend(), nil }
		w.start(context.Background())

		if got := w.Descriptors(); got != len(dirs) {
			t.Errorf("the watch holds %d descriptors over %d covered directories", got, len(dirs))
		}
		var reported bool
		for _, rec := range logRecords(t, &buf) {
			if rec["mechanism"] == nil {
				continue
			}
			reported = true
			if rec["watch_descriptors"] != float64(len(dirs)) {
				t.Errorf("the record says %v descriptors, and %d are held", rec["watch_descriptors"], len(dirs))
			}
			if rec["directories_covered"] != float64(len(dirs)) {
				t.Errorf("the record says %v covered directories, and there are %d",
					rec["directories_covered"], len(dirs))
			}
		}
		if !reported {
			t.Errorf("the descriptor count was never recorded: %s", buf.String())
		}
	})

	for _, tc := range []struct {
		name       string
		arrange    func(w *Watches, stub *stubBackend, dirs []string)
		dependency string
	}{
		{
			name:       "this build's own bound",
			arrange:    func(w *Watches, _ *stubBackend, _ []string) { w.maxDescriptors = 1 },
			dependency: "the watch-descriptor bound",
		},
		{
			name: "the host's own watch limit",
			arrange: func(_ *Watches, stub *stubBackend, dirs []string) {
				stub.failOn[dirs[1]] = errors.New("no space left on device")
			},
			dependency: "the host's own watch limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, dirs := covered(t)
			var encodes atomic.Int32
			eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
				func(c *config.Config) { watchedRoots(c, 60) })
			eng.SetCoverage(dirs, nil)
			var buf bytes.Buffer
			eng.Log = jsonLogger(&buf)

			stub := newStubBackend()
			w := eng.NewWatches()
			w.mechanism = "inotify"
			w.newBackend = func() (watchBackend, error) { return stub, nil }
			tc.arrange(w, stub, dirs)
			w.start(context.Background())

			held := w.Descriptors()
			if held == 0 || held >= len(dirs) {
				t.Fatalf("the watch holds %d of %d directories; this case is about holding SOME of them",
					held, len(dirs))
			}

			var degraded []map[string]any
			for _, rec := range logRecords(t, &buf) {
				if rec["dependency"] == nil || rec["library_root"] != root {
					continue
				}
				degraded = append(degraded, rec)
			}
			if len(degraded) != 1 {
				t.Fatalf("want one degradation record for %s, got %d: %s", root, len(degraded), buf.String())
			}
			rec := degraded[0]
			if rec["level"] != "WARN" {
				t.Errorf("the degradation was recorded at %v, want WARN", rec["level"])
			}
			if dep, _ := rec["dependency"].(string); !strings.Contains(dep, tc.dependency) {
				t.Errorf("the record names dependency %q, want it to name %q", dep, tc.dependency)
			}
			if msg, _ := rec["msg"].(string); !strings.Contains(msg, "NO LONGER FULLY WATCHED") {
				t.Errorf("the record does not say the root is no longer fully watched: %q", msg)
			}
			if next, _ := rec["next"].(string); !strings.Contains(next, "interval scan") {
				t.Errorf("the record's next action is %q, which does not name the interval scan", next)
			}
			if rec["watch_descriptors"] != float64(held) {
				t.Errorf("the record says %v descriptors and %d are held", rec["watch_descriptors"], held)
			}
			if rec["directories_covered"] != float64(len(dirs)) {
				t.Errorf("the record says %v covered directories and there are %d",
					rec["directories_covered"], len(dirs))
			}

			// And nothing anywhere reports a watch over the whole of a tree it sees part
			// of: the record naming the mechanism carries both counts, and they differ.
			for _, r := range logRecords(t, &buf) {
				if r["mechanism"] == nil {
					continue
				}
				if r["watch_descriptors"] == r["directories_covered"] {
					t.Errorf("the started record claims a descriptor for every covered directory while "+
						"only %d of %d are held: %v", held, len(dirs), r)
				}
			}
		})
	}
}

// ---- AC-8: a path that goes away before it settles ---------------------------

// TestWatch_VanishedPathIsDroppedNotProcessed grades AC-8. A create-then-delete is
// routine - a download client writing to a temporary name, an *arr moving an import
// through - and it must cost a dropped entry and nothing else. A vanished path that
// reached a probe would be a probe of a file that is not there, and a vanished path
// recorded at error would train an operator to ignore the log they need for the swap.
func TestWatch_VanishedPathIsDroppedNotProcessed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, tc := range []struct {
		name string
		// gone removes the file the way this case is about.
		gone func(t *testing.T, w *Watches, path string)
	}{
		{
			name: "deleted before it settled",
			gone: func(t *testing.T, _ *Watches, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove: %v", err)
				}
			},
		},
		{
			name: "the event source said it went away",
			gone: func(_ *testing.T, w *Watches, path string) {
				w.observe(watchEvent{Path: path, Gone: true})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			arriving := filepath.Join(root, "arriving.mkv")
			if err := os.WriteFile(arriving, bytes.Repeat([]byte("a"), 1000), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			var encodes atomic.Int32
			eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes),
				func(c *config.Config) { watchedRoots(c, 1) })
			eng.SetCoverage([]string{root}, nil)
			var buf bytes.Buffer
			eng.Log = jsonLogger(&buf)

			clk := newFakeClock()
			w := eng.NewWatches()
			w.now = clk.now
			w.mechanism = "inotify"
			w.newBackend = func() (watchBackend, error) { return newStubBackend(), nil }
			w.start(context.Background())

			w.observe(watchEvent{Path: arriving})
			w.settleOnce()
			tc.gone(t, w, arriving)
			clk.advance(time.Hour)
			w.settleOnce()

			if got := offeredPaths(w); len(got) != 0 {
				t.Errorf("offered %v for a path that went away before it settled", got)
			}
			if got := encodes.Load(); got != 0 {
				t.Errorf("the encoder was reached %d times for a path that is not there", got)
			}
			for _, rec := range logRecords(t, &buf) {
				if rec["level"] == "ERROR" {
					t.Errorf("a path that went away before it settled was recorded at error, which is the "+
						"level that means a human must act: %v", rec)
				}
			}
			// The pending set does not keep it either: an entry per vanished path would be
			// a leak on exactly the workload this feature exists for.
			w.mu.Lock()
			n := len(w.pending)
			w.mu.Unlock()
			if n != 0 {
				t.Errorf("%d paths are still pending after they went away", n)
			}
		})
	}
}

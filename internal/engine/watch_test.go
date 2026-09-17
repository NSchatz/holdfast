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
func watchedRoots(cfg *config.Config, settle int) {
	roots := make([]config.Root, 0, len(cfg.LibraryRoots))
	for _, r := range cfg.LibraryRoots {
		roots = append(roots, config.Root{
			Path:    r,
			Clean:   filepath.Clean(r),
			Profile: cfg.TopLevelProfile(),
			Filters: cfg.TopLevelFilters(),
			Watch:   config.Watch{Enabled: true, SettleSec: settle},
		})
	}
	cfg.Roots = roots
}

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

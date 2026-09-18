package engine

// The STREAMED enumeration (S0099): the scan hands files to workers WHILE it is still
// listing the library, rather than listing the whole tree and then feeding from a slice.
//
// Every case here names the acceptance criterion it grades. The two that had to red first
// are [AC-1] and [AC-4]: they are the pair the item filed as the proof that the enumeration
// really is incremental, and that the evidence the retention pass reads (`observed`) is
// still exactly what this run listed once it is.
//
// The seams these cases substitute are the FILESYSTEM (readDirFn, the coverage-bounded
// pass's only route to a listing) and the per-file attribute read (statFn, the first thing
// ProcessFile does with a path a worker was handed). Neither stands in for the subject: the
// enumeration, the feed and the workers are the real ones.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// streamWait bounds every case here that waits for one goroutine to reach a point another
// one is holding open. It is a DEADLOCK bound and not a performance assertion: the work it
// waits on is a directory listing of a handful of entries and a stat, so a build that
// streams reaches it in milliseconds and a build that does not never reaches it at all.
const streamWait = 20 * time.Second

// TestScan_StartsWorkBeforeEnumerationCompletes is [AC-1].
//
// The last covered directory's listing does not return until the case lets it. If the
// enumeration only feeds workers once the whole tree is listed, no worker can have touched
// anything by then, and this case reds by reaching its bound with nothing having begun.
func TestScan_StartsWorkBeforeEnumerationCompletes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	early := filepath.Join(root, "aaa-early")
	slow := filepath.Join(root, "zzz-slow")
	earlyFile := filepath.Join(early, "Early.mkv")
	mustWrite(t, earlyFile)
	mustWrite(t, filepath.Join(slow, "Late.mkv"))

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	eng.Coverage = []string{early, slow}

	release := make(chan struct{})
	var slowListingReturned atomic.Bool
	eng.readDirFn = func(dir string) ([]os.DirEntry, error) {
		if dir == slow {
			<-release
			slowListingReturned.Store(true)
		}
		return os.ReadDir(dir)
	}

	// A worker has BEGUN a file the instant ProcessFile reads that file's attributes: it is
	// the first thing it does with a path, on the worker's own goroutine.
	begun := make(chan string, 4)
	eng.statFn = func(path string) (os.FileInfo, error) {
		select {
		case begun <- path:
		default:
		}
		return os.Stat(path)
	}

	ctx := context.Background()
	eng.EnsureHoldBacks(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := eng.scanOnce(ctx, eng.passListings())
		done <- err
	}()

	select {
	case got := <-begun:
		if slowListingReturned.Load() {
			t.Fatalf("a worker began %s only after the last directory's listing had returned", got)
		}
		if got != earlyFile {
			t.Errorf("a worker began %s first, want %s: the first directory listed is the first fed", got, earlyFile)
		}
	case err := <-done:
		close(release)
		t.Fatalf("the scan returned (%v) without any worker having begun a file, while the last covered "+
			"directory's listing was still outstanding", err)
	case <-time.After(streamWait):
		close(release)
		<-done
		t.Fatalf("no worker had begun any file %s after the scan started, with the last covered directory's "+
			"listing still blocked: the enumeration is not feeding workers until it has listed the whole "+
			"library, so the first encode waits for the last readdir", streamWait)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
}

// TestScan_ObservedIsCompleteAfterAStreamedPass is [AC-4].
//
// `observed` is the retention pass's evidence: a directory in it is one this run LISTED, and
// the retention pass is entitled to read a file missing from such a directory as a file that
// is gone. So this asserts the exact SET and not its size, and it asserts it of a pass in
// which listing and working really did overlap - the last directory's listing is held until
// a worker has begun a file, so a build that lists everything before it feeds anything reds
// here rather than passing on a property it never exercised.
func TestScan_ObservedIsCompleteAfterAStreamedPass(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	first := filepath.Join(root, "a-first")
	empty := filepath.Join(root, "b-empty")
	gone := filepath.Join(root, "c-gone") // covered, and not on disk: listing it FAILS
	last := filepath.Join(root, "d-last")
	mustWrite(t, filepath.Join(first, "First.mkv"))
	mustWrite(t, filepath.Join(last, "Last.mkv"))
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	eng.Coverage = []string{first, empty, gone, last}

	begun := make(chan struct{})
	var once sync.Once
	eng.statFn = func(path string) (os.FileInfo, error) {
		once.Do(func() { close(begun) })
		return os.Stat(path)
	}

	streamed := make(chan bool, 1)
	eng.readDirFn = func(dir string) ([]os.DirEntry, error) {
		if dir == last {
			select {
			case <-begun:
				streamed <- true
			case <-time.After(streamWait):
				streamed <- false
			}
		}
		return os.ReadDir(dir)
	}

	ctx := context.Background()
	eng.EnsureHoldBacks(ctx)
	observed, err := eng.scanOnce(ctx, eng.passListings())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if !<-streamed {
		t.Fatalf("no worker had begun any file %s into the last covered directory's listing: this pass did "+
			"not stream, so what it recorded as observed says nothing about a pass that does", streamWait)
	}

	want := map[string]bool{first: true, empty: true, last: true}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("observed = %v, want exactly %v: a covered directory that listed EMPTY is evidence "+
			"(holdfast looked and found nothing) and one whose listing FAILED is not (%s)",
			sortedKeys(observed), sortedKeys(want), gone)
	}
}

// --- shared apparatus ---------------------------------------------------------------

// arrivals records every path a WORKER began, in the order the workers reached it. A worker
// begins a file at ProcessFile's attribute read, which is the first thing it does with a
// path it was handed, on the worker's own goroutine.
type arrivals struct {
	mu   sync.Mutex
	seen []string
	// then, when non-nil, runs on the worker's goroutine once the arrival is recorded. It
	// is how a case holds a worker inside one file while it drives the enumeration past it.
	then func(path string)
}

func (a *arrivals) statFn(path string) (os.FileInfo, error) {
	a.mu.Lock()
	a.seen = append(a.seen, path)
	then := a.then
	a.mu.Unlock()
	if then != nil {
		then(path)
	}
	return os.Stat(path)
}

func (a *arrivals) paths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func (a *arrivals) after(then func(path string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.then = then
}

// safeLog is a logger whose buffer may be read while workers are still running. The suite's
// captureLogger writes into a bare bytes.Buffer, which is right for a case that reads it
// after the run and wrong for one whose subject is a record made DURING it.
type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *safeLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// streamEngine builds an engine over root, bounded by coverage, with a recorder on the
// per-file attribute read. It publishes this run's hold-backs exactly as RunOneshot does.
func streamEngine(t *testing.T, root string, coverage []string, workers int) (*Engine, *arrivals) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	e := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = workers })
	e.Coverage = coverage
	a := &arrivals{}
	e.statFn = a.statFn
	e.EnsureHoldBacks(context.Background())
	return e, a
}

// listedDirs records which directories a run actually asked the filesystem for, which is
// how a case asserts that a stopped scan stopped LISTING and not merely feeding.
type listedDirs struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newListedDirs() *listedDirs { return &listedDirs{seen: map[string]bool{}} }

func (l *listedDirs) readDir(dir string) ([]os.DirEntry, error) {
	l.mu.Lock()
	l.seen[dir] = true
	l.mu.Unlock()
	return os.ReadDir(dir)
}

func (l *listedDirs) was(dir string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[dir]
}

// sameSet reports whether two path slices hold the same paths, whatever the order.
func sameSet(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

// TestScan_HandOutOrderIsTotalAndDeterministic is [AC-2].
//
// The rule the hand-out order follows is stated in docs/enumeration-order.md and it is
// PINNED here, spelled out file by file rather than re-derived: a later spec declaring a
// queue order builds on this sequence, so a change to it has to be a change somebody made on
// purpose. The fixture's names are chosen so that this order and a global sort of the full
// paths are DIFFERENT answers - `zz.mkv` in the root comes before everything under `sub/`,
// where a global sort of the paths would put it last.
func TestScan_HandOutOrderIsTotalAndDeterministic(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		filepath.Join(root, "b.mkv"),
		filepath.Join(root, "a.mkv"),
		filepath.Join(root, "zz.mkv"),
		filepath.Join(root, "sub", "c.mkv"),
		filepath.Join(root, "sub", "a.mkv"),
		filepath.Join(root, "sub", "deeper", "b.mkv"),
		filepath.Join(root, "sub", "deeper", "a.mkv"),
	} {
		mustWrite(t, p)
	}
	res := walkOver(t, root, []string{"mkv"}, newListingCounter())
	wantCoverage := []string{root, filepath.Join(root, "sub"), filepath.Join(root, "sub", "deeper")}
	if !reflect.DeepEqual(res.Coverage, wantCoverage) {
		t.Fatalf("the startup walk covered %v, want %v: the hand-out order below IS that sequence, so a "+
			"change to it is a change to the documented rule", res.Coverage, wantCoverage)
	}
	want := []string{
		filepath.Join(root, "a.mkv"),
		filepath.Join(root, "b.mkv"),
		filepath.Join(root, "zz.mkv"),
		filepath.Join(root, "sub", "a.mkv"),
		filepath.Join(root, "sub", "c.mkv"),
		filepath.Join(root, "sub", "deeper", "a.mkv"),
		filepath.Join(root, "sub", "deeper", "b.mkv"),
	}

	// The order as the enumeration produces it, at a given worker count and over a given
	// reading of the filesystem.
	enumerated := func(workers int, readDir func(string) ([]os.DirEntry, error)) []string {
		e, _ := streamEngine(t, root, res.Coverage, workers)
		e.readDirFn = readDir
		var got []string
		e.enumerateStream(e.passListings(), sink{offer: func(p string) bool {
			got = append(got, p)
			return true
		}})
		return got
	}

	t.Run("two scans over an unchanged library agree, and hand out every source once", func(t *testing.T) {
		for _, pass := range []string{"first", "second"} {
			e, a := streamEngine(t, root, res.Coverage, 1)
			if _, err := e.scanOnce(context.Background(), e.passListings()); err != nil {
				t.Fatalf("%s scanOnce: %v", pass, err)
			}
			// ONE worker: the order files come off the channel is the order they went on
			// it, so what the worker saw is the hand-out order, end to end.
			if got := a.paths(); !reflect.DeepEqual(got, want) {
				t.Fatalf("the %s scan handed out\n  %v\nwant\n  %v", pass, got, want)
			}
		}
	})

	t.Run("the order does not read the worker count", func(t *testing.T) {
		for _, workers := range []int{1, 4, 16} {
			if got := enumerated(workers, nil); !reflect.DeepEqual(got, want) {
				t.Errorf("with %d workers configured the enumeration produced\n  %v\nwant\n  %v", workers, got, want)
			}
		}
	})

	t.Run("the order is the enumeration's own and not the filesystem's", func(t *testing.T) {
		reversed := func(dir string) ([]os.DirEntry, error) {
			ents, err := os.ReadDir(dir)
			sort.Slice(ents, func(i, j int) bool { return ents[i].Name() > ents[j].Name() })
			return ents, err
		}
		if got := enumerated(1, reversed); !reflect.DeepEqual(got, want) {
			t.Errorf("over a filesystem returning each listing in reverse name order the enumeration "+
				"produced\n  %v\nwant\n  %v", got, want)
		}
	})

	// The order is unaffected by WHICH WORKER FINISHES WHEN. Two workers, an unbuffered
	// channel, and every worker held inside its file until this case releases it: with both
	// held the enumeration is blocked on its next send, so releasing one - the one handed the
	// MOST RECENT file, which is the reverse of the order they went out in - frees exactly one
	// receiver and the next arrival is unambiguous. Only the first two arrivals can be
	// permuted (two workers racing to record one each), so those are asserted as a SET and
	// every arrival after them in exact sequence.
	t.Run("the order is unaffected by the order workers finish in", func(t *testing.T) {
		const workers = 2
		gates := make(map[string]chan struct{}, len(want))
		for _, p := range want {
			gates[p] = make(chan struct{})
		}
		arrived := make(chan string, len(want))

		e, _ := streamEngine(t, root, res.Coverage, workers)
		e.statFn = func(path string) (os.FileInfo, error) {
			arrived <- path
			<-gates[path]
			return os.Stat(path)
		}

		done := make(chan error, 1)
		go func() {
			_, err := e.scanOnce(context.Background(), e.passListings())
			done <- err
		}()

		var got, inFlight []string
		for len(got) < len(want) {
			select {
			case p := <-arrived:
				got = append(got, p)
				inFlight = append(inFlight, p)
			case <-time.After(streamWait):
				for _, p := range inFlight {
					close(gates[p])
				}
				t.Fatalf("the scan handed out %d of %d files and then stopped for %s", len(got), len(want), streamWait)
			}
			if len(inFlight) == workers || len(got) == len(want) {
				last := inFlight[len(inFlight)-1]
				inFlight = inFlight[:len(inFlight)-1]
				close(gates[last])
			}
		}
		for _, p := range inFlight {
			close(gates[p])
		}
		if err := <-done; err != nil {
			t.Fatalf("scanOnce: %v", err)
		}

		if !sameSet(got[:workers], want[:workers]) {
			t.Errorf("the first %d files handed out were %v, want those two in some order: %v",
				workers, got[:workers], want[:workers])
		}
		if !reflect.DeepEqual(got[workers:], want[workers:]) {
			t.Errorf("with workers finishing in the reverse of the order they were fed, the scan handed out\n"+
				"  %v\nwant\n  %v\nfrom position %d on", got, want, workers)
		}
	})
}

// fourDirs builds a library of four covered directories and returns the root and those
// directories in the order a scan takes them.
//
// The SECOND holds no source at all, which is not decoration: a stop is noticed at two
// different places in the enumeration - between two directories, and at a file it was about
// to hand out - and a library whose every directory holds a source only ever exercises the
// second. A run of source-free directories after a cancellation is precisely where a scan
// could go on listing (and so go on OBSERVING) after it had stopped feeding.
func fourDirs(t *testing.T) (root string, dirs []string) {
	t.Helper()
	root = t.TempDir()
	for i, name := range []string{"a-dir", "b-dir", "c-dir", "d-dir"} {
		dir := filepath.Join(root, name)
		if i == 1 {
			mustWrite(t, filepath.Join(dir, "notes.txt"))
		} else {
			mustWrite(t, filepath.Join(dir, "Source.mkv"))
		}
		dirs = append(dirs, dir)
	}
	return root, dirs
}

// stopPoints are the two places a stop is noticed, named by the directory whose listing
// triggers it. In both, the first directory's source has already gone out to a worker.
var stopPoints = []struct {
	name   string
	stopAt int
}{
	{"between two directories, with no file to hand out", 1},
	{"at a file it was about to hand out", 2},
}

// TestScan_CancelMidStreamObservesOnlyWhatItListed is [AC-5].
//
// Cancellation lands with the second covered directory listed and the third and fourth not.
// The set this reports as observed is asserted by IDENTITY: a directory it never listed
// appearing there is the failure this whole item is written around, because the retention
// pass reads a file missing from an observed directory as a file that is GONE and expires
// the undo record that is the only route back to the original bytes.
func TestScan_CancelMidStreamObservesOnlyWhatItListed(t *testing.T) {
	for _, point := range stopPoints {
		t.Run("cancelled "+point.name, func(t *testing.T) {
			root, dirs := fourDirs(t)
			e, a := streamEngine(t, root, dirs, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			seen := newListedDirs()
			e.readDirFn = func(dir string) ([]os.DirEntry, error) {
				ents, err := seen.readDir(dir)
				if dir == dirs[point.stopAt] {
					// Listed, and cancelled with the listing in hand: THIS directory is
					// evidence, and the ones after it must not become any.
					cancel()
				}
				return ents, err
			}

			observed, err := e.scanOnce(ctx, e.passListings())
			if !errors.Is(err, context.Canceled) {
				t.Errorf("scanOnce returned %v, want the cancellation error handed back to the caller", err)
			}

			want := map[string]bool{}
			for _, dir := range dirs[:point.stopAt+1] {
				want[dir] = true
			}
			if !reflect.DeepEqual(observed, want) {
				t.Fatalf("observed = %v, want exactly %v: a cancelled scan reports the directories it had "+
					"already listed and no others", sortedKeys(observed), sortedKeys(want))
			}
			for _, dir := range dirs[point.stopAt+1:] {
				if seen.was(dir) {
					t.Errorf("%s was listed after the scan was cancelled; a cancelled scan stops where it "+
						"stands, and a directory it goes on to list is one it would go on to OBSERVE", dir)
				}
			}
			for _, p := range a.paths() {
				if filepath.Dir(p) != dirs[0] {
					t.Errorf("%s was handed to a worker after the scan was cancelled", p)
				}
			}
		})
	}
}

// TestScan_PauseMidStreamObservesOnlyWhatItListed is [AC-6].
//
// A worker is held inside the first file for the whole of the pause, so all four clauses are
// asserted of one pass: no new file goes out, the in-flight one finishes (the scan does not
// return until it has), the observed set is exactly what had been listed by then, and what
// was never handed out is handed out by the next scan after resume.
func TestScan_PauseMidStreamObservesOnlyWhatItListed(t *testing.T) {
	for _, point := range stopPoints {
		t.Run("paused "+point.name, func(t *testing.T) {
			root, dirs := fourDirs(t)
			e, a := streamEngine(t, root, dirs, 1)
			var paused atomic.Bool
			e.Paused = paused.Load

			first := filepath.Join(dirs[0], "Source.mkv")
			begun := make(chan struct{})
			finish := make(chan struct{})
			a.after(func(string) {
				close(begun)
				<-finish
			})

			seen := newListedDirs()
			var pauseOnce sync.Once
			e.readDirFn = func(dir string) ([]os.DirEntry, error) {
				if dir == dirs[point.stopAt] {
					// The pause lands with a worker inside the first file and this
					// directory's listing in hand: paused WHILE the enumeration is still
					// running, which is [AC-6]'s premise. Once only, so the scan after
					// resume is not paused again.
					pauseOnce.Do(func() {
						<-begun
						paused.Store(true)
					})
				}
				return seen.readDir(dir)
			}

			type result struct {
				observed map[string]bool
				err      error
			}
			done := make(chan result, 1)
			go func() {
				observed, err := e.scanOnce(context.Background(), e.passListings())
				done <- result{observed, err}
			}()

			select {
			case <-begun:
			case got := <-done:
				t.Fatalf("the scan returned (%v) without any worker having begun a file", got.err)
			case <-time.After(streamWait):
				close(finish)
				t.Fatalf("no worker began a file within %s", streamWait)
			}
			// The in-flight file is still in flight, and the scan has not returned: a pause
			// never interrupts work already running.
			select {
			case got := <-done:
				close(finish)
				t.Fatalf("the paused scan returned (%v) with a file still in flight", got.err)
			case <-time.After(250 * time.Millisecond):
			}
			close(finish)

			got := <-done
			if got.err != nil {
				t.Errorf("scanOnce returned %v; a pause is not an error", got.err)
			}

			want := map[string]bool{}
			for _, dir := range dirs[:point.stopAt+1] {
				want[dir] = true
			}
			if !reflect.DeepEqual(got.observed, want) {
				t.Fatalf("observed = %v, want exactly %v: a paused scan reports the directories it had "+
					"already listed and no others", sortedKeys(got.observed), sortedKeys(want))
			}
			for _, dir := range dirs[point.stopAt+1:] {
				if seen.was(dir) {
					t.Errorf("%s was listed after the scan was paused; a directory it goes on to list is "+
						"one it would go on to OBSERVE", dir)
				}
			}
			if fed := a.paths(); !reflect.DeepEqual(fed, []string{first}) {
				t.Errorf("the paused scan handed out %v, want only the file that was already in flight", fed)
			}

			// Resumed: the files it never handed out are still pending, and the next scan
			// takes them.
			paused.Store(false)
			a.after(nil)
			next, err := e.scanOnce(context.Background(), e.passListings())
			if err != nil {
				t.Fatalf("the scan after resume: %v", err)
			}
			all := []string{first, filepath.Join(dirs[2], "Source.mkv"), filepath.Join(dirs[3], "Source.mkv")}
			if fed := a.paths()[1:]; !reflect.DeepEqual(fed, all) {
				t.Errorf("the scan after resume handed out %v, want every source including the ones the "+
					"paused scan left pending: %v", fed, all)
			}
			if len(next) != len(dirs) {
				t.Errorf("the scan after resume observed %v, want all four directories", sortedKeys(next))
			}
		})
	}
}

// TestScan_ListingErrorMidStreamIsNotObservedAndDoesNotAbort is [AC-7].
//
// One directory's listing fails while a worker is running. The scan keeps going, that worker
// is untouched, the directory is absent from the observed set, the failure is not the scan's
// returned error, and it is RECORDED once - naming the directory, the operation and that the
// run continues without it (observability O4), at warn because the process continued in a
// degraded state and no human has to act for this pass to finish (O3).
func TestScan_ListingErrorMidStreamIsNotObservedAndDoesNotAbort(t *testing.T) {
	root, dirs := fourDirs(t)
	e, a := streamEngine(t, root, dirs, 1)
	log := &safeLog{}
	e.Log = log.logger()

	begun := make(chan struct{})
	failed := make(chan struct{})
	first := filepath.Join(dirs[0], "Source.mkv")
	a.after(func(path string) {
		if path != first {
			return
		}
		close(begun)
		// Held until the listing has failed, so the failure lands on a RUNNING worker.
		<-failed
	})

	boom := errors.New("simulated listing failure")
	e.readDirFn = func(dir string) ([]os.DirEntry, error) {
		if dir == dirs[1] {
			<-begun
			defer close(failed)
			return nil, boom
		}
		return os.ReadDir(dir)
	}

	observed, err := e.scanOnce(context.Background(), e.passListings())
	if err != nil {
		t.Errorf("scanOnce returned %v; a directory this run could not list is skipped with a reason, never "+
			"turned into the scan's error", err)
	}

	want := map[string]bool{dirs[0]: true, dirs[2]: true, dirs[3]: true}
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("observed = %v, want exactly %v: a directory whose listing FAILED is no evidence about "+
			"what is in it", sortedKeys(observed), sortedKeys(want))
	}

	var wantFed []string
	for _, dir := range []string{dirs[0], dirs[2], dirs[3]} {
		wantFed = append(wantFed, filepath.Join(dir, "Source.mkv"))
	}
	if fed := a.paths(); !reflect.DeepEqual(fed, wantFed) {
		t.Errorf("the scan handed out %v, want %v: the enumeration carries on past a directory it could not "+
			"list, and the worker already running is left alone", fed, wantFed)
	}

	out := log.String()
	var records []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "could not be listed") {
			records = append(records, line)
		}
	}
	if len(records) != 1 {
		t.Fatalf("the failed listing was recorded %d time(s), want exactly once:\n%s", len(records), out)
	}
	rec := records[0]
	for _, must := range []string{"level=WARN", dirs[1], "operation=", "continues without it"} {
		if !strings.Contains(rec, must) {
			t.Errorf("the record does not carry %q: %s", must, rec)
		}
	}
}

// TestScan_HoldBacksSurviveStreaming is [AC-8].
//
// Streaming moved where a path LEAVES the enumeration; it must not move where the two
// hold-backs are asked, nor when the undo retention area is skipped. Every withheld path
// here sits in a directory the scan does list, and an ordinary source sits beside it, so
// nothing passes by the scan having narrowed itself.
func TestScan_HoldBacksSurviveStreaming(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	ctx := context.Background()

	parkedSrc := filepath.Join(root, "Parked.mkv")
	parkedRepl := filepath.Join(root, "Parked."+RetainedMarker+".mkv")
	orphanRepl := filepath.Join(root, "Orphan."+RetainedMarker+".mkv") // no record at all
	excluded := filepath.Join(root, "Excluded.mkv")                    // an ORDINARY name: only the record holds it
	ordinary := filepath.Join(root, "Ordinary.mkv")
	deep := filepath.Join(root, "sub", "Deep.mkv")
	undo := undoDirFor(root)
	inUndo := filepath.Join(undo, "Rescued.mkv") // a plain source NAME inside the retention area
	for _, p := range []string{parkedSrc, parkedRepl, orphanRepl, excluded, ordinary, deep, inUndo} {
		mustWrite(t, p)
	}

	if err := ts.RecordSwapIncident(ctx, store.SwapIncident{
		SourcePath: parkedSrc, SourceFingerprint: "1:1", ReplacementPath: parkedRepl,
		SourceAttrs: "1:1", ReplacementAttrs: "2:2", Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record the parked incident: %v", err)
	}
	// A replacement whose recorded disposition still EXCLUDES it: resolved, so nothing is
	// parked on its account and the record is the only thing holding the path back.
	if err := ts.RecordSwapIncident(ctx, store.SwapIncident{
		SourcePath: ordinary, SourceFingerprint: "3:3", ReplacementPath: excluded,
		SourceAttrs: "3:3", ReplacementAttrs: "4:4", Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record the second incident: %v", err)
	}
	if err := ts.ResolveIncident(ctx, 2, store.Resolution{
		Determination: store.SourceIsIntact, By: "operator",
		DispositionSource: store.KeptInPlace, DispositionReplacement: store.RetainedExcluded,
	}); err != nil {
		t.Fatalf("resolve the second incident: %v", err)
	}

	coverage := []string{root, filepath.Join(root, "sub"), undo}
	e, a := streamEngine(t, root, coverage, 1)
	e.Store = ts
	e.held.Store(e.loadHoldBacks(ctx))

	observed, err := e.scanOnce(ctx, e.passListings())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}

	fed := a.paths()
	for _, held := range []struct{ path, why string }{
		{parkedRepl, "a retained replacement, held back on its NAME as something holdfast wrote"},
		{orphanRepl, "a retained replacement no record survived for, held back on its name alone"},
		{parkedSrc, "a parked job's recorded SOURCE path"},
		{excluded, "a replacement whose recorded disposition still excludes it"},
		{inUndo, "a source name inside the undo retention area"},
	} {
		if contains(fed, held.path) {
			t.Errorf("%s reached a worker: %s", held.path, held.why)
		}
	}
	if want := []string{ordinary, deep}; !reflect.DeepEqual(fed, want) {
		t.Errorf("the scan handed out %v, want exactly the two ordinary sources %v", fed, want)
	}
	if observed[undo] {
		t.Error("the undo retention area was reported as observed; it is skipped by DIRECTORY NAME before " +
			"anything in it is read, and a directory this run never opened is no evidence about what is in it")
	}
	wantObserved := map[string]bool{root: true, filepath.Join(root, "sub"): true}
	if !reflect.DeepEqual(observed, wantObserved) {
		t.Errorf("observed = %v, want exactly %v", sortedKeys(observed), sortedKeys(wantObserved))
	}
}

// TestScan_EmptyCoverageStreamsNothingAndObservesWhatItListed is [AC-11], both of its cases:
// a covered set with nothing in it, and one whose every directory lists empty. The second is
// the one that matters - holdfast LOOKED there, which is evidence, and is not the same as
// never looking.
func TestScan_EmptyCoverageStreamsNothingAndObservesWhatItListed(t *testing.T) {
	t.Run("the covered set is empty", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "Unreachable.mkv"))
		e, a := streamEngine(t, root, []string{}, 1)
		observed, err := e.scanOnce(context.Background(), e.passListings())
		if err != nil {
			t.Fatalf("scanOnce: %v", err)
		}
		if fed := a.paths(); len(fed) != 0 {
			t.Errorf("a scan bounded by an empty covered set handed out %v", fed)
		}
		if len(observed) != 0 {
			t.Errorf("observed = %v, want nothing at all", sortedKeys(observed))
		}
	})

	t.Run("every covered directory lists empty", func(t *testing.T) {
		root := t.TempDir()
		var coverage []string
		for _, name := range []string{"a-empty", "b-empty"} {
			dir := filepath.Join(root, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			coverage = append(coverage, dir)
		}
		e, a := streamEngine(t, root, coverage, 1)
		observed, err := e.scanOnce(context.Background(), e.passListings())
		if err != nil {
			t.Fatalf("scanOnce: %v", err)
		}
		if fed := a.paths(); len(fed) != 0 {
			t.Errorf("a scan over directories that all list empty handed out %v", fed)
		}
		want := map[string]bool{coverage[0]: true, coverage[1]: true}
		if !reflect.DeepEqual(observed, want) {
			t.Fatalf("observed = %v, want exactly %v", sortedKeys(observed), sortedKeys(want))
		}
	})
}

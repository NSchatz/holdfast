package engine

// The declared QUEUE ORDER (S0095): which candidate a scan offers first, and what that
// ordering costs.
//
// Every case here names the acceptance criterion it grades (testing T1). Two of them carry
// the names the item mandated - TestEnumerate_LargestFirstIsTotalAndDeterministic and
// TestEnumerate_OrderingAddsNoAdditionalStat - and each of those carries a comment saying
// which criterion it is, because the mandated name does not.
//
// THE SEAMS, and what they stand in for. The per-candidate attribute read (statFn) is the
// ONE route the ordering takes to a file's size and modification time, so substituting it
// is how a case COUNTS what an ordering costs rather than timing it: on a warm page cache
// elapsed time cannot tell one read from three. The directory listing (readDirFn) is
// substituted only where a case needs a library too large to put on a disk. Neither stands
// in for the subject: the enumeration, the ordering and the feed under test are the real
// ones, and the cases that assert an ORDER read real files with real sizes and real
// modification times.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/heapmeasure"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- fixtures ---------------------------------------------------------------------

// orderedFile is one fixture file: where it goes, how big it is and when it was last
// modified. The two numbers are what the four keyed orders sort on, and they are written
// onto real files so the ordering reads what a real scan reads.
type orderedFile struct {
	rel   string
	bytes int
	mtime time.Time
}

// at is a modification time that is unambiguous to read in a failure message.
func at(year int) time.Time { return time.Date(year, 3, 4, 5, 6, 7, 0, time.UTC) }

// writeLibrary lays the files out under a fresh root and returns it. Sizes are real bytes
// on disk and modification times are really set, so nothing here depends on a double.
func writeLibrary(t *testing.T, files []orderedFile) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), f.bytes), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// orderingLibrary is the fixture every sequence case reads: five files whose size order,
// modification-time order and path order are three different sequences, so an assertion
// about one of them cannot be satisfied by accident by another.
var orderingLibrary = []orderedFile{
	{rel: "a/alpha.mkv", bytes: 500, mtime: at(2020)},
	{rel: "b/bravo.mkv", bytes: 100, mtime: at(2024)},
	{rel: "c/charlie.mkv", bytes: 300, mtime: at(2022)},
	{rel: "d/delta.mkv", bytes: 900, mtime: at(2021)},
	{rel: "e/echo.mkv", bytes: 700, mtime: at(2023)},
}

// orderingEngine builds an engine over root with the queue order set and nothing else
// unusual. It needs no tooling: nothing here encodes, and the enumeration is what is under
// test.
func orderingEngine(t *testing.T, root, order string) *Engine {
	t.Helper()
	cfg := baseCfg(root)
	cfg.QueueOrder = order
	eng := toollessEngine(t, cfg, newTestStore(t, root), discardLogger())
	eng.EnsureHoldBacks(context.Background())
	return eng
}

// coveredOrderingEngine is orderingEngine over the COVERAGE-bounded branch, with the
// directories in the sequence a startup walk produces: a parent before its own
// subdirectories.
//
// That branch is what a daemon runs, and it is the one whose hand-out order is NOT the
// full paths in ascending order (docs/enumeration.md): a file sitting in a directory is
// handed out before everything inside that directory's subdirectories. A case about the
// tie-break has to be built on it, or "break ties on the path" and "leave ties in the
// order they arrived" produce the same sequence and the assertion grades neither.
func coveredOrderingEngine(t *testing.T, root, order string, dirs ...string) *Engine {
	t.Helper()
	eng := orderingEngine(t, root, order)
	for i, d := range dirs {
		dirs[i] = filepath.Join(root, d)
	}
	eng.Coverage = dirs
	return eng
}

// enumerated is the sequence one enumeration hands out, as paths relative to root so a
// failure message is readable.
func enumerated(t *testing.T, eng *Engine, root string) []string {
	t.Helper()
	files, _ := eng.enumerate()
	out := make([]string, 0, len(files))
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			t.Fatalf("filepath.Rel(%s, %s): %v", root, f, err)
		}
		out = append(out, rel)
	}
	return out
}

// countingStat wraps the real attribute read with a counter. It is the instrument AC-5 is
// graded with: an ABSOLUTE count of the stat-family calls one enumeration makes through the
// engine's own seam, taken from this build rather than compared against a build that no
// longer exists.
func countingStat(eng *Engine, n *atomic.Int64) {
	eng.statFn = func(path string) (os.FileInfo, error) {
		n.Add(1)
		return os.Stat(path)
	}
}

// ---- [AC-1] the five orders -------------------------------------------------------

// TestEnumerate_OffersTheCandidatesInTheConfiguredOrder is [AC-1]. Each of the five values
// produces its own sequence over one fixture whose size, time and path orders all differ.
func TestEnumerate_OffersTheCandidatesInTheConfiguredOrder(t *testing.T) {
	root := writeLibrary(t, orderingLibrary)
	for _, tc := range []struct {
		order string
		want  []string
	}{
		{config.QueueOrderPath, []string{"a/alpha.mkv", "b/bravo.mkv", "c/charlie.mkv", "d/delta.mkv", "e/echo.mkv"}},
		{config.QueueOrderLargest, []string{"d/delta.mkv", "e/echo.mkv", "a/alpha.mkv", "c/charlie.mkv", "b/bravo.mkv"}},
		{config.QueueOrderSmallest, []string{"b/bravo.mkv", "c/charlie.mkv", "a/alpha.mkv", "e/echo.mkv", "d/delta.mkv"}},
		{config.QueueOrderNewest, []string{"b/bravo.mkv", "e/echo.mkv", "c/charlie.mkv", "d/delta.mkv", "a/alpha.mkv"}},
		{config.QueueOrderOldest, []string{"a/alpha.mkv", "d/delta.mkv", "c/charlie.mkv", "e/echo.mkv", "b/bravo.mkv"}},
	} {
		t.Run(tc.order, func(t *testing.T) {
			got := enumerated(t, orderingEngine(t, root, tc.order), root)
			if !slices.Equal(got, tc.want) {
				t.Errorf("queue_order %q offered\n  %v\nwant\n  %v", tc.order, got, tc.want)
			}
		})
	}
}

// TestEnumerate_EveryOrderOffersTheSameCandidateSet is [AC-1]'s other half and the
// invariant the whole item rests on: this decides SEQUENCE and never membership. A sort
// that dropped, duplicated or invented a candidate would still satisfy the sequences above
// if they were asserted alone.
func TestEnumerate_EveryOrderOffersTheSameCandidateSet(t *testing.T) {
	root := writeLibrary(t, orderingLibrary)
	want := enumerated(t, orderingEngine(t, root, config.QueueOrderPath), root)
	slices.Sort(want)
	for _, order := range config.QueueOrders {
		got := enumerated(t, orderingEngine(t, root, order), root)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("queue_order %q offered the SET\n  %v\nwhere %q offers\n  %v: an order decides "+
				"which file goes first, never which files are offered", order, got, config.QueueOrderPath, want)
		}
	}
}

// ---- [AC-2] the default -----------------------------------------------------------

// TestEnumerate_AnAbsentQueueOrderOffersExactlyWhatPathOffers is [AC-2]. The two
// configurations are run against ONE library in ONE build, so this compares two readings of
// the same thing rather than a claim about the build before the key existed.
func TestEnumerate_AnAbsentQueueOrderOffersExactlyWhatPathOffers(t *testing.T) {
	root := writeLibrary(t, orderingLibrary)
	absent := enumerated(t, orderingEngine(t, root, ""), root)
	explicit := enumerated(t, orderingEngine(t, root, config.QueueOrderPath), root)
	if !slices.Equal(absent, explicit) {
		t.Errorf("a configuration that never mentions queue_order offered\n  %v\nwhere queue_order: path "+
			"offers\n  %v: an install that predates this key must not change what it does by upgrading",
			absent, explicit)
	}

	// And the resolved value is what gets REPORTED, which is the other half of the
	// criterion: an operator reading the startup record of a configuration that sets
	// nothing has to see `path` rather than an empty field.
	cfg := baseCfg(root)
	if got := cfg.EffectiveQueueOrder(); got != config.QueueOrderPath {
		t.Errorf("an absent queue_order resolves to %q, want %q", got, config.QueueOrderPath)
	}
}

// ---- [AC-4] total and deterministic -----------------------------------------------

// TestEnumerate_LargestFirstIsTotalAndDeterministic is [AC-4], and it carries the name the
// item mandated.
//
// THREE files share one size and one carries another, so the tie-break is exercised rather
// than assumed and the second size proves the key still outranks the path.
//
// It is built on the COVERAGE branch, where the hand-out order is deliberately not the full
// paths in ascending order: `movies/zulu.mkv` is handed out before everything under
// `movies/sub/`, which a path sort puts first. So the three tied candidates ARRIVE in an
// order the tie-break has to change, and a sort that merely left ties alone reds here. The
// same library is then enumerated twice by two independently built engines: an order that
// depended on map iteration, on the filesystem's own listing order or on anything else
// unstable passes once and reds on the second reading.
func TestEnumerate_LargestFirstIsTotalAndDeterministic(t *testing.T) {
	root := writeLibrary(t, []orderedFile{
		{rel: "movies/mike.mkv", bytes: 400, mtime: at(2020)},
		{rel: "movies/zulu.mkv", bytes: 400, mtime: at(2021)},
		{rel: "movies/sub/alpha.mkv", bytes: 400, mtime: at(2022)},
		{rel: "movies/sub/biggest.mkv", bytes: 900, mtime: at(2019)},
	})
	covered := func(order string) *Engine {
		return coveredOrderingEngine(t, root, order, "movies", "movies/sub")
	}

	// The order the candidates ARRIVE in, which is the sequence the tie-break has to
	// change. Asserted rather than assumed: if the traversal ever became a path sort, the
	// case below would stop grading the tie-break and would say nothing about it.
	arrive := enumerated(t, covered(config.QueueOrderPath), root)
	if !slices.Equal(arrive, []string{"movies/mike.mkv", "movies/zulu.mkv",
		"movies/sub/alpha.mkv", "movies/sub/biggest.mkv"}) {
		t.Fatalf("the fixture arrives as %v, which is not the coverage branch's own order: the "+
			"tie-break below would be graded against a sequence that already agrees with it", arrive)
	}

	want := []string{
		// The distinct key first, then the three tied candidates on the FULL PATH
		// ascending - not on the basename, and not in the order they arrived.
		"movies/sub/biggest.mkv", "movies/mike.mkv", "movies/sub/alpha.mkv", "movies/zulu.mkv",
	}
	first := enumerated(t, covered(config.QueueOrderLargest), root)
	if !slices.Equal(first, want) {
		t.Errorf("largest-first offered\n  %v\nwant\n  %v", first, want)
	}
	second := enumerated(t, covered(config.QueueOrderLargest), root)
	if !slices.Equal(first, second) {
		t.Errorf("two scans over an unchanged library offered\n  %v\nand\n  %v: a queue nobody can "+
			"predict makes a partial run impossible to reason about", first, second)
	}

	// The order is total under EVERY value, not only this one: two readings agree in all
	// five, which is what makes a resumed pass a continuation rather than a reshuffle.
	for _, order := range config.QueueOrders {
		a := enumerated(t, covered(order), root)
		b := enumerated(t, covered(order), root)
		if !slices.Equal(a, b) {
			t.Errorf("queue_order %q is not deterministic:\n  %v\nthen\n  %v", order, a, b)
		}
		if len(a) != 4 {
			t.Errorf("queue_order %q offered %d candidates, want 4: the order is TOTAL, so every "+
				"candidate is handed out exactly once", order, len(a))
		}
	}
}

// ---- [AC-5] what the ordering costs -----------------------------------------------

// TestEnumerate_OrderingAddsNoAdditionalStat is [AC-5], and it carries the name the item
// mandated.
//
// TWO ABSOLUTE COUNTS, both taken from THIS build through the substituted seam every
// pre-claim attribute read goes through: exactly zero under `path`, and at most one per
// candidate under each of the four keyed orders. It is deliberately not a comparison
// against the build before this change, which nothing in this tree can run.
func TestEnumerate_OrderingAddsNoAdditionalStat(t *testing.T) {
	root := writeLibrary(t, orderingLibrary)
	candidates := int64(len(orderingLibrary))

	t.Run("path reads nothing at all", func(t *testing.T) {
		var reads atomic.Int64
		eng := orderingEngine(t, root, config.QueueOrderPath)
		countingStat(eng, &reads)
		if got := len(enumerated(t, eng, root)); int64(got) != candidates {
			t.Fatalf("enumerated %d candidates, want %d", got, candidates)
		}
		if n := reads.Load(); n != 0 {
			t.Errorf("the path order took %d metadata inspections over %d candidates, want exactly 0: "+
				"the sequence IS the traversal, so there is nothing to read", n, candidates)
		}
	})

	for _, order := range []string{config.QueueOrderLargest, config.QueueOrderSmallest,
		config.QueueOrderNewest, config.QueueOrderOldest} {
		t.Run(order+" reads each candidate once", func(t *testing.T) {
			var reads atomic.Int64
			eng := orderingEngine(t, root, order)
			countingStat(eng, &reads)
			if got := len(enumerated(t, eng, root)); int64(got) != candidates {
				t.Fatalf("enumerated %d candidates, want %d", got, candidates)
			}
			if n := reads.Load(); n > candidates {
				t.Errorf("queue_order %q took %d metadata inspections over %d candidates, want at most "+
					"one each: a second pass over the library is exactly what this ordering may not add",
					order, n, candidates)
			}
		})
	}
}

// ---- [AC-6] what the ordering holds, and when the first file moves ----------------

// TestScan_ThePathOrderFeedsAWorkerBeforeTheLibraryIsListed is [AC-6]'s streaming half.
//
// The last covered directory's listing is held open until the case releases it. Under
// `path` a candidate has to reach a worker while that listing is still outstanding; a build
// that collected the library before feeding anything never reaches one and reds on the
// bound.
func TestScan_ThePathOrderFeedsAWorkerBeforeTheLibraryIsListed(t *testing.T) {
	root := t.TempDir()
	early := filepath.Join(root, "aaa-early")
	slow := filepath.Join(root, "zzz-slow")
	earlyFile := filepath.Join(early, "Early.mkv")
	mustWrite(t, earlyFile)
	mustWrite(t, filepath.Join(slow, "Late.mkv"))

	cfg := baseCfg(root)
	cfg.QueueOrder = config.QueueOrderPath
	eng := toollessEngine(t, cfg, newTestStore(t, root), discardLogger())
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

	// A worker has BEGUN a file the instant ProcessFile reads its attributes, which is the
	// first thing it does with a path it was handed. Answering "not there" is the cheapest
	// honest per-file path there is: the door leaves such a file alone, so what this
	// measures is the feed and not an encode.
	begun := make(chan string, 4)
	eng.statFn = func(path string) (os.FileInfo, error) {
		select {
		case begun <- path:
		default:
		}
		return nil, fs.ErrNotExist
	}

	ctx := context.Background()
	eng.EnsureHoldBacks(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := eng.scanOnce(ctx, eng.passListings(), nil)
		done <- err
	}()

	select {
	case got := <-begun:
		if slowListingReturned.Load() {
			t.Fatalf("a worker began %s only after the last directory's listing had returned", got)
		}
		if got != earlyFile {
			t.Errorf("a worker began %s first, want %s", got, earlyFile)
		}
	case err := <-done:
		close(release)
		t.Fatalf("the scan returned (%v) with no worker having begun a file while the last covered "+
			"directory's listing was still outstanding: under queue_order: path the first candidate "+
			"must reach a worker before the library has been listed", err)
	case <-time.After(20 * time.Second):
		close(release)
		<-done
		t.Fatal("no worker began any file while the last directory's listing was blocked")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
}

// syntheticDirEntry is one generated directory entry: its name, and the two answers a
// listing is asked for. Nothing here exists on a disk.
type syntheticDirEntry struct{ name string }

func (e syntheticDirEntry) Name() string               { return e.name }
func (e syntheticDirEntry) IsDir() bool                { return false }
func (e syntheticDirEntry) Type() fs.FileMode          { return 0 }
func (e syntheticDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("synthetic entry") }

// syntheticInfo is what the substituted attribute read answers with: a size and a
// modification time and nothing else, which is all an ordering key is taken from.
type syntheticInfo struct {
	size  int64
	mtime time.Time
}

func (i syntheticInfo) Name() string       { return "" }
func (i syntheticInfo) Size() int64        { return i.size }
func (i syntheticInfo) Mode() fs.FileMode  { return 0 }
func (i syntheticInfo) ModTime() time.Time { return i.mtime }
func (i syntheticInfo) IsDir() bool        { return false }
func (i syntheticInfo) Sys() any           { return nil }

// TestEnumerate_AKeyedOrderHoldsOnlyTheKeyAndThePath is [AC-6]'s memory half.
//
// THE FIGURE AND WHAT IT IS TAKEN OVER, both stated here rather than in prose beside it
// (performance PB4, PB5): 100,000 candidates, spread over 1,000 directories, whose paths
// are at most 120 bytes - a length this case asserts rather than assumes. The reading is
// live heap above a baseline taken in the SAME process after the fixture was built, which
// is the only heap figure this repository treats as comparable (see internal/heapmeasure).
//
// The library is synthetic and is never written to a disk: both seams are substituted, so a
// hundred thousand paths cost no inodes and the case runs in the ordinary suite.
func TestEnumerate_AKeyedOrderHoldsOnlyTheKeyAndThePath(t *testing.T) {
	const (
		candidates    = 100_000
		directories   = 1_000
		maxPathBytes  = 120
		bytesAllowed  = 256 // per candidate, the ceiling this criterion states
		samplesWanted = 50
	)
	perDir := candidates / directories

	// A short, synthetic root: the path length is part of what is being measured, so it is
	// controlled here rather than inherited from wherever the test binary's temp directory
	// happens to be.
	const root = "/lib"
	dirs := make([]string, directories)
	for i := range dirs {
		dirs[i] = fmt.Sprintf("%s/Library/Section %02d/Title %05d (2019) Extended Edition", root, i%100, i)
	}
	name := func(dir, i int) string {
		return fmt.Sprintf("Title %05d - s01e%03d - 2160p HEVC Remux.mkv", dir, i)
	}
	if longest := len(dirs[directories-1]) + 1 + len(name(directories-1, perDir-1)); longest > maxPathBytes {
		t.Fatalf("the fixture's longest path is %d bytes, over the %d this figure is stated at: the "+
			"measurement below would be taken over paths the criterion does not describe", longest, maxPathBytes)
	}

	cfg := config.Config{LibraryRoots: []string{root}, VideoExts: []string{"mkv"},
		QueueOrder: config.QueueOrderLargest}
	eng := New(cfg, probe.New("", ""), nil, nil, discardLogger())
	eng.Coverage = dirs

	var reads atomic.Int64
	eng.statFn = func(path string) (os.FileInfo, error) {
		reads.Add(1)
		return syntheticInfo{size: int64(len(path)), mtime: at(2020)}, nil
	}

	// The baseline is read with the coverage set and the engine already built, so what the
	// probe reports is what the ORDERING held and not what its fixture weighs.
	probeHeap := heapmeasure.Start(fmt.Sprintf("the queue a keyed order holds over %d candidates", candidates))
	every := directories / samplesWanted
	listed := 0
	eng.readDirFn = func(dir string) ([]os.DirEntry, error) {
		i := slices.Index(dirs, dir)
		if i < 0 {
			return nil, fmt.Errorf("%s is not one of the synthetic library's directories", dir)
		}
		listed++
		if listed%every == 0 {
			probeHeap.Sample()
		}
		out := make([]os.DirEntry, perDir)
		for j := range out {
			out[j] = syntheticDirEntry{name: name(i, j)}
		}
		return out, nil
	}

	offered := 0
	eng.enumerateOrdered(eng.passListings(), sink{offer: func(string) bool { offered++; return true }})
	if offered != candidates {
		t.Fatalf("the ordering offered %d candidates, want %d", offered, candidates)
	}
	if n := reads.Load(); n != candidates {
		t.Fatalf("the ordering took %d metadata inspections over %d candidates", n, candidates)
	}

	peak, err := probeHeap.Peak()
	if err != nil {
		t.Fatalf("%v", err)
	}
	perCandidate := float64(peak) / float64(candidates)
	t.Logf("%d candidates over %d directories, paths at most %d bytes: peak heap %d bytes above "+
		"baseline, %.1f bytes per candidate (ceiling %d)",
		candidates, directories, maxPathBytes, peak, perCandidate, bytesAllowed)
	if perCandidate > bytesAllowed {
		t.Errorf("a keyed order held %.1f bytes per candidate at %d candidates, over the %d this "+
			"criterion allows (%d bytes above a baseline taken in the same process): what a queue "+
			"holds per candidate is the ordering key and the path, and anything else in it is "+
			"multiplied by the size of the library",
			perCandidate, candidates, bytesAllowed, peak)
	}
}

// ---- [AC-9] the resumed feed ------------------------------------------------------

// lockedBuffer is a log sink several goroutines may write to at once. The scan's workers
// and its feed both log, so an unguarded buffer is a data race rather than a capture.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// spentAWorker records every path a WORKER was actually spent on, which is the observable
// AC-9 is about and is not the same as the set of paths that got past the claim: a file the
// claim turns away has still cost a worker the attribute read, the hold-back re-check and
// the claim transaction, and that cost is exactly what the feed's hold-out exists to avoid.
//
// PathIsExcluded is the marker because ProcessFile asks it of every file it is handed, on
// the worker's own goroutine, and nothing else in a pass asks it at all.
type spentAWorker struct {
	store.Store
	mu    sync.Mutex
	paths []string
}

func (s *spentAWorker) PathIsExcluded(ctx context.Context, path string) (bool, error) {
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
	return s.Store.PathIsExcluded(ctx, path)
}

// spent is the paths a worker was spent on, in the order they were handed over. The order
// is the FEED's own as long as the pass runs one worker, which is this configuration's
// default and is what the cases below rely on.
func (s *spentAWorker) spent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// TestScan_AResumedFeedPassesOverTheRowsAClaimWouldRefuse is [AC-9].
//
// The library is half decided: two files carry a terminal row recorded under the
// configuration in force, one carries a terminal row recorded under a configuration that
// has since moved, and one has never been seen. A resumed pass must continue in the
// configured order, spend no worker on the two the claim would turn away, and still offer
// the re-opened one and the unseen one - which is the difference between a feed that skips
// settled work and one that quietly stops processing a library.
func TestScan_AResumedFeedPassesOverTheRowsAClaimWouldRefuse(t *testing.T) {
	root := writeLibrary(t, []orderedFile{
		{rel: "lib/done-big.mkv", bytes: 900, mtime: at(2020)},
		{rel: "lib/done-small.mkv", bytes: 100, mtime: at(2021)},
		{rel: "lib/moved.mkv", bytes: 700, mtime: at(2022)},
		{rel: "lib/unseen.mkv", bytes: 500, mtime: at(2023)},
	})
	path := func(rel string) string { return filepath.Join(root, rel) }

	cfg := baseCfg(root)
	cfg.QueueOrder = config.QueueOrderLargest
	st := newTestStore(t, root)
	spent := &spentAWorker{Store: st}
	logs := &lockedBuffer{}
	// At DEBUG, because the per-file line is: one line per already-decided file is the
	// right detail for an operator asking about one file and the wrong volume for a pass
	// over a library of them, so the count is what an ordinary run records.
	eng := toollessEngine(t, cfg, spent,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx := context.Background()

	// The two rows the configuration in force still re-derives, recorded through the same
	// resolution a claim is handed, and one recorded under a configuration that has moved.
	inForce := DecisionInputsFor(cfg)
	moved := store.InputsRead(map[string]string{InputCRF: "1"})
	for _, seed := range []struct {
		rel    string
		inputs store.DecisionInputs
	}{
		{"lib/done-big.mkv", inForce},
		{"lib/done-small.mkv", inForce},
		{"lib/moved.mkv", moved},
	} {
		p := path(seed.rel)
		key := probe.Fingerprint(p)
		if _, err := st.Claim(ctx, p, key, "seed", 3, store.DecisionInputs{}); err != nil {
			t.Fatalf("seeding %s: claim: %v", seed.rel, err)
		}
		if err := st.Finish(ctx, p, key, store.Done, &store.Outcome{DecisionInputs: seed.inputs}, 3); err != nil {
			t.Fatalf("seeding %s: finish: %v", seed.rel, err)
		}
	}

	eng.EnsureHoldBacks(ctx)
	if _, err := eng.scanOnce(ctx, eng.passListings(), nil); err != nil {
		t.Fatalf("scanOnce: %v", err)
	}

	// The SET a worker was spent on, and the ORDER with it: largest-first puts the re-opened
	// 700-byte file ahead of the unseen 500-byte one, so this one assertion grades both
	// halves of the criterion. A file the claim would turn away still costs a worker its
	// attribute read, its hold-back re-check and its claim transaction if the feed hands it
	// over, and not spending that is the whole of what this is for.
	got := spent.spent()
	want := []string{path("lib/moved.mkv"), path("lib/unseen.mkv")}
	if !slices.Equal(got, want) {
		t.Errorf("a worker was spent on\n  %v\nwant\n  %v: a row whose recorded decision inputs "+
			"still match is one the claim refuses, and a row whose inputs have moved is one it "+
			"re-opens - the feed must pass over the first set and offer the second, in the "+
			"configured order", got, want)
	}

	// And it SAID so, in both registers: the file by name at debug, and the COUNT in the
	// line an ordinary run records. A library that is mostly done otherwise produces a scan
	// that offers almost nothing and says nothing about why.
	for _, want := range []string{"done-big.mkv", "done-small.mkv",
		"files_their_recorded_outcome_still_holds=2"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the records never say %q about the files this feed passed over:\n%s",
				want, logs.String())
		}
	}
}

// TestScan_AChangedFileIsStillOfferedThoughItsOldRowIsTerminal is [AC-9]'s unhappy path,
// and the one failure the feed's hold-out must not have.
//
// A terminal row is keyed by the source's size and modification time. A file that has been
// re-downloaded or edited since that row was written keys to no row at all and is claimed
// exactly as an unseen file is - so a hold-out that matched on the PATH would hold the new
// bytes out of the pipeline for ever, and no later scan would ever pick them up.
func TestScan_AChangedFileIsStillOfferedThoughItsOldRowIsTerminal(t *testing.T) {
	root := writeLibrary(t, []orderedFile{{rel: "lib/changed.mkv", bytes: 300, mtime: at(2020)}})
	p := filepath.Join(root, "lib/changed.mkv")

	cfg := baseCfg(root)
	st := newTestStore(t, root)
	spent := &spentAWorker{Store: st}
	eng := toollessEngine(t, cfg, spent, discardLogger())
	ctx := context.Background()

	// A terminal row for the file AS IT WAS, recorded under the configuration in force.
	oldKey := probe.Fingerprint(p)
	if _, err := st.Claim(ctx, p, oldKey, "seed", 3, store.DecisionInputs{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := st.Finish(ctx, p, oldKey, store.Done,
		&store.Outcome{DecisionInputs: DecisionInputsFor(cfg)}, 3); err != nil {
		t.Fatalf("seed finish: %v", err)
	}

	// The file is then replaced with different bytes at a different time, which is what an
	// *arr upgrade or a re-rip looks like from here.
	if err := os.WriteFile(p, bytes.Repeat([]byte("y"), 800), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at(2026), at(2026)); err != nil {
		t.Fatal(err)
	}
	if probe.Fingerprint(p) == oldKey {
		t.Fatal("the fixture did not change the file's key, so this case would prove nothing")
	}

	eng.EnsureHoldBacks(ctx)
	if _, err := eng.scanOnce(ctx, eng.passListings(), nil); err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if got := spent.spent(); !slices.Equal(got, []string{p}) {
		t.Errorf("a worker was spent on %v, want [%s]: the row is terminal for the file that WAS "+
			"at this path, and the file there now has never been seen", got, p)
	}
}

// ---- [AC-10] a key that cannot be read --------------------------------------------

// TestEnumerate_ACandidateWhoseKeyCannotBeReadIsStillOfferedLast is [AC-10]. A file that
// vanished between the listing and the ordering, and one this process may not look at, are
// both still offered - after every candidate whose key was read, in path order among
// themselves - and each is named in a record saying why.
func TestEnumerate_ACandidateWhoseKeyCannotBeReadIsStillOfferedLast(t *testing.T) {
	root := writeLibrary(t, []orderedFile{
		{rel: "movies/readable-small.mkv", bytes: 100, mtime: at(2020)},
		{rel: "movies/sub/readable-big.mkv", bytes: 900, mtime: at(2021)},
		// The two unreadable ones ARRIVE in the opposite order to the one they must be
		// offered in, so the tie-break among them is graded rather than inherited.
		{rel: "movies/vanished.mkv", bytes: 500, mtime: at(2022)},
		{rel: "movies/sub/refused.mkv", bytes: 700, mtime: at(2023)},
	})
	vanished := filepath.Join(root, "movies/vanished.mkv")
	refused := filepath.Join(root, "movies/sub/refused.mkv")

	logs := &bytes.Buffer{}
	eng := coveredOrderingEngine(t, root, config.QueueOrderLargest, "movies", "movies/sub")
	eng.Log = slog.New(slog.NewTextHandler(logs, nil))
	eng.statFn = func(path string) (os.FileInfo, error) {
		switch path {
		case vanished:
			return nil, fs.ErrNotExist
		case refused:
			return nil, fs.ErrPermission
		}
		return os.Stat(path)
	}

	got := enumerated(t, eng, root)
	want := []string{
		"movies/sub/readable-big.mkv", "movies/readable-small.mkv", // keyed, largest first
		"movies/sub/refused.mkv", "movies/vanished.mkv", // unreadable, after them, on the path
	}
	if !slices.Equal(got, want) {
		t.Errorf("a library with two unreadable keys was offered as\n  %v\nwant\n  %v: a candidate "+
			"whose key nobody could read is placed after every candidate whose key WAS read and is "+
			"never dropped - this decides sequence, and a file left out of a queue is a file that "+
			"is never processed", got, want)
	}

	for _, name := range []string{"vanished.mkv", "refused.mkv"} {
		if !strings.Contains(logs.String(), name) {
			t.Errorf("nothing in the records names %s, whose ordering key could not be read:\n%s",
				name, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "permission denied") {
		t.Errorf("the record does not carry the REASON the key could not be read:\n%s", logs.String())
	}
}

// ---- [AC-11] nothing to do --------------------------------------------------------

// TestScan_AnEmptyLibraryCompletesWithoutAMetadataInspection is [AC-11]. Under every value
// of the key, a pass that finds no candidate finishes without an error and without reading
// anything: the cost of an ordering is paid per candidate, so no candidates is no cost.
func TestScan_AnEmptyLibraryCompletesWithoutAMetadataInspection(t *testing.T) {
	for _, order := range config.QueueOrders {
		t.Run(order, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int64
			eng := orderingEngine(t, root, order)
			countingStat(eng, &reads)

			files, observed := eng.enumerate()
			if len(files) != 0 {
				t.Errorf("an empty library enumerated %v", files)
			}
			if len(observed) == 0 {
				t.Error("an empty library observed no directory at all: holdfast LOOKED, and a " +
					"directory it listed and found empty is not the same as one it never listed")
			}
			if n := reads.Load(); n != 0 {
				t.Errorf("a pass over an empty library took %d metadata inspections, want 0", n)
			}

			if _, err := eng.scanOnce(context.Background(), eng.passListings(), nil); err != nil {
				t.Errorf("a pass over an empty library returned an error: %v", err)
			}
		})
	}
}

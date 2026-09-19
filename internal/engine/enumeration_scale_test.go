package engine

// What the ENUMERATION costs at library scale (S0099), measured rather than asserted about.
//
// Two figures, both of them RATIOS across a ten-fold change in input at a FIXED directory
// count, and both of them taken inside one process against one baseline:
//
//   - the peak heap the scan holds   [AC-9]
//   - the time to hand its first file to a worker  [AC-10]
//
// An absolute byte count or an absolute duration is a property of the machine, the build and
// the allocator's mood, and a gate on one is false-positive often enough to be ignored inside
// a month (performance PB5). A ratio between two readings taken the same way in the same run
// is not. A materialising enumeration shows roughly ten; a streaming one shows roughly one;
// the bound is two, which sits an order of magnitude clear of the first and comfortably above
// the second.
//
// It is a BENCHMARK and not a test, deliberately: `make check` runs `go test` with no -bench,
// so this compiles on every gate run and executes on none of them. `make check-enumeration-memory`
// is the one thing that runs it, and that target also refuses a repository whose recorded
// figures are absent or missing their hardware, their build or their date.
//
// SYNTHETIC, never a real library: the filesystem is substituted (readDirFn, the
// coverage-bounded pass's only route to a listing) and generates media-shaped names on
// demand, so a million paths cost no inodes and no test touches real media. The enumeration,
// the feed and the workers under measurement are the real ones.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/heapmeasure"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// scaleDirectories is the FIXED number of directories both measurements are taken over. It
// is fixed because the per-directory observed map is deliberately outside the bound being
// asserted: the thing under measurement is what one more FILE costs, and holding the
// directory count still is what separates the two.
const scaleDirectories = 10_000

// scaleSizes are the two library sizes, ten-fold apart.
var scaleSizes = []int{100_000, 1_000_000}

// scaleRatioBound is the bound both ratios must sit under. See the file comment for why it
// is a ratio and why it is two.
const scaleRatioBound = 2.0

// syntheticEntry is one generated directory entry. It answers the three questions a listing
// is asked - its name, whether the listing called it a directory, and its mode bits - and
// nothing else: listDir never asks a non-symlink entry for its Info.
type syntheticEntry struct{ name string }

func (e syntheticEntry) Name() string               { return e.name }
func (e syntheticEntry) IsDir() bool                { return false }
func (e syntheticEntry) Type() fs.FileMode          { return 0 }
func (e syntheticEntry) Info() (fs.FileInfo, error) { return nil, errors.New("synthetic entry") }

// syntheticLibrary is a library of dirs directories holding total source paths between them,
// generated on demand. Nothing is written to disk and nothing is retained: one call produces
// one directory's listing and the enumeration releases it as it consumes it.
type syntheticLibrary struct {
	root      string
	dirs      []string
	perDir    int
	perDirIdx map[string]int
}

func newSyntheticLibrary(root string, dirs, total int) *syntheticLibrary {
	l := &syntheticLibrary{
		root:      root,
		perDir:    total / dirs,
		perDirIdx: make(map[string]int, dirs),
	}
	for i := 0; i < dirs; i++ {
		// Media-shaped, and deep enough that a path is a realistic length rather than a
		// short one that would understate what a library of them weighs.
		dir := filepath.Join(root, fmt.Sprintf("Library/Section %02d/Title %05d (2019)", i%100, i))
		l.dirs = append(l.dirs, dir)
		l.perDirIdx[dir] = i
	}
	return l
}

func (l *syntheticLibrary) readDir(dir string) ([]os.DirEntry, error) {
	i, ok := l.perDirIdx[dir]
	if !ok {
		return nil, fmt.Errorf("synthetic library: %s is not one of its directories", dir)
	}
	out := make([]os.DirEntry, l.perDir)
	for j := range out {
		out[j] = syntheticEntry{name: fmt.Sprintf("Title %05d - s01e%03d - 2160p HEVC.mkv", i, j)}
	}
	return out, nil
}

// scaleFigure is one library size, measured.
type scaleFigure struct {
	paths   int
	peak    int64
	samples int
	first   time.Duration
	elapsed time.Duration
}

// measureEnumerationAt runs one whole scan over a synthetic library of total paths spread
// over scaleDirectories directories, and reports what it held and how long it took to reach
// a worker.
//
// The heap is sampled from the enumeration's own goroutine, at a fixed fraction of the
// DIRECTORIES, so both sizes are sampled the same number of times and neither gets a better
// chance of catching a peak than the other.
func measureEnumerationAt(b *testing.B, total int) scaleFigure {
	b.Helper()
	root := b.TempDir()
	lib := newSyntheticLibrary(root, scaleDirectories, total)

	db, err := store.Open(filepath.Join(b.TempDir(), "jobs.db"))
	if err != nil {
		b.Fatalf("open the store: %v", err)
	}
	defer func() { _ = db.Close() }()

	cfg := config.Config{LibraryRoots: []string{root}, VideoExts: []string{"mkv"}, Workers: 4}
	e := New(cfg, probe.New("", ""), nil, db, discardLogger())
	e.Coverage = lib.dirs

	start := time.Now()
	var firstAt atomic.Int64
	e.statFn = func(path string) (os.FileInfo, error) {
		// A worker has BEGUN this file: ProcessFile's attribute read is the first thing it
		// does with a path it was handed. Nothing here exists on disk, and the door's
		// fail-safe leaves such a file alone - which is the cheapest honest per-file path
		// there is, so what this measures is the enumeration and the feed and not an encode.
		firstAt.CompareAndSwap(0, int64(time.Since(start)))
		return nil, fs.ErrNotExist
	}

	// The baseline is read with the library, the coverage set and the engine already built,
	// so what the probe reports is what the SCAN held and not what its fixture weighs.
	probeHeap := heapmeasure.Start(fmt.Sprintf("the scan over %d synthetic paths", total))
	const samplesWanted = 50
	every := scaleDirectories / samplesWanted
	listed := 0
	e.readDirFn = func(dir string) ([]os.DirEntry, error) {
		listed++
		if listed%every == 0 {
			probeHeap.Sample()
		}
		return lib.readDir(dir)
	}

	e.EnsureHoldBacks(context.Background())
	if _, err := e.scanOnce(context.Background(), e.passListings(), nil); err != nil {
		b.Fatalf("scanOnce over %d paths: %v", total, err)
	}
	elapsed := time.Since(start)

	peak, err := probeHeap.Peak()
	if err != nil {
		b.Fatalf("%v", err)
	}
	first := time.Duration(firstAt.Load())
	if first == 0 {
		b.Fatalf("no file reached a worker during the scan over %d paths, so there is no time-to-first-file "+
			"to report", total)
	}
	return scaleFigure{paths: total, peak: peak, samples: samplesWanted, first: first, elapsed: elapsed}
}

// BenchmarkScan_EnumerationAtScale is [AC-9] and [AC-10]. It measures, prints and then
// REFUSES: a peak heap or a time-to-first-file that grows with the library rather than with
// the directory count fails this benchmark, which is what `make check-enumeration-memory`
// runs it for.
//
// It ignores b.N and runs each size once: the fixture is a million generated paths and the
// figure is a peak, not a per-operation cost, so repeating it inside the timer would measure
// the same thing again more slowly. Run it with -benchtime 1x.
func BenchmarkScan_EnumerationAtScale(b *testing.B) {
	figures := make([]scaleFigure, 0, len(scaleSizes))
	for _, total := range scaleSizes {
		figures = append(figures, measureEnumerationAt(b, total))
	}

	for _, f := range figures {
		b.Logf("%9d paths over %d directories: peak heap %d bytes (%.2f MiB) over %d samples, "+
			"first file to a worker after %s, whole scan %s",
			f.paths, scaleDirectories, f.peak, float64(f.peak)/(1<<20), f.samples, f.first, f.elapsed)
	}

	small, large := figures[0], figures[1]
	if small.peak <= 0 {
		b.Fatalf("the scan over %d paths held %d bytes above its baseline, so the ratio below would be "+
			"measured against nothing", small.paths, small.peak)
	}
	if ratio := float64(large.peak) / float64(small.peak); ratio >= scaleRatioBound {
		b.Fatalf("peak heap at %d paths is %.2fx the figure at %d paths (%d vs %d bytes), want under %.1fx: "+
			"what the scan holds is growing with the number of FILES in the library, which is what a "+
			"materialising enumeration does and a streaming one does not",
			large.paths, ratio, small.paths, large.peak, small.peak, scaleRatioBound)
	}
	if ratio := float64(large.first) / float64(small.first); ratio >= scaleRatioBound {
		b.Fatalf("the first file reached a worker %.2fx slower at %d paths than at %d paths (%s vs %s), "+
			"want under %.1fx: the first encode is waiting on how much of the library is left to list",
			ratio, large.paths, small.paths, large.first, small.first, scaleRatioBound)
	}
}

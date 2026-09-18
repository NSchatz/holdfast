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
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

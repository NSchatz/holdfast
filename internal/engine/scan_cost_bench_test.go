package engine

// What one NO-OP scan pass costs per file, at the two library sizes an operator actually
// runs (S0098, AC-8). The steady state of a `scan_interval_sec` daemon is a library where
// every file already carries a terminal row, and this measures precisely that pass: the
// enumeration, the pre-claim reads and the claim that concludes there is nothing to do.
//
// It is NOT part of `make check` - the `test` target runs no `-bench` - because a figure
// measured on a shared runner is a figure with a spread, and an absolute number is never a
// build gate. Run it by hand and record what it prints beside the machine, the build and
// the date, in docs/scan-cost.md:
//
//	go test -run '^$' -bench '^BenchmarkScan_NoOpPass$' -benchtime 1x ./internal/engine/
//
// THE FIXTURE IS A SIDE EFFECT AND IS TREATED AS ONE. A hundred thousand files is a real
// demand on whatever filesystem the temporary directory lands on, and these suites share
// that filesystem with every other session on the machine. So the library is built under
// the benchmark's OWN temporary directory, it is removed the moment that size is done
// rather than at the end of the run, and a filesystem that cannot take it FAILS the
// benchmark instead of being filled - a leaked library there reds unrelated suites with
// checkout errors that read as code defects rather than as a full disk.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// scanFixtureDir is the actionable half of the refusal below. The library goes under the
// benchmark's own temporary directory by default, which is the right answer on a machine
// whose temporary filesystem is a real one - and the wrong answer on a machine where it is
// a small shared tmpfs, where the honest choices are to refuse or to be pointed somewhere
// else. This is somewhere else. The library is created under a fresh subdirectory of it
// and removed when that size is finished, exactly as the default is.
//
//	go test -run '^$' -bench '^BenchmarkScan_NoOpPass$' -benchtime 1x ./internal/engine/ \
//	    -args -scan-fixture-dir=/var/tmp
var scanFixtureDir = flag.String("scan-fixture-dir", "",
	"build BenchmarkScan_NoOpPass's library under this directory rather than under the "+
		"benchmark's own temporary one. It is removed either way.")

// noOpFixtureBytesPerFile is what one fixture file costs on disk, generously rounded up
// from the handful of bytes written to a whole filesystem block plus its inode and its
// directory entry, plus the ledger row that goes with it. The check below is a refusal
// bound and not an accounting, so it errs high on purpose.
const noOpFixtureBytesPerFile = 8 << 10

// BenchmarkScan_NoOpPass measures one pass over a library whose every file is already
// terminal, at 10,000 and at 100,000 files.
func BenchmarkScan_NoOpPass(b *testing.B) {
	for _, files := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			b.StopTimer()
			root, cleanup := noOpFixtureRoot(b, files)
			defer cleanup()
			cfg := baseCfg(root)
			// The ledger goes BESIDE the library rather than inside it, so the scan never
			// enumerates the database, and under the same fixture directory, so one
			// removal takes the whole of what this size created.
			st := benchStore(b, filepath.Dir(root))
			seedProcessedLibrary(b, st, root, files, DecisionInputsFor(cfg))

			eng := New(cfg, probe.New(filepath.Join(b.TempDir(), "no-such-ffmpeg"),
				filepath.Join(b.TempDir(), "no-such-ffprobe")), benchRefusingEncoder{b}, st, discardLogger())
			ctx := context.Background()

			b.ResetTimer()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if err := eng.RunOneshot(ctx); err != nil {
					b.Fatalf("RunOneshot: %v", err)
				}
			}
			b.StopTimer()
			// ns/op is the whole PASS. The figure an operator cares about is the per-file
			// cost, so it is reported rather than left to be divided by hand.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
		})
	}
}

// noOpFixtureRoot builds a library of n already-processed files under the benchmark's own
// temporary directory, and returns it with the removal that undoes it. The removal is
// explicit and is called when THIS size is finished rather than when the whole run is, so
// the 10,000-file library is gone before the 100,000-file one is built.
func noOpFixtureRoot(b *testing.B, n int) (string, func()) {
	b.Helper()
	parent := b.TempDir()
	if *scanFixtureDir != "" {
		var err error
		if parent, err = os.MkdirTemp(*scanFixtureDir, "holdfast-scan-cost-"); err != nil {
			b.Fatalf("creating the fixture library under %s: %v", *scanFixtureDir, err)
		}
	}
	root := filepath.Join(parent, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatalf("creating the fixture library: %v", err)
	}
	requireRoomFor(b, root, n)

	// Spread over subdirectories: a single directory holding 100,000 entries measures the
	// filesystem's directory index rather than the pass, and a real library is a tree.
	const perDir = 1000
	body := []byte("a fixture source: nothing here is ever probed, decoded or encoded")
	for i := 0; i < n; i++ {
		if i%perDir == 0 {
			if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%04d", i/perDir)), 0o755); err != nil {
				b.Fatalf("creating a fixture directory: %v", err)
			}
		}
		p := filepath.Join(root, fmt.Sprintf("d%04d", i/perDir), fmt.Sprintf("film%06d.mkv", i))
		if err := os.WriteFile(p, body, 0o644); err != nil {
			// FAIL rather than fill: a partial library is a wrong measurement, and a
			// filesystem that ran out under this is one every other suite on the machine
			// is about to meet as an unexplained checkout error.
			b.Fatalf("the fixture library could not be written at %d of %d files (%v). "+
				"This benchmark needs roughly %d MiB of free space and %d free inodes under %s",
				i, n, err, (int64(n)*noOpFixtureBytesPerFile)>>20, n, root)
		}
	}
	// The whole parent goes, not only the library inside it: where the operator pointed
	// this somewhere of their own, the directory made for it is this benchmark's to remove.
	return root, func() { _ = os.RemoveAll(parent) }
}

// requireRoomFor refuses the run when the filesystem it was handed cannot take the
// library, BEFORE a single file is written. The refusal is the point: the alternative is
// filling a few gigabytes that every other session on this machine is also using.
func requireRoomFor(b *testing.B, dir string, n int) {
	b.Helper()
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		b.Fatalf("cannot measure the free space under %s (%v), and this benchmark will not "+
			"write %d files onto a filesystem it could not size first", dir, err, n)
	}
	wantBytes := uint64(n) * noOpFixtureBytesPerFile
	freeBytes := fs.Bavail * uint64(fs.Bsize)
	if freeBytes < wantBytes {
		b.Fatalf("%s has %d MiB free and this fixture needs about %d MiB for %d files. "+
			"Point TMPDIR at a filesystem that can take it rather than filling this one",
			dir, freeBytes>>20, wantBytes>>20, n)
	}
	// Files is the inode count on the filesystems that report one; zero means it does not
	// report one (btrfs, some network filesystems) and there is nothing to refuse on.
	if fs.Files > 0 && fs.Ffree < uint64(n)+uint64(n)/perDirInodeSlack {
		b.Fatalf("%s has %d free inodes and this fixture needs more than %d. Point TMPDIR at a "+
			"filesystem that can take it", dir, fs.Ffree, n)
	}
}

// perDirInodeSlack is the headroom the directory entries themselves want, as a fraction of
// the file count.
const perDirInodeSlack = 100

// benchStore opens the ledger for one benchmark size under dir.
func benchStore(b *testing.B, dir string) *store.SQLite {
	b.Helper()
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	return st
}

// seedProcessedLibrary records the terminal row a completed pass leaves for every file in
// root: the verdict AND the configuration values the guard read, which is what holds the
// row out of the pipeline on every later pass. Without the inputs every row would be
// re-opened by the inputs rule alone and the pass being measured would not be a no-op.
func seedProcessedLibrary(b *testing.B, st store.Store, root string, n int, in store.DecisionInputs) {
	b.Helper()
	ctx := context.Background()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		key := probe.Fingerprint(p)
		if ok, cerr := st.Claim(ctx, p, key, "seed", 3, in); cerr != nil || !ok {
			return fmt.Errorf("seed claim %s: ok=%v err=%w", p, ok, cerr)
		}
		return st.Finish(ctx, p, key, store.Skipped,
			&store.Outcome{Reason: SkipAlreadyTargetCodec, DecisionInputs: in}, 3)
	})
	if err != nil {
		b.Fatalf("seeding the ledger: %v", err)
	}
}

// benchRefusingEncoder fails the benchmark the moment anything is offered to it. A no-op
// pass that reached the encoder would be measuring something else entirely.
type benchRefusingEncoder struct{ b *testing.B }

func (e benchRefusingEncoder) Encode(_ context.Context, in, _ string, _ *probe.VideoProps) error {
	e.b.Fatalf("a file was offered to the encoder: %q. A no-op pass encodes nothing", in)
	return nil
}

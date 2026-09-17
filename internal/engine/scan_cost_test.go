package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/diskfree"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// WHAT A NO-OP PASS COSTS (S0098).
//
// A fully processed library is the steady state of a scan_interval_sec daemon, and the
// tests here grade what one pass over it SPENDS to reach the same answer as the last one:
// how many write statements it issues per file, and how many times it reads one file's
// attributes. Neither is visible in anything the pass records - a DELETE that matches no
// row and a stat nobody needed leave exactly the same ledger behind as not issuing them -
// so both are counted through a seam rather than inferred, and never from elapsed time,
// which on a warm page cache cannot tell one read from three.

// countingStore counts the store calls a scan makes and attributes each to the file it was
// made about. It WRAPS the real store rather than standing in for it (testing T3): the
// store is outside the engine's boundary, the engine under test runs for real, and every
// call reaches the same SQLite the scan would have reached, so nothing the pass decides is
// changed by being counted.
//
// Every path-keyed write on the Store interface that a scan can reach is counted. The two
// that only an operator surface reaches (ExcludePath, UnexcludePath) are not: no route from
// a scan calls them, so an entry from one would be evidence about the HTTP surface.
type countingStore struct {
	store.Store

	mu sync.Mutex
	// writes is one entry per write statement issued ABOUT A PATH, in order.
	writes []storeWrite
	// claimed is every path a Claim actually took, which is the gate every write after
	// the claim stands behind: nothing claimed means nothing below the claim ran.
	claimed []string
	// claimErr, when set, is what Claim reports instead of taking one. It is the store
	// failing underneath the claim decision, which is the only way that branch is
	// reachable at all.
	claimErr error
}

// storeWrite is one write and the file it was about.
type storeWrite struct{ method, path string }

func (c *countingStore) record(method, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, storeWrite{method: method, path: path})
}

// writesFor returns the methods that wrote about path, so a failure names what was issued
// rather than only how much.
func (c *countingStore) writesFor(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var got []string
	for _, w := range c.writes {
		if w.path == path {
			got = append(got, w.method)
		}
	}
	return got
}

func (c *countingStore) allWrites() []storeWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]storeWrite(nil), c.writes...)
}

// reset forgets what the fixture's own seeding cost, so what is counted is the pass.
func (c *countingStore) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes, c.claimed = nil, nil
}

func (c *countingStore) claimedPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.claimed...)
}

func (c *countingStore) Claim(ctx context.Context, path, fingerprint, worker string, maxFailures int,
	current store.DecisionInputs) (bool, error) {
	c.mu.Lock()
	claimErr := c.claimErr
	c.mu.Unlock()
	if claimErr != nil {
		return false, claimErr
	}
	took, err := c.Store.Claim(ctx, path, fingerprint, worker, maxFailures, current)
	if took {
		c.record("Claim", path)
		c.mu.Lock()
		c.claimed = append(c.claimed, path)
		c.mu.Unlock()
	}
	return took, err
}

func (c *countingStore) ClearSkip(ctx context.Context, path, fingerprint, reason string) error {
	c.record("ClearSkip", path)
	return c.Store.ClearSkip(ctx, path, fingerprint, reason)
}

func (c *countingStore) RecordSkip(ctx context.Context, path, fingerprint, reason string,
	by store.Decision, profile string) (bool, error) {
	c.record("RecordSkip", path)
	return c.Store.RecordSkip(ctx, path, fingerprint, reason, by, profile)
}

func (c *countingStore) Finish(ctx context.Context, path, fingerprint string, s store.Status,
	o *store.Outcome, maxFailures int) error {
	c.record("Finish", path)
	return c.Store.Finish(ctx, path, fingerprint, s, o, maxFailures)
}

func (c *countingStore) Advance(ctx context.Context, path, fingerprint string, s store.Status) error {
	c.record("Advance", path)
	return c.Store.Advance(ctx, path, fingerprint, s)
}

func (c *countingStore) Delete(ctx context.Context, path, fingerprint string) error {
	c.record("Delete", path)
	return c.Store.Delete(ctx, path, fingerprint)
}

func (c *countingStore) Reopen(ctx context.Context, path, fingerprint string, clearFailures bool) (bool, error) {
	c.record("Reopen", path)
	return c.Store.Reopen(ctx, path, fingerprint, clearFailures)
}

func (c *countingStore) Retain(ctx context.Context, r store.Retained) error {
	c.record("Retain", r.SourcePath)
	return c.Store.Retain(ctx, r)
}

func (c *countingStore) MarkRestored(ctx context.Context, sourcePath string, at int64) error {
	c.record("MarkRestored", sourcePath)
	return c.Store.MarkRestored(ctx, sourcePath, at)
}

func (c *countingStore) DropRetained(ctx context.Context, sourcePath string) error {
	c.record("DropRetained", sourcePath)
	return c.Store.DropRetained(ctx, sourcePath)
}

func (c *countingStore) RecordSwapIncident(ctx context.Context, in store.SwapIncident) error {
	c.record("RecordSwapIncident", in.SourcePath)
	return c.Store.RecordSwapIncident(ctx, in)
}

// countingStat counts the attribute reads a scan takes, per file, and performs each for
// real against the filesystem. It is the seam AC-4 is graded through.
type countingStat struct {
	mu sync.Mutex
	n  map[string]int
}

func newCountingStat() *countingStat { return &countingStat{n: map[string]int{}} }

func (c *countingStat) stat(path string) (os.FileInfo, error) {
	c.mu.Lock()
	c.n[path]++
	c.mu.Unlock()
	return os.Stat(path)
}

func (c *countingStat) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[path]
}

func (c *countingStat) worst() (path string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, got := range c.n {
		if got > n {
			path, n = p, got
		}
	}
	return path, n
}

// ---- the already-processed fixture library -----------------------------------

// terminalLibrary writes n source files under root and records a TERMINAL row for each: a
// done row whose recorded decision inputs are the empty set, which is the record a verdict
// no configuration change can move carries, so the row stays terminal for every
// configuration and the pass over it is a true no-op.
//
// The rows are seeded through the store's own Claim/Finish rather than by writing SQL, so
// what the pass meets is a row this build would itself have written.
func terminalLibrary(tb testing.TB, st store.Store, root string, n int) []string {
	tb.Helper()
	ctx := context.Background()
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%06d.mkv", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			tb.Fatalf("write fixture %s: %v (the filesystem under the test's temporary "+
				"directory could not take a library of %d files)", p, err, n)
		}
		key := probe.Fingerprint(p)
		took, err := st.Claim(ctx, p, key, "seed", 3, store.InputsRead(nil))
		if err != nil || !took {
			tb.Fatalf("seed claim %s: took=%v err=%v", p, took, err)
		}
		if err := st.Finish(ctx, p, key, store.Done,
			&store.Outcome{DecisionInputs: store.InputsRead(nil)}, 3); err != nil {
			tb.Fatalf("seed finish %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	return paths
}

// noOpEngine is an engine over root wired to st, with the attribute-read seam pointed at
// stat. Nothing in a no-op pass reaches the prober or the encoder - a terminal row is
// refused at the claim, which is upstream of both - so neither is given a working one, and
// a change that started probing before the claim would surface here as a failure rather
// than as a silent cost.
func noOpEngine(tb testing.TB, root string, st store.Store, stat func(string) (os.FileInfo, error)) *Engine {
	tb.Helper()
	eng := New(baseCfg(root), probe.New("", ""), nil, st, discardLogger())
	eng.statFn = stat
	return eng
}

// countingEngine is the pair the cost tests use: a real SQLite store behind a write
// counter, and an engine over root whose attribute reads are counted.
func countingEngine(t *testing.T, root string) (*Engine, *countingStore, *countingStat) {
	t.Helper()
	dbDir := t.TempDir()
	sq, err := store.Open(filepath.Join(dbDir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	cs := &countingStore{Store: sq}
	stat := newCountingStat()
	return noOpEngine(t, root, cs, stat.stat), cs, stat
}

// ---- AC-1 --------------------------------------------------------------------

// TestScan_IssuesNoStoreWriteForATerminalFile grades AC-1: a file whose stored row is
// already terminal for the current configuration, and whose size and modification time are
// unchanged, costs the store NO write statement.
//
// It counts statements and not time deliberately. The two clears this item removes are a
// DELETE that matches no row, which changes nothing an assertion about the ledger could
// see; the only observable is whether the statement was issued.
func TestScan_IssuesNoStoreWriteForATerminalFile(t *testing.T) {
	root := t.TempDir()
	eng, cs, _ := countingEngine(t, root)
	paths := terminalLibrary(t, cs, root, 4)
	// The seeding went through the counter, so its own writes are proof the counter is
	// wired to the store the engine holds - a shadowed method that stopped being called
	// would otherwise read as "no writes" and pass.
	if seeded := len(cs.allWrites()); seeded != 2*len(paths) {
		t.Fatalf("seeding recorded %d writes through the counter, want %d (one Claim and "+
			"one Finish per file): the write counter is not wired to the engine's store",
			seeded, 2*len(paths))
	}
	cs.reset()

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	for _, p := range paths {
		if got := cs.writesFor(p); len(got) != 0 {
			t.Errorf("%s: a terminal, unmodified file cost %d store write(s): %v",
				filepath.Base(p), len(got), got)
		}
	}
	if took := cs.claimedPaths(); len(took) != 0 {
		t.Errorf("a terminal, unmodified file was claimed: %v", took)
	}
}

// ---- AC-12 -------------------------------------------------------------------

// TestScan_EmptyLibraryIssuesNoStoreWrite grades AC-12: a scan over a library holding no
// eligible file completes without error and issues no write about a file.
func TestScan_EmptyLibraryIssuesNoStoreWrite(t *testing.T) {
	root := t.TempDir()
	// A directory and a file with no video extension: both are enumerated and neither is
	// eligible, so the pass has something to walk and nothing to work on.
	if err := os.Mkdir(filepath.Join(root, "season 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "poster.jpg"), []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, cs, _ := countingEngine(t, root)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot over an empty library: %v", err)
	}
	if got := cs.allWrites(); len(got) != 0 {
		t.Errorf("an empty library cost %d store write(s) about a file: %v", len(got), got)
	}
}

// ---- AC-4 --------------------------------------------------------------------

// TestScan_ReadsFileAttributesOncePerFile grades AC-4: processing one file reads that
// file's filesystem attributes at most once before the claim decision.
//
// The count comes from the engine's own stat seam, which every pre-claim read goes through.
// An elapsed-time proxy is refused by the criterion and would be worthless anyway: three
// stats of a file whose inode is in cache are not distinguishable from one by a clock.
func TestScan_ReadsFileAttributesOncePerFile(t *testing.T) {
	root := t.TempDir()
	eng, cs, stat := countingEngine(t, root)
	paths := terminalLibrary(t, cs, root, 4)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	for _, p := range paths {
		if got := stat.count(p); got > 1 {
			t.Errorf("%s: %d attribute reads before the claim decision, want at most 1",
				filepath.Base(p), got)
		}
	}
	if p, n := stat.worst(); n > 1 {
		t.Errorf("worst file was %s at %d reads", filepath.Base(p), n)
	}
}

// ---- AC-8 --------------------------------------------------------------------

// requireRoomFor fails the benchmark BEFORE it builds anything when the filesystem it was
// handed cannot take the library. These suites run on a scratch filesystem of a few
// gigabytes shared by every session on the container, and a benchmark that fills it reds
// unrelated suites with checkout errors that read as code defects rather than as a full
// disk. The budget is deliberately generous per file: the files themselves are empty, and
// what actually accumulates is one ledger row and one directory entry each.
func requireRoomFor(b *testing.B, dir string, files int) {
	b.Helper()
	const perFile = 1024
	need := uint64(files) * perFile
	free, err := diskfree.Bytes(dir)
	if err != nil {
		b.Fatalf("cannot read the free space of %s, so cannot tell whether a %d-file "+
			"library fits: %v", dir, files, err)
	}
	if free < need {
		b.Fatalf("refusing to build a %d-file library under %s: it needs about %d bytes "+
			"and %d are free. This filesystem is shared with every other session on this "+
			"container; filling it fails unrelated suites.", files, dir, need, free)
	}
}

// BenchmarkScan_NoOpPass grades AC-8: what one pass over an already-processed library
// costs, at 10,000 and at 100,000 terminal files, reported per file.
//
// It is not part of `make check` - the `test` target passes no -bench - so the figure is
// taken by hand and recorded in docs/scan-cost.md against the machine, the build and the
// date it was taken on (performance PB5). It fails on nothing; it reports.
//
// Both fixture libraries are built under the benchmark's OWN temporary directory, which the
// testing package removes when it returns, and the room for one is checked before a byte of
// it is written.
func BenchmarkScan_NoOpPass(b *testing.B) {
	for _, files := range []int{10_000, 100_000} {
		b.Run(strconv.Itoa(files), func(b *testing.B) {
			root := b.TempDir()
			requireRoomFor(b, root, files)
			dbDir := b.TempDir()
			sq, err := store.Open(filepath.Join(dbDir, "jobs.db"))
			if err != nil {
				b.Fatalf("store.Open: %v", err)
			}
			b.Cleanup(func() { _ = sq.Close() })
			terminalLibrary(b, sq, root, files)
			eng := noOpEngine(b, root, sq, nil)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := eng.RunOneshot(context.Background()); err != nil {
					b.Fatalf("RunOneshot: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(files), "ns/file")
		})
	}
}

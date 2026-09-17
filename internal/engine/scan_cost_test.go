package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	current store.DecisionInputs, supersedes ...string) (bool, error) {
	c.mu.Lock()
	claimErr := c.claimErr
	c.mu.Unlock()
	if claimErr != nil {
		return false, claimErr
	}
	took, err := c.Store.Claim(ctx, path, fingerprint, worker, maxFailures, current, supersedes...)
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
	by store.Decision, profile string, supersedes ...string) (bool, error) {
	c.record("RecordSkip", path)
	return c.Store.RecordSkip(ctx, path, fingerprint, reason, by, profile, supersedes...)
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

// countingEngine is the pair the cost tests use: a real SQLite store behind a write counter,
// and an engine over root.
//
// It deliberately does NOT install the attribute-read counter. That counter is a seam over
// the pre-claim read, and the read is exactly what the fingerprint cases are about - an
// engine using the seam would key its files off whatever the seam does, so a production read
// that followed the wrong kind of link would be invisible to them. Only the test that
// COUNTS reads installs it; every other test here exercises the real one.
func countingEngine(t *testing.T, root string) (*Engine, *countingStore) {
	t.Helper()
	dbDir := t.TempDir()
	sq, err := store.Open(filepath.Join(dbDir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	cs := &countingStore{Store: sq}
	return noOpEngine(t, root, cs, nil), cs
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
	eng, cs := countingEngine(t, root)
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
	eng, cs := countingEngine(t, root)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot over an empty library: %v", err)
	}
	if got := cs.allWrites(); len(got) != 0 {
		t.Errorf("an empty library cost %d store write(s) about a file: %v", len(got), got)
	}
}

// ---- AC-2, AC-3 --------------------------------------------------------------

// skipReasonAt returns the reason recorded on the SKIPPED row for path, or "" when no
// skipped row stands for it.
func skipReasonAt(t *testing.T, st store.Store, path string) string {
	t.Helper()
	rows, err := st.List(context.Background(), []store.Status{store.Skipped}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r.Outcome.Reason
		}
	}
	return ""
}

// TestMutableGuard_StillClearsWhenTheConditionResolves grades AC-2 (hardlink) and AC-3
// (undo retention): a file parked by a MUTABLE guard whose condition has since resolved is
// CLAIMED on that scan, and no skip row is left standing for it under that guard's reason.
//
// This is the property the deleted clears carried, and it is the one that must not be lost
// with them: a mutable guard's row records a condition that gets fixed, so a row that
// outlives the condition is a file that stops being worked on for ever. Each case seeds the
// row the way the guard itself writes it, then removes the condition and scans.
func TestMutableGuard_StillClearsWhenTheConditionResolves(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, reason string
		seed         func(t *testing.T, st store.Store, path, key string)
	}{
		{
			// The hardlink guard parks through RecordSkip, which records no decision
			// inputs at all.
			name:   "hardlink",
			reason: SkipHardlinked,
			seed: func(t *testing.T, st store.Store, path, key string) {
				if _, err := st.RecordSkip(ctx, path, key, SkipHardlinked, store.Decision{}, ""); err != nil {
					t.Fatalf("seed hardlink skip: %v", err)
				}
			},
		},
		{
			// The undo window parks through a terminal Finish, and the row it leaves
			// records the EMPTY input set - a verdict no configuration change re-opens.
			// Nothing but this clear ever offers that file to the pipeline again, which
			// is what makes this the case that must not be lost.
			name:   "undo_retention",
			reason: SkipUndoRetentionFailed,
			seed: func(t *testing.T, st store.Store, path, key string) {
				took, err := st.Claim(ctx, path, key, "seed", 3, store.InputsRead(nil))
				if err != nil || !took {
					t.Fatalf("seed claim: took=%v err=%v", took, err)
				}
				if err := st.Finish(ctx, path, key, store.Skipped, &store.Outcome{
					Reason: SkipUndoRetentionFailed, DecisionInputs: store.InputsRead(nil),
				}, 3); err != nil {
					t.Fatalf("seed undo-retention skip: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, "movie.mkv")
			if err := os.WriteFile(p, []byte("a source this scan will meet"), 0o644); err != nil {
				t.Fatal(err)
			}
			eng, cs := countingEngine(t, root)
			key := probe.Fingerprint(p)
			tc.seed(t, cs, p, key)
			cs.reset()

			if err := eng.RunOneshot(ctx); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			claimed := false
			for _, got := range cs.claimedPaths() {
				if got == p {
					claimed = true
				}
			}
			if !claimed {
				t.Errorf("a file parked under %q was not claimed once the condition resolved: "+
					"it is parked for ever", tc.reason)
			}
			if got := skipReasonAt(t, cs, p); got == tc.reason {
				t.Errorf("a skip row under %q still stands after the condition resolved", tc.reason)
			}
		})
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
	eng, cs := countingEngine(t, root)
	stat := newCountingStat()
	eng.statFn = stat.stat
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

// ---- AC-5, AC-6 --------------------------------------------------------------

// fingerprintFixture is one file the no-op pass will meet, and the fingerprint text the
// store already holds for it.
//
// want is a LITERAL, and that is the whole point of these cases. It was captured by running
// probe.Fingerprint over exactly these fixtures at the commit BEFORE the pre-claim reads
// were consolidated, and it is what the row is seeded under here. A value recomputed from
// the consolidated read would prove only that the new code agrees with itself, which is
// precisely the regression that re-keys a library: every stored row stops matching, every
// file that was already done is offered back to a pipeline that deletes its source, and no
// re-run undoes the deletion.
//
// Each fixture's size and modification time are SET, so the text is deterministic and the
// literal means something. The values sit on representation boundaries on purpose: the
// epoch, the stat-failure sentinel's own text, the 32-bit signed second and the one after
// it, a time before the epoch, and a sub-second time whose fraction the record truncates.
type fingerprintFixture struct {
	name string
	size int
	sec  int64
	nsec int64
	want string
	// symlinkTo, when set, makes this fixture a symbolic link to a file of that size and
	// time built OUTSIDE the library. Its fingerprint is the TARGET's, because the read
	// follows the link - a link's own size is the length of the path it holds, which is a
	// different number in every temporary directory, so an Lstat here would not even be
	// stable, let alone equal to what the store holds.
	symlinkTo bool
}

var fingerprintFixtures = []fingerprintFixture{
	{name: "plain.mkv", size: 12, sec: 1700000000, want: "12:1700000000"},
	{name: "epoch.mkv", size: 0, sec: 0, want: "0:0"},
	{name: "y2038.mkv", size: 5, sec: 2147483647, want: "5:2147483647"},
	{name: "beyond2038.mkv", size: 6, sec: 2147483648, want: "6:2147483648"},
	{name: "preepoch.mkv", size: 8, sec: -1, want: "8:-1"},
	{name: "subsecond.mkv", size: 9, sec: 1234567890, nsec: 999999999, want: "9:1234567890"},
	{name: "link.mkv", size: 21, sec: 1600000000, want: "21:1600000000", symlinkTo: true},
}

// buildFingerprintFixtures writes the fixtures into root (targets of links go to outside,
// which is NOT a library root, so a link's target is never itself enumerated) and returns
// each fixture's path.
func buildFingerprintFixtures(t *testing.T, root, outside string) map[string]string {
	t.Helper()
	write := func(dir, name string, size int, sec, nsec int64) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		when := time.Unix(sec, nsec)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatalf("set times on %s: %v", p, err)
		}
		return p
	}
	paths := make(map[string]string, len(fingerprintFixtures))
	for _, fx := range fingerprintFixtures {
		if !fx.symlinkTo {
			paths[fx.name] = write(root, fx.name, fx.size, fx.sec, fx.nsec)
			continue
		}
		target := write(outside, "target-of-"+fx.name, fx.size, fx.sec, fx.nsec)
		link := filepath.Join(root, fx.name)
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
		paths[fx.name] = link
	}
	return paths
}

// TestScan_NoOpPassReOpensNoTerminalRow grades AC-6: a no-op pass over a library holding a
// symlinked source and files whose recorded size or time sits at a representation boundary
// derives, for each of them, the fingerprint text the store ALREADY HOLDS - so every
// terminal row stays matched and none is re-opened.
//
// Every row here is seeded under the captured literal, never under a value this build
// derived, so the assertion is against the store as an older build left it.
func TestScan_NoOpPassReOpensNoTerminalRow(t *testing.T) {
	ctx := context.Background()
	root, outside := t.TempDir(), t.TempDir()
	paths := buildFingerprintFixtures(t, root, outside)
	eng, cs := countingEngine(t, root)

	for _, fx := range fingerprintFixtures {
		p := paths[fx.name]
		took, err := cs.Claim(ctx, p, fx.want, "seed", 3, store.InputsRead(nil))
		if err != nil || !took {
			t.Fatalf("seed claim %s at %q: took=%v err=%v", fx.name, fx.want, took, err)
		}
		// The symlinked source is seeded as the skip its own guard records; the rest as
		// done. Both are terminal, and neither records an input a configuration change
		// could move.
		status, out := store.Done, &store.Outcome{DecisionInputs: store.InputsRead(nil)}
		if fx.symlinkTo {
			status = store.Skipped
			out.Reason = SkipSymlink
		}
		if err := cs.Finish(ctx, p, fx.want, status, out, 3); err != nil {
			t.Fatalf("seed finish %s: %v", fx.name, err)
		}
	}
	cs.reset()

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if took := cs.claimedPaths(); len(took) != 0 {
		t.Errorf("the no-op pass re-opened %d terminal row(s): %v - the pass derived a "+
			"fingerprint the store does not hold for those files", len(took), took)
	}
	rows, err := cs.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	held := map[string][]store.Job{}
	for _, r := range rows {
		held[r.Path] = append(held[r.Path], r)
	}
	for _, fx := range fingerprintFixtures {
		p := paths[fx.name]
		got := held[p]
		if len(got) != 1 {
			t.Errorf("%s: the store holds %d rows for it after the pass, want 1 - a second "+
				"row means the pass keyed the file differently and re-offered it", fx.name, len(got))
			continue
		}
		if got[0].Fingerprint != fx.want {
			t.Errorf("%s: row is keyed %q after the pass, want the seeded %q",
				fx.name, got[0].Fingerprint, fx.want)
		}
		if got[0].Status != store.Done && got[0].Status != store.Skipped {
			t.Errorf("%s: row is %q after the pass, want it left terminal", fx.name, got[0].Status)
		}
	}
}

// TestProcessFile_SymlinkedSourceStillSkips grades AC-5: a source file that is itself a
// symbolic link records a skip carrying the symlinked-source reason, and both the link and
// its target are left byte-for-byte unchanged.
//
// The guard it grades runs AFTER the claim and takes its own Lstat, and the consolidation
// may not move it: a read before the claim that did not follow the link would change the key
// of every symlinked source, and one after it that DID follow would swap the link for a
// regular file and orphan the target.
func TestProcessFile_SymlinkedSourceStillSkips(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "target.mkv")
	if err := os.WriteFile(target, []byte("the real file, which the library only points at"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	targetBefore := md5f(t, target)

	eng, cs := countingEngine(t, root)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if got := skipReasonAt(t, cs, link); got != SkipSymlink {
		t.Errorf("skip reason for a symlinked source = %q, want %q", got, SkipSymlink)
	}
	li, err := os.Lstat(link)
	if err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the library entry is no longer a symbolic link: %v (err %v)", li, err)
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Errorf("the link points at %q (err %v), want %q", got, err, target)
	}
	if md5f(t, target) != targetBefore {
		t.Error("the link's target was modified")
	}
}

// ---- AC-10, AC-11 ------------------------------------------------------------

// recordedLine is one log record the engine emitted, reduced to what a criterion about
// records asks: at what level, saying what, about which file, carrying which error.
type recordedLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// lineRecorder collects the engine's log records so a test can ask what the process said
// about a condition it handled. It is a handler and not a parsed buffer, so the level is the
// level rather than a string that happens to start with one.
type lineRecorder struct {
	mu    sync.Mutex
	lines []recordedLine
}

func (r *lineRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *lineRecorder) Handle(_ context.Context, rec slog.Record) error {
	line := recordedLine{level: rec.Level, msg: rec.Message, attrs: map[string]string{}}
	rec.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

func (r *lineRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *lineRecorder) WithGroup(string) slog.Handler      { return r }

// about returns the records naming path in their "file" attribute.
func (r *lineRecorder) about(path string) []recordedLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []recordedLine
	for _, l := range r.lines {
		if l.attrs["file"] == path {
			got = append(got, l)
		}
	}
	return got
}

// TestScan_UnreadableFileIsSkippedAndTheScanContinues grades AC-10: a file whose attributes
// cannot be read - because there is nothing at the other end of the path, or because this
// process may not search the directory holding it - is left alone, gets no row, is RECORDED
// at a level that does not demand a human act (observability O3), and does not stop the scan
// reaching the rest of the library.
//
// Both conditions are produced for real rather than injected. The consolidation collapsed
// four failure sites into one, so this is the branch that carries every one of them.
func TestScan_UnreadableFileIsSkippedAndTheScanContinues(t *testing.T) {
	root := t.TempDir()

	// Nothing at the other end: the ordinary dangling link, and the same read failure a
	// file that went away between the enumeration and now produces.
	dangling := filepath.Join(root, "dangling.mkv")
	if err := os.Symlink(filepath.Join(root, "no-such-target.mkv"), dangling); err != nil {
		t.Fatal(err)
	}

	// Denied: a directory this process may LIST but not SEARCH, so its entries are
	// enumerated and stat of any one of them is refused.
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	denied := filepath.Join(locked, "denied.mkv")
	if err := os.WriteFile(denied, []byte("unreachable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o444); err != nil {
		t.Fatal(err)
	}
	// Registered AFTER the TempDir whose cleanup must follow it, so the directory is
	// searchable again before anything tries to remove its contents.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(denied); err == nil {
		t.Skip("this process can stat inside a directory it has no search permission on " +
			"(running as root?), so the denied half of this criterion cannot be produced")
	}

	// The rest of the library, which the scan must still reach.
	reachable := filepath.Join(root, "reachable.mkv")
	if err := os.WriteFile(reachable, []byte("a source the scan must still meet"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, cs := countingEngine(t, root)
	rec := &lineRecorder{}
	eng.Log = slog.New(rec)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	for _, unreadable := range []string{dangling, denied} {
		if got := cs.writesFor(unreadable); len(got) != 0 {
			t.Errorf("%s: an unreadable file cost %v store write(s)", filepath.Base(unreadable), got)
		}
		lines := rec.about(unreadable)
		if len(lines) == 0 {
			t.Errorf("%s: the scan skipped an unreadable file and recorded nothing about it",
				filepath.Base(unreadable))
			continue
		}
		for _, l := range lines {
			if l.level >= slog.LevelError {
				t.Errorf("%s: recorded at %v, which demands a human act for a condition the "+
					"scan handled itself: %q", filepath.Base(unreadable), l.level, l.msg)
			}
		}
	}
	// Lstat, because the dangling link is exactly a path os.Stat cannot answer for: the
	// question here is whether the entry is still there, not what it points at.
	if _, err := os.Lstat(dangling); err != nil {
		t.Errorf("the scan removed the dangling link it could not read: %v", err)
	}
	if _, err := os.Lstat(locked); err != nil {
		t.Errorf("the scan removed the directory it could not search: %v", err)
	}
	claimed := false
	for _, p := range cs.claimedPaths() {
		if p == reachable {
			claimed = true
		}
	}
	if !claimed {
		t.Error("the scan did not reach the readable file beside the unreadable ones")
	}
}

// TestScan_StoreErrorInTheClaimPathRetriesNextPass grades AC-11: a store that errors while
// the claim decision is being taken leaves the file unmutated, records nothing about it,
// leaves it eligible on the next pass, and says which dependency failed, what it attempted
// and what happens next (observability O4) rather than only carrying a trace.
func TestScan_StoreErrorInTheClaimPathRetriesNextPass(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(p, []byte("a source no failing store may touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := md5f(t, p)

	eng, cs := countingEngine(t, root)
	rec := &lineRecorder{}
	eng.Log = slog.New(rec)
	cs.claimErr = errors.New("database is locked")

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot must survive a store error: %v", err)
	}

	if got := cs.writesFor(p); len(got) != 0 {
		t.Errorf("a file whose claim errored cost %v store write(s)", got)
	}
	if md5f(t, p) != before {
		t.Error("a file whose claim errored was modified")
	}
	rows, err := cs.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == p {
			t.Errorf("a file whose claim errored has a %q row: a store error must never read "+
				"as a verdict about the file", r.Status)
		}
	}
	lines := rec.about(p)
	if len(lines) == 0 {
		t.Fatal("the claim failed and nothing was recorded about the file")
	}
	said := lines[len(lines)-1]
	if said.level >= slog.LevelError {
		t.Errorf("recorded at %v for a condition the scan handled itself: %q", said.level, said.msg)
	}
	if !strings.Contains(said.msg, "store") {
		t.Errorf("the record does not name the dependency that failed: %q", said.msg)
	}
	if !strings.Contains(said.msg, "next scan") {
		t.Errorf("the record does not say what happens to the file next: %q", said.msg)
	}
	if said.attrs["err"] == "" {
		t.Errorf("the record carries no error to say what the dependency answered: %v", said.attrs)
	}

	// Still eligible: the store recovers and the very next pass takes the file.
	cs.mu.Lock()
	cs.claimErr = nil
	cs.mu.Unlock()
	cs.reset()
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot after the store recovered: %v", err)
	}
	claimed := false
	for _, got := range cs.claimedPaths() {
		if got == p {
			claimed = true
		}
	}
	if !claimed {
		t.Error("the file was not claimed on the pass after the store recovered: a store " +
			"error excluded it rather than deferring it")
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

package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/heapmeasure"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

func coverageEngine(t *testing.T, root string, coverage []string) *Engine {
	t.Helper()
	cfg := config.Config{
		LibraryRoots: []string{root},
		VideoExts:    []string{"mkv", "mp4"},
	}
	// A REAL prober (see heldEngine): the sweep asks the verify gate's own questions of
	// a file before it may be removed.
	ffmpeg, ffprobe := tools(t)
	e := New(cfg, probe.New(ffmpeg, ffprobe), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Coverage = coverage
	return e
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEnumerate_TheStartupWalkCoverageBoundsWhatTheScanCanSee. The startup walk
// decides what this run may touch, and the scan may not go behind it: it lists
// exactly the directories that walk traversed successfully, with no recursion of
// its own. A directory the walk declined (an already-walked region, a bind loop),
// could not read, or never reached yields no source at all - which is what stops
// one file being enumerated twice through two spellings, and what makes the scan
// terminate over a mount layout a recursive walk would follow for ever.
func TestEnumerate_TheStartupWalkCoverageBoundsWhatTheScanCanSee(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "Walked.mkv"))
	mustWrite(t, filepath.Join(root, "declined", "Declined.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "deeper", "Deeper.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "notes.txt"))
	mustWrite(t, filepath.Join(root, "walked", "Half.__transcoding__.mkv"))

	e := coverageEngine(t, root, []string{root, filepath.Join(root, "walked")})
	got, observed := e.enumerate()
	want := []string{
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "walked", "Walked.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
	}
	// The observed set is the same bound in its other form, and the retention pass
	// depends on it being exactly this: a directory holdfast did NOT list is one it
	// may draw no conclusion from, however absent a file under it looks.
	wantObserved := map[string]bool{root: true, filepath.Join(root, "walked"): true}
	if !reflect.DeepEqual(observed, wantObserved) {
		t.Fatalf("enumerate() observed %v, want exactly %v", observed, wantObserved)
	}

	// Without a coverage set (an Engine built with no startup check) the old
	// recursive behaviour is unchanged.
	plain := coverageEngine(t, root, nil)
	plainFiles, plainObserved := plain.enumerate()
	if n := len(plainFiles); n != 4 {
		t.Fatalf("unbounded enumerate() found %d sources, want the 4 in the tree", n)
	}
	for _, dir := range []string{root, filepath.Join(root, "walked"), filepath.Join(root, "declined"),
		filepath.Join(root, "walked", "deeper")} {
		if !plainObserved[dir] {
			t.Errorf("the unbounded walk listed %s and did not report observing it", dir)
		}
	}

	// And an EMPTY bound is not an absent one. nil means "no startup check
	// ran"; a non-nil empty slice means "that check traversed nothing, so this
	// run may enumerate nothing". Confusing the two would un-bound the scan at
	// exactly the moment the bound matters most, which is why the startup check
	// never returns a nil Coverage (see startup.Result.Coverage).
	if got, obs := coverageEngine(t, root, []string{}).enumerate(); len(got) != 0 || len(obs) != 0 {
		t.Fatalf("an empty coverage set enumerated %v and observed %v, want nothing at all", got, obs)
	}
}

// TestEnumerate_CoverageThatNoLongerExistsIsSkipped: the walk ran before the
// scan, and the world can move underneath it. A directory that has since gone is
// skipped, never a crash and never an abort of the whole scan.
func TestEnumerate_CoverageThatNoLongerExistsIsSkipped(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	e := coverageEngine(t, root, []string{root, filepath.Join(root, "gone")})
	got, observed := e.enumerate()
	if want := []string{filepath.Join(root, "Top.mkv")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
	}
	// Skipped, and NOT reported as observed. A directory that has gone since the walk is
	// one this run has no evidence about - which is what stops the retention pass reading
	// an unmounted subtree as a library the operator deleted.
	if observed[filepath.Join(root, "gone")] {
		t.Fatal("a coverage entry that no longer exists was reported as a directory this run listed")
	}
	if !observed[root] {
		t.Fatal("the root was listed and not reported as observed")
	}
}

// TestCleanStaleTemps_IsBoundedByTheSameCoverage: the temp sweep mutates files
// under a library root, so it obeys the same bound. A directory the startup walk
// did not traverse is one this run touches in no way at all.
//
// Each temp is staged with its SOURCE beside it, which is what a killed run actually
// leaves behind (tempPath puts the temp in the source's own directory under the
// source's own stem). It is load-bearing rather than decoration: strayReplacementHold
// asks whether there is anything beside the file to measure it against BEFORE it asks
// anything that can fail, and a temp with no source beside it is held whatever else is
// true - so a fixture without one would be testing the hold, not the coverage bound.
func TestCleanStaleTemps_IsBoundedByTheSameCoverage(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "walked", "Film.__transcoding__.mkv")
	outside := filepath.Join(root, "declined", "Film.__transcoding__.mkv")
	mustWrite(t, inside)
	mustWrite(t, outside)
	mustWrite(t, filepath.Join(root, "walked", "Film.mkv"))
	mustWrite(t, filepath.Join(root, "declined", "Film.mkv"))

	e := coverageEngine(t, root, []string{root, filepath.Join(root, "walked")})
	e.cleanStaleTemps(context.Background())

	if _, err := os.Stat(inside); err == nil {
		t.Fatal("a temp under a traversed directory survived the sweep")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a temp under a directory the walk did not traverse was removed: %v", err)
	}
}

// TestIsSourceName is the ONE definition of a media file this run would
// enumerate, shared by the scan and the startup walk, and it decides from the
// name alone - no file is opened to answer it.
func TestIsSourceName(t *testing.T) {
	exts := []string{"mkv", "mp4"}
	tests := []struct {
		base string
		want bool
	}{
		{"Film.mkv", true},
		{"Film.MKV", true},
		{"Film.mp4", true},
		{"notes.txt", false},
		{"Film", false},
		{"Film." + TempMarker + ".mkv", false},
	}
	for _, tc := range tests {
		if got := IsSourceName(tc.base, exts); got != tc.want {
			t.Fatalf("IsSourceName(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

// What one PASS over the library costs, and what it enumerates from what the
// startup walk already read (FILESYSTEM-1).
//
// The walk lists every directory beneath the roots to classify the storage. The
// sweep then listed each of them again to find this tool's own orphaned temps,
// and the enumeration a third time to find sources - three listings of the same
// directory for one `run`, every entry name identical in all three. The cases
// below assert what a pass costs, counted rather than assumed, and that reading
// the walk's own listings enumerates the same sources a re-listing does.

// listingCounter counts every directory listing a RUN makes, wherever it is
// made. The walk reads the host through a startup.Platform and the scan reads it
// through the engine's own seam, so a criterion about what a pass costs is only
// assertable if one counter sees both.
type listingCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func newListingCounter() *listingCounter { return &listingCounter{n: map[string]int{}} }

func (c *listingCounter) count(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[filepath.Clean(dir)]++
}

func (c *listingCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.n))
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

// engineSeam is the substituted filesystem the scan lists through. It answers
// from the real host, so what is counted is the production answer and not a
// fixture's idea of one.
func (c *listingCounter) engineSeam() func(string) ([]os.DirEntry, error) {
	return func(dir string) ([]os.DirEntry, error) {
		c.count(dir)
		return os.ReadDir(dir)
	}
}

type countingPlatform struct {
	startup.Platform
	c *listingCounter
}

func (p countingPlatform) ReadDir(dir string) ([]startup.Entry, error) {
	p.c.count(dir)
	return p.Platform.ReadDir(dir)
}

// walkOver runs the REAL startup check over root, counting its listings. Only
// the filesystem-type lookup is substituted, and for the reason that seam
// exists: the gate has no second real filesystem, and these cases are about
// listings and enumeration rather than about classification.
func walkOver(t *testing.T, root string, exts []string, c *listingCounter) startup.Result {
	t.Helper()
	res := startup.Run(startup.Check{
		Roots:       []string{root},
		StateDir:    t.TempDir(),
		IsMediaFile: func(base string) bool { return IsSourceName(base, exts) },
		Platform: countingPlatform{
			Platform: startup.System(func(string) (string, error) { return "ext4", nil }, nil),
			c:        c,
		},
	})
	if !res.Start {
		t.Fatalf("the startup check refused the fixture at row %d: %+v", res.Row, res.Causes)
	}
	return res
}

// TestScan_ListsEachDirectoryOnce is the cost, counted. A `run` over a library
// the walk listed without failure issues exactly one listing per directory the
// walk attempted, for the whole process - the walk's, which the sweep and the
// enumeration read instead of making their own.
//
// MEASURED, not predicted: against the build this replaces the same fixture
// costs THREE listings of every covered directory on the first scan (the walk,
// the stale-temp sweep, the enumeration) and TWO on every scan after it.
//
// MUTATION (measured): have SetCoverage drop the entries it is given AND have
// the sweep list for itself instead of reading this pass's listings, and this
// reds at exactly those numbers - 3 per directory for the first scan, 2 for the
// second.
func TestScan_ListsEachDirectoryOnce(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	// Already at the target codec, so the pass has real work to decide and no
	// encode to pay for; the temp is staged with its source beside it, which is
	// what a killed run leaves and what the sweep is entitled to reclaim.
	for _, dir := range []string{"season", filepath.Join("season", "extras"), "empty"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkHevc(t, ffmpeg, filepath.Join(root, "Top.mkv"), "800k")
	mkHevc(t, ffmpeg, filepath.Join(root, "season", "Ep1.mkv"), "800k")
	mkHevc(t, ffmpeg, filepath.Join(root, "season", "extras", "Behind.mkv"), "800k")
	mustWrite(t, filepath.Join(root, "season", "notes.txt"))
	mustWrite(t, filepath.Join(root, "season", "Ep1."+TempMarker+".mkv"))

	counts := newListingCounter()
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	res := walkOver(t, root, eng.Cfg.VideoExts, counts)
	if len(res.Coverage) != 4 {
		t.Fatalf("the walk covered %v, want the four directories of the fixture", res.Coverage)
	}
	eng.readDirFn = counts.engineSeam()
	eng.SetCoverage(res.Coverage, res.Entries)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	first := counts.snapshot()
	for _, dir := range res.Coverage {
		if first[dir] != 1 {
			t.Errorf("%s was listed %d times by one run, want exactly 1: the walk read it, and the sweep "+
				"and the enumeration read what the walk read", dir, first[dir])
		}
	}
	for dir, n := range first {
		if !contains(res.Coverage, dir) {
			t.Errorf("%s was listed %d times and is not a directory the walk covered", dir, n)
		}
	}

	// A second scan in the same process. The walk's listings were consumed by the
	// first scan and released, so this one lists for itself - and still exactly
	// once per directory, across the sweep and the enumeration together.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	second := counts.snapshot()
	for _, dir := range res.Coverage {
		if got := second[dir] - first[dir]; got != 1 {
			t.Errorf("the second scan listed %s %d times, want exactly 1 for the sweep and the enumeration "+
				"together", dir, got)
		}
	}
}

// asThePinnedBuildDid enumerates the way the build before this one did: it lists
// each covered directory, takes every entry the LISTING calls a non-directory
// whose name is a source name, and applies the same record-based hold-back. It
// is here so the one declared departure is demonstrated rather than asserted
// about - a symbolic link to a directory is a non-directory to every listing,
// whatever it points at.
func asThePinnedBuildDid(e *Engine) []string {
	var out []string
	for _, dir := range e.Coverage {
		if filepath.Base(dir) == UndoDirName {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() {
				continue
			}
			if IsSourceName(ent.Name(), e.Cfg.VideoExts) {
				if p := filepath.Join(dir, ent.Name()); e.offered(p) {
					out = append(out, p)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func without(paths []string, drop string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != drop {
			out = append(out, p)
		}
	}
	return out
}

// offerFixture is one library holding every entry kind that decides something
// about what a scan may act on: nested subdirectories, a non-media file, a
// work-in-progress temp, a retained replacement, a file a parked record holds
// back, a symbolic link to a media file whose target is under the root, one
// whose target is under NO root, and one whose name is a source name and whose
// target is a DIRECTORY. It returns the parked path and the directory link.
func offerFixture(t *testing.T, root string, ts *testStore) (parked, dirLink string) {
	t.Helper()
	external := t.TempDir()
	for _, p := range []string{
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "notes.txt"),
		filepath.Join(root, "Half."+TempMarker+".mkv"),
		filepath.Join(root, "Kept."+RetainedMarker+".mkv"),
		filepath.Join(root, "Parked.mkv"),
		filepath.Join(root, "season", "Ep1.mkv"),
		filepath.Join(root, "season", "deeper", "Ep2.mp4"),
		filepath.Join(external, "Outside.mkv"),
	} {
		mustWrite(t, p)
	}
	for _, l := range []struct{ target, link string }{
		{filepath.Join(root, "season", "Ep1.mkv"), filepath.Join(root, "Inside.mkv")},
		{filepath.Join(external, "Outside.mkv"), filepath.Join(root, "Outside.mkv")},
		{external, filepath.Join(root, "Collection.mkv")},
	} {
		if err := os.Symlink(l.target, l.link); err != nil {
			t.Fatalf("symlink %s: %v", l.link, err)
		}
	}
	parked = filepath.Join(root, "Parked.mkv")
	if err := ts.RecordSwapIncident(context.Background(), store.SwapIncident{
		SourcePath: parked, SourceFingerprint: "1:1",
		ReplacementPath: filepath.Join(root, "Parked."+TempMarker+".mkv"),
		SourceAttrs:     "1:1", ReplacementAttrs: "2:2", Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record incident: %v", err)
	}
	return parked, filepath.Join(root, "Collection.mkv")
}

// TestEnumerate_TheCarriedEntriesOfferExactlyWhatARelistingOffers. The whole
// change is that the scan reads what the walk read instead of reading it again,
// so the two must decide identically over a library holding every entry kind
// that decides anything - and they do, apart from the single case where the two
// disagreed all along: a listing reports a symbolic link as a non-directory
// whatever it points at, so the build this replaces offered a DIRECTORY to a
// pipeline whose every step is about a file.
func TestEnumerate_TheCarriedEntriesOfferExactlyWhatARelistingOffers(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	parked, dirLink := offerFixture(t, root, ts)

	counts := newListingCounter()
	exts := []string{"mkv", "mp4"}
	res := walkOver(t, root, exts, counts)

	carried := heldEngine(t, root, ts, nil)
	carried.SetCoverage(res.Coverage, res.Entries)
	fromWalk, observedFromWalk := carried.enumerate()

	// The same coverage set with no entry information: this one lists for itself,
	// which is the algorithm the pinned build ran.
	relisting := heldEngine(t, root, ts, res.Coverage)
	fromListing, observedFromListing := relisting.enumerate()

	if !reflect.DeepEqual(fromWalk, fromListing) {
		t.Fatalf("the walk's entries enumerate\n%v\nand re-listing the same directories enumerates\n%v", fromWalk, fromListing)
	}
	if !reflect.DeepEqual(observedFromWalk, observedFromListing) {
		t.Fatalf("the two routes observed %v and %v", observedFromWalk, observedFromListing)
	}
	want := []string{
		filepath.Join(root, "Inside.mkv"),
		filepath.Join(root, "Outside.mkv"),
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "season", "Ep1.mkv"),
		filepath.Join(root, "season", "deeper", "Ep2.mp4"),
	}
	if !reflect.DeepEqual(fromWalk, want) {
		t.Fatalf("enumerate() = %v, want %v", fromWalk, want)
	}
	if contains(fromWalk, parked) {
		t.Errorf("the parked job's source was offered: %v", fromWalk)
	}

	// The departure, both halves of it. The pinned algorithm over the same
	// coverage offers the directory link, so the fixture really does exercise it;
	// and the enumeration under test differs from it by that path and no other.
	pinned := asThePinnedBuildDid(relisting)
	if !contains(pinned, dirLink) {
		t.Fatalf("the fixture does not exercise the symlink-to-directory case: the pinned algorithm offered %v", pinned)
	}
	if !reflect.DeepEqual(fromWalk, without(pinned, dirLink)) {
		t.Fatalf("the enumeration differs from the pinned one by more than the one declared case:\ngot    %v\npinned %v", fromWalk, pinned)
	}
}

// TestEnumerate_ASourceNameOnADirectoryIsNotOfferedByEitherBranch is the same
// rule where the engine has no startup walk to tell it: the recursive fallback
// does not follow links either, so it has to ask, and it must answer the way the
// coverage branch does. A pipeline handed a directory can only fail on it, once
// per pass, for ever.
func TestEnumerate_ASourceNameOnADirectoryIsNotOfferedByEitherBranch(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	_, dirLink := offerFixture(t, root, ts)

	var log bytes.Buffer
	e := heldEngine(t, root, ts, nil) // no coverage at all: the recursive fallback
	e.Log = captureLogger(&log)
	got, _ := e.enumerate()

	if contains(got, dirLink) {
		t.Errorf("the recursive fallback offered a directory under a source name: %v", got)
	}
	if !strings.Contains(log.String(), "a source name on a directory") || !strings.Contains(log.String(), dirLink) {
		t.Errorf("the fallback skipped %s and did not say why:\n%s", dirLink, log.String())
	}
	// Everything else in the same tree is still offered, so the rule costs one
	// path and never narrows the run. The fallback recurses, so it reaches the
	// same five sources the bounded branch does.
	for _, want := range []string{
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "Inside.mkv"),
		filepath.Join(root, "Outside.mkv"),
		filepath.Join(root, "season", "Ep1.mkv"),
		filepath.Join(root, "season", "deeper", "Ep2.mp4"),
	} {
		if !contains(got, want) {
			t.Errorf("%s was withheld by the fallback: %v", want, got)
		}
	}

	// And the coverage branch says the same thing in the same words.
	counts := newListingCounter()
	res := walkOver(t, root, e.Cfg.VideoExts, counts)
	var bounded bytes.Buffer
	c := heldEngine(t, root, ts, nil)
	c.Log = captureLogger(&bounded)
	c.SetCoverage(res.Coverage, res.Entries)
	if got, _ := c.enumerate(); contains(got, dirLink) {
		t.Errorf("the coverage branch offered a directory under a source name: %v", got)
	}
	if !strings.Contains(bounded.String(), "a source name on a directory") || !strings.Contains(bounded.String(), dirLink) {
		t.Errorf("the coverage branch skipped %s and did not say why:\n%s", dirLink, bounded.String())
	}
}

// TestScan_ADirectoryTheWalkCouldNotListYieldsNothingAndIsNotListedAgain. A
// listing that did not return is not evidence, and it is not an invitation to
// try again either: the walk has already had that answer, so the pass carries it
// rather than paying for a second listing and drawing a different conclusion
// half a second later.
func TestScan_ADirectoryTheWalkCouldNotListYieldsNothingAndIsNotListedAgain(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	denied := filepath.Join(root, "denied")
	hidden := filepath.Join(denied, "Hidden.mkv")
	mustWrite(t, hidden)
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	counts := newListingCounter()
	res := walkOver(t, root, []string{"mkv", "mp4"}, counts)
	if contains(res.Coverage, denied) {
		t.Fatal("the fixture did not deny the listing; the case asserts nothing")
	}
	if n := counts.snapshot()[denied]; n != 1 {
		t.Fatalf("the walk attempted %d listings of the denied directory, want the one that failed", n)
	}

	e := heldEngine(t, root, ts, nil)
	e.readDirFn = counts.engineSeam()
	e.SetCoverage(res.Coverage, res.Entries)
	files, observed := e.enumerate()

	if contains(files, hidden) {
		t.Errorf("a source was enumerated from a directory whose listing failed: %v", files)
	}
	if observed[denied] {
		t.Error("a directory whose listing failed was recorded as one this run listed; the retention pass " +
			"reads that set as proof of where holdfast actually looked")
	}
	if n := counts.snapshot()[denied]; n != 1 {
		t.Errorf("the scan listed the denied directory again (%d listings in the run); a listing that failed "+
			"during the walk is an answer this pass already has", n)
	}
}

// TestScan_ADeclinedRegionYieldsNothing. The region rule is what makes the walk
// terminate over a link or bind loop, and the entries must not go behind it: a
// path exposing storage the walk already entered is listed by nobody, yields no
// source, and is no evidence about anything - its files are enumerated exactly
// once, under the spelling the walk reached first.
func TestScan_ADeclinedRegionYieldsNothing(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	mustWrite(t, filepath.Join(root, "season", "Ep1.mkv"))
	back := filepath.Join(root, "season", "back")
	if err := os.Symlink(root, back); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	counts := newListingCounter()
	res := walkOver(t, root, []string{"mkv", "mp4"}, counts)
	if contains(res.Coverage, back) {
		t.Fatalf("the fixture did not exercise the region rule; coverage %v", res.Coverage)
	}
	if _, ok := res.Entries[back]; ok {
		t.Error("the walk carried entries for a path it declined to enter")
	}

	e := heldEngine(t, root, ts, nil)
	e.readDirFn = counts.engineSeam()
	e.SetCoverage(res.Coverage, res.Entries)
	files, observed := e.enumerate()

	want := []string{filepath.Join(root, "Top.mkv"), filepath.Join(root, "season", "Ep1.mkv")}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("enumerate() = %v, want %v: one entry per file, under the spelling the walk reached first", files, want)
	}
	if observed[back] {
		t.Error("a declined path was recorded as one this run listed")
	}
	if n := counts.snapshot()[back]; n != 0 {
		t.Errorf("the declined path was listed %d times; the walk declined to enter it and nothing else may", n)
	}
}

// TestScan_AFileThatAppearsAfterTheWalkIsEnumeratedByTheNextScan is the
// freshness rule, stated as it behaves. The walk's listings are the picture the
// FIRST scan after that walk acts on, and they are released as that scan
// consumes them - so every later scan lists for itself and a file that appeared
// in the meantime is enumerated by the next scan to begin, which is what keeps
// `serve` (one walk at process start, a scan on every tick) honest.
func TestScan_AFileThatAppearsAfterTheWalkIsEnumeratedByTheNextScan(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	mustWrite(t, filepath.Join(root, "Top.mkv"))

	counts := newListingCounter()
	res := walkOver(t, root, []string{"mkv", "mp4"}, counts)

	e := heldEngine(t, root, ts, nil)
	e.readDirFn = counts.engineSeam()
	e.SetCoverage(res.Coverage, res.Entries)

	// It appears after the walk has been and gone.
	appeared := filepath.Join(root, "Appeared.mkv")
	mustWrite(t, appeared)

	first, _ := e.enumerate()
	if !reflect.DeepEqual(first, []string{filepath.Join(root, "Top.mkv")}) {
		t.Fatalf("the first scan enumerated %v, want what the walk that fed it saw", first)
	}
	second, observed := e.enumerate()
	if !contains(second, appeared) {
		t.Errorf("the next scan enumerated %v; a file that appeared after the walk must be picked up by the "+
			"next scan to begin, or a `serve` that walks once at start would never see it", second)
	}
	if !observed[root] {
		t.Error("the scan that listed for itself did not record the root as observed")
	}
}

// TestEnumerate_ACoverageSetWithNoEntryInformationListsForItselfAndSaysSo. An
// embedder may set the bound without the listings - and every scan after the
// first is in exactly that position. It lists those directories itself, draws
// its observed set from those listings, enumerates what a re-listing
// enumerates, and says that is what it did: entry information that was never
// collected is never evidence.
func TestEnumerate_ACoverageSetWithNoEntryInformationListsForItselfAndSaysSo(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	mustWrite(t, filepath.Join(root, "season", "Ep1.mkv"))
	gone := filepath.Join(root, "gone")

	var log bytes.Buffer
	e := heldEngine(t, root, ts, []string{root, filepath.Join(root, "season"), gone})
	e.Log = captureLogger(&log)
	files, observed := e.enumerate()

	want := []string{filepath.Join(root, "Top.mkv"), filepath.Join(root, "season", "Ep1.mkv")}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("enumerate() = %v, want %v", files, want)
	}
	wantObserved := map[string]bool{root: true, filepath.Join(root, "season"): true}
	if !reflect.DeepEqual(observed, wantObserved) {
		t.Fatalf("observed %v, want exactly %v: a directory that no longer exists is one this run has no "+
			"evidence about", observed, wantObserved)
	}
	if !strings.Contains(log.String(), "this scan listed covered directories itself") {
		t.Errorf("the scan listed for itself and did not say so:\n%s", log.String())
	}
}

// TestScan_ObservedIsTheListingThisScanDrewItsSourcesFrom. The observed set is
// what the retention pass reads as "holdfast looked here", and this change moves
// where the looking happened - so its meaning is asserted directly. A covered
// directory that listed EMPTY is in it, because holdfast looked and found
// nothing; the retention area is not, because the scan deliberately never opens
// it.
func TestScan_ObservedIsTheListingThisScanDrewItsSourcesFrom(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	undo := undoDirFor(root)
	mustWrite(t, filepath.Join(undo, "held."+UndoMarker+".mkv"))

	counts := newListingCounter()
	res := walkOver(t, root, []string{"mkv", "mp4"}, counts)
	if !contains(res.Coverage, undo) || !contains(res.Coverage, empty) {
		t.Fatalf("the walk covered %v, want both the empty directory and the retention area", res.Coverage)
	}
	if ents, ok := res.Entries[empty]; !ok || len(ents) != 0 {
		t.Fatalf("the walk carried entries=%v present=%v for the empty directory", ents, ok)
	}

	e := heldEngine(t, root, ts, nil)
	e.SetCoverage(res.Coverage, res.Entries)
	files, observed := e.enumerate()

	if !reflect.DeepEqual(files, []string{filepath.Join(root, "Top.mkv")}) {
		t.Fatalf("enumerate() = %v, want the one source outside the retention area", files)
	}
	if !observed[empty] {
		t.Error("a covered directory whose listing returned NO entries is absent from the observed set; " +
			"holdfast looked there and found nothing, which is evidence and is not the same as never looking")
	}
	if observed[undo] {
		t.Error("the retention area was recorded as listed; the scan skips it before it reads anything, and " +
			"a directory it never opened is no evidence about what is in it")
	}
	if !observed[root] {
		t.Error("the root was listed by the walk and is absent from the observed set")
	}

	// And the consequence end to end, on the production path: a row for a file
	// that would live in the empty directory IS spent (holdfast listed it and the
	// file was not there), while a row inside the retention area is not.
	e2 := buildEngine(t, "", "", root, nil, func(c *config.Config) { c.HistoryRetentionRows = 1 })
	rows := e2.Store.(*testStore)
	ghost := filepath.Join(undo, "vanished."+UndoMarker+".mkv")
	seedRow(t, rows, ghost, store.Done, nil)
	for i := 0; i < 5; i++ {
		seedRow(t, rows, filepath.Join(empty, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	}
	res2 := walkOver(t, root, e2.Cfg.VideoExts, counts)
	e2.SetCoverage(res2.Coverage, res2.Entries)
	e2.Paused = func() bool { return true } // no encoder here: this half is about the rows
	if err := e2.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	var ghostKept, emptyPruned bool
	emptyPruned = true
	for _, row := range terminalRows(t, rows) {
		if row.Path == ghost {
			ghostKept = true
		}
		if filepath.Dir(row.Path) == empty {
			emptyPruned = false
		}
	}
	if !ghostKept {
		t.Error("the retention pass removed a row inside the retention area on the strength of an absence " +
			"inside a directory this run never listed")
	}
	if !emptyPruned {
		t.Error("rows for files absent from a directory this run LISTED and found empty were kept; an empty " +
			"listing is evidence, and a retention that ignored it would never bound the ledger")
	}
}

// --- what a scan RETAINS ---------------------------------------------------
//
// Carrying the walk's listings means the entry names of a whole library are in
// memory when a scan starts, where before each directory's names were read and
// dropped one directory at a time. That is the cost of not reading them three
// times, and it is bounded here rather than asserted about.
//
// Every figure below is a reading THIS run took, through internal/heapmeasure,
// and it is only ever compared with another reading the same run took in the
// same process: live heap after two forced collections, minus a baseline read
// before the engine existed. Nothing here is compared with a byte count written
// down anywhere else, because that form was tried here and it did not grade the
// code at all.
//
// WHAT AN ABSOLUTE CEILING ACTUALLY GRADED, measured: the same unchanged scan,
// on one machine, in one afternoon, measured 528896 bytes at the peak instant
// and 239616 bytes at the same instant with nothing changed but the LENGTH OF
// THE TEMPORARY DIRECTORY the fixture was built in. The peak holds one path
// string per source, so 2000 sources under a 132-character temp path cost a
// quarter of a megabyte more than the same 2000 under a 4-character one. The
// pinned ceiling of 497328 therefore reported a regression in the first
// environment and clean in the second, for identical code - and a permanently
// red assertion reds identically for a clean tree and for a real regression,
// which is the expensive half. Live heap at an instant also moves with the
// hardware, the allocator's arena layout, the race detector's shadow state, the
// coverage counters `make check` runs with, and with whatever the test binary
// has already run: the same fixture measured 399 KiB and 464 KiB at the same
// instant in two different positions.
//
// A DIFFERENCE between two readings of one process survives all of that. Across
// that same 289 KiB swing in the absolute figure, the pair below stayed within
// 4672 bytes of each other and the deliberate regression stayed at 82 KiB.

// retainedHeapMargin is the ONLY allowance either comparison below makes, and
// what it absorbs is measurement NOISE and nothing else. The readings of a pair
// are taken at different points in one process, and between them the allocator
// may have kept a fresh span, a background goroutine may be holding a buffer,
// and the race detector's and the coverage counters' own bookkeeping moves with
// whatever has already run. That is the only thing a pair may legitimately
// differ by, because everything else about the two readings - hardware, Go
// build, instrumentation, position - is held identical by construction.
//
// THE NOISE IT ABSORBS, MEASURED, not estimated and not raised until a run went
// green: 32 KiB is the figure this file already carried, twice, before either
// comparison became a comparison. The warmed peak pair has differed by 240,
// 336, 496, 528 and -4672 bytes with no change to the code under test, and the
// retained-after-return reading by -12016, -5552, -656, -528, -296, -72, 104,
// 328, 744 and 4624 bytes across the same pair of fixtures. 32 KiB is several
// times the largest of those, and it is a third of what the property it guards
// costs: a second, redundant copy of one library's carried entries measures ~82
// KiB over these fixtures (82072, 83304, 84184, 84248 and 84344 bytes, across
// both temp-path lengths), which is why
// TestScan_PeakRetainedHeapRedsOnADeliberateRegression reds straight through
// this margin instead of being swallowed by it. It is FIXED and never a function
// of the source count: a margin that grew with N would pass the very build these
// cases exist to catch.
const retainedHeapMargin = 32_768 // 32 KiB

// entryCarriage is what a measured scan does with the entry information its
// startup walk collected. The walk itself is IDENTICAL in every case - same
// roots, same platform, same listings, same cost - so a pair of readings taken
// across two of these differs by the CARRYING and by nothing else.
type entryCarriage int

const (
	// carryEntries is production (FILESYSTEM-1): the walk's listings are handed
	// to the engine, and the first scan after that walk reads them instead of
	// listing every covered directory a second time.
	carryEntries entryCarriage = iota

	// dropEntries is the same walk with its entries NOT carried - the coverage
	// set alone. It is the build the carrying replaced and it is also what every
	// scan after the first one already gets, since passListings leaves none
	// behind; it is the reading the carried one is compared with.
	dropEntries

	// carryEntriesTwice is the REGRESSION, and nothing in the engine does it:
	// the walk's entries carried, plus a second, redundant copy of the same
	// entries held alive beside them across the whole measurement. It exists so
	// that the comparison can be WATCHED going red.
	carryEntriesTwice
)

// duplicateEntries is the regression itself: a second, redundant copy of the
// entry information a walk collected, names and all. strings.Clone is
// load-bearing - a shallow copy of each slice would share every name with the
// original and duplicate only the entry headers, which is a fraction of what
// holding a library's entries twice actually costs, and the case would then be
// grading a smaller regression than the one it claims to drive.
func duplicateEntries(walked map[string][]startup.Entry) map[string][]startup.Entry {
	dup := make(map[string][]startup.Entry, len(walked))
	for dir, ents := range walked {
		copies := make([]startup.Entry, len(ents))
		for i, ent := range ents {
			ent.Name = strings.Clone(ent.Name)
			copies[i] = ent
		}
		dup[strings.Clone(dir)] = copies
	}
	return dup
}

// synthLibrary writes perDir sources into each of dirs directories, with a
// non-media file beside them so a listing is not all sources.
func synthLibrary(t *testing.T, root string, dirs, perDir int) {
	t.Helper()
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, "d"+strconv.Itoa(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perDir; i++ {
			if err := os.WriteFile(filepath.Join(dir, "src"+strconv.Itoa(i)+".mkv"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// scanEngine builds the engine a `run` builds: a real store, the real startup
// walk over the same roots, and a coverage set carrying the listings that walk
// made - or not carrying them, per carry. The walk's own result stays inside
// this function, so a measurement over the engine it returns is about what the
// ENGINE retains and not about what a caller happens to be holding.
//
// The second return is whatever the carriage asks to be held alive BESIDE the
// engine. It is nil for both production carriages; only the regression has one,
// and its caller keeps it reachable until every reading has been taken.
func scanEngine(t *testing.T, root string, carry entryCarriage) (*Engine, map[string][]startup.Entry) {
	t.Helper()
	cfg := baseCfg(root)
	eng := New(cfg, probe.New("", ""), nil, newTestStore(t, root), discardLogger())
	res := walkOver(t, root, cfg.VideoExts, newListingCounter())

	// Taken BEFORE SetCoverage, which empties the walk's map by design.
	var redundant map[string][]startup.Entry
	if carry == carryEntriesTwice {
		redundant = duplicateEntries(res.Entries)
	}
	if carry == dropEntries {
		eng.SetCoverage(res.Coverage, nil)
		return eng, nil
	}
	eng.SetCoverage(res.Coverage, res.Entries)
	return eng, redundant
}

// scanRetention runs one scan over a synthetic library and reports what the run
// retains at two instants: at the point inside it where it holds its complete
// enumeration, and once it has returned. Both readings are taken against ONE
// baseline, so they are comparable with each other as well as with it. Nothing
// is fed to a worker - the pass is paused before the first file - so what is
// measured is the scan's own machinery and never an encode's.
//
// A reading it could not take is a FAILURE naming what could not be measured,
// never a zero: see internal/heapmeasure, which refuses rather than defaulting.
func scanRetention(t *testing.T, dirs, perDir int, carry entryCarriage) (afterScan, atEnumeration int64) {
	t.Helper()
	root := t.TempDir()
	synthLibrary(t, root, dirs, perDir)

	base := heapmeasure.Retained()
	peak := heapmeasure.From("the heap a scan holds at the instant it has its complete enumeration", base)
	left := heapmeasure.From("the heap a scan still holds once it has returned", base)

	eng, redundant := scanEngine(t, root, carry)
	eng.Paused = func() bool {
		// The feed loop asks this before it hands out the first file, so it is
		// called with the whole enumeration in hand: every source path this pass
		// will act on, and whatever the pass still holds of the listings it drew
		// them from.
		peak.Sample()
		return true
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	left.Sample()
	runtime.KeepAlive(eng)
	// Held until both readings are taken, which is what makes a redundant copy
	// part of what this scan RETAINED rather than garbage by the time it counts.
	runtime.KeepAlive(redundant)

	atEnumeration, err := peak.Peak()
	if err != nil {
		t.Fatal(err)
	}
	afterScan, err = left.Delta()
	if err != nil {
		t.Fatal(err)
	}
	return afterScan, atEnumeration
}

// peakRetainedPair takes BOTH readings of the peak-retained comparison, in this
// process, in this run and under this binary's instrumentation - `make check`
// runs the suite with the race detector and atomic coverage counters, both of
// which perturb heap accounting, so two readings are comparable only when the
// same instrumented binary took them both. carry is the carriage of the CARRIED
// side; the other side is always the same walk with its entries dropped.
//
// One unmeasured scan runs first. A scan's first pass through a test binary
// allocates arenas, buffers and per-goroutine state every later pass reuses, so
// an unwarmed first reading is taken over a colder heap than the second - the
// same fixture in this package has measured 399 KiB and 464 KiB at the same
// instant in two different positions. Warming is what makes the pair differ by
// the carrying rather than by its position in the run.
func peakRetainedPair(t *testing.T, dirs, perDir int, carry entryCarriage) (carried, notCarried int64) {
	t.Helper()
	scanRetention(t, dirs, perDir, dropEntries) // warm-up, deliberately not read

	_, notCarried = scanRetention(t, dirs, perDir, dropEntries)
	_, carried = scanRetention(t, dirs, perDir, carry)

	t.Logf("peak retained heap, the walk's entries CARRIED: %d bytes over %d sources", carried, dirs*perDir)
	t.Logf("peak retained heap, the SAME walk's entries NOT carried: %d bytes over %d sources",
		notCarried, dirs*perDir)
	t.Logf("margin allowed between the two readings: %d bytes", retainedHeapMargin)
	return carried, notCarried
}

// peakRetainedFailure is the comparison itself, and it RETURNS the failure it
// would report instead of reporting it. That is what lets
// TestScan_PeakRetainedHeapRedsOnADeliberateRegression assert this comparison
// still goes red when the property is broken on purpose: a grader nobody has
// watched fail is not evidence that it can.
func peakRetainedFailure(carried, notCarried int64) string {
	if carried <= notCarried+retainedHeapMargin {
		return ""
	}
	return fmt.Sprintf("the scan retains %d bytes where it holds the most with the walk's entries carried, "+
		"against %d bytes at the same instant over the same fixture with the SAME walk's entries not carried "+
		"(margin %d): carrying the walk's listings has raised what a pass costs in memory",
		carried, notCarried, retainedHeapMargin)
}

// TestScan_RetainsNothingPerSourceAfterTheScanThatConsumedIt. The listings a
// walk carried are released as the scan consumes them, so once a scan has
// returned it holds nothing that grew with the number of files it enumerated.
// Quadrupling the library over the same directories must not move what is left
// behind at all, beyond the noise the margin absorbs.
//
// The comparison is BETWEEN THE TWO FIXTURES, both measured in this process and
// this run: the figure that decides is the difference between them, so the
// hardware, the allocator and the instrumentation cancel out of it. The margin
// is deliberately NOT a function of N either - one that grew with the source
// count would pass a build that had kept every entry name for ever, which is the
// failure this exists to catch.
//
// MUTATION (measured): stop releasing each directory's listing as it is
// consumed AND stop taking them off the engine - `take` without its delete, and
// Load in place of Swap in passListings - and this reds with a growth of 242512
// bytes, because the library's entry names are then still on the engine when the
// scan that consumed them has returned. Either half alone is enough to release
// them, so the mutation needs both.
func TestScan_RetainsNothingPerSourceAfterTheScanThatConsumedIt(t *testing.T) {
	const dirs, perDir = 8, 250 // 2000 sources, and 8000 in the second fixture

	afterN, _ := scanRetention(t, dirs, perDir, carryEntries)
	after4N, _ := scanRetention(t, dirs, perDir*4, carryEntries)
	growth := after4N - afterN
	t.Logf("heap retained after the scan returned: %d bytes over %d sources, %d bytes over %d - a growth of %d "+
		"(margin %d)", afterN, dirs*perDir, after4N, dirs*perDir*4, growth, retainedHeapMargin)

	if growth > retainedHeapMargin {
		t.Errorf("quadrupling the sources left %d more bytes retained after the scan than the same run's own "+
			"reading over a quarter of them, which is past the %d bytes of measurement noise this comparison "+
			"allows. Something the scan consumed is outliving it, per source", growth, retainedHeapMargin)
	}
}

// TestScan_PeakRetainedHeapIsNotRaisedByCarryingTheWalksEntries. The other half:
// not raising what a scan holds AT ITS PEAK, which is the instant it has the
// whole enumeration in hand. The walk's entries are released directory by
// directory as they are consumed, so what is held there is the list of sources -
// the same list, of the same size, as a scan that listed the directories a
// second time to build it.
//
// So the property is a COMPARISON and is graded as one: the same walk, measured
// with its entries carried and with them not carried, in this process and this
// run. Carrying may not be the more expensive of the two. Nothing here is held
// against a byte count from another machine; the reason that matters is above
// retainedHeapMargin, and the proof that this comparison can still go red is
// TestScan_PeakRetainedHeapRedsOnADeliberateRegression, immediately below.
func TestScan_PeakRetainedHeapIsNotRaisedByCarryingTheWalksEntries(t *testing.T) {
	const dirs, perDir = 8, 250 // the identical fixture the sibling above measures

	carried, notCarried := peakRetainedPair(t, dirs, perDir, carryEntries)
	if failure := peakRetainedFailure(carried, notCarried); failure != "" {
		t.Error(failure)
	}
}

// TestScan_PeakRetainedHeapRedsOnADeliberateRegression is the anti-vacuity half,
// and it is the case that makes the one above worth running. A relative
// comparison with a margin can be made to pass by widening the margin, by
// measuring the same thing twice, or by losing hold of the regression before the
// reading is taken - and every one of those failures looks exactly like a green
// run. So the regression is DRIVEN here, by the committed suite, in the same
// invocation: the walk's result is made to carry a second, redundant copy of the
// entries it already carries, and peakRetainedFailure must report a failure over
// that pair. If it ever does not, this case is red and the route's exit status
// says the comparison has stopped being able to fail.
//
// Nothing in the engine carries entries twice. The copy is held by the harness
// across both readings, which is what an engine holding a redundant reference
// would cost and is why the regression is measurable rather than argued.
func TestScan_PeakRetainedHeapRedsOnADeliberateRegression(t *testing.T) {
	const dirs, perDir = 8, 250 // the identical fixture the passing case measures

	carried, notCarried := peakRetainedPair(t, dirs, perDir, carryEntriesTwice)
	failure := peakRetainedFailure(carried, notCarried)
	if failure == "" {
		t.Fatalf("a scan made to carry a SECOND, redundant copy of the entries it already carries retained %d "+
			"bytes at its peak against %d with the same walk's entries not carried, and the comparison "+
			"reported NO failure. The peak-retained comparison cannot go red, so the case beside this one is "+
			"not evidence of anything: either the margin is wide enough to swallow a whole duplicate of the "+
			"library's entry names, or the duplicate is not reachable at the instant the reading is taken",
			carried, notCarried)
	}
	t.Logf("the comparison reported the failure it must: %s", failure)
	if !strings.Contains(failure, strconv.FormatInt(carried, 10)) ||
		!strings.Contains(failure, strconv.FormatInt(notCarried, 10)) {
		t.Errorf("the failure the comparison reported does not name both readings it decided from, so a human "+
			"reading a red run cannot see what it compared: %s", failure)
	}
}

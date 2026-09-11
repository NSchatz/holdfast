package engine

import (
	"bytes"
	"context"
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
// Both baselines below are MEASUREMENTS taken on the build this change replaces
// (holdfast 82e4757, the same scan bounded by a coverage set with the sweep and
// the enumeration each listing for themselves), over the identical fixtures,
// through the identical code below with `eng.SetCoverage(res.Coverage,
// res.Entries)` replaced by `eng.Coverage = res.Coverage`, in the package's own
// test binary with nothing else running in it, on the container this repository
// is developed in (linux/amd64, go1.25.14, GOGC at its default), repeated three
// times, the WORST of the three recorded:
//
//	go test ./internal/engine/ -run TestScan_ -v -count=3
//
// Both figures are live heap after two forced collections MINUS the same reading
// taken before the engine existed, so what is measured is what this run HOLDS
// and not what the test binary happened to be holding already.
const (
	// The heap a completed scan still retains grew by this much when the same
	// fixture went from 2000 sources to 8000 on the unchanged build (measured:
	// -12016, 328 and 744 bytes, and -72 and 4624 under -race). It is
	// approximately nothing, and that is the point: what a scan retains after it
	// has returned is fixed by the number of DIRECTORIES, never by the number of
	// files in them.
	pinnedRetainedGrowth = 4_624

	// The heap the scan retains at the instant it holds its complete
	// enumeration, over 2000 sources on the unchanged build (measured: 463536,
	// 463776 and 464560 bytes, and 463632 and 463824 under -race). That instant
	// is the peak of what a pass holds of its own - the whole list of files it is
	// about to feed to the workers, plus anything it has not released of the
	// listings it built that list from.
	pinnedPeakRetained = 464_560

	// The margins are FIXED allowances for measurement drift, never functions of
	// the source count: a margin that grew with N would pass the very build this
	// is written to catch. Both readings move with what the test binary has
	// already run - the same fixture measured 399 KiB and 464 KiB at the same
	// instant in two different positions, which is why the baselines above were
	// taken from these two tests, in this order, and not from a bespoke harness.
	retainedGrowthMargin = 32_768 // 32 KiB
	peakRetainedMargin   = 32_768 // 32 KiB
)

// retainedHeap is live heap after a forced collection: everything unreachable
// has gone, so what is left is what is still HELD. Twice, because the first
// collection can leave finalisable objects for the second.
func retainedHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
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
// walk over the same roots, and the coverage set WITH the listings that walk
// made. The walk's own result stays inside this function, so the measurements
// below are about what the engine retains and not about what a caller happens
// to be holding.
//
// BASELINE: replace the SetCoverage call with `eng.Coverage = res.Coverage` and
// this is the pinned build's engine, which is how the two constants above were
// measured.
func scanEngine(t *testing.T, root string) *Engine {
	t.Helper()
	cfg := baseCfg(root)
	eng := New(cfg, probe.New("", ""), nil, newTestStore(t, root), discardLogger())
	res := walkOver(t, root, cfg.VideoExts, newListingCounter())
	eng.SetCoverage(res.Coverage, res.Entries)
	return eng
}

// scanRetention runs one scan over a synthetic library and reports what the run
// retains at two instants: once it has returned, and at the point inside it
// where it holds its complete enumeration. Nothing is fed to a worker - the pass
// is paused before the first file, so what is measured is the scan's own
// machinery and never an encode's.
func scanRetention(t *testing.T, dirs, perDir int) (afterScan, atEnumeration int64) {
	t.Helper()
	root := t.TempDir()
	synthLibrary(t, root, dirs, perDir)

	before := retainedHeap()
	eng := scanEngine(t, root)
	var peak uint64
	eng.Paused = func() bool {
		// The feed loop asks this before it hands out the first file, so it is
		// called with the whole enumeration in hand: every source path this pass
		// will act on, and whatever the pass still holds of the listings it drew
		// them from.
		if peak == 0 {
			peak = retainedHeap()
		}
		return true
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	after := retainedHeap()
	runtime.KeepAlive(eng)
	if peak == 0 {
		t.Fatal("the scan never reached its feed loop, so nothing was measured")
	}
	return int64(after) - int64(before), int64(peak) - int64(before)
}

// TestScan_RetainsNothingPerSourceAfterTheScanThatConsumedIt. The listings a
// walk carried are released as the scan consumes them, so once a scan has
// returned it holds nothing that grew with the number of files it enumerated.
// Quadrupling the library over the same directories must not move what is left
// behind by more than the same fixtures moved it on the build this replaces.
//
// The ceiling is deliberately NOT a function of N: a margin that grew with the
// source count would pass a build that had kept every entry name for ever,
// which is the failure this exists to catch.
//
// MUTATION (measured): stop releasing each directory's listing as it is
// consumed AND stop taking them off the engine - `take` without its delete, and
// Load in place of Swap in passListings - and this reds with a growth of 242512
// bytes against a ceiling of 37392, because the library's entry names are then
// still on the engine when the scan that consumed them has returned. Either half
// alone is enough to release them, so the mutation needs both.
func TestScan_RetainsNothingPerSourceAfterTheScanThatConsumedIt(t *testing.T) {
	const dirs, perDir = 8, 250 // 2000 sources, and 8000 in the second fixture

	afterN, _ := scanRetention(t, dirs, perDir)
	after4N, _ := scanRetention(t, dirs, perDir*4)
	growth := after4N - afterN
	t.Logf("heap retained after the scan returned: %d bytes over %d sources, %d bytes over %d - a growth of %d "+
		"(the unchanged build grew by %d over the same pair; margin %d)",
		afterN, dirs*perDir, after4N, dirs*perDir*4, growth, pinnedRetainedGrowth, retainedGrowthMargin)

	if ceiling := int64(pinnedRetainedGrowth + retainedGrowthMargin); growth > ceiling {
		t.Errorf("quadrupling the sources left %d more bytes retained after the scan, and the same pair of "+
			"fixtures left %d more on the build this replaces (ceiling %d). Something the scan consumed is "+
			"outliving it, per source", growth, pinnedRetainedGrowth, ceiling)
	}
}

// TestScan_PeakRetainedHeapIsNotRaisedByCarryingTheWalksEntries. The other half:
// not raising what a scan holds AT ITS PEAK, which is the instant it has the
// whole enumeration in hand. The walk's entries are released directory by
// directory as they are consumed, so what is held there is the list of sources -
// the same list, of the same size, as the build that listed the directories a
// second time to build it.
//
// MUTATION (measured): the same two-part mutation as above - never release a
// consumed listing, never take the carried ones off the engine - and this reds
// at 546344 bytes against a ceiling of 497328, the whole library's entry names
// alive beside the enumeration they produced.
func TestScan_PeakRetainedHeapIsNotRaisedByCarryingTheWalksEntries(t *testing.T) {
	const dirs, perDir = 8, 250 // the identical fixture the baseline was taken over

	_, atEnumeration := scanRetention(t, dirs, perDir)
	ceiling := int64(pinnedPeakRetained + peakRetainedMargin)
	t.Logf("heap retained at the instant the scan holds its complete enumeration: %d bytes over %d sources "+
		"(the unchanged build retained %d at the same instant over the same fixture; margin %d)",
		atEnumeration, dirs*perDir, pinnedPeakRetained, peakRetainedMargin)

	if atEnumeration > ceiling {
		t.Errorf("the scan retains %d bytes where it holds the most, against %d on the build this replaces "+
			"(ceiling %d): carrying the walk's listings has raised what a pass costs in memory", atEnumeration,
			pinnedPeakRetained, ceiling)
	}
}

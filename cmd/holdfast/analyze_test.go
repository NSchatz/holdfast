package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// The census command's suite. Two properties are the reason the command exists and
// they are written first: that running it is a READ (no claim, no row, not a byte
// moved anywhere), and that it reports what is in the roots BESIDE what the
// configuration in force would act on, naming every mechanism between the two.

// encodeFixture writes one real, probeable media file. Real on purpose: every
// distribution below is read out of the file by ffprobe, so a fixture of zero bytes
// with a .mkv name would prove the buckets exist and nothing about what fills them.
func encodeFixture(t *testing.T, path, codec, size, pixFmt string) {
	t.Helper()
	encodeFixtureFrames(t, path, codec, size, pixFmt, "3")
}

func encodeFixtureFrames(t *testing.T, path, codec, size, pixFmt, frames string) {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required for the census proofs: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=12:size="+size+":rate=5", "-frames:v", frames,
		"-c:v", codec, "-preset", "ultrafast", "-pix_fmt", pixFmt, "--", path).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture %s (%s %s %s): %v\n%s", path, codec, size, pixFmt, err, out)
	}
}

func writeCensusFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// censusLibrary lays out one library carrying, deliberately, one file for every
// mechanism that can sit between "what is in the roots" and "what holdfast would act
// on": three real sources, two files no configured extension matches, one
// work-in-progress temp, one retained replacement, and one original inside the undo
// window's retention area.
//
// It returns the config path, the library root and the state directory (which does
// NOT exist: a census must not need one, and must not create one).
func censusLibrary(t *testing.T, extra string) (cfgPath, lib, state string) {
	t.Helper()
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	state = filepath.Join(dir, "state")

	encodeFixture(t, filepath.Join(lib, "movie.mkv"), "libx264", "320x240", "yuv420p")
	encodeFixture(t, filepath.Join(lib, "show", "ep1.mp4"), "libx264", "1920x1080", "yuv420p")
	encodeFixture(t, filepath.Join(lib, "show", "ep2.mkv"), "libx265", "1280x720", "yuv420p10le")

	writeCensusFile(t, filepath.Join(lib, "notes.txt"), "not media\n")
	writeCensusFile(t, filepath.Join(lib, "cover.jpg"), "not media either\n")
	writeCensusFile(t, filepath.Join(lib, "movie."+engine.TempMarker+".mkv"), "work in progress\n")
	writeCensusFile(t, filepath.Join(lib, "show", "ep1."+engine.RetainedMarker+".mp4"), "a replacement holdfast kept\n")
	writeCensusFile(t, filepath.Join(lib, engine.UndoDirName, "movie.abc123."+engine.UndoMarker+".mkv"), "a retained original\n")

	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))
	return cfgPath, lib, state
}

// treeSnapshot is every path under root with its mode and the digest of its bytes.
// It is what "byte-for-byte identical" is checked against: a file whose content moved,
// one that appeared (a cache, a memo, a SQLite sidecar) and one that vanished are all
// a difference here.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if d.IsDir() {
			out[rel] = fmt.Sprintf("dir %v", info.Mode().Perm())
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		out[rel] = fmt.Sprintf("%v %d %s", info.Mode().Perm(), len(b), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func assertSameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	var problems []string
	for path, was := range before {
		now, ok := after[path]
		if !ok {
			problems = append(problems, "removed: "+path)
			continue
		}
		if now != was {
			problems = append(problems, fmt.Sprintf("changed: %s (%s -> %s)", path, was, now))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			problems = append(problems, "created: "+path)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%s is not byte-for-byte what it was before the command:\n  %s",
			what, strings.Join(problems, "\n  "))
	}
}

// seedCensusLedger writes a real ledger with one claimed row in it, so "no row was
// created" is checked against a table that HAS rows rather than against an empty one,
// and so a second row would be visible as a count.
func seedCensusLedger(t *testing.T, state string, rows ...string) int {
	t.Helper()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, path := range rows {
		if _, err := st.Claim(context.Background(), path, fmt.Sprintf("fingerprint-%d", i), "w0", 3,
			store.DecisionInputs{}); err != nil {
			t.Fatal(err)
		}
	}
	return len(rows)
}

// jobRowCount is every row in the jobs table, whatever state it is in.
func jobRowCount(t *testing.T, state string) int {
	t.Helper()
	st, err := store.OpenReadOnly(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open the ledger to count its rows: %v", err)
	}
	defer func() { _ = st.Close() }()
	summary, err := st.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range summary {
		n += c
	}
	return n
}

// TestAnalyze_WritesNoJobRows is AC1a, and it is the criterion that keeps this command
// a read: after it has run - to completion, and with the health pass that opens every
// source - no job has been claimed, the ledger holds exactly the rows it held before,
// and every byte under the state directory and under the library root is the byte that
// was there when the command started.
func TestAnalyze_WritesNoJobRows(t *testing.T) {
	for _, args := range [][]string{
		{"analyze", "--config"},
		{"analyze", "--health", "--config"},
		{"analyze", "--json", "--health", "--config"},
	} {
		t.Run(strings.Join(args[1:len(args)-1], " "), func(t *testing.T) {
			cfgPath, lib, state := censusLibrary(t, "")
			want := seedCensusLedger(t, state, "/gone/vanished.mkv")

			libBefore := treeSnapshot(t, lib)
			stateBefore := treeSnapshot(t, state)

			var out, errOut bytes.Buffer
			if code := dispatch(append(args, cfgPath), &out, &errOut); code != 0 {
				t.Fatalf("analyze code = %d, want 0 (stderr: %s)", code, errOut.String())
			}

			assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
			assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
			if got := jobRowCount(t, state); got != want {
				t.Fatalf("the ledger holds %d row(s) after analyze, want the %d it held before", got, want)
			}
		})
	}
}

// TestAnalyze_ReportsFilteredAndUnfilteredCounts is AC4: two file counts and two byte
// totals, and the output NAMES each mechanism that produced the gap between them. The
// fixture carries one file for every such mechanism, so a report that quietly folded
// any of them into the source count would be short by a known number.
func TestAnalyze_ReportsFilteredAndUnfilteredCounts(t *testing.T) {
	cfgPath, lib, _ := censusLibrary(t, "")

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("analyze code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	got := out.String()

	// Every regular file in the covered directories: 3 sources + 2 by extension + 1
	// temp + 1 retained replacement + 1 retained original = 8.
	var allBytes int64
	var sourceBytes int64
	sources := map[string]bool{
		filepath.Join(lib, "movie.mkv"):       true,
		filepath.Join(lib, "show", "ep1.mp4"): true,
		filepath.Join(lib, "show", "ep2.mkv"): true,
	}
	err := filepath.WalkDir(lib, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		allBytes += info.Size()
		if sources[path] {
			sourceBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		fmt.Sprint(8),           // every regular file in the covered directories
		fmt.Sprint(allBytes),    // and their bytes
		fmt.Sprint(3),           // the subset the configuration would act on
		fmt.Sprint(sourceBytes), // and theirs
		"video_exts",            // the mechanisms, each named
		engine.UndoDirName,
		engine.TempMarker,
		engine.RetainedMarker,
		mechRecord,
		"coverage",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the census never reports %q:\n%s", want, got)
		}
	}

	// The same two figures, and the account between them, in the machine-readable form.
	c := analyzeJSON(t, cfgPath)
	if c.Total.Found.Files != 8 || c.Total.Found.Bytes != allBytes {
		t.Fatalf("found = %d file(s) %d byte(s), want 8 and %d", c.Total.Found.Files, c.Total.Found.Bytes, allBytes)
	}
	if c.Total.Sources.Files != 3 || c.Total.Sources.Bytes != sourceBytes {
		t.Fatalf("sources = %d file(s) %d byte(s), want 3 and %d",
			c.Total.Sources.Files, c.Total.Sources.Bytes, sourceBytes)
	}
	// The account is complete: the mechanisms explain the whole of the difference.
	var withheld int64
	named := map[string]int64{}
	for _, m := range c.Total.Withheld {
		named[m.Name] = m.Files
		if m.Name != mechIrregular {
			withheld += m.Files
		}
	}
	if c.Total.Sources.Files+withheld != c.Total.Found.Files {
		t.Fatalf("%d source(s) + %d withheld != %d found: the mechanisms do not account for the gap",
			c.Total.Sources.Files, withheld, c.Total.Found.Files)
	}
	for name, want := range map[string]int64{
		mechExt: 2, mechUndoDir: 1, mechTemp: 1, mechRetained: 1, mechRecord: 0,
	} {
		if named[name] != want {
			t.Fatalf("mechanism %q withheld %d file(s), want %d (all: %+v)", name, named[name], want, named)
		}
	}
	if c.Total.Coverage.Boundary == "" {
		t.Fatal("the coverage boundary - the mechanism with no file count - is never named")
	}
}

// analyzeJSON runs the census in its machine-readable form and decodes it. Decoding
// into the command's own type is deliberate: a field the renderer publishes and this
// test cannot see does not exist.
func analyzeJSON(t *testing.T, cfgPath string, extra ...string) census {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"analyze", "--config", cfgPath, "--json"}, extra...)
	code := dispatch(args, &out, &errOut)
	var c census
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		t.Fatalf("--json did not write one JSON document (code %d): %v\n%s", code, err, out.String())
	}
	return c
}

// TestAnalyze_ReportsEveryFigurePerRootAndInTotal is AC1: per root and for all roots
// combined, the source count, the bytes, and four distributions whose buckets plus an
// explicitly reported excluded count equal that file count.
func TestAnalyze_ReportsEveryFigurePerRootAndInTotal(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "films")
	second := filepath.Join(dir, "series")
	encodeFixture(t, filepath.Join(first, "a.mkv"), "libx264", "320x240", "yuv420p")
	encodeFixture(t, filepath.Join(second, "b.mkv"), "libx264", "1920x1080", "yuv420p")
	encodeFixture(t, filepath.Join(second, "c.mp4"), "libx265", "1280x720", "yuv420p10le")
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + first + "\n  - " + second + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	c := analyzeJSON(t, cfgPath)
	if len(c.Roots) != 2 {
		t.Fatalf("the census reports %d root(s), want one per configured root", len(c.Roots))
	}
	wantPerRoot := map[string]int64{first: 1, second: 2}
	for _, rc := range c.Roots {
		if rc.Sources.Files != wantPerRoot[rc.Root] {
			t.Fatalf("root %s reports %d source(s), want %d", rc.Root, rc.Sources.Files, wantPerRoot[rc.Root])
		}
		assertDistributionsAddUp(t, rc)
	}
	if c.Total.Sources.Files != 3 {
		t.Fatalf("the combined census reports %d source(s), want 3", c.Total.Sources.Files)
	}
	if c.Total.Sources.Bytes != c.Roots[0].Sources.Bytes+c.Roots[1].Sources.Bytes {
		t.Fatal("the combined byte total is not the sum of the roots'")
	}
	assertDistributionsAddUp(t, c.Total)

	// The four distributions are the four the criterion names, and the codecs and
	// depths are the ones the fixtures actually carry.
	byName := map[string]distribution{}
	for _, d := range c.Total.Distributions {
		byName[d.Name] = d
	}
	for _, name := range []string{"video codec", "resolution band", "container", "video bit depth"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("no %q distribution in the census", name)
		}
	}
	for name, want := range map[string]map[string]int64{
		"video codec":     {"h264": 2, "hevc": 1},
		"resolution band": {"480 and below": 1, "481-720": 1, "721-1080": 1},
		"container":       {"mkv": 2, "mp4": 1},
		"video bit depth": {"8-bit": 2, "10-bit": 1},
	} {
		got := map[string]int64{}
		for _, b := range byName[name].Buckets {
			got[b.Key] = b.Count
		}
		for k, n := range want {
			if got[k] != n {
				t.Fatalf("%s bucket %q = %d, want %d (all: %+v)", name, k, got[k], n, got)
			}
		}
	}
}

func assertDistributionsAddUp(t *testing.T, rc *rootCensus) {
	t.Helper()
	for _, d := range rc.Distributions {
		var counted int64
		for _, b := range d.Buckets {
			counted += b.Count
		}
		if counted+d.Excluded != rc.Sources.Files {
			t.Fatalf("%s under %s: %d bucketed + %d excluded != %d source file(s)",
				d.Name, rc.Root, counted, d.Excluded, rc.Sources.Files)
		}
		if d.Set == "" {
			t.Fatalf("%s under %s does not declare the set it was computed over", d.Name, rc.Root)
		}
	}
}

// TestAnalyze_UnansweredPropertiesGoToADeclaredUnknownBucket is AC1b: a file ffprobe
// answers about but can tell nothing about lands in a declared unknown bucket, never in
// a numeric band and never at a guessed depth - and the output declares the whole band
// set, so a reader can tell an empty band from a band that does not exist.
func TestAnalyze_UnansweredPropertiesGoToADeclaredUnknownBucket(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	encodeFixture(t, filepath.Join(lib, "real.mkv"), "libx264", "320x240", "yuv420p")
	// A file with a configured extension that is not media at all: ffprobe reads the
	// path and refuses it, which is an answer ABOUT THE FILE and not a broken host.
	writeCensusFile(t, filepath.Join(lib, "notreally.mkv"), "this is not a matroska file\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+
		filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	c := analyzeJSON(t, cfgPath)
	if c.Total.Sources.Files != 2 {
		t.Fatalf("the census reports %d source(s), want 2 - an unreadable file is still a file", c.Total.Sources.Files)
	}
	for _, name := range []string{"video codec", "resolution band", "video bit depth"} {
		var d distribution
		for _, cand := range c.Total.Distributions {
			if cand.Name == name {
				d = cand
			}
		}
		got := map[string]int64{}
		for _, b := range d.Buckets {
			got[b.Key] = b.Count
		}
		if got[bandUnknown] != 1 {
			t.Fatalf("%s has %d file(s) in the %q bucket, want 1 (all: %+v)", name, got[bandUnknown], bandUnknown, got)
		}
		for key, n := range got {
			if key == bandUnknown || n == 0 {
				continue
			}
			if key == "h264" || key == "480 and below" || key == "8-bit" {
				continue
			}
			t.Fatalf("%s put an unanswered file in %q, which is a guess: %+v", name, key, got)
		}
	}
	if len(c.Bands.Bands) == 0 || c.Bands.Basis == "" {
		t.Fatalf("the census does not declare the band set it used: %+v", c.Bands)
	}
	var out, errOut bytes.Buffer
	dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut)
	for _, band := range c.Bands.Bands {
		if !strings.Contains(out.String(), band) {
			t.Fatalf("the table never declares the band %q:\n%s", band, out.String())
		}
	}
}

// TestAnalyze_JSONIsOneDocumentAndTheTableIsNot is AC1c.
func TestAnalyze_JSONIsOneDocumentAndTheTableIsNot(t *testing.T) {
	cfgPath, _, _ := censusLibrary(t, "")

	var jsonOut, jsonErr bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath, "--json"}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("analyze --json code = %d (stderr: %s)", code, jsonErr.String())
	}
	var single any
	dec := json.NewDecoder(bytes.NewReader(jsonOut.Bytes()))
	if err := dec.Decode(&single); err != nil {
		t.Fatalf("--json stdout does not parse as a single value: %v\n%s", err, jsonOut.String())
	}
	if err := dec.Decode(new(any)); err == nil {
		t.Fatalf("--json wrote MORE than one JSON document to stdout:\n%s", jsonOut.String())
	}

	var tableOut, tableErr bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &tableOut, &tableErr); code != 0 {
		t.Fatalf("analyze code = %d (stderr: %s)", code, tableErr.String())
	}
	if err := json.Unmarshal(tableOut.Bytes(), new(any)); err == nil {
		t.Fatal("the default output parses as JSON, so a script cannot tell the two forms apart")
	}

	// Every figure the table carries is in the document: each numeric leaf of the JSON
	// is printed somewhere in the table, and each figure declares its own set.
	var c census
	if err := json.Unmarshal(jsonOut.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	table := tableOut.String()
	for _, rc := range append([]*rootCensus{c.Total}, c.Roots...) {
		for _, n := range []int64{rc.Found.Files, rc.Found.Bytes, rc.Sources.Files, rc.Sources.Bytes,
			rc.Coverage.DirectoriesRead, rc.Coverage.DirectoriesNotRead, rc.Coverage.FilesNotInspected} {
			if !strings.Contains(table, fmt.Sprint(n)) {
				t.Fatalf("the table never prints the figure %d that the document carries:\n%s", n, table)
			}
		}
		if rc.Found.Set == "" || rc.Sources.Set == "" {
			t.Fatalf("a figure under %s does not declare the set it covers", rc.Root)
		}
		for _, d := range rc.Distributions {
			for _, b := range d.Buckets {
				if !strings.Contains(table, b.Key) {
					t.Fatalf("the table never prints the bucket %q:\n%s", b.Key, table)
				}
			}
		}
	}
}

// ---- the substituted tools -----------------------------------------------------
//
// Three of the criteria are about what the command does when a tool answers badly, or
// about WHETHER it ran a tool at all. Both questions are answered by substituting the
// binary through the same HOLDFAST_FFMPEG / HOLDFAST_FFPROBE seam an operator has, so
// what is under test is the command's own behaviour and not a mock of it.

func fakeTool(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAnalyze_HealthStatesItsCostBeforeItDecodesAnything is AC2: the cost statement
// names the file count and the total bytes, it is written BEFORE the first decode, and
// the pass then decodes every enumerated source and reports each failure by path.
func TestAnalyze_HealthStatesItsCostBeforeItDecodesAnything(t *testing.T) {
	cfgPath, lib, _ := censusLibrary(t, "")
	dir := t.TempDir()
	log := filepath.Join(dir, "decodes.log")
	// Records every invocation, and fails exactly the file named broken.
	t.Setenv("HOLDFAST_FFMPEG", fakeTool(t, dir, "ffmpeg", `
echo "$@" >> `+log+`
for a in "$@"; do
  case "$a" in *broken*) exit 1;; esac
done
exit 0
`))
	broken := filepath.Join(lib, "broken.mkv")
	writeCensusFile(t, broken, "not a matroska file at all\n")

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath, "--health"}, &out, &errOut); code != 0 {
		t.Fatalf("analyze --health code = %d (stderr: %s)", code, errOut.String())
	}
	got := out.String()

	cost := strings.Index(got, "FULLY DECODE")
	if cost < 0 {
		t.Fatalf("no cost statement before the decode pass:\n%s", got)
	}
	first := strings.Index(got, "decode-integrity\n")
	if first < 0 || first < cost {
		t.Fatalf("the cost statement does not precede the decode-integrity report:\n%s", got)
	}
	// It names both figures: how many files, and how many bytes.
	c := analyzeJSON(t, cfgPath, "--health")
	for _, want := range []string{fmt.Sprint(c.Health.Files), fmt.Sprint(c.Health.Bytes)} {
		if !strings.Contains(got[cost:first], want) {
			t.Fatalf("the cost statement does not name %q:\n%s", want, got[cost:first])
		}
	}
	if !strings.Contains(got, "FAILED "+broken) {
		t.Fatalf("the failing source is not reported by path:\n%s", got)
	}

	// Every enumerated source was decoded, and nothing else was.
	decoded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		filepath.Join(lib, "movie.mkv"), filepath.Join(lib, "show", "ep1.mp4"),
		filepath.Join(lib, "show", "ep2.mkv"), broken,
	} {
		if !strings.Contains(string(decoded), src) {
			t.Fatalf("the decode pass never opened %s:\n%s", src, decoded)
		}
	}
	for _, notASource := range []string{"notes.txt", engine.UndoDirName, engine.TempMarker} {
		if strings.Contains(string(decoded), notASource) {
			t.Fatalf("the decode pass opened %s, which is not an enumerated source:\n%s", notASource, decoded)
		}
	}
}

// TestAnalyze_HealthReportsAFailureAndDoesNothingElse is AC2a, and it is graded against
// a REAL decode failure rather than a substituted one: the file is a real encode with
// its tail cut off, so the pass fails it the way it fails an operator's damaged film.
//
// The "and nothing else" half is the load-bearing one: the failing file is byte-for-byte
// where it was, no ledger row was written about it, and a `holdfast run` afterwards
// enumerates and decides the whole library EXACTLY as it does with no analyze in front
// of it - which is checked by running it both ways and comparing the ledgers.
func TestAnalyze_HealthReportsAFailureAndDoesNothingElse(t *testing.T) {
	cfgPath, lib, state := censusLibrary(t, "dry_run: true\nmin_bitrate_kbps: 0\nvmaf_enable: false\n")
	broken := filepath.Join(lib, "damaged.mkv")
	encodeFixtureFrames(t, broken, "libx264", "320x240", "yuv420p", "60")
	whole, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	// Damage in the MIDDLE of the picture data, which is the corruption an operator
	// meets: the file still probes, still has a codec and a duration, and only a full
	// decode finds it. (A file cut off at the end does not fail this gate at all, which
	// is itself worth knowing and is why the damage is written rather than truncated.)
	for i := len(whole) / 3; i < len(whole)/3+4000 && i < len(whole); i++ {
		whole[i] = 0xff
	}
	if err := os.WriteFile(broken, whole, 0o644); err != nil {
		t.Fatal(err)
	}

	// What `run` does to this library with no census in front of it.
	if code := dispatch([]string{"run", "--config", cfgPath}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("the baseline run failed")
	}
	baseline := ledgerRows(t, state)
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}

	libBefore := treeSnapshot(t, lib)
	var out, errOut bytes.Buffer
	code := dispatch([]string{"analyze", "--config", cfgPath, "--health"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("analyze --health code = %d (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "FAILED "+broken) {
		t.Fatalf("the decode-integrity pass did not report the truncated file:\n%s", out.String())
	}
	assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
	if _, err := os.Stat(state); err == nil {
		t.Fatal("analyze created the state directory: it wrote a row about what it found")
	}

	// And the run that follows is the same run.
	if code := dispatch([]string{"run", "--config", cfgPath}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("the run after analyze failed")
	}
	after := ledgerRows(t, state)
	if len(after) == 0 {
		t.Fatal("the run recorded nothing, so this proves nothing")
	}
	if fmt.Sprint(baseline) != fmt.Sprint(after) {
		t.Fatalf("the run after analyze decided differently:\n before: %v\n  after: %v", baseline, after)
	}
	assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
}

// ledgerRows is every row in the ledger as `path -> status/reason`, ordered, which is
// what "the run decided the same thing" is compared on.
func ledgerRows(t *testing.T, state string) []string {
	t.Helper()
	st, err := store.OpenReadOnly(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() { _ = st.Close() }()
	var out []string
	if err := st.EachTerminal(context.Background(), func(j store.Job) error {
		out = append(out, fmt.Sprintf("%s %s %s %s", j.Path, j.Status, j.Outcome.Reason, j.Outcome.SourceCodec))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestAnalyze_WithoutHealthNothingIsDecoded is AC2b: no flag, no decode - and a source
// known not to decode is counted in the census exactly like any other file.
func TestAnalyze_WithoutHealthNothingIsDecoded(t *testing.T) {
	cfgPath, lib, _ := censusLibrary(t, "")
	dir := t.TempDir()
	log := filepath.Join(dir, "decodes.log")
	t.Setenv("HOLDFAST_FFMPEG", fakeTool(t, dir, "ffmpeg", "echo \"$@\" >> "+log+"\nexit 0\n"))
	writeCensusFile(t, filepath.Join(lib, "broken.mkv"), "not a matroska file at all\n")

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("analyze code = %d (stderr: %s)", code, errOut.String())
	}
	if _, err := os.Stat(log); err == nil {
		body, _ := os.ReadFile(log)
		t.Fatalf("a decode ran without --health:\n%s", body)
	}
	for _, unwanted := range []string{"decode-integrity", "FULLY DECODE"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("the report carries %q without --health:\n%s", unwanted, out.String())
		}
	}
	c := analyzeJSON(t, cfgPath)
	if c.Health != nil {
		t.Fatalf("the document carries a health report without --health: %+v", c.Health)
	}
	if c.Total.Sources.Files != 4 {
		t.Fatalf("the census counts %d source(s), want 4 - a file that does not decode is still a source",
			c.Total.Sources.Files)
	}
}

// TestAnalyze_ContradictsAWrongLedger is AC3: rows for paths that no longer exist and a
// row recording a codec the file does not carry, and the census reports the filesystem
// as it is now - with nothing written that a second invocation could read instead of
// looking again.
func TestAnalyze_ContradictsAWrongLedger(t *testing.T) {
	cfgPath, lib, state := censusLibrary(t, "")
	seedCensusLedger(t, state, "/gone/vanished-one.mkv", "/gone/vanished-two.mkv")
	// A row that records h265 for a file that is really h264.
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(lib, "movie.mkv")
	if _, err := st.Claim(context.Background(), real, "fp-real", "w0", 3, store.DecisionInputs{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(context.Background(), real, "fp-real", store.Done,
		&store.Outcome{SourceCodec: "hevc", Encoder: "cpu"}, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stateBefore := treeSnapshot(t, state)
	first := analyzeJSON(t, cfgPath)

	// The vanished paths are in no figure: the census counts the 8 files that are there.
	if first.Total.Found.Files != 8 || first.Total.Sources.Files != 3 {
		t.Fatalf("the ledger's vanished rows reached the figures: found %d, sources %d",
			first.Total.Found.Files, first.Total.Sources.Files)
	}
	// The codec is the file's, not the row's: two h264 files and one hevc, whatever the
	// ledger says about movie.mkv.
	codecs := map[string]int64{}
	for _, d := range first.Total.Distributions {
		if d.Name == "video codec" {
			for _, b := range d.Buckets {
				codecs[b.Key] = b.Count
			}
		}
	}
	if codecs["h264"] != 2 || codecs["hevc"] != 1 {
		t.Fatalf("the codec distribution followed the ledger rather than the files: %+v", codecs)
	}

	// Nothing was written, so the second invocation had nothing to read but the files.
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
	second := analyzeJSON(t, cfgPath)
	if fmt.Sprint(first.Total.Distributions) != fmt.Sprint(second.Total.Distributions) {
		t.Fatal("two invocations disagree, so one of them read something other than the filesystem")
	}
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
}

// TestAnalyze_DegradedProbeStillReportsTheFilesystem is AC5.
func TestAnalyze_DegradedProbeStillReportsTheFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(t *testing.T, dir string)
		expect string
	}{
		{
			name:   "the binary cannot be run at all",
			set:    func(t *testing.T, dir string) { t.Setenv("HOLDFAST_FFPROBE", filepath.Join(dir, "nope")) },
			expect: "could not be run",
		},
		{
			name: "it runs and answers nothing",
			set: func(t *testing.T, dir string) {
				t.Setenv("HOLDFAST_FFPROBE", fakeTool(t, dir, "ffprobe", "exit 1\n"))
			},
			expect: "answered nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, _ := censusLibrary(t, "")
			tc.set(t, t.TempDir())

			var out, errOut bytes.Buffer
			code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut)
			if code == 0 {
				t.Fatalf("a partial census exited 0, so a script would read it as a complete one:\n%s", out.String())
			}
			c := analyzeJSON(t, cfgPath)
			if c.Total.Found.Files != 8 || c.Total.Sources.Files != 3 {
				t.Fatalf("the filesystem figures did not survive the degraded probe: found %d, sources %d",
					c.Total.Found.Files, c.Total.Sources.Files)
			}
			for _, d := range c.Total.Distributions {
				if d.Name == "container" {
					if len(d.Buckets) == 0 {
						t.Fatal("the container distribution is not probe-derived and should have survived")
					}
					continue
				}
				if d.Unavailable == "" {
					t.Fatalf("%s is not marked unavailable: %+v", d.Name, d)
				}
				if !strings.Contains(d.Unavailable, tc.expect) {
					t.Fatalf("%s does not say WHY it is unavailable: %q", d.Name, d.Unavailable)
				}
				if len(d.Buckets) != 0 {
					t.Fatalf("%s published buckets from a probe that could not answer: %+v", d.Name, d.Buckets)
				}
				if d.Excluded != c.Total.Sources.Files {
					t.Fatalf("%s excludes %d of %d source(s): an unavailable figure excludes all of them",
						d.Name, d.Excluded, c.Total.Sources.Files)
				}
			}
			if !strings.Contains(out.String(), "UNAVAILABLE") {
				t.Fatalf("the table does not say the distributions are unavailable:\n%s", out.String())
			}
		})
	}
}

// censusPlatform is the host as the startup walk reads it, with two failures injected
// that no permission bit can produce reliably on every machine this suite runs on: a
// directory whose listing fails, and a directory whose listing returns a name that is
// not there - which is exactly what a file deleted between the walk and the census
// looks like from here.
type censusPlatform struct {
	startup.Platform
	unlistable string
	ghostIn    string
}

func (p censusPlatform) ReadDir(path string) ([]startup.Entry, error) {
	if path == p.unlistable {
		return nil, fs.ErrPermission
	}
	ents, err := p.Platform.ReadDir(path)
	if err == nil && path == p.ghostIn {
		ents = append(ents, startup.Entry{Name: "vanished.mkv"})
	}
	return ents, err
}

// TestAnalyze_ReportsWhatItCouldNotRead is AC6: the census completes over what it did
// read, says how many directories it could not read and how many files it could not
// inspect, and never reports an unread directory's contents as absent.
func TestAnalyze_ReportsWhatItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	encodeFixture(t, filepath.Join(lib, "a.mkv"), "libx264", "320x240", "yuv420p")
	locked := filepath.Join(lib, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	encodeFixture(t, filepath.Join(locked, "hidden.mkv"), "libx264", "320x240", "yuv420p")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+
		filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := startupPlatform
	startupPlatform = func() startup.Platform {
		return censusPlatform{Platform: startup.System(fixedType("ext4"), nil), unlistable: locked, ghostIn: lib}
	}
	t.Cleanup(func() { startupPlatform = old })

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("analyze code = %d, want 0 - a census over what was read is still a census (stderr: %s)",
			code, errOut.String())
	}
	c := analyzeJSON(t, cfgPath)
	if c.Total.Sources.Files != 1 {
		t.Fatalf("the census over what it read reports %d source(s), want 1", c.Total.Sources.Files)
	}
	if c.Total.Coverage.DirectoriesNotRead < 1 {
		t.Fatalf("the census does not report the directory it could not read: %+v", c.Total.Coverage)
	}
	if c.Total.Coverage.FilesNotInspected != 1 {
		t.Fatalf("the census reports %d file(s) it could not inspect, want 1", c.Total.Coverage.FilesNotInspected)
	}
	if !strings.Contains(c.Total.Coverage.Boundary, "UNKNOWN rather than absent") {
		t.Fatalf("the census does not say that an unread directory's contents are unknown: %q",
			c.Total.Coverage.Boundary)
	}
	if !strings.Contains(out.String(), "never reported as absent") {
		t.Fatalf("the table does not say what the coverage boundary costs:\n%s", out.String())
	}
	found := false
	for _, b := range c.Total.Coverage.NotReadWhy {
		if b.Key == string(startup.NoticeUnreadable) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the census does not say WHY a directory went unread: %+v", c.Total.Coverage.NotReadWhy)
	}
}

// TestAnalyze_AnEmptyRootIsACensusNotAnError is AC7a.
func TestAnalyze_AnEmptyRootIsACensusNotAnError(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(filepath.Join(lib, "empty-subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCensusFile(t, filepath.Join(lib, "readme.txt"), "no media here\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+
		filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("an empty library exited %d, want 0 (stderr: %s)", code, errOut.String())
	}
	c := analyzeJSON(t, cfgPath)
	if c.Total.Sources.Files != 0 || c.Total.Sources.Bytes != 0 {
		t.Fatalf("an empty library reports %d source(s): %+v", c.Total.Sources.Files, c.Total.Sources)
	}
	for _, d := range c.Total.Distributions {
		if len(d.Buckets) != 0 || d.Excluded != 0 {
			t.Fatalf("%s over an empty library is not empty: %+v", d.Name, d)
		}
		if d.Unavailable != "" {
			t.Fatalf("%s is marked unavailable, but it was computable and empty: %q", d.Name, d.Unavailable)
		}
	}
}

// TestAnalyze_AMissingRootWritesTheSameRefusal is AC7b: the account is the one the
// start-or-refuse decision itself writes, word for word - not a second wording of it.
func TestAnalyze_AMissingRootWritesTheSameRefusal(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+missing+"\nstate_dir: "+
		filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 1 {
		t.Fatalf("analyze over a missing root exited %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), missing) {
		t.Fatalf("the refusal does not name the root:\n%s", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a refused census still wrote a report to stdout:\n%s", out.String())
	}

	var runOut, runErr bytes.Buffer
	dispatch([]string{"run", "--config", cfgPath}, &runOut, &runErr)
	const marker = "holdfast: refusing to start"
	i, j := strings.Index(errOut.String(), marker), strings.Index(runErr.String(), marker)
	if i < 0 || j < 0 {
		t.Fatalf("one of the two never wrote the refusal account:\nanalyze:\n%s\nrun:\n%s", errOut.String(), runErr.String())
	}
	if errOut.String()[i:] != runErr.String()[j:] {
		t.Fatalf("analyze writes a DIFFERENT account from run:\nanalyze:\n%s\nrun:\n%s",
			errOut.String()[i:], runErr.String()[j:])
	}
}

// TestAnalyze_ReportsTheStorageVerdictAndCensusesAnyway is AC8a, and AC8b beside it:
// the same configuration that `run` refuses is one `analyze` reports on and proceeds
// from, and `run` still refuses it exactly as it did.
func TestAnalyze_ReportsTheStorageVerdictAndCensusesAnyway(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "nas", "media")
	encodeFixture(t, filepath.Join(lib, "movie.mkv"), "libx264", "320x240", "yuv420p")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+
		filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, typesByPrefix(map[string]string{filepath.Join(dir, "nas"): "nfs"}))

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("analyze over storage that is not local exited %d, want 0 (stderr: %s)", code, errOut.String())
	}
	c := analyzeJSON(t, cfgPath)
	if c.Storage.WouldStart {
		t.Fatal("the census reports that a mutating run would start on storage that is not local")
	}
	if c.Storage.DecidedAt != rowStorageNotLocal || len(c.Storage.Causes) == 0 {
		t.Fatalf("the storage verdict is not reported: %+v", c.Storage)
	}
	if c.Total.Sources.Files != 1 {
		t.Fatalf("the census was not produced: %d source(s)", c.Total.Sources.Files)
	}
	for _, want := range []string{"nfs", lib, "REFUSE"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the table does not report %q of the storage verdict:\n%s", want, out.String())
		}
	}

	// AC8b: the mutating commands are untouched by any of this.
	var runOut, runErr bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath}, &runOut, &runErr); code != 1 {
		t.Fatalf("run exited %d over storage analyze reported on: the gate moved", code)
	}
	if !strings.Contains(runErr.String(), "refusing to start") {
		t.Fatalf("run no longer refuses:\n%s", runErr.String())
	}
}

// TestAnalyze_ConfigErrorsAreTheOnesEveryOtherCommandGives is AC9, graded by comparing
// analyze against the commands that already take a config: same message, same exit
// code, and no directory listed or file probed on the way out.
func TestAnalyze_ConfigErrorsAreTheOnesEveryOtherCommandGives(t *testing.T) {
	dir := t.TempDir()
	probes := filepath.Join(dir, "probes.log")
	t.Setenv("HOLDFAST_FFPROBE", fakeTool(t, dir, "ffprobe", "echo \"$@\" >> "+probes+"\nexit 0\n"))

	unreadable := filepath.Join(dir, "missing.yaml")
	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("library_roots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no --config at all", nil},
		{"a config that cannot be loaded", []string{"--config", unreadable}},
		{"a config that does not validate", []string{"--config", invalid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var aOut, aErr bytes.Buffer
			aCode := dispatch(append([]string{"analyze"}, tc.args...), &aOut, &aErr)
			var vOut, vErr bytes.Buffer
			vCode := dispatch(append([]string{"validate"}, tc.args...), &vOut, &vErr)
			if aCode != vCode {
				t.Fatalf("analyze exits %d where validate exits %d", aCode, vCode)
			}
			if aErr.String() != vErr.String() {
				t.Fatalf("analyze says %q where validate says %q", aErr.String(), vErr.String())
			}
			if aCode == 0 {
				t.Fatal("a broken config exited 0")
			}
			if aOut.Len() != 0 {
				t.Fatalf("analyze wrote a report for a config it refused:\n%s", aOut.String())
			}
		})
	}
	if _, err := os.Stat(probes); err == nil {
		body, _ := os.ReadFile(probes)
		t.Fatalf("a file was probed despite the config failing:\n%s", body)
	}
}

// TestAnalyze_IsDiscoverableTheWayEveryCommandIs is AC10a.
func TestAnalyze_IsDiscoverableTheWayEveryCommandIs(t *testing.T) {
	var out, errOut bytes.Buffer
	dispatch(nil, &out, &errOut)
	if !strings.Contains(errOut.String(), "analyze") {
		t.Fatalf("`holdfast` with no arguments does not list analyze:\n%s", errOut.String())
	}
	line := ""
	for _, l := range strings.Split(errOut.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "analyze ") {
			line = l
		}
	}
	if len(strings.Fields(line)) < 3 {
		t.Fatalf("analyze is listed without a description: %q", line)
	}

	var hOut, hErr bytes.Buffer
	if code := dispatch([]string{"analyze", "-h"}, &hOut, &hErr); code != 0 {
		t.Fatalf("analyze -h exited %d, want 0", code)
	}
	help := hOut.String() + hErr.String()
	for _, flagName := range []string{"-config", "-json", "-health"} {
		if !strings.Contains(help, flagName) {
			t.Fatalf("analyze -h does not list %s:\n%s", flagName, help)
		}
	}
	for _, described := range []string{"JSON document", "decode"} {
		if !strings.Contains(help, described) {
			t.Fatalf("analyze -h lists a flag without saying what it does (%q):\n%s", described, help)
		}
	}
}

// TestAnalyze_IsInTheReadmeQuickStart is AC10c: the command is discoverable without
// reading the source, in the block every other command is shown in.
func TestAnalyze_IsInTheReadmeQuickStart(t *testing.T) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	quick := ""
	for _, section := range strings.Split(string(body), "\n## ") {
		if strings.HasPrefix(section, "Quick start") {
			quick = section
		}
	}
	if quick == "" {
		t.Fatal("the README has no quick start section")
	}
	if !strings.Contains(quick, "holdfast analyze") {
		t.Fatalf("the quick start does not show `holdfast analyze`:\n%s", quick)
	}
	if !strings.Contains(quick, "holdfast run") {
		t.Fatalf("this test is no longer looking at the block the commands are shown in:\n%s", quick)
	}
}

// TestAnalyze_AnInterruptedRunLeavesNothingBehind is the rest of AC1a: a census
// interrupted in the middle of its probe pass has still written nothing, anywhere, and
// says so rather than printing a report that looks complete.
func TestAnalyze_AnInterruptedRunLeavesNothingBehind(t *testing.T) {
	cfgPath, lib, state := censusLibrary(t, "")
	want := seedCensusLedger(t, state, "/gone/vanished.mkv")
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	// A probe that announces itself and then hangs, so the interruption lands INSIDE
	// the pass rather than at a moment the test guessed at.
	t.Setenv("HOLDFAST_FFPROBE", fakeTool(t, dir, "ffprobe", "touch "+started+"\nsleep 30\n"))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	libBefore := treeSnapshot(t, lib)
	stateBefore := treeSnapshot(t, state)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 600; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	var out, errOut bytes.Buffer
	code := runAnalyze(ctx, cfg, analyzeOptions{}, &out, &errOut)
	if code == 0 {
		t.Fatalf("an interrupted census exited 0:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "interrupted") {
		t.Fatalf("an interrupted census does not say so:\n%s", errOut.String())
	}
	assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
	if got := jobRowCount(t, state); got != want {
		t.Fatalf("the interrupted census left %d row(s) in the ledger, want %d", got, want)
	}
}

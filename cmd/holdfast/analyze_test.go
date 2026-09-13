package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
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
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required for the census proofs: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size="+size+":rate=5", "-frames:v", "3",
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
	cfgPath, lib, state := censusLibrary(t, "")
	want := seedCensusLedger(t, state, "/gone/vanished.mkv")

	libBefore := treeSnapshot(t, lib)
	stateBefore := treeSnapshot(t, state)

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"analyze", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("analyze code = %d, want 0 (stderr: %s)", code, errOut.String())
	}

	assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
	if got := jobRowCount(t, state); got != want {
		t.Fatalf("the ledger holds %d row(s) after analyze, want the %d it held before", got, want)
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
		"coverage",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the census never reports %q:\n%s", want, got)
		}
	}
}

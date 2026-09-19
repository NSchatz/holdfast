package engine

// The bounded run in the ENGINE (S0100): one named file, or a count of decisions.
//
// A bounded run uses the identical per-file pipeline, so nothing here re-proves a guard,
// a gate or the swap - those are proved over the whole library everywhere else in this
// package. What is new, and what these cases grade, is the SHAPE of the pass: what it
// carries to a terminal outcome, what it stops at, the two whole-library passes it does
// not run, and what it says out loud about being bounded.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// rowsByName is every terminal row in a store, keyed by the file's basename, which is how
// two runs over two identically-built libraries are compared: the directories differ and
// the decisions must not.
func rowsByName(t *testing.T, ts *testStore) map[string]store.Job {
	t.Helper()
	out := map[string]store.Job{}
	for _, row := range terminalRows(t, ts) {
		out[filepath.Base(row.Path)] = row
	}
	return out
}

// names is the sorted key set of such a map, for an assertion that reads as a set
// comparison rather than as a count.
func names(rows map[string]store.Job) []string {
	out := make([]string, 0, len(rows))
	for k := range rows {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// staleTemp writes an orphaned work-in-progress temp of the kind a killed run leaves
// behind - the file the stale-temp sweep exists to discard. It is named after a source
// that is really there, because a temp with no source beside it is one the sweep HOLDS
// (strayReplacementHold): nothing can establish what such a file is, and a fixture built
// that way would be held for a reason that has nothing to do with the bound.
func staleTemp(t *testing.T, root, stem string) string {
	t.Helper()
	p := filepath.Join(root, stem+"."+TempMarker+".mkv")
	if err := os.WriteFile(p, []byte("half an encode"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBoundedRun_DoesNotEnforceRetention grades AC-10, and it grades it BOTH DIRECTIONS
// over ONE fixture library, because this is the single way a bounded run could cost an
// operator audit history that no re-run restores.
//
// Retention is ENABLED here on purpose. It is disabled by the shipped default and
// enforceRetention returns before it reads the store at all in that state, so a fixture
// that left the default in place would assert precisely nothing: the bounded run would
// "prune nothing" because nothing prunes.
//
// The order is the evidence. The bounded pass runs FIRST over rows that are provably
// prunable and removes none of them; the unbounded pass then runs over the SAME library,
// the SAME ledger and the SAME configuration and removes them. Without the second half
// the first is a fixture with nothing to take.
func TestBoundedRun_DoesNotEnforceRetention(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	// Two files already at the target codec: a bounded pass has something real to decide,
	// and deciding it is a skip rather than an encode.
	for i := 0; i < 2; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "already"+strconv.Itoa(i)+".mkv"), "800k")
	}
	temp := staleTemp(t, root, "already0")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 2 // aggressive on purpose: prune everything it may
	})
	if !eng.Cfg.RetentionEnabled() {
		t.Fatal("this fixture asserts nothing unless retention is ENABLED")
	}
	ts := eng.Store.(*testStore)
	// History for files that were in the library and are not any more: this run lists the
	// directory they name and does not find them, which is exactly what makes them
	// removable - and what the bounded pass must not conclude from.
	for i := 0; i < 12; i++ {
		seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	}
	before := len(terminalRows(t, ts))
	if before != 12 {
		t.Fatalf("the fixture seeded %d terminal rows, want 12", before)
	}

	// --- direction one: the bounded pass removes nothing -------------------------------
	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded: %v", err)
	}
	after := terminalRows(t, ts)
	if len(after) < before {
		t.Fatalf("a bounded run removed %d terminal row(s); it must remove none - the pass did not list "+
			"the whole library, so an absent file is no evidence that the file is gone", before-len(after))
	}
	if _, err := os.Stat(temp); err != nil {
		t.Errorf("a bounded run swept the stale temp %s: %v", temp, err)
	}

	// --- direction two: the same fixture DOES prune under an unbounded pass ------------
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 2 {
		t.Fatalf("an unbounded pass over the same fixture left %d terminal rows, want 2 - if it removed "+
			"nothing either, the bounded half above proved nothing", got)
	}
	if _, err := os.Stat(temp); err == nil {
		t.Errorf("an unbounded pass left the stale temp %s in place, so the bounded half proved nothing", temp)
	}
}

// library builds n h264 sources a run will really transcode, named in the order the
// enumeration hands them out.
func library(t *testing.T, ffmpeg, root, stem string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, stem+strconv.Itoa(i)+".mkv"), "8M")
	}
}

// TestBoundedRun_FileCarriesExactlyThatFileAndRecordsWhatAScanWouldHave grades AC-1: a
// `--file` run carries exactly the named file to a terminal outcome and no other file, and
// the outcome it records for that path is the one an unbounded run over the same tree and
// the same configuration records for it.
//
// Two identically-built libraries, one bounded run and one whole-library run, and the
// comparison is between the ROWS: same status, same recorded reason, same verdict about
// the swap. It is the comparison the criterion names rather than a re-derivation of what
// the pipeline should have decided, which would be this test asserting the implementation.
func TestBoundedRun_FileCarriesExactlyThatFileAndRecordsWhatAScanWouldHave(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	bounded := t.TempDir()
	library(t, ffmpeg, bounded, "film", 3)
	boundedEng := buildEngine(t, ffmpeg, ffprobe, bounded, nil, nil)
	target := filepath.Join(bounded, "film1.mkv")
	if err := boundedEng.RunBounded(context.Background(), Bound{File: target}); err != nil {
		t.Fatalf("RunBounded(file): %v", err)
	}

	whole := t.TempDir()
	library(t, ffmpeg, whole, "film", 3)
	wholeEng := buildEngine(t, ffmpeg, ffprobe, whole, nil, nil)
	if err := wholeEng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	got := rowsByName(t, boundedEng.Store.(*testStore))
	want := rowsByName(t, wholeEng.Store.(*testStore))

	// EXACTLY that one file, and no other. The other two were never offered, so they
	// carry no row at all - which is what makes this a bound rather than a filter applied
	// after the fact.
	if len(got) != 1 || got["film1.mkv"].Path == "" {
		t.Fatalf("a --file run recorded terminal rows for %v, want exactly [film1.mkv]", names(got))
	}
	if len(want) != 3 {
		t.Fatalf("the unbounded run recorded %v, want three rows - there is nothing to compare against", names(want))
	}

	a, b := got["film1.mkv"], want["film1.mkv"]
	if a.Status != b.Status {
		t.Errorf("the bounded run recorded status %q for film1.mkv where a whole-library run records %q",
			a.Status, b.Status)
	}
	if a.Outcome.Reason != b.Outcome.Reason {
		t.Errorf("the bounded run recorded reason %q for film1.mkv where a whole-library run records %q",
			a.Outcome.Reason, b.Outcome.Reason)
	}
	// The SWAP decision, read off the library rather than off the row: a run that decided
	// to replace the source produces a file at that path whose codec has moved.
	if got, want := codecOf(t, ffprobe, target), codecOf(t, ffprobe, filepath.Join(whole, "film1.mkv")); got != want {
		t.Errorf("after the bounded run film1.mkv is %s where the whole-library run leaves %s", got, want)
	}
	// And no other file in the bounded library was touched.
	for _, other := range []string{"film0.mkv", "film2.mkv"} {
		if c := codecOf(t, ffprobe, filepath.Join(bounded, other)); c != "h264" {
			t.Errorf("the bounded run left %s as %s; it must not have been touched at all", other, c)
		}
	}
}

// TestBoundedRun_LimitStopsAtThatManyTerminalOutcomes grades AC-4: a `--limit N` run stops
// offering files once N have reached a terminal outcome, counting DECISIONS rather than
// directory entries, and records no more than N.
//
// The fixture makes the difference between the two countable, and it makes it the way a
// real library does. Three of its six files were decided by an earlier pass under this
// very configuration, so the claim turns each of them away: this pass enumerates them,
// takes no decision about them, records nothing, and must not spend the bound on them. If
// the bound counted entries the run would stop having recorded nothing at all.
func TestBoundedRun_LimitStopsAtThatManyTerminalOutcomes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "a_decided"+strconv.Itoa(i)+".mkv"), "800k")
	}
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	ts := eng.Store.(*testStore)
	// The earlier pass. Its rows are the engine's own, taken under the inputs this
	// configuration still offers, which is what makes the claim refuse them next time.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("the pass that decides the first three: %v", err)
	}
	if n := len(rowsByName(t, ts)); n != 3 {
		t.Fatalf("the first pass recorded %d rows, want 3", n)
	}
	for i := 0; i < 3; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "b_fresh"+strconv.Itoa(i)+".mkv"), "800k")
	}

	if err := eng.RunBounded(context.Background(), Bound{Limit: 2}); err != nil {
		t.Fatalf("RunBounded(limit): %v", err)
	}

	rows := rowsByName(t, ts)
	if len(rows) != 5 {
		t.Fatalf("the ledger holds %v after a --limit 2 run over three already-decided files and three "+
			"fresh ones; want the three seeded rows plus exactly two new ones", names(rows))
	}
	for i := 0; i < 2; i++ {
		if _, ok := rows["b_fresh"+strconv.Itoa(i)+".mkv"]; !ok {
			t.Errorf("b_fresh%d.mkv has no terminal row; the bound stopped before the decisions it was to count", i)
		}
	}
	// The file past the bound was never offered, which is the half of the criterion a
	// count of the rows alone cannot see.
	if _, ok := rows["b_fresh2.mkv"]; ok {
		t.Error("b_fresh2.mkv has a terminal row; the run recorded more outcomes than the bound allowed")
	}
}

// TestBoundedRun_AnExhaustedLibraryIsACompleteRun grades AC-5: where the roots hold fewer
// files eligible for a terminal outcome than the bound asks for, every one of them is
// processed, the pass returns no error, and nothing about the unmet bound is recorded at
// `error` level - an exhausted library is a complete run, not a failure (observability O3).
func TestBoundedRun_AnExhaustedLibraryIsACompleteRun(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "only"+strconv.Itoa(i)+".mkv"), "800k")
	}
	var buf bytes.Buffer
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	eng.Log = captureLogger(&buf)

	if err := eng.RunBounded(context.Background(), Bound{Limit: 9}); err != nil {
		t.Fatalf("RunBounded over a library smaller than the bound: %v", err)
	}
	if rows := rowsByName(t, eng.Store.(*testStore)); len(rows) != 2 {
		t.Errorf("a --limit 9 run over two eligible files recorded %v, want both of them", names(rows))
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("an unmet bound was recorded at error level, which asks a human to act on a complete run:\n%s", buf.String())
	}
}

// TestBoundedRun_SaysItIsBoundedAndWhatItSkipped grades AC-11: a bounded run records at
// `info`, as structured fields rather than only as prose, that it was bounded, which bound
// applied with its value, and that the stale-temp sweep and the retention pass were both
// skipped because the pass did not list the whole library.
func TestBoundedRun_SaysItIsBoundedAndWhatItSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a count bound", func(t *testing.T) {
		root := t.TempDir()
		mkHevc(t, ffmpeg, filepath.Join(root, "one.mkv"), "800k")
		var buf bytes.Buffer
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		mustRecordFields(t, buf.String(), []string{
			"level=INFO", "bounded=true", "bound=limit", "bound_value=1",
			"stale_temp_sweep=skipped", "ledger_retention_pass=skipped",
		})
	})

	t.Run("a file bound", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "one.mkv")
		mkHevc(t, ffmpeg, target, "800k")
		var buf bytes.Buffer
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{File: target}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		mustRecordFields(t, buf.String(), []string{
			"level=INFO", "bounded=true", "bound=file", "bound_value=" + target,
			"stale_temp_sweep=skipped", "ledger_retention_pass=skipped",
		})
	})

	// The anti-vacuity arm: an UNBOUNDED pass says none of it, so the fields above are
	// evidence about the bound rather than about every run this engine takes.
	t.Run("an unbounded pass says none of it", func(t *testing.T) {
		root := t.TempDir()
		mkHevc(t, ffmpeg, filepath.Join(root, "one.mkv"), "800k")
		var buf bytes.Buffer
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
		eng.Log = captureLogger(&buf)
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if strings.Contains(buf.String(), "bounded=true") {
			t.Errorf("an unbounded pass reported itself as bounded:\n%s", buf.String())
		}
	})
}

// mustRecordFields fails unless ONE log line carries every field. Asserting them on one
// line is the criterion rather than tidiness: fields scattered across separate records are
// not a record of the bound, they are three records nothing joins.
func mustRecordFields(t *testing.T, logged string, fields []string) {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		all := true
		for _, f := range fields {
			if !strings.Contains(line, f) {
				all = false
				break
			}
		}
		if all {
			return
		}
	}
	t.Errorf("no single record carries %v:\n%s", fields, logged)
}

// TestBoundedRun_AFileBoundReachesTheHoldBacks grades AC-1's "same guards" on the one
// door property a `--file` run
// could plausibly have been narrowed past: it hands a path straight to ProcessFile rather
// than enumerating it, so the hold-backs the enumeration applies have to be applied by the
// door itself. A path an operator withheld is not processed by either route.
func TestBoundedRun_AFileBoundReachesTheHoldBacks(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	target := filepath.Join(root, "withheld.mkv")
	mkH264(t, ffmpeg, target, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	ts := eng.Store.(*testStore)
	if _, err := ts.ExcludePath(context.Background(), target); err != nil {
		t.Fatalf("withhold %s: %v", target, err)
	}
	if err := eng.RunBounded(context.Background(), Bound{File: target}); err != nil {
		t.Fatalf("RunBounded: %v", err)
	}
	if c := codecOf(t, ffprobe, target); c != "h264" {
		t.Errorf("a withheld file was transcoded by a --file run (now %s)", c)
	}
	rows := rowsByName(t, ts)
	if row, ok := rows["withheld.mkv"]; ok && row.Status != store.Skipped {
		t.Errorf("a withheld file recorded %q; a --file run must reach the same verdict a scan does", row.Status)
	}
}

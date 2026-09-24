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
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
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

// everyTerminalRow is every row in a store whose status the LEDGER answers Terminal() for,
// which is the set of decisions the bound is a count of.
//
// It is deliberately not terminalRows: that helper lists the three ordinary statuses, and
// the two a swap incident records - indeterminate and applied-despite-error - are exactly
// the rows a count of the ordinary three cannot see.
func everyTerminalRow(t *testing.T, ts *testStore) []store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{
		store.Done, store.Skipped, store.Failed,
		store.WouldTranscode, store.Indeterminate, store.AppliedDespiteError,
	}, 0)
	if err != nil {
		t.Fatalf("List(every terminal status): %v", err)
	}
	return rows
}

// TestBoundedRun_LimitCountsASwapIncidentAsTheDecisionItIs grades AC-4 over the outcomes a
// swap that did not complete cleanly records: indeterminate and applied-despite-error.
//
// Both are terminal in this repository's own vocabulary - store.Status.Terminal() answers
// yes for each and neither is ever re-claimable - so each is "a file this run carried to a
// state the ledger records as final", which is the spec's own definition of what the bound
// counts. They are written by recordIncident rather than by finishStore, so they are a
// second writer of a terminal row and a second place the count has to be raised.
//
// What is at stake is the bound itself. A pass that never observes a decision being
// recorded offers, encodes, gates and swaps every eligible file in the library, and the
// applied-despite-error half is the sharp one: there the rename took effect, so the
// overshoot replaces a source on a file the operator did not ask this run to touch.
//
// The branch is reached through the engine's own seams - a rename that reports an error,
// over storage the filesystem lookup answers "nfs" for - which is how swap_test.go reaches
// the same outcome.
func TestBoundedRun_LimitCountsASwapIncidentAsTheDecisionItIs(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, "film"+strconv.Itoa(i)+".mkv"), "8M")
	}
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 1 })
	eng.renameFn = failingRename(errSwap)
	eng.fsLookup = lookups("nfs")
	ts := eng.Store.(*testStore)

	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded(limit 1): %v", err)
	}

	rows := everyTerminalRow(t, ts)
	// The fixture has to actually reach the branch, or the assertion below is vacuous.
	incidents := 0
	for _, r := range rows {
		if r.Status == store.Indeterminate || r.Status == store.AppliedDespiteError {
			incidents++
		}
	}
	if incidents == 0 {
		t.Fatalf("no swap-incident row was produced, so this fixture asserts nothing; rows: %+v", rows)
	}
	if len(rows) > 1 {
		paths := make([]string, 0, len(rows))
		for _, r := range rows {
			paths = append(paths, filepath.Base(r.Path)+"="+string(r.Status))
		}
		t.Fatalf("a --limit 1 run recorded %d terminal outcomes (%v), want no more than 1: a row a "+
			"swap incident writes is a decision this run took, so it spends the bound", len(rows), paths)
	}
}

// TestBoundedRun_LimitHoldsWhereTheSwapCompletes is the anti-vacuity arm of the case above:
// the identical fixture with the rename seam removed records exactly one outcome, so a
// failure there is about the uncounted incident row rather than about the bound being
// broken for every run.
//
// It grades AC-4.
func TestBoundedRun_LimitHoldsWhereTheSwapCompletes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, "film"+strconv.Itoa(i)+".mkv"), "8M")
	}
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 1 })
	ts := eng.Store.(*testStore)

	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded(limit 1): %v", err)
	}
	if rows := everyTerminalRow(t, ts); len(rows) != 1 {
		t.Fatalf("a --limit 1 run over three clean sources recorded %d terminal outcomes, want 1", len(rows))
	}
}

// TestBoundedRun_LimitHoldsWithSeveralWorkers grades the half of AC-4 a single-worker
// fixture cannot reach: the bound is exact where a pool of workers is holding files when
// the last slot is recorded, which is what budget.admit's in-flight accounting is for.
//
// The assertion is two-sided on purpose. Fewer rows than the limit is a bound that stopped
// early (an over-count), more is the overshoot the criterion forbids.
func TestBoundedRun_LimitHoldsWithSeveralWorkers(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, limit := range []int{1, 2, 3} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < 8; i++ {
				mkHevc(t, ffmpeg, filepath.Join(root, "f"+strconv.Itoa(i)+".mkv"), "800k")
			}
			eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 4 })
			ts := eng.Store.(*testStore)
			if err := eng.RunBounded(context.Background(), Bound{Limit: limit}); err != nil {
				t.Fatalf("RunBounded(limit %d): %v", limit, err)
			}
			if rows := everyTerminalRow(t, ts); len(rows) != limit {
				t.Fatalf("a --limit %d run over eight eligible files with four workers recorded %d "+
					"terminal outcomes, want exactly %d", limit, len(rows), limit)
			}
		})
	}
}

// terminalRowWriters is every store call that can create a terminal ledger row. A bound is
// a count of those rows, so this set is what the count has to be complete over.
var terminalRowWriters = map[string]bool{
	"Finish":             true,
	"RecordSkip":         true,
	"RecordSwapIncident": true,
}

// TestBoundedRun_EveryWriterOfATerminalRowIsCounted is the other half of AC-4, and it is
// the half that keeps the bound honest as the engine changes.
//
// A `--limit N` run stops on a count of the terminal rows it wrote, so the bound is only as
// complete as the enumeration of what writes one. A writer that does not raise the count is
// not a smaller bug than an off-by-one: the pass never observes a decision being recorded,
// so it offers, encodes, gates and swaps EVERY eligible file in the library.
//
// So the enumeration is read out of the source rather than remembered. Every function in
// this package that calls one of the three store writes above is held here, with whether it
// raises the count, and a fourth writer or a fifth call site fails this rather than shipping
// as a bound nobody can see is broken.
func TestBoundedRun_EveryWriterOfATerminalRowIsCounted(t *testing.T) {
	// The enumeration. Each entry is a function that writes a terminal row, and whether it
	// must raise the count.
	//
	// recordRestoreInJobs is the one that must not, and it is not an exception to the rule
	// so much as a case outside it: the undo window is reached by `holdfast restore`, an
	// operator command that runs no pass, and it is deliberately separable from the engine
	// (it has no counter to raise). A run pass that ever reaches it is the change that has
	// to revisit this line.
	want := map[string]bool{
		"(*Engine).ProcessFile":             true,
		"(*Engine).finishStore":             true,
		"(*Engine).recordIncident":          true,
		"(*UndoWindow).recordRestoreInJobs": false,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the engine package: %v", err)
	}
	pkg, ok := pkgs["engine"]
	if !ok {
		t.Fatal("no package engine here; this check cannot read the writers")
	}

	found := map[string]bool{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !writesATerminalRow(fd) {
				continue
			}
			name := funcName(fd)
			found[name] = true
			raises, expected := want[name]
			if !expected {
				t.Errorf("%s writes a terminal ledger row and is not in this enumeration: a bounded "+
					"run counts rows it does not know are being written, so `--limit N` would carry "+
					"every eligible file in the library", name)
				continue
			}
			if raises != calls(fd, "countTerminalRow") {
				t.Errorf("%s writes a terminal ledger row and raises the count: %v, want %v",
					name, !raises, raises)
			}
		}
	}
	// The anti-vacuity arm: a walk that found nothing would agree with any enumeration at
	// all, including an empty one.
	for name := range want {
		if !found[name] {
			t.Errorf("the walk never found %s, so it grades less than it claims to", name)
		}
	}
}

// writesATerminalRow reports whether fd calls one of the store writes that can create a
// terminal ledger row, on the store itself (`x.Store.Finish(...)`).
func writesATerminalRow(fd *ast.FuncDecl) bool {
	writes := false
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !terminalRowWriters[sel.Sel.Name] {
			return true
		}
		if on, ok := sel.X.(*ast.SelectorExpr); ok && on.Sel.Name == "Store" {
			writes = true
		}
		return true
	})
	return writes
}

// calls reports whether fd calls the named method on its own receiver.
func calls(fd *ast.FuncDecl, name string) bool {
	called := false
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			called = true
		}
		return true
	})
	return called
}

// funcName is how a function is named in the enumeration above: `(*Type).Method` for a
// method, the plain name for anything else.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		if id, ok := star.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fd.Name.Name
		}
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
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

// TestBoundedRun_SaysItIsBoundedAndWhatItSkipped grades S0100 AC-11 as S0159 AC-2 moved
// it: a bounded run records at `info`, as structured fields rather than only as prose, that
// it was bounded, which bound applied with its value, that its stale-temp sweep is the
// owner-checked one rather than skipped, and that the retention pass was skipped because
// the pass did not list the whole library.
func TestBoundedRun_SaysItIsBoundedAndWhatItSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a count bound", func(t *testing.T) {
		root := t.TempDir()
		mkHevc(t, ffmpeg, filepath.Join(root, "one.mkv"), "800k")
		var buf bytes.Buffer
		eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		mustRecordFields(t, buf.String(), []string{
			"level=INFO", "bounded=true", "bound=limit", "bound_value=1",
			"stale_temp_sweep=owner-checked", "ledger_retention_pass=skipped",
		})
	})

	t.Run("a file bound", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "one.mkv")
		mkHevc(t, ffmpeg, target, "800k")
		var buf bytes.Buffer
		eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{File: target}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		mustRecordFields(t, buf.String(), []string{
			"level=INFO", "bounded=true", "bound=file", "bound_value=" + target,
			"stale_temp_sweep=owner-checked", "ledger_retention_pass=skipped",
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

// ---- S0159: the owner-checked stale-temp sweep ------------------------------------------
//
// Every temp holdfast writes beside a source now carries an owner record (tempowner.go),
// and a bounded run removes a temp only where that record proves its owner dead. These
// cases take the record through the engine's own code (ownTemp) and then do to it exactly
// what a SIGKILL does - the file stays on disk and the lock goes with the process's last
// descriptor on it - so the record the sweep decides is the record a killed run leaves.
// Killing a real process is graded at the command surface (cmd/holdfast/run_bounded_test.go).

// ownedEngine is buildEngine with owner records turned on, in a directory of their own on
// storage the filesystem-type lookup answers fsType for. The gate hands out temp
// directories on tmpfs or overlay, which fsclass rightly calls undetermined, so the local
// case is a substituted type NAME that fsclass still classifies.
func ownedEngine(t *testing.T, ffmpeg, ffprobe, root, fsType string) (*Engine, string) {
	t.Helper()
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	dir := filepath.Join(t.TempDir(), TempOwnersDirName)
	eng.TrackTempOwners(dir, lookups(fsType))
	return eng, dir
}

// killedOwner takes the owner record for temp through the engine's own code and then does
// what a SIGKILL does to it: the record stays, and its lock is dropped with the last
// descriptor on it. It returns the record's path.
func killedOwner(t *testing.T, eng *Engine, temp, source string) string {
	t.Helper()
	h, err := eng.ownTemp(temp, source)
	if err != nil || h == nil {
		t.Fatalf("take the owner record for %s: %v (handle %v)", temp, err, h)
	}
	if err := h.f.Close(); err != nil {
		t.Fatal(err)
	}
	return h.path
}

// writeTemp puts a half-written encode at path.
func writeTemp(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("half an encode"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hasField reports whether one text-handler record carries key=value as a field of its own.
func hasField(line, key, value string) bool {
	for _, form := range []string{key + "=" + value, key + "=" + strconv.Quote(value)} {
		for from := 0; ; {
			i := strings.Index(line[from:], form)
			if i < 0 {
				break
			}
			i += from
			end := i + len(form)
			if (i == 0 || line[i-1] == ' ') && (end == len(line) || line[end] == ' ') {
				return true
			}
			from = i + 1
		}
	}
	return false
}

// removalsOf counts the records in logged that say path was removed.
func removalsOf(logged, path string) int {
	n := 0
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `msg="removed an orphaned temp file"`) && hasField(line, "file", path) {
			n++
		}
	}
	return n
}

// treeMD5 is every regular file under root, hashed.
func treeMD5(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out[p] = md5f(t, p)
		}
		return nil
	})
	return out
}

// sameTree fails on every file removed, changed or created between two treeMD5 readings.
func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, sum := range before {
		if got, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		} else if got != sum {
			t.Errorf("%s changed", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}

// TestBoundedRun_RemovesATempWhoseOwnerIsProvablyDeadAndRecordsEachPath grades AC-2, and
// the engine half of AC-1: a `--file` run whose bound never lists the temp's directory
// removes the temp a dead owner left there - and the attached-picture file named after it -
// recording each removal with the removed temp's full path as a field, while the record
// that says the run is bounded carries a stale_temp_sweep that is not `skipped` beside
// ledger_retention_pass=skipped.
func TestBoundedRun_RemovesATempWhoseOwnerIsProvablyDeadAndRecordsEachPath(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	killed := filepath.Join(root, "killed", "film.mkv")
	mustWrite(t, killed)
	target := filepath.Join(root, "elsewhere", "one.mkv")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	mkHevc(t, ffmpeg, target, "800k")

	eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
	var buf bytes.Buffer
	eng.Log = captureLogger(&buf)

	temp := tempPath(filepath.Dir(killed), "film", "mkv", 0)
	picture := picturePath(temp, 0)
	record := killedOwner(t, eng, temp, killed)
	writeTemp(t, temp)
	writeTemp(t, picture)
	sourceBefore := md5f(t, killed)

	if err := eng.RunBounded(context.Background(), Bound{File: target}); err != nil {
		t.Fatalf("RunBounded(file): %v", err)
	}
	logged := buf.String()
	for _, p := range []string{temp, picture} {
		if exists(p) {
			t.Errorf("the bounded run left %s, whose owner is provably dead", p)
		}
		if n := removalsOf(logged, p); n != 1 {
			t.Errorf("the bounded run recorded %d removal(s) of %s, want exactly one carrying its path:\n%s", n, p, logged)
		}
	}
	if exists(record) {
		t.Errorf("the owner record %s outlived the temp it names", record)
	}
	if md5f(t, killed) != sourceBefore {
		t.Error("the killed run's source changed")
	}
	mustRecordFields(t, logged, []string{"bounded=true", "stale_temp_sweep=owner-checked", "ledger_retention_pass=skipped"})
	if strings.Contains(logged, "stale_temp_sweep=skipped") {
		t.Errorf("the bounded run still says its stale-temp sweep was skipped:\n%s", logged)
	}
}

// TestSweep_AnUnboundedSweepLeavesATempWhoseRecordedOwnerIsAlive grades AC-4 at the sweep
// itself (sweepStaleTemps): a temp whose owner record was written, through the engine's own
// code, by THIS process - which is alive, and holds the record's lock through its own
// descriptor - is left in place with no removal recorded, and so is the attached-picture
// file named after it. The last half is the anti-vacuity arm: the same owner dying makes
// the same sweep take both, so what kept them was the owner and nothing else.
func TestSweep_AnUnboundedSweepLeavesATempWhoseRecordedOwnerIsAlive(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "live.mkv")
	mustWrite(t, source)
	temp := tempPath(root, "live", "mkv", 0)
	picture := picturePath(temp, 0)

	eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
	var buf bytes.Buffer
	eng.Log = captureLogger(&buf)
	owned, err := eng.ownTemp(temp, source)
	if err != nil || owned == nil {
		t.Fatalf("take the owner record: %v", err)
	}
	writeTemp(t, temp)
	writeTemp(t, picture)

	eng.held.Store(eng.loadHoldBacks(ctx))
	eng.sweepStaleTemps(ctx, eng.passListings())
	for _, p := range []string{temp, picture} {
		if !exists(p) {
			t.Errorf("an unbounded sweep removed %s while its recorded owner is alive", p)
		}
		if removalsOf(buf.String(), p) != 0 {
			t.Errorf("an unbounded sweep recorded a removal of %s, whose owner is alive:\n%s", p, buf.String())
		}
	}

	if err := owned.f.Close(); err != nil { // the owner dies: the lock goes, the record stays
		t.Fatal(err)
	}
	eng.sweepStaleTemps(ctx, eng.passListings())
	for _, p := range []string{temp, picture} {
		if exists(p) {
			t.Errorf("the anti-vacuity arm: %s survived the same sweep once its owner was dead, "+
				"so the first half did not prove the owner kept it", p)
		}
	}
}

// TestBoundedRun_LeavesATempWhoseOwnerIsNotProvablyDeadAndNamesIt grades AC-5: every way
// a temp's owner can fail to be provably dead leaves the temp where it is under a bounded
// run, and - the temp lying in a directory the run lists - a `warn` record carries the
// temp's full path and why it was left.
//
// The liveness check is reached through its own seam, the filesystem-type lookup the
// record's storage is classified by: an error from it is the check that errors, and a
// network type is the check that cannot decide. In both of those the record itself is a
// dead owner's, which AC-2 shows IS removed on local storage - so what keeps the temp is the
// check and nothing else.
func TestBoundedRun_LeavesATempWhoseOwnerIsNotProvablyDeadAndNamesIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	writeRecordFile := func(t *testing.T, eng *Engine, temp, body string) {
		t.Helper()
		p := eng.owners.recordPath(temp)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		fsType  string
		arrange func(t *testing.T, eng *Engine, temp, source string)
		why     string
	}{
		{"no owner record, as every temp an older build wrote", "ext4",
			func(*testing.T, *Engine, string, string) {}, "no owner record"},
		{"an owner record that cannot be read", "ext4", func(t *testing.T, eng *Engine, temp, _ string) {
			if err := os.MkdirAll(eng.owners.recordPath(temp), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "could not be opened"},
		{"a malformed owner record", "ext4", func(t *testing.T, eng *Engine, temp, _ string) {
			writeRecordFile(t, eng, temp, "{not a record")
		}, "malformed"},
		{"an owner record at a format version this build does not read", "ext4", func(t *testing.T, eng *Engine, temp, source string) {
			body, err := json.Marshal(map[string]any{
				"format": tempOwnerFormat, "version": 2, "temp": temp, "source": source,
			})
			if err != nil {
				t.Fatal(err)
			}
			writeRecordFile(t, eng, temp, string(body))
		}, "format version 2"},
		{"a liveness check that errors", "", func(t *testing.T, eng *Engine, temp, source string) {
			killedOwner(t, eng, temp, source)
		}, "lookup failed"},
		{"a liveness check that cannot decide", "nfs", func(t *testing.T, eng *Engine, temp, source string) {
			killedOwner(t, eng, temp, source)
		}, "non-local (nfs)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "film.mkv")
			mkHevc(t, ffmpeg, source, "800k")
			temp := tempPath(root, "film", "mkv", 0)
			eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, c.fsType)
			var buf bytes.Buffer
			eng.Log = captureLogger(&buf)
			c.arrange(t, eng, temp, source)
			writeTemp(t, temp)
			before := md5f(t, temp)

			if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
				t.Fatalf("RunBounded(limit 1): %v", err)
			}
			logged := buf.String()
			if !exists(temp) {
				t.Fatalf("a bounded run removed %s, whose owner is not provably dead", temp)
			}
			if md5f(t, temp) != before {
				t.Errorf("a bounded run changed %s", temp)
			}
			if removalsOf(logged, temp) != 0 {
				t.Errorf("a bounded run recorded a removal of %s:\n%s", temp, logged)
			}
			named := false
			for _, line := range strings.Split(logged, "\n") {
				if strings.Contains(line, "level=WARN") && hasField(line, "file", temp) && strings.Contains(line, c.why) {
					named = true
				}
			}
			if !named {
				t.Errorf("no warn record names %s with the reason it was left (%q):\n%s", temp, c.why, logged)
			}
		})
	}
}

// TestSweeps_ADeadOwnerLicensesNothingAHoldBackKeeps grades AC-6: a temp whose recorded
// owner is provably dead is still left by BOTH sweeps, byte-identical, where either rule
// that holds at the pin applies - a live ledger record naming it as a job's replacement, or
// a finished replacement at it that no record names (strayReplacementHold).
func TestSweeps_ADeadOwnerLicensesNothingAHoldBackKeeps(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name    string
		arrange func(t *testing.T, eng *Engine, source, temp string)
	}{
		{"a live ledger record names it as a job's replacement", func(t *testing.T, eng *Engine, source, temp string) {
			mustWrite(t, source)
			writeTemp(t, temp)
			if err := eng.Store.(*testStore).RecordSwapIncident(context.Background(), store.SwapIncident{
				SourcePath: source, SourceFingerprint: "1:1", ReplacementPath: temp,
				SourceAttrs: "1:1", ReplacementAttrs: "2:2", Outcome: store.Indeterminate,
			}); err != nil {
				t.Fatalf("record incident: %v", err)
			}
		}},
		{"it holds a finished replacement no record names", func(t *testing.T, _ *Engine, source, temp string) {
			mkH264(t, ffmpeg, source, "8M")
			mkHevcFrom(t, ffmpeg, source, temp, "")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			source := filepath.Join(root, "kept", "film.mkv")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "elsewhere", "one.mkv")
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			mkHevc(t, ffmpeg, target, "800k")
			temp := tempPath(filepath.Dir(source), "film", "mkv", 0)
			eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
			c.arrange(t, eng, source, temp)
			killedOwner(t, eng, temp, source)
			before := md5f(t, temp)

			if err := eng.RunBounded(ctx, Bound{File: target}); err != nil {
				t.Fatalf("RunBounded(file): %v", err)
			}
			if !exists(temp) || md5f(t, temp) != before {
				t.Fatalf("the bounded sweep removed or changed %s, which a hold-back keeps", temp)
			}

			eng.held.Store(eng.loadHoldBacks(ctx))
			eng.sweepStaleTemps(ctx, eng.passListings())
			if !exists(temp) || md5f(t, temp) != before {
				t.Fatalf("the unbounded sweep removed or changed %s, which a hold-back keeps", temp)
			}
		})
	}
}

// TestSweep_AnUnboundedPassStillRemovesATempWithNoOwnerRecord grades AC-9 on an engine that
// keeps owner records: a whole-library pass removes a temp that has none, as every temp an
// older build wrote has none, exactly as at the pin - and a hold-back still keeps one.
func TestSweep_AnUnboundedPassStillRemovesATempWithNoOwnerRecord(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.mkv"))
	orphan := staleTemp(t, root, "old")
	parked := filepath.Join(root, "parked.mkv")
	mustWrite(t, parked)
	held := staleTemp(t, root, "parked")

	eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
	var buf bytes.Buffer
	eng.Log = captureLogger(&buf)
	if err := eng.Store.(*testStore).RecordSwapIncident(context.Background(), store.SwapIncident{
		SourcePath: parked, SourceFingerprint: "1:1", ReplacementPath: held,
		SourceAttrs: "1:1", ReplacementAttrs: "2:2", Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record incident: %v", err)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if exists(orphan) {
		t.Errorf("an unbounded pass left %s, a temp with no owner record", orphan)
	}
	if removalsOf(buf.String(), orphan) != 1 {
		t.Errorf("the removal of %s was not recorded with its path:\n%s", orphan, buf.String())
	}
	if !exists(held) {
		t.Errorf("an unbounded pass removed %s, which a live record names as a job's replacement", held)
	}
}

// TestBoundedRun_ARecordWithNothingToRemoveRemovesNothing grades AC-10: an owner record
// naming a dead owner with nothing at the temp path it names, and the records of owners
// that completed their jobs - by a swap, by a gate failure and by a graceful stop - cost a
// later bounded run nothing: it returns cleanly, removes no file and records no removal.
//
// The completed jobs run through the real ProcessFile of an engine keeping owner records,
// so each one's record is written and cleared by the code a run uses; that none is left
// behind is asserted directly, since it is what leaves a later run nothing to act on.
func TestBoundedRun_ARecordWithNothingToRemoveRemovesNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	laterRun := func(t *testing.T, eng *Engine, root, target string) {
		t.Helper()
		var buf bytes.Buffer
		eng.Log = captureLogger(&buf)
		before := treeMD5(t, root)
		if err := eng.RunBounded(context.Background(), Bound{File: target}); err != nil {
			t.Fatalf("the later bounded run: %v", err)
		}
		sameTree(t, before, treeMD5(t, root))
		if strings.Contains(buf.String(), `msg="removed an orphaned temp file"`) {
			t.Errorf("the later bounded run recorded a removal:\n%s", buf.String())
		}
	}
	fixture := func(t *testing.T) (root, target string) {
		t.Helper()
		root = t.TempDir()
		target = filepath.Join(root, "elsewhere", "one.mkv")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		mkHevc(t, ffmpeg, target, "800k")
		return root, target
	}

	t.Run("a dead owner with nothing at the temp path", func(t *testing.T) {
		root, target := fixture(t)
		source := filepath.Join(root, "killed", "film.mkv")
		mustWrite(t, source)
		mustWrite(t, filepath.Join(root, "killed", "notes.txt"))
		eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
		killedOwner(t, eng, tempPath(filepath.Dir(source), "film", "mkv", 0), source)
		laterRun(t, eng, root, target)
	})

	jobs := []struct {
		name string
		enc  func(started chan<- struct{}) Encoder
		stop bool
	}{
		{"an owner that swapped", func(chan<- struct{}) Encoder { return nil }, false},
		{"an owner whose encode a gate rejected", func(chan<- struct{}) Encoder {
			return EncoderFunc(func(_ context.Context, _, out string, _ *probe.VideoProps) error {
				return os.WriteFile(out, []byte("not a video"), 0o600)
			})
		}, false},
		{"an owner stopped gracefully mid-encode", func(started chan<- struct{}) Encoder {
			return EncoderFunc(func(ctx context.Context, _, out string, _ *probe.VideoProps) error {
				if err := os.WriteFile(out, []byte("half an encode"), 0o600); err != nil {
					return err
				}
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		}, true},
	}
	for _, j := range jobs {
		t.Run(j.name, func(t *testing.T) {
			root, target := fixture(t)
			source := filepath.Join(root, "done", "film.mkv")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			mkH264(t, ffmpeg, source, "8M")
			eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
			started := make(chan struct{})
			if enc := j.enc(started); enc != nil {
				eng.Enc = enc
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if j.stop {
				go func() { <-started; cancel() }()
			}
			eng.held.Store(eng.loadHoldBacks(ctx))
			err := eng.ProcessFile(ctx, "w0", source)
			if j.stop != (err != nil) {
				t.Fatalf("ProcessFile returned %v", err)
			}
			if n := nTemp(t, root); n != 0 {
				t.Fatalf("the job left %d temp(s) behind", n)
			}
			if records, err := eng.owners.ownerRecords(); err != nil || len(records) != 0 {
				t.Fatalf("the job left owner record(s) %v behind (%v): its owner lived through its exit", records, err)
			}
			laterRun(t, eng, root, target)
		})
	}
}

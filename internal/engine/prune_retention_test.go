package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0168: what the engine does once the startup walk prunes a directory the path filters
// exclude whole. The walk's own cases are in internal/startup; these run the REAL walk into
// the real engine, so the coverage a pass is bounded by is the one a daemon gets.

// prunedWalk runs the real startup check over cfg's roots with the path filters' prune
// decision in force - built by internal/config, exactly as the one start-or-refuse
// construction builds it - and logs what it established the way a starting daemon does.
// Only the filesystem-type lookup is substituted, for the reason walkOver gives.
func prunedWalk(t *testing.T, cfg config.Config, log *slog.Logger) startup.Result {
	t.Helper()
	res := startup.Run(startup.Check{
		Roots:       cfg.LibraryRoots,
		StateDir:    t.TempDir(),
		IsMediaFile: func(base string) bool { return IsSourceName(base, cfg.VideoExts) },
		Excluded:    cfg.DirectoryExcluded(),
		Platform:    startup.System(func(string) (string, error) { return "ext4", nil }, nil),
	})
	res.Log(log)
	if !res.Start {
		t.Fatalf("the startup check refused the fixture at row %d: %+v", res.Row, res.Causes)
	}
	return res
}

// TestS0168AC8_ARowBeneathAPrunedDirectorySurvivesRetention is S0168 [AC-8]: a terminal row
// whose file lies beneath a pruned directory is kept through the ledger retention pass, on the
// first pass after the walk and on a later one that lists for itself, whether or not the file
// is still there. The retention is as aggressive as it can be set, and the rows for files gone
// from a directory the run DID list are removed by the same passes - so the survival above is
// the rule, not a pass that pruned nothing.
func TestS0168AC8_ARowBeneathAPrunedDirectorySurvivesRetention(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	trash := filepath.Join(root, ".Trash-0")
	present := filepath.Join(trash, "Old.mkv")
	absent := filepath.Join(trash, "Gone.mkv")
	absentDeep := filepath.Join(trash, "sub", "Deeper.mkv")
	mustWrite(t, present)
	if err := os.MkdirAll(filepath.Dir(absentDeep), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 1 // aggressive on purpose: prune everything it may
		c.ExcludePaths = []string{"**/.Trash-*/**"}
	})
	eng.Log = captureLogger(&buf)
	ts := eng.Store.(*testStore)

	// The rows beneath the pruned directory sort BEFORE the gone ones, so the prune offers
	// itself every one of them and has to refuse each out loud rather than never reach it.
	seedRowForRealFile(t, ts, present, store.Done, &store.Outcome{Reason: "seeded"})
	seedRow(t, ts, absent, store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	seedRow(t, ts, absentDeep, store.Done, &store.Outcome{Reason: "seeded"})
	listedGone := []string{filepath.Join(root, "zgone0.mkv"), filepath.Join(root, "zgone1.mkv")}
	for _, p := range listedGone {
		seedRow(t, ts, p, store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	}

	res := prunedWalk(t, eng.Cfg, eng.Log)
	for _, dir := range res.Coverage {
		if dir == trash || strings.HasPrefix(dir, trash+string(filepath.Separator)) {
			t.Fatalf("the walk covered %s, so the directory was not pruned and this case proves nothing", dir)
		}
	}
	eng.SetCoverage(res.Coverage, res.Entries)

	for pass := 1; pass <= 2; pass++ {
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		held := map[string]bool{}
		for _, r := range terminalRows(t, ts) {
			held[r.Path] = true
		}
		for _, p := range []string{present, absent, absentDeep} {
			if !held[p] {
				t.Fatalf("pass %d removed the terminal row for %s, which lies beneath a pruned directory: the "+
					"retention pass concluded something about a directory this run never listed", pass, p)
			}
		}
		for _, p := range listedGone {
			if held[p] {
				t.Fatalf("pass %d kept %s, whose file is gone from a directory it listed, so the retention "+
					"pass did not run and the survival above proves nothing", pass, p)
			}
		}
	}
	// WHY they were kept: the directory was not listed - never "the file is still there",
	// which is true of only one of the three.
	if !strings.Contains(buf.String(), "kept_directory_this_run_did_not_list=3") {
		t.Fatalf("the rows were not kept as rows in a directory this run did not list:\n%s", buf.String())
	}
}

// namesPath reports whether any value of rec names dir or a path beneath it. Containment is
// enough here: no other path in these fixtures has dir as a prefix.
func namesPath(rec map[string]any, dir string) bool {
	for _, v := range rec {
		if strings.Contains(fmt.Sprint(v), dir) {
			return true
		}
	}
	return false
}

// TestS0168AC11_NoRecordSaysAnExcludedDirectoryWasListedOrWarnsAboutOne is S0168 [AC-11]:
// with the path filters keeping a directory AND a file out of a run, no log record - from the
// startup walk or from any pass - asserts that a directory a filter excludes was listed, and no
// record at warn names the pruned directory or a path beneath it (observability O3: nothing
// degraded, the operator's own filter did what it says).
//
// The pruned directory is the shape of the report that seeded this item: one the process may
// not list. Two anti-vacuity halves: the filtered-files report really is emitted, so the first
// assertion reads the record it is about; and the same library with no filter DOES warn about
// that directory, so the second is the prune and not a directory nothing ever complained of.
func TestS0168AC11_NoRecordSaysAnExcludedDirectoryWasListedOrWarnsAboutOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-000 directory is still readable, so there is nothing to deny")
	}
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	trash := filepath.Join(root, ".Trash-0")
	mustWrite(t, filepath.Join(trash, "sub", "Old.mkv"))
	// Excluded as a FILE, in a directory the run lists: the filtered-files report's case.
	mustWrite(t, filepath.Join(root, "Film.sample.mkv"))
	if err := os.Chmod(trash, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(trash, 0o755) })

	run := func(exclude []string) []map[string]any {
		var buf bytes.Buffer
		log := jsonLogger(&buf)
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ExcludePaths = exclude })
		eng.Log = log
		res := prunedWalk(t, eng.Cfg, log)
		eng.SetCoverage(res.Coverage, res.Entries)
		for pass := 1; pass <= 2; pass++ {
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
		}
		return logRecords(t, &buf)
	}

	reported := false
	for _, rec := range run([]string{"**/.Trash-*/**", "**/*.sample.*"}) {
		msg := fmt.Sprint(rec["msg"])
		for _, claim := range []string{"still listed", "would have listed", "was listed"} {
			if strings.Contains(msg, claim) {
				t.Errorf("a record asserts an excluded directory was listed: %v", rec)
			}
		}
		if lvl := fmt.Sprint(rec["level"]); (lvl == "WARN" || lvl == "ERROR") && namesPath(rec, trash) {
			t.Errorf("a %s record names the pruned directory: %v", lvl, rec)
		}
		if rec["files_excluded_by_a_path_filter"] == float64(1) {
			reported = true
		}
	}
	if !reported {
		t.Fatal("the filtered-files report was never emitted, so no record of it was read")
	}

	warned := false
	for _, rec := range run(nil) {
		if fmt.Sprint(rec["level"]) == "WARN" && namesPath(rec, trash) {
			warned = true
		}
	}
	if !warned {
		t.Fatal("with no filter the unreadable directory is not warned about either, so this case proves nothing")
	}
}

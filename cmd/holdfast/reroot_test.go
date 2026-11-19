package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast reroot`, through the real command: dispatch, the real config load, the real
// ledger and real files. A root move is staged the way an operator makes one: the ledger is
// recorded under <dir>/media/old, the tree is RENAMED to <dir>/media/new (same filesystem,
// so the same inodes, sizes and mtimes), and the configuration names only the new root.

type rerootFixture struct {
	dir, cfgPath, state, oldRoot, newRoot string
	db                                    string
}

// newRerootFixture records a ledger under the old root and moves the tree: a skipped film,
// a deterministic failure, a done row whose file has since been deleted, a retained original
// and a withheld path. Nothing here encodes.
func newRerootFixture(t *testing.T) *rerootFixture {
	t.Helper()
	return newRerootFixtureWith(t, 0)
}

// newRerootFixtureWith is newRerootFixture with extra more skipped files, movies/extra-NN.mkv,
// so a test can put a match ratio either side of a threshold.
func newRerootFixtureWith(t *testing.T, extra int) *rerootFixture {
	t.Helper()
	dir := t.TempDir()
	f := &rerootFixture{
		dir:     dir,
		state:   filepath.Join(dir, "state"),
		oldRoot: filepath.Join(dir, "media", "old"),
		newRoot: filepath.Join(dir, "media", "new"),
	}
	f.db = filepath.Join(f.state, "jobs.db")
	write := func(rel, body string) string {
		p := filepath.Join(f.oldRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	film := write("movies/film.mkv", "the film, encoded")
	ep := write("tv/Show/ep.mkv", "an episode that failed its VMAF gate")
	retained := write("movies/.holdfast-undo/film.25-1.__undo__.mkv.holdfast-undo", "the original film's bytes")

	ctx := context.Background()
	seedLedger(t, f.state, func(st *store.SQLite) {
		root := store.Decision{LibraryRoot: f.oldRoot}
		if _, err := st.RecordSkip(ctx, film, probe.Fingerprint(film), engine.SkipAlreadyTargetCodec, root, "", store.SourceFacts{}); err != nil {
			t.Fatal(err)
		}
		if ok, err := st.Claim(ctx, ep, probe.Fingerprint(ep), "seed", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if err := st.Finish(ctx, ep, probe.Fingerprint(ep), store.Failed, &store.Outcome{
			Reason: "VMAF below threshold", FailureClass: store.FailureDeterministic}, 3); err != nil {
			t.Fatal(err)
		}
		seedTerminalRow(t, st, filepath.Join(f.oldRoot, "movies", "gone.mkv"), store.Done, "", store.DecisionInputs{})
		if err := st.Retain(ctx, store.Retained{SourcePath: film, SwappedPath: film, RetainedPath: retained,
			SourceBytes: probe.FileSize(retained), SwappedFingerprint: probe.Fingerprint(film),
			RetainedAt: 1, ExpiresAt: 1 << 40}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ExcludePath(ctx, filepath.Join(f.oldRoot, "tv", "Show")); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < extra; i++ {
			p := write(fmt.Sprintf("movies/extra-%02d.mkv", i), fmt.Sprintf("extra film %d", i))
			if _, err := st.RecordSkip(ctx, p, probe.Fingerprint(p), engine.SkipLowBitrate, root, "", store.SourceFacts{}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
		t.Fatal(err)
	}
	f.cfgPath = f.config(t, f.newRoot)
	return f
}

func (f *rerootFixture) config(t *testing.T, roots ...string) string {
	t.Helper()
	body := "library_roots:\n"
	for _, r := range roots {
		body += "  - " + r + "\n"
	}
	body += "state_dir: " + f.state + "\nundo_window_hours: 24\n"
	p := filepath.Join(f.dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *rerootFixture) reroot(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	return cli(t, append([]string{"reroot", "--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot}, extra...)...)
}

// ledgerDump is every row of every table, read over a plain connection, so "nothing
// changed" compares everything the ledger holds.
func ledgerDump(t *testing.T, db string) map[string][]string {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tables, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	_ = tables.Close()
	out := map[string][]string{}
	for _, n := range names {
		rows, err := conn.Query(`SELECT * FROM ` + n + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for i, v := range vals {
				if raw, ok := v.([]byte); ok {
					v = string(raw)
				}
				fmt.Fprintf(&b, "%s=%v;", cols[i], v)
			}
			out[n] = append(out[n], b.String())
		}
		_ = rows.Close()
	}
	return out
}

// ledgerNames is every path-bearing value the ledger holds under root.
func ledgerNames(t *testing.T, db, root string) []string {
	t.Helper()
	var out []string
	for _, rows := range ledgerDump(t, db) {
		for _, r := range rows {
			if strings.Contains(r, root+"/") || strings.Contains(r, "="+root+";") {
				out = append(out, r)
			}
		}
	}
	return out
}

func TestReroot_DryRunReportsEveryTableAndTheEvidenceAndRewritesNothing(t *testing.T) {
	f := newRerootFixture(t)
	before := ledgerDump(t, f.db)

	code, out, errOut := f.reroot(t)
	if code != 0 {
		t.Fatalf("a dry run over a moved root exited %d: %s", code, errOut)
	}
	for _, want := range []string{
		"dry run, nothing is rewritten",
		"jobs                        3 row(s): path 3, target_path 0, library_root 1, subtitle_sidecars 0",
		"retained_originals          1 row(s): source_path 1, swapped_path 1, retained_path 1",
		"swap_incidents              0 row(s): source_path 0, replacement_path 0",
		"path_exclusions             1 row(s): path 1",
		"health_checks               0 row(s): path 0",
		"node_leases                 0 row(s): path 0, temp_path 0",
		"in all                      5 row(s)",
		"of 4 file(s) the ledger recorded, 3 are under " + f.newRoot + " as recorded (0 of them the same file at " +
			"both paths), 0 were replaced since (carried as recorded), 1 are at neither path (deleted since; carried " +
			"as recorded), 0 differ from or are only at the old path, and 0 retained original(s) are not the recorded size",
		"match ratio 3/3 = 1.000, threshold 0.95 (--min-match); coverage 3/4 = 0.750, threshold 0.5 (--min-coverage)",
		"--apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, out)
		}
	}
	if after := ledgerDump(t, f.db); !reflect.DeepEqual(before, after) {
		t.Error("a dry run changed the ledger")
	}
}

// TestReroot_ApplyCarriesEveryRowAndValidateFindsThemRooted: after the apply, nothing names
// the old root, every row names the new one, the evidence the scan keys on still matches,
// and `validate` no longer reports rows under no configured root.
func TestReroot_ApplyCarriesEveryRowAndValidateFindsThemRooted(t *testing.T) {
	f := newRerootFixture(t)
	if _, out, _ := cli(t, "validate", "--config", f.cfgPath); !strings.Contains(out, "holdfast reroot --from <old root> --to <new root>") {
		t.Errorf("validate does not point rows under no configured root at holdfast reroot:\n%s", out)
	}
	moved := len(ledgerNames(t, f.db, f.oldRoot))

	code, out, errOut := f.reroot(t, "--apply")
	if code != 0 {
		t.Fatalf("reroot --apply exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "rewrote 5 row(s) from "+f.oldRoot+" to "+f.newRoot+" in one transaction") {
		t.Errorf("the apply does not report what it rewrote:\n%s", out)
	}
	if left := ledgerNames(t, f.db, f.oldRoot); len(left) != 0 {
		t.Errorf("rows still name %s after the apply: %v", f.oldRoot, left)
	}
	if got := len(ledgerNames(t, f.db, f.newRoot)); got != moved {
		t.Errorf("%d row(s) name %s after the apply, want the %d that named %s", got, f.newRoot, moved, f.oldRoot)
	}

	st, err := store.OpenReadOnly(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ep := filepath.Join(f.newRoot, "tv", "Show", "ep.mkv")
	if status, _, ok, err := st.Get(context.Background(), ep, probe.Fingerprint(ep)); err != nil || !ok || status != store.Failed {
		t.Errorf("the deterministic failure is not found at its new path under the file's own fingerprint: %v %v %v", status, ok, err)
	}
	if ok, err := st.PathIsExcluded(context.Background(), filepath.Join(f.newRoot, "tv", "Show")); err != nil || !ok {
		t.Errorf("the withheld path was not carried: %v %v", ok, err)
	}

	if _, out, _ := cli(t, "validate", "--config", f.cfgPath); strings.Contains(out, "lie under no configured library root") {
		t.Errorf("validate still reports rows under no configured root after the reroot:\n%s", out)
	}
}

// TestReroot_Refusals: every refusal exits non-zero, names its reason, and leaves the ledger
// exactly as it was. @OLD@ and @NEW@ in what a refusal says stand for the fixture's roots.
func TestReroot_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *rerootFixture) []string // the reroot argv after "reroot"
		code  int
		says  []string
	}{
		{"--to is not a configured root", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", filepath.Join(f.dir, "media", "other")}
		}, 1, []string{"is not a configured library root"}},
		{"--from is a configured root", func(t *testing.T, f *rerootFixture) []string {
			if err := os.MkdirAll(f.oldRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			f.cfgPath = f.config(t, f.newRoot, f.oldRoot)
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot}
		}, 1, []string{"overlaps the configured library root @OLD@"}},
		{"--from contains a configured root", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", filepath.Join(f.dir, "media"), "--to", f.newRoot}
		}, 1, []string{"overlaps the configured library root"}},
		{"--from lies inside a configured root", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", filepath.Join(f.newRoot, "movies"), "--to", f.newRoot}
		}, 1, []string{"overlaps the configured library root"}},
		{"a relative --from", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", "media/old", "--to", f.newRoot}
		}, 1, []string{"is not an absolute path"}},
		{"no --to", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot}
		}, 2, []string{"needs both --from and --to"}},
		{"nothing recorded under --from", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", filepath.Join(f.dir, "elsewhere"), "--to", f.newRoot}
		}, 1, []string{"records nothing at or under"}},
		{"a collision at the new path", func(t *testing.T, f *rerootFixture) []string {
			film := filepath.Join(f.newRoot, "movies", "film.mkv")
			seedLedger(t, f.state, func(st *store.SQLite) {
				if _, err := st.RecordSkip(context.Background(), film, probe.Fingerprint(film), engine.SkipLowBitrate,
					store.Decision{}, "", store.SourceFacts{}); err != nil {
					t.Fatal(err)
				}
			})
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"collision: jobs " + "@NEW@/movies/film.mkv (fingerprint ", "is already recorded", "Nothing is merged"}},
		{"the files are still under --from and not under --to", func(t *testing.T, f *rerootFixture) []string {
			// The move never happened: the old tree is in place and the new root is empty.
			if err := os.Rename(f.newRoot, f.oldRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.newRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"not carried: @NEW@/tv/Show/ep.mkv - the ledger recorded ", "and it is not there, and still at @OLD@/tv/Show/ep.mkv",
			"@OLD@ is still live and does not agree with @NEW@: 0 recorded file(s) are a different file at each path, and 3 are still under"}},
		{"no recorded file is anywhere", func(t *testing.T, f *rerootFixture) []string {
			if err := os.RemoveAll(f.newRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.newRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"none of the 4 file(s) the ledger recorded under", "(0 found replaced, 4 gone)", "nothing shows"}},
		{"too few of the files under --to are the ones recorded", func(t *testing.T, f *rerootFixture) []string {
			ep := filepath.Join(f.newRoot, "tv", "Show", "ep.mkv")
			if err := os.WriteFile(ep, []byte("a different file at the same name"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"only 2 of the 3 recorded file(s) found under @NEW@ match", "a match ratio of 0.667",
			"under the --min-match threshold of 0.95", "is not shown to be the same files"}},
		{"a retained original that is not the recorded size, whatever the ratio", func(t *testing.T, f *rerootFixture) []string {
			retained := filepath.Join(f.newRoot, "movies", ".holdfast-undo", "film.25-1.__undo__.mkv.holdfast-undo")
			if err := os.WriteFile(retained, []byte("short"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--min-match", "0.1", "--apply"}
		}, 1, []string{"retained original not the one recorded: @NEW@/movies/.holdfast-undo/film.25-1.__undo__.mkv.holdfast-undo" +
			" - the ledger recorded a retained original of 25 bytes, and it is 5 bytes",
			"1 retained original(s) under @NEW@ are not the size of the original their retention recorded"}},
		{"--min-match of 0", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--min-match", "0"}
		}, 2, []string{"--min-match 0 is not a share"}},
		{"--min-match above 1", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--min-match", "1.5"}
		}, 2, []string{"--min-match 1.5 is not a share"}},
		{"--min-match NaN", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--min-match", "NaN"}
		}, 2, []string{"is not a share"}},
		{"--min-coverage of 0", func(t *testing.T, f *rerootFixture) []string {
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--min-coverage", "0"}
		}, 2, []string{"--min-coverage 0 is not a share"}},
		{"a dry run over a ledger an earlier build wrote", func(t *testing.T, f *rerootFixture) []string {
			setUserVersion(t, f.db, userVersion(t, f.db)-1)
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot}
		}, 1, []string{"the ledger's schema is older than this build's", "does not upgrade the ledger", "With --apply"}},
		{"the rewrite fails part way", func(t *testing.T, f *rerootFixture) []string {
			conn, err := sql.Open("sqlite", "file:"+f.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(`CREATE TRIGGER fail_midway BEFORE UPDATE OF path ON path_exclusions
				BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"the rewrite was rolled back", "injected failure"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRerootFixture(t)
			args := c.setup(t, f)
			before := ledgerDump(t, f.db)
			code, _, errOut := cli(t, append([]string{"reroot"}, args...)...)
			if code != c.code {
				t.Errorf("exited %d, want %d: %s", code, c.code, errOut)
			}
			for _, want := range c.says {
				want = strings.ReplaceAll(want, "@OLD@", f.oldRoot)
				want = strings.ReplaceAll(want, "@NEW@", f.newRoot)
				if !strings.Contains(errOut, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, errOut)
				}
			}
			if code == 1 && !strings.Contains(errOut, "nothing was rewritten") {
				t.Errorf("the refusal does not say nothing was rewritten:\n%s", errOut)
			}
			if after := ledgerDump(t, f.db); !reflect.DeepEqual(before, after) {
				t.Errorf("a refused reroot changed the ledger:\n before %v\n after  %v", before, after)
			}
		})
	}
}

// TestReroot_RefusedWhileAnotherProcessHoldsTheLedger runs the reroot in a REAL child
// process while this one holds the ledger open the daemon's way, idle: the refusal rests on
// the lock SQLite takes between processes, not on anything one process remembers.
func TestReroot_RefusedWhileAnotherProcessHoldsTheLedger(t *testing.T) {
	f := newRerootFixture(t)
	daemon, err := store.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Summary(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, out := cliProcess(t, "reroot", "--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply")
	if code != 1 || !strings.Contains(out, "is open in another process") {
		t.Errorf("reroot beside an open daemon exited %d, want 1 naming the other process:\n%s", code, out)
	}
	if _, err := daemon.Summary(context.Background()); err != nil {
		t.Errorf("the refused reroot disturbed the daemon's handle: %v", err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	if left := ledgerNames(t, f.db, f.oldRoot); len(left) == 0 {
		t.Error("the refused reroot rewrote the ledger")
	}
	// And with the daemon stopped, the same command goes through.
	if code, out := cliProcess(t, "reroot", "--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"); code != 0 {
		t.Errorf("reroot with the daemon stopped exited %d:\n%s", code, out)
	}
}

// TestReroot_LogsWhatItDidAsRecords: one record per table and one for the whole, every
// figure an attribute; a refusal is one record naming its reason token.
func TestReroot_LogsWhatItDidAsRecords(t *testing.T) {
	f := newRerootFixture(t)
	cfg := loadConfigFile(t, f.cfgPath)
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req := rerootRequest{From: filepath.Join(f.dir, "elsewhere"), To: f.newRoot, MinMatch: rerootMinMatch,
		MinCoverage: rerootMinCoverage}
	if code := runReroot(context.Background(), log, cfg, req, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("a reroot of nothing exited %d, want 1", code)
	}
	recs := logRecords(t, &buf)
	if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["reason"] != "nothing-recorded" ||
		recs[0]["component"] != rerootComponent {
		t.Errorf("a refusal is not one WARN record naming reason=nothing-recorded: %v", recs)
	}

	buf.Reset()
	req = rerootRequest{From: f.oldRoot, To: f.newRoot, Apply: true, MinMatch: rerootMinMatch,
		MinCoverage: rerootMinCoverage}
	if code := runReroot(context.Background(), log, cfg, req, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("reroot --apply exited %d", code)
	}
	recs = logRecords(t, &buf)
	byTable := map[string]map[string]any{}
	var whole map[string]any
	for _, r := range recs {
		if r["component"] != rerootComponent || r["msg"] != "ledger reroot applied" || r["applied"] != true {
			t.Errorf("a record that is not an applied reroot record: %v", r)
			continue
		}
		if tb, ok := r["table"].(string); ok {
			byTable[tb] = r
		} else {
			whole = r
		}
	}
	jobs := byTable["jobs"]
	if jobs == nil || jobs["rows"] != float64(3) || jobs["columns"].(map[string]any)["path"] != float64(3) {
		t.Errorf("the jobs record does not carry its rows and per-column counts: %v", jobs)
	}
	if len(byTable) != 6 {
		t.Errorf("%d table record(s), want one for each of the 6 tables holding a path: %v", len(byTable), byTable)
	}
	if whole == nil || whole["rows"] != float64(5) || whole["evidence_matched"] != float64(3) ||
		whole["evidence_replaced"] != float64(0) || whole["evidence_absent"] != float64(1) ||
		whole["evidence_retained_mismatched"] != float64(0) || whole["match_ratio"] != float64(1) ||
		whole["min_match"] != rerootMinMatch || whole["coverage"] != 0.75 || whole["min_coverage"] != rerootMinCoverage ||
		whole["evidence_same_file"] != float64(0) || whole["from"] != f.oldRoot || whole["to"] != f.newRoot {
		t.Errorf("the summary record does not carry the rows and the evidence: %v", whole)
	}
}

// TestReroot_RestoreReachesARetainedOriginalRecordedUnderTheOldRoot is the end-to-end case:
// a real run swaps a file and retains its original under the old root, the tree moves, and
// until the reroot `restore` refuses - KEEPING the record - and afterwards it puts the
// original back, byte for byte, at its new path.
func TestReroot_RestoreReachesARetainedOriginalRecordedUnderTheOldRoot(t *testing.T) {
	cfgPath, lib, state, src := undoLibrary(t, 24, "")
	original := sha256File(t, src)
	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	if sha256File(t, src) == original {
		t.Fatal("the swap did not happen, so there is nothing to restore - the fixture proves nothing")
	}

	// The move: the same tree, renamed, and a configuration naming only its new path.
	dir := filepath.Dir(lib)
	newLib := filepath.Join(dir, "media-new")
	if err := os.Rename(lib, newLib); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(body), "  - "+lib+"\n", "  - "+newLib+"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(newLib, filepath.Base(src))

	code, _, errOut := cli(t, "restore", "--config", cfgPath, src)
	if code == 0 || !strings.Contains(errOut, "holdfast reroot") || !strings.Contains(errOut, "the record is kept") {
		t.Fatalf("restore under the old path before a reroot exited %d, want a refusal naming holdfast reroot: %s", code, errOut)
	}
	if _, out, _ := cli(t, "restore", "--config", cfgPath); !strings.Contains(out, "under no configured library root") {
		t.Errorf("the restore list does not say its record lies under no configured root:\n%s", out)
	}

	if code, out, errOut := cli(t, "reroot", "--config", cfgPath, "--from", lib, "--to", newLib, "--apply"); code != 0 {
		t.Fatalf("reroot exited %d: %s%s", code, out, errOut)
	}
	code, out, errOut := cli(t, "restore", "--config", cfgPath, moved)
	if code != 0 {
		t.Fatalf("restore at the new path after the reroot exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, moved) {
		t.Errorf("the restore does not name the file it restored: %s", out)
	}
	if got := sha256File(t, moved); got != original {
		t.Errorf("the restored file is not the pre-swap source")
	}
	st := openStore(t, state)
	rec, ok, err := st.GetRetained(context.Background(), moved)
	if err != nil || !ok || rec.RestoredAt == nil {
		t.Errorf("the ledger does not record the restore at the new path: %+v %v %v", rec, ok, err)
	}
}

// TestReroot_TheReleaseKeepsTheRecordUntilTheRerootAndThenReachesTheFile: the undo window's
// release runs at the start of every pass. Before the reroot the expired retention's record
// names a path under no configured root and is KEPT (not dropped as unrestorable, which
// would orphan the original for good); after it, the release finds the retained original at
// the new path and returns it.
func TestReroot_TheReleaseKeepsTheRecordUntilTheRerootAndThenReachesTheFile(t *testing.T) {
	f := newRerootFixture(t)
	cfg := loadConfigFile(t, f.cfgPath)
	oldFilm := filepath.Join(f.oldRoot, "movies", "film.mkv")
	retainedNew := filepath.Join(f.newRoot, "movies", ".holdfast-undo", "film.25-1.__undo__.mkv.holdfast-undo")

	release := func() (engine.ReleaseReport, []store.Retained) {
		t.Helper()
		st, err := store.Open(f.db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		rep := engine.NewUndoWindow(*cfg, st, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))).ReleaseExpired(context.Background())
		rows, err := st.ListRetained(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return rep, rows
	}
	seedLedger(t, f.state, func(st *store.SQLite) {
		r, ok, err := st.GetRetained(context.Background(), oldFilm)
		if err != nil || !ok {
			t.Fatalf("the fixture's retention: %v %v", ok, err)
		}
		r.ExpiresAt = 1 // long past
		if err := st.Retain(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	})

	rep, rows := release()
	if rep.Unrooted != 1 || rep.Unrestorable != 0 || len(rows) != 1 {
		t.Fatalf("before the reroot the release reported %+v and left %d record(s); want the one record kept as unrooted", rep, len(rows))
	}
	if _, err := os.Stat(retainedNew); err != nil {
		t.Fatalf("the retained original at the new path is gone before the reroot: %v", err)
	}

	if code, _, errOut := f.reroot(t, "--apply"); code != 0 {
		t.Fatalf("reroot --apply exited %d: %s", code, errOut)
	}
	rep, rows = release()
	if rep.Released != 1 || len(rows) != 0 {
		t.Errorf("after the reroot the release reported %+v and left %d record(s); want it released", rep, len(rows))
	}
	if _, err := os.Stat(retainedNew); !os.IsNotExist(err) {
		t.Errorf("the release did not remove the retained original at the new path (stat: %v)", err)
	}
}

// TestReroot_FilesReplacedSinceTheMoveAreCarriedAsRecorded is the dominance rule, both sides
// of the default threshold: 33 recorded files exist under the new root. One replaced since
// (an upgrade under the same name) is a match ratio of 32/33, which carries every row - the
// replaced file's row AS RECORDED, under its old fingerprint. Two replaced is 31/33, under
// 0.95, and refuses.
//
// The carried row is history and nothing more: a row is keyed by path AND fingerprint, so
// it collides with nothing, and the next scan claims the replaced file as the new file it is.
func TestReroot_FilesReplacedSinceTheMoveAreCarriedAsRecorded(t *testing.T) {
	replace := func(t *testing.T, f *rerootFixture, rel string) (string, string) {
		t.Helper()
		p := filepath.Join(f.newRoot, rel)
		was := probe.Fingerprint(p)
		if err := os.WriteFile(p, []byte("an upgraded release under the same name"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p, was
	}

	t.Run("under the threshold the rows are carried", func(t *testing.T) {
		f := newRerootFixtureWith(t, 30)
		ep, was := replace(t, f, filepath.Join("tv", "Show", "ep.mkv"))

		code, out, errOut := f.reroot(t, "--apply")
		if code != 0 {
			t.Fatalf("a reroot with 1 of 33 files replaced exited %d: %s", code, errOut)
		}
		for _, want := range []string{
			"32 are under " + f.newRoot + " as recorded (0 of them the same file at both paths), 1 were replaced since (carried as recorded)",
			"match ratio 32/33 = 0.970, threshold 0.95 (--min-match); coverage 32/34 = 0.941",
			"replaced since (carried as recorded): " + ep + " - the ledger recorded " + was + ", and it is " + probe.Fingerprint(ep),
			"rewrote 35 row(s)",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("the reroot does not say %q:\n%s", want, out)
			}
		}
		if left := ledgerNames(t, f.db, f.oldRoot); len(left) != 0 {
			t.Errorf("rows still name %s: %v", f.oldRoot, left)
		}

		st, err := store.Open(f.db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		ctx := context.Background()
		if status, _, ok, err := st.Get(ctx, ep, was); err != nil || !ok || status != store.Failed {
			t.Errorf("the replaced file's row was not carried as recorded (path %s, fingerprint %s): %v %v %v", ep, was, status, ok, err)
		}
		now := probe.Fingerprint(ep)
		if _, _, ok, err := st.Get(ctx, ep, now); err != nil || ok {
			t.Fatalf("a row already holds the replaced file's own key: %v %v", ok, err)
		}
		if held, err := st.TerminalHolds(ctx, ep, store.DecisionInputs{}); err != nil || anyEquals(held, now) {
			t.Errorf("the carried row holds the replaced file out of the pipeline: %v %v", held, err)
		}
		if ok, err := st.Claim(ctx, ep, now, "w0", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Errorf("the next scan cannot claim the replaced file as the new file it is: %v %v", ok, err)
		}
	})

	t.Run("over the threshold it refuses", func(t *testing.T) {
		f := newRerootFixtureWith(t, 30)
		replace(t, f, filepath.Join("tv", "Show", "ep.mkv"))
		replace(t, f, filepath.Join("movies", "extra-07.mkv"))
		before := ledgerDump(t, f.db)

		code, out, errOut := f.reroot(t, "--apply")
		if code != 1 || !strings.Contains(errOut, "a match ratio of 0.939, under the --min-match threshold of 0.95") {
			t.Errorf("a reroot with 2 of 33 files replaced exited %d, want 1 naming the ratio and the threshold: %s", code, errOut)
		}
		if !strings.Contains(out, "match ratio 31/33 = 0.939") {
			t.Errorf("the report does not state the ratio:\n%s", out)
		}
		if after := ledgerDump(t, f.db); !reflect.DeepEqual(before, after) {
			t.Error("a refused reroot changed the ledger")
		}
		// And the threshold is the operator's to set.
		if code, _, errOut := f.reroot(t, "--min-match", "0.9", "--apply"); code != 0 {
			t.Errorf("the same reroot under --min-match 0.9 exited %d: %s", code, errOut)
		}
	})
}

func userVersion(t *testing.T, db string) int {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var v int
	if err := conn.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// setUserVersion stamps the ledger as an earlier build's. Only the dry run's refusal reads
// it here, so the shape behind the number does not matter.
func setUserVersion(t *testing.T, db string, v int) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatal(err)
	}
}

// moveBack puts the tree back at the old root, as if the move never happened, and leaves
// the new root empty.
func moveBack(t *testing.T, f *rerootFixture) {
	t.Helper()
	if err := os.Rename(f.newRoot, f.oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
}

// copyRecorded copies one file from the old tree to the same place under the new one, with
// its modification time, so its size and mtime are exactly what the ledger recorded.
func copyRecorded(t *testing.T, f *rerootFixture, rel string) {
	t.Helper()
	src, dst := filepath.Join(f.oldRoot, rel), filepath.Join(f.newRoot, rel)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dst, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// TestReroot_ACopyIsNotAMove is the reviewer's case: the old tree left in place and a copy of
// one recorded file under --to. Presence alone would pass it (1 matched, the rest gone) and
// carry a retained original's record to a path where nothing is, orphaning the original
// still under --from. Read at both paths it is a copy - two files, two identities - and the
// rest are still under --from, so it refuses and changes nothing.
func TestReroot_ACopyIsNotAMove(t *testing.T) {
	f := newRerootFixture(t)
	moveBack(t, f)
	copyRecorded(t, f, filepath.Join("movies", "film.mkv"))
	before := ledgerDump(t, f.db)

	code, out, errOut := f.reroot(t, "--apply")
	if code != 1 {
		t.Fatalf("a reroot onto a copy exited %d, want 1:\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"not carried: " + filepath.Join(f.newRoot, "movies", "film.mkv") + " - the ledger recorded ",
		"and it is a different file from the one still at " + filepath.Join(f.oldRoot, "movies", "film.mkv"),
		"not carried: " + filepath.Join(f.newRoot, "movies", ".holdfast-undo", "film.25-1.__undo__.mkv.holdfast-undo"),
		"1 recorded file(s) are a different file at each path, and 2 are still under " + f.oldRoot + " but not under " + f.newRoot,
		"This is a copy or a partial one, or the wrong --to, not a move",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, errOut)
		}
	}
	if after := ledgerDump(t, f.db); !reflect.DeepEqual(before, after) {
		t.Error("a refused reroot changed the ledger")
	}
}

// TestReroot_TheOldMountStillInPlace: a host can have the old root still mounted as a second
// mount of the same filesystem. Every file then has ONE identity at both paths, which is the
// strongest evidence there is, and the reroot carries the rows. A test cannot mount
// unprivileged, so the second mount is a hard link of every file: the same device and inode
// at both paths, which is exactly what two mounts of one filesystem show.
func TestReroot_TheOldMountStillInPlace(t *testing.T) {
	f := newRerootFixture(t)
	err := filepath.WalkDir(f.newRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.newRoot, p)
		old := filepath.Join(f.oldRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(old, 0o755)
		}
		return os.Link(p, old)
	})
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := f.reroot(t, "--apply")
	if code != 0 {
		t.Fatalf("a reroot with the old mount still in place exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "3 are under "+f.newRoot+" as recorded (3 of them the same file at both paths)") {
		t.Errorf("the report does not say the files are the same file at both paths:\n%s", out)
	}
	if left := ledgerNames(t, f.db, f.oldRoot); len(left) != 0 {
		t.Errorf("rows still name %s: %v", f.oldRoot, left)
	}
}

// TestReroot_ASparseTreeIsNotTheLibrary: with the old root unmounted, a tree that holds a few
// recorded files and lacks the rest has a perfect match ratio and is still not shown to be
// the library that moved. The coverage floor - matched over every recorded file, deleted ones
// included - refuses it, and --min-coverage sets it.
func TestReroot_ASparseTreeIsNotTheLibrary(t *testing.T) {
	f := newRerootFixtureWith(t, 30)
	for i := 0; i < 25; i++ {
		if err := os.Remove(filepath.Join(f.newRoot, "movies", fmt.Sprintf("extra-%02d.mkv", i))); err != nil {
			t.Fatal(err)
		}
	}
	before := ledgerDump(t, f.db)

	code, out, errOut := f.reroot(t, "--apply")
	if code != 1 || !strings.Contains(errOut, "only 8 of the 34 file(s) the ledger recorded under "+f.oldRoot+
		" are under "+f.newRoot+" as recorded (a coverage of 0.235, under the --min-coverage threshold of 0.5)") {
		t.Errorf("a sparse tree exited %d, want 1 naming the coverage and the threshold:\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "match ratio 8/8 = 1.000") {
		t.Errorf("the case is not about the match ratio, which should be perfect:\n%s", out)
	}
	if after := ledgerDump(t, f.db); !reflect.DeepEqual(before, after) {
		t.Error("a refused reroot changed the ledger")
	}
	if code, _, errOut := f.reroot(t, "--min-coverage", "0.2", "--apply"); code != 0 {
		t.Errorf("the same reroot under --min-coverage 0.2 exited %d: %s", code, errOut)
	}
}

// TestReroot_ADryRunNeverUpgradesTheLedger: a dry run over a ledger an earlier build wrote
// refuses rather than migrate it, so an older daemon can still open its ledger afterwards.
func TestReroot_ADryRunNeverUpgradesTheLedger(t *testing.T) {
	f := newRerootFixture(t)
	behind := userVersion(t, f.db) - 1
	setUserVersion(t, f.db, behind)

	code, _, errOut := f.reroot(t)
	if code != 1 || !strings.Contains(errOut, "does not upgrade the ledger") {
		t.Errorf("a dry run over an older ledger exited %d, want 1 refusing to upgrade it: %s", code, errOut)
	}
	if got := userVersion(t, f.db); got != behind {
		t.Errorf("the dry run moved the ledger's schema version to %d, want it left at %d", got, behind)
	}
}

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
		"of 4 file(s) the ledger recorded, 3 exist under " + f.newRoot + " and match what it recorded, 0 do not match, " +
			"and 1 no longer exist",
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
		{"no recorded file exists under --to", func(t *testing.T, f *rerootFixture) []string {
			// The rename never happened: the new root is an empty tree.
			if err := os.Rename(f.newRoot, f.oldRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(f.newRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"none of the 4 file(s) the ledger recorded under", "nothing shows"}},
		{"a file under --to is not the one recorded", func(t *testing.T, f *rerootFixture) []string {
			ep := filepath.Join(f.newRoot, "tv", "Show", "ep.mkv")
			if err := os.WriteFile(ep, []byte("a different file at the same name"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"--config", f.cfgPath, "--from", f.oldRoot, "--to", f.newRoot, "--apply"}
		}, 1, []string{"mismatch: @NEW@/tv/Show/ep.mkv - the ledger recorded ", "1 of the 3 file(s)", "is not shown to be the same files"}},
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

	req := rerootRequest{From: filepath.Join(f.dir, "elsewhere"), To: f.newRoot}
	if code := runReroot(context.Background(), log, cfg, req, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("a reroot of nothing exited %d, want 1", code)
	}
	recs := logRecords(t, &buf)
	if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["reason"] != "nothing-recorded" ||
		recs[0]["component"] != rerootComponent {
		t.Errorf("a refusal is not one WARN record naming reason=nothing-recorded: %v", recs)
	}

	buf.Reset()
	req = rerootRequest{From: f.oldRoot, To: f.newRoot, Apply: true}
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
		whole["evidence_absent"] != float64(1) || whole["from"] != f.oldRoot || whole["to"] != f.newRoot {
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// `holdfast reroot`'s ledger half: which columns hold a path, what a plan reports, and the
// one-transaction rewrite. The fixtures use synthetic roots, /media/old and /media/new.

const (
	oldRoot = "/media/old"
	newRoot = "/media/new"
)

// TestRerootPath_ComparesPathComponentsNotStrings is the boundary rule, and the SQL range
// that narrows each read must agree with it: every case is also seeded as a jobs row, and
// the plan must pick exactly the rows RerootPath does. The names around the boundary are
// the bytes either side of '/' and the two LIKE wildcards, so a plan that used LIKE or a
// bare string prefix reds here.
func TestRerootPath_ComparesPathComponentsNotStrings(t *testing.T) {
	cases := []struct {
		path string
		want string // "" when the path is not in the tree
	}{
		{"/media/old", "/media/new"},
		{"/media/old/film.mkv", "/media/new/film.mkv"},
		{"/media/old/tv/Show/Season 1/ep.mkv", "/media/new/tv/Show/Season 1/ep.mkv"},
		{"/media/old/.holdfast-undo/a.1-2.__undo__.mkv.holdfast-undo", "/media/new/.holdfast-undo/a.1-2.__undo__.mkv.holdfast-undo"},
		{"/media/old//double.mkv", "/media/new//double.mkv"},
		{"/media/old2/film.mkv", ""},
		{"/media/older", ""},
		{"/media/ol", ""},
		{"/media", ""},
		{"/media/old.mkv", ""},
		{"/media/old-x/film.mkv", ""},
		{"/media/old0/film.mkv", ""},
		{"/media/old_/film.mkv", ""},
		{"/media/old%/film.mkv", ""},
		{"/media/Old/film.mkv", ""},
		{"/other/media/old/film.mkv", ""},
	}
	s := openTest(t)
	ctx := context.Background()
	var want []string
	for _, c := range cases {
		got, ok := RerootPath(c.path, oldRoot, newRoot)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("RerootPath(%q) = %q, %v; want %q", c.path, got, ok, c.want)
		}
		seedJob(t, s, c.path, "1:1")
		if c.want != "" {
			want = append(want, c.want)
		}
	}

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	if got := countOf(plan, "jobs", "path"); got != int64(len(want)) {
		t.Errorf("the plan rewrites %d jobs.path value(s), want %d", got, len(want))
	}
	if _, err := s.ApplyReroot(ctx, plan); err != nil {
		t.Fatalf("ApplyReroot: %v", err)
	}
	got := map[string]bool{}
	for _, row := range dumpTable(t, s, "jobs") {
		got[row["path"].(string)] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s is not in the ledger after the reroot", w)
		}
	}
	for _, c := range cases {
		if c.want == "" && !got[c.path] {
			t.Errorf("%s lies outside %s and was rewritten anyway", c.path, oldRoot)
		}
	}
}

// TestCheckRerootPaths_RefusesAPairTheRewriteIsNotDefinedOver: every refusal is taken
// before anything is read, and PlanReroot and ApplyReroot both take it.
func TestCheckRerootPaths_RefusesAPairTheRewriteIsNotDefinedOver(t *testing.T) {
	cases := []struct{ name, from, to, says string }{
		{"a relative from", "media/old", newRoot, "absolute"},
		{"a relative to", oldRoot, "new", "absolute"},
		{"an unclean from", "/media/old/", newRoot, "clean"},
		{"an unclean to", oldRoot, "/media/./new", "clean"},
		{"the filesystem root", "/", newRoot, "filesystem root"},
		{"to inside from", "/media", "/media/new", "inside"},
		{"from inside to", "/media/new/old", "/media/new", "inside"},
		{"the same tree", oldRoot, oldRoot, "inside"},
		{"not UTF-8", "/media/\xff", newRoot, "UTF-8"},
	}
	s := openTest(t)
	seedJob(t, s, "/media/old/film.mkv", "1:1")
	before := dumpLedger(t, s)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.PlanReroot(context.Background(), c.from, c.to)
			if !errors.Is(err, ErrRerootPaths) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("PlanReroot(%q, %q) = %v, want ErrRerootPaths saying %q", c.from, c.to, err, c.says)
			}
			_, err = s.ApplyReroot(context.Background(), RerootPlan{From: c.from, To: c.to})
			if !errors.Is(err, ErrRerootPaths) {
				t.Errorf("ApplyReroot(%q, %q) = %v, want ErrRerootPaths", c.from, c.to, err)
			}
		})
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Error("a refused reroot changed the ledger")
	}
}

// TestRerootColumns_EveryColumnIsClassified reads the SCHEMA, not the list: every column
// of every table is either a path the reroot rewrites or one named here as holding none.
// A migration that adds a column fails this until someone decides which it is, so the
// reroot cannot silently leave a new path column under the old root.
func TestRerootColumns_EveryColumnIsClassified(t *testing.T) {
	notPaths := map[string]bool{}
	for _, c := range []string{
		// jobs: identity, state, the proof and the recorded inputs. reason is free text and
		// keeps the words it was written with (see rerootTables).
		"jobs.fingerprint", "jobs.status", "jobs.fail_count", "jobs.worker", "jobs.updated_at", "jobs.reason",
		"jobs.encoder", "jobs.vmaf_mean", "jobs.vmaf_min", "jobs.vmaf_model", "jobs.source_bytes",
		"jobs.output_bytes", "jobs.encode_ms", "jobs.vmaf_pix_fmt", "jobs.vmaf_chroma", "jobs.vmaf_chroma_metric",
		"jobs.guard_attributes", "jobs.guard_time_resolution", "jobs.guard_residual_window", "jobs.swap_cause",
		"jobs.source_codec", "jobs.failure_class", "jobs.decision_inputs", "jobs.vmaf_stream",
		"jobs.profile_digest", "jobs.schema_version", "jobs.profile", "jobs.dropped_streams",
		"jobs.selection_not_applied", "jobs.vmaf_skipped", "jobs.source_width", "jobs.source_height",
		"jobs.output_width", "jobs.output_height", "jobs.deinterlaced", "jobs.deinterlace_filter",
		"jobs.downscaled", "jobs.downscale_scaler", "jobs.vmaf_scored_width", "jobs.vmaf_scored_height",
		"jobs.audio_tracks", "jobs.crop",
		"retained_originals.source_bytes", "retained_originals.swapped_fingerprint",
		"retained_originals.retained_at", "retained_originals.expires_at", "retained_originals.restored_at",
		"retained_originals.schema_version",
		"ledger_totals.id", "ledger_totals.reclaimed_pruned",
		// swap_incidents: the attribute records and the outcome; swap_error and
		// removal_error are free text.
		"swap_incidents.id", "swap_incidents.source_fingerprint", "swap_incidents.source_attrs",
		"swap_incidents.replacement_attrs", "swap_incidents.observed_attrs", "swap_incidents.outcome",
		"swap_incidents.swap_error", "swap_incidents.swap_cause", "swap_incidents.storage_class",
		"swap_incidents.storage_type", "swap_incidents.created_at", "swap_incidents.resolution",
		"swap_incidents.resolved_by", "swap_incidents.resolved_at", "swap_incidents.observed_source",
		"swap_incidents.observed_replacement", "swap_incidents.disposition_source",
		"swap_incidents.disposition_replacement", "swap_incidents.removal_error", "swap_incidents.schema_version",
		"sqlite_sequence.name", "sqlite_sequence.seq",
		"path_exclusions.created_at", "path_exclusions.schema_version",
		"health_sweeps.id", "health_sweeps.started_at", "health_sweeps.finished_at", "health_sweeps.ok_count",
		"health_sweeps.corrupt_count", "health_sweeps.unreadable_count", "health_sweeps.schema_version",
		"health_checks.sweep_id", "health_checks.size", "health_checks.mtime_ns", "health_checks.checked_at",
		"health_checks.result", "health_checks.reason", "health_checks.schema_version",
		// node_leases: job_key is the store key, the fingerprint.
		"node_leases.id", "node_leases.job_key", "node_leases.node", "node_leases.epoch", "node_leases.state",
		"node_leases.expires_at", "node_leases.reserved_bytes", "node_leases.args_digest",
		"node_leases.source_size", "node_leases.source_mtime_ns", "node_leases.output_digest",
		"node_leases.output_bytes", "node_leases.source_digest", "node_leases.upload_attempts",
		"node_leases.reason", "node_leases.granted_at", "node_leases.updated_at", "node_leases.ended_at",
		"node_leases.schema_version",
		// The playback evidence: the identity of a swap and of a session beside the path, the
		// readings a session carries, and the client's own names for itself. last_failure is
		// a failure class, not a path.
		"playback_swaps.fingerprint", "playback_swaps.swapped_at", "playback_swaps.superseded_at",
		"playback_swaps.recorded_at", "playback_swaps.schema_version",
		"playback_sessions.source", "playback_sessions.session_id", "playback_sessions.fingerprint",
		"playback_sessions.started_at", "playback_sessions.stopped_at", "playback_sessions.offset_ms",
		"playback_sessions.offset_derived", "playback_sessions.duration_ms", "playback_sessions.played_seconds",
		"playback_sessions.client", "playback_sessions.platform", "playback_sessions.player",
		"playback_sessions.decision", "playback_sessions.recorded_at", "playback_sessions.schema_version",
		"playback_polls.source", "playback_polls.cursor", "playback_polls.covered_through",
		"playback_polls.last_attempt_at", "playback_polls.last_failure", "playback_polls.unresolved_rows",
		"playback_polls.schema_version",
	} {
		notPaths[c] = true
	}
	paths := map[string]bool{}
	for _, c := range RerootColumns() {
		paths[c.String()] = true
	}

	s := openTest(t)
	seen := map[string]bool{}
	for _, table := range tableNames(t, s.db) {
		for _, col := range columnNames(t, s.db, table) {
			name := table + "." + col
			seen[name] = true
			switch {
			case paths[name] && notPaths[name]:
				t.Errorf("%s is listed both as a path the reroot rewrites and as one holding no path", name)
			case !paths[name] && !notPaths[name]:
				t.Errorf("%s is in neither store.rerootTables nor this test's list of columns holding no path: "+
					"decide whether it can hold a library path, and add it to one of them", name)
			}
		}
	}
	for name := range paths {
		if !seen[name] {
			t.Errorf("the reroot rewrites %s, which this schema does not have", name)
		}
	}
}

// TestApplyReroot_RewritesEveryPathColumnAndNothingElse seeds a row in every table with
// every path column under the old root, beside rows under another tree, and compares the
// WHOLE ledger after the apply against the one before with exactly the path values
// mapped. A column the rewrite missed, a non-path column it touched, a row outside the
// tree it moved, or an updated_at or schema_version it re-stamped all red here.
func TestApplyReroot_RewritesEveryPathColumnAndNothingElse(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i, root := range []string{oldRoot, "/media/old2"} {
		seedEveryTable(t, s, root, i+1)
	}
	before := dumpLedger(t, s)

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	wantCounts := map[string]int64{
		"jobs.path": 2, "jobs.target_path": 2, "jobs.library_root": 2, "jobs.subtitle_sidecars": 1,
		"retained_originals.source_path": 1, "retained_originals.swapped_path": 1, "retained_originals.retained_path": 1,
		"swap_incidents.source_path": 1, "swap_incidents.replacement_path": 1,
		"path_exclusions.path": 1, "health_checks.path": 1, "node_leases.path": 1, "node_leases.temp_path": 1,
		"playback_swaps.path": 1, "playback_sessions.path": 1,
	}
	wantRows := map[string]int64{"jobs": 2, "retained_originals": 1, "swap_incidents": 1, "path_exclusions": 1,
		"health_checks": 1, "node_leases": 1, "playback_swaps": 1, "playback_sessions": 1}
	if len(plan.Tables) != len(rerootTables) {
		t.Errorf("the plan reports %d table(s), want every one of the %d that hold a path", len(plan.Tables), len(rerootTables))
	}
	for _, tb := range plan.Tables {
		if tb.Rows != wantRows[tb.Table] {
			t.Errorf("%s: the plan rewrites %d row(s), want %d", tb.Table, tb.Rows, wantRows[tb.Table])
		}
	}
	// wantCounts is this test's own list of the path columns, so a column dropped from the
	// reroot's list reds here rather than dropping out of both sides of the comparison.
	for name, want := range wantCounts {
		table, column, _ := strings.Cut(name, ".")
		if got := countOf(plan, table, column); got != want {
			t.Errorf("%s: the plan rewrites %d value(s), want %d", name, got, want)
		}
	}
	if plan.Rows() != 9 {
		t.Errorf("the plan rewrites %d row(s) in all, want 9", plan.Rows())
	}
	if len(plan.Collisions) != 0 {
		t.Errorf("collisions on a ledger with nothing under %s: %+v", newRoot, plan.Collisions)
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Error("PlanReroot changed the ledger")
	}

	done, err := s.ApplyReroot(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyReroot: %v", err)
	}
	if done.Rows() != plan.Rows() {
		t.Errorf("the apply reports %d row(s), the plan %d", done.Rows(), plan.Rows())
	}

	want := map[string][]map[string]any{}
	for table, rows := range before {
		for _, row := range rows {
			moved := map[string]any{}
			for col, v := range row {
				moved[col] = v
				if _, path := wantCounts[table+"."+col]; !path {
					continue
				}
				if str, ok := v.(string); ok {
					if col == "subtitle_sidecars" {
						moved[col] = strings.ReplaceAll(str, `"`+oldRoot+`/`, `"`+newRoot+`/`)
					} else if nv, ok := RerootPath(str, oldRoot, newRoot); ok {
						moved[col] = nv
					}
				}
			}
			want[table] = append(want[table], moved)
		}
	}
	if got := dumpLedger(t, s); !reflect.DeepEqual(got, want) {
		for table := range want {
			if !reflect.DeepEqual(got[table], want[table]) {
				t.Errorf("%s after the reroot:\n got  %v\n want %v", table, got[table], want[table])
			}
		}
	}

	again, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot after the apply: %v", err)
	}
	if again.Rows() != 0 {
		t.Errorf("%d row(s) still name %s after the apply", again.Rows(), oldRoot)
	}
}

// TestPlanReroot_CollisionsAreTheTableKeys: a collision is a rewritten row whose KEY a row
// under the new root already holds. Where the key pairs the path with something else, the
// same path with a different partner is no collision - a superseded jobs row beside the
// current one, two sweeps' checks of one file, two epochs of one lease are the table's
// ordinary state. A colliding plan is refused by ApplyReroot and changes nothing.
func TestPlanReroot_CollisionsAreTheTableKeys(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedEveryTable(t, s, oldRoot, 1)
	// Under the new root: the same keys for jobs (extra.mkv), retained_originals and
	// path_exclusions, and different partners for health_checks (another sweep) and
	// node_leases (another epoch). film.mkv's jobs row there carries another fingerprint,
	// which is no collision.
	seedEveryTable(t, s, newRoot, 2)
	seedJob(t, s, newRoot+"/film.mkv", "999:999")
	execSQL(t, s, `DELETE FROM jobs WHERE path = ? AND fingerprint = '1:1700000000'`, newRoot+"/film.mkv")
	// A session on the same file with the old root's session id: the same whole key,
	// (source, session_id, path), so a collision named by both of its partner columns.
	execSQL(t, s, `INSERT INTO playback_sessions (source, session_id, path, fingerprint, started_at, offset_derived,
		client, platform, player, decision, recorded_at) VALUES ('tautulli', 's-1', ?, '5:1700000002', 1, 0, '', '', '', '', 1)`,
		newRoot+"/film.mkv")
	before := dumpLedger(t, s)

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	got := map[string]string{}
	for _, c := range plan.Collisions {
		if c.Was != strings.Replace(c.Path, newRoot, oldRoot, 1) {
			t.Errorf("a collision names %q as the row's own path and %q as its rewrite", c.Was, c.Path)
		}
		got[c.Table+" "+c.Path] = c.With
	}
	want := map[string]string{
		"jobs " + newRoot + "/extra.mkv":              "fingerprint 2:1700000000",
		"retained_originals " + newRoot + "/film.mkv": "",
		"path_exclusions " + newRoot + "/withheld":    "",
		"playback_swaps " + newRoot + "/film.mkv":     "fingerprint 5:1700000002",
		"playback_sessions " + newRoot + "/film.mkv":  "source tautulli, session_id s-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collisions:\n got  %v\n want %v", got, want)
	}

	_, err = s.ApplyReroot(ctx, plan)
	var refused *RerootCollisionsError
	if !errors.As(err, &refused) || len(refused.Collisions) != len(want) {
		t.Fatalf("ApplyReroot over a colliding plan = %v, want a RerootCollisionsError naming %d", err, len(want))
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Error("a refused reroot changed the ledger")
	}
}

// TestPlanReroot_EvidenceIsWhatTheLedgerRecordsPerPath: one entry per rewritten path,
// carrying every fingerprint the ledger records for the file there - each jobs row of the
// path and the swap's record of what it left - and, for a retained original, the size of
// the original it holds.
func TestPlanReroot_EvidenceIsWhatTheLedgerRecordsPerPath(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedEveryTable(t, s, oldRoot, 1)
	seedJob(t, s, oldRoot+"/film.mkv", "7:1600000000") // a superseded row of the same path

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	want := []RerootEvidence{
		{Was: oldRoot + "/.holdfast-undo/film.9-1600000000.__undo__.mkv.holdfast-undo",
			Path:     newRoot + "/.holdfast-undo/film.9-1600000000.__undo__.mkv.holdfast-undo",
			Retained: true, RetainedBytes: 9},
		{Was: oldRoot + "/extra.mkv", Path: newRoot + "/extra.mkv", Fingerprints: []string{"2:1700000000"}},
		{Was: oldRoot + "/film.mkv", Path: newRoot + "/film.mkv",
			Fingerprints: []string{"1:1700000000", "4:1700000001", "5:1700000002", "7:1600000000"}},
	}
	if !reflect.DeepEqual(plan.Evidence, want) {
		t.Errorf("evidence:\n got  %+v\n want %+v", plan.Evidence, want)
	}
}

// TestApplyReroot_AFailureMidwayLeavesNothingChanged makes the LAST table's rewrite fail
// (a trigger raises on it), after every other table's rows have been rewritten inside the
// transaction, and asserts the whole ledger is exactly as it was.
func TestApplyReroot_AFailureMidwayLeavesNothingChanged(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedEveryTable(t, s, oldRoot, 1)
	execSQL(t, s, `CREATE TRIGGER fail_midway BEFORE UPDATE OF path ON node_leases
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	before := dumpLedger(t, s)

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	if rerootTables[len(rerootTables)-1].name != "node_leases" {
		t.Fatal("node_leases is no longer the last table rewritten, so this failure is not mid-way through anything")
	}
	if _, err := s.ApplyReroot(ctx, plan); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("ApplyReroot = %v, want the injected failure", err)
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Errorf("a failed reroot left the ledger changed:\n before %v\n after  %v", before, after)
	}
}

// TestApplyReroot_ARowLeftUnderTheOldRootCommitsNothing: after the rewrite the plan is
// taken again inside the transaction and must find nothing under the old root. A trigger
// that puts one row's old path back stands in for whatever could leave one behind.
func TestApplyReroot_ARowLeftUnderTheOldRootCommitsNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedEveryTable(t, s, oldRoot, 1)
	execSQL(t, s, `CREATE TRIGGER put_back AFTER UPDATE OF path ON path_exclusions
		BEGIN UPDATE path_exclusions SET path = OLD.path WHERE rowid = NEW.rowid; END`)
	before := dumpLedger(t, s)

	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	if _, err := s.ApplyReroot(ctx, plan); err == nil || !strings.Contains(err.Error(), "still name") {
		t.Fatalf("ApplyReroot = %v, want the refusal of a rewrite that left a row under %s", err, oldRoot)
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Error("a rewrite that left a row behind was committed")
	}
}

// TestApplyReroot_RefusesALedgerThatMovedSinceThePlan: the evidence was taken over the
// plan the caller holds, so an apply that would rewrite anything else is refused.
func TestApplyReroot_RefusesALedgerThatMovedSinceThePlan(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedEveryTable(t, s, oldRoot, 1)
	plan, err := s.PlanReroot(ctx, oldRoot, newRoot)
	if err != nil {
		t.Fatalf("PlanReroot: %v", err)
	}
	seedJob(t, s, oldRoot+"/late.mkv", "3:3")
	before := dumpLedger(t, s)
	if _, err := s.ApplyReroot(ctx, plan); !errors.Is(err, ErrRerootLedgerMoved) {
		t.Fatalf("ApplyReroot = %v, want ErrRerootLedgerMoved", err)
	}
	if after := dumpLedger(t, s); !reflect.DeepEqual(before, after) {
		t.Error("a refused reroot changed the ledger")
	}
}

// TestOpenExclusive_IsTheLedgersOnlyUser: refused while another handle has the ledger
// open (a daemon idles with its connection open, and that is enough), and while it holds
// the ledger nothing else can read it. It never creates a ledger.
func TestOpenExclusive_IsTheLedgersOnlyUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	daemon, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := daemon.Summary(context.Background()); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if st, err := OpenExclusive(path); !errors.Is(err, ErrLedgerInUse) {
		if st != nil {
			_ = st.Close()
		}
		t.Fatalf("OpenExclusive beside an open daemon handle = %v, want ErrLedgerInUse", err)
	}
	if _, err := daemon.Summary(context.Background()); err != nil {
		t.Fatalf("the refused exclusive open disturbed the daemon's handle: %v", err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenExclusive(path)
	if err != nil {
		t.Fatalf("OpenExclusive with nothing else open: %v", err)
	}
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = other.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n)
	_ = other.Close()
	if !busy(err) {
		t.Errorf("another connection read the ledger while it was held exclusively (err = %v)", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "absent.db")
	if st, err := OpenExclusive(missing); err == nil {
		_ = st.Close()
		t.Error("OpenExclusive opened a ledger that does not exist")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("OpenExclusive created %s", missing)
	}
}

// ---- fixtures ---------------------------------------------------------------

func execSQL(t *testing.T, s *SQLite, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seedJob(t *testing.T, s *SQLite, path, fp string) {
	t.Helper()
	execSQL(t, s, `INSERT INTO jobs (path, fingerprint, status, updated_at, schema_version) VALUES (?, ?, 'skipped', 7, 3)`, path, fp)
}

// seedEveryTable writes, under root, one row in every table that holds a path, with every
// path column set, and a second jobs row. id keeps the ids, sweeps and epochs apart.
func seedEveryTable(t *testing.T, s *SQLite, root string, id int) {
	t.Helper()
	film := root + "/film.mkv"
	retained := root + "/.holdfast-undo/film.9-1600000000.__undo__.mkv.holdfast-undo"
	sidecars := RecordSidecars([]Sidecar{
		{Index: 2, Codec: "subrip", Language: "eng", Path: root + "/film.eng.srt"},
		{Index: 3, Codec: "subrip", Language: "fre", Path: "/elsewhere/film.fre.srt"},
		{Index: 4, Codec: "hdmv_pgs_subtitle", Skipped: "bitmap"},
	}).Encode()
	execSQL(t, s, `INSERT INTO jobs (path, fingerprint, status, updated_at, reason, target_path, library_root,
		subtitle_sidecars, decision_inputs, schema_version)
		VALUES (?, '1:1700000000', 'done', 11, NULL, ?, ?, ?, '{"crf":"22"}', 20)`, film, film, root, sidecars)
	execSQL(t, s, `INSERT INTO jobs (path, fingerprint, status, updated_at, reason, target_path, library_root, schema_version)
		VALUES (?, '2:1700000000', 'failed', 12, 'VMAF below threshold', ?, ?, NULL)`,
		root+"/extra.mkv", root+"/extra.mp4", root)
	execSQL(t, s, `INSERT INTO retained_originals (source_path, swapped_path, retained_path, source_bytes,
		swapped_fingerprint, retained_at, expires_at, restored_at, schema_version)
		VALUES (?, ?, ?, 9, '4:1700000001', 100, 200, NULL, 25)`, film, film, retained)
	execSQL(t, s, `INSERT INTO swap_incidents (source_path, source_fingerprint, replacement_path, source_attrs,
		replacement_attrs, outcome, created_at, schema_version)
		VALUES (?, '1:1', ?, 'a', 'b', 'indeterminate', 5, 25)`, film, root+"/film.holdfast-tmp.mkv")
	execSQL(t, s, `INSERT INTO path_exclusions (path, created_at, schema_version) VALUES (?, 6, 25)`, root+"/withheld")
	execSQL(t, s, `INSERT INTO health_sweeps (id, started_at) VALUES (?, 1)`, id)
	execSQL(t, s, `INSERT INTO health_checks (sweep_id, path, size, mtime_ns, checked_at, result, reason)
		VALUES (?, ?, 1, 1700000000000000000, 2, 'ok', '')`, id, film)
	// An earlier swap at the path, since superseded, and a session played on it.
	execSQL(t, s, `INSERT INTO playback_swaps (path, fingerprint, swapped_at, superseded_at, recorded_at, schema_version)
		VALUES (?, '5:1700000002', 8, 9, 10, 26)`, film)
	execSQL(t, s, `INSERT INTO playback_sessions (source, session_id, path, fingerprint, started_at, offset_derived,
		client, platform, player, decision, recorded_at) VALUES ('tautulli', ?, ?, '5:1700000002', 8, 0, 'c', 'p', 'pl', 'direct play', 10)`,
		fmt.Sprintf("s-%d", id), film)
	execSQL(t, s, `INSERT INTO node_leases (id, path, job_key, node, epoch, state, expires_at, temp_path, reserved_bytes,
		args_digest, source_size, source_mtime_ns, upload_attempts, reason, granted_at, updated_at)
		VALUES (?, ?, '1:1700000000', 'n1', ?, 'expired', 1, ?, 1, 'd', 1, 1, 0, 'expired', 1, 1)`,
		fmt.Sprintf("lease-%d", id), film, id, root+"/film.lease.tmp")
}

func countOf(p RerootPlan, table, column string) int64 {
	for _, tb := range p.Tables {
		if tb.Table != table {
			continue
		}
		for _, c := range tb.Columns {
			if c.Column == column {
				return c.Values
			}
		}
	}
	return -1
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// dumpTable is every row of table, by rowid, every column by name.
func dumpTable(t *testing.T, s *SQLite, table string) []map[string]any {
	t.Helper()
	cols := columnNames(t, s.db, table)
	rows, err := s.db.Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out
}

// dumpLedger is every table's dump, so "nothing changed" is a comparison of everything.
func dumpLedger(t *testing.T, s *SQLite) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, table := range tableNames(t, s.db) {
		out[table] = dumpTable(t, s, table)
	}
	return out
}

// TestOpenExclusiveCurrent_RefusesAnOlderSchemaAndChangesNothing: the dry run's door holds
// the ledger like OpenExclusive and migrates nothing. A ledger an earlier build wrote is
// refused with its version left exactly as it was; OpenExclusive migrates the same file.
func TestOpenExclusiveCurrent_RefusesAnOlderSchemaAndChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedTwoTerminalRows(t, path)
	behind := windBackOneSchemaVersion(t, path)

	if st, err := OpenExclusiveCurrent(path); !errors.Is(err, ErrSchemaBehind) {
		if st != nil {
			_ = st.Close()
		}
		t.Fatalf("OpenExclusiveCurrent on an older ledger = %v, want ErrSchemaBehind", err)
	}
	if got := rawUserVersion(t, path); got != behind {
		t.Fatalf("the refused open moved the schema version to %d, want it left at %d", got, behind)
	}

	st, err := OpenExclusive(path)
	if err != nil {
		t.Fatalf("OpenExclusive on an older ledger: %v", err)
	}
	if len(st.MigrationReport()) != 1 {
		t.Errorf("OpenExclusive applied %d step(s), want the 1 the ledger was behind", len(st.MigrationReport()))
	}
	_ = st.Close()

	st, err = OpenExclusiveCurrent(path)
	if err != nil {
		t.Fatalf("OpenExclusiveCurrent on a current ledger: %v", err)
	}
	_ = st.Close()
}

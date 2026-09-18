package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// seedPreviousBuildDoneRow writes a DONE row of the shape the previous build wrote: a real
// encode, with its proof, and no deinterlace columns because that build had none.
//
// It is a done row rather than the would-transcode one the target-path fixture uses,
// because the export's walk carries the three terminal statuses only - and the export is
// what this case is about.
func seedPreviousBuildDoneRow(t *testing.T, path, jobPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			encoder, vmaf_mean, vmaf_min, vmaf_model, vmaf_pix_fmt,
			source_codec, source_bytes, output_bytes, schema_version)
		 VALUES (?, ?, ?, 0, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobPath, "9:9", string(Done), 4000,
		"cpu", 98.4, 96.1, "version=vmaf_v0.6.1", "yuv420p10le",
		"h264", 4_000_000, 1_500_000, schemaVersion()-1); err != nil {
		t.Fatalf("seed the previous build's row: %v", err)
	}
}

// TestExport_PreExistingRowsCarryNullDeinterlaceFields grades [AC-14] of
// S0107-holdfast-interlacing-decision: a terminal row written before this change reads - and
// therefore exports - its deinterlace fields as NOT RECORDED, and is never reported as
// not-deinterlaced.
//
// Unmeasured and measured-false are different facts, and here they are different facts
// about a file this tool has already deleted. Every row in an existing ledger was written
// by a build that could not deinterlace at all, so nothing about it was measured; a stored
// 0, or a reader that turned NULL into false, would put a provenance claim on every one of
// those rows at once - and the source that would settle the question is gone.
//
// The fixture is a REAL database at the version before this build's, migrated by this
// build's own door, not a current database wearing an older number.
func TestExport_PreExistingRowsCarryNullDeinterlaceFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	prev := schemaVersion() - 1
	atShippedVersion(t, dbPath, prev)
	older := "/lib/decided-by-the-previous-build.mkv"
	seedPreviousBuildDoneRow(t, dbPath, older)
	before := rowCountOf(t, dbPath, "jobs")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open a ledger the previous build wrote: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// The migration EXPANDED: every row survived it.
	if after := rowCountOf(t, dbPath, "jobs"); after != before {
		t.Fatalf("the migration left %d job row(s), was %d", after, before)
	}

	// Read through the walk `holdfast export` itself uses, so what is graded is the row the
	// export publishes rather than a projection written for this test.
	var old Job
	found := false
	if err := st.EachTerminal(ctx, func(j Job) error {
		if j.Path == older {
			old, found = j, true
		}
		return nil
	}); err != nil {
		t.Fatalf("EachTerminal: %v", err)
	}
	if !found {
		t.Fatalf("the previous build's row is not in the export walk after the migration")
	}

	if old.Outcome.Deinterlaced != nil {
		t.Errorf("a row written before these columns existed reads deinterlaced = %v. Nothing measured "+
			"it: that build could not deinterlace at all, and an explicit false here is a claim about "+
			"the provenance of a file whose source has been deleted", *old.Outcome.Deinterlaced)
	}
	if old.Outcome.DeinterlaceFilter != "" {
		t.Errorf("a row written before these columns existed names the filter %q", old.Outcome.DeinterlaceFilter)
	}
	// Everything that build DID record is untouched: an expansion adds, it does not rewrite.
	if old.Outcome.SourceCodec != "h264" || old.Outcome.SourceBytes == nil || *old.Outcome.SourceBytes != 4_000_000 ||
		old.Outcome.VmafPixFmt != "yuv420p10le" || old.Outcome.VmafMean == nil {
		t.Errorf("the migration disturbed what the previous build recorded: %+v", old.Outcome)
	}
	// The stamp is what makes "not recorded" a fact about the WRITER rather than a guess
	// from an empty column.
	if v, ok := old.Stamp.Version(); !ok || v != prev {
		t.Errorf("the previous build's row is stamped %s, want version %d", old.Stamp, prev)
	}

	// And the distinction the null exists for: a row THIS build writes for a job that
	// deinterlaced nothing says so OUTRIGHT, with an explicit false - which is a different
	// answer from the one above and must be stored and read as one.
	no := false
	fresh := "/lib/decided-now.mkv"
	if ok, err := st.Claim(ctx, fresh, "8:8", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", fresh, ok, err)
	}
	if err := st.Finish(ctx, fresh, "8:8", Done, &Outcome{Encoder: "cpu", Deinterlaced: &no}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var written Job
	found = false
	if err := st.EachTerminal(ctx, func(j Job) error {
		if j.Path == fresh {
			written, found = j, true
		}
		return nil
	}); err != nil {
		t.Fatalf("EachTerminal: %v", err)
	}
	if !found {
		t.Fatalf("the row this build just wrote is not in the export walk")
	}
	if written.Outcome.Deinterlaced == nil {
		t.Fatal("a row this build wrote for a job that deinterlaced nothing reads as NOT RECORDED - " +
			"then the two states are one state, and the null above says nothing")
	}
	if *written.Outcome.Deinterlaced {
		t.Errorf("a job that deinterlaced nothing reads deinterlaced = true")
	}

	// A job that DID deinterlace round-trips the whole expression, mode and parity included:
	// the same filter at a different mode is a different transformation.
	yes := true
	const spec = "yadif=mode=send_frame:parity=auto:deint=all"
	did := "/lib/broadcast.mkv"
	if ok, err := st.Claim(ctx, did, "7:7", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", did, ok, err)
	}
	if err := st.Finish(ctx, did, "7:7", Done,
		&Outcome{Encoder: "cpu", Deinterlaced: &yes, DeinterlaceFilter: spec}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	rows, err := st.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.Path != did {
			continue
		}
		if r.Outcome.Deinterlaced == nil || !*r.Outcome.Deinterlaced || r.Outcome.DeinterlaceFilter != spec {
			t.Errorf("a deinterlaced job round-trips as deinterlaced=%v filter=%q, want true and %q",
				r.Outcome.Deinterlaced, r.Outcome.DeinterlaceFilter, spec)
		}
	}
}

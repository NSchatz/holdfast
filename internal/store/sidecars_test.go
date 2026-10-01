package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// The subtitle sidecar record across the column: a recorded list and a recorded empty set
// round-trip through a terminal row, a row that recorded nothing reads as NOT RECORDED, and a
// ledger the previous build wrote opens with the column added and
// every row reading as not recorded.
func TestSidecar_RecordRoundTripsThroughATerminalRow(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	three := 3
	list := []Sidecar{
		{Index: 2, Codec: "subrip", Language: "eng", Path: "/lib/film.eng.srt", Events: &three},
		{Index: 3, Codec: "ass", Language: "fre", Forced: true, Skipped: "sidecar-event-count-mismatch",
			Detail: "source stream has 3 events, sidecar has 1", Events: &three, FontsLost: true},
		{Index: 4, Codec: "hdmv_pgs_subtitle", Skipped: "bitmap-subtitle"},
	}
	for name, c := range map[string]struct {
		in       Sidecars
		recorded bool
		want     []Sidecar
	}{
		"list":         {RecordSidecars(list), true, list},
		"empty":        {RecordSidecars(nil), true, []Sidecar{}},
		"not recorded": {Sidecars{}, false, []Sidecar{}},
	} {
		path := "/lib/" + name + ".mkv"
		if _, err := st.Claim(ctx, path, "1:1", "w", 3, DecisionInputs{}); err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, path, "1:1", Done, &Outcome{SubtitleSidecars: c.in}, 3); err != nil {
			t.Fatal(err)
		}
		got := outcomeOf(t, st, path)
		if got.Recorded() != c.recorded || !reflect.DeepEqual(got.List(), c.want) {
			t.Errorf("%s: read back recorded=%v %+v, want recorded=%v %+v", name, got.Recorded(), got.List(), c.recorded, c.want)
		}
	}
}

func outcomeOf(t *testing.T, st *SQLite, path string) Sidecars {
	t.Helper()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r.Outcome.SubtitleSidecars
		}
	}
	t.Fatalf("no row for %s", path)
	return Sidecars{}
}

func TestSidecar_EncodeAndParseKeepAbsenceApart(t *testing.T) {
	if got := (Sidecars{}).Encode(); got != "" {
		t.Errorf("not recorded encodes as %q, want \"\" (NULL)", got)
	}
	if got := RecordSidecars(nil).Encode(); got != "[]" {
		t.Errorf("an empty record encodes as %q, want []", got)
	}
	for _, bad := range []string{"", "  ", "null", "{", `{"index":1}`, "[1]"} {
		if ParseSidecars(bad).Recorded() {
			t.Errorf("ParseSidecars(%q) reads as recorded", bad)
		}
	}
	if p := ParseSidecars("[]"); !p.Recorded() || len(p.List()) != 0 {
		t.Errorf("[] parses as %+v", p)
	}
}

// A ledger the previous build wrote opens under this one, keeps its row, and reads it as
// recording nothing about sidecars.
func TestSidecar_ALedgerWrittenBeforeTheColumnReadsAsNotRecorded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	atShippedVersion(t, dbPath, schemaVersion()-1)
	seedPreviousBuildRow(t, dbPath, "/lib/older.mkv")
	before := rowCountOf(t, dbPath, "jobs")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if after := rowCountOf(t, dbPath, "jobs"); after != before {
		t.Fatalf("rows %d, was %d", after, before)
	}
	if got := outcomeOf(t, st, "/lib/older.mkv"); got.Recorded() {
		t.Fatalf("a row written before the column reads as recorded: %+v", got.List())
	}
}

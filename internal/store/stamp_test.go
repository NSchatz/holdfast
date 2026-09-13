package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// The version stamp each persisted record carries: what a record says about the build
// that wrote it, and what a reader does with a record that says nothing, or says
// something this build cannot place.

// [AC-7] Every record the store writes - a job ledger row, a retained original, a swap
// incident - names the schema version in force at the moment it was written, and a read of
// that record through the store returns it.
//
// Each of the three is written through its own production path and read back through its
// own production reader, because the stamp is worth nothing if it holds on the table a
// test happened to pick.
func TestStamp_EveryRecordTheStoreWritesNamesTheVersionThatWroteIt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if ok, err := s.Claim(ctx, "/lib/a.mkv", "1:1", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/a.mkv", "1:1", Done, &Outcome{
		Encoder: "cpu", SourceBytes: i64(4096), OutputBytes: i64(1024),
	}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d row(s), want 1", len(rows))
	}
	assertStampedNow(t, "the job row", rows[0].Stamp)

	if err := s.Retain(ctx, Retained{
		SourcePath: "/lib/a.mkv", SwappedPath: "/lib/a.mkv",
		RetainedPath: "/lib/.holdfast-undo/a", SourceBytes: 4096,
		SwappedFingerprint: "2:2", RetainedAt: 100, ExpiresAt: 200,
	}); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	retained, ok, err := s.GetRetained(ctx, "/lib/a.mkv")
	if err != nil || !ok {
		t.Fatalf("GetRetained: ok=%v err=%v", ok, err)
	}
	assertStampedNow(t, "the retained original", retained.Stamp)

	if err := s.RecordSwapIncident(ctx, SwapIncident{
		SourcePath: "/lib/b.mkv", SourceFingerprint: "3:3",
		ReplacementPath: "/lib/b.hevc.mkv", SourceAttrs: "3:3", ReplacementAttrs: "9:9",
		Outcome: Indeterminate, SwapError: "rename failed",
	}); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	parked, err := s.ParkedIncidents(ctx)
	if err != nil || len(parked) != 1 {
		t.Fatalf("ParkedIncidents: %d incident(s), err=%v", len(parked), err)
	}
	assertStampedNow(t, "the swap incident", parked[0].Stamp)
}

// assertStampedNow is the whole of "it names the version in force": recorded, recognised,
// and equal to the version this build's history ends at, which is also the version stamped
// in the database header.
func assertStampedNow(t *testing.T, what string, stamp SchemaStamp) {
	t.Helper()
	if !stamp.Recorded() {
		t.Errorf("%s carries no version stamp: %s", what, stamp)
		return
	}
	v, ok := stamp.Version()
	if !ok || v != schemaVersion() {
		t.Errorf("%s names version %d (recognised=%v), want %d", what, v, ok, schemaVersion())
	}
}

// [AC-8] A record carrying no stamp is presented as written before the stamp existed, is
// never presented as a schema version, has no stamp written onto it by being read, and
// returns every one of its other fields exactly as it did before the stamp existed.
//
// Driven from a real database at the version before the stamp shipped, with records in it,
// which is the population the rule is for: everything already in the field.
func TestStamp_ARecordWrittenBeforeTheStampReadsAsUnstampedAndIsLeftThatWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	atShippedVersion(t, path, schemaVersion()-1)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != seededJobRows {
		t.Fatalf("the ledger holds %d row(s), want %d", len(rows), seededJobRows)
	}
	for _, j := range rows {
		if j.Stamp.Recorded() {
			t.Errorf("%s carries a stamp nobody wrote: %s", j.Path, j.Stamp)
		}
		if v, ok := j.Stamp.Version(); ok || v != 0 {
			t.Errorf("%s reads as version %d (recognised=%v) - an unstamped record must never "+
				"be presented as a schema version", j.Path, v, ok)
		}
	}

	// Every other field is what it was. The fingerprint, status, attempt count and
	// transition time are the record; a reader that gained a stamp must not have lost any
	// of them.
	byPath := make(map[string]Job, len(rows))
	for _, j := range rows {
		byPath[j.Path] = j
	}
	if j := byPath["/lib/failed.mkv"]; j.Status != Failed || j.FailCount != 2 ||
		j.Fingerprint != "30:300" || j.UpdatedAt != 1002 {
		t.Errorf("an unstamped record came back mangled: %+v", j)
	}

	// Reading it wrote nothing: the column is still NULL on every row, asked of SQLite.
	for _, table := range []string{"jobs", "retained_originals", "swap_incidents"} {
		if got := stampedRows(t, path, table); got != 0 {
			t.Errorf("%d row(s) in %s gained a stamp by being read", got, table)
		}
	}
}

// [AC-9] A record whose stamp is not a whole number inside the schema history this build
// knows is presented as UNRECOGNISED, never as this build's version, and the rest of the
// record still reads.
//
// The values are the ones that can really arrive: a version from a newer holdfast (a
// database an operator restored, or one a rolled-back binary is now reading), a zero or a
// negative, and text an operator's repair script put there. A typed destination would fail
// the whole read on the last of those, which would cost the evidence in every other field
// of a record over one column nobody can interpret.
func TestStamp_AnUnrecognisedStampReadsAsUnrecognisedAndTheRecordStillReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write any
		raw   string
	}{
		// Derived from the history rather than written out, because the two are the same
		// number: a literal here agrees with the history on the day it is typed and
		// disagrees with it the first time a step is appended.
		{name: "a version past the end of this build's history", write: schemaVersion() + 1, raw: strconv.Itoa(schemaVersion() + 1)},
		{name: "zero, which names no step", write: 0, raw: "0"},
		{name: "a negative number", write: -3, raw: "-3"},
		{name: "text, which is not a version at all", write: "banana", raw: "banana"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			if ok, err := s.Claim(ctx, "/lib/a.mkv", "1:1", "w0", 3, sameConfig); err != nil || !ok {
				t.Fatalf("Claim: ok=%v err=%v", ok, err)
			}
			if err := s.Finish(ctx, "/lib/a.mkv", "1:1", Done, &Outcome{
				Encoder: "cpu", SourceBytes: i64(4096), OutputBytes: i64(1024),
			}, 3); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			// Written through the raw handle, because no build of holdfast can put this
			// there: that is what makes it the record this rule is about.
			if _, err := s.db.ExecContext(ctx,
				`UPDATE jobs SET schema_version = ? WHERE path = ?`, tc.write, "/lib/a.mkv"); err != nil {
				t.Fatalf("write an unrecognisable stamp: %v", err)
			}

			rows, err := s.List(ctx, nil, 0)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("the ledger holds %d row(s), want 1", len(rows))
			}
			j := rows[0]
			if !j.Stamp.Recorded() {
				t.Errorf("a record carrying %q reads as carrying no stamp at all", tc.raw)
			}
			if j.Stamp.Recognised() {
				t.Errorf("%q was recognised as a schema version", tc.raw)
			}
			if v, ok := j.Stamp.Version(); ok || v != 0 {
				t.Errorf("a record carrying %q reads as version %d (recognised=%v)", tc.raw, v, ok)
			}
			if j.Stamp.Raw() != tc.raw {
				t.Errorf("the unrecognised stamp reads back as %q, want %q", j.Stamp.Raw(), tc.raw)
			}
			// The rest of the record is still there: the read did not fail over the one
			// field it could not place.
			if j.Path != "/lib/a.mkv" || j.Status != Done || j.Outcome.Encoder != "cpu" ||
				j.Outcome.SourceBytes == nil || *j.Outcome.SourceBytes != 4096 {
				t.Errorf("a record with an unrecognisable stamp lost its other fields: %+v", j)
			}
		})
	}
}

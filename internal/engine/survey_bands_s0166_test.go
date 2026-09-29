package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0166: the ledger survey behind the startup report and `validate` resolves a row on a root
// that bands its files by source height against the band that row's STORED height selects,
// exactly as the scan resolves the file, and counts a banded row that stored no height as an
// upper bound of its own rather than guessing a band for it.
//
// THE REPORTED CASE, in the shape the spec gives it: one banded root whose own floor is 2500,
// three disjoint height bands with the floors 3000, 4000 and 10000, and `skipped/low-bitrate`
// rows that each store a source height and record the floor they were compared against - some
// in each band, and some at a height no band admits, recorded at the root's own floor. The
// spec's counts come from one operator's ledger; this public repository uses its own counts of
// the same shape (15, 17, 1 and 5 rows), so no deployment's figures are recorded here.

const (
	s0166InBand3000   = 15
	s0166InBand4000   = 17
	s0166InBand10000  = 1
	s0166InNoBand     = 5
	s0166ReportedRows = s0166InBand3000 + s0166InBand4000 + s0166InBand10000 + s0166InNoBand
)

// s0166Layout is the library a survey case is about: a root whose rules band on the source
// height, and beside it a root with no rules and a root whose one rule carries no height bound.
type s0166Layout struct{ banded, unruled, unbounded string }

func s0166Roots(t *testing.T) s0166Layout {
	t.Helper()
	base := t.TempDir()
	l := s0166Layout{
		banded:    filepath.Join(base, "banded"),
		unruled:   filepath.Join(base, "unruled"),
		unbounded: filepath.Join(base, "unbounded"),
	}
	for _, d := range []string{l.banded, l.unruled, l.unbounded} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// s0166Config is THE REPORTED CASE's configuration with two of its floors as parameters: the
// banded root's own, and the 4000 band's. The other two roots are the AC-4 and AC-6 contrast.
func s0166Config(t *testing.T, l s0166Layout, rootFloor, band4000Floor, unboundedFloor int) config.Config {
	t.Helper()
	return profileCfg(t, fmt.Sprintf(`
library_roots:
  - path: %s
    min_bitrate_kbps: %d
    rules:
      - when:
          min_source_height: 480
          max_source_height: 719
        min_bitrate_kbps: 3000
      - when:
          min_source_height: 720
          max_source_height: 1079
        min_bitrate_kbps: %d
      - when:
          min_source_height: 1080
        min_bitrate_kbps: 10000
  - path: %s
    min_bitrate_kbps: 2500
  - path: %s
    min_bitrate_kbps: 2500
    rules:
      - min_bitrate_kbps: %d
encoder: cpu
`, l.banded, rootFloor, band4000Floor, l.unruled, l.unbounded, unboundedFloor))
}

// s0166Row is one terminal `skipped/low-bitrate` row: the floor it recorded, the source's
// true height, and whether the row stored that height.
type s0166Row struct {
	path     string
	height   int
	stored   bool
	floor    int
	recorded bool
}

func (r s0166Row) outcome() *store.Outcome {
	o := &store.Outcome{Reason: SkipLowBitrate}
	if r.recorded {
		o.DecisionInputs = store.InputsRead(map[string]string{InputMinBitrateKbps: strconv.Itoa(r.floor)})
	}
	if r.stored {
		o.SourceWidth, o.SourceHeight = ptr(r.height*16/9), ptr(r.height)
	}
	return o
}

// s0166ReportedRows is THE REPORTED CASE's rows on the banded root.
func s0166ReportedCase(l s0166Layout) []s0166Row {
	var rows []s0166Row
	add := func(n, height, floor int, name string) {
		for i := 0; i < n; i++ {
			rows = append(rows, s0166Row{path: filepath.Join(l.banded, fmt.Sprintf("%s-%02d.mkv", name, i)),
				height: height, stored: true, floor: floor, recorded: true})
		}
	}
	add(s0166InBand3000, 576, 3000, "band3000")
	add(s0166InBand4000, 720, 4000, "band4000")
	add(s0166InBand10000, 2160, 10000, "band10000")
	add(s0166InNoBand, 360, 2500, "noband")
	return rows
}

// s0166Survey seeds rows into one ledger and surveys it under cfg, the way the startup report
// and `validate` ask.
func s0166Survey(t *testing.T, cfg config.Config, rows []s0166Row) store.DecisionInputsSurvey {
	t.Helper()
	ts := newTestStore(t, t.TempDir())
	for _, r := range rows {
		seedTerminal(t, ts, r.path, store.Skipped, r.outcome())
	}
	got, err := ts.SurveyDecisionInputs(context.Background(), DecisionInputsPerPath(cfg))
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	return got
}

// TestS0166AC1_TheReportedCaseUnderItsOwnConfigurationMovesNothing grades AC-1: every row was
// decided under the configuration in front of it, so none has moved - including the rows a
// band's floor decided, which the root's own floor used to announce as moved.
func TestS0166AC1_TheReportedCaseUnderItsOwnConfigurationMovesNothing(t *testing.T) {
	l := s0166Roots(t)
	got := s0166Survey(t, s0166Config(t, l, 2500, 4000, 2500), s0166ReportedCase(l))
	want := store.DecisionInputsSurvey{Matching: s0166ReportedRows}
	if got != want {
		t.Errorf("survey = %+v, want %+v", got, want)
	}
}

// TestS0166AC2_EditingOneBandMovesThatBandOnly grades AC-2: an edit to the 4000 band's floor
// moves exactly the rows that band decided.
func TestS0166AC2_EditingOneBandMovesThatBandOnly(t *testing.T) {
	l := s0166Roots(t)
	got := s0166Survey(t, s0166Config(t, l, 2500, 4500, 2500), s0166ReportedCase(l))
	want := store.DecisionInputsSurvey{Moved: s0166InBand4000, Matching: s0166ReportedRows - s0166InBand4000}
	if got != want {
		t.Errorf("survey = %+v, want %+v", got, want)
	}
}

// TestS0166AC3_EditingTheRootsOwnFloorMovesTheUnbandedRowsOnly grades AC-3: an edit to the
// root's own floor moves exactly the rows no band admitted.
func TestS0166AC3_EditingTheRootsOwnFloorMovesTheUnbandedRowsOnly(t *testing.T) {
	l := s0166Roots(t)
	got := s0166Survey(t, s0166Config(t, l, 2000, 4000, 2500), s0166ReportedCase(l))
	want := store.DecisionInputsSurvey{Moved: s0166InNoBand, Matching: s0166ReportedRows - s0166InNoBand}
	if got != want {
		t.Errorf("survey = %+v, want %+v", got, want)
	}
}

// s0166MixedLedger is AC-4's ledger: banded rows with and without a stored height, rows on
// the two other roots with and without one, a row that recorded nothing, and rows under no
// configured root.
func s0166MixedLedger(l s0166Layout) []s0166Row {
	b := func(name string) string { return filepath.Join(l.banded, name) }
	u := func(name string) string { return filepath.Join(l.unruled, name) }
	n := func(name string) string { return filepath.Join(l.unbounded, name) }
	return []s0166Row{
		{path: b("kept-3000.mkv"), height: 576, stored: true, floor: 3000, recorded: true},
		{path: b("moved-4000.mkv"), height: 720, stored: true, floor: 4000, recorded: true},
		{path: b("kept-10000.mkv"), height: 1080, stored: true, floor: 10000, recorded: true},
		{path: b("moved-root.mkv"), height: 360, stored: true, floor: 2500, recorded: true},
		{path: b("heightless-3000.mkv"), height: 576, stored: false, floor: 3000, recorded: true},
		{path: b("heightless-4000.mkv"), height: 720, stored: false, floor: 4000, recorded: true},
		{path: b("heightless-unrecorded.mkv"), height: 720, stored: false, recorded: false},
		{path: u("kept.mkv"), height: 720, stored: true, floor: 2500, recorded: true},
		{path: u("heightless.mkv"), height: 720, stored: false, floor: 2500, recorded: true},
		{path: n("moved.mkv"), height: 720, stored: true, floor: 2600, recorded: true},
		{path: n("heightless-moved.mkv"), height: 720, stored: false, floor: 2600, recorded: true},
		{path: "/nowhere/under-no-root.mkv", height: 720, stored: true, floor: 2500, recorded: true},
	}
}

// s0166Resolved is what the SCAN hands a claim of this row's path: the configuration in force
// resolved for that file at its TRUE source height, which the scan reads off the file whether
// or not the row stored it.
func s0166Resolved(cfg config.Config, r s0166Row) store.DecisionInputs {
	prof := cfg.TopLevelProfile()
	for _, root := range cfg.RootProfiles() {
		if root.Contains(r.path) {
			prof = root.Profile
			break
		}
	}
	prof = prof.WithRules(r.height)
	return DecisionInputsForJob(cfg, prof, cfg.TranscodeIn(prof, r.path))
}

// s0166Bucket is the one survey figure a single row lands in.
func s0166Bucket(s store.DecisionInputsSurvey) string {
	switch {
	case s.Moved == 1:
		return "moved"
	case s.NotRecorded == 1:
		return "not recorded"
	case s.NoSourceHeight == 1:
		return "no source height"
	case s.Matching == 1:
		return "matching"
	}
	return fmt.Sprintf("none (%+v)", s)
}

// TestS0166AC4_NoRowTheClaimReopensIsUndercounted grades AC-4 row by row: every row the REAL
// store claim re-opens - handed the inputs the scan resolves for the file at its true height -
// is in the moved, not-recorded or no-source-height figure, and no row whose height is stored
// is counted as moved while that claim leaves it terminal. Each row gets a ledger of its own,
// so the survey's figures name that row's bucket and the claim acts on that row alone.
func TestS0166AC4_NoRowTheClaimReopensIsUndercounted(t *testing.T) {
	l := s0166Roots(t)
	edited := s0166Config(t, l, 2000, 4500, 2500)
	claims, counted := 0, 0
	for _, r := range s0166MixedLedger(l) {
		ts := newTestStore(t, t.TempDir())
		seedTerminal(t, ts, r.path, store.Skipped, r.outcome())
		survey, err := ts.SurveyDecisionInputs(context.Background(), DecisionInputsPerPath(edited))
		if err != nil {
			t.Fatalf("SurveyDecisionInputs: %v", err)
		}
		bucket := s0166Bucket(survey)
		reopened, err := ts.Claim(context.Background(), r.path, "fp", "s0166", 3, s0166Resolved(edited, r))
		if err != nil {
			t.Fatalf("claim %s: %v", r.path, err)
		}
		if reopened {
			claims++
		}
		if bucket != "matching" {
			counted++
		}
		switch {
		case reopened && bucket == "matching":
			t.Errorf("%s: the claim re-opens it but the survey counts it as still matching - an "+
				"undercount, the direction that tells an operator nothing will move", r.path)
		case !reopened && bucket == "moved" && r.stored:
			t.Errorf("%s: the survey counts it as moved, but the claim leaves it terminal", r.path)
		case bucket == "no source height" && r.stored:
			t.Errorf("%s stores its height and was still counted as having none", r.path)
		}
	}
	// Anti-vacuity: the edit moves something, and the ledger has rows the claim leaves alone.
	if claims == 0 || claims == len(s0166MixedLedger(l)) {
		t.Fatalf("the claim re-opened %d of %d rows, so this ledger cannot tell an undercount from a "+
			"correct count", claims, len(s0166MixedLedger(l)))
	}
	if counted < claims {
		t.Errorf("%d row(s) are counted as possibly re-opening against %d the claim re-opens", counted, claims)
	}
}

// TestS0166AC5_ABandedRowWithNoHeightIsItsOwnFigure grades AC-5: a banded root's row that
// stored no height is counted in the no-source-height figure and in neither moved nor still
// matching, whatever floor it recorded - including one that matches no band's floor now.
func TestS0166AC5_ABandedRowWithNoHeightIsItsOwnFigure(t *testing.T) {
	l := s0166Roots(t)
	cfg := s0166Config(t, l, 2500, 4000, 2500)
	for _, floor := range []int{3000, 4000, 10000, 2500, 7777} {
		row := s0166Row{path: filepath.Join(l.banded, "heightless.mkv"), height: 720, floor: floor, recorded: true}
		got := s0166Survey(t, cfg, []s0166Row{row})
		if want := (store.DecisionInputsSurvey{NoSourceHeight: 1}); got != want {
			t.Errorf("recorded floor %d, no stored height: survey = %+v, want %+v", floor, got, want)
		}
		if got.Reopening() != 0 {
			t.Errorf("recorded floor %d: the no-source-height row is counted in Reopening (%d)", floor, got.Reopening())
		}
	}
}

// TestS0166AC6_AnUnbandedRootIsClassifiedAsBefore grades AC-6: on a root with no rules, or
// whose rule carries no height bound, a row is classified exactly as it was before this
// change - by the root's resolution, whatever height it stored or did not store - and never
// lands in the no-source-height figure.
func TestS0166AC6_AnUnbandedRootIsClassifiedAsBefore(t *testing.T) {
	l := s0166Roots(t)
	cfg := s0166Config(t, l, 2500, 4000, 2600)
	for _, tc := range []struct {
		row  s0166Row
		want store.DecisionInputsSurvey
	}{
		{s0166Row{path: filepath.Join(l.unruled, "a.mkv"), height: 720, stored: true, floor: 2500, recorded: true},
			store.DecisionInputsSurvey{Matching: 1}},
		{s0166Row{path: filepath.Join(l.unruled, "b.mkv"), height: 720, stored: false, floor: 2500, recorded: true},
			store.DecisionInputsSurvey{Matching: 1}},
		{s0166Row{path: filepath.Join(l.unruled, "c.mkv"), height: 720, stored: false, floor: 3000, recorded: true},
			store.DecisionInputsSurvey{Moved: 1}},
		{s0166Row{path: filepath.Join(l.unbounded, "d.mkv"), height: 2160, stored: true, floor: 2600, recorded: true},
			store.DecisionInputsSurvey{Matching: 1}},
		{s0166Row{path: filepath.Join(l.unbounded, "e.mkv"), height: 0, stored: false, floor: 2600, recorded: true},
			store.DecisionInputsSurvey{Matching: 1}},
		{s0166Row{path: filepath.Join(l.unbounded, "f.mkv"), height: 0, stored: false, floor: 2500, recorded: true},
			store.DecisionInputsSurvey{Moved: 1}},
	} {
		got := s0166Survey(t, cfg, []s0166Row{tc.row})
		if got != tc.want {
			t.Errorf("%s (stored height %v): survey = %+v, want %+v", tc.row.path, tc.row.stored, got, tc.want)
		}
		// Before this change the resolver answered from the path alone; for these roots the
		// stored height must not move that answer.
		withHeight, _, determined := DecisionInputsPerPath(cfg)(tc.row.path, ptr(tc.row.height))
		without, _, _ := DecisionInputsPerPath(cfg)(tc.row.path, nil)
		if !determined || withHeight.Encode() != without.Encode() {
			t.Errorf("%s: the stored height changed the resolution of a root that does not band on it", tc.row.path)
		}
	}
}

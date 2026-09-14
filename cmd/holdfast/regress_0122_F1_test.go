package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0122 refuter finding F1 at the operator's surface, against [AC-7].
//
// The same ledger TestValidateInputs_ReportsWhatItCouldNotResolveAndStillPasses grades,
// with one difference: the unrooted row records NOTHING, which is what every row of a
// ledger an earlier build wrote records. `validate` then prints the re-open count with no
// mention of the tree it can no longer find, so the operator is told the next scan will
// offer back a file the scan will never enumerate, and is given no path to go and look at.
func TestRegressS0122F1_ValidateSaysNothingAboutAnUnrootedRowThatRecordedNothing(t *testing.T) {
	cfgPath, state, lib := profiledLedgerConfig(t)
	cfg := loadConfigFile(t, cfgPath)
	perPath := engine.DecisionInputsPerPath(*cfg)

	here, rooted := perPath(filepath.Join(lib, "here.mkv"))
	if !rooted {
		t.Fatal("the library root resolved nothing, so this fixture is not the one described")
	}
	if _, rooted := perPath("/gone/film.mkv"); rooted {
		t.Fatal("/gone/film.mkv resolved to a configured root, so this case is not about an unrooted row")
	}
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, filepath.Join(lib, "here.mkv"), store.Skipped, engine.SkipLowBitrate, here)
		// The row an earlier build left, under a root that has since been renamed away.
		seedTerminalRow(t, st, "/gone/film.mkv", store.Skipped, engine.SkipLowBitrate, store.DecisionInputs{})
	})

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "1 terminal row(s) record no decision inputs") {
		t.Fatalf("the fixture is not the one this case describes:\n%s", out)
	}
	for _, want := range []string{"lie under no configured library root", "/gone/film.mkv"} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q. It has just told the operator that the next scan "+
				"re-opens this row; the scan walks the configured roots and will never reach it, and "+
				"nothing in the report names the tree that went away:\n%s", want, out)
		}
	}
}

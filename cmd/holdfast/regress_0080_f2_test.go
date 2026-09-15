package main

import (
	"strings"
	"testing"
)

// REFUTER PROBE for S0080, finding F2. It documents behaviour; it does not fix anything.
//
// The contract: "WHEN `holdfast validate` runs and the configured state directory holds a
// `jobs` database, THE SYSTEM SHALL report the same two counts and exit zero" - the two
// counts being how many terminal rows record inputs that differ from the current
// configuration and how many record no inputs at all.
//
// The population that criterion exists for most sharply is the ledger written by the
// PREVIOUS build: every row in it records no inputs, so the first scan after the upgrade
// re-opens all of them. reportLedgerAgainstConfig opens the ledger through
// store.OpenReadOnly, which refuses a schema older than this build's, so on exactly that
// ledger `validate` reports neither count.
func TestRegress0080F2_ValidateReportsNoCountsForALedgerTheShippedBuildWrote(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	seedOlderLedger(t, state)
	if got := readSchemaStamp(t, state+"/jobs.db"); got != olderSchemaVersion {
		t.Fatalf("the fixture is at schema %d, want %d - it is not a previous build's ledger",
			got, olderSchemaVersion)
	}

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	t.Logf("exit=%d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0", code)
	}

	// Two terminal rows, both recording nothing, and the next scan will re-open both.
	for _, want := range []string{
		"terminal row(s) were taken under a configuration that has since moved",
		"terminal row(s) record no decision inputs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q for a ledger written by the previous build; an "+
				"operator running validate on upgrade day is told nothing about the library the "+
				"first scan is about to re-open:\n%s", want, out)
		}
	}
}

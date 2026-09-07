package main

// Refuter artifact for S0049-holdfast-undo-6, finding F2 (ADVISORY - it documents a
// measurable consequence of a declared deviation, not a blocking defect).
//
// AC5: "IF the undo window is disabled THEN THE SYSTEM SHALL say so at startup,
// because that is the configuration in which a swap is final."
//
// The spec's task 1 said to put the announcement in Warnings(); the implementation put
// it in a new Notices() list logged at INFO. Warnings() is logged at WARN and would
// therefore have survived `log_level: warn`, which is a legal configuration
// (config.Validate accepts debug|info|warn|error). This test measures what the
// deviation costs: at `log_level: warn` the daemon starts, swaps a file, and says
// nothing at all about the window being disabled. The `validate` half of AC5 is
// unaffected (it prints to stdout, not through the logger).

import (
	"bytes"
	"strings"
	"testing"
)

func TestRegress0049F2_TheDisabledWindowAnnouncementVanishesAtLogLevelWarn(t *testing.T) {
	// Control first: at the default level the announcement is there, so a failure
	// below is the level and not a broken fixture.
	t.Run("control: log_level info announces it", func(t *testing.T) {
		cfgPath, _, _, _ := preflightLibrary(t, "vmaf_enable: false\nlog_level: info\n")
		got := captureStderr(t, func() {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
				t.Errorf("run exited %d: %s", code, errOut.String())
			}
		})
		for _, want := range undoDisabledPhrases {
			if !strings.Contains(got, want) {
				t.Fatalf("the control lost the announcement at log_level info (%q):\n%s", want, got)
			}
		}
	})

	t.Run("log_level warn says nothing about the window", func(t *testing.T) {
		cfgPath, _, _, _ := preflightLibrary(t, "vmaf_enable: false\nlog_level: warn\n")
		got := captureStderr(t, func() {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
				t.Errorf("run exited %d: %s", code, errOut.String())
			}
		})
		for _, want := range undoDisabledPhrases {
			if !strings.Contains(got, want) {
				t.Errorf("AC5: at log_level warn the startup output does not contain %q; a swap ran anyway. stderr was:\n%s",
					want, got)
			}
		}
	})
}

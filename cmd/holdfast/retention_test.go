package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ORDERING half of criterion 9 (LEDGER-5): a refused retention stops the process
// "before it opens the job store".
//
// internal/config/retention_test.go proves the refusal itself - that Load and Validate
// reject a negative or non-integer value, naming the key and the value. It cannot prove
// WHEN, because the ordering is cmd's: loadConfig runs first and buildEngine (which calls
// store.Open) runs only if it returned a config. This asserts that from the outside, on
// the real dispatch path, by looking for the artefact a store open leaves behind.
//
// It matters because store.Open CREATES the database and its parent directory. A refusal
// that happened afterwards would leave a jobs.db (and a WAL) in a state directory the
// operator's configuration was just rejected for - state created by a run that refused to
// run.

func TestRetention_ARefusedRetentionOpensNoJobStore(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"a negative retention", "-5"},
		{"a fractional retention", "3.7"},
		{"a retention that is not a number at all", "many"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "library")
			stateDir := filepath.Join(dir, "state")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatalf("mkdir library: %v", err)
			}
			cfgPath := filepath.Join(dir, "config.yaml")
			body := "library_roots:\n  - " + root + "\nstate_dir: " + stateDir +
				"\nhistory_retention_rows: " + c.value + "\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			for _, cmd := range []string{"run", "serve", "validate", "export"} {
				var out, errBuf bytes.Buffer
				code := dispatch([]string{cmd, "--config", cfgPath}, &out, &errBuf)
				if code == 0 {
					t.Errorf("%s accepted history_retention_rows: %s", cmd, c.value)
				}
				if !strings.Contains(errBuf.String(), "history_retention_rows") {
					t.Errorf("%s refused without naming the key: %q", cmd, errBuf.String())
				}
				if !strings.Contains(errBuf.String(), c.value) {
					t.Errorf("%s refused without naming the offending value %q: %q", cmd, c.value, errBuf.String())
				}
			}

			// The whole point: nothing was created. store.Open would have made this
			// directory and a jobs.db inside it.
			if _, err := os.Stat(stateDir); err == nil {
				ents, _ := os.ReadDir(stateDir)
				names := make([]string, 0, len(ents))
				for _, e := range ents {
					names = append(names, e.Name())
				}
				t.Errorf("a refused retention created the state directory %s (holding %v); the refusal must "+
					"land before the job store is opened", stateDir, names)
			}
		})
	}
}

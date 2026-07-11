package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatch(t *testing.T) {
	dir := t.TempDir()
	goodCfg := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(goodCfg, []byte("library_roots:\n  - /mnt/media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badCfg := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badCfg, []byte("library_roots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // substring in stdout
	}{
		{"no args", nil, 2, ""},
		{"version", []string{"version"}, 0, "transcode "},
		{"unknown command", []string{"frobnicate"}, 2, ""},
		{"validate ok", []string{"validate", "--config", goodCfg}, 0, "config OK"},
		{"validate missing flag", []string{"validate"}, 2, ""},
		{"validate bad config", []string{"validate", "--config", badCfg}, 1, ""},
		{"run refuses to touch files", []string{"run", "--config", goodCfg}, 0, "No files were touched"},
		{"run bad config", []string{"run", "--config", badCfg}, 1, ""},
		{"subcommand help exits zero", []string{"validate", "-h"}, 0, ""},
		{"top-level help exits zero", []string{"help"}, 0, "Usage"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := dispatch(tc.args, &out, &errOut)
			if code != tc.wantCode {
				t.Fatalf("dispatch(%v) code = %d, want %d (stderr: %s)", tc.args, code, tc.wantCode, errOut.String())
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("dispatch(%v) stdout = %q, want substring %q", tc.args, out.String(), tc.wantOut)
			}
		})
	}
}

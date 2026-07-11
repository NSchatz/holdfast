package main

import (
	"bytes"
	"os"
	"os/exec"
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
	typoCfg := filepath.Join(dir, "typo.yaml")
	if err := os.WriteFile(typoCfg, []byte("library_roots:\n  - /mnt/media\ncrff: 22\n"), 0o600); err != nil {
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
		{"validate unknown key", []string{"validate", "--config", typoCfg}, 1, ""},
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

// TestRunEmptyDir exercises the full run wiring: a valid config pointing at an
// empty library root scans cleanly and exits 0 (no files, nothing to do). Requires
// ffmpeg/ffprobe on PATH (or TRANSCODE_FFMPEG/FFPROBE); skips otherwise, since this
// is the CLI-wiring check, not the engine safety proof (which fails loud instead).
func TestRunEmptyDir(t *testing.T) {
	if _, err := exec.LookPath(envOr("TRANSCODE_FFMPEG", "ffmpeg")); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("run empty dir code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
}

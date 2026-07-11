package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/transcode/internal/config"
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

// TestServeSmoke exercises the full serve wiring end-to-end: it binds a real
// listener, serves the embedded UI + API, runs an initial scan over an empty
// library, and shuts down when its context is cancelled. Uses runServer directly
// (context-driven) so no OS signal is involved. Requires ffmpeg (the encoder
// capability check runs); skips otherwise, since this is the serve-wiring check,
// not the engine safety proof.
func TestServeSmoke(t *testing.T) {
	if _, err := exec.LookPath(envOr("TRANSCODE_FFMPEG", "ffmpeg")); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	// Grab a free localhost port, then release it for the server to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nserver_addr: " + addr + "\nserver_auth_token: tok\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, discardLog(), io.Discard) }()

	base := "http://" + addr
	waitHTTP(t, base+"/api/summary", 3*time.Second)

	// The embedded UI serves from the binary.
	if bdy := httpGet(t, base+"/"); !strings.Contains(bdy, "<title>transcode</title>") {
		t.Fatalf("UI not served from binary: %q", bdy[:min(80, len(bdy))])
	}
	// TRANSCODE-8: metrics endpoint is served (default-on) and exposes our series.
	if bdy := httpGet(t, base+"/metrics"); !strings.Contains(bdy, "transcode_files_total") {
		t.Fatalf("/metrics did not expose transcode metrics: %q", bdy[:min(120, len(bdy))])
	}
	// A control action requires the token: without it, 403/401; with it, accepted.
	if code := httpPostCode(t, base+"/api/pause", "tok"); code != 200 {
		t.Fatalf("authorized pause: code %d, want 200", code)
	}
	// With a token configured, a missing bearer is 401 (403 is reserved for the
	// no-token-configured "control disabled" case).
	if code := httpPostCode(t, base+"/api/pause", ""); code != 401 {
		t.Fatalf("unauthenticated pause with token configured: code %d, want 401", code)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServer exit code = %d, want 0", code)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("runServer did not shut down after context cancel")
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server never became ready at %s", url)
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func httpPostCode(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

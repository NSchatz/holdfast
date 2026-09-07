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

	"github.com/NSchatz/holdfast/internal/config"
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
		{"version", []string{"version"}, 0, "holdfast "},
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
// ffmpeg/ffprobe on PATH (or HOLDFAST_FFMPEG/FFPROBE); skips otherwise, since this
// is the CLI-wiring check, not the engine safety proof (which fails loud instead).
func TestRunEmptyDir(t *testing.T) {
	if _, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg")); err != nil {
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
	if _, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg")); err != nil {
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
	if bdy := httpGet(t, base+"/"); !strings.Contains(bdy, "<title>holdfast</title>") {
		t.Fatalf("UI not served from binary: %q", bdy[:min(80, len(bdy))])
	}
	// TRANSCODE-8: metrics endpoint is served (default-on) and exposes our series.
	if bdy := httpGet(t, base+"/metrics"); !strings.Contains(bdy, "holdfast_files_total") {
		t.Fatalf("/metrics did not expose holdfast metrics: %q", bdy[:min(120, len(bdy))])
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

// ---- VMAF model preflight (GATE-4) -------------------------------------------

// preflightLibrary lays out a real library with one real, encodable source in it,
// plus a config that sets the VMAF knobs from extraCfg. The source is REAL on
// purpose: every case below asserts the run stopped before touching it, which only
// means something if a run that did NOT stop would have transcoded it.
func preflightLibrary(t *testing.T, extraCfg string) (cfgPath, lib, state, src string) {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg required for the model-preflight proof: %v", err)
	}
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	state = filepath.Join(dir, "state")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(lib, "movie.mkv")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "--", src).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\nmin_bitrate_kbps: 0\n" + extraCfg
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, lib, state, src
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestRun_RefusesAnUnresolvableVmafModelBeforeEncodingAnything is GATE-4's third and
// ninth acceptance criteria together: with the quality gate ENABLED and a model that
// does not resolve in this ffmpeg build, the process exits NONZERO, says which model
// was configured, leaves no encoded output and no swapped file behind, and takes that
// decision BEFORE the job store is opened.
//
// Why it exists at all: `vmaf.Available` only greps the filter list, which says the
// build HAS libvmaf and says nothing about whether it ships the MODEL libvmaf was
// asked for. Until this check, a typo in vmaf_model - or a build without the UHD
// model holdfast's own `auto` selects above 1440 lines - was discovered after a full
// library's worth of encoding, one rejected file at a time, because an unmeasurable
// encode is (correctly) never accepted.
//
// The store clause is the one that is easy to get wrong and easy to check: opened
// first, a refused run would leave a jobs.db, a WAL and a shared-memory file in a
// state directory the operator never got a run out of. The encoder capability check
// has the same shape and the same placement, which is why the preflight sits beside it.
func TestRun_RefusesAnUnresolvableVmafModelBeforeEncodingAnything(t *testing.T) {
	cfgPath, lib, state, src := preflightLibrary(t,
		"vmaf_enable: true\nvmaf_model: definitely_not_a_real_model\n")
	before := readAll(t, src)

	var out, errOut bytes.Buffer
	code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
	if code == 0 {
		t.Fatalf("run with an unresolvable vmaf_model exited 0 - the refusal must be LOUD "+
			"(stdout: %s)", out.String())
	}
	// The message names the CONFIGURED value, which is what the operator has to fix.
	// Naming only the resolved spec would send them looking for a string they never typed.
	msg := errOut.String()
	if !strings.Contains(msg, "definitely_not_a_real_model") {
		t.Errorf("the refusal must name the configured model; got: %s", msg)
	}
	if !strings.Contains(msg, "vmaf_model") {
		t.Errorf("the refusal must name the configuration key; got: %s", msg)
	}

	// Nothing was encoded and nothing was swapped.
	if !bytes.Equal(readAll(t, src), before) {
		t.Error("the source changed on a preflight refusal")
	}
	entries, err := os.ReadDir(lib)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "movie.mkv" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the library holds %v after a preflight refusal, want only the untouched source", names)
	}

	// And the decision came before the job store was opened: there is no state
	// directory at all.
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		var names []string
		if entries, rerr := os.ReadDir(state); rerr == nil {
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		t.Errorf("the state directory exists after a preflight refusal (%v) - the model check "+
			"must be taken BEFORE store.Open, so a refused run leaves no jobs.db behind", names)
	}
}

// The anti-vacuity control: the IDENTICAL layout with a model this build DOES ship
// starts, runs and exits 0 - so the refusal above is the model and not the fixture.
//
// It also settles the roadmap's open question about vmaf_4k_v0.6.1 by ASKING the
// build rather than assuming: `auto` can resolve to either built-in depending on
// output height, so the preflight checks both, and a build missing the UHD model
// reds here rather than four hours into somebody's 4K library.
func TestRun_ResolvableVmafModelStartsAndRuns(t *testing.T) {
	for _, model := range []string{"auto", "vmaf_v0.6.1", "version=vmaf_v0.6.1"} {
		t.Run(model, func(t *testing.T) {
			cfgPath, _, state, _ := preflightLibrary(t, "vmaf_enable: true\nvmaf_model: "+model+"\n")
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
				t.Fatalf("run with vmaf_model %q exited %d, want 0 (stderr: %s)", model, code, errOut.String())
			}
			// It really got past the preflight and opened the store.
			if _, err := os.Stat(filepath.Join(state, "jobs.db")); err != nil {
				t.Errorf("no jobs.db after a run that cleared the preflight: %v", err)
			}
		})
	}
}

// TestRun_DisabledGateDoesNotRefuseAnUnresolvableModel is GATE-4's tenth criterion.
// With vmaf_enable: false nothing will ever ask libvmaf for a model, so refusing the
// run over one would be refusing a configuration that cannot fail. The run must still
// SAY, loudly, that there is no perceptual gate - that is the strictly weakest
// setting this tool has, and the one an operator most needs told about.
func TestRun_DisabledGateDoesNotRefuseAnUnresolvableModel(t *testing.T) {
	cfgPath, _, state, _ := preflightLibrary(t,
		"vmaf_enable: false\nvmaf_model: definitely_not_a_real_model\n")

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("run with the gate DISABLED and an unresolvable model exited %d, want 0 "+
			"(stderr: %s)", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err != nil {
		t.Errorf("the run did not reach the store: %v", err)
	}

	// The run reports that there is no perceptual gate. `run` and `serve` log the same
	// warning set through logConfigWarnings; `validate` prints it to stdout, which is
	// where it is checkable without capturing slog.
	var vOut, vErr bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &vOut, &vErr); code != 0 {
		t.Fatalf("validate exited %d (stderr: %s)", code, vErr.String())
	}
	if !strings.Contains(vOut.String(), "there is NO perceptual gate") {
		t.Errorf("a run with the gate disabled must report that there is no perceptual gate; "+
			"got: %s", vOut.String())
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

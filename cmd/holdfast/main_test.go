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
	"github.com/NSchatz/holdfast/internal/sourceoffer"
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
	// The control token is reached BY REFERENCE (secrets K1): a literal here is a startup
	// refusal, so the smoke test writes the token into a file and points the key at it,
	// which is the shape a real deployment uses.
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("tok\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nserver_addr: " + addr + "\nserver_auth_token: file:" + tokenFile + "\n"
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

	// The root path serves the plain-text page: holdfast ships no frontend, so what a
	// browser gets at / is the endpoint banner and the AGPL section 13 source offer.
	// Both halves are asserted - a banner with no offer would be a licence failure the
	// binary is meant to make impossible.
	if bdy := httpGet(t, base+"/"); !strings.Contains(bdy, "/api/summary") ||
		!strings.Contains(bdy, sourceoffer.Label+": ") {
		t.Fatalf("root page not served from binary: %q", bdy[:min(160, len(bdy))])
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

// ---- UNDO-6: the startup announcement ---------------------------------------

// undoDisabledPhrases are the two things the announcement has to carry: WHICH setting
// it is about, and what it means for a swap. A message that named the key without
// saying the swap is final would be a line an operator skips.
var undoDisabledPhrases = []string{"undo_window_hours", "THE UNDO WINDOW IS DISABLED", "a swap is FINAL"}

// captureStderr redirects the PROCESS's stderr for the duration of fn and returns
// what was written to it. It is needed because the startup announcement goes through
// the real logger, which writes to os.Stderr: asserting on a buffer handed to
// dispatch would prove the announcement exists somewhere other than where an operator
// would ever see it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old }()
	fn()
	os.Stderr = old
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRun_SaysAtStartupWhenTheUndoWindowIsDisabled is UNDO-6's fifth criterion. With
// the window off - which is the DEFAULT, and therefore the case that matters most -
// the swap is final the microsecond it happens, and a tool that deletes originals has
// to say that out loud before it deletes one.
//
// Both spellings of "off" are graded: the key absent (a stranger's first config) and
// the key explicitly 0 (an operator who turned it off).
func TestRun_SaysAtStartupWhenTheUndoWindowIsDisabled(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"the key is absent (the shipped default)", ""},
		{"the key is explicitly zero", "undo_window_hours: 0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, _, _ := preflightLibrary(t, "vmaf_enable: false\n"+tc.extra)
			code := -1
			got := captureStderr(t, func() {
				var out, errOut bytes.Buffer
				code = dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
			})
			if code != 0 {
				t.Fatalf("run exited %d:\n%s", code, got)
			}
			for _, want := range undoDisabledPhrases {
				if !strings.Contains(got, want) {
					t.Errorf("the startup output does not contain %q:\n%s", want, got)
				}
			}
		})
	}

	// The control: with the window OPEN the announcement must be absent. Without this,
	// a message printed unconditionally would pass every assertion above while telling
	// an operator with a 24-hour window that their swaps are final.
	t.Run("control: an open window says nothing of the kind", func(t *testing.T) {
		cfgPath, _, _, _ := preflightLibrary(t, "vmaf_enable: false\nundo_window_hours: 24\n")
		got := captureStderr(t, func() {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 0 {
				t.Errorf("run exited %d: %s", code, errOut.String())
			}
		})
		if strings.Contains(got, "THE UNDO WINDOW IS DISABLED") {
			t.Errorf("a run with a 24h undo window announced that the window is disabled:\n%s", got)
		}
	})
}

// TestValidate_PrintsTheDisabledUndoWindow is the other half of the same criterion.
// `validate` is where an operator checks a configuration BEFORE pointing it at a
// library, so it has to say the same thing the daemon says at startup.
func TestValidate_PrintsTheDisabledUndoWindow(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - /mnt/media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range undoDisabledPhrases {
		if !strings.Contains(got, want) {
			t.Errorf("validate does not print %q:\n%s", want, got)
		}
	}

	// The control, again: an open window is not announced as a closed one.
	openCfg := filepath.Join(dir, "open.yaml")
	if err := os.WriteFile(openCfg, []byte("library_roots:\n  - /mnt/media\nundo_window_hours: 24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := dispatch([]string{"validate", "--config", openCfg}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "THE UNDO WINDOW IS DISABLED") {
		t.Errorf("validate announced a disabled window for a 24h one:\n%s", out.String())
	}
}

// TestValidate_PrintsThePathFiltersInForce is [AC-8]: `validate` prints, per library
// root, the patterns in force for that root, which layer supplied each list, and a count
// OF PATTERNS - and it exits 0 even when a directory beneath a root cannot be listed,
// because it describes a configuration and walks no library.
//
// The count is of patterns and never of matching files. A count of files is a number
// `validate` cannot produce without becoming a different command, and an operator would
// read a wrong one as "this is how much of my library is protected".
func TestValidate_PrintsThePathFiltersInForce(t *testing.T) {
	dir := t.TempDir()
	// A REAL library root with a directory beneath it that this process may not list.
	// If `validate` walked the library it would meet this; it must not, and it must
	// still exit 0.
	lib := filepath.Join(dir, "media")
	sealed := filepath.Join(lib, "sealed")
	if err := os.MkdirAll(sealed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	if _, err := os.ReadDir(sealed); err == nil {
		t.Fatal("the fixture directory is still listable, so this case would prove nothing about a walk")
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "exclude_paths:\n  - \"**/Extras\"\n  - \"**/Featurettes\"\n" +
		"library_roots:\n" +
		"  - path: " + lib + "\n" +
		"    include_paths:\n      - \"movies/**\"\n" +
		"  - path: " + filepath.Join(dir, "music") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d over a root with an unlistable directory beneath it: %s",
			code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		// Every pattern in force, per root.
		"**/Extras", "**/Featurettes", "movies/**",
		// The count, which is of PATTERNS.
		"2 pattern(s)", "1 pattern(s)",
		// Which layer supplied each list. The second root inherits the top-level
		// exclude list and has no include list at all.
		"exclude_paths", "include_paths",
		string(config.LayerTopLevel), string(config.LayerProfile), string(config.LayerDefault),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("validate does not print %q:\n%s", want, got)
		}
	}
	// Nothing here counts files. A figure that looked like one would be read as a
	// measurement of the library, which this command never opened.
	if strings.Contains(got, "file(s) matched") || strings.Contains(got, "files matched") {
		t.Errorf("validate reported a count of matching FILES; it walks no library:\n%s", got)
	}

	// The covers-nothing report, on stdout with the other things a valid configuration
	// has to say about itself, and still exit 0.
	stray := filepath.Join(dir, "stray.yaml")
	strayBody := "exclude_paths:\n  - \"/srv/elsewhere/**\"\nlibrary_roots:\n  - " + lib + "\n"
	if err := os.WriteFile(stray, []byte(strayBody), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := dispatch([]string{"validate", "--config", stray}, &out, &errOut); code != 0 {
		t.Fatalf("a pattern that covers nothing refused the configuration (exit %d): %s", code, errOut.String())
	}
	for _, want := range []string{"COVERS NOTHING", "/srv/elsewhere/**", lib} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("validate does not report the pattern that covers nothing (%q):\n%s", want, out.String())
		}
	}
}

// TestValidate_RefusesAMalformedPatternTheSameWay is [AC-6] at the command: `validate`
// refuses the same configuration a run refuses, in the same words, non-zero, on stderr.
// An operator checks a configuration here BEFORE pointing it at a library, so a
// refusal only the daemon makes is a refusal discovered too late.
func TestValidate_RefusesAMalformedPatternTheSameWay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - /mnt/media\nexclude_paths:\n  - \"movies/[4k/**\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut)
	if code == 0 {
		t.Fatalf("validate accepted a malformed pattern (stdout: %s)", out.String())
	}
	for _, want := range []string{"exclude_paths", "movies/[4k/**"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, errOut.String())
		}
	}
	// The same configuration refuses a RUN too, and the run is the one that would have
	// touched files.
	out.Reset()
	errOut.Reset()
	if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code == 0 {
		t.Fatalf("run accepted a malformed pattern (stdout: %s)", out.String())
	}
	if !strings.Contains(errOut.String(), "movies/[4k/**") {
		t.Errorf("the run's refusal does not name the pattern:\n%s", errOut.String())
	}
}

// TestValidate_PrintsTheStreamSelectionInForce is [AC-17]: `validate` prints, per library
// root, the resolved value of each of the four stream-selection keys and which layer
// supplied it, exits 0 on a valid configuration, and walks no library doing it.
//
// The resolved value is the only thing that answers "what will this root do to my files".
// A key may be a built-in default, a top-level choice or that root's own entry, and the
// value reads the same in all three cases - so the YAML cannot tell an operator which, and
// on these keys the wrong answer is a track that is gone from the replacement.
func TestValidate_PrintsTheStreamSelectionInForce(t *testing.T) {
	dir := t.TempDir()
	tv := filepath.Join(dir, "tv")
	anime := filepath.Join(dir, "anime")
	for _, r := range []string{tv, anime} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A real media file under a root, so "walks no library" has something to walk.
	media := filepath.Join(tv, "ep.mkv")
	if err := os.WriteFile(media, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(
		"library_roots:\n"+
			"  - path: "+tv+"\n"+
			"  - path: "+anime+"\n"+
			"    audio_languages: [jpn]\n"+
			"    remux_only: true\n"+
			"audio_languages: [eng]\n"+
			"keep_commentary: false\n"+
			"state_dir: "+stateDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d on a valid configuration: %s", code, errOut.String())
	}
	got := out.String()

	tvSection, animeSection := sectionFor(t, got, tv), sectionFor(t, got, anime)
	// Every one of the four is printed under EVERY root: a key an operator cannot see
	// resolved is one they find out about from the file that came back missing a track.
	for _, knob := range config.SelectionKnobs() {
		for name, section := range map[string]string{tv: tvSection, anime: animeSection} {
			if !strings.Contains(section, knob) {
				t.Errorf("the printed configuration for %s does not carry %q:\n%s", name, knob, section)
			}
		}
	}
	// The values, and the layer each came from. Three layers are represented, which is what
	// a printer that only ever wrote one of them would fail on.
	assertKnob(t, tvSection, "audio_languages", "eng", string(config.LayerTopLevel))
	assertKnob(t, animeSection, "audio_languages", "jpn", string(config.LayerProfile))
	assertKnob(t, tvSection, "subtitle_languages", "none", string(config.LayerDefault))
	assertKnob(t, tvSection, "keep_commentary", "false", string(config.LayerTopLevel))
	assertKnob(t, animeSection, "keep_commentary", "false", string(config.LayerTopLevel))
	assertKnob(t, tvSection, "remux_only", "false", string(config.LayerDefault))
	assertKnob(t, animeSection, "remux_only", "true", string(config.LayerProfile))

	// And it walked nothing: no state directory was created and the library is as it was.
	if _, err := os.Stat(stateDir); err == nil {
		t.Error("validate created the state directory: it describes a configuration and must not " +
			"open a ledger to do it")
	}
	entries, err := os.ReadDir(tv)
	if err != nil || len(entries) != 1 || entries[0].Name() != "ep.mkv" {
		t.Errorf("the library root is not as it was (%v, err=%v)", entries, err)
	}
}

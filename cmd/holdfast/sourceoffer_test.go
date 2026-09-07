package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/version"
	"github.com/NSchatz/holdfast/internal/webui"
)

const (
	forkValue    = "https://example.invalid/fork"
	hostileValue = `https://example.invalid/a?b="><img src=x onerror=1>`
)

// freeAddr grabs a localhost port and releases it, so a server can bind it and a test
// can prove that nothing did.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// writeServeConfig writes a valid serve config bound to addr and returns its path plus
// the state directory it names (which must not exist if serve refused before doing
// anything).
func writeServeConfig(t *testing.T, addr string) (cfgPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + stateDir + "\nserver_addr: " + addr + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir
}

func setSourceURL(t *testing.T, v string) {
	t.Helper()
	old := sourceoffer.URL
	sourceoffer.URL = v
	t.Cleanup(func() { sourceoffer.URL = old })
}

// requireWorkingEncoder skips a test that needs `serve` to get all the way up. Serve
// builds the engine before it listens, and buildEngine refuses to start on an ffmpeg
// whose configured encoder does not actually work - correctly, and loudly.
//
// The tests that use this measure the SERVE WIRING (which handler is mounted at the
// root, what bytes it returns), not the no-loss contract, so a codec-poor ffmpeg on a
// developer box is a reason to skip them. It is never a reason to skip the engine
// safety proof, which fails loud and must keep doing so. Every assertion that can be
// made without an encoder - the whole AC6 refusal set, and every rendering assertion
// in internal/webui and internal/server - runs unconditionally.
func requireWorkingEncoder(t *testing.T) {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobe := envOr("HOLDFAST_FFPROBE", "ffprobe")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := encoder.RequireAvailable(context.Background(), ffmpeg, ffprobe, "cpu"); err != nil {
		t.Skipf("this host's ffmpeg cannot encode with the default encoder, so `serve` cannot start: %v", err)
	}
}

// AC6: a build whose Corresponding Source URL is unusable REFUSES to start any
// listener that would serve the root path, exits non-zero, and names the rejected
// value. The refusal is the first thing the serve command does, ahead of every
// listener it can create and ahead of the engine, so neither root-serving branch - the
// dashboard nor the API-only page - can be reached with a rejected value (binding
// advisories F13 and F16: one validation, on the path both branches share).
//
// It needs no encoder: nothing runs before the refusal.
func TestServe_RefusesToStartOnARejectedSourceURL(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-a-url", "javascript:alert(1)", "//example.com"} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			setSourceURL(t, bad)
			addr := freeAddr(t)
			cfgPath, stateDir := writeServeConfig(t, addr)

			var out, errOut bytes.Buffer
			code := dispatch([]string{"serve", "--config", cfgPath}, &out, &errOut)

			if code == 0 {
				t.Fatalf("serve exited 0 with source URL %q; it must refuse", bad)
			}
			if !strings.Contains(errOut.String(), strconv.Quote(bad)) {
				t.Errorf("the failure message does not name the rejected value: %q", errOut.String())
			}
			// Nothing was served on the configured address: the port is still free.
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("something is listening on %s after the refusal: %v", addr, err)
			}
			_ = ln.Close()
			// And nothing happened at all before the refusal - no store was opened.
			if _, err := os.Stat(stateDir); err == nil {
				t.Errorf("the state directory %s was created before the refusal", stateDir)
			}
			// It never falls back to upstream, which would tell a fork's users that
			// upstream is the source of a binary it is not.
			if strings.Contains(errOut.String(), sourceoffer.Upstream) {
				t.Errorf("the refusal named upstream as a fall back: %q", errOut.String())
			}
		})
	}
}

// A command that starts no root-serving listener is unaffected by AC6: `version` and
// `validate` still work on a binary whose source URL was rejected. This is the
// converse of the refusal above and is why the check sits in runServer rather than in
// buildEngine, which `run` shares.
func TestRejectedSourceURL_DoesNotBreakNonServingCommands(t *testing.T) {
	setSourceURL(t, "not-a-url")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - /mnt/media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"version"}, {"validate", "--config", cfgPath}} {
		var out, errOut bytes.Buffer
		if code := dispatch(args, &out, &errOut); code != 0 {
			t.Errorf("%v exited %d, want 0 (stderr: %s)", args, code, errOut.String())
		}
	}
}

// AC4, over a real listener and a real HTTP fetch, on a host with no working encoder.
//
// Verification bullet 4 says: "run that binary's `version` subcommand and fetch the
// page from the same binary, stamped with version and commit values the test chooses,
// and compare the values found in each". That is exactly what happens here - the
// binary is this test binary, the subcommand is dispatched through the real
// command-line entry point, and the page is fetched over HTTP from the real dashboard
// handler this binary would mount. The one thing it does not exercise is `serve`
// standing the ENGINE up, which needs a working ffmpeg encoder;
// TestServe_ServesTheOfferAndTheVersionSubcommandAgrees below does that and skips
// where the host cannot. Split this way, AC4 is observed on every machine and the
// end-to-end path is still observed wherever the repo's own gate can run at all.
func TestVersionSubcommandAndTheServedPageNameTheSameBuild(t *testing.T) {
	setSourceURL(t, forkValue)
	oldV, oldC, oldD := version.Version, version.Commit, version.Date
	version.Version, version.Commit, version.Date = "v7.7.7-chosen", "0ddba11", "2026-05-06T07:08:09Z"
	t.Cleanup(func() { version.Version, version.Commit, version.Date = oldV, oldC, oldD })

	// The `version` subcommand of this same binary, through dispatch.
	var vout, verr bytes.Buffer
	if code := dispatch([]string{"version"}, &vout, &verr); code != 0 {
		t.Fatalf("version subcommand exited %d (stderr: %s)", code, verr.String())
	}
	banner := strings.TrimSpace(vout.String())
	if banner == "" {
		t.Fatal("the version subcommand printed nothing")
	}

	// The page from that same binary, over a real listener.
	ts := httptest.NewServer(webui.HandlerFor(sourceoffer.Current()))
	defer ts.Close()
	offer := dashboardOfferOf(t, httpGet(t, ts.URL+"/"))

	if !strings.Contains(offer, banner) {
		t.Errorf("the offer does not name the build the version subcommand reports.\n version: %q\n   offer: %s", banner, offer)
	}
	for _, want := range []string{"v7.7.7-chosen", "0ddba11"} {
		if !strings.Contains(banner, want) {
			t.Errorf("the version subcommand lost the chosen stamp %q: %q", want, banner)
		}
		if !strings.Contains(offer, want) {
			t.Errorf("the offer lost the chosen stamp %q: %s", want, offer)
		}
	}
	// AC5's converse on the same path: with a stamp applied, the offer shows it
	// rather than the unstamped defaults.
	for _, gone := range []string{"0.0.0-dev", "commit unknown"} {
		if strings.Contains(offer, gone) {
			t.Errorf("the offer shows the unstamped default %q on a stamped build: %s", gone, offer)
		}
	}
}

// AC1, AC4, AC7, AC11 end to end through the real router and a real listener, and
// AC6's converse: an absolute https value carrying HTML-significant characters STARTS
// and SERVES rather than exiting.
//
// AC4 is asserted the way Verification names it: the `version` subcommand of this same
// binary is run, with version and commit values the test chose, and the values it
// prints are compared against the ones found on the served page.
func TestServe_ServesTheOfferAndTheVersionSubcommandAgrees(t *testing.T) {
	requireWorkingEncoder(t)
	setSourceURL(t, hostileValue)

	oldV, oldC, oldD := version.Version, version.Commit, version.Date
	version.Version, version.Commit, version.Date = "v9.9.9-test", "cafed00d", "2026-01-02T03:04:05Z"
	t.Cleanup(func() { version.Version, version.Commit, version.Date = oldV, oldC, oldD })

	addr := freeAddr(t)
	cfgPath, _ := writeServeConfig(t, addr)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, discardLog(), io.Discard) }()

	base := "http://" + addr
	waitHTTP(t, base+"/api/summary", 10*time.Second)
	page := httpGet(t, base+"/")

	// The `version` subcommand of this same binary.
	var vout, verr bytes.Buffer
	if code := dispatch([]string{"version"}, &vout, &verr); code != 0 {
		t.Fatalf("version subcommand exited %d", code)
	}
	banner := strings.TrimSpace(vout.String())
	if banner == "" {
		t.Fatal("the version subcommand printed nothing")
	}
	if !strings.Contains(page, banner) {
		t.Errorf("the served page does not name the build the version subcommand reports (%q)", banner)
	}
	for _, want := range []string{"v9.9.9-test", "cafed00d"} {
		if !strings.Contains(banner, want) || !strings.Contains(page, want) {
			t.Errorf("the chosen stamp %q is not in both the version output and the page", want)
		}
	}
	// AC11: the value is served, escaped, and introduces no markup.
	if !strings.Contains(page, `href="https://example.invalid/a?b=&#34;&gt;&lt;img src=x onerror=1&gt;"`) {
		t.Error("the served page does not carry the escaped source URL as the link target")
	}
	if strings.Contains(page, "<img") {
		t.Error("the source URL introduced an img element into the served page")
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServer exit code = %d, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServer did not shut down after context cancel")
	}
}

// --- the -ldflags path itself -------------------------------------------------
//
// Everything above moves the source URL by assigning the package variable. That
// proves the RENDERING but not the STAMPING: `go build -X` on a symbol path that does
// not exist is silently ignored, so a typo in the Makefile or the Dockerfile would
// leave every build offering upstream while every unit test stayed green. These two
// tests compile a real binary with real -ldflags and observe it.

// buildStamped compiles cmd/holdfast with the given -X assignments and returns the
// binary's path.
func buildStamped(t *testing.T, sourceURL, ver, commit string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	const pkg = "github.com/NSchatz/holdfast/internal/"
	bin := filepath.Join(t.TempDir(), "holdfast")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	ld := strings.Join([]string{
		"-X " + pkg + "version.Version=" + ver,
		"-X " + pkg + "version.Commit=" + commit,
		"-X " + pkg + "sourceoffer.URL=" + sourceURL,
	}, " ")
	cmd := exec.Command("go", "build", "-ldflags="+ld, "-o", bin, "./cmd/holdfast")
	cmd.Dir = ".." + string(filepath.Separator) + ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// AC13's Makefile half and AC6, on a real stamped binary: the -ldflags symbol path
// actually lands, and a stamped-in rejected value makes `serve` exit non-zero with the
// value named. If the -X were ignored the binary would carry the upstream default,
// which is valid, and serve would get past this point.
func TestLdflags_StampedRejectedValueRefusesToServe(t *testing.T) {
	bin := buildStamped(t, "not-a-url", "v0.0.1-stamp", "abc1234")
	addr := freeAddr(t)
	cfgPath, _ := writeServeConfig(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--config", cfgPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the stamped binary served with a rejected source URL:\n%s", out)
	}
	if !strings.Contains(string(out), strconv.Quote("not-a-url")) {
		t.Errorf("the stamped binary's refusal does not name the rejected value:\n%s", out)
	}
	if ln, lerr := net.Listen("tcp", addr); lerr != nil {
		t.Errorf("something is listening on %s after the refusal: %v", addr, lerr)
	} else {
		_ = ln.Close()
	}
}

// AC3, AC4 and AC13's Makefile half on a real stamped binary: build once with a source
// URL sharing no substring with upstream, serve, and assert the link target and its
// displayed text are both that value, that the upstream URL is nowhere in the offer,
// and that the identity is the one the same binary's `version` subcommand prints.
func TestLdflags_StampedForkValueIsServedAndUpstreamIsAbsent(t *testing.T) {
	requireWorkingEncoder(t)
	bin := buildStamped(t, forkValue, "v1.2.3-stamp", "deadbee")

	// The version subcommand of that same binary.
	verOut, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", bin, err)
	}
	banner := strings.TrimSpace(string(verOut))
	if !strings.Contains(banner, "v1.2.3-stamp") || !strings.Contains(banner, "deadbee") {
		t.Fatalf("the -ldflags version stamp did not land: %q", banner)
	}

	addr := freeAddr(t)
	cfgPath, _ := writeServeConfig(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--config", cfgPath)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	waitHTTP(t, "http://"+addr+"/api/summary", 30*time.Second)
	page := httpGet(t, "http://"+addr+"/")

	offer := dashboardOfferOf(t, page)
	wantLink := `<a class="source-offer-link" href="` + forkValue + `">` + forkValue + `</a>`
	if !strings.Contains(offer, wantLink) {
		t.Errorf("the stamped fork value is not both the link target and the displayed text: %s", offer)
	}
	if strings.Contains(offer, sourceoffer.Upstream) {
		t.Errorf("the upstream URL occurs inside a stamped fork build's offer: %s", offer)
	}
	if !strings.Contains(offer, banner) {
		t.Errorf("the offer does not carry the identity `version` prints (%q): %s", banner, offer)
	}
}

func dashboardOfferOf(t *testing.T, body string) string {
	t.Helper()
	const open = `<p class="source-offer">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("the served page carries no source offer:\n%s", body[:min(400, len(body))])
	}
	rest := body[i:]
	j := strings.Index(rest, "</p>")
	if j < 0 {
		t.Fatal("the source offer element is not closed")
	}
	return rest[:j+len("</p>")]
}

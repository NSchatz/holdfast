package main

// S0175 server-warning-scope: the two statements about the read surface are made by the
// command that can vouch for them and by no other.
//
// `serve` binds the address and has every layer the token can arrive through, so it states
// the exposure as a fact. `validate` sees the file and its own environment only, so it says
// the token is not set IN THIS CONFIG and may be supplied where `serve` runs. `run`, in each
// of its shapes, binds no listener and says nothing about a surface it does not open.
//
// Every test here grades a REAL PROCESS: the test binary re-execs itself as the CLI
// (subprocessEnv, see TestMain), so what is read is what `dispatch` wrote to the streams an
// operator's shell would see, under an environment the test chose. For `serve` that is the
// whole startup path - cmdServe's own logging, then the listener - and never a helper called
// beside it: the in-process serve tests call runServer directly and so skip exactly the
// lines this spec is about.

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/hwdevice"
)

const (
	readTokenEnv  = "HOLDFAST_SERVER_READ_TOKEN"
	serverAddrEnv = "HOLDFAST_SERVER_ADDR"
	// noticeScopeBind is a non-loopback server_addr for the commands that never bind it.
	noticeScopeBind = "0.0.0.0:8099"
)

// noticeScopeEnv is the environment every child here starts from: neither key this spec
// turns on is inherited from whoever ran the tests, so "unset" below means unset. A case
// that wants one set, or present and empty, overrides it.
func noticeScopeEnv(over map[string]*string) map[string]*string {
	env := map[string]*string{readTokenEnv: nil, serverAddrEnv: nil}
	for k, v := range over {
		env[k] = v
	}
	return env
}

// linesSaying returns the lines of a stream that contain phrase, case-insensitively. The
// criteria are all about LINES: a statement is one record, and "no line contains" is a
// claim that must name the line when it fails.
func linesSaying(stream, phrase string) []string {
	var out []string
	for _, line := range strings.Split(stream, "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(phrase)) {
			out = append(out, line)
		}
	}
	return out
}

// assertNoLine fails, naming the line, when any line of stream contains any phrase.
func assertNoLine(t *testing.T, ac, what, stream string, phrases ...string) {
	t.Helper()
	for _, p := range phrases {
		for _, line := range linesSaying(stream, p) {
			t.Errorf("%s: %s carries a line containing %q:\n%s", ac, what, p, line)
		}
	}
}

// assertLineSays fails for every phrase the one line does not contain, case-insensitively.
func assertLineSays(t *testing.T, ac, line string, phrases ...string) {
	t.Helper()
	low := strings.ToLower(line)
	for _, p := range phrases {
		if !strings.Contains(low, strings.ToLower(p)) {
			t.Errorf("%s: the statement never says %q:\n%s", ac, p, line)
		}
	}
}

// assertLineNeverSays is assertLineSays's converse.
func assertLineNeverSays(t *testing.T, ac, line string, phrases ...string) {
	t.Helper()
	low := strings.ToLower(line)
	for _, p := range phrases {
		if strings.Contains(low, strings.ToLower(p)) {
			t.Errorf("%s: the statement says %q, which it must not:\n%s", ac, p, line)
		}
	}
}

// noticeScopeConfig writes a valid configuration over a present, empty library, plus extra.
// An empty addr leaves server_addr ABSENT, which is the shipped default and not the same
// thing as any value that could be written there.
func noticeScopeConfig(t *testing.T, addr, extra string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	if addr != "" {
		body += "server_addr: \"" + addr + "\"\n"
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(body+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// readTokenRef writes a token file and returns the `file:` reference to it and the value it
// holds, so a test can prove neither one reaches a statement.
func readTokenRef(t *testing.T) (ref, value string) {
	t.Helper()
	value = "s0175-read-token-value-never-logged"
	p := filepath.Join(t.TempDir(), "read-token")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file:" + p, value
}

// ---- serve: the real startup, in a real process -------------------------------------------

// serveStartup is what one real `serve` said while it started.
type serveStartup struct {
	// stderr is everything the process wrote to its stderr, to its exit.
	stderr string
	// answered is whether the listener answered a request, and atFirstAnswer is the stderr
	// as it stood when the first answer arrived - so a line found there was written BEFORE
	// the listener accepted that request, which is the ordering AC-2 names.
	answered      bool
	atFirstAnswer string
}

// hostCanServe reports whether `serve` can come all the way up here: it builds the engine
// before it listens, and the engine refuses an ffmpeg whose default encoder does not work.
// It is asked so that a server that did NOT come up on a host where it can is a failure,
// and never so that a statement goes ungraded - the read-surface lines are logged ahead of
// the engine and are graded on every host.
func hostCanServe() bool {
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return false
	}
	return requireEncoder(context.Background(), &config.Config{Encoder: "cpu", CRF: 28, Preset: "ultrafast", PixelFormat: "auto"},
		ffmpeg, envOr("HOLDFAST_FFPROBE", "ffprobe"), "cpu", hwdevice.Assignment{}) == nil
}

// startServe runs `serve` over cfgPath as a child process until its listener answers the
// root path (which no token gates), then stops it with SIGTERM and returns what it wrote.
// probe is the loopback address the bind is reachable at.
func startServe(t *testing.T, cfgPath, probe string, env map[string]*string) serveStartup {
	t.Helper()
	cmd := exec.Command(os.Args[0], "serve", "--config", cfgPath)
	env = noticeScopeEnv(env)
	for _, kv := range os.Environ() {
		if _, set := env[strings.SplitN(kv, "=", 2)[0]]; !set {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, subprocessEnv+"=1")
	for k, v := range env {
		if v != nil {
			cmd.Env = append(cmd.Env, k+"="+*v)
		}
	}
	var errOut lockedBuffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting serve: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	var got serveStartup
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(120 * time.Second)
poll:
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			break poll
		default:
		}
		if resp, err := client.Get("http://" + probe + "/"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				got.answered, got.atFirstAnswer = true, errOut.String()
				break poll
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Errorf("serve did not stop within 60s of SIGTERM")
	}
	got.stderr = errOut.String()
	if !got.answered {
		if hostCanServe() {
			t.Fatalf("serve never answered on %s although this host can serve. stderr:\n%s", probe, got.stderr)
		}
		t.Logf("this host's ffmpeg cannot encode, so serve stopped before its listener; the startup " +
			"statements are logged ahead of the engine and are graded all the same")
	}
	return got
}

// serveBinds returns a non-loopback bind on a free port and the loopback address it is
// reachable at.
func serveBinds(t *testing.T) (bind, probe string) {
	t.Helper()
	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	return "0.0.0.0:" + port, "127.0.0.1:" + port
}

// assertServeStatesTheOpenLibrary is AC-2's whole assertion over one startup, shared with
// AC-7 because AC-7 requires the SAME line: exactly one WARN record naming server_read_token,
// naming server_addr and the bind, stating the exposure unhedged, written before the
// listener answered.
func assertServeStatesTheOpenLibrary(t *testing.T, ac string, got serveStartup, bind string) {
	t.Helper()
	lines := linesSaying(got.stderr, "server_read_token")
	if len(lines) != 1 {
		t.Fatalf("%s: serve logged %d line(s) naming server_read_token, want exactly 1. stderr:\n%s",
			ac, len(lines), got.stderr)
	}
	line := lines[0]
	assertLineSays(t, ac, line, "level=WARN", "server_addr", bind, "every media path", "without a credential",
		"is served without a credential", "no library datum")
	assertLineNeverSays(t, ac, line, "not set in this config", "may be supplied by", "ships no frontend")
	if got.answered {
		if !strings.Contains(got.atFirstAnswer, line) {
			t.Errorf("%s: the statement was not on stderr when the listener first answered a request", ac)
		}
		listening := strings.Index(got.stderr, `msg="serve listening"`)
		if listening < 0 || strings.Index(got.stderr, line) > listening {
			t.Errorf("%s: the statement does not precede the `serve listening` record. stderr:\n%s", ac, got.stderr)
		}
	}
}

// TestS0175_AC2_ServeStatesTheOpenLibraryUnhedged: `serve` on a non-loopback bind with the
// token empty after every layer logs the exposure at WARN, as a fact, before it listens.
// This is the statement that must never go quiet: it is the one that tells an operator
// their media paths are served without a credential on a host that also deletes originals.
func TestS0175_AC2_ServeStatesTheOpenLibraryUnhedged(t *testing.T) {
	bind, probe := serveBinds(t)
	got := startServe(t, noticeScopeConfig(t, bind, ""), probe, nil)
	assertServeStatesTheOpenLibrary(t, "AC-2", got, bind)
}

// TestS0175_AC6_ServeWithATokenStatesWhatIsStillOpen: with the token set - in the file, or
// in serve's own environment - `serve` logs the set-token statement at WARN and nothing
// about every media path. The statement names the key and carries neither the reference
// nor the value it resolves to.
func TestS0175_AC6_ServeWithATokenStatesWhatIsStillOpen(t *testing.T) {
	for _, from := range []string{"file", "environment"} {
		t.Run("token from the "+from, func(t *testing.T) {
			ref, value := readTokenRef(t)
			bind, probe := serveBinds(t)
			var extra string
			var env map[string]*string
			if from == "file" {
				extra = "server_read_token: " + ref + "\n"
			} else {
				env = map[string]*string{readTokenEnv: ptr(ref)}
			}
			got := startServe(t, noticeScopeConfig(t, bind, extra), probe, env)

			lines := linesSaying(got.stderr, "server_read_token is set")
			if len(lines) != 1 {
				t.Fatalf("AC-6: serve logged %d set-token line(s), want exactly 1. stderr:\n%s", len(lines), got.stderr)
			}
			assertLineSays(t, "AC-6", lines[0], "level=WARN", "root path at /", "/metrics", "without a credential",
				"no library datum")
			assertLineNeverSays(t, "AC-6", lines[0], "ships no frontend")
			assertNoLine(t, "AC-6", "serve's stderr", got.stderr, "every media path", "not set in this config")
			// Observability O6: no startup record carries the credential, and the statement
			// itself does not carry the reference either.
			assertLineNeverSays(t, "AC-6", lines[0], ref)
			assertNoLine(t, "AC-6", "serve's stderr", got.stderr, value)
			if got.answered && !strings.Contains(got.atFirstAnswer, lines[0]) {
				t.Errorf("AC-6: the statement was not on stderr when the listener first answered a request")
			}
		})
	}
}

// TestS0175_AC5_LoopbackBindsSayNothingAboutTheReadSurface: on a loopback bind with no
// token, neither `serve` nor `validate` says anything about every media path or about a
// key not being set. That is the shipped default, and a statement on the default is the
// cry-wolf this spec removes.
//
// `validate` is graded on the absent server_addr too. `serve` is graded on the loopback
// spellings it can bind a free port on: an absent server_addr binds 127.0.0.1:8080, which
// a test must not take on a host it shares.
func TestS0175_AC5_LoopbackBindsSayNothingAboutTheReadSurface(t *testing.T) {
	silent := []string{"every media path", "not set in this config"}

	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"", "127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port, "[::1]:" + port} {
		t.Run("validate addr="+addr, func(t *testing.T) {
			code, out, errOut := cliProcessEnv(t, noticeScopeEnv(nil), "validate", "--config", noticeScopeConfig(t, addr, ""))
			if code != 0 {
				t.Fatalf("AC-5: validate exited %d: %s", code, errOut)
			}
			assertNoLine(t, "AC-5", "validate's output", out+errOut, append(silent, "server_read_token")...)
			// Anti-vacuity: the report was printed, so the silence is about the read surface.
			if len(linesSaying(out, "undo_window_hours is 0")) != 1 {
				t.Errorf("AC-5: validate did not print its notes at all:\n%s", out)
			}
		})
	}

	for _, host := range []string{"127.0.0.1", "localhost"} {
		t.Run("serve addr="+host, func(t *testing.T) {
			_, port, err := net.SplitHostPort(freeAddr(t))
			if err != nil {
				t.Fatal(err)
			}
			got := startServe(t, noticeScopeConfig(t, host+":"+port, ""), "127.0.0.1:"+port, nil)
			assertNoLine(t, "AC-5", "serve's stderr", got.stderr, append(silent, "server_read_token")...)
			if len(linesSaying(got.stderr, "undo_window_hours is 0")) != 1 {
				t.Errorf("AC-5: serve did not log its startup statements at all:\n%s", got.stderr)
			}
		})
	}
}

// ---- validate -----------------------------------------------------------------------------

// assertValidateHedges is AC-3's whole assertion over one `validate`, shared with AC-7:
// exit 0, exactly one `note:` line naming server_read_token, saying what `validate` can
// observe and stating the exposure only as a consequence.
func assertValidateHedges(t *testing.T, ac string, code int, out, errOut, bind string) {
	t.Helper()
	if code != 0 {
		t.Fatalf("%s: validate exited %d, want 0 (stderr: %s)", ac, code, errOut)
	}
	lines := linesSaying(out, "server_read_token")
	if len(lines) != 1 {
		t.Fatalf("%s: validate printed %d line(s) naming server_read_token, want exactly 1:\n%s", ac, len(lines), out)
	}
	line := lines[0]
	if !strings.HasPrefix(line, "note: ") {
		t.Errorf("%s: the statement is not a `note:` line:\n%s", ac, line)
	}
	assertLineSays(t, ac, line,
		"not set in this config",           // (a)
		"may be supplied by", readTokenEnv, // (b)
		"server_addr", bind, // (c)
		"every media path", "without a credential", // (d)
		"no library datum")
	assertLineNeverSays(t, ac, line, "is served without a credential", "ships no frontend") // (e)
	// It is a report on stdout and nothing of it leaks to the other stream.
	assertNoLine(t, ac, "validate's stderr", errOut, "server_read_token")
}

// TestS0175_AC3_ValidateSaysNotSetInThisConfig: `validate` cannot see the environment of
// the process that will serve, so a token it cannot find is "not set in this config", may
// be supplied by HOLDFAST_SERVER_READ_TOKEN, and the exposure is what follows if it is
// unset there too - never an assertion that the library IS served without a credential.
func TestS0175_AC3_ValidateSaysNotSetInThisConfig(t *testing.T) {
	for _, bind := range []string{noticeScopeBind, ":8099", "192.168.1.10:8099", "holdfast.lan:8099"} {
		t.Run(bind, func(t *testing.T) {
			code, out, errOut := cliProcessEnv(t, noticeScopeEnv(nil), "validate", "--config", noticeScopeConfig(t, bind, ""))
			assertValidateHedges(t, "AC-3", code, out, errOut, bind)
		})
	}
}

// TestS0175_AC4_ValidateSeesATokenInItsOwnEnvironment: the file does not set the key and
// validate's own environment does. That IS a set token as far as this command can tell, so
// it prints the set-token note and neither the hedged statement nor the exposure.
func TestS0175_AC4_ValidateSeesATokenInItsOwnEnvironment(t *testing.T) {
	ref, _ := readTokenRef(t)
	code, out, errOut := cliProcessEnv(t, noticeScopeEnv(map[string]*string{readTokenEnv: ptr(ref)}),
		"validate", "--config", noticeScopeConfig(t, noticeScopeBind, ""))
	if code != 0 {
		t.Fatalf("AC-4: validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	lines := linesSaying(out, "server_read_token")
	if len(lines) != 1 {
		t.Fatalf("AC-4: validate printed %d line(s) naming server_read_token, want exactly 1:\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "note: ") {
		t.Errorf("AC-4: the set-token statement is not a `note:` line:\n%s", lines[0])
	}
	assertLineSays(t, "AC-4", lines[0], "server_read_token is set", "root path at /", "/metrics", "no library datum")
	assertLineNeverSays(t, "AC-4", lines[0], ref, "ships no frontend")
	assertNoLine(t, "AC-4", "validate's output", out+errOut, "not set in this config", "every media path")
}

// TestS0175_AC7_AnEmptyVariableIsAnUnsetToken: HOLDFAST_SERVER_READ_TOKEN present and EMPTY
// is not a token. `serve` states the open library exactly as AC-2 has it and `validate`
// hedges exactly as AC-3 has it: an empty variable that read as "set" would silence the one
// statement an operator with an unauthenticated library is owed.
func TestS0175_AC7_AnEmptyVariableIsAnUnsetToken(t *testing.T) {
	empty := map[string]*string{readTokenEnv: ptr("")}

	t.Run("serve", func(t *testing.T) {
		bind, probe := serveBinds(t)
		got := startServe(t, noticeScopeConfig(t, bind, ""), probe, empty)
		assertServeStatesTheOpenLibrary(t, "AC-7", got, bind)
		assertNoLine(t, "AC-7", "serve's stderr", got.stderr, "server_read_token is set")
	})

	t.Run("validate", func(t *testing.T) {
		code, out, errOut := cliProcessEnv(t, noticeScopeEnv(empty), "validate", "--config",
			noticeScopeConfig(t, noticeScopeBind, ""))
		assertValidateHedges(t, "AC-7", code, out, errOut, noticeScopeBind)
		assertNoLine(t, "AC-7", "validate's output", out, "server_read_token is set")
	})
}

// ---- run ----------------------------------------------------------------------------------

// TestS0175_AC1_RunSaysNothingAboutAReadSurfaceItDoesNotOpen: a oneshot run binds no
// listener, in any of its three shapes and whatever the token is, so nothing it writes
// names server_read_token or says anything is served without a credential. The undo-window
// statement is asserted in the same output, which is what proves the silence is scoped to
// the read surface rather than a startup that stopped announcing altogether.
//
// The library holds one real source and the run really transcodes it (preflightLibrary):
// the whole of stderr is graded, which is the startup output and everything after it.
func TestS0175_AC1_RunSaysNothingAboutAReadSurfaceItDoesNotOpen(t *testing.T) {
	for _, token := range []string{"unset", "set"} {
		for _, shape := range []string{"unbounded", "--limit", "--file"} {
			t.Run(shape+", token "+token, func(t *testing.T) {
				extra := "vmaf_enable: false\nserver_addr: \"" + noticeScopeBind + "\"\n"
				if token == "set" {
					ref, _ := readTokenRef(t)
					extra += "server_read_token: " + ref + "\n"
				}
				cfgPath, _, _, src := preflightLibrary(t, extra)
				args := []string{"run", "--config", cfgPath}
				switch shape {
				case "--limit":
					args = append(args, "--limit", "1")
				case "--file":
					args = append(args, "--file", src)
				}
				code, out, errOut := cliProcessEnv(t, noticeScopeEnv(nil), args...)
				if code != 0 {
					t.Fatalf("AC-1: %v exited %d: %s", args, code, errOut)
				}
				assertNoLine(t, "AC-1", "run's output", out+errOut, "server_read_token", "without a credential")
				undo := linesSaying(errOut, "undo_window_hours is 0")
				if len(undo) != 1 {
					t.Fatalf("AC-1: run logged %d undo-window line(s), want exactly 1 - the suppression is "+
						"scoped to the read surface, not blanket. stderr:\n%s", len(undo), errOut)
				}
				assertLineSays(t, "AC-1", undo[0], "level=WARN")
			})
		}
	}
}

// TestS0175_AC8_AMalformedServerAddrStillRefusesRunAndValidate: `run` no longer DESCRIBES
// the bind, and it still VALIDATES it. A server_addr that is no host:port refuses both
// commands with exit 1 and a stderr message naming the key, before anything else happens.
func TestS0175_AC8_AMalformedServerAddrStillRefusesRunAndValidate(t *testing.T) {
	for _, bad := range []string{"not-a-host-port", "0.0.0.0", "host:port:extra"} {
		for _, command := range []string{"run", "validate"} {
			t.Run(command+" "+bad, func(t *testing.T) {
				cfgPath := noticeScopeConfig(t, bad, "")
				code, out, errOut := cliProcessEnv(t, noticeScopeEnv(nil), command, "--config", cfgPath)
				if code != 1 {
					t.Fatalf("AC-8: %s with server_addr %q exited %d, want 1 (stdout: %s, stderr: %s)",
						command, bad, code, out, errOut)
				}
				if !strings.Contains(errOut, "server_addr") {
					t.Errorf("AC-8: the refusal does not name server_addr: %s", errOut)
				}
				if strings.Contains(out, "config OK") {
					t.Errorf("AC-8: %s reported a valid configuration: %s", command, out)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "state")); err == nil {
					t.Errorf("AC-8: %s created the state directory before refusing", command)
				}
			})
		}
	}
	// Anti-vacuity: the same file with a well-formed address validates, so the refusals
	// above are about the address.
	code, _, errOut := cliProcessEnv(t, noticeScopeEnv(nil), "validate", "--config", noticeScopeConfig(t, noticeScopeBind, ""))
	if code != 0 {
		t.Fatalf("AC-8: the control configuration did not validate (exit %d): %s", code, errOut)
	}
}

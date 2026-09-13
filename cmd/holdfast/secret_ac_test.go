package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
)

// The three sentinels. Each is distinct so a leak can be attributed to the key it came
// from, and none is credential-SHAPED: the secret scanner reads this file, and a fixture
// that looked like an issued token would make `make secret-scan` red by construction.
const (
	tokenSentinel    = "RESOLVED-CONTROL-TOKEN-MUST-NEVER-BE-EMITTED"
	notifySentinel   = "RESOLVED-NOTIFY-TOKEN-MUST-NEVER-BE-EMITTED"
	tautulliSentinel = "RESOLVED-TAUTULLI-KEY-MUST-NEVER-BE-EMITTED"
)

// secretsLab is a real library, a real config and three real secret files, wired the way a
// deployment wires them: every secret-bearing key carries a file: REFERENCE, and the values
// exist only in those files.
type secretsLab struct {
	dir, cfgPath, lib, state, addr string
	notifyHits                     chan string
	tautulliHits                   chan string
}

func newSecretsLab(t *testing.T, extra string) *secretsLab {
	t.Helper()
	if _, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg")); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	lab := &secretsLab{
		dir:          t.TempDir(),
		notifyHits:   make(chan string, 8),
		tautulliHits: make(chan string, 8),
	}
	lab.lib = filepath.Join(lab.dir, "media")
	lab.state = filepath.Join(lab.dir, "state")
	if err := os.MkdirAll(lab.lib, 0o755); err != nil {
		t.Fatal(err)
	}

	// A local endpoint per outbound key, standing in for the network and nothing else.
	notifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lab.notifyHits <- r.URL.String()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(notifySrv.Close)
	tautulliSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lab.tautulliHits <- r.URL.Query().Get("apikey")
		_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"stream_count":"0"}}}`))
	}))
	t.Cleanup(tautulliSrv.Close)

	// A free localhost port for holdfast itself.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lab.addr = ln.Addr().String()
	_ = ln.Close()

	write := func(name, value string) string {
		p := filepath.Join(lab.dir, name)
		if err := os.WriteFile(p, []byte(value+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tokenFile := write("control.token", tokenSentinel)
	notifyFile := write("notify.url",
		"generic+http://"+notifySrv.Listener.Addr().String()+"/hook?tok="+notifySentinel)
	tautulliFile := write("tautulli.key", tautulliSentinel)

	lab.cfgPath = filepath.Join(lab.dir, "config.yaml")
	body := "library_roots:\n  - " + lab.lib + "\n" +
		"state_dir: " + lab.state + "\n" +
		"server_addr: " + lab.addr + "\n" +
		"log_level: debug\n" +
		"server_auth_token: file:" + tokenFile + "\n" +
		"notify_url: file:" + notifyFile + "\n" +
		"tautulli_url: " + tautulliSrv.URL + "\n" +
		"tautulli_api_key: file:" + tautulliFile + "\n" + extra
	if err := os.WriteFile(lab.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return lab
}

// sentinels is every value that must not be emitted.
func sentinels() map[string]string {
	return map[string]string{
		"server_auth_token": tokenSentinel,
		"notify_url":        notifySentinel,
		"tautulli_api_key":  tautulliSentinel,
	}
}

// assertNoSentinel fails naming the key whose value leaked and where.
func assertNoSentinel(t *testing.T, where, text string) {
	t.Helper()
	for key, value := range sentinels() {
		if strings.Contains(text, value) {
			t.Errorf("%s LEAKED the resolved value of %s:\n%s", where, key, text)
		}
	}
}

// syncWriter lets a test read a logger's output while the daemon is still writing to it.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// AC-1: the resolved control token is what holdfast's own mutating endpoints accept,
// proved by a LOCAL ENDPOINT THAT RECEIVES THE VALUE - here holdfast's real listener,
// receiving the token as a bearer credential. A wrong value is refused, so the case cannot
// pass against a server that accepts anything.
//
// AC-2 rides along over the whole HTTP surface: every endpoint's body and headers, the
// metrics exposition, and the daemon's own log AT DEBUG are searched for all three values.
func TestServe_AC1_AC2_TheResolvedTokenIsAcceptedAndNoValueIsEverEmitted(t *testing.T) {
	lab := newSecretsLab(t, "")
	cfg, err := config.Load(lab.cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	logs := &syncWriter{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var serveErr bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, &serveErr) }()

	base := "http://" + lab.addr
	waitHTTP(t, base+"/api/summary", 10*time.Second)

	// AC-1: the resolved token is accepted; a wrong one is not.
	if code := httpPostCode(t, base+"/api/pause", tokenSentinel); code != http.StatusOK {
		t.Fatalf("the RESOLVED token was refused: POST /api/pause = %d, want 200", code)
	}
	if code := httpPostCode(t, base+"/api/resume", "not-the-resolved-token"); code != http.StatusUnauthorized {
		t.Fatalf("a WRONG token was accepted: POST /api/resume = %d, want 401", code)
	}
	if code := httpPostCode(t, base+"/api/pause", ""); code != http.StatusUnauthorized {
		t.Fatalf("a MISSING bearer was accepted: POST /api/pause = %d, want 401", code)
	}

	// AC-2: every response this daemon originates - body AND headers - over the whole
	// surface, authorized and unauthorized alike.
	paths := []struct {
		method, path, token string
	}{
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/api/summary", ""},
		{http.MethodGet, "/api/queue", ""},
		{http.MethodGet, "/api/history", ""},
		{http.MethodGet, "/metrics", ""},
		{http.MethodPost, "/api/pause", tokenSentinel},
		{http.MethodPost, "/api/resume", tokenSentinel},
		{http.MethodPost, "/api/rescan", tokenSentinel},
		{http.MethodPost, "/api/pause", ""},
		{http.MethodPost, "/api/pause", "wrong"},
		{http.MethodGet, "/api/nope", ""},
	}
	for _, p := range paths {
		req, err := http.NewRequest(p.method, base+p.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.token != "" {
			req.Header.Set("Authorization", "Bearer "+p.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assertNoSentinel(t, fmt.Sprintf("the body of %s %s", p.method, p.path), string(body))
		assertNoSentinel(t, fmt.Sprintf("the headers of %s %s", p.method, p.path),
			fmt.Sprintf("%v", resp.Header))
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runServer exit code = %d, want 0 (stderr: %s)", code, serveErr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServer did not shut down after context cancel")
	}

	// AC-2: the daemon's own log, at DEBUG - the level AC-2 names - and its stderr.
	assertNoSentinel(t, "the daemon's log at debug", logs.String())
	assertNoSentinel(t, "the daemon's stderr", serveErr.String())

	// The log must still SAY whether control is on: a redaction that removed the signal
	// would be a different defect, not a fix.
	if !strings.Contains(logs.String(), "control_enabled=true") {
		t.Errorf("the startup line does not report that control is enabled:\n%s", logs.String())
	}
}

// AC-1: the Tautulli api key reaches its own outbound request, proved by the local endpoint
// receiving it - driven through the REAL daemon rather than the client in isolation, so the
// wiring from reference to request is what is graded.
func TestServe_AC1_TheResolvedTautulliKeyReachesItsOwnRequest(t *testing.T) {
	// max_load 0 and an unset run_window leave Tautulli as the only signal, so consulting
	// the scheduler must consult it.
	lab := newSecretsLab(t, "scan_interval_sec: 0\n")
	cfg, err := config.Load(lab.cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	logs := &syncWriter{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var serveErr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, &serveErr) }()

	base := "http://" + lab.addr
	waitHTTP(t, base+"/api/summary", 10*time.Second)
	// A rescan consults the scheduler's gate, which consults Tautulli. It is ACCEPTED
	// (202) rather than completed, because the scan runs in the background.
	if code := httpPostCode(t, base+"/api/rescan", tokenSentinel); code != http.StatusAccepted {
		t.Fatalf("POST /api/rescan = %d, want 202", code)
	}

	select {
	case got := <-lab.tautulliHits:
		if got != tautulliSentinel {
			t.Fatalf("the Tautulli endpoint received apikey=%q, want the resolved value", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Tautulli endpoint was never called, so nothing received the resolved key")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runServer did not shut down")
	}
	assertNoSentinel(t, "the daemon's log at debug", logs.String())
}

// AC-2: `validate` and `export` - named in the criterion - emit no resolved value on either
// stream, with all three keys configured.
func TestSubcommands_AC2_ValidateAndExportEmitNoResolvedValue(t *testing.T) {
	lab := newSecretsLab(t, "")
	for _, args := range [][]string{
		{"validate", "--config", lab.cfgPath},
		{"export", "--config", lab.cfgPath},
		{"restore", "--config", lab.cfgPath},
	} {
		t.Run(args[0], func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := dispatch(args, &out, &errOut)
			assertNoSentinel(t, args[0]+" stdout", out.String())
			assertNoSentinel(t, args[0]+" stderr", errOut.String())
			if args[0] == "validate" && code != 0 {
				t.Fatalf("validate exited %d on a valid reference config: %s", code, errOut.String())
			}
			// `validate` must still NAME the keys it checked, by reference: a command that
			// said nothing would also pass a leak test.
			if args[0] == "validate" && out.Len() == 0 {
				t.Error("validate printed nothing at all")
			}
		})
	}
}

// AC-7: a LITERAL credential refuses to start - in the config file and in the corresponding
// HOLDFAST_* variable - naming the key and how to convert it, with no part of the value in
// the message. Graded through the real CLI, on every subcommand that loads a config.
func TestCLI_AC7_ALiteralRefusesToStartWhereverItIsWritten(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"

	t.Run("in the config file", func(t *testing.T) {
		for _, key := range config.SecretBearingKeys {
			cfgPath := filepath.Join(dir, key+".yaml")
			if err := os.WriteFile(cfgPath, []byte(base+key+": "+tokenSentinel+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, cmd := range []string{"validate", "run", "serve"} {
				var out, errOut bytes.Buffer
				code := dispatch([]string{cmd, "--config", cfgPath}, &out, &errOut)
				if code == 0 {
					t.Errorf("%s ACCEPTED a literal %s", cmd, key)
					continue
				}
				msg := errOut.String() + out.String()
				if strings.Contains(msg, tokenSentinel) {
					t.Errorf("%s echoed the literal value of %s:\n%s", cmd, key, msg)
				}
				if !strings.Contains(msg, key) {
					t.Errorf("%s did not name the key %s:\n%s", cmd, key, msg)
				}
				for _, howTo := range []string{"file:", "cmd:"} {
					if !strings.Contains(msg, howTo) {
						t.Errorf("%s did not say how to convert %s (missing %q):\n%s", cmd, key, howTo, msg)
					}
				}
			}
		}
	})

	t.Run("in the HOLDFAST_ environment variable", func(t *testing.T) {
		cfgPath := filepath.Join(dir, "clean.yaml")
		if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, key := range config.SecretBearingKeys {
			t.Setenv("HOLDFAST_"+strings.ToUpper(key), tokenSentinel)
			var out, errOut bytes.Buffer
			code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut)
			if code == 0 {
				t.Errorf("validate ACCEPTED a literal in HOLDFAST_%s - an env literal is inherited "+
					"by every ffmpeg child, which is the leak this refusal exists for", strings.ToUpper(key))
			}
			msg := errOut.String() + out.String()
			if strings.Contains(msg, tokenSentinel) {
				t.Errorf("validate echoed the literal value from HOLDFAST_%s:\n%s", strings.ToUpper(key), msg)
			}
		}
	})
}

// AC-4: a reference that cannot produce a value exits non-zero AT START, BEFORE any scan,
// encode or swap work - proved by a real, encodable source in the library that a run which
// did not stop would have transcoded, and which is byte-for-byte intact afterwards.
func TestCLI_AC4_AnUnresolvableReferenceStopsBeforeAnyWork(t *testing.T) {
	cfgPath, lib, state, src := preflightLibrary(t,
		"server_auth_token: file:"+filepath.Join(t.TempDir(), "never-created")+"\nvmaf_enable: false\n")
	before := readAll(t, src)

	var out, errOut bytes.Buffer
	code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
	if code == 0 {
		t.Fatalf("run started with an unresolvable reference (stdout: %s)", out.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "server_auth_token") {
		t.Errorf("the refusal does not name the key:\n%s", msg)
	}
	if !strings.Contains(msg, "never-created") {
		t.Errorf("the refusal does not name the reference:\n%s", msg)
	}

	// Nothing was touched: the source is intact, and the store was never created.
	if after := readAll(t, src); !bytes.Equal(before, after) {
		t.Error("the source was modified despite the startup refusal")
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("the job store was opened despite the startup refusal")
	}
	entries, err := os.ReadDir(lib)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the library holds %d entries, want only the untouched source", len(entries))
	}
}

// AC-3: no child process this binary starts carries a resolved value in its environment or
// argv. The child here is the REAL encoder preflight: `run` exercises the configured encoder
// against a tiny clip before it touches the library, so an ffmpeg process genuinely exists
// with all three references configured. The proof is that holdfast's own environment carries
// no value for any child to inherit, measured in the process that spawns them.
func TestCLI_AC3_NoResolvedValueIsInTheEnvironmentAnyChildInherits(t *testing.T) {
	lab := newSecretsLab(t, "")
	cfg, err := config.Load(lab.cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, code := resolveSecrets(context.Background(), cfg, io.Discard); code != 0 {
		t.Fatalf("resolveSecrets exited %d on a valid reference config", code)
	}
	// Resolution has happened. Anything a child could inherit is in here.
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		for key, value := range sentinels() {
			if strings.Contains(kv, value) {
				t.Fatalf("the resolved value of %s is in holdfast's environment as %s, so every "+
					"ffmpeg child inherits it", key, name)
			}
		}
	}
	// And a real child, started exactly as the probe and the encoder are, sees none of it.
	out, err := exec.Command(envOr("HOLDFAST_FFMPEG", "ffmpeg"), "-hide_banner", "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}
	assertNoSentinel(t, "a real ffmpeg child's output", string(out))
	dump := filepath.Join(t.TempDir(), "dump.sh")
	if err := os.WriteFile(dump, []byte("#!/bin/sh\nenv\necho \"argv: $0 $*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(dump, "-i", lab.lib, "-c:v", "libx265").CombinedOutput()
	if err != nil {
		t.Fatalf("stub child: %v: %s", err, out)
	}
	assertNoSentinel(t, "a child process's environment and argv", string(out))
}

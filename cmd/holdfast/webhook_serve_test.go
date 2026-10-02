package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// TestServe_WebhookIntakeIsWiredToItsOwnCredential runs the real daemon and delivers a Sonarr
// Download to it the way an arr does. It is the wiring proof the server package cannot give:
// `serve` resolves `webhook_token` and hands it to the intake, the intake reads the
// configured `sonarr_path_map` in reverse, and an accepted file reaches the ENGINE - the
// ledger carries a row for it under the path holdfast sees it by.
//
// The file is not media, so the engine's own probe ends the job without an encode: this is
// about the route into the pipeline, and the pipeline's proofs are internal/engine's.
func TestServe_WebhookIntakeIsWiredToItsOwnCredential(t *testing.T) {
	if _, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg")); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	rel := "tv/Synthetic Series/Season 01/Synthetic Series - S01E01.mkv"
	file := filepath.Join(lib, rel)
	// A marker the startup scan finds. The file the webhook names is written only AFTER
	// that scan is over, and no later scan runs (scan_interval_sec is 0), so a ledger row
	// for it can only have come through the intake.
	marker := filepath.Join(lib, "marker.mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("not media"), 0o644); err != nil {
		t.Fatal(err)
	}
	secretFile := func(name, value string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(value+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const controlTok, hookTok = "control-e2e-71c0", "webhook-e2e-9a4f"
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nserver_addr: " + addr +
		"\nserver_auth_token: file:" + secretFile("control", controlTok) +
		"\nwebhook_token: file:" + secretFile("webhook", hookTok) +
		"\nsonarr_path_map:\n  - {from: " + filepath.Join(lib, "tv") + ", to: /tv}\n"
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
	ledger := func() string { return httpGet(t, base+"/api/history") + httpGet(t, base+"/api/queue") }
	inLedger := func(path string) bool {
		quoted := string(mustJSON(t, path))
		return strings.Contains(ledger(), quoted)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; the ledger: %s", what, ledger())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the startup scan to record the marker", func() bool { return inLedger(marker) })
	waitFor("the startup scan to finish", func() bool {
		return strings.Contains(httpGet(t, base+"/api/summary"), `"scanning":false`)
	})
	if err := os.WriteFile(file, []byte("not media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if inLedger(file) {
		t.Fatal("the ledger already names the file before any webhook was delivered")
	}

	payload, err := os.ReadFile(filepath.Join("..", "..", "internal", "server", "testdata", "webhook", "sonarr-download-episodefile.json"))
	if err != nil {
		t.Fatal(err)
	}
	deliver := func(app string, auth func(*http.Request)) (int, string) {
		req, err := http.NewRequest(http.MethodPost, base+"/api/webhook/"+app, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		auth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/webhook/%s: %v", app, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// The control token is not the intake's credential, and the intake's is not control's.
	if code, _ := deliver("sonarr", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+controlTok) }); code != 401 {
		t.Errorf("the control token on the intake: code %d, want 401", code)
	}
	if code := httpPostCode(t, base+"/api/pause", hookTok); code != 401 {
		t.Errorf("the webhook token on POST /api/pause: code %d, want 401", code)
	}

	// The arr's own way: the token as the Basic password.
	code, raw := deliver("sonarr", func(r *http.Request) { r.SetBasicAuth("sonarr", hookTok) })
	if code != http.StatusAccepted {
		t.Fatalf("the Sonarr Download: code %d, want 202: %s", code, raw)
	}
	var answer struct {
		EventType string `json:"event_type"`
		Accepted  int    `json:"accepted"`
		Results   []struct {
			Path, Mapped, Resolved string
			Accepted               bool
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v: %s", err, raw)
	}
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if answer.EventType != "Download" || answer.Accepted != 1 || len(answer.Results) != 1 ||
		answer.Results[0].Path != "/"+rel || answer.Results[0].Mapped != file || answer.Results[0].Resolved != want {
		t.Fatalf("the answer does not describe the one file, mapped back through sonarr_path_map: %s", raw)
	}

	// And the engine has the file: the ledger carries a row under holdfast's path for it.
	waitFor("the engine to record the file the webhook named", func() bool { return inLedger(want) })

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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

package main

// The per-root filesystem watch at the level the criterion is written about (S0091):
// THE SYSTEM. internal/engine grades what the watch decides; only the daemon can be
// asked whether a watch is started at all, and a watch nothing ever runs would satisfy
// every engine case in the repository while accelerating nothing.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
)

// syncBuffer is a log sink several goroutines write to at once. runServer starts the hub,
// the notifier, the submission queue, the watch and the scan loop against one logger, so
// an unsynchronized buffer here is a data race rather than a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// watchRecords is every structured record the watch emitted about root, by the component
// bound where its logger is built.
func watchRecords(t *testing.T, b *syncBuffer, root string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader([]byte(b.String())))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["component"] == "watch" && rec["library_root"] == root {
			out = append(out, rec)
		}
	}
	return out
}

// TestServeStartsTheWatchOnlyForTheRootThatAskedForOne grades AC-1 where the criterion is
// stated - "WHEN a library_roots entry carries the watch opt-in THE SYSTEM SHALL start a
// watch for that root, and WHEN an entry omits it THE SYSTEM SHALL start no watch for it".
// The engine cases grade the decision; this grades that the daemon takes it at all, which
// is the one thing removing the wiring would leave every other case green about.
//
// The record is what is asserted rather than the descriptor, because the two outcomes for
// the opted-in root - watched, or fallen back to the interval scan - are both records that
// name it, and the case must hold on a host whose temporary directory is not storage the
// FILESYSTEM-1 check positively identifies as local.
func TestServeStartsTheWatchOnlyForTheRootThatAskedForOne(t *testing.T) {
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
	watched, unwatched := filepath.Join(dir, "tv"), filepath.Join(dir, "film")
	for _, lib := range []string{watched, unwatched} {
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n" +
		"  - path: " + watched + "\n" +
		"    watch: true\n" +
		"    watch_settle_sec: 60\n" +
		"  - path: " + unwatched + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"server_addr: " + addr + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	sink := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, sink) }()

	base := "http://" + addr
	waitHTTP(t, base+"/api/summary", 15*time.Second)

	deadline := time.Now().Add(15 * time.Second)
	for len(watchRecords(t, sink, watched)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon started with a root carrying the watch opt-in and said nothing about a watch "+
				"over it: %s", sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := watchRecords(t, sink, unwatched); len(got) != 0 {
		t.Errorf("the root that carried no watch key was reported on by the watch: %v", got)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServer exit code = %d, want 0: %s", code, sink.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runServer did not shut down after context cancel with a watch running")
	}
}

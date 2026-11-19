package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/playback"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestPlaybackPoller_OffUnlessASourceIsWritten: no source is no poller and no record; a source is
// a Tautulli poller under the configured interval, rules and Plex path map; a source whose key
// resolved to nothing is no poller, said once.
func TestPlaybackPoller_OffUnlessASourceIsWritten(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte("SYNTHETIC-KEY\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ref, err := secret.ParseRef("tautulli_api_key", "file:"+keyFile)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	taut := schedule.NewTautulli("http://tautulli.test:8181", ref, v)

	if p := newPlaybackPoller(&config.Config{}, taut, st, st, log); p != nil || buf.Len() != 0 {
		t.Errorf("no source: poller %v, records %q", p, buf.String())
	}
	paths := config.PathMap{{From: "/media", To: "/data"}}
	cfg := &config.Config{PlaybackSource: "tautulli", PlaybackPollMinutes: 30, PlaybackPlayedFraction: 0.8, PlexPathMap: paths}
	p := newPlaybackPoller(cfg, taut, st, st, log)
	if p == nil {
		t.Fatal("a configured source built no poller")
	}
	if p.Source.Name() != "tautulli" || p.Interval != 30*time.Minute || p.Rules != playback.RulesFor(cfg) ||
		len(p.Paths) != 1 || p.Paths[0] != paths[0] {
		t.Errorf("poller: source %s, interval %v, rules %+v, paths %v", p.Source.Name(), p.Interval, p.Rules, p.Paths)
	}
	if p := newPlaybackPoller(cfg, nil, st, st, log); p != nil || !strings.Contains(buf.String(), "resolved to an empty value") {
		t.Errorf("no client: poller %v, records %q", p, buf.String())
	}
}

// TestTautulliPause_OffKeepsEvidenceWithoutHoldingWork: with tautulli_pause: false a configured
// Tautulli that reports a stream holds no new work, while playback evidence still reads its
// history through the same client; with the key unset (the default) the stream holds work as it
// always did.
func TestTautulliPause_OffKeepsEvidenceWithoutHoldingWork(t *testing.T) {
	var historyReads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmd") {
		case "get_activity":
			_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"stream_count":"1"}}}`))
		case "get_history":
			historyReads++
			_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":0,"data":[]}}}`))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "tautulli-key")
	if err := os.WriteFile(keyFile, []byte("SYNTHETIC-KEY\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	load := func(extra string) (*config.Config, *secret.Set) {
		t.Helper()
		p := filepath.Join(dir, "config.yaml")
		body := "library_roots: [" + filepath.Join(dir, "lib") + "]\ntautulli_url: " + srv.URL +
			"\ntautulli_api_key: file:" + keyFile + "\nplayback_source: tautulli\n" + extra
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		refs, err := cfg.SecretRefs()
		if err != nil {
			t.Fatal(err)
		}
		secrets, err := secret.Resolve(context.Background(), refs)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, secrets
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	cfg, secrets := load("tautulli_pause: false\n")
	client, pause := tautulliClients(cfg, secrets)
	if client == nil || pause != nil {
		t.Fatalf("tautulli_pause: false: client %v, pause %v", client, pause)
	}
	if ok, why := schedule.New(schedule.Window{}, 0, pause, log).MayRun(ctx); !ok {
		t.Errorf("a stream held new work with the pause off: %s", why)
	}
	src := newPlaybackPoller(cfg, client, nil, nil, log)
	if src == nil {
		t.Fatal("no evidence poller with the pause off")
	}
	if _, _, err := src.Source.Sessions(ctx, time.Unix(1_790_000_000, 0), playback.Resume{}); err != nil || historyReads != 1 {
		t.Errorf("evidence did not read the history: %v, %d reads", err, historyReads)
	}

	cfg, secrets = load("")
	client, pause = tautulliClients(cfg, secrets)
	if client == nil || pause != client {
		t.Fatalf("the default: client %v, pause %v", client, pause)
	}
	if ok, why := schedule.New(schedule.Window{}, 0, pause, log).MayRun(ctx); ok || !strings.Contains(why, "streaming") {
		t.Errorf("the default pause did not hold new work during a stream: %v %q", ok, why)
	}

	// No Tautulli at all: neither.
	if client, pause := tautulliClients(&config.Config{}, nil); client != nil || pause != nil {
		t.Errorf("no Tautulli: %v %v", client, pause)
	}
}

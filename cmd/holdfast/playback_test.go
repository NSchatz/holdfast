package main

import (
	"bytes"
	"context"
	"log/slog"
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

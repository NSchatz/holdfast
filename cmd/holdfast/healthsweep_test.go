package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestServeHealthSweep_OffByDefaultAndWiredToTheEnginesEnumeration: with the key unset the
// daemon builds no sweep (I5). With it set, the sweep reads the ENGINE's enumeration - an
// exclude_paths pattern keeps a file out of the sweep exactly as it keeps it out of a scan -
// decodes with the engine's ffmpeg, and starts nothing while the daemon is paused.
func TestServeHealthSweep_OffByDefaultAndWiredToTheEnginesEnumeration(t *testing.T) {
	ffmpeg, ffprobe := envOr("HOLDFAST_FFMPEG", "ffmpeg"), envOr("HOLDFAST_FFPROBE", "ffprobe")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: %q not found - this fixture requires the pinned ffmpeg: %v", ffmpeg, err)
	}
	lib := t.TempDir()
	kept := filepath.Join(lib, "keep", "a.mkv")
	excluded := filepath.Join(lib, "import", "b.mkv")
	for _, p := range []string{kept, excluded} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
			"testsrc2=duration=1:size=160x120:rate=24", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
			p).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{LibraryRoots: []string{lib}, VideoExts: []string{"mkv"}, ExcludePaths: []string{"import/**"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(*cfg, probe.New(ffmpeg, ffprobe), nil, st, discardLog())
	sched := schedule.New(schedule.Window{}, 0, nil, discardLog())

	if sw := newHealthSweep(cfg, eng, st, func() bool { return false }, sched, nil, discardLog()); sw != nil {
		t.Fatal("a configuration without health_sweep_interval_hours built a sweep")
	}

	cfg.HealthSweepIntervalHours = 24
	paused := true
	sw := newHealthSweep(cfg, eng, st, func() bool { return paused }, sched, nil, discardLog())
	if sw == nil || sw.Interval != 24*time.Hour || sw.Workers != 1 {
		t.Fatalf("the enabled sweep is %+v", sw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	sw.Wait = func(context.Context, time.Duration) bool {
		waits++
		if live := sw.Live(); live.Why != "paused (POST /api/resume)" {
			t.Errorf("while paused the sweep says %+v", live)
		}
		cancel()
		return false
	}
	if _, err := sw.RunDue(ctx); !errors.Is(err, context.Canceled) || waits != 1 {
		t.Fatalf("a paused daemon's sweep: err=%v after %d wait(s), want it waiting", err, waits)
	}
	if _, ok, _ := st.LatestHealthSweep(context.Background()); ok {
		t.Fatal("a paused daemon opened a sweep")
	}

	paused = false
	if ran, err := sw.RunDue(context.Background()); err != nil || !ran {
		t.Fatalf("RunDue: ran=%v err=%v", ran, err)
	}
	done, _, _ := st.LastFinishedHealthSweep(context.Background())
	c, ok, _ := st.HealthCheckOf(context.Background(), done.ID, kept)
	if !ok || c.Result != store.HealthOK {
		t.Errorf("the kept file: %+v ok=%v, want ok", c, ok)
	}
	if _, ok, _ := st.HealthCheckOf(context.Background(), done.ID, excluded); ok || done.Counts.Checked() != 1 {
		t.Errorf("the sweep checked %d file(s) and the excluded one was recorded=%v; a path filter keeps a file out of the sweep",
			done.Counts.Checked(), ok)
	}
}

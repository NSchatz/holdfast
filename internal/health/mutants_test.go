package health

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// TestOpenForRead_ProvesReadAccessAndReportsAMissingFile: the production open seam.
func TestOpenForRead_ProvesReadAccessAndReportsAMissingFile(t *testing.T) {
	_, paths := library(t, 1)
	if err := openForRead(paths[0]); err != nil {
		t.Errorf("an ordinary file: %v", err)
	}
	if err := openForRead(filepath.Join(t.TempDir(), "absent.mkv")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v, want ErrNotExist", err)
	}
}

// TestSleep_WaitsOrStopsWithTheContext: the production wait seam.
func TestSleep_WaitsOrStopsWithTheContext(t *testing.T) {
	if !sleep(context.Background(), time.Millisecond) {
		t.Error("a wait that ran its course reported false")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleep(ctx, time.Hour) {
		t.Error("a wait on a cancelled context reported true")
	}
}

// TestHealthSweep_WorkersBoundTheDecodesInFlight: one worker (and 0, its default) never
// runs two decodes at once; two workers do.
func TestHealthSweep_WorkersBoundTheDecodesInFlight(t *testing.T) {
	for _, tc := range []struct{ workers, want int }{{0, 1}, {1, 1}, {2, 2}} {
		dir, _ := library(t, 6)
		st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
		clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
		var mu sync.Mutex
		inFlight, peak := 0, 0
		dec := &decodes{each: func(string) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
		}}
		s := sweeper(st, dir, dec, clk)
		s.Workers = tc.workers
		if _, err := s.RunDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		if peak != tc.want {
			t.Errorf("health_sweep_workers %d ran %d decodes at once, want %d", tc.workers, peak, tc.want)
		}
	}
}

// TestHealthSweep_WarnsOnAProblemFileAndNotOnAGoodOne: an operator reading the log sees each
// file that failed, named with its result, and no line for a file that passed.
func TestHealthSweep_WarnsOnAProblemFileAndNotOnAGoodOne(t *testing.T) {
	dir, paths := library(t, 2)
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	var buf bytes.Buffer
	dec := &decodes{corrupt: map[string]string{paths[1]: "boom"}}
	s := New(time.Hour, 1, st, walkSources(dir), dec, slog.New(slog.NewTextHandler(&buf, nil)))
	clk := &clock{t: time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local)}
	s.Now, s.Wait = clk.Now, clk.Wait
	if _, err := s.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	warns := strings.Count(out, "level=WARN")
	if warns != 1 || !strings.Contains(out, "clip01.mkv") || !strings.Contains(out, "result=corrupt") {
		t.Errorf("%d warn line(s), want one naming clip01 as corrupt:\n%s", warns, out)
	}
	if c, _, _ := st.HealthCheckOf(context.Background(), 1, paths[0]); c.Result != store.HealthOK {
		t.Errorf("clip00: %+v", c)
	}
}

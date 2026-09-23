package engine

// S0160 impl-gate 1, F4 (AC-2). Refuter artifact.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

func TestRegress_0160_F4_DaemonPoolsOutrunTheDividedShare(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv")
	mkH264(t, ffmpeg, a, "8M")
	mkH264(t, ffmpeg, b, "8M")
	const quota = 2
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.VmafEnable, c.MinVmaf, c.Workers = boolPtr(true), 95, 1
	})
	eng.vmafThreads = deriveVmafThreads(quotaRoot(t, quota), eng.Cfg.EffectiveWorkers())

	var mu sync.Mutex
	var gates, threads, peak, peakGates int
	release, once := make(chan struct{}), sync.Once{}
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
		mu.Lock()
		gates, threads = gates+1, threads+req.Threads
		if threads > peak {
			peak, peakGates = threads, gates
		}
		if gates >= 2 {
			once.Do(func() { close(release) })
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(90 * time.Second):
		}
		mu.Lock()
		gates, threads = gates-1, threads-req.Threads
		mu.Unlock()
		return vmaf.Result{HarmonicMean: 99, Min: 98, ChromaMin: 45,
			ChromaMetric: vmaf.ChromaMetricName, PixelFormat: req.PixelFormat}, nil
	}

	subs := eng.NewSubmissions(0, 8) // what `serve` builds, beside its scan loop
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); subs.Run(ctx) }()
	for _, p := range []string{a, b} {
		r, _, _, ok := subs.Judge(p)
		if !ok || !subs.Offer(r) {
			t.Fatalf("could not submit %s", p)
		}
	}
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Minute); len(subs.Results()) < 2 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if peakGates < 2 {
		t.Fatalf("precondition: the pools never overlapped (peak %d gate)", peakGates)
	}
	if peak > quota {
		t.Errorf("workers 1, quota %d, share %d: scan + submission pools ran %d gates asking for "+
			"%d threads together (AC-2)", quota, eng.vmafThreadCount(), peakGates, peak)
	}
}

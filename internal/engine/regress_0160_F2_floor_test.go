package engine

// S0160 impl-gate 2, F2 carried (AC-2) at the share floor. Refuter artifact.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// namedPoolsStub stands in for ffmpeg only to report what the REAL vmaf.Score names for one
// gate: the filtergraph pool (-filter_complex_threads) plus libvmaf's pool (n_threads= in
// the -lavfi graph). It exits 1, so Score returns that report inside its error.
const namedPoolsStub = `#!/bin/sh
g=0; v=0; prev=
for a in "$@"; do
  case "$prev" in
    -filter_complex_threads|-filter_threads|-threads) g=$((g + a)) ;;
    -lavfi|-filter_complex)
      n=$(printf '%s' "$a" | sed -n 's/.*n_threads=\([0-9][0-9]*\).*/\1/p')
      v=$((v + ${n:-0})) ;;
  esac
  prev=$a
done
echo "NAMED $((g + v))" >&2
exit 1
`

func TestRegress_0160_F2_AtAShareOfOneTheGatesNameTwiceTheQuota(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	stub := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(stub, []byte(namedPoolsStub), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv")
	mkH264(t, ffmpeg, a, "8M")
	mkH264(t, ffmpeg, b, "8M")
	// Two workers on a two-CPU quota: the workers do not exceed the quota, so no floor
	// forces any gate over its share.
	const quota, workers = 2, 2
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.VmafEnable, c.MinVmaf, c.Workers = boolPtr(true), 95, workers
	})
	eng.vmafThreads = deriveVmafThreads(quotaRoot(t, quota), eng.Cfg.EffectiveWorkers())

	re := regexp.MustCompile(`NAMED (\d+)`)
	var mu sync.Mutex
	var calls, gates, named, peak, peakGates int
	arrived, once := make(chan struct{}), sync.Once{}
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
		_, err := vmaf.Score(ctx, stub, req)
		m := re.FindStringSubmatch(fmt.Sprint(err))
		if m == nil {
			t.Errorf("the stub's report did not come back through Score: %v", err)
			return vmaf.Result{}, err
		}
		n, _ := strconv.Atoi(m[1])
		mu.Lock()
		calls, gates, named = calls+1, gates+1, named+n
		if named > peak {
			peak, peakGates = named, gates
		}
		if gates >= 2 {
			once.Do(func() { close(arrived) })
		}
		mu.Unlock()
		// Hold long enough for a second gate the budget ADMITS to overlap this one, and never
		// wait on a gate the budget QUEUES: a queued second gate is a compliant answer, so
		// overlap is not a precondition here.
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
		}
		mu.Lock()
		gates, named = gates-1, named-n
		mu.Unlock()
		return vmaf.Result{HarmonicMean: 99, Min: 98, ChromaMin: 45,
			ChromaMetric: vmaf.ChromaMetricName, PixelFormat: req.PixelFormat}, nil
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("precondition: %d gate(s) scored, want both files gated", calls)
	}
	if peak > quota {
		t.Errorf("quota %d, workers %d: %d gates scored at once and their ffmpeg invocations "+
			"named %d threads together (AC-2)", quota, workers, peakGates, peak)
	}
}

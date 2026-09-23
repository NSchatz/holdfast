package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// quotaRoot writes a cgroup v2 mount naming a bandwidth limit of cpus CPUs. The cgroup
// filesystem is outside this package's boundary; the subject here is what the engine does
// with the figure it reads back.
func quotaRoot(t *testing.T, cpus int) string {
	t.Helper()
	root := t.TempDir()
	body := "max 100000\n"
	if cpus > 0 {
		body = strconv.Itoa(cpus*100000) + " 100000\n"
	}
	if err := os.WriteFile(filepath.Join(root, "cpu.max"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestS0160_AC2_TheQuotaIsDividedAcrossTheConfiguredWorkers: the per-job count is the
// quota split by the workers that may score at once, so the gates of a run with four
// workers cannot together ask for more CPU than the container was given.
func TestS0160_AC2_TheQuotaIsDividedAcrossTheConfiguredWorkers(t *testing.T) {
	root := quotaRoot(t, 12)
	for _, tc := range []struct {
		workers, want int
	}{
		{1, 12}, {2, 6}, {3, 4}, {4, 3}, {5, 2}, {12, 1}, {24, 1},
	} {
		got := deriveVmafThreads(root, tc.workers).threads
		if got != tc.want {
			t.Errorf("a 12-CPU quota across %d workers derived %d threads, want %d",
				tc.workers, got, tc.want)
		}
		if tc.workers <= 12 && got*tc.workers > 12 {
			t.Errorf("%d workers at %d threads each would ask for %d against a quota of 12",
				tc.workers, got, got*tc.workers)
		}
	}
}

// TestS0160_AC2_AGateThatWouldTakeTheSumOverTheQuotaWaits is the bound the divided share
// cannot give on its own. A gate sizes itself when it starts, so one that started with a
// single file in flight holds the whole quota; a second file claimed after it divides the
// quota by two, and its share still does not fit beside the first. It must wait for the
// first to hand its threads back rather than run over the quota, and a run cancelled while
// it waits must come back as an error rather than as a share.
func TestS0160_AC2_AGateThatWouldTakeTheSumOverTheQuotaWaits(t *testing.T) {
	eng := &Engine{Log: discardLogger(), vmafThreads: deriveVmafThreads(quotaRoot(t, 4), 1)}
	ctx := context.Background()

	// Both files stay in flight to the end, so the share the second gate takes once it is
	// admitted is the one two files in flight divide the quota into.
	defer eng.enterGateFlight()()
	a, releaseA, err := eng.takeGateThreads(ctx, "a.mkv")
	if err != nil || a != 4 {
		t.Fatalf("a lone gate on a 4-CPU quota took %d threads (err %v), want 4", a, err)
	}
	defer eng.enterGateFlight()()

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if n, _, err := eng.takeGateThreads(cancelled, "b.mkv"); err == nil {
		t.Errorf("a gate that could not fit beside a running one took %d threads, so the two "+
			"together hold %d of a 4-CPU quota", n, a+n)
	}

	got := make(chan int, 1)
	go func() {
		n, release, err := eng.takeGateThreads(ctx, "b.mkv")
		if err != nil {
			t.Error(err)
		}
		got <- n
		release()
	}()
	select {
	case n := <-got:
		t.Fatalf("the second gate took %d threads while the first still held 4 of a 4-CPU quota", n)
	case <-time.After(200 * time.Millisecond):
	}
	releaseA()
	select {
	case n := <-got:
		if n != 2 {
			t.Errorf("the second gate took %d threads with two files in flight on a 4-CPU quota, "+
				"want 2", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second gate never started after the first handed its threads back")
	}
}

// TestS0160_AC1_NewWiresTheDerivedCountIntoTheEngine: the count an Engine carries is the
// one derived from this host's quota and this configuration's workers, and the gate asks
// for it. Both halves matter: a count derived and not handed over buys nothing.
func TestS0160_AC1_NewWiresTheDerivedCountIntoTheEngine(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		cfg := baseCfg(t.TempDir())
		cfg.Workers = workers
		eng := New(cfg, nil, nil, nil, discardLogger())
		want := deriveVmafThreads(cpuquota.DefaultRoot, workers).threads
		if eng.vmafThreads.threads != want {
			t.Errorf("New with workers=%d carries %d threads, want the %d this host's quota derives",
				workers, eng.vmafThreads.threads, want)
		}
		if eng.vmafThreadCount() < 1 {
			t.Errorf("the gate would ask libvmaf for %d threads, which the library reads as its "+
				"own default rather than as a request", eng.vmafThreadCount())
		}
	}
	// An Engine assembled without New has never derived anything, and must still name a
	// count the filter can act on.
	if got := (&Engine{}).vmafThreadCount(); got != cpuquota.FallbackShare {
		t.Errorf("an underived Engine asks for %d threads, want the stated fallback of %d",
			got, cpuquota.FallbackShare)
	}
}

// TestS0160_AC1_TheGateAsksForTheDerivedCount is the end of the wire: the request the
// quality gate actually issues carries the engine's derived count, over a real encode.
func TestS0160_AC1_TheGateAsksForTheDerivedCount(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	eng.vmafThreads = vmafThreadPlan{threads: 7}
	var asked []int
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
		asked = append(asked, req.Threads)
		return vmaf.Result{HarmonicMean: 99, Min: 98, ChromaMin: 45,
			ChromaMetric: vmaf.ChromaMetricName, PixelFormat: req.PixelFormat}, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 {
		t.Fatal("the quality gate never ran, so this test decides nothing")
	}
	for _, n := range asked {
		if n != 7 {
			t.Errorf("the gate asked libvmaf for %d threads, want the engine's derived 7", n)
		}
	}
}

// TestS0160_AC5_AnUnreadableQuotaWarnsAndFallsBackWithoutFailingTheJob: the gate still
// runs, nothing fails, and the run says which dependency it could not read, what it tried
// and what it did instead (observability O4, at warn because the process continued
// degraded, O3).
func TestS0160_AC5_AnUnreadableQuotaWarnsAndFallsBackWithoutFailingTheJob(t *testing.T) {
	for name, root := range map[string]string{
		"no cgroup files at all": t.TempDir(),
		"a malformed cpu.max":    malformedQuotaRoot(t),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			plan := deriveVmafThreads(root, 4)
			got := plan.threads
			if got != cpuquota.FallbackShare {
				t.Errorf("an unreadable quota derived %d threads, want the stated fallback of %d",
					got, cpuquota.FallbackShare)
			}
			if got < 1 {
				t.Error("the fallback is below 1, which libvmaf reads as its own default")
			}
			// Announced through the Engine's own guard, twice, because the announcement is
			// made once per run and the workers reach it concurrently.
			eng := &Engine{Log: jsonLogger(&buf), vmafThreads: plan}
			eng.announceVmafThreads()
			eng.announceVmafThreads()
			warns := 0
			for _, r := range logRecords(t, &buf) {
				if r["level"] != "WARN" {
					continue
				}
				warns++
				for _, field := range []string{"dependency", "attempted", "next_action", "vmaf_threads"} {
					if _, ok := r[field]; !ok {
						t.Errorf("the warn does not carry %q, so it does not say which dependency "+
							"failed, what was tried and what was done instead: %v", field, r)
					}
				}
				if r["attempted"] != root {
					t.Errorf("the warn says it attempted %v, want %q", r["attempted"], root)
				}
			}
			if warns != 1 {
				t.Errorf("an unreadable quota produced %d warn records, want exactly 1", warns)
			}
			// Nothing at error: this is a condition the code recovered from itself, and an
			// error record asks a human to act (observability O3).
			for _, r := range logRecords(t, &buf) {
				if r["level"] == "ERROR" {
					t.Errorf("an unreadable quota logged at error, which asks a human to act on a "+
						"condition the run handled itself: %v", r)
				}
			}
		})
	}
}

// TestS0160_AC5_TheGateStillRunsOnAnUnreadableQuota is the other half of the criterion:
// the job is neither failed nor skipped, and the source is still gated.
func TestS0160_AC5_TheGateStillRunsOnAnUnreadableQuota(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	// What an unreadable quota leaves behind.
	eng.vmafThreads = deriveVmafThreads(t.TempDir(), 1)
	ts := eng.Store.(*testStore)
	scored := 0
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
		scored++
		if req.Threads < 1 {
			t.Errorf("the gate asked libvmaf for %d threads on the fallback path", req.Threads)
		}
		return vmaf.Result{HarmonicMean: 99, Min: 98, ChromaMin: 45,
			ChromaMetric: vmaf.ChromaMetricName, PixelFormat: req.PixelFormat}, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scored == 0 {
		t.Error("the quality gate was SKIPPED on the fallback path - an unreadable quota must " +
			"never turn the perceptual gate off")
	}
	if ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Error("an unreadable CPU quota failed the job; it must only make the gate slower")
	}
}

// TestS0160_AC8_ARejectedInvocationKeepsTheSource: when the invocation the gate built is
// refused, the job is recorded as not having passed and the source is neither replaced nor
// deleted. The refusal driven here is the REAL one, from the real Score, for the unusable
// thread count the criterion names.
func TestS0160_AC8_ARejectedInvocationKeepsTheSource(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})
	ts := eng.Store.(*testStore)
	eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
		// An invocation libvmaf would read as a request for its own default. Score refuses
		// it, and this is that real refusal rather than an error written by the test.
		req.Threads = 0
		return vmaf.Score(ctx, ffmpeg, req)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if md5f(t, src) != before {
		t.Error("the source changed after an invocation the gate could not run")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("the source was swapped after an invocation the gate could not run")
	}
	if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Error("a rejected invocation was not recorded as a job that did not pass the gate")
	}
}

// TestS0160_AC11_TheDerivedCountNeverChangesTheSamplingInterval: the interval the gate
// asks for is the operator's configured value at every thread count, so a faster gate
// never quietly changes what the worst-frame and chroma floors bound.
func TestS0160_AC11_TheDerivedCountNeverChangesTheSamplingInterval(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, configured := range []int{1, 3} {
		for _, threads := range []int{1, 7} {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
				c.VmafEnable = boolPtr(true)
				c.MinVmaf = 95
				c.VmafSubsample = configured
			})
			eng.vmafThreads = vmafThreadPlan{threads: threads}
			var asked []int
			eng.vmafScore = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
				asked = append(asked, req.Subsample)
				return vmaf.Result{HarmonicMean: 99, Min: 98, ChromaMin: 45,
					ChromaMetric: vmaf.ChromaMetricName, PixelFormat: req.PixelFormat}, nil
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(asked) == 0 {
				t.Fatal("the quality gate never ran, so this case decides nothing")
			}
			for _, got := range asked {
				if got != configured {
					t.Errorf("vmaf_subsample=%d at %d threads reached the gate as %d: the interval "+
						"must come from operator configuration and from nothing else",
						configured, threads, got)
				}
			}
		}
	}
}

func malformedQuotaRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu.max"), []byte("as much as you like\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

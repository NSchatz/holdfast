package main

// S0163 through the real commands: what `validate` prints about the worker pool, what `run`
// and `serve` record about it at start, and that a worker setting outside its forms stops all
// three before anything is touched. The cgroup hierarchy is a fixture handed over through
// engine.CgroupRootEnv, the way a real deployment's mount would be read; the libraries are
// EMPTY, because these cases grade what the commands SAY, not what they encode.

import (
	"bytes"
	"context"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/logging"
)

// s0163Rec is one parsed slog TEXT record.
type s0163Rec struct {
	level, msg string
	attrs      map[string]string
	raw        string
}

// s0163Parse reads every levelled record out of a capture.
func s0163Parse(out string) []s0163Rec {
	var recs []s0163Rec
	for _, line := range strings.Split(out, "\n") {
		r, ok := hpParseLine(line)
		if !ok {
			continue
		}
		recs = append(recs, s0163Rec{level: r.level, msg: r.msg, attrs: s0161Attrs(line), raw: line})
	}
	return recs
}

// s0163Workers returns the worker-pool component's records at level ("" for any).
func s0163Workers(recs []s0163Rec, level string) []s0163Rec {
	var out []s0163Rec
	for _, r := range recs {
		if r.attrs["component"] == "workers" && (level == "" || strings.EqualFold(r.level, level)) {
			out = append(out, r)
		}
	}
	return out
}

// s0163Start is the one "worker pool for this run" record, failing unless there is exactly one.
func s0163Start(t *testing.T, recs []s0163Rec, all string) s0163Rec {
	t.Helper()
	var found []s0163Rec
	for _, r := range s0163Workers(recs, "INFO") {
		if r.msg == "worker pool for this run" {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d worker pool records, want exactly 1:\n%s", len(found), all)
	}
	return found[0]
}

// s0163CgroupRoot builds a cgroup mount from files and points every reading at it.
func s0163CgroupRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(engine.CgroupRootEnv, root)
	return root
}

// s0163Run runs `holdfast run` in this process over cfgPath and returns its exit code and
// what it logged.
func s0163Run(t *testing.T, cfgPath string) (int, string) {
	t.Helper()
	code := -1
	var errOut bytes.Buffer
	got := captureStderr(t, func() {
		var out bytes.Buffer
		code = dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
	})
	return code, got + errOut.String()
}

// s0163Serve runs `holdfast serve`'s core over cfgPath until its API answers, then stops it,
// and returns what it logged.
func s0163Serve(t *testing.T, extra string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg, err := config.Load(emptyLibraryConfig(t, extra+"server_addr: "+addr+"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	logs := &lockedBuffer{}
	log := logging.To(logs, "info")
	// cmdServe's own sequence, with a context this case can cancel in place of the signal.
	logResolvedProfiles(cfg, log)
	logConfigWarnings(cfg, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, &bytes.Buffer{}) }()
	waitHTTP(t, "http://"+addr+"/api/summary", serverReady)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServer exited %d:\n%s", code, logs.String())
		}
	case <-time.After(12 * time.Second):
		t.Fatal("runServer did not shut down after the context was cancelled")
	}
	return logs.String()
}

// TestS0163_AC3_RunStartsOnAnAbsentOrUnusableQuotaAndSaysSo grades AC-3 through `holdfast
// run`: with `workers: auto`, an absent cpu.max and a malformed one both START (exit 0), the
// first with one info record saying no quota applies and the second with one warn record
// naming cgroup cpu.max, what was attempted and the CPU-count fallback.
func TestS0163_AC3_RunStartsOnAnAbsentOrUnusableQuotaAndSaysSo(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		s0163CgroupRoot(t, map[string]string{})
		code, got := s0163Run(t, emptyLibraryConfig(t, "workers: auto\n"))
		if code != 0 {
			t.Fatalf("run exited %d on an absent quota:\n%s", code, got)
		}
		recs := s0163Parse(got)
		var infos []s0163Rec
		for _, r := range s0163Workers(recs, "INFO") {
			if strings.Contains(r.msg, "no CPU quota applies") {
				infos = append(infos, r)
			}
		}
		if len(infos) != 1 {
			t.Fatalf("%d info records say no quota applies, want exactly 1:\n%s", len(infos), got)
		}
		if w := s0163Workers(recs, "WARN"); len(w) != 0 {
			t.Errorf("an absent quota warned: %v", w)
		}
		if s := s0163Start(t, recs, got); s.attrs["cpu_quota_source"] != config.QuotaFromCPUCount ||
			s.attrs["cpus"] != strconv.Itoa(runtime.NumCPU()) {
			t.Errorf("the pool record does not carry Q from the CPU count: %s", s.raw)
		}
	})
	for _, bad := range []string{"abc 100000", "0 100000", "100000 100000 5"} {
		t.Run("malformed "+bad, func(t *testing.T) {
			root := s0163CgroupRoot(t, map[string]string{"cpu.max": bad + "\n"})
			code, got := s0163Run(t, emptyLibraryConfig(t, "workers: auto\n"))
			if code != 0 {
				t.Fatalf("run refused to start over a malformed quota (exit %d):\n%s", code, got)
			}
			recs := s0163Parse(got)
			warns := s0163Workers(recs, "WARN")
			if len(warns) != 1 {
				t.Fatalf("%d worker-pool warn records, want exactly 1:\n%s", len(warns), got)
			}
			w := warns[0]
			if w.attrs["dependency"] != config.QuotaFromCgroup || !strings.Contains(w.attrs["attempted"], root) ||
				w.attrs["fallback"] != config.QuotaFromCPUCount {
				t.Errorf("the warn does not name cgroup cpu.max, what was attempted and the fallback: %s", w.raw)
			}
			if s := s0163Start(t, recs, got); s.attrs["cpu_quota_source"] != config.QuotaFromCPUCount {
				t.Errorf("the pool record does not carry the CPU-count fallback: %s", s.raw)
			}
		})
	}
}

// TestS0163_AC4_ValidateRunAndServeRefuseAWorkerSettingOutsideItsForms grades AC-4 through
// the three commands: each exits non-zero, names the key and what it accepts, and creates
// nothing - not even the state directory.
func TestS0163_AC4_ValidateRunAndServeRefuseAWorkerSettingOutsideItsForms(t *testing.T) {
	for _, tc := range []struct {
		name, extra, env, envVal string
		want                     []string
	}{
		{"workers: Auto", "workers: Auto\n", "", "", []string{"workers", "0 to 1024", "auto"}},
		{"workers: 1025", "workers: 1025\n", "", "", []string{"workers", "0 to 1024", "auto"}},
		{"HOLDFAST_WORKERS=2.5", "", "HOLDFAST_WORKERS", "2.5", []string{"workers", "0 to 1024", "auto"}},
		{"cores_per_worker: 0", "workers: auto\ncores_per_worker: 0\n", "", "", []string{"cores_per_worker", "1 to 1024"}},
		{"HOLDFAST_CORES_PER_WORKER=1.5", "", "HOLDFAST_CORES_PER_WORKER", "1.5", []string{"cores_per_worker", "1 to 1024"}},
	} {
		for _, command := range []string{"validate", "run", "serve"} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				if tc.env != "" {
					t.Setenv(tc.env, tc.envVal)
				}
				cfgPath := emptyLibraryConfig(t, tc.extra)
				var out, errOut bytes.Buffer
				var code int
				logged := captureStderr(t, func() {
					code = dispatch([]string{command, "--config", cfgPath}, &out, &errOut)
				})
				if code == 0 {
					t.Fatalf("%s exited 0 on %s:\n%s%s", command, tc.name, errOut.String(), logged)
				}
				for _, w := range tc.want {
					if !strings.Contains(errOut.String(), w) {
						t.Errorf("%s's refusal does not name %q: %s", command, w, errOut.String())
					}
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "state")); err == nil {
					t.Errorf("%s created the state directory before refusing", command)
				}
			})
		}
	}
}

// TestS0163_AC5_EveryCommandSaysCoresPerWorkerHasNoEffectBesideANumericWorkers grades AC-5
// through the commands: the numeric count runs, `validate` prints the statement, and `run`
// and `serve` carry it in their startup records.
func TestS0163_AC5_EveryCommandSaysCoresPerWorkerHasNoEffectBesideANumericWorkers(t *testing.T) {
	const extra = "workers: 2\ncores_per_worker: 8\n"
	const say = "cores_per_worker is 8, which has no effect unless workers is auto"

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, extra)}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "note: "+say) || !strings.Contains(out.String(), "workers: 2 - ") {
		t.Errorf("validate does not print the numeric count and %q:\n%s", say, out.String())
	}

	check := func(t *testing.T, who, got string) {
		t.Helper()
		if !strings.Contains(got, say) {
			t.Errorf("%s's startup records do not say %q:\n%s", who, say, got)
		}
		s := s0163Start(t, s0163Parse(got), got)
		if s.attrs["workers"] != "2" || !strings.Contains(s.attrs["cores_per_worker_effect"], "no effect unless workers is auto") {
			t.Errorf("%s's pool record does not run 2 workers and state the no-effect: %s", who, s.raw)
		}
	}
	code, got := s0163Run(t, emptyLibraryConfig(t, extra))
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, got)
	}
	check(t, "run", got)
	check(t, "serve", s0163Serve(t, extra))
}

// TestS0163_AC7_TheResolvedWorkersAreStatedByValidateRunAndServe grades AC-7: `validate`
// prints the resolved count and, for auto, Q, where it came from and cores_per_worker; `run`
// and `serve` carry the same values as fields of one startup record.
func TestS0163_AC7_TheResolvedWorkersAreStatedByValidateRunAndServe(t *testing.T) {
	s0163CgroupRoot(t, map[string]string{"cpu.max": "300000 100000\n"})
	// Q is min(3, C): the quota wherever this host lets the process run on 3 or more CPUs.
	q, source := 3.0, config.QuotaFromCgroup
	if c := float64(runtime.NumCPU()); c < q {
		q, source = c, config.QuotaFromCPUCount
	}
	want := strconv.Itoa(int(math.Floor(q)))
	qs := strconv.FormatFloat(q, 'f', -1, 64)
	const extra = "workers: auto\ncores_per_worker: 1\n"

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, extra)}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	line := "workers: " + want + " - auto: floor(Q " + qs + " / cores_per_worker 1), where Q is " + qs +
		" CPU(s) from " + source
	if !strings.Contains(out.String(), line) {
		t.Errorf("validate does not print %q:\n%s", line, out.String())
	}

	check := func(t *testing.T, who, got string) {
		t.Helper()
		s := s0163Start(t, s0163Parse(got), got)
		for k, v := range map[string]string{"workers": want, "workers_setting": config.WorkersAuto,
			"cpu_quota": qs, "cpu_quota_source": source, "cores_per_worker": "1",
			"cpus": strconv.Itoa(runtime.NumCPU())} {
			if s.attrs[k] != v {
				t.Errorf("%s's pool record carries %s=%q, want %q: %s", who, k, s.attrs[k], v, s.raw)
			}
		}
	}
	code, got := s0163Run(t, emptyLibraryConfig(t, extra))
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, got)
	}
	check(t, "run", got)
	check(t, "serve", s0163Serve(t, extra))

	// A numeric setting states its own count, and no quota.
	out.Reset()
	if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, "workers: 4\n")}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "workers: 4 - the number of files this configuration encodes at once (workers: 4)") {
		t.Errorf("validate does not print the numeric count:\n%s", out.String())
	}
	code, got = s0163Run(t, emptyLibraryConfig(t, "workers: 4\n"))
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, got)
	}
	if s := s0163Start(t, s0163Parse(got), got); s.attrs["workers"] != "4" || s.attrs["workers_setting"] != "4" ||
		s.attrs["cpu_quota"] != "" {
		t.Errorf("run's pool record for workers: 4 is %s", s.raw)
	}
}

// TestS0163_AC13_RestoreListsEveryRetentionOfConcurrentSwapsInOneDirectory grades AC-13's
// command half: `holdfast run` with three workers over three sources in ONE directory and
// the undo window open retains each original once; `holdfast restore` with no argument lists
// every one of them, holding the sum of their pre-encode sizes; and restoring one puts its
// original bytes back. The forced interleavings of the same criterion are the engine's.
func TestS0163_AC13_RestoreListsEveryRetentionOfConcurrentSwapsInOneDirectory(t *testing.T) {
	ffmpegBin, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("::error:: ffmpeg required for the swaps: %v", err)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(lib, "a.mkv")
	if out, err := exec.Command(ffmpegBin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", "yuv420p", "--", first).CombinedOutput(); err != nil {
		t.Fatalf("building the library fixture: %v\n%s", err, out)
	}
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	srcs := []string{first, filepath.Join(lib, "b.mkv"), filepath.Join(lib, "c.mkv")}
	for _, p := range srcs[1:] {
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
		"\nvmaf_enable: false\nmin_bitrate_kbps: 0\npreset: ultrafast\nworkers: 3\nundo_window_hours: 24\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	code, got := s0163Run(t, cfgPath)
	if code != 0 {
		t.Fatalf("run exited %d:\n%s", code, got)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"restore", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("restore (list) exited %d: %s", code, errOut.String())
	}
	held := strconv.Itoa(3 * len(body))
	if !strings.Contains(out.String(), "3 retained original(s), holding "+held+" byte(s)") {
		t.Errorf("restore does not list three retentions holding %s bytes:\n%s", held, out.String())
	}
	for _, p := range srcs {
		if !strings.Contains(out.String(), "  "+p+"  "+strconv.Itoa(len(body))+" bytes") {
			t.Errorf("restore does not list %s at its pre-encode size:\n%s", p, out.String())
		}
	}
	out.Reset()
	if code := dispatch([]string{"restore", "--config", cfgPath, srcs[2]}, &out, &errOut); code != 0 {
		t.Fatalf("restore %s exited %d: %s", srcs[2], code, errOut.String())
	}
	if back, err := os.ReadFile(srcs[2]); err != nil || !bytes.Equal(back, body) {
		t.Errorf("the restored %s is not its original bytes (err %v)", srcs[2], err)
	}
}

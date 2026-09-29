package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// S0163: `workers: auto` and cores_per_worker. The cgroup hierarchy is a fixture built under
// t.TempDir() and handed to Load through cpuquota.RootEnv, exactly as a child `holdfast run`
// is handed one; the CPU count is numCPU, stood in for per case, so a 56-CPU host and an
// 8-CPU host are both exercised on whatever machine this runs on.

// s0163Cgroup builds a cgroup mount holding the given files and points Load at it.
func s0163Cgroup(t *testing.T, files map[string]string) string {
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
	t.Setenv(cpuquota.RootEnv, root)
	return root
}

// s0163CPUs stands in a host whose process may run on n CPUs.
func s0163CPUs(t *testing.T, n int) {
	t.Helper()
	old := numCPU
	numCPU = func() int { return n }
	t.Cleanup(func() { numCPU = old })
}

// s0163Records captures what Announce says, one decoded JSON record per line.
func s0163Records(t *testing.T, p WorkerPlan) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	p.Announce(slog.New(slog.NewJSONHandler(&buf, nil)))
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a record is not one JSON object: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// s0163Only returns the records at level whose message contains msg.
func s0163Only(recs []map[string]any, level, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["level"] == level && strings.Contains(fmt.Sprint(r["msg"]), msg) {
			out = append(out, r)
		}
	}
	return out
}

// TestS0163_AC1_ANumericWorkersRunsExactlyThatManyWhateverTheQuotaReads grades AC-1's
// configuration half: absent and 0 run 1, and 1 to 1024 run exactly that many, from the file
// and from HOLDFAST_WORKERS alike - under a quota that `auto` would size to a different
// count, so a numeric setting that consulted the quota could not pass by accident.
func TestS0163_AC1_ANumericWorkersRunsExactlyThatManyWhateverTheQuotaReads(t *testing.T) {
	s0163Cgroup(t, map[string]string{"cpu.max": "4800000 100000\n"})
	s0163CPUs(t, 56)
	// Anti-vacuity: under this quota `auto` is 3, which none of the cases below is.
	if got := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: auto\n").EffectiveWorkers(); got != 3 {
		t.Fatalf("the fixture quota sizes auto to %d, want 3 - the cases below would prove nothing", got)
	}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"absent", "", 1},
		{"0", "workers: 0\n", 1},
		{"1", "workers: 1\n", 1},
		{"2", "workers: 2\n", 2},
		{"4", "workers: 4\n", 4},
		{"1024", "workers: 1024\n", 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := loadYAML(t, "library_roots:\n  - /mnt/tv\n"+tc.body)
			if got := c.EffectiveWorkers(); got != tc.want {
				t.Errorf("EffectiveWorkers() = %d, want %d", got, tc.want)
			}
			p := c.WorkerPlan()
			if p.Auto || c.WorkersAuto || p.Workers != tc.want {
				t.Errorf("WorkerPlan() = %+v, want a numeric plan of %d", p, tc.want)
			}
			if p.Quota != 0 || p.QuotaSource != "" || p.Interface != "" {
				t.Errorf("a numeric setting read the quota: %+v", p)
			}
		})
	}
	t.Setenv("HOLDFAST_WORKERS", "6")
	if got := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: 2\n").EffectiveWorkers(); got != 6 {
		t.Errorf("HOLDFAST_WORKERS=6 over a file's 2 resolved to %d, want 6", got)
	}
}

// TestS0163_AC2_AutoDividesTheCPUQuotaByCoresPerWorker grades AC-2's five named cases, the
// cap, and both spellings of `auto` (the file and HOLDFAST_WORKERS).
func TestS0163_AC2_AutoDividesTheCPUQuotaByCoresPerWorker(t *testing.T) {
	for _, tc := range []struct {
		name, cpuMax string
		cpus         int
		cores        string // "" leaves cores_per_worker absent (the default of 16)
		want         int
		quota        float64
		source       string
	}{
		{"400000/100000, 56 CPUs, the default", "400000 100000", 56, "", 1, 4, QuotaFromCgroup},
		{"4800000/100000, 56 CPUs, 16", "4800000 100000", 56, "16", 3, 48, QuotaFromCgroup},
		{"150000/100000, 1 per worker", "150000 100000", 56, "1", 1, 1.5, QuotaFromCgroup},
		{"max, 56 CPUs, 16", "max 100000", 56, "16", 3, 56, QuotaFromCPUCount},
		{"4800000/100000, 8 CPUs, 4", "4800000 100000", 8, "4", 2, 8, QuotaFromCPUCount},
		{"the cap: max on 4096 CPUs at 1 per worker", "max 100000", 4096, "1", 1024, 4096, QuotaFromCPUCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s0163Cgroup(t, map[string]string{"cpu.max": tc.cpuMax + "\n"})
			s0163CPUs(t, tc.cpus)
			body := "library_roots:\n  - /mnt/tv\nworkers: auto\n"
			if tc.cores != "" {
				body += "cores_per_worker: " + tc.cores + "\n"
			}
			c := loadYAML(t, body)
			if !c.WorkersAuto {
				t.Fatal("workers: auto did not load as auto")
			}
			if got := c.EffectiveWorkers(); got != tc.want {
				t.Errorf("EffectiveWorkers() = %d, want %d", got, tc.want)
			}
			p := c.WorkerPlan()
			if p.Workers != tc.want || p.Quota != tc.quota || p.QuotaSource != tc.source || p.CPUs != tc.cpus {
				t.Errorf("WorkerPlan() = workers %d, Q %v from %q, C %d; want %d, %v from %q, %d",
					p.Workers, p.Quota, p.QuotaSource, p.CPUs, tc.want, tc.quota, tc.source, tc.cpus)
			}
			if p.Err != nil || p.Absent {
				t.Errorf("a readable cpu.max was reported unusable: %+v", p)
			}

			// The environment spelling of the same configuration resolves the same way.
			t.Setenv("HOLDFAST_WORKERS", "auto")
			if tc.cores != "" {
				t.Setenv("HOLDFAST_CORES_PER_WORKER", tc.cores)
			}
			env := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: 2\n")
			if !env.WorkersAuto || env.EffectiveWorkers() != tc.want {
				t.Errorf("HOLDFAST_WORKERS=auto resolved to auto=%v workers=%d, want auto and %d",
					env.WorkersAuto, env.EffectiveWorkers(), tc.want)
			}
		})
	}

	// The default divisor is 16 and lives in the defaults layer, so an absent key is 16.
	if v, ok := defaultLayer()[coresPerWorkerKey]; !ok || v != DefaultCoresPerWorker || DefaultCoresPerWorker != 16 {
		t.Errorf("defaultLayer() carries cores_per_worker = %v (present %v), want 16", v, ok)
	}
	// A Config assembled by hand carries no divisor and uses the default too.
	if got := (&Config{}).EffectiveCoresPerWorker(); got != 16 {
		t.Errorf("a Config with no cores_per_worker divides by %d, want 16", got)
	}
	if got := (&Config{CoresPerWorker: 5}).EffectiveCoresPerWorker(); got != 5 {
		t.Errorf("a Config carrying cores_per_worker 5 divides by %d", got)
	}
	// The floor is taken AFTER the division and before nothing else: 31.9 CPUs at 16 is
	// one worker, 32 is two, and a quota under one worker's worth is still one.
	for _, tc := range []struct {
		q     float64
		cores int
		want  int
	}{{31.9, 16, 1}, {32, 16, 2}, {0.5, 16, 1}, {3, 0, 1}, {48, 0, 3}, {2048, 1, 1024}, {1025, 1, 1024}, {1024, 1, 1024}} {
		if got := autoWorkers(tc.q, tc.cores); got != tc.want {
			t.Errorf("autoWorkers(%v, %d) = %d, want %d", tc.q, tc.cores, got, tc.want)
		}
	}
	// A Config assembled by hand with WorkersAuto set resolves from the live hierarchy.
	s0163Cgroup(t, map[string]string{"cpu.max": "3200000 100000\n"})
	s0163CPUs(t, 56)
	if got := (&Config{WorkersAuto: true}).EffectiveWorkers(); got != 2 {
		t.Errorf("a hand-built auto Config under a 32-CPU quota runs %d workers, want 2", got)
	}
	// cgroup v1's bandwidth pair is read by the same reader and named as such.
	s0163Cgroup(t, map[string]string{"cpu/cpu.cfs_quota_us": "3200000\n", "cpu/cpu.cfs_period_us": "100000\n"})
	v1 := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: auto\n").WorkerPlan()
	if v1.Workers != 2 || v1.QuotaSource != QuotaFromCgroupV1 {
		t.Errorf("a v1 quota of 32 CPUs resolved to %d workers from %q, want 2 from %q", v1.Workers,
			v1.QuotaSource, QuotaFromCgroupV1)
	}
}

// TestS0163_AC3_AnAbsentOrUnusableQuotaFallsBackToTheCPUCountAndNeverRefuses grades AC-3's
// configuration half: an ABSENT cpu.max is Q = C and one info record saying no quota
// applies; an unreadable or malformed one is Q = C too, never a refusal, and one warn record
// naming cgroup cpu.max, what was attempted and the CPU-count fallback.
func TestS0163_AC3_AnAbsentOrUnusableQuotaFallsBackToTheCPUCountAndNeverRefuses(t *testing.T) {
	s0163CPUs(t, 56)
	body := "library_roots:\n  - /mnt/tv\nworkers: auto\n"

	t.Run("absent", func(t *testing.T) {
		root := s0163Cgroup(t, map[string]string{})
		c, err := load(t, body)
		if err != nil {
			t.Fatalf("an absent quota refused the start: %v", err)
		}
		p := c.WorkerPlan()
		if !p.Absent || p.Err != nil || p.Quota != 56 || p.QuotaSource != QuotaFromCPUCount || p.Workers != 3 {
			t.Fatalf("WorkerPlan() = %+v, want Q = 56 from the CPU count and 3 workers", p)
		}
		recs := s0163Records(t, p)
		infos := s0163Only(recs, "INFO", "no CPU quota applies")
		if len(infos) != 1 {
			t.Fatalf("%d info records say no quota applies, want exactly 1: %v", len(infos), recs)
		}
		if why := fmt.Sprint(infos[0]["why"]); !strings.Contains(why, root) {
			t.Errorf("the record does not name the mount it looked under (%s): %v", root, infos[0])
		}
		if warns := s0163Only(recs, "WARN", ""); len(warns) != 0 {
			t.Errorf("an absent quota is an ordinary state and warned: %v", warns)
		}
	})

	t.Run("max names no ceiling", func(t *testing.T) {
		s0163Cgroup(t, map[string]string{"cpu.max": "max 100000\n"})
		p := loadYAML(t, body).WorkerPlan()
		recs := s0163Records(t, p)
		if !p.Unlimited || len(s0163Only(recs, "INFO", "no CPU quota applies")) != 1 ||
			len(s0163Only(recs, "WARN", "")) != 0 {
			t.Errorf("a cpu.max of max was not stated as no quota at info: %+v %v", p, recs)
		}
	})

	for _, bad := range []string{"abc 100000", "100000", "0 100000", "100000 0", "-100000 100000",
		"100000 -1", "100000 100000 5"} {
		t.Run("malformed "+bad, func(t *testing.T) {
			root := s0163Cgroup(t, map[string]string{"cpu.max": bad + "\n"})
			c, err := load(t, body)
			if err != nil {
				t.Fatalf("a malformed cpu.max refused the start: %v", err)
			}
			p := c.WorkerPlan()
			if p.Err == nil || p.Quota != 56 || p.QuotaSource != QuotaFromCPUCount || p.Workers != 3 {
				t.Fatalf("WorkerPlan() = %+v, want the CPU-count fallback of Q = 56 and 3 workers", p)
			}
			recs := s0163Records(t, p)
			warns := s0163Only(recs, "WARN", "could not be used")
			if len(warns) != 1 {
				t.Fatalf("%d warn records, want exactly 1: %v", len(warns), recs)
			}
			w := warns[0]
			if w["dependency"] != QuotaFromCgroup {
				t.Errorf("the warn names dependency %v, want %q", w["dependency"], QuotaFromCgroup)
			}
			if a := fmt.Sprint(w["attempted"]); !strings.Contains(a, root) {
				t.Errorf("the warn does not say what was attempted under %s: %v", root, w)
			}
			if w["fallback"] != QuotaFromCPUCount || w["interface"] != filepath.Join(root, "cpu.max") ||
				w["read"] != strings.TrimSpace(bad) {
				t.Errorf("the warn does not name the file, what it held and the fallback: %v", w)
			}
			if len(s0163Only(recs, "INFO", "no CPU quota applies")) != 0 {
				t.Errorf("a malformed quota was also reported as no quota: %v", recs)
			}
			if start := s0163Only(recs, "INFO", "worker pool for this run"); len(start) != 1 ||
				start[0]["cpu_quota_source"] != QuotaFromCPUCount {
				t.Errorf("the pool record does not carry the fallback source: %v", start)
			}
		})
	}
}

// TestS0163_AC4_AWorkersOrCoresPerWorkerOutsideItsFormsRefusesToLoad grades AC-4's
// configuration half: every value outside the accepted forms, from the file and from the
// environment, refuses Load naming the key and what it accepts.
func TestS0163_AC4_AWorkersOrCoresPerWorkerOutsideItsFormsRefusesToLoad(t *testing.T) {
	workersForms := []string{"workers", "0 to 1024", `auto`}
	coresForms := []string{"cores_per_worker", "1 to 1024"}
	refused := func(t *testing.T, err error, want []string) {
		t.Helper()
		if err == nil {
			t.Fatal("Load = nil, want a refusal")
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the refusal does not name %q: %v", w, err)
			}
		}
	}
	for _, bad := range []string{"-1", "1025", "2.5", "Auto", "automatic", "AUTO", "true", "", "[4]", `"auto "`} {
		t.Run("file workers "+bad, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - /mnt/tv\nworkers: "+bad+"\n")
			refused(t, err, workersForms)
		})
	}
	for _, bad := range []string{"-1", "1025", "2.5", "Auto", "automatic", "four"} {
		t.Run("env workers "+bad, func(t *testing.T) {
			t.Setenv("HOLDFAST_WORKERS", bad)
			_, err := load(t, "library_roots:\n  - /mnt/tv\n")
			refused(t, err, workersForms)
		})
	}
	for _, bad := range []string{"0", "-4", "1.5", "1025", "auto", "", "many"} {
		t.Run("file cores_per_worker "+bad, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - /mnt/tv\nworkers: auto\ncores_per_worker: "+bad+"\n")
			refused(t, err, coresForms)
		})
	}
	for _, bad := range []string{"0", "-4", "1.5", "1025"} {
		t.Run("env cores_per_worker "+bad, func(t *testing.T) {
			t.Setenv("HOLDFAST_CORES_PER_WORKER", bad)
			_, err := load(t, "library_roots:\n  - /mnt/tv\n")
			refused(t, err, coresForms)
		})
	}
	// A Config assembled by hand is held to the same ranges by Validate.
	for _, n := range []int{-1, 1025} {
		c := Config{LibraryRoots: []string{"/mnt/tv"}, CoresPerWorker: n}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cores_per_worker") {
			t.Errorf("Validate with CoresPerWorker=%d = %v, want a refusal naming cores_per_worker", n, err)
		}
	}
	// Anti-vacuity: the bounds of both keys, in both layers, load.
	for _, good := range []string{"workers: 0\n", "workers: 1024\n", "workers: auto\ncores_per_worker: 1\n",
		"workers: auto\ncores_per_worker: 1024\n", "workers: \"3\"\n"} {
		if _, err := load(t, "library_roots:\n  - /mnt/tv\n"+good); err != nil {
			t.Errorf("Load with %q refused: %v", good, err)
		}
	}
	for _, c := range []Config{{LibraryRoots: []string{"/mnt/tv"}, CoresPerWorker: 1},
		{LibraryRoots: []string{"/mnt/tv"}, CoresPerWorker: 1024}, {LibraryRoots: []string{"/mnt/tv"}}} {
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with CoresPerWorker=%d: %v", c.CoresPerWorker, err)
		}
	}
	t.Setenv("HOLDFAST_CORES_PER_WORKER", "7")
	if c := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: auto\ncores_per_worker: 3\n"); c.CoresPerWorker != 7 {
		t.Errorf("HOLDFAST_CORES_PER_WORKER=7 over a file's 3 resolved to %d, want 7", c.CoresPerWorker)
	}
}

// TestS0163_AC5_CoresPerWorkerBesideANumericWorkersIsStatedAsNoEffect grades AC-5's
// configuration half: the numeric count runs, and the statement that cores_per_worker has no
// effect unless workers is auto is a notice (`validate` prints notices, `run` and `serve` log
// them) and a field of the pool record.
func TestS0163_AC5_CoresPerWorkerBesideANumericWorkersIsStatedAsNoEffect(t *testing.T) {
	s0163Cgroup(t, map[string]string{"cpu.max": "4800000 100000\n"})
	s0163CPUs(t, 56)
	const say = "cores_per_worker is 8, which has no effect unless workers is auto"
	noted := func(c *Config) bool {
		return slices.ContainsFunc(c.Notices(), func(n string) bool { return strings.Contains(n, say) })
	}

	c := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: 4\ncores_per_worker: 8\n")
	if got := c.EffectiveWorkers(); got != 4 {
		t.Errorf("EffectiveWorkers() = %d, want the numeric 4", got)
	}
	if !noted(c) {
		t.Errorf("no notice says %q: %v", say, c.Notices())
	}
	start := s0163Only(s0163Records(t, c.WorkerPlan()), "INFO", "worker pool for this run")
	if len(start) != 1 || !strings.Contains(fmt.Sprint(start[0]["cores_per_worker_effect"]),
		"no effect unless workers is auto") || start[0]["workers"] != float64(4) {
		t.Errorf("the pool record does not state the no-effect: %v", start)
	}

	// The environment spelling of the key is the same statement.
	t.Run("from the environment", func(t *testing.T) {
		t.Setenv("HOLDFAST_CORES_PER_WORKER", "8")
		if c := loadYAML(t, "library_roots:\n  - /mnt/tv\n"); !noted(c) || c.EffectiveWorkers() != 1 {
			t.Errorf("HOLDFAST_CORES_PER_WORKER=8 with workers absent: workers %d, notices %v",
				c.EffectiveWorkers(), c.Notices())
		}
	})

	// Anti-vacuity: the key absent, or read by auto, says nothing of the kind.
	for _, body := range []string{"workers: 4\n", "workers: auto\ncores_per_worker: 8\n"} {
		c := loadYAML(t, "library_roots:\n  - /mnt/tv\n"+body)
		for _, n := range c.Notices() {
			if strings.Contains(n, "no effect unless workers is auto") {
				t.Errorf("%q earned the no-effect notice: %s", body, n)
			}
		}
		for _, r := range s0163Records(t, c.WorkerPlan()) {
			if _, ok := r["cores_per_worker_effect"]; ok {
				t.Errorf("%q earned the no-effect field: %v", body, r)
			}
		}
	}
}

// TestS0163_AC6_CoresPerWorkerInsideALibraryRootIsRefusedAsWorkersIs grades AC-6: the key is
// refused inside a library_roots entry, naming it, in exactly the shape workers is refused
// there - as a key that describes the process - and never as a typo.
func TestS0163_AC6_CoresPerWorkerInsideALibraryRootIsRefusedAsWorkersIs(t *testing.T) {
	for _, key := range []string{"cores_per_worker", "workers"} {
		t.Run(key, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - path: /mnt/tv\n    "+key+": 4\n")
			if err == nil {
				t.Fatalf("Load with %s inside a library_roots entry = nil, want a refusal", key)
			}
			for _, want := range []string{key, "describes the process, not a library"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not carry %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "typo") {
				t.Errorf("a daemon-level key must not be reported as a typo: %v", err)
			}
		})
	}
	if slices.Contains(ProfileKnobs(), coresPerWorkerKey) || isProfileKnob(coresPerWorkerKey) ||
		isFilterKey(coresPerWorkerKey) {
		t.Error("cores_per_worker is accepted inside an entry, so a root could claim a divisor nothing reads")
	}
	// Anti-vacuity: at the top level it loads.
	if c := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: auto\ncores_per_worker: 4\n"); c.CoresPerWorker != 4 {
		t.Errorf("top-level cores_per_worker: 4 loaded as %d", c.CoresPerWorker)
	}
}

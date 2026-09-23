package cpuquota

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeRoot builds a cgroup mount out of files, so the readings below run on this host
// without a second container. The cgroup filesystem is outside this package's boundary -
// the SUBJECT is the reading and the division, and both run for real here.
func fakeRoot(t *testing.T, files map[string]string) string {
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
	return root
}

// atRoot points the v2 reader at the mount root rather than at whatever cgroup this test
// process happens to be in, so a fixture's cpu.max is the file the walk reads.
func atRoot(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "self-cgroup")
	if err := os.WriteFile(p, []byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := selfCgroup
	selfCgroup = p
	t.Cleanup(func() { selfCgroup = old })
}

// TestS0160_AC1_QuotaIsReadFromTheBandwidthLimitNotTheHostCPUCount is the criterion this
// item turns on: the figure comes from the cgroup CPU bandwidth limit, and a host that
// advertises a different number of CPUs does not change it.
func TestS0160_AC1_QuotaIsReadFromTheBandwidthLimitNotTheHostCPUCount(t *testing.T) {
	atRoot(t)
	// A limit deliberately unequal to this host's CPU count, so "read the quota" and
	// "read the CPU count" cannot produce the same answer and the test cannot pass by
	// accident.
	cpus := 2
	if runtime.NumCPU() == cpus {
		cpus = 3
	}
	for name, files := range map[string]map[string]string{
		"cgroup v2": {"cpu.max": strconv.Itoa(cpus*100000) + " 100000\n"},
		"cgroup v1": {
			"cpu/cpu.cfs_quota_us":  strconv.Itoa(cpus * 100000),
			"cpu/cpu.cfs_period_us": "100000",
		},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := Read(fakeRoot(t, files))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if q.CPUs != float64(cpus) {
				t.Errorf("CPUs = %v, want %v read from the bandwidth limit", q.CPUs, cpus)
			}
			if !q.Limited {
				t.Error("Limited = false on a cgroup that names a ceiling")
			}
			if got := Divide(q, 1); got != cpus {
				t.Errorf("Divide = %d, want %d: the count must come from the quota and not from "+
					"the host's %d CPUs", got, cpus, runtime.NumCPU())
			}
		})
	}
}

// TestS0160_AC1_TheMostRestrictiveLimitOnThePathWins covers the layout the v2 walk exists
// for: a process in a nested cgroup is bounded by its parent too, and reading only its own
// file would report a ceiling it is not allowed to use.
func TestS0160_AC1_TheMostRestrictiveLimitOnThePathWins(t *testing.T) {
	root := fakeRoot(t, map[string]string{
		"cpu.max":                  "800000 100000\n", // the mount allows 8
		"system/hf/cpu.max":        "200000 100000\n", // this process is allowed 2
		"system/hf/nested/cpu.max": "max 100000\n",    // and its leaf names no ceiling of its own
	})
	self := filepath.Join(t.TempDir(), "self-cgroup")
	if err := os.WriteFile(self, []byte("0::/system/hf/nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := selfCgroup
	selfCgroup = self
	t.Cleanup(func() { selfCgroup = old })

	q, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if q.CPUs != 2 {
		t.Errorf("CPUs = %v, want 2: the walk must take the most restrictive cpu.max between the "+
			"process's own cgroup and the mount root, not the leaf's own and not the root's", q.CPUs)
	}
}

// TestS0160_AC2_SharesOfConcurrentJobsFitInsideTheQuota is the concurrency criterion: the
// per-job count times the number of jobs that may run at once is within the quota.
func TestS0160_AC2_SharesOfConcurrentJobsFitInsideTheQuota(t *testing.T) {
	for _, quota := range []float64{1, 2, 4, 8, 12, 24, 36.5} {
		for _, workers := range []int{1, 2, 3, 4, 8, 24} {
			q := Quota{CPUs: quota, Limited: true, Source: SourceCgroupV2}
			got := Divide(q, workers)
			if got < 1 {
				t.Fatalf("Divide(%v, %d) = %d: a share is never below 1", quota, workers, got)
			}
			// Above the floor, the shares of every concurrent job have to fit together.
			// Below it there is no positive integer share that does, and one thread each
			// is what an unthreaded build already asked for.
			if float64(workers) <= quota && float64(got*workers) > quota {
				t.Errorf("Divide(%v, %d) = %d: %d workers would ask for %d threads against a quota "+
					"of %v", quota, workers, got, workers, got*workers, quota)
			}
			if float64(workers) > quota && got != 1 {
				t.Errorf("Divide(%v, %d) = %d, want 1: with more workers than CPUs of quota the "+
					"smallest request is all there is", quota, workers, got)
			}
		}
	}
}

// TestS0160_AC2_MoreWorkersNeverMeansMoreThreadsEach is the anti-vacuity half: the
// division has to actually divide, or AC-2 would hold for a build that ignored workers.
func TestS0160_AC2_MoreWorkersNeverMeansMoreThreadsEach(t *testing.T) {
	q := Quota{CPUs: 24, Limited: true, Source: SourceCgroupV2}
	one, four := Divide(q, 1), Divide(q, 4)
	if one != 24 || four != 6 {
		t.Errorf("Divide(24 CPUs) = %d at one worker and %d at four, want 24 and 6", one, four)
	}
	prev := Divide(q, 1)
	for w := 2; w <= 24; w++ {
		got := Divide(q, w)
		if got > prev {
			t.Errorf("Divide(24, %d) = %d rose above the %d at %d workers: the share must not grow "+
				"as the concurrency does", w, got, prev, w-1)
		}
		prev = got
	}
}

// TestS0160_AC5_AnUnreadableQuotaIsAStatedErrorNotAGuess covers the three ways the reading
// can fail: the file is absent, it cannot be read, or its contents do not parse. Each is a
// typed error naming what was attempted, never a number derived from a layout this build
// did not understand.
func TestS0160_AC5_AnUnreadableQuotaIsAStatedErrorNotAGuess(t *testing.T) {
	atRoot(t)
	cases := map[string]map[string]string{
		"no cgroup files at all":     {},
		"v2 empty":                   {"cpu.max": "\n"},
		"v2 one field":               {"cpu.max": "200000\n"},
		"v2 three fields":            {"cpu.max": "200000 100000 50000\n"},
		"v2 non-numeric quota":       {"cpu.max": "lots 100000\n"},
		"v2 non-numeric period":      {"cpu.max": "200000 often\n"},
		"v2 zero period":             {"cpu.max": "200000 0\n"},
		"v2 negative quota":          {"cpu.max": "-5 100000\n"},
		"v1 non-numeric quota":       {"cpu/cpu.cfs_quota_us": "plenty", "cpu/cpu.cfs_period_us": "100000"},
		"v1 zero period":             {"cpu/cpu.cfs_quota_us": "200000", "cpu/cpu.cfs_period_us": "0"},
		"v1 quota without a period":  {"cpu/cpu.cfs_quota_us": "200000"},
		"v1 zero quota":              {"cpu/cpu.cfs_quota_us": "0", "cpu/cpu.cfs_period_us": "100000"},
		"a directory where the file": {"cpu.max/placeholder": "not a file"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			q, err := Read(fakeRoot(t, files))
			if err == nil {
				t.Fatalf("Read returned %+v and no error: an unreadable or malformed quota must be "+
					"stated, because the only number available to guess with is the host CPU count", q)
			}
			if !errors.Is(err, ErrNoQuota) {
				t.Errorf("error %v does not wrap ErrNoQuota, so a caller cannot tell it from a "+
					"reading", err)
			}
			if q != (Quota{}) {
				t.Errorf("Read returned %+v beside its error - a failed reading carries no figure", q)
			}
		})
	}
}

// TestS0160_AC5_FallbackShareIsOneAndNotZero pins the stated fallback. 0 is not a smaller
// request, it is libvmaf's own default, which is the single-CPU measurement this item
// exists to end.
func TestS0160_AC5_FallbackShareIsOneAndNotZero(t *testing.T) {
	if FallbackShare != 1 {
		t.Errorf("FallbackShare = %d, want 1", FallbackShare)
	}
}

// TestS0160_AC6_AnUnlimitedQuotaSizesFromTheCPUsThisProcessMayRunOn covers both spellings
// of "no ceiling", v2's "max" and v1's -1.
func TestS0160_AC6_AnUnlimitedQuotaSizesFromTheCPUsThisProcessMayRunOn(t *testing.T) {
	atRoot(t)
	for name, files := range map[string]map[string]string{
		"cgroup v2 max": {"cpu.max": "max 100000\n"},
		"cgroup v1 -1": {
			"cpu/cpu.cfs_quota_us":  "-1",
			"cpu/cpu.cfs_period_us": "100000",
		},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := Read(fakeRoot(t, files))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if q.Limited {
				t.Error("Limited = true on a cgroup that names no ceiling")
			}
			if q.CPUs != float64(runtime.NumCPU()) {
				t.Errorf("CPUs = %v, want the %d CPUs this process may be scheduled on",
					q.CPUs, runtime.NumCPU())
			}
			if q.Source != SourceAffinity {
				t.Errorf("Source = %q, want %q", q.Source, SourceAffinity)
			}
			if n := Divide(q, 1); n < 1 {
				t.Errorf("Divide = %d: an unlimited quota must still derive a count of at least 1, "+
					"because libvmaf reads 0 as its own default rather than as a request", n)
			}
		})
	}
}

// TestS0160_AC7_LessThanOneWholeCPUIsExactlyOneThread.
func TestS0160_AC7_LessThanOneWholeCPUIsExactlyOneThread(t *testing.T) {
	atRoot(t)
	for name, files := range map[string]map[string]string{
		"v2 half a CPU":   {"cpu.max": "50000 100000\n"},
		"v2 a tenth":      {"cpu.max": "10000 100000\n"},
		"v2 one microsec": {"cpu.max": "1 100000\n"},
		"v1 three tenths": {"cpu/cpu.cfs_quota_us": "30000", "cpu/cpu.cfs_period_us": "100000"},
		"v2 just under 1": {"cpu.max": "99999 100000\n"},
		"v2 just under 2": {"cpu.max": "199999 100000\n"},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := Read(fakeRoot(t, files))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			want := 1
			if q.CPUs >= 2 {
				want = int(q.CPUs)
			}
			if got := Divide(q, 1); got != want {
				t.Errorf("Divide(%v CPUs) = %d, want %d", q.CPUs, got, want)
			}
		})
	}
	// The floor is a floor and not a rounding: 1.9 CPUs is one whole CPU of share, never
	// two, or the shares of two concurrent jobs would exceed the quota they came from.
	if got := Divide(Quota{CPUs: 1.9, Limited: true}, 1); got != 1 {
		t.Errorf("Divide(1.9 CPUs) = %d, want 1: the share is floored, not rounded", got)
	}
}

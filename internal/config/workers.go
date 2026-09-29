package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// The size of the worker pool: a whole number an operator writes, or `auto`, which sizes
// the pool from the CPU quota this process actually runs under.
//
// A whole number is exactly what it always was - absent and 0 mean 1, and 1 to 1024 run
// that many workers whatever the quota reads - so no configuration written before `auto`
// existed changes its worker count. `auto` is opt-in and resolves ONCE, at load:
//
//	workers = max(1, floor(Q / cores_per_worker)), capped at 1024
//
// where Q is the CPU quota: the cgroup's cpu.max bandwidth (MAX / PERIOD), read by the same
// reader the libx265 pool and the VMAF threads are sized from (internal/cpuquota), and never
// more than C, the number of CPUs the process may run on (its affinity set). A cgroup that
// names no ceiling, or has no CPU controller at all, leaves Q = C. Q may be fractional; the
// floor is taken after the division, so a quota of 1.5 CPUs at one core per worker is one
// worker and never two.
//
// A quota that cannot be read never refuses a start. More or fewer workers is a throughput
// question and no data-safety property depends on the count, so an unreadable or malformed
// cpu.max falls back to the CPU count and says so at warn (Announce), where refusing would
// turn a reporting problem into an outage.

const (
	// workersKey and coresPerWorkerKey are the one place each key is spelled: knownKeys,
	// defaultLayer, the refusals and the notice all read them from here.
	workersKey        = "workers"
	coresPerWorkerKey = "cores_per_worker"

	// WorkersAuto is the one accepted spelling of the value that sizes the pool from the
	// CPU quota. It is exact and lowercase: `Auto` or `automatic` is a typo on the knob
	// that decides how many transcode-and-delete pipelines run at once, and it is refused
	// rather than guessed at.
	WorkersAuto = "auto"

	// DefaultCoresPerWorker is the CPUs of quota one worker is sized for under `auto`.
	// One worker measured a host load of 16 to 19 on the report that asked for `auto`, so
	// 16 is one worker's worth of CPU as observed rather than as hoped.
	DefaultCoresPerWorker = 16

	// maxWorkers is the ceiling on the pool, numeric or derived: a figure past it is a
	// typo on any host this build runs on, not a pool.
	maxWorkers = 1024
	// maxCoresPerWorker bounds the divisor the same way.
	maxCoresPerWorker = 1024
)

// Where Q came from, as `validate` prints it and the startup record carries it. An operator
// reading "3 workers" cannot tell a quota of 48 CPUs from a 48-CPU host with no quota at
// all, and the two want different actions when the figure is wrong.
const (
	// QuotaFromCgroup is the cgroup v2 cpu.max bandwidth limit.
	QuotaFromCgroup = "cgroup cpu.max"
	// QuotaFromCgroupV1 is the cgroup v1 bandwidth pair, which the shared reader also
	// reads on a host that mounts only v1.
	QuotaFromCgroupV1 = "cgroup cpu.cfs_quota_us"
	// QuotaFromCPUCount is the CPUs the process may run on: no ceiling was named, the
	// ceiling was above the CPU count, or no quota could be read at all.
	QuotaFromCPUCount = "cpu count"
)

// numCPU is C, the number of CPUs this process may run on. runtime.NumCPU reads the
// affinity mask at startup, which is the set the Definitions name. A var so a test can
// stand in a 56-CPU or an 8-CPU host on whatever machine it runs on.
var numCPU = runtime.NumCPU

// WorkerPlan is the resolved size of the worker pool and the account of where it came from.
// Every field past Setting is `auto`'s account and is zero for a numeric setting.
type WorkerPlan struct {
	// Workers is how many workers the pool runs: at least 1, at most 1024.
	Workers int
	// Auto reports that workers was written `auto`.
	Auto bool
	// Setting is workers as this configuration resolved it: "auto", or the whole number
	// (0 for a Config that carries none, which runs the default of 1).
	Setting string
	// CoresPerWorker is the divisor `auto` uses: the configured value, or the default.
	CoresPerWorker int
	// CoresPerWorkerSet reports that cores_per_worker was written, in the file or the
	// environment, whether or not workers is `auto`.
	CoresPerWorkerSet bool

	// Quota is Q, in CPUs, and QuotaSource where it came from.
	Quota       float64
	QuotaSource string
	// CPUs is C, the CPUs the process may run on.
	CPUs int
	// Root is the cgroup mount that was read.
	Root string
	// Interface is the file the quota was read from, or the one that could not be used.
	Interface string
	// Read is what that file held, where it held something that does not parse.
	Read string
	// Unlimited reports that the cgroup names no ceiling ("max").
	Unlimited bool
	// Absent reports that no cgroup CPU bandwidth interface exists at all.
	Absent bool
	// Err is why a bandwidth interface that EXISTS could not be used, and nil otherwise.
	Err error
}

// resolveAutoWorkers resolves `workers: auto` against the cgroup hierarchy mounted at root
// (cpuquota.DefaultRoot where it is "") on a process that may run on cpus CPUs.
func resolveAutoWorkers(coresPerWorker int, root string, cpus int) WorkerPlan {
	if root == "" {
		root = cpuquota.DefaultRoot
	}
	cpus = max(cpus, 1)
	p := WorkerPlan{Auto: true, Setting: WorkersAuto, CoresPerWorker: coresPerWorker,
		CPUs: cpus, Root: root, Quota: float64(cpus), QuotaSource: QuotaFromCPUCount}
	q, err := cpuquota.Read(root)
	switch {
	case err == nil && q.Limited:
		p.Interface = q.Origin
		// Q is the SMALLER of the bandwidth and the CPU count: a quota of 48 CPUs on a
		// process pinned to 8 cannot use more than 8 at once.
		if q.CPUs < p.Quota {
			p.Quota, p.QuotaSource = q.CPUs, quotaSourceOf(q.Source)
		}
	case err == nil:
		p.Interface, p.Unlimited = q.Origin, true
	case errors.Is(err, cpuquota.ErrNoInterface):
		p.Absent = true
	default:
		p.Err = err
		var ie *cpuquota.InterfaceError
		if errors.As(err, &ie) {
			p.Interface, p.Read = ie.Path, ie.Content
		}
	}
	p.Workers = autoWorkers(p.Quota, coresPerWorker)
	return p
}

// autoWorkers is the formula: max(1, floor(q / coresPerWorker)), capped at maxWorkers.
func autoWorkers(q float64, coresPerWorker int) int {
	if coresPerWorker < 1 {
		coresPerWorker = DefaultCoresPerWorker
	}
	return int(min(max(math.Floor(q/float64(coresPerWorker)), 1), maxWorkers))
}

// quotaSourceOf names the file a limited reading came from.
func quotaSourceOf(source string) string {
	if source == cpuquota.SourceCgroupV1 {
		return QuotaFromCgroupV1
	}
	return QuotaFromCgroup
}

// WorkerPlan is this configuration's worker pool. Load resolves `auto` once, so every reader
// of one loaded Config - and every copy of it - sees one answer. A Config assembled by hand
// with WorkersAuto set is resolved here, from the live hierarchy, each time it is asked.
func (c *Config) WorkerPlan() WorkerPlan {
	if !c.WorkersAuto {
		return WorkerPlan{Workers: c.numericWorkers(), Setting: strconv.Itoa(c.Workers),
			CoresPerWorker: c.EffectiveCoresPerWorker(), CoresPerWorkerSet: c.coresPerWorkerSet}
	}
	p := c.autoPlan
	if !p.Auto {
		p = resolveAutoWorkers(c.EffectiveCoresPerWorker(), os.Getenv(cpuquota.RootEnv), numCPU())
	}
	p.CoresPerWorkerSet = c.coresPerWorkerSet
	return p
}

// numericWorkers is a whole-number workers setting as it has always resolved: 0 (absent) or
// a negative value is 1.
func (c *Config) numericWorkers() int { return max(c.Workers, 1) }

// EffectiveCoresPerWorker is the divisor `auto` uses: the configured value, or the default
// where a Config carries none.
func (c *Config) EffectiveCoresPerWorker() int {
	if c.CoresPerWorker < 1 {
		return DefaultCoresPerWorker
	}
	return c.CoresPerWorker
}

// workersValue reads the RAW workers value a layer carried: a whole number from 0 to 1024,
// or exactly `auto`. It runs before the decoder, which would truncate 2.5 to 2 and read
// `true` as 1 - two worker counts nobody wrote - and would refuse `auto` in words that do
// not name what IS accepted.
func workersValue(raw any, where string) (auto bool, err error) {
	if s, ok := raw.(string); ok && s == WorkersAuto {
		return true, nil
	}
	if n, ok := wholeNumber(raw); ok && isWholeNumber(raw) && n >= 0 && n <= maxWorkers {
		return false, nil
	}
	return false, fmt.Errorf("%s %s in %s is not accepted: write a whole number from 0 to %d "+
		"(0 means the default of 1), or %s to size the pool from the CPU quota "+
		"(floor of the quota over %s)", workersKey, renderRaw(raw), where, maxWorkers, WorkersAuto,
		coresPerWorkerKey)
}

// coresPerWorkerValue reads the RAW cores_per_worker value a layer carried: a whole number
// from 1 to 1024. 0 is refused rather than read as the default, because a written 0 is a
// divisor of nothing, not a request for 16.
func coresPerWorkerValue(raw any, where string) error {
	if n, ok := wholeNumber(raw); ok && isWholeNumber(raw) && n >= 1 && n <= maxCoresPerWorker {
		return nil
	}
	return fmt.Errorf("%s %s in %s is not accepted: write a whole number of CPUs from 1 to %d "+
		"(the default is %d): it is how much of the CPU quota one worker is sized for when %s is %s",
		coresPerWorkerKey, renderRaw(raw), where, maxCoresPerWorker, DefaultCoresPerWorker,
		workersKey, WorkersAuto)
}

// renderRaw names a raw value back to the operator the way they wrote it.
func renderRaw(raw any) string {
	switch v := raw.(type) {
	case nil:
		return "(no value)"
	case string:
		return strconv.Quote(v)
	}
	return fmt.Sprint(raw)
}

// coresPerWorkerNotice is what Notices says about a cores_per_worker that nothing reads.
func (c *Config) coresPerWorkerNotice() []string {
	if !c.coresPerWorkerSet || c.WorkersAuto {
		return nil
	}
	return []string{fmt.Sprintf("%s is %d, which has no effect unless %s is %s: this configuration "+
		"runs %d worker(s), the number %s names, whatever the CPU quota reads. Set %s: %s to size "+
		"the pool from the quota, or remove %s", coresPerWorkerKey, c.CoresPerWorker, workersKey,
		WorkersAuto, c.numericWorkers(), workersKey, workersKey, WorkersAuto, coresPerWorkerKey)}
}

// Announce states the worker pool of a run, once, at start. For `auto` it is preceded by
// the quota record where one is owed: an INFO where no CPU quota applies (the cgroup has no
// CPU controller, or names no ceiling), which is an ordinary state; and a WARN where the
// cgroup's cpu.max exists and could not be used, naming the dependency, what was tried and
// that the pool was sized from the CPU count instead (observability O4). Neither refuses
// the start.
//
// The pool record itself is always INFO and carries the figures as fields, so a reader can
// select on them: workers and workers_setting always, and for `auto` the quota, where it came
// from, the CPU count and cores_per_worker.
func (p WorkerPlan) Announce(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "workers")
	if p.Auto {
		switch {
		case p.Err != nil:
			log.Warn("the cgroup CPU quota exists but could not be used, so workers: auto sized the "+
				"pool from the CPU count instead: the run starts, with one worker per cores_per_worker "+
				"of the CPUs this process may run on",
				"dependency", QuotaFromCgroup,
				"attempted", "read the CPU bandwidth limit under "+p.Root,
				"interface", p.Interface, "read", p.Read, "err", p.Err,
				"fallback", QuotaFromCPUCount, "cpus", p.CPUs,
				"next_action", "continue with the pool sized from the CPU count")
		case p.Absent || p.Unlimited:
			why := "no cgroup CPU bandwidth interface exists under " + p.Root
			if p.Unlimited {
				why = "the cgroup names no CPU ceiling (" + p.Interface + " is max)"
			}
			log.Info("no CPU quota applies to this process, so workers: auto divides the CPUs it may "+
				"run on", "why", why, "dependency", QuotaFromCgroup, "cpus", p.CPUs,
				"quota_source", QuotaFromCPUCount)
		}
	}
	attrs := []any{"workers", p.Workers, "workers_setting", p.Setting}
	if p.Auto {
		attrs = append(attrs, "cpu_quota", p.Quota, "cpu_quota_source", p.QuotaSource,
			"cpus", p.CPUs, "cores_per_worker", p.CoresPerWorker)
	} else if p.CoresPerWorkerSet {
		attrs = append(attrs, "cores_per_worker", p.CoresPerWorker,
			"cores_per_worker_effect", "none: cores_per_worker has no effect unless workers is auto")
	}
	log.Info("worker pool for this run", attrs...)
}

package engine

import (
	"log/slog"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// vmafThreadPlan is how many threads a quality gate asks libvmaf for, together with the
// account of where the number came from. The two travel as one value because a count with
// no account cannot be reported: an operator meeting "5 threads" cannot tell a quota of
// five CPUs from a five-CPU machine with no quota at all, and the two want different
// actions.
//
// It is derived ONCE per run and SILENTLY. The announcement is a separate step, taken the
// first time the gate actually runs, because an Engine is also constructed by commands
// that never score anything - `plan` and `analyze` enumerate and report - and a scoring
// thread count announced there is a line about work that is not going to happen, emitted
// onto whatever writer that command was handed.
type vmafThreadPlan struct {
	// threads is the count, never below 1.
	threads int
	// quota is what was read, and the zero value where nothing could be.
	quota cpuquota.Quota
	// root is the cgroup mount that was read, named in the account.
	root string
	// workers is the concurrency the quota was divided across.
	workers int
	// err is why no quota could be read, and nil on every reading that succeeded.
	err error
}

// deriveVmafThreads is the whole of how many threads a quality gate asks libvmaf for.
//
// Why it is derived at all: scoring is the slowest phase of a job by a wide margin, and
// libvmaf with no thread count runs on about one CPU. Why it is derived from the QUOTA and
// not from the CPU count: inside a bandwidth-limited container the CPU count is the host's
// answer, and a gate sized from it asks for CPU the scheduler will never hand over. Why it
// is divided by the workers: the gates of every concurrently running worker are asking at
// the same time, and shares that each fit the quota alone do not fit it together.
//
// It never fails. A quota that cannot be read yields the stated fallback of one thread,
// which is what an unthreaded build already ran at, so a host whose cgroup layout this
// build does not understand is exactly as fast as it was and never oversubscribed. The
// gate is not skipped and no job fails; announce() says so, once.
//
// root is the cgroup mount to read, taken as a parameter so a test can hand it a directory
// it built: the cgroup filesystem is outside this function's boundary, and faking it is
// how the unreadable, unlimited and sub-CPU paths are exercised without a second host.
func deriveVmafThreads(root string, workers int) vmafThreadPlan {
	p := vmafThreadPlan{root: root, workers: workers, threads: cpuquota.FallbackShare}
	p.quota, p.err = cpuquota.Read(root)
	if p.err == nil {
		p.threads = cpuquota.Divide(p.quota, workers)
	}
	return p
}

// announce states the derivation once, at the level that matches what happened: a quota
// that could not be read is a WARN naming the dependency, what was attempted and what was
// done instead (observability O4), because the run continues in a degraded state and that
// is what warn means here (O3). A reading that succeeded is an INFO: nothing is degraded,
// and the fields are what an operator checks the derivation against.
func (p vmafThreadPlan) announce(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if p.err != nil {
		log.Warn("the effective CPU quota could not be read, so the quality gate runs libvmaf on a "+
			"single thread: the gate still runs and no job fails, but scoring is as slow as it was "+
			"and no thread count is guessed from the host CPU count, which inside a limited "+
			"container would ask for CPU this process is not allowed",
			"dependency", "cgroup cpu bandwidth limit",
			"attempted", p.root,
			"next_action", "continue with the fallback thread count",
			"vmaf_threads", p.threads,
			"err", p.err)
		return
	}
	log.Info("libvmaf thread count derived from the CPU this process is allowed",
		"vmaf_threads", p.threads, "effective_cpus", p.quota.CPUs, "quota_limited", p.quota.Limited,
		"quota_source", p.quota.Source, "quota_origin", p.quota.Origin, "workers", p.workers)
}

// vmafThreadCount is what the gate actually asks for, and it is never below 1.
//
// The floor is the same statement deriveVmafThreads makes on an unreadable quota, applied
// where the count is READ rather than where it is derived: libvmaf reads 0 as its own
// default rather than as a request, so an Engine assembled without going through New must
// still produce a count the filter can act on rather than one that silently means "decide
// for me".
func (e *Engine) vmafThreadCount() int {
	if e.vmafThreads.threads < 1 {
		return cpuquota.FallbackShare
	}
	return e.vmafThreads.threads
}

// announceVmafThreads states the derivation the first time the gate is about to use it,
// and never again. Once per Engine is once per run: a oneshot pass builds one, and the
// daemon builds one for the life of the process, so an operator gets the line once rather
// than once per file or once per scan.
//
// It is guarded rather than emitted at construction because the workers reach the gate
// concurrently, and because the commands that build an Engine without scoring anything
// must stay silent about a scoring thread count.
func (e *Engine) announceVmafThreads() {
	e.vmafThreadsSaid.Do(func() { e.vmafThreads.announce(e.Log) })
}

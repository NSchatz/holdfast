package engine

import (
	"log/slog"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// vmafThreadCount is what the gate actually asks for, and it is never below 1.
//
// The floor is the same statement deriveVmafThreads makes on an unreadable quota, applied
// where the count is READ rather than where it is derived: libvmaf reads 0 as its own
// default rather than as a request, so an Engine assembled without going through New must
// still produce a count the filter can act on rather than one that silently means "decide
// for me".
func (e *Engine) vmafThreadCount() int {
	if e.vmafThreads < 1 {
		return cpuquota.FallbackShare
	}
	return e.vmafThreads
}

// deriveVmafThreads is the whole of how many threads a quality gate asks libvmaf for, and
// it is answered ONCE per run rather than per file.
//
// Why it is derived at all: scoring is the slowest phase of a job by a wide margin, and
// libvmaf with no thread count runs on about one CPU. Why it is derived from the QUOTA and
// not from the CPU count: inside a bandwidth-limited container the CPU count is the host's
// answer, and a gate sized from it asks for CPU the scheduler will never hand over. Why it
// is divided by the workers: the gates of every concurrently running worker are asking at
// the same time, and shares that each fit the quota alone do not fit it together.
//
// It never fails. A quota that cannot be read is a WARN and a stated fallback of one
// thread, which is what an unthreaded build already ran at, so a host whose cgroup layout
// this build does not understand is exactly as fast as it was and never oversubscribed.
// The gate is not skipped, no job fails, and the line says which dependency could not be
// read, what was attempted and what was done instead.
//
// root is the cgroup mount to read, taken as a parameter so a test can hand it a directory
// it built: the cgroup filesystem is outside this function's boundary, and faking it is
// how the unreadable, unlimited and sub-CPU paths are exercised without a second host.
func deriveVmafThreads(root string, workers int, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	q, err := cpuquota.Read(root)
	if err != nil {
		log.Warn("the effective CPU quota could not be read, so the quality gate runs libvmaf on a "+
			"single thread: the gate still runs and no job fails, but scoring is as slow as it was "+
			"and no thread count is guessed from the host CPU count, which inside a limited "+
			"container would ask for CPU this process is not allowed",
			"dependency", "cgroup cpu bandwidth limit",
			"attempted", root,
			"next_action", "continue with the fallback thread count",
			"vmaf_threads", cpuquota.FallbackShare,
			"err", err)
		return cpuquota.FallbackShare
	}
	n := cpuquota.Divide(q, workers)
	log.Info("libvmaf thread count derived from the CPU this process is allowed",
		"vmaf_threads", n, "effective_cpus", q.CPUs, "quota_limited", q.Limited,
		"quota_source", q.Source, "quota_origin", q.Origin, "workers", workers)
	return n
}

package engine

import (
	"context"
	"log/slog"
	"sync"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// vmafThreadPlan is the CPU bandwidth the quality gates of this run share, together with
// the account of where the number came from. The two travel as one value because a count
// with no account cannot be reported: an operator meeting "5 threads" cannot tell a quota
// of five CPUs from a five-CPU machine with no quota at all, and the two want different
// actions.
//
// It is derived ONCE per run and SILENTLY. The announcement is a separate step, taken the
// first time the gate actually runs, because an Engine is also constructed by commands
// that never score anything - `plan` and `analyze` enumerate and report - and a scoring
// thread count announced there is a line about work that is not going to happen, emitted
// onto whatever writer that command was handed.
type vmafThreadPlan struct {
	// threads is the share one gate asks for when the configured workers are all that
	// may score at once, never below 1. It is the figure announce() states; the share a
	// gate actually takes is vmafThreadCount's, which may be smaller.
	threads int
	// quota is what was read, and the zero value where nothing could be.
	quota cpuquota.Quota
	// root is the cgroup mount that was read, named in the account.
	root string
	// workers is the configured concurrency, the smallest divisor a gate uses.
	workers int
	// err is why no quota could be read, and nil on every reading that succeeded.
	err error
}

// deriveVmafThreads reads the CPU bandwidth the quality gates of this run share.
//
// Why it is derived at all: scoring is the slowest phase of a job by a wide margin, and
// libvmaf with no thread count runs on about one CPU. Why it is derived from the QUOTA and
// not from the CPU count: inside a bandwidth-limited container the CPU count is the host's
// answer, and a gate sized from it asks for CPU the scheduler will never hand over. Why it
// is divided: the gates of every file in flight are asking at the same time, and shares
// that each fit the quota alone do not fit it together.
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

// bounded reports whether this plan carries a quota the gates can be held to. A quota
// that could not be read carries none - the ceiling is unknown, and each gate asks for the
// stated fallback exactly as each gate of the build before this one effectively did - and
// neither does a plan assembled by hand with only a count in it.
func (p vmafThreadPlan) bounded() bool { return p.err == nil && p.quota.CPUs > 0 }

// budget is how many threads the gates scoring at once may hold between them: the quota
// in whole CPUs, and never below 1, so a single gate can always run.
func (p vmafThreadPlan) budget() int { return cpuquota.Divide(p.quota, 1) }

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
	log.Info("libvmaf thread count derived from the CPU this process is allowed; a gate that "+
		"starts while more files are in flight than there are workers takes a smaller share, "+
		"and the gates scoring at once never hold more than the budget between them",
		"vmaf_threads", p.threads, "gate_thread_budget", p.budget(),
		"effective_cpus", p.quota.CPUs, "quota_limited", p.quota.Limited,
		"quota_source", p.quota.Source, "quota_origin", p.quota.Origin, "workers", p.workers)
}

// vmafThreadCount is the share a gate starting NOW asks for, and it is never below 1.
//
// The divisor is the larger of the configured workers and the files in flight. A oneshot
// run never has more files in flight than workers, so it asks for exactly what the plan
// states. The daemon runs more than one pool over the same engine - the scan's workers,
// the submission queue's and the watch's, each `workers` wide - and when they have more
// files in flight between them than one pool's width, every gate that starts divides the
// quota by all of them rather than by the width of the pool it happens to come from.
//
// The floor of 1 is the same statement deriveVmafThreads makes on an unreadable quota,
// applied where the count is READ: libvmaf reads 0 as its own default rather than as a
// request, so an Engine assembled without going through New must still produce a count the
// filter can act on rather than one that silently means "decide for me".
func (e *Engine) vmafThreadCount() int {
	p := e.vmafThreads
	if !p.bounded() {
		if p.threads < 1 {
			return cpuquota.FallbackShare
		}
		return p.threads
	}
	n := int(e.gateFlight.Load())
	if n < p.workers {
		n = p.workers
	}
	return cpuquota.Divide(p.quota, n)
}

// enterGateFlight counts one claimed file toward the divisor vmafThreadCount uses, and
// returns the one way to stop counting it. Calling that more than once is harmless, so the
// caller can both defer it and call it as soon as the gates have ruled; it is called from
// the goroutine that took it, so the guard needs no lock.
func (e *Engine) enterGateFlight() (leave func()) {
	e.gateFlight.Add(1)
	left := false
	return func() {
		if !left {
			left = true
			e.gateFlight.Add(-1)
		}
	}
}

// gateBudget is the account of the threads the gates scoring right now hold. The zero
// value is ready to use.
type gateBudget struct {
	mu   sync.Mutex
	held int
	// freed is closed, and forgotten, whenever threads are handed back, so every gate
	// waiting for room wakes and asks again.
	freed chan struct{}
}

// takeGateThreads is the share one gate scores with, taken out of the budget, and the
// release that hands it back.
//
// The divided share alone is not a bound. A gate sizes itself from what is in flight when
// it STARTS, and a gate that started while one file was in flight keeps its larger share
// after a second pool has put more files behind it; shares that each fit when taken do
// not fit together afterwards. So the share is taken out of an account of what the gates
// scoring right now hold, capped at the quota, and a gate that would take the sum over it
// waits until a running gate hands threads back. Waiting costs nothing the host had: the
// gates already scoring hold the whole budget, so the CPU is busy either way.
//
// A lone gate is always admitted, and its share never exceeds the budget, so nothing waits
// on a gate that cannot start. A plan with no readable quota is not held to a budget at
// all: its ceiling is unknown, and each gate asks for the fallback of one, which is what
// every gate of the build before this one asked for.
func (e *Engine) takeGateThreads(ctx context.Context, file string) (int, func(), error) {
	b := &e.gateThreads
	said := false
	for {
		n := e.vmafThreadCount()
		if !e.vmafThreads.bounded() {
			return n, func() {}, nil
		}
		b.mu.Lock()
		if b.held == 0 || b.held+n <= e.vmafThreads.budget() {
			b.held += n
			b.mu.Unlock()
			var once sync.Once
			return n, func() { once.Do(func() { b.give(n) }) }, nil
		}
		if b.freed == nil {
			b.freed = make(chan struct{})
		}
		wait, held := b.freed, b.held
		b.mu.Unlock()
		if !said {
			said = true
			log := e.Log
			if log == nil {
				log = slog.Default()
			}
			log.Info("the quality gate is waiting for CPU: the gates already scoring hold the "+
				"thread budget, and this one starts as soon as one of them hands threads back",
				"file", file, "vmaf_threads", n, "gate_threads_held", held,
				"gate_thread_budget", e.vmafThreads.budget())
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

// give hands n threads back and wakes every gate waiting for room.
func (b *gateBudget) give(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= n
	if b.freed != nil {
		close(b.freed)
		b.freed = nil
	}
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

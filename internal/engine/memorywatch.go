package engine

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/memlimit"
)

// MemoryBound is the resident-memory ceiling every encode of a run is held to: the effective
// cgroup memory limit and the threshold derived from it (memlimit.Percent of it, rounded
// down). The zero value is no ceiling at all, and an encoder carrying it runs every encode
// exactly as it ran before the watchdog existed.
type MemoryBound struct {
	// Limit is the effective cgroup memory limit, in bytes.
	Limit int64
	// Threshold is the resident memory, in bytes, at or above which an encode is aborted.
	Threshold int64
}

// Armed reports whether encodes are watched at all.
func (b MemoryBound) Armed() bool { return b.Threshold > 0 }

// The watchdog's timing. The interval is how often an encode's resident memory is sampled,
// and the grace is how long a polite termination request is given before the process is
// killed outright. Together they bound the time from a crossing to the process being gone:
// one interval to see it, one grace to act on it, well inside ten seconds.
const (
	memorySampleInterval = time.Second
	memoryKillGrace      = 3 * time.Second
)

// MemoryAbortError is the error an encode returns when the watchdog terminated it. It is
// ALWAYS a failed encode, whatever the process's exit status was: a process asked to stop
// may still exit 0, and what it left behind is a truncated file, never a candidate for the
// swap.
type MemoryAbortError struct {
	// RSS is the resident memory the process was observed at, in bytes.
	RSS int64
	// Limit and Threshold are the bound it was held to, in bytes.
	Limit, Threshold int64
}

func (e *MemoryAbortError) Error() string {
	return fmt.Sprintf("encode aborted for memory: ffmpeg's resident memory reached %d bytes, at or above "+
		"the abort threshold of %d bytes (%d%% of the cgroup memory limit of %d bytes)",
		e.RSS, e.Threshold, memlimit.Percent, e.Limit)
}

// memoryWatchdog samples one running encode's resident memory and terminates it when the
// sample reaches the threshold. It never touches the job's context: the engine reads a
// cancelled context as an interruption (the SIGTERM drain), and an abort is a failure.
type memoryWatchdog struct {
	bound    MemoryBound
	proc     *os.Process
	procRoot string
	exited   chan struct{}
	done     chan struct{}
	// abort is written by the watchdog goroutine before it closes done, and read only after.
	abort *MemoryAbortError
}

// startMemoryWatchdog begins watching p, reading its resident memory under procRoot ("" is
// /proc). The caller must call stop once the process has been waited for.
func startMemoryWatchdog(b MemoryBound, p *os.Process, procRoot string) *memoryWatchdog {
	if procRoot == "" {
		procRoot = "/proc"
	}
	w := &memoryWatchdog{bound: b, proc: p, procRoot: procRoot,
		exited: make(chan struct{}), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *memoryWatchdog) run() {
	defer close(w.done)
	tick := time.NewTicker(memorySampleInterval)
	defer tick.Stop()
	for {
		select {
		case <-w.exited:
			return
		case <-tick.C:
		}
		// A sample that cannot be read - the process has just exited, or its entry is gone -
		// decides nothing: the process's own exit decides the encode's outcome.
		rss, ok := vmRSS(w.procRoot, w.proc.Pid)
		if !ok || rss < w.bound.Threshold {
			continue
		}
		w.abort = &MemoryAbortError{RSS: rss, Limit: w.bound.Limit, Threshold: w.bound.Threshold}
		_ = w.proc.Signal(syscall.SIGTERM)
		select {
		case <-w.exited:
		case <-time.After(memoryKillGrace):
			_ = w.proc.Kill()
		}
		return
	}
}

// stop ends the watch once the process has been waited for, and returns the abort the
// watchdog carried out, or nil. A nil watchdog (an unarmed encode) returns nil.
func (w *memoryWatchdog) stop() *MemoryAbortError {
	if w == nil {
		return nil
	}
	close(w.exited)
	<-w.done
	return w.abort
}

// vmRSS reads a process's resident memory, in bytes, from the VmRSS line of
// <procRoot>/<pid>/status - the figure the kernel reports for that process. ok is false
// when the file cannot be read, carries no VmRSS line (a process that has exited and not yet
// been reaped has none) or the line is not the "<n> kB" form.
func vmRSS(procRoot string, pid int) (int64, bool) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), "VmRSS:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// The startup record's messages. Each is a constant, so no value is interpolated into one
// and every figure travels as an attribute (observability O1).
const (
	memoryWatchArmedMsg    = "encode memory watchdog armed"
	memoryWatchUnarmedMsg  = "encode memory watchdog not armed: no cgroup memory limit is set"
	memoryWatchUnusableMsg = "the cgroup memory limit exists but could not be used, so encodes run unwatched"
)

// MemoryPlan is the memory bound of one run and the account of where it came from.
type MemoryPlan struct {
	// Bound is what every encode of the run is held to; the zero value where no limit was
	// established.
	Bound MemoryBound
	// Root is the cgroup mount that was read.
	Root string
	// Origin is the memory.max the limit came from, "" where none did.
	Origin string
	// NoInterface is set where no memory.max exists on the walk at all.
	NoInterface bool
	// Err is set ONLY where a memory.max exists and could not be read or did not parse.
	Err error
}

// DeriveMemoryWatch establishes the memory bound of one run from the cgroup hierarchy
// mounted at root ("" is the default mount), read once. A limit of "max", an absent
// interface and an unusable one all leave the bound unarmed: the watchdog protects
// liveness, not the no-loss invariant, and without it holdfast encodes exactly as before.
func DeriveMemoryWatch(root string) MemoryPlan {
	if root == "" {
		root = cpuquota.DefaultRoot
	}
	p := MemoryPlan{Root: root}
	lim, err := memlimit.Read(root)
	switch {
	case err == nil:
		p.Bound = MemoryBound{Limit: lim.Bytes, Threshold: lim.Threshold()}
		p.Origin = lim.Origin
	case errors.Is(err, memlimit.ErrNoInterface):
		p.NoInterface = true
	default:
		p.Err = err
	}
	return p
}

// Announce states the run's memory bound in exactly one record: info carrying the limit and
// the threshold where one was established, info saying none is set where the cgroup names
// none, and warn naming the interface, what was read there and that encodes run unwatched
// where it exists and could not be used (observability O3, O4). Never an error: the run
// continues, and no human has to act for it to.
func (p MemoryPlan) Announce(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "encode.memory")
	switch {
	case p.Err != nil:
		iface, read := p.Root, ""
		var ie *memlimit.InterfaceError
		if errors.As(p.Err, &ie) {
			iface, read = ie.Path, ie.Content
		}
		log.Warn(memoryWatchUnusableMsg,
			"dependency", "cgroup memory interface (memory.max)",
			"interface", iface,
			"read", read,
			"err", p.Err,
			"next_action", "run every encode unwatched, with no memory abort")
	case p.Bound.Armed():
		log.Info(memoryWatchArmedMsg,
			"memory_limit_bytes", p.Bound.Limit,
			"memory_threshold_bytes", p.Bound.Threshold,
			"threshold_percent", memlimit.Percent,
			"cgroup_origin", p.Origin,
			"cgroup_root", p.Root)
	default:
		why := "every memory.max on the cgroup walk is max"
		if p.NoInterface {
			why = "no cgroup v2 memory.max exists on the cgroup walk"
		}
		log.Info(memoryWatchUnarmedMsg,
			"why", why,
			"cgroup_root", p.Root,
			"next_action", "run every encode unwatched, with no memory abort")
	}
}

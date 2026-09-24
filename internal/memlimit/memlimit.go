// Package memlimit reads the memory limit of the cgroup THIS PROCESS runs in, and derives
// from it the resident-memory threshold at which an encode is aborted.
//
// It reads cgroup v2's memory.max over the same walk the CPU quota reader takes
// (cpuquota.Levels): the process's own cgroup, then each ancestor up to the mount root. A
// v2 child can never use more than its parent allows, so the effective limit is the
// smallest numeric memory.max anywhere on that walk, and "max" at a level means that level
// imposes none.
//
// It fails OPEN, deliberately and unlike the CPU reader's floor: the threshold protects
// liveness, not the no-loss invariant. A limit that cannot be established yields no
// threshold at all, and every encode runs exactly as it did before a watchdog existed.
// What it never does is invent a limit - not from the host's total memory, and not from
// cgroup v1, whose limit files this package does not read.
package memlimit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/cpuquota"
)

// Percent is the share of the effective limit an encode's resident memory may reach before
// it is aborted. It is fixed, with no configuration key: 7.1 GB of an 8 GB limit, the
// runaway this exists for, is 88.75%, and ordinary encodes sit at 34 to 40% of 8 GB.
const Percent = 85

// ErrNoInterface reports that no memory.max exists anywhere on the walk: a host with no
// cgroup v2 memory controller this process can see, a cgroup-v1-only host among them. That
// is an ordinary state, not a degraded one, and it is told apart from a file that exists and
// could not be used, which is an InterfaceError instead.
var ErrNoInterface = errors.New("no cgroup v2 memory limit interface (memory.max) exists")

// InterfaceError is a memory.max that EXISTS and could not be used: it could not be read, or
// what it holds is neither "max" nor a positive byte count. It carries the file and what was
// read there, so a caller stating the failure can name both.
type InterfaceError struct {
	// Path is the memory.max that was tried.
	Path string
	// Content is what the file held, trimmed, and "" where it could not be read at all.
	Content string
	err     error
}

func (e *InterfaceError) Error() string { return e.err.Error() }

func (e *InterfaceError) Unwrap() error { return e.err }

// Limit is the effective memory limit of this process's cgroup.
type Limit struct {
	// Bytes is the limit, and 0 where every memory.max on the walk says "max".
	Bytes int64
	// Origin is the memory.max the limit was read from, and "" where there is none.
	Origin string
}

// Set reports whether a numeric limit was found.
func (l Limit) Set() bool { return l.Bytes > 0 }

// Threshold is Percent of the limit, rounded down to a whole byte, and 0 where no limit is
// set. It is computed without multiplying the whole limit first, so no limit a kernel can
// report overflows it.
func (l Limit) Threshold() int64 {
	if !l.Set() {
		return 0
	}
	return l.Bytes/100*Percent + l.Bytes%100*Percent/100
}

// Read returns the effective memory limit under the cgroup hierarchy mounted at root ("" is
// cpuquota.DefaultRoot). It returns an error wrapping ErrNoInterface when no memory.max
// exists on the walk, and an *InterfaceError when one exists and cannot be used. A Limit
// that is not Set with a nil error is a cgroup that names no limit.
func Read(root string) (Limit, error) {
	return readLevels(cpuquota.Levels(root))
}

// readLevels reads memory.max at each directory of the walk and keeps the smallest numeric
// limit. A level with no memory.max has no memory controller delegated to it and imposes
// nothing; a level whose memory.max cannot be read or does not parse stops the read, because
// the limit it failed to state may be the one that binds this process.
func readLevels(levels []string) (Limit, error) {
	var lim Limit
	found := false
	for _, dir := range levels {
		p := filepath.Join(dir, "memory.max")
		b, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return Limit{}, &InterfaceError{Path: p, err: fmt.Errorf("%s exists and could not be read, "+
				"and it may be the limit that binds this process: %w", p, err)}
		}
		found = true
		s := strings.TrimSpace(string(b))
		if s == "max" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return Limit{}, &InterfaceError{Path: p, Content: s,
				err: fmt.Errorf("%s: %q is neither \"max\" nor a positive byte count", p, s)}
		}
		if !lim.Set() || n < lim.Bytes {
			lim = Limit{Bytes: n, Origin: p}
		}
	}
	if !found {
		return Limit{}, fmt.Errorf("%w on the walk from %s", ErrNoInterface, levels[0])
	}
	return lim, nil
}

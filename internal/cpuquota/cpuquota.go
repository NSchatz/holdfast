// Package cpuquota reads the CPU bandwidth THIS PROCESS is actually allowed, and divides
// it across the work that runs at once.
//
// The distinction it exists for: runtime.NumCPU() answers "how many CPUs may this process
// be scheduled on", and inside a bandwidth-limited container that is the HOST's answer,
// not the share the container was given. Sizing a thread pool from it asks the scheduler
// for CPU it will never hand over, and the kernel pays the difference in throttling and
// context switches rather than in work. So every consumer here sizes from the cgroup
// bandwidth limit, and reads the CPU count only where no limit exists at all.
//
// It fails SAFE in this repository's sense. A layout it does not recognise, a file it
// cannot read and a file whose contents do not parse all come back as an error naming
// what could not be read, and the caller continues at a stated floor instead of guessing.
// Guessing here has a direction: the guess available is the host CPU count, which is the
// oversubscription this package exists to stop, so it is never taken silently.
package cpuquota

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DefaultRoot is where a Linux host mounts the cgroup hierarchy. It is a parameter of
// Read rather than a constant reached inside it so a test can point the reader at a
// directory it built: the filesystem is outside this package's boundary, and faking it is
// how the unreadable, malformed and unlimited paths get exercised without a second host.
const DefaultRoot = "/sys/fs/cgroup"

// FallbackShare is the share a caller uses when Read returns an error: one CPU, which is
// what an unthreaded consumer already got. It is deliberately the SMALLEST defensible
// number rather than a derived one. A fallback taken from the host CPU count would be the
// confident wrong answer on exactly the hosts this package exists for, and a fallback of
// 0 is not a share at all - libvmaf reads a thread count of 0 as "use my own default".
const FallbackShare = 1

// The three ways a figure can be arrived at, recorded on the Quota so a log line can say
// WHERE the number came from. An operator reading "4 threads" cannot tell a quota of four
// CPUs from a four-CPU machine with no quota at all, and the two want different actions.
const (
	SourceCgroupV2 = "cgroup-v2"
	SourceCgroupV1 = "cgroup-v1"
	SourceAffinity = "affinity"
)

// ErrNoQuota reports that no CPU bandwidth limit could be read: no layout this package
// recognises was present, or the one that was present could not be parsed. It is a typed
// error because the caller's response is prescribed - state the fallback, continue - and
// a caller that cannot tell this from a real figure would be free to invent one.
var ErrNoQuota = errors.New("the effective CPU quota could not be read")

// ErrNoInterface reports, beside ErrNoQuota, that NEITHER layout's bandwidth file exists
// under the root at all: no cpu.max anywhere on the v2 walk and no cpu.cfs_quota_us where
// v1 puts it. That is a host with no CPU controller this build can see, which is an
// ordinary state rather than a degraded one, and a caller may answer it differently from
// a file that is there and could not be used - which is an InterfaceError instead.
var ErrNoInterface = errors.New("no cgroup CPU bandwidth interface exists")

// InterfaceError is a bandwidth file that EXISTS and could not be used: it could not be
// read, or what it holds does not parse. It carries the file and what was read there, so
// a caller stating the failure can name both rather than re-deriving them from a message.
type InterfaceError struct {
	// Path is the interface file that was tried.
	Path string
	// Content is what the file held, trimmed, and "" where it could not be read at all.
	Content string
	err     error
}

func (e *InterfaceError) Error() string { return e.err.Error() }

func (e *InterfaceError) Unwrap() error { return e.err }

// errAbsent marks a layout whose bandwidth file does not exist at all. It stays inside
// this package: Read turns "absent under both layouts" into ErrNoInterface, and absent
// under ONE layout says nothing on its own.
var errAbsent = errors.New("interface file absent")

// Quota is the CPU bandwidth this process may use, in whole-and-fractional CPUs.
type Quota struct {
	// CPUs is the effective bandwidth. Where Limited is false it carries the number of
	// CPUs the process may actually be scheduled on instead, so one formula sizes both
	// cases and no caller has to branch on Limited to get a usable number.
	CPUs float64
	// Limited says whether a bandwidth limit was found. False means the cgroup said
	// "max": there is no ceiling, and CPUs is the schedulable CPU count.
	Limited bool
	// Source names which of the three readings produced CPUs.
	Source string
	// Origin is the file the figure was read from, and "" where no file was involved.
	// It is what an operator greps for when the derived number surprises them.
	Origin string
}

// selfCgroup is the file naming the cgroup this process is IN, which under cgroup v2 is
// not necessarily the root of the mount. A var so a test can point it at a fixture; the
// process's own cgroup path is filesystem state, outside this package's boundary.
var selfCgroup = "/proc/self/cgroup"

// Read returns the effective CPU quota under the cgroup hierarchy mounted at root, trying
// cgroup v2 first and cgroup v1 second, or an error wrapping ErrNoQuota when neither
// layout can be read. That error also wraps ErrNoInterface when neither layout's file
// exists at all, and wraps the InterfaceError of each file that exists and could not be
// used, so a caller can name the file and what it held.
//
// Which layout a host exposes is the host's choice and not something this process can
// influence, so both are read and neither is assumed. Anything else - a hybrid mount
// whose files are not where either layout puts them, a cgroup namespace this process
// cannot see into, a kernel with the cpu controller disabled - takes the error path, and
// the caller states its fallback rather than deriving a number from a layout it did not
// understand.
func Read(root string) (Quota, error) {
	if root == "" {
		root = DefaultRoot
	}
	q, v2 := readV2(root)
	if v2 == nil {
		return q, nil
	}
	q, v1 := readV1(root)
	if v1 == nil {
		return q, nil
	}
	if errors.Is(v2, errAbsent) && errors.Is(v1, errAbsent) {
		return Quota{}, fmt.Errorf("%w under %s (%w): cgroup v2: %w; cgroup v1: %w",
			ErrNoQuota, root, ErrNoInterface, v2, v1)
	}
	return Quota{}, fmt.Errorf("%w under %s: cgroup v2: %w; cgroup v1: %w", ErrNoQuota, root, v2, v1)
}

// Divide splits a quota across the jobs that may run at once and returns the whole-CPU
// share ONE of them may ask for.
//
// It floors rather than rounds, so the shares of every concurrent job add up to no more
// than the quota, and it never returns 0: a share of 0 is not a smaller request, it is
// the absence of one, and the consumers here read 0 as "decide for me" - which is the
// host-sized default this package exists to avoid.
//
// The floor of 1 is the one case where the shares can exceed the quota, and it is
// unavoidable: with more concurrent jobs than CPUs of quota there is no positive integer
// share that fits. One thread each is then both the smallest request that can be made and
// what an unthreaded build already asked for, so the floor never makes a configuration
// worse than it was.
func Divide(q Quota, concurrency int) int {
	if concurrency < 1 {
		concurrency = 1
	}
	n := int(math.Floor(q.CPUs / float64(concurrency)))
	if n < 1 {
		return 1
	}
	return n
}

// unlimited is the answer for a cgroup that names no ceiling: the CPUs this process may
// actually be scheduled on. runtime.NumCPU() is that number on Linux rather than the
// machine's total, because the Go runtime reads the affinity mask at startup - so a
// process pinned to 4 of 64 CPUs sizes to 4 here, which is the honest figure and the one
// the criterion asks for.
func unlimited(source, origin string) Quota {
	return Quota{CPUs: float64(runtime.NumCPU()), Limited: false, Source: source, Origin: origin}
}

// readV2 walks from the cgroup this process is in up to the mount root, reading every
// cpu.max on the way and keeping the most restrictive limit it finds.
//
// The walk is the correctness part. A v2 child cannot exceed its parent's bandwidth, so
// the effective limit is the minimum along the path, and a process in a nested cgroup
// that read only its own file would miss a ceiling its parent imposes. A container with
// its own cgroup namespace sees its limit at the root and the walk ends immediately,
// which is the common case and costs one read.
//
// A cpu.max that is PRESENT and does not parse is an error, not a skipped file: that is
// a layout this package does not understand, and continuing past it would report a
// ceiling taken from some other level as if it were the whole answer.
//
// So is a cpu.max that is present and cannot be READ. Only a level where the file does
// not exist is passed over - that is a level with no cpu controller delegated to it, and
// it imposes nothing. A file that exists and refuses to be read (a permission, a
// directory in its place, an I/O error) may be the very limit that binds this process,
// and a minimum taken over the levels that happened to be readable is a figure that was
// never read: on a host whose tightest ceiling is the unreadable one it is the host CPU
// count dressed as a reading.
func readV2(root string) (Quota, error) {
	dir := filepath.Clean(filepath.Join(root, selfCgroupPath()))
	if !within(root, dir) {
		dir = root
	}
	limit := math.Inf(1)
	var origin, unlimitedOrigin string
	read := 0
	for {
		p := filepath.Join(dir, "cpu.max")
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			read++
			cpus, limited, perr := parseCPUMax(string(b))
			if perr != nil {
				return Quota{}, &InterfaceError{Path: p, Content: strings.TrimSpace(string(b)),
					err: fmt.Errorf("%s: %w", p, perr)}
			}
			switch {
			case limited && cpus < limit:
				limit, origin = cpus, p
			case !limited && unlimitedOrigin == "":
				unlimitedOrigin = p
			}
		case errors.Is(err, fs.ErrNotExist):
			// No cpu controller at this level: nothing here bounds the process.
		default:
			return Quota{}, &InterfaceError{Path: p, err: fmt.Errorf("%s exists and could not be read, "+
				"and it may be the limit that binds this process: %w", p, err)}
		}
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if read == 0 {
		return Quota{}, fmt.Errorf("no readable cpu.max between %s and %s: %w",
			filepath.Join(root, selfCgroupPath()), root, errAbsent)
	}
	if math.IsInf(limit, 1) {
		return unlimited(SourceAffinity, unlimitedOrigin), nil
	}
	return Quota{CPUs: limit, Limited: true, Source: SourceCgroupV2, Origin: origin}, nil
}

// readV1 reads the cpu controller's bandwidth pair, at the controller's own directory
// under the mount or at the mount itself, which is where a container that mounts the
// single controller puts it. v1 is not walked: a v1 container sees only its own leaf
// through the mount it was given, so there is no parent to weigh, and inventing a walk
// over a layout this build cannot observe would be a guess dressed as a reading.
func readV1(root string) (Quota, error) {
	dir := filepath.Join(root, "cpu")
	if _, err := os.Stat(filepath.Join(dir, "cpu.cfs_quota_us")); err != nil {
		dir = root
	}
	quotaPath := filepath.Join(dir, "cpu.cfs_quota_us")
	periodPath := filepath.Join(dir, "cpu.cfs_period_us")
	quota, err := readInt(quotaPath)
	// Only the QUOTA file's absence is an absent layout. A quota with no period beside it
	// is a layout that exists and cannot be used, and readInt already says so.
	if errors.Is(err, fs.ErrNotExist) {
		return Quota{}, fmt.Errorf("%s: %w", quotaPath, errAbsent)
	}
	if err != nil {
		return Quota{}, err
	}
	// -1 is how v1 spells "no ceiling", the same statement v2 makes with "max".
	if quota < 0 {
		return unlimited(SourceAffinity, quotaPath), nil
	}
	period, err := readInt(periodPath)
	if err != nil {
		return Quota{}, err
	}
	if quota == 0 || period <= 0 {
		return Quota{}, &InterfaceError{Path: quotaPath,
			Content: fmt.Sprintf("quota %d period %d", quota, period),
			err: fmt.Errorf("%s/%s: quota %d period %d is not a bandwidth this build can read",
				quotaPath, filepath.Base(periodPath), quota, period)}
	}
	return Quota{CPUs: float64(quota) / float64(period), Limited: true, Source: SourceCgroupV1, Origin: quotaPath}, nil
}

// readInt reads one v1 integer file. Every failure is an InterfaceError naming the file,
// with what it held where anything could be read; a file that does not exist is one too,
// and still answers errors.Is(err, fs.ErrNotExist) for the caller that has to tell an
// absent layout from a broken one.
func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, &InterfaceError{Path: path, err: err}
	}
	s := strings.TrimSpace(string(b))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, &InterfaceError{Path: path, Content: s, err: fmt.Errorf("%s: %q is not an integer", path, s)}
	}
	return n, nil
}

// parseCPUMax reads the two-value cpu.max format, "$MAX $PERIOD", where $MAX is either a
// microsecond budget or the literal "max" for no limit.
func parseCPUMax(s string) (cpus float64, limited bool, err error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, false, fmt.Errorf("%q is not the two-value cpu.max format", strings.TrimSpace(s))
	}
	period, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, false, fmt.Errorf("period %q is not a positive integer", f[1])
	}
	if f[0] == "max" {
		return 0, false, nil
	}
	quota, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0, false, fmt.Errorf("quota %q is neither \"max\" nor a positive integer", f[0])
	}
	return float64(quota) / float64(period), true, nil
}

// selfCgroupPath returns the v2 path of the cgroup this process is in, and "/" when that
// cannot be read - which is the same answer as "this process is at the root" and leaves
// the walk reading the mount itself.
func selfCgroupPath() string {
	b, err := os.ReadFile(selfCgroup)
	if err != nil {
		return "/"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok && rest != "" {
			return rest
		}
	}
	return "/"
}

// within reports whether path is root or lies beneath it, so a cgroup path that escapes
// the mount (a "..", an absolute path from a namespace this process cannot see) cannot
// send the walk outside the directory it was handed.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

package engine

import (
	"errors"
	"log/slog"

	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/encoder"
)

// CgroupRootEnv names the environment variable that points the libx265 parallelism
// reading at a cgroup mount other than cpuquota.DefaultRoot.
//
// It is an environment variable rather than a configuration key because it is not
// something an operator configures: the cgroup mount is where the kernel put it. It exists
// so the reading can be steered from OUTSIDE the process - a test that runs the real
// program in a child process has no other way to hand it a fixture hierarchy - and it is
// read in the same place, and the same way, as HOLDFAST_FFMPEG and HOLDFAST_FFPROBE.
const CgroupRootEnv = "HOLDFAST_CGROUP_ROOT"

// The three places a run's libx265 parallelism can come from, as the startup record
// names them. An operator reading "pools=8" cannot tell a configured 8 from a quota of
// eight CPUs, and the two want different actions when the figure is wrong.
const (
	// X265FromConfiguration is the x265_cpus key: the operator named the figure.
	X265FromConfiguration = "configuration"
	// X265FromCgroup is the process's own cgroup CPU bandwidth limit.
	X265FromCgroup = "cgroup"
	// X265NoQuota is neither: no limit was found, and libx265 keeps its own defaults.
	X265NoQuota = "no-quota-found"
)

// X265Plan is the libx265 parallelism of one run and the account of where it came from.
type X265Plan struct {
	// Parallelism is what every libx265 encode of the run is told to use, and the zero
	// value - nothing passed, libx265's own defaults - where no figure was derived.
	Parallelism encoder.X265Parallelism
	// Source is one of X265FromConfiguration, X265FromCgroup and X265NoQuota.
	Source string
	// Root is the cgroup mount that was read, and "" where a configured figure meant
	// nothing was.
	Root string
	// Quota is what the cgroup reported, and the zero value where nothing was read.
	Quota cpuquota.Quota
	// Err is set ONLY where a cgroup bandwidth interface exists and could not be read or
	// did not parse. An absent interface and a cgroup that names no limit are both
	// ordinary states, and leave it nil.
	Err error
}

// DeriveX265 decides the libx265 parallelism of one run: the configured whole-CPU
// figure where the x265_cpus key names one, otherwise the CPU bandwidth limit of this
// process's own cgroup under root, read once.
//
// The figure is the quota divided by the period, rounded down and floored at 1. It is
// never the machine's logical CPU count: inside a bandwidth-limited container that is
// the HOST's answer, and sizing libx265 from it is what ran 113 threads against a quota
// of 24. So where no limit can be read the answer is no figure at all rather than that
// one - libx265 keeps the defaults it has always had, and nothing is guessed.
//
// It takes the figure for ONE encode and divides it by nothing. How a quota is shared
// between concurrent workers is the workers key's question, and this does not read it.
func DeriveX265(configured int, root string) X265Plan {
	if configured > 0 {
		return X265Plan{Parallelism: encoder.X265ParallelismFor(configured), Source: X265FromConfiguration}
	}
	if root == "" {
		root = cpuquota.DefaultRoot
	}
	p := X265Plan{Source: X265NoQuota, Root: root}
	q, err := cpuquota.Read(root)
	switch {
	case err == nil && q.Limited:
		p.Quota, p.Source = q, X265FromCgroup
		p.Parallelism = encoder.X265ParallelismFor(cpuquota.Divide(q, 1))
	case err == nil:
		// "max", or v1's -1: the cgroup names no ceiling.
		p.Quota = q
	case errors.Is(err, cpuquota.ErrNoInterface):
		// No bandwidth interface exists to be read: nothing is degraded.
	default:
		p.Err = err
	}
	return p
}

// Announce states the run's libx265 parallelism: exactly one info record carrying the
// figures and their source as attributes, preceded - only where a bandwidth interface
// exists and could not be used - by one warn naming the interface, what was read there
// and that the run continues without a derived figure (observability O3, O4). It is never
// an error: the run continues, and no human has to act for it to.
func (p X265Plan) Announce(log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "encoder.parallelism")
	if p.Err != nil {
		iface, read := p.Root, ""
		var ie *cpuquota.InterfaceError
		if errors.As(p.Err, &ie) {
			iface, read = ie.Path, ie.Content
		}
		log.Warn("the cgroup CPU bandwidth limit exists but could not be used, so libx265 runs at its "+
			"own default parallelism: no worker-pool size or frame-thread count is passed, and none is "+
			"guessed from the host CPU count",
			"dependency", "cgroup cpu bandwidth limit",
			"interface", iface,
			"read", read,
			"err", p.Err,
			"next_action", "continue without a derived figure")
	}
	attrs := []any{"x265_parallelism_source", p.Source}
	if p.Parallelism.Set() {
		attrs = append(attrs,
			"x265_cpus", p.Parallelism.CPUs,
			"x265_pools", p.Parallelism.Pools,
			"x265_frame_threads", p.Parallelism.FrameThreads)
	} else {
		attrs = append(attrs, "x265_cpus", "auto", "x265_pools", "auto", "x265_frame_threads", "auto")
	}
	if p.Source == X265FromCgroup {
		attrs = append(attrs, "cgroup_cpus", p.Quota.CPUs, "cgroup_origin", p.Quota.Origin)
	}
	if p.Root != "" {
		attrs = append(attrs, "cgroup_root", p.Root)
	}
	log.Info("libx265 parallelism for this run", attrs...)
}

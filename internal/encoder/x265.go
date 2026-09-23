package encoder

import "strconv"

// X265MaxFrameThreads is the largest frame-thread count libx265 accepts. Its CLI
// documents --frame-threads as "any value between 0 and 16", 0 being its own
// auto-detect, so nothing this package derives may leave that range.
const X265MaxFrameThreads = 16

// X265Parallelism is the parallelism ONE libx265 encode is told to use: a worker-pool
// size and a frame-thread count, and the whole-CPU figure both were derived from.
//
// The zero value is "tell libx265 nothing", which leaves the encoder's own defaults in
// force - one pool with a thread per logical CPU of the HOST, and an auto-detected frame
// count. That is what every encode did before a figure existed, so an encoder built
// without one emits exactly the argument list it always emitted.
type X265Parallelism struct {
	// CPUs is the whole-CPU budget the encode was sized for, and 0 where none was.
	CPUs int
	// Pools is the thread count of the single worker pool, passed as x265's `pools`.
	Pools int
	// FrameThreads is the number of concurrently encoded frames, passed as x265's
	// `frame-threads`. Always within 1..X265MaxFrameThreads when CPUs is set.
	FrameThreads int
}

// X265ParallelismFor derives the parallelism of one libx265 encode from a whole-CPU
// budget. A budget below 1 derives nothing and returns the zero value.
//
// The pool takes the whole budget: it is the WPP and motion-search worker set, and a
// budget of N CPUs asks for N workers rather than one per host CPU. The frame-thread
// count is NOT the same number. x265 states that over-allocating frame threads "will not
// improve performance, it will generally just increase memory use", and that fewer of
// them compress slightly better because more of each reference frame is complete when it
// is searched. So the count follows the table libx265 applies to a CPU count of its own
// when it auto-detects frame threads for a WPP encode - 1 below 4 CPUs, 2 from 4, 3 from
// 8, 4 from 16 and 5 from 32 - which makes an encode under a budget of N the encode
// libx265 would have chosen on an N-CPU machine rather than one sized for the host.
func X265ParallelismFor(cpus int) X265Parallelism {
	if cpus < 1 {
		return X265Parallelism{}
	}
	return X265Parallelism{CPUs: cpus, Pools: cpus, FrameThreads: x265FrameThreads(cpus)}
}

func x265FrameThreads(cpus int) int {
	switch {
	case cpus >= 32:
		return 5
	case cpus >= 16:
		return 4
	case cpus >= 8:
		return 3
	case cpus >= 4:
		return 2
	default:
		return 1
	}
}

// Set reports whether a budget was derived, which is whether Params says anything.
func (p X265Parallelism) Set() bool { return p.CPUs > 0 }

// Params is the fragment this parallelism adds to libx265's colon-separated
// `-x265-params` string, colon-led so it appends to the fragment before it, and "" for
// the zero value so an unsized encode's string is byte for byte what it was.
func (p X265Parallelism) Params() string {
	if !p.Set() {
		return ""
	}
	return ":pools=" + strconv.Itoa(p.Pools) + ":frame-threads=" + strconv.Itoa(p.FrameThreads)
}

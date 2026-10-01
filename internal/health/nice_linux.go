//go:build linux

package health

import "syscall"

// decodeNice is the scheduling priority a sweep decode runs at: the lowest, so a decode
// yields the CPU to an encode competing for it. It is set on the decode's process right
// after it starts, and the threads ffmpeg starts after that inherit it; a failure to set
// it is not a reason to skip the decode. That an encode loses less to a niced decode is
// the documented behaviour of nice(2) (https://man7.org/linux/man-pages/man2/nice.2.html,
// read 2026-10-01); how much less on a given host is not measured here (ASSUMED).
const decodeNice = 19

func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, decodeNice)
}

//go:build linux

package diskfree

import (
	"errors"
	"syscall"
)

// errNoBlockSize is the refusal for a filesystem that answered statfs(2) without a
// usable block size. There is no arithmetic to do from that, and returning 0 would
// read to every caller as "the device is full" while returning a large number would
// read as "there is plenty" - both of them inventions.
var errNoBlockSize = errors.New("the filesystem reported no usable block size, so its free space cannot be established")

// bytes reads statfs(2) and reports f_bavail * f_bsize: the blocks available to an
// UNPRIVILEGED process, deliberately, rather than f_bfree, which includes the
// blocks a filesystem reserves for root and which holdfast cannot write into.
func bytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, errNoBlockSize
	}
	return st.Bavail * uint64(st.Bsize), nil
}

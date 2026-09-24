//go:build linux

package diskfree

import (
	"errors"
	"fmt"
	"strconv"
	"syscall"
)

// errNoBlockSize is the refusal for a filesystem that answered statfs(2) without a
// usable block size. There is no arithmetic to do from that, and returning 0 would
// read to every caller as "the device is full" while returning a large number would
// read as "there is plenty" - both of them inventions.
var errNoBlockSize = errors.New("the filesystem reported no usable block size, so its free space cannot be established")

// bytes reads statfs(2) and hands the answer to fromStatfs. The syscall and the
// arithmetic are separated so the arithmetic - which is the part with a decision in
// it - can be graded against filesystem shapes no test machine can be made to have.
func bytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return fromStatfs(st)
}

// fromStatfs reports f_bavail * f_bsize: the blocks available to an UNPRIVILEGED
// process, deliberately, rather than f_bfree, which includes the blocks a filesystem
// reserves for root and which holdfast cannot write into. On a default ext4 that
// reserve is 5% of the device, which is a whole 4K source on a small array and is
// exactly the margin a check like this is asked about.
func fromStatfs(st syscall.Statfs_t) (uint64, error) {
	if st.Bsize <= 0 {
		return 0, errNoBlockSize
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// id prefers f_fsid, which Linux derives from the BACKING device when a filesystem
// publishes none of its own, so one filesystem mounted twice answers once. Where f_fsid
// is absent (zero, or statfs failed) it falls back to st_dev, which still names every
// directory of one mount alike. The key has the same shape the startup walk gives a
// region's filesystem half.
func id(path string) (string, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err == nil && (fs.Fsid.X__val[0] != 0 || fs.Fsid.X__val[1] != 0) {
		return fmt.Sprintf("fsid:%d:%d", fs.Fsid.X__val[0], fs.Fsid.X__val[1]), nil
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", err
	}
	return "dev:" + strconv.FormatUint(uint64(st.Dev), 10), nil
}

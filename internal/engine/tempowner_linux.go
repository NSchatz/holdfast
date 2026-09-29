//go:build linux

package engine

import (
	"errors"
	"os"
	"syscall"
)

// ownerLocksSupported: this platform takes open-file-description locks, so owner records
// are written and read.
const ownerLocksSupported = true

// fOFDSetlk is F_OFD_SETLK from the kernel's asm-generic fcntl.h (Linux 3.15 and later),
// which every architecture shares. The syscall package does not name it.
const fOFDSetlk = 37

// lockRecord takes an exclusive open-file-description lock on the whole of f without
// waiting. errRecordBusy is a lock another open file description holds - another process's,
// or this one's through a separate open, which is what makes a sweep see this process's
// own in-flight temps as alive. Any other error is returned as it is, and a caller reads it
// as "cannot decide".
func lockRecord(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	// Whence, Start and Len zero: the whole file. Pid must be zero for an OFD lock.
	lk := syscall.Flock_t{Type: syscall.F_WRLCK}
	var lerr error
	if err := rc.Control(func(fd uintptr) { lerr = syscall.FcntlFlock(fd, fOFDSetlk, &lk) }); err != nil {
		return err
	}
	if errors.Is(lerr, syscall.EAGAIN) || errors.Is(lerr, syscall.EACCES) {
		return errRecordBusy
	}
	return lerr
}

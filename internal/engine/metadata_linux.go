//go:build linux

package engine

import (
	"io/fs"
	"syscall"
)

// ownerOf reads a file's uid and gid from the platform's own stat result.
//
// The ok return is not decoration. fs.FileInfo.Sys() is documented as returning the
// underlying data source, which MAY be nil and is whatever the filesystem implementation
// chose - an os.Stat on Linux gives a *syscall.Stat_t, but an fs.FS in front of something
// else need not, and a build that assumed the type would panic where it should carry no
// ownership. A false ok means "this build cannot read an ownership here", which the caller
// treats as nothing to carry rather than as a failure.
func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

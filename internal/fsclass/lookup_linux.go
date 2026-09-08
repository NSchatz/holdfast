//go:build linux

package fsclass

import "syscall"

// magicNames maps a Linux superblock magic to the type NAME this build reasons about.
// The magics are the ones the kernel documents in <linux/magic.h>; they are stable
// (a filesystem's magic is part of its on-disk/wire format and cannot be renumbered
// without breaking every existing volume), which is why keying on them is safe where
// parsing /proc/mounts would not be - a bind mount, a container's mount namespace and
// an autofs trigger all lie about the mount table, and none of them can lie about
// statfs of the actual path.
//
// A magic that is not here is NOT an error: it is reported as unknown(0x...) and
// classifies Undetermined, hence not local.
// The spellings are deliberately the same ones internal/startup's own mount-table
// lookup produces, so the two lookups in the program name the same storage the same
// way and the ONE enumeration in fsclass.go classifies both identically.
var magicNames = map[int64]string{
	// --- the local side, per the repository's own durability limitation ---
	0xEF53:     "ext4",  // shared by ext2/ext3/ext4; all three are host-attached
	0x58465342: "xfs",   //
	0x9123683E: "btrfs", //
	0xF2F52010: "f2fs",  //
	0x3153464A: "jfs",   //
	0x2FC12FC1: "zfs",   // ZFS on Linux

	// --- the network side ---
	0x6969:     "nfs",   // NFSv3 and NFSv4 share this magic
	0xFF534D42: "cifs",  // the original CIFS client
	0xFE534D42: "smb2",  // the SMB2/SMB3 client
	0x517B:     "smbfs", // the historic smbfs client
	0x01021997: "9p",    //
	0x00C36400: "ceph",  //

	// --- named, but in NEITHER enumeration, so Undetermined and thus not local ---
	0x794C7630: "overlay", // a container overlay: its layers may themselves be remote
	0x01021994: "tmpfs",   // RAM-backed: host-attached but not durable storage at all
	0x858458F6: "ramfs",   //
	0x9FA0:     "proc",    //
	0x62656572: "sysfs",   //
	0x73717368: "squashfs",
	0x65735546: "fuse", // anything at all can be behind a FUSE mount, including a network
}

// LookupType returns the filesystem type name of the storage path is on, via statfs(2).
// statfs is asked about the PATH, not about a mount table, so a bind mount or a
// container mount namespace cannot mislead it.
//
// An error is returned verbatim (the caller classifies it Undetermined and reports it):
// ENOENT for a path that is gone, EACCES for a directory component the process may not
// traverse, and EIO for storage that has dropped out are all "we do not know", which is
// exactly the fail-safe answer.
func LookupType(path string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	// The low 32 bits, deliberately. Statfs_t.Type is int64 on amd64/arm64 and int32 on
	// the 32-bit arches, and every magic the kernel documents fits in 32 bits - so
	// masking is what stops a 32-bit build sign-extending 0xFF534D42 into a negative
	// number that matches no entry and silently classifies a CIFS mount Undetermined.
	magic := int64(uint32(st.Type))
	if name, ok := magicNames[magic]; ok {
		return name, nil
	}
	return unknownType(magic), nil
}

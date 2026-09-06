//go:build linux

package startup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// System returns a Platform that reads the real host.
//
// Both substitutable seams are parameters rather than globals, because the gate
// this repo is tested on has neither a network mount nor a second real
// filesystem: typeLookup replaces the filesystem-type lookup and mountLookup
// replaces the mount-point test. Production passes nil for both.
func System(typeLookup func(string) (string, error), mountLookup func(string) (bool, error)) Platform {
	return &system{typeLookup: typeLookup, mountLookup: mountLookup}
}

// system reads the real host. One instance serves ONE start: the mount table it
// reads and the filesystem identities it derives are cached for the duration of
// that startup and never across starts, so every start classifies afresh.
type system struct {
	typeLookup  func(string) (string, error)
	mountLookup func(string) (bool, error)

	once   sync.Once
	mounts *mountTable
	mErr   error

	mu    sync.Mutex
	fsIDs map[uint64]string // st_dev -> the filesystem identity used in a Region
}

func (s *system) Inspect(path string) (Info, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, fmt.Errorf("stat %s: the platform returned no storage identity", path)
	}
	return Info{
		IsDir:  fi.IsDir(),
		DevID:  "dev:" + strconv.FormatUint(uint64(st.Dev), 10),
		Region: Region{FS: s.filesystemID(path, uint64(st.Dev)), Node: strconv.FormatUint(st.Ino, 10)},
	}, nil
}

// filesystemID derives the FILESYSTEM half of a Region key, and it is where the
// "one filesystem mounted twice" hazard is answered.
//
// The obvious key is st_dev, and it is wrong on its own: two mounts of one
// filesystem need not share a superblock, st_dev then differs, the walk would
// treat the two mount points as two regions, walk both, and enumerate one source
// twice - which on the default in-place path queues a second swap against a
// source the first already replaced. So the key prefers f_fsid, which Linux
// derives from the BACKING DEVICE (bd_dev) rather than from the per-mount
// anonymous device number when a filesystem does not publish one of its own:
// mount /dev/sdb1 twice, with or without a shared superblock, and both mounts
// answer with the same f_fsid, so both mount points collapse to one region.
//
// Residual, stated rather than hidden: a filesystem with NO backing block device
// (NFS, CIFS, FUSE) mounted twice with separate superblocks can still answer
// with two identities, and those two mount points are then two regions. That
// case cannot start unnoticed - such storage classifies `non-local` or
// `undetermined`, so the run is refused unless the operator opts in for EACH
// mount point by name, and both mount points carry their own record naming both
// spellings.
//
// The fallback is st_dev, and the direction of the fallback is the fail-safe
// one: two regions wrongly collapsed cost coverage and are REPORTED (the walk
// says which path it did not descend and why), while two regions wrongly split
// cost a second swap.
func (s *system) filesystemID(path string, dev uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fsIDs == nil {
		s.fsIDs = make(map[uint64]string)
	}
	if id, ok := s.fsIDs[dev]; ok {
		return id
	}
	var st syscall.Statfs_t
	fsid0, fsid1, ok := int32(0), int32(0), false
	if err := syscall.Statfs(path, &st); err == nil {
		fsid0, fsid1, ok = st.Fsid.X__val[0], st.Fsid.X__val[1], true
	}
	id := regionFSKey(fsid0, fsid1, ok, dev)
	s.fsIDs[dev] = id
	return id
}

// regionFSKey is the choice itself, kept pure so it can be tested: a published
// filesystem id wins over the per-mount device number, and only an absent one
// falls back to it.
func regionFSKey(fsid0, fsid1 int32, haveFsid bool, dev uint64) string {
	if haveFsid && (fsid0 != 0 || fsid1 != 0) {
		return fmt.Sprintf("fsid:%d:%d", fsid0, fsid1)
	}
	return "dev:" + strconv.FormatUint(dev, 10)
}

func (s *system) ReadDir(path string) ([]Entry, error) {
	// os.ReadDir returns the entries it read BEFORE a mid-listing failure
	// together with the error, which is exactly the partial listing the walk has
	// to be able to see and refuse to trust.
	ents, err := os.ReadDir(path)
	out := make([]Entry, 0, len(ents))
	for _, e := range ents {
		out = append(out, Entry{
			Name:   e.Name(),
			IsDir:  e.IsDir(),
			IsLink: e.Type()&os.ModeSymlink != 0,
		})
	}
	return out, err
}

func (s *system) Resolve(path string) (string, error) { return filepath.EvalSymlinks(path) }

// FSType names the filesystem type at path.
//
// The statfs magic number is the primary source because it needs no mount table
// and cannot be out of date. It is not sufficient: several types share one magic
// or have none this build knows, and only the mount table spells a FUSE type's
// real name. So the table is consulted first for the NAME when it is readable,
// the magic answers when it is not, and a path whose type ONLY the table could
// have settled reports ErrMountInfoUnavailable when the table cannot be read -
// which classifies it `undetermined` and refuses the run unless opted in, rather
// than guessing.
//
// THE TABLE IS KEYED BY THE PATH THE KERNEL WOULD ACT ON, never by the path's
// spelling. /proc/self/mountinfo lists no symbolic link - the kernel resolves a
// path before it mounts anything - so a LEXICAL lookup of a path reached through
// a link matches no mount point of its own but its textual ancestors', and
// answers with a filesystem the path is not on. The direction of that error is
// the fail-open one this whole package exists to close: a library root or state
// directory symlinked onto a NAS share would report `local`, name the local
// filesystem's type, demand no opt-in and start. Every other operation here
// follows the link - os.Stat, syscall.Statfs, and every byte holdfast later
// writes and renames through that path - so this must too. What is classified is
// the STORAGE (Definitions/Classification); the spelling the report and any
// declaration use is a separate field the walk carries (Record.Path, AC2b).
func (s *system) FSType(path string) (string, error) {
	if s.typeLookup != nil {
		return s.typeLookup(path)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "", err
	}
	table, tErr := s.mountInfo()
	resolved, rErr := s.Resolve(path)
	if tErr == nil && rErr == nil {
		if name := table.typeFor(resolved); name != "" {
			return name, nil
		}
	}
	// The magic number came from a statfs that already followed the link, so it
	// describes the real storage and is safe to fall back to when the table
	// cannot be read OR the path cannot be resolved.
	if name, ok := magicNames[int64(st.Type)]; ok {
		return name, nil
	}
	if tErr != nil {
		return "", ErrMountInfoUnavailable
	}
	if rErr != nil {
		return "", rErr
	}
	return fmt.Sprintf("unknown(0x%x)", uint64(st.Type)), nil
}

// MountPoint reports whether path is itself the root of a mounted filesystem.
// The mount table is the only thing that can see a bind mount (which shares its
// device number with the filesystem it exposes), so this is where the walk
// consults it - and where its absence is reported rather than silently answered
// "no".
//
// It is keyed by the RESOLVED path for the same reason FSType is, and the stake
// is AC2d1's record: a bind mount reached through a symlinked root would match
// no entry of a table that lists resolved kernel paths, so the walk would not
// see a mount point there at all, emit no classification record for it, and
// demand no opt-in for storage the operator was never told about.
func (s *system) MountPoint(path string) (bool, error) {
	if s.mountLookup != nil {
		return s.mountLookup(path)
	}
	table, err := s.mountInfo()
	if err != nil {
		return false, ErrMountInfoUnavailable
	}
	resolved, rErr := s.Resolve(path)
	if rErr != nil {
		return false, rErr
	}
	return table.isMountPoint(resolved), nil
}

func (s *system) mountInfo() (*mountTable, error) {
	s.once.Do(func() { s.mounts, s.mErr = readMountInfo(mountInfoPath) })
	return s.mounts, s.mErr
}

// mountInfoPath is the kernel's per-process mount table. It is a variable so the
// unparseable-and-absent paths have a test.
var mountInfoPath = "/proc/self/mountinfo"

// mountTable is the parsed mount table: each mount point with the filesystem
// type mounted there, in the file's own order (a later entry over the same point
// shadows an earlier one).
type mountTable struct {
	points []mountEntry
	set    map[string]bool
}

type mountEntry struct {
	point  string
	fsType string
}

// readMountInfo parses /proc/self/mountinfo. Its fields are, in order: mount id,
// parent id, major:minor, root, MOUNT POINT, mount options, zero or more
// optional fields, a "-" separator, FILESYSTEM TYPE, source, super options. An
// unparseable file is an error rather than a short list, because a half-read
// mount table would silently answer "not a mount point" for everything after the
// damage.
func readMountInfo(path string) (*mountTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	t := &mountTable{set: map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || sep+1 >= len(fields) {
			return nil, fmt.Errorf("mount table %s: unparseable line %q", path, line)
		}
		point := unescapeMountField(fields[4])
		t.points = append(t.points, mountEntry{point: point, fsType: fields[sep+1]})
		t.set[point] = true
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

// unescapeMountField undoes the octal escaping the kernel applies to the space,
// tab, newline and backslash characters in a mount point.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (t *mountTable) isMountPoint(path string) bool { return t.set[cleanPath(path)] }

// typeFor returns the filesystem type mounted at the longest mount point that
// contains path, the last such entry winning (a mount over a mount shadows it).
func (t *mountTable) typeFor(path string) string {
	p := cleanPath(path)
	best, bestLen := "", -1
	for _, e := range t.points {
		if e.point == p || lexicallyBeneath(p, e.point) {
			if len(e.point) >= bestLen {
				best, bestLen = e.fsType, len(e.point)
			}
		}
	}
	return best
}

// magicNames maps the statfs magic numbers this build knows to type names. It is
// the answer when the mount table is unreadable, and the reason an ordinary
// local host still starts without one.
//
// ext2, ext3 and ext4 share magic 0xEF53: the mount table distinguishes them and
// is consulted first, and this fallback names the family by its current member.
// All three classify `local`, so only the reported spelling is approximate.
var magicNames = map[int64]string{
	0xEF53:     "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0xF2F52010: "f2fs",
	0x2FC12FC1: "zfs",
	0x3153464A: "jfs",
	0x01021994: "tmpfs",
	0x858458F6: "ramfs",
	0x794C7630: "overlay",
	0x65735546: "fuse",
	0x6969:     "nfs",
	0xFF534D42: "cifs",
	0x01021997: "9p",
	0x00C36400: "ceph",
	0x73717368: "squashfs",
	0x9FA0:     "proc",
	0x62656572: "sysfs",
	0x1CD1:     "devpts",
	0x64626720: "debugfs",
	0x27E0EB:   "cgroup",
	0x63677270: "cgroup2",
}

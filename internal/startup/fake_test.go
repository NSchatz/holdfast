package startup

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// fakeFS is the substituted platform every walk test runs against: an in-memory
// MOUNT LAYOUT plus directory tree, with the filesystem-type lookup substituted
// beside it. Both seams have to be substitutable because the gate this repo is
// tested on has neither a network mount nor a second real filesystem, and a
// criterion about bind mounts, sub-mounts, NFS or a denied inspection that could
// only be tested on real ones would not be tested at all.
//
// It models the four things the walk actually reads:
//
//   - a directory tree, with symbolic links, that can be listed and inspected;
//   - a mount layout: a distinct filesystem (mount), one directory exposed at a
//     second path (bind), and one filesystem exposed at two mount points with
//     DIFFERENT device numbers (remount);
//   - a filesystem type per subtree, and every way the lookup can fail;
//   - injected failures: a denied stat, a denied listing, and a listing that
//     fails partway with the entries it did read.
type fakeFS struct {
	nodes       map[string]*fnode
	binds       map[string]string // mount point -> the path whose subtree it exposes
	devOverride map[string]string // mount point -> device id for its subtree
	table       map[string]bool   // what the mount table lists
	noTable     bool              // the mount table cannot be read at all
	typeErr     map[string]error
	readErr     map[string]error
	readPartial map[string]bool
	statErr     map[string]error
	ino         int

	// call counts, so a test can show what the walk's cost is bounded BY. There
	// is deliberately no way to open a file here at all: the Platform interface
	// has no such method, so a walk that needed to read a media file to classify
	// a path could not compile, let alone pass.
	inspects int
	readDirs int
	types    int
}

type fnode struct {
	dir  bool
	link string
	dev  string
	fsID string
	ino  string
	typ  string
}

func newFS() *fakeFS {
	f := &fakeFS{
		nodes:       map[string]*fnode{},
		binds:       map[string]string{},
		devOverride: map[string]string{},
		table:       map[string]bool{},
		typeErr:     map[string]error{},
		readErr:     map[string]error{},
		readPartial: map[string]bool{},
		statErr:     map[string]error{},
	}
	f.nodes["/"] = &fnode{dir: true, dev: "dev0", fsID: "fs0", ino: f.nextIno(), typ: "ext4"}
	return f
}

func (f *fakeFS) nextIno() string {
	f.ino++
	return "i" + strconv.Itoa(f.ino)
}

// mkdir creates a directory (and any missing parent), on its parent's filesystem.
func (f *fakeFS) mkdir(path string) *fakeFS {
	p := cleanPath(path)
	if _, ok := f.nodes[p]; ok {
		return f
	}
	parent := filepath.Dir(p)
	if parent != p {
		f.mkdir(parent)
	}
	pn := f.nodes[parent]
	f.nodes[p] = &fnode{dir: true, dev: pn.dev, fsID: pn.fsID, ino: f.nextIno()}
	return f
}

func (f *fakeFS) mkfile(path string) *fakeFS {
	p := cleanPath(path)
	f.mkdir(filepath.Dir(p))
	pn := f.nodes[filepath.Dir(p)]
	f.nodes[p] = &fnode{dev: pn.dev, fsID: pn.fsID, ino: f.nextIno()}
	return f
}

func (f *fakeFS) symlink(path, target string) *fakeFS {
	p := cleanPath(path)
	f.mkdir(filepath.Dir(p))
	f.nodes[p] = &fnode{link: target, ino: f.nextIno()}
	return f
}

// mount creates a DISTINCT mounted filesystem at path: its own device, its own
// filesystem identity, its own type, and an entry in the mount table.
func (f *fakeFS) mount(path, fsType string) *fakeFS {
	p := cleanPath(path)
	f.mkdir(filepath.Dir(p))
	id := f.nextIno()
	f.nodes[p] = &fnode{dir: true, dev: "dev" + id, fsID: "fs" + id, ino: f.nextIno(), typ: fsType}
	f.table[p] = true
	return f
}

// bind exposes the subtree at target under a second mount point, the way
// `mount --bind` does: the SAME device number as the filesystem it exposes, so
// only the mount table can see that it is a mount point at all.
func (f *fakeFS) bind(path, target string) *fakeFS {
	p := cleanPath(path)
	f.mkdir(filepath.Dir(p))
	f.binds[p] = cleanPath(target)
	f.table[p] = true
	return f
}

// remount exposes the subtree at target under a second mount point with a
// DIFFERENT device number, which is what one filesystem mounted twice with
// separate superblocks looks like. The filesystem identity is unchanged, so the
// two mount points are one REGION even though st_dev differs.
func (f *fakeFS) remount(path, target, dev string) *fakeFS {
	f.bind(path, target)
	f.devOverride[cleanPath(path)] = dev
	return f
}

func (f *fakeFS) setType(path, t string) *fakeFS {
	f.mkdir(path)
	f.nodes[cleanPath(path)].typ = t
	return f
}

func (f *fakeFS) failType(path string, err error) *fakeFS { f.typeErr[cleanPath(path)] = err; return f }
func (f *fakeFS) failStat(path string, err error) *fakeFS { f.statErr[cleanPath(path)] = err; return f }
func (f *fakeFS) failRead(path string, err error) *fakeFS { f.readErr[cleanPath(path)] = err; return f }

// failReadPartway injects a listing that fails after handing back some entries -
// the shape a directory removed or damaged mid-walk has.
func (f *fakeFS) failReadPartway(path string, err error) *fakeFS {
	f.failRead(path, err)
	f.readPartial[cleanPath(path)] = true
	return f
}

func (f *fakeFS) hideMountTable() *fakeFS { f.noTable = true; return f }

// resolveBinds maps a literal path onto the path whose storage it exposes, and
// reports any device number the mount it crossed imposes.
func (f *fakeFS) resolveBinds(p string) (string, string) {
	cur, dev := "/", ""
	for _, part := range splitPath(p) {
		cur = filepath.Join(cur, part)
		if tgt, ok := f.binds[cur]; ok {
			dev = f.devOverride[cur]
			cur = tgt
		}
	}
	return cur, dev
}

func splitPath(p string) []string {
	c := cleanPath(p)
	if c == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(c, "/"), "/")
}

func (f *fakeFS) lookup(p string) (*fnode, string, bool) {
	canon, dev := f.resolveBinds(p)
	n, ok := f.nodes[canon]
	return n, dev, ok
}

// resolveLinks resolves symbolic links only. Bind mounts are NOT collapsed: a
// bind mount point is a real path, and the resolved form of a path through one
// is that path.
func (f *fakeFS) resolveLinks(p string, depth int) (string, error) {
	if depth > 40 {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: fmt.Errorf("too many levels of symbolic links")}
	}
	cur := "/"
	for _, part := range splitPath(p) {
		next := filepath.Join(cur, part)
		n, _, ok := f.lookup(next)
		if !ok {
			return "", &fs.PathError{Op: "resolve", Path: next, Err: fs.ErrNotExist}
		}
		if n.link != "" {
			tgt := n.link
			if !filepath.IsAbs(tgt) {
				tgt = filepath.Join(cur, tgt)
			}
			r, err := f.resolveLinks(tgt, depth+1)
			if err != nil {
				return "", err
			}
			next = r
		}
		cur = next
	}
	return cur, nil
}

func (f *fakeFS) Inspect(p string) (Info, error) {
	f.inspects++
	if err := f.statErr[cleanPath(p)]; err != nil {
		return Info{}, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	res, err := f.resolveLinks(p, 0)
	if err != nil {
		return Info{}, err
	}
	if err := f.statErr[res]; err != nil {
		return Info{}, &fs.PathError{Op: "stat", Path: res, Err: err}
	}
	n, dev, ok := f.lookup(res)
	if !ok {
		return Info{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	if dev == "" {
		dev = n.dev
	}
	return Info{IsDir: n.dir, DevID: dev, Region: Region{FS: n.fsID, Node: n.ino}}, nil
}

func (f *fakeFS) ReadDir(p string) ([]Entry, error) {
	f.readDirs++
	res, err := f.resolveLinks(p, 0)
	if err != nil {
		return nil, err
	}
	n, _, ok := f.lookup(res)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
	}
	if !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: fmt.Errorf("not a directory")}
	}
	canon, _ := f.resolveBinds(res)
	var out []Entry
	seen := map[string]bool{}
	add := func(child, name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		cn, _, ok := f.lookup(child)
		if !ok {
			return
		}
		out = append(out, Entry{Name: name, IsDir: cn.dir && cn.link == "", IsLink: cn.link != ""})
	}
	for path := range f.nodes {
		if filepath.Dir(path) == canon && path != canon {
			add(path, filepath.Base(path))
		}
	}
	// A mount point is an entry of the directory it is mounted over, even when
	// the underlying directory carries no node of its own.
	for mp := range f.binds {
		if filepath.Dir(mp) == canon {
			add(mp, filepath.Base(mp))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if err, bad := f.readErr[res]; bad {
		if f.readPartial[res] {
			return out, &fs.PathError{Op: "readdir", Path: p, Err: err}
		}
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: err}
	}
	return out, nil
}

func (f *fakeFS) Resolve(p string) (string, error) { return f.resolveLinks(p, 0) }

func (f *fakeFS) FSType(p string) (string, error) {
	f.types++
	if err, bad := f.typeErr[cleanPath(p)]; bad {
		return "", err
	}
	res, err := f.resolveLinks(p, 0)
	if err != nil {
		return "", err
	}
	if err, bad := f.typeErr[res]; bad {
		return "", err
	}
	canon, _ := f.resolveBinds(res)
	for cur := canon; ; cur = filepath.Dir(cur) {
		if n, ok := f.nodes[cur]; ok && n.typ != "" {
			return n.typ, nil
		}
		if cur == "/" {
			return "", nil
		}
	}
}

func (f *fakeFS) MountPoint(p string) (bool, error) {
	if f.noTable {
		return false, ErrMountInfoUnavailable
	}
	return f.table[cleanPath(p)], nil
}

// mediaByExt is the media-file test the walk is given: by NAME alone, so no file
// is ever opened to decide it.
func mediaByExt(base string) bool {
	return strings.HasSuffix(strings.ToLower(base), ".mkv") || strings.HasSuffix(strings.ToLower(base), ".mp4")
}

// --- assertions used across the tests ---------------------------------------

func recordFor(res Result, path string) (Record, bool) {
	for _, r := range res.Records {
		if r.Path == cleanPath(path) {
			return r, true
		}
	}
	return Record{}, false
}

func noticesOf(res Result, kind NoticeKind) []Notice {
	var out []Notice
	for _, n := range res.Notices {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func hasNotice(res Result, kind NoticeKind, path string) bool {
	for _, n := range noticesOf(res, kind) {
		if n.Path == path || n.Path == cleanPath(path) {
			return true
		}
	}
	return false
}

func causesOf(res Result, kind CauseKind) []Cause {
	var out []Cause
	for _, c := range res.Causes {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func hasCause(res Result, kind CauseKind, path string) bool {
	for _, c := range causesOf(res, kind) {
		if c.Path == path || c.Path == cleanPath(path) {
			return true
		}
	}
	return false
}

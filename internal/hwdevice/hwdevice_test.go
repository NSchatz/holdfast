package hwdevice

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fileGID is path's owning group, read the way the node's owner is read.
func fileGID(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(fi.Sys().(*syscall.Stat_t).Gid)
}

// fakeHost builds a filesystem root with the named render nodes (regular files stand in for
// the device nodes; opening one read-write is the same check) and their sysfs vendor files.
// A vendor of "" writes no vendor file.
func fakeHost(t *testing.T, nodes map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dri := filepath.Join(root, "dev", "dri")
	if err := os.MkdirAll(dri, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, vendor := range nodes {
		if err := os.WriteFile(filepath.Join(dri, name), nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if vendor == "" {
			continue
		}
		dev := filepath.Join(root, "sys", "class", "drm", name, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dev, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDiscover_NoDRIDirectoryIsNoNodesAndNoError(t *testing.T) {
	nodes, err := Discover(t.TempDir())
	if err != nil || len(nodes) != 0 {
		t.Fatalf("Discover(empty root) = %v, %v; want no nodes and no error", nodes, err)
	}
}

func TestDiscover_AnUnlistableDRIDirectoryIsAnErrorNotNoNodes(t *testing.T) {
	root := t.TempDir()
	// /dev/dri exists but is a file: listing it fails with something other than "missing".
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev", "dri"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if nodes, err := Discover(root); err == nil {
		t.Fatalf("Discover over an unlistable /dev/dri = %v, nil; want an error", nodes)
	}
}

func TestDiscover_ReadsEachRenderNodesVendorInNodeOrderAndIgnoresCardNodes(t *testing.T) {
	root := fakeHost(t, map[string]string{
		"renderD129":  "0x1002",
		"renderD128":  "0x8086",
		"renderD1000": "0x10DE", // upper case, and after 129 numerically though not by name
		"renderD130":  "0x1234", // a vendor this build does not know
		"renderD131":  "",       // no vendor file
		"card0":       "0x8086",
		"renderD":     "0x8086", // no number
		"renderDx":    "0x8086", // not a number
	})
	nodes, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		got = append(got, filepath.Base(n.Path)+"="+n.Vendor)
		if !strings.HasPrefix(n.Path, "/dev/dri/") {
			t.Errorf("node path %q is not the path an encoder is told (/dev/dri/...)", n.Path)
		}
		if n.OpenErr != nil {
			t.Errorf("node %s: unexpected open error %v", n.Path, n.OpenErr)
		}
	}
	want := "renderD128=intel renderD129=amd renderD130=unknown renderD131=unknown renderD1000=nvidia"
	if strings.Join(got, " ") != want {
		t.Errorf("Discover = %s\nwant       %s", strings.Join(got, " "), want)
	}
}

func TestDiscover_ANodeThisProcessCannotOpenNamesGroupAdd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-000 file; the permission path cannot be shown as root")
	}
	root := fakeHost(t, map[string]string{"renderD128": "0x8086"})
	if err := os.Chmod(filepath.Join(root, "dev", "dri", "renderD128"), 0); err != nil {
		t.Fatal(err)
	}
	nodes, err := Discover(root)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("Discover = %v, %v", nodes, err)
	}
	n := nodes[0]
	var perm *PermissionError
	if !errors.As(n.OpenErr, &perm) {
		t.Fatalf("OpenErr = %v, want a *PermissionError", n.OpenErr)
	}
	if n.Usable() {
		t.Error("a node this process cannot open is Usable")
	}
	msg := n.OpenErr.Error()
	for _, want := range []string{"/dev/dri/renderD128", "permission denied", "group_add", "--group-add",
		"GID " + strconv.Itoa(fileGID(t, filepath.Join(root, "dev", "dri", "renderD128")))} {
		if !strings.Contains(msg, want) {
			t.Errorf("permission error %q does not name %q", msg, want)
		}
	}
	a := Assign(nodes)
	if a.VAAPI != "" || a.QSV != "" {
		t.Errorf("Assign handed out an unopenable node: %+v", a)
	}
	if !strings.Contains(a.Why["vaapi"], "group_add") || !strings.Contains(a.Why["qsv"], "group_add") {
		t.Errorf("Assign's reasons do not carry the permission lever: %+v", a.Why)
	}
}

func TestDiscover_AMissingNodeIsNotUsable(t *testing.T) {
	root := fakeHost(t, map[string]string{"renderD128": "0x8086"})
	// A dangling entry: listed, but opening it fails with something other than permission.
	p := filepath.Join(root, "dev", "dri", "renderD128")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nowhere"), p); err != nil {
		t.Fatal(err)
	}
	nodes, err := Discover(root)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("Discover = %v, %v", nodes, err)
	}
	var perm *PermissionError
	if nodes[0].OpenErr == nil || errors.As(nodes[0].OpenErr, &perm) {
		t.Errorf("OpenErr = %v, want a non-permission error", nodes[0].OpenErr)
	}
	if nodes[0].Usable() {
		t.Error("a dangling node is Usable")
	}
}

func TestPermissionError_UnknownGroup(t *testing.T) {
	msg := (&PermissionError{Path: "/dev/dri/renderD128", GID: -1}).Error()
	if !strings.Contains(msg, "the group that owns it") || strings.Contains(msg, "GID") {
		t.Errorf("unknown-GID message = %q", msg)
	}
	if got := ownerGID(filepath.Join(t.TempDir(), "absent")); got != -1 {
		t.Errorf("ownerGID(absent) = %d, want -1", got)
	}
}

func TestAssign_VAAPITakesTheFirstUsableIntelOrAMDNodeAndQSVTheFirstIntel(t *testing.T) {
	cases := []struct {
		name       string
		nodes      []Node
		vaapi, qsv string
		whyVAAPI   string
		whyQSV     string
	}{
		{name: "no nodes",
			whyVAAPI: "no render node", whyQSV: "no render node"},
		{name: "nvidia only",
			nodes:    []Node{{Path: "/dev/dri/renderD128", Vendor: VendorNVIDIA}},
			whyVAAPI: "no intel or amd render node", whyQSV: "no intel render node"},
		{name: "unknown vendor only",
			nodes:    []Node{{Path: "/dev/dri/renderD128", Vendor: VendorUnknown}},
			whyVAAPI: "no intel or amd render node", whyQSV: "no intel render node"},
		{name: "amd then intel",
			nodes: []Node{{Path: "/dev/dri/renderD128", Vendor: VendorAMD},
				{Path: "/dev/dri/renderD129", Vendor: VendorIntel}},
			vaapi: "/dev/dri/renderD128", qsv: "/dev/dri/renderD129"},
		{name: "nvidia then intel then amd",
			nodes: []Node{{Path: "/dev/dri/renderD128", Vendor: VendorNVIDIA},
				{Path: "/dev/dri/renderD129", Vendor: VendorIntel},
				{Path: "/dev/dri/renderD130", Vendor: VendorAMD}},
			vaapi: "/dev/dri/renderD129", qsv: "/dev/dri/renderD129"},
		{name: "amd only",
			nodes:  []Node{{Path: "/dev/dri/renderD128", Vendor: VendorAMD}},
			vaapi:  "/dev/dri/renderD128",
			whyQSV: "no intel render node"},
		{name: "an unusable intel node is skipped for the next",
			nodes: []Node{{Path: "/dev/dri/renderD128", Vendor: VendorIntel, OpenErr: errors.New("boom")},
				{Path: "/dev/dri/renderD129", Vendor: VendorIntel}},
			vaapi: "/dev/dri/renderD129", qsv: "/dev/dri/renderD129"},
		{name: "an unusable amd node's reason is given for vaapi, and not for qsv",
			nodes:    []Node{{Path: "/dev/dri/renderD128", Vendor: VendorAMD, OpenErr: errors.New("amd-boom")}},
			whyVAAPI: "amd-boom", whyQSV: "no intel render node"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Assign(c.nodes)
			if a.VAAPI != c.vaapi || a.QSV != c.qsv {
				t.Errorf("Assign = vaapi %q qsv %q, want %q %q", a.VAAPI, a.QSV, c.vaapi, c.qsv)
			}
			check := func(key, got, want string) {
				if want == "" {
					if _, ok := a.Why[key]; ok {
						t.Errorf("Why[%s] = %q for an assigned encoder", key, got)
					}
					return
				}
				if !strings.Contains(got, want) {
					t.Errorf("Why[%s] = %q, want it to contain %q", key, got, want)
				}
				if key == "qsv" && strings.Contains(got, "amd-boom") {
					t.Errorf("Why[qsv] carries an AMD node's reason: %q", got)
				}
			}
			check("vaapi", a.Why["vaapi"], c.whyVAAPI)
			check("qsv", a.Why["qsv"], c.whyQSV)
		})
	}
}

func TestRenderMinor(t *testing.T) {
	for name, want := range map[string]int{"renderD128": 128, "renderD0": 0} {
		if got, ok := renderMinor(name); !ok || got != want {
			t.Errorf("renderMinor(%q) = %d, %v", name, got, ok)
		}
	}
	for _, name := range []string{"card0", "renderD", "renderD-1", "renderDx", "xrenderD128"} {
		if _, ok := renderMinor(name); ok {
			t.Errorf("renderMinor(%q) accepted", name)
		}
	}
}

// Package hwdevice finds the render nodes a hardware encoder can open, and says which one each
// encoder is handed. It answers from what the host shows this process - the device nodes
// under /dev/dri and the vendor sysfs names for each - and never from a guess: a node that
// is missing is absent, a node this process may not open is named with the reason, and an
// encoder with no node it can use is assigned none. Nothing here decides that an encoder
// WORKS; that is internal/encoder's probe, which runs the real command line against the node
// assigned here (docs/design/hardware.md#detection).
//
// Only VAAPI and QSV are told a node. NVENC reaches its device through the CUDA driver the
// NVIDIA Container Toolkit injects, not through a render node, and AMF is not supported in
// the image (docs/design/hardware.md#amf).
package hwdevice

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// The PCI vendor IDs this build recognises, as sysfs prints them. Read 2026-09-30 from the
// PCI ID repository: https://pci-ids.ucw.cz/read/PC/8086 (Intel Corporation),
// https://pci-ids.ucw.cz/read/PC/1002 (Advanced Micro Devices, Inc. [AMD/ATI]),
// https://pci-ids.ucw.cz/read/PC/10de (NVIDIA Corporation).
const (
	VendorIntel  = "intel"
	VendorAMD    = "amd"
	VendorNVIDIA = "nvidia"
	// VendorUnknown is a node whose vendor file is missing, unreadable or names a vendor
	// this build does not recognise. It is never assigned to an encoder.
	VendorUnknown = "unknown"
)

var vendorIDs = map[string]string{
	"0x8086": VendorIntel,
	"0x1002": VendorAMD,
	"0x10de": VendorNVIDIA,
}

// Node is one render node as this process sees it.
type Node struct {
	// Path is the node's path as the encoder is told it, e.g. /dev/dri/renderD128.
	Path string
	// Vendor is one of the Vendor constants.
	Vendor string
	// OpenErr is why this process cannot open the node read-write, or nil when it can.
	OpenErr error
}

// Usable reports whether an encoder may be handed this node.
func (n Node) Usable() bool { return n.OpenErr == nil && n.Vendor != VendorUnknown }

// Discover lists the render nodes under root (the filesystem root; "/" in production, a
// fake tree in a test), in node order. A missing /dev/dri is no nodes and no error: a host
// without a render node is an ordinary host. Any other failure to list the directory is
// returned, because an answer of "no nodes" there would be a guess.
func Discover(root string) ([]Node, error) {
	dir := filepath.Join(root, "dev", "dri")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	var nodes []Node
	for _, e := range entries {
		name := e.Name()
		if _, ok := renderMinor(name); !ok {
			continue
		}
		path := filepath.Join(dir, name)
		nodes = append(nodes, Node{
			Path:    filepath.Join("/dev/dri", name),
			Vendor:  vendorOf(root, name),
			OpenErr: openErr(path),
		})
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, _ := renderMinor(filepath.Base(nodes[i].Path))
		b, _ := renderMinor(filepath.Base(nodes[j].Path))
		return a < b
	})
	return nodes, nil
}

// renderMinor is the number of a render node's name ("renderD128" is 128), and false for
// any other name (the card nodes, by-path links).
func renderMinor(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "renderD")
	if !ok || digits == "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// vendorOf reads the node's PCI vendor from sysfs (/sys/class/drm/<node>/device/vendor).
func vendorOf(root, name string) string {
	b, err := os.ReadFile(filepath.Join(root, "sys", "class", "drm", name, "device", "vendor"))
	if err != nil {
		return VendorUnknown
	}
	if v, ok := vendorIDs[strings.ToLower(strings.TrimSpace(string(b)))]; ok {
		return v
	}
	return VendorUnknown
}

// openErr is why this process cannot open path read-write (the access an encoder needs), or
// nil. A permission failure names the lever: the process's user is not in the group that
// owns the node, which a container fixes with group_add.
func openErr(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		_ = f.Close()
		return nil
	}
	if errors.Is(err, fs.ErrPermission) {
		return &PermissionError{Path: path, GID: ownerGID(path)}
	}
	return err
}

// ownerGID is the group that owns path, or -1 when it cannot be read.
func ownerGID(path string) int {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return -1
}

// PermissionError is a render node this process may not open.
type PermissionError struct {
	Path string
	// GID is the node's owning group, or -1 when it could not be read.
	GID int
}

func (e *PermissionError) Error() string {
	group, add := "the group that owns it", "the node's group"
	if e.GID >= 0 {
		gid := strconv.Itoa(e.GID)
		group, add = "its group (GID "+gid+")", "GID "+gid
	}
	return "cannot open " + e.Path + " (permission denied): this process's user is not in " + group +
		"; add " + add + " to the container with group_add (compose) or --group-add (docker run), " +
		"see docs/docker.md"
}

// Assignment is the node each encoder that needs one is handed, and why an encoder got none.
type Assignment struct {
	// VAAPI is the node hevc_vaapi opens: the first usable Intel or AMD node. NVIDIA's
	// render nodes are not VAAPI devices without a driver this image does not carry.
	VAAPI string
	// QSV is the node hevc_qsv's VAAPI child device opens: the first usable Intel node.
	QSV string
	// Why is, for an encoder assigned no node, the reason, keyed by encoder ("vaapi",
	// "qsv").
	Why map[string]string
}

// Assign chooses each encoder's node from nodes.
func Assign(nodes []Node) Assignment {
	a := Assignment{Why: map[string]string{}}
	for _, n := range nodes {
		if !n.Usable() {
			continue
		}
		if a.VAAPI == "" && (n.Vendor == VendorIntel || n.Vendor == VendorAMD) {
			a.VAAPI = n.Path
		}
		if a.QSV == "" && n.Vendor == VendorIntel {
			a.QSV = n.Path
		}
	}
	if a.VAAPI == "" {
		a.Why["vaapi"] = why(nodes, VendorIntel, VendorAMD)
	}
	if a.QSV == "" {
		a.Why["qsv"] = why(nodes, VendorIntel)
	}
	return a
}

// why says why no node of the wanted vendors was usable: none present, or each one's reason.
func why(nodes []Node, vendors ...string) string {
	var reasons []string
	for _, n := range nodes {
		for _, v := range vendors {
			if n.Vendor == v && n.OpenErr != nil {
				reasons = append(reasons, n.OpenErr.Error())
			}
		}
	}
	if len(reasons) > 0 {
		return strings.Join(reasons, "; ")
	}
	names := strings.Join(vendors, " or ")
	if len(nodes) == 0 {
		return "no render node under /dev/dri (pass the host's /dev/dri to the container with devices:)"
	}
	return "no " + names + " render node under /dev/dri"
}

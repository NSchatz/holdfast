package config

// The per-root working location: a library_roots entry may name its own scratch_dir,
// which REPLACES the top-level one for that root's files, empty string included (an
// entry's `scratch_dir: ""` writes that root's encodes beside the source again).
//
// What it is for is a library spread over several drives. One top-level scratch_dir
// puts every drive's encodes on one device; an entry's own puts each root's encodes on
// a directory of its choosing - the same drive's, outside the library folders, which
// keeps a media server watching those folders from rescanning them on every write the
// encoder makes. Everything the top-level key promises holds for each entry's value
// unchanged: it moves only the working file, the accepted bytes are copied back beside
// the source and the swap is the same same-directory rename (docs/scratch.md), and the
// startup check refuses one that is missing, unwritable, short of the floor or
// overlapping any library root.

import (
	"fmt"
	"path/filepath"
	"strings"
)

// scratchDirKey is the one place the key is spelled: the top-level key, the root-entry
// parser and the refusals read it from here.
const scratchDirKey = "scratch_dir"

// scratchDirValue reads an entry's scratch_dir as written: a string, and either empty
// (beside the source) or an absolute path. The remaining checks are the same as the
// top-level value's and run with them, in Validate and at startup.
func scratchDirValue(where string, raw any) (string, error) {
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s sets %q to %#v: it is a directory path, or \"\" to encode beside the source",
			where, scratchDirKey, raw)
	}
	s = strings.TrimSpace(s)
	if s != "" && !filepath.IsAbs(s) {
		return "", fmt.Errorf("%s sets %q to %q, which is not an absolute path", where, scratchDirKey, s)
	}
	return s, nil
}

// ScratchDirFor is the working location for a source at path p: the scratch_dir of
// the root containing p where that root's entry names one, the top-level scratch_dir
// otherwise. "" means beside the source.
func (c *Config) ScratchDirFor(p string) string {
	if r, ok := c.RootFor(p); ok && r.ScratchDir != nil {
		return strings.TrimSpace(*r.ScratchDir)
	}
	return strings.TrimSpace(c.ScratchDir)
}

// ScratchDirs is every working location some root's files are written to, cleaned, in
// configured order and each once. The top-level scratch_dir is in it only where a root
// inherits it, or where there are no roots to say otherwise.
func (c *Config) ScratchDirs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	roots := c.RootProfiles()
	if len(roots) == 0 {
		add(c.ScratchDir)
	}
	for _, r := range roots {
		if r.ScratchDir != nil {
			add(*r.ScratchDir)
		} else {
			add(c.ScratchDir)
		}
	}
	return out
}

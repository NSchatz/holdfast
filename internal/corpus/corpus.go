// Package corpus locates this repository and the Markdown it ships.
//
// Both halves are defined RELATIVE TO THE REPOSITORY rather than to a working
// directory: a caller that guessed its own root would read a different set of files
// depending on who invoked it, and the set is the point.
package corpus

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepoRoot walks up from dir until it finds the directory holding go.mod, which is the
// repository root.
func RepoRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("corpus: no go.mod above %s - cannot locate the repository root", dir)
		}
		d = parent
	}
}

// Markdown returns every Markdown file under root, sorted, skipping VCS and vendor
// directories. It is a WALK and not a list, so a document that did not exist when a
// caller was written is still in the set.
func Markdown(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Package corpus locates this repository and the documents it ships.
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

// ExampleConfigName is the shipped example configuration.
const ExampleConfigName = "config.example.yaml"

// Documents returns every document this repository SHIPS TO A READER: the Markdown, plus
// the example configuration.
//
// The example belongs in the set for the same reason every Markdown file does, and the fact
// that it is not Markdown is a property of its syntax rather than of its readership. It is
// the file an operator copies before the first file goes, it is where three of this build's
// knobs are actually written, and a claim standing in it reaches exactly the reader a claim
// standing in the README reaches. A check over "the documents" that admitted only one file
// extension would be narrowed by a detail nobody chose, and an ABSENCE check narrowed that
// way reports everything clean.
func Documents(root string) ([]string, error) {
	out, err := Markdown(root)
	if err != nil {
		return nil, err
	}
	example := filepath.Join(root, ExampleConfigName)
	if _, err := os.Stat(example); err != nil {
		return nil, fmt.Errorf("corpus: %s: %w", ExampleConfigName, err)
	}
	out = append(out, example)
	sort.Strings(out)
	return out, nil
}

package config

import (
	"reflect"
	"strings"
	"testing"
)

// A library root's own scratch_dir (docs/scratch.md#per-root): an entry's value replaces
// the top-level one for that root, "" included, an entry that names none inherits it, and
// the key moves no profile digest.

const perRootScratchLibrary = `
scratch_dir: /mnt/cache/holdfast
library_roots:
  - path: /mnt/plex1/movies
    scratch_dir: /mnt/plex1/.holdfast/work
  - path: /mnt/plex1/tv
    scratch_dir: /mnt/plex1/.holdfast/work
  - path: /mnt/plex2/movies
    scratch_dir: ""
  - path: /mnt/plex3/movies
`

func TestScratchDir_AnEntryReplacesTheTopLevelOneForItsRoot(t *testing.T) {
	c := loadYAML(t, perRootScratchLibrary)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for file, want := range map[string]string{
		"/mnt/plex1/movies/RRR (2022)/RRR.mkv": "/mnt/plex1/.holdfast/work",
		"/mnt/plex1/tv/Show/Season 1/e1.mkv":   "/mnt/plex1/.holdfast/work",
		"/mnt/plex2/movies/Film/film.mkv":      "",
		"/mnt/plex3/movies/Film/film.mkv":      "/mnt/cache/holdfast",
	} {
		if got := c.ScratchDirFor(file); got != want {
			t.Errorf("ScratchDirFor(%s) = %q, want %q", file, got, want)
		}
	}
	want := []string{"/mnt/plex1/.holdfast/work", "/mnt/cache/holdfast"}
	if got := c.ScratchDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ScratchDirs() = %v, want %v (each once, \"\" never)", got, want)
	}
}

func TestScratchDir_TheTopLevelOneIsNotCheckedWhereEveryRootReplacesIt(t *testing.T) {
	c := loadYAML(t, `
scratch_dir: /mnt/cache/holdfast
library_roots:
  - path: /mnt/plex1/movies
    scratch_dir: /mnt/plex1/.holdfast/work
`)
	if got, want := c.ScratchDirs(), []string{"/mnt/plex1/.holdfast/work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ScratchDirs() = %v, want %v", got, want)
	}
}

func TestScratchDir_IsInNoProfileDigest(t *testing.T) {
	with := rootByPath(t, loadYAML(t, `
library_roots:
  - path: /mnt/films
    scratch_dir: /mnt/work
`), "/mnt/films")
	without := rootByPath(t, loadYAML(t, `
library_roots:
  - path: /mnt/films
`), "/mnt/films")
	if with.Profile.Digest() != without.Profile.Digest() {
		t.Error("an entry's scratch_dir moved the root's profile digest, which would re-open its terminal rows")
	}
}

func TestScratchDir_AnEntryValueIsRefusedUnlessAnAbsolutePathOrEmpty(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"relative":   {`work`, "not an absolute path"},
		"not string": {`[a, b]`, "is a directory path"},
		"root":       {`/`, "filesystem root"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "library_roots:\n  - path: /mnt/films\n    scratch_dir: "+tc.value+"\n")
			if err == nil {
				err = c.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

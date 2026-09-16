package corpus

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRepoRoot_IsTheSameRootFromAnyDirectoryInside grades [AC-7] of S0150: the root is
// defined relative to the REPOSITORY and not to a working directory. A caller that
// resolved its own root would read a different set of documents depending on who invoked
// it, and the set is what ScratchCorpus's check is run over.
func TestRepoRoot_IsTheSameRootFromAnyDirectoryInside(t *testing.T) {
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatalf("RepoRoot(.): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("RepoRoot returned %s, which holds no go.mod: %v", root, err)
	}

	// Two directories at different depths, neither of them the root.
	for _, from := range []string{
		filepath.Join(root, "internal", "corpus"),
		filepath.Join(root, "internal", "engine"),
	} {
		got, err := RepoRoot(from)
		if err != nil {
			t.Fatalf("RepoRoot(%s): %v", from, err)
		}
		if got != root {
			t.Errorf("RepoRoot(%s) = %s, want %s - the root moved with the caller", from, got, root)
		}
	}
}

// TestMarkdown_IsAWalkAndNotAList grades [AC-7] of S0150: the set is every Markdown file
// under the root, found by walking. A set narrowed to whichever files a caller happened to
// name would go on passing while a document written anywhere else went unread - and from
// the outside the two look identical. The skipped directories are asserted too: .git and a
// vendored tree carry Markdown this repository does not ship.
func TestMarkdown_IsAWalkAndNotAList(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{
		"top.md",
		filepath.Join("docs", "nested.md"),
		filepath.Join("docs", "deeper", "again.md"),
		"notes.txt",
		filepath.Join(".git", "COMMIT_EDITMSG.md"),
		filepath.Join("vendor", "dependency.md"),
		filepath.Join("node_modules", "package.md"),
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# hi\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Markdown(dir)
	if err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	want := []string{
		filepath.Join(dir, "docs", "deeper", "again.md"),
		filepath.Join(dir, "docs", "nested.md"),
		filepath.Join(dir, "top.md"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Markdown walked to %v, want %v - a document at depth, a file that is not "+
			"Markdown, or a skipped directory is being treated wrongly", got, want)
	}

	// The same walk over the repository itself, so the rule above is not only true of a
	// fixture: a set narrowed to one file is not what this repository ships.
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatalf("RepoRoot(.): %v", err)
	}
	shipped, err := Markdown(root)
	if err != nil {
		t.Fatalf("Markdown(%s): %v", root, err)
	}
	if len(shipped) < 2 {
		t.Fatalf("the repository's corpus holds %d file(s): %v", len(shipped), shipped)
	}
	for _, f := range shipped {
		if !strings.HasPrefix(f, root+string(os.PathSeparator)) || !strings.HasSuffix(f, ".md") {
			t.Errorf("%s is in the corpus and is not a Markdown file under %s", f, root)
		}
	}
}

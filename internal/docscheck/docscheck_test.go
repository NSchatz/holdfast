package docscheck

// The documentation check, exercised in BOTH directions.
//
// A check only run against documentation that passes proves nothing: it could be
// returning "no problems" because it is looking at the wrong file, or at no file, or
// because its anchor never matches anything. So the shipped documentation is asserted to
// PASS (which is what makes `make check` red when a statement is later deleted), and
// three deliberately broken corpora are asserted to FAIL.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShippedDocumentation_CarriesBothResidualWindowStatements is the check itself, over
// the repository's own corpus. It is the test that fails `make check` when the
// documentation loses either statement - which is the whole of the criterion.
func TestShippedDocumentation_CarriesBothResidualWindowStatements(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus(%s): %v", root, err)
	}
	problems, err := Check(files)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("the shipped documentation does not satisfy the residual-window obligation:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// TestCorpus_IsTheRepositorysShippedDocumentationAndNotOneConvenientFile pins the SET
// the check reads. A corpus narrowed to whichever file happens to carry the anchors
// would pass forever while a statement written anywhere else went unchecked - and a
// reader could not tell the difference from the outside. This asserts the walk finds
// the documents that exist today AND that it is a walk rather than a list, by proving a
// Markdown file added anywhere under the root joins the corpus.
func TestCorpus_IsTheRepositorysShippedDocumentationAndNotOneConvenientFile(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("the corpus holds %d file(s) - a set narrowed to one file is not what the repository ships: %v",
			len(files), files)
	}
	for _, want := range []string{
		"README.md",
		filepath.Join("docs", "docker.md"),
		filepath.Join("docs", "filesystem.md"),
		filepath.Join("docs", "migration.md"),
	} {
		if !containsSuffix(files, want) {
			t.Errorf("the corpus does not carry %s; it holds %v", want, files)
		}
	}
	// A walk, not a list: a new document is in the corpus the moment it exists.
	sub := t.TempDir()
	if err := os.WriteFile(filepath.Join(sub, "nfs.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Corpus(sub)
	if err != nil {
		t.Fatalf("Corpus(temp): %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "nfs.md" {
		t.Errorf("a Markdown file added under the root did not join the corpus: %v", got)
	}
}

// TestCheck_TwoBareAnchorsFail is the first negative direction: documentation reduced to
// the two anchors with nothing under them. An anchor with no text is not a statement,
// and a check that accepted it would pass a document that says nothing at all.
func TestCheck_TwoBareAnchorsFail(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": "# Limitations\n\n<a id=\"" + AnchorLocal + "\"></a>\n\n<a id=\"" + AnchorNetwork + "\"></a>\n",
	})
	problems := check(t, dir)
	if len(problems) != 2 {
		t.Fatalf("want a problem for each bare anchor, got %d: %v", len(problems), problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, "nothing follows it") {
			t.Errorf("problem does not name the empty statement: %q", p)
		}
	}
}

// TestCheck_MissingAnchorsFail: the statement is not there at all.
func TestCheck_MissingAnchorsFail(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": "# Limitations\n\nholdfast re-checks the source before the swap.\n",
	})
	problems := check(t, dir)
	if len(problems) != 2 {
		t.Fatalf("want a problem for each missing anchor, got %d: %v", len(problems), problems)
	}
}

// TestCheck_NetworkStatementWithoutTheTokenFails is the second negative direction, and
// the sharper one: the network section HAS text, and the text is plausible - it is just
// text that never attributes the window to the client's attribute caching. That
// attribution is the whole content of the network statement, so a section without it is
// not the statement that was owed.
func TestCheck_NetworkStatementWithoutTheTokenFails(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": "# Limitations\n\n" +
			"<a id=\"" + AnchorLocal + "\"></a>\n\nThe guard compares size and a whole-second mtime.\n\n" +
			"<a id=\"" + AnchorNetwork + "\"></a>\n\nOn a network filesystem the window is wider and holdfast is slower to notice.\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the missing-token problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], NetworkToken) {
		t.Errorf("the problem does not name the token it wanted: %q", problems[0])
	}
}

// TestCheck_BothStatementsPresentPass is the positive direction over a MINIMAL corpus,
// so the shipped-documentation test above is not the only thing standing between this
// check and a false green. It also pins the two stop conditions: text runs to the next
// anchor or heading, and a heading does not count as the statement's text.
func TestCheck_BothStatementsPresentPass(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.md": "# Something else entirely\n\nnothing to see.\n",
		"b.md": "## Windows\n\n" +
			"<a id=\"" + AnchorLocal + "\"></a>\n\nSize plus a whole-second mtime; a same-size rewrite inside one second is invisible.\n\n" +
			"<a id=\"" + AnchorNetwork + "\"></a>\n\nWidened by the client's ATTRIBUTE CACHING, which belongs to the client, not to holdfast.\n\n" +
			"## Another section\n",
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("documentation that satisfies the rule was reported as failing: %v", problems)
	}
}

// TestCheck_TheTokenSurvivesAMarkdownLineWrap pins the normalisation. In Markdown a
// single newline inside a paragraph is a space, so a check that matched the raw bytes
// would make the position of the author's line wrap decide whether the documentation
// passes - and re-flowing a paragraph is not a change to what it says. This is not
// hypothetical: the shipped paragraph wraps between "attribute" and "cache".
func TestCheck_TheTokenSurvivesAMarkdownLineWrap(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"b.md": "<a id=\"" + AnchorLocal + "\"></a>\n\nsize and a whole-second mtime\n\n" +
			"<a id=\"" + AnchorNetwork + "\"></a>\n\nthe same window, widened by the client's attribute\ncache, which is the client's and not holdfast's\n",
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a wrapped paragraph was reported as failing: %v", problems)
	}
}

// TestCheck_AHeadingImmediatelyAfterAnAnchorIsNotAStatement pins the rule that the text
// must be text: a heading following the anchor ends the statement rather than being it.
func TestCheck_AHeadingImmediatelyAfterAnAnchorIsNotAStatement(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"b.md": "<a id=\"" + AnchorLocal + "\"></a>\n\n## Local\n\nreal text, but under the heading and after it\n\n" +
			"<a id=\"" + AnchorNetwork + "\"></a>\n\nattribute cache text\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 || !strings.Contains(problems[0], "nothing follows it") {
		t.Fatalf("want the local anchor reported as carrying no statement, got %v", problems)
	}
}

// TestShippedDocumentation_TheCheckBitesOnTheREALTEXT is the mutation proof against the
// documentation this repository actually ships, not a fixture that resembles it.
//
// Every negative test above builds its own corpus, which proves the RULE is right and
// proves nothing about the shipped file: a check whose anchors had drifted away from the
// real document would pass all of them and pass the positive shipped test too, because
// "no problems" is also what an anchor that matches nothing produces on a corpus with no
// anchors. So this copies the real corpus, breaks the real statements one at a time, and
// demands each break be caught.
func TestShippedDocumentation_TheCheckBitesOnTheREALTEXT(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}

	// Which shipped file carries each anchor is the implementer's choice and is not
	// fixed - so find it rather than assuming it.
	carrier := func(anchor string) string {
		for _, f := range files {
			st, err := findStatement(f, anchor)
			if err != nil {
				t.Fatalf("findStatement(%s, %s): %v", f, anchor, err)
			}
			if st.File != "" && st.Present() {
				return f
			}
		}
		t.Fatalf("no shipped file carries a present statement for %q", anchor)
		return ""
	}

	mutations := []struct {
		name    string
		anchor  string
		mutate  func(string) string
		problem string
	}{
		{
			name:   "the local anchor is removed from the shipped text",
			anchor: AnchorLocal,
			mutate: func(s string) string {
				return strings.Replace(s, `<a id="`+AnchorLocal+`"></a>`, "", 1)
			},
			problem: AnchorLocal,
		},
		{
			name:   "the network statement loses the attribute-cache attribution",
			anchor: AnchorNetwork,
			mutate: func(s string) string {
				out := strings.ReplaceAll(s, "attribute\ncache", "REDACTED")
				out = strings.ReplaceAll(out, "attribute cache", "REDACTED")
				out = strings.ReplaceAll(out, "attribute caching", "REDACTED")
				return out
			},
			problem: NetworkToken,
		},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			file := carrier(m.anchor)
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			mutated := m.mutate(string(original))
			if mutated == string(original) {
				t.Fatalf("the mutation changed nothing in %s - it proves nothing", file)
			}

			// Copy the WHOLE corpus, so a statement hiding in another shipped file
			// would keep the check green and be caught here rather than missed.
			dir := t.TempDir()
			for _, f := range files {
				rel, err := filepath.Rel(root, f)
				if err != nil {
					t.Fatal(err)
				}
				var body []byte
				if f == file {
					body = []byte(mutated)
				} else {
					body, err = os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
				}
				dst := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dst, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			problems := check(t, dir)
			if len(problems) == 0 {
				t.Fatalf("breaking the SHIPPED %s statement in %s was not caught - the check does not read the real text",
					m.anchor, file)
			}
			if !strings.Contains(strings.Join(problems, "\n"), m.problem) {
				t.Errorf("the problem does not name %q: %v", m.problem, problems)
			}
		})
	}
}

// --- helpers ------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := RepoRoot(wd)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	return root
}

func writeCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func check(t *testing.T, dir string) []string {
	t.Helper()
	files, err := Corpus(dir)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	problems, err := Check(files)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return problems
}

func containsSuffix(files []string, suffix string) bool {
	for _, f := range files {
		if strings.HasSuffix(f, suffix) {
			return true
		}
	}
	return false
}

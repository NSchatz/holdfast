// Package docscheck is a MECHANICAL check over the repository's own shipped
// documentation. It rides `make check` (via the ordinary test step), so documentation
// that has lost a statement the code's safety story depends on fails the gate that
// everything else fails.
//
// # Why a check at all
//
// The source-mutation guard has a residual window, and the window is different on local
// storage than it is on a network mount. That difference is not a footnote: it is the
// difference between "holdfast will catch a mid-encode rewrite" and "holdfast may not",
// and an operator can only weigh it if it is written down. Documentation obligations
// that nothing enforces are documentation obligations that quietly lapse - so this one
// is enforced by the same target that runs the fixture suite.
//
// # What it checks, and what it deliberately does not
//
// It checks PRESENCE and one token. It does not judge whether the prose is true, or
// well written, or complete - no mechanical check can, and one that pretended to would
// either fail good documentation or pass bad. Precisely:
//
//   - Two fixed anchors must exist: residual-window-local and residual-window-network.
//     They are FIXED here rather than chosen per-run, because a check free to pick its
//     own anchor is a check that can be made to pass by moving the goalposts.
//   - A statement is PRESENT only when its anchor exists AND at least one non-blank
//     line follows it, before the next anchor or heading, that is not itself a heading
//     or an anchor. An anchor with nothing under it is not a statement.
//   - The network statement must ALSO carry the case-insensitive token "attribute cach"
//     (matching "attribute cache" and "attribute caching"). That is the whole of
//     "attributes the window to the client's attribute caching", mechanically: the
//     network window belongs to the client's cache and a network statement that never
//     says so is not the statement that was owed.
//
// # The corpus
//
// Corpus() walks the repository and returns EVERY Markdown file in it. That set is
// recorded here, and it is what the repository ships as documentation, for a reason
// that can be rechecked rather than taken on trust:
//
//   - The repository distributes two things. The container image carries the binary,
//     the bundled ffmpeg and NOTICE - no prose at all - so every document a reader can
//     actually consult is a file in the source tree.
//   - In the source tree, the documents are the Markdown files and nothing else. LICENSE
//     and NOTICE are legal texts, not documentation of behaviour; config.example.yaml
//     and docker-compose.yml are examples that carry comments but state no contract;
//     the Go doc comments are compiled-in developer text, not something a deploying
//     operator reads.
//   - Walking for *.md therefore returns the whole set rather than a selection, and it
//     keeps doing so when a document is ADDED - which is the property a fixed list of
//     filenames would lose the first time somebody wrote docs/nfs.md and put the
//     statement there instead.
//
// At the time of writing that walk finds five files: README.md, CLAUDE.md,
// docs/docker.md, docs/filesystem.md and docs/migration.md. Which of them carries the
// anchors is not fixed and is not this package's business - the obligation is about the
// TEXT the repository ships, so a statement anywhere in the corpus satisfies it and a
// statement outside the corpus satisfies nothing however it is anchored. (Today they
// are in docs/filesystem.md, beside the rest of what the storage a library sits on
// costs; that is a choice about where the prose reads best, not a narrowing of the set
// this package checks.)
package docscheck

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The two FIXED anchors. They are the same identifiers the store records as a job's
// residual-window class label, so a reader holding a job's record can grep straight to
// the statement that explains it.
const (
	AnchorLocal   = "residual-window-local"
	AnchorNetwork = "residual-window-network"
)

// NetworkToken is the case-insensitive substring the NETWORK statement must carry. It
// is the shortest string that matches both "attribute cache" and "attribute caching"
// without matching anything else that could be written by accident.
const NetworkToken = "attribute cach"

// anchorRe matches an HTML anchor of the form <a id="..."></a> or <a name="...">, which
// is how a Markdown document that has to be readable on GitHub declares one.
var anchorRe = regexp.MustCompile(`(?i)<a\s+(?:id|name)\s*=\s*"([^"]+)"`)

// headingRe matches an ATX Markdown heading.
var headingRe = regexp.MustCompile(`^\s{0,3}#{1,6}\s`)

// Corpus returns every Markdown file under root, sorted, skipping VCS and vendor
// directories. See the package doc for why this set IS what the repository ships.
func Corpus(root string) ([]string, error) {
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

// RepoRoot walks up from dir until it finds the directory holding go.mod - the
// repository root, and therefore the root of the corpus. It is here rather than in the
// test because the corpus is defined RELATIVE TO THE REPOSITORY, and a check that
// guessed its own root from a working directory would be checking a different set of
// files depending on who invoked it.
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
			return "", fmt.Errorf("docscheck: no go.mod above %s - cannot locate the repository root", dir)
		}
		d = parent
	}
}

// Statement is what one anchor introduced, gathered from one file.
type Statement struct {
	Anchor string
	File   string
	// Text is every line after the anchor, up to the next anchor or heading, that is
	// itself neither a heading nor an anchor.
	Text string
}

// Present reports whether the anchor introduced any real text at all.
func (s Statement) Present() bool { return strings.TrimSpace(s.Text) != "" }

// findStatement scans one file for the text an anchor introduces.
//
// The stop conditions are the criterion word for word: text runs to the NEXT ANCHOR or
// the next HEADING, and a line that is itself a heading or an anchor does not count as
// the statement's text. Two anchors back to back therefore introduce nothing, which is
// exactly the failure this has to catch.
func findStatement(path, anchor string) (Statement, error) {
	f, err := os.Open(path)
	if err != nil {
		return Statement{}, err
	}
	defer func() { _ = f.Close() }()

	st := Statement{Anchor: anchor, File: path}
	var b strings.Builder
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		anchors := anchorRe.FindStringSubmatch(line)
		isHeading := headingRe.MatchString(line)

		if !found {
			if len(anchors) == 2 && anchors[1] == anchor {
				found = true
			}
			continue
		}
		if len(anchors) == 2 || isHeading {
			break // the next anchor or heading ends the statement
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return Statement{}, err
	}
	if !found {
		return Statement{Anchor: anchor, File: ""}, nil
	}
	st.Text = b.String()
	return st, nil
}

// Check applies the whole rule to a corpus and returns one problem per line, empty when
// the documentation satisfies it.
//
// An anchor appearing in more than one file is not an error: the check wants the
// statement to EXIST in what the repository ships, so ANY occurrence that satisfies the
// rule satisfies the criterion, and the problem is only reported when NONE does. The
// message then names the strongest near-miss, so a failure points at a file rather than
// saying "nowhere".
func Check(files []string) ([]string, error) {
	var problems []string

	local, err := statements(files, AnchorLocal)
	if err != nil {
		return nil, err
	}
	switch {
	case len(local) == 0:
		problems = append(problems, fmt.Sprintf(
			"no shipped document carries the anchor %q - the local residual-window statement is missing", AnchorLocal))
	case !anyPresent(local):
		problems = append(problems, fmt.Sprintf(
			"%s: the anchor %q is present but nothing follows it - an anchor with no text is not a statement",
			local[0].File, AnchorLocal))
	}

	network, err := statements(files, AnchorNetwork)
	if err != nil {
		return nil, err
	}
	switch {
	case len(network) == 0:
		problems = append(problems, fmt.Sprintf(
			"no shipped document carries the anchor %q - the network residual-window statement is missing", AnchorNetwork))
	case !anyPresent(network):
		problems = append(problems, fmt.Sprintf(
			"%s: the anchor %q is present but nothing follows it - an anchor with no text is not a statement",
			network[0].File, AnchorNetwork))
	case !anyCarriesToken(network):
		problems = append(problems, fmt.Sprintf(
			"%s: the network residual-window statement never says %q - the network window is the CLIENT's attribute cache, "+
				"and a statement that does not attribute it there is not the statement that was owed",
			firstPresent(network).File, NetworkToken))
	}

	return problems, nil
}

// statements returns every occurrence of an anchor across the corpus, in corpus order.
func statements(files []string, anchor string) ([]Statement, error) {
	var out []Statement
	for _, f := range files {
		st, err := findStatement(f, anchor)
		if err != nil {
			return nil, err
		}
		if st.File == "" {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

func anyPresent(sts []Statement) bool {
	for _, s := range sts {
		if s.Present() {
			return true
		}
	}
	return false
}

func anyCarriesToken(sts []Statement) bool {
	for _, s := range sts {
		if s.Present() && strings.Contains(normalize(s.Text), NetworkToken) {
			return true
		}
	}
	return false
}

// normalize lower-cases a statement and collapses every run of whitespace to one space
// before the token is looked for. That is not a loosening, it is what "the text carries
// this token" MEANS in Markdown: a single newline inside a paragraph is a space, so
// where the author's line wrap happens to fall would otherwise decide whether the
// documentation passes - and re-flowing a paragraph is not a change to what it says.
// (Observed, not theorised: the shipped paragraph wrapped between "attribute" and
// "cache" and the un-normalised check failed it.)
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func firstPresent(sts []Statement) Statement {
	for _, s := range sts {
		if s.Present() {
			return s
		}
	}
	return sts[0]
}

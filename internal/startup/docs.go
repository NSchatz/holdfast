package startup

import (
	"fmt"
	"sort"
	"strings"
)

// The shipped documentation carries two things this package is the authority on,
// and a document may not merely gesture at either: a statement of what startup
// COSTS, and the complete set of filesystem types this build classifies local.
// Both are checked MECHANICALLY over the shipped text by a test that the
// aggregate check target runs, so a heading with nothing under it, a cost
// statement missing one of its three parts, or a documented set that has drifted
// from the build's own fails the gate rather than reaching a reader.
const (
	// DocFile is the shipped documentation these checks read.
	DocFile = "docs/filesystem.md"
	// CostHeading is the heading the cost statement lives under.
	CostHeading = "## What the startup check costs"
	// LocalSetHeading is the heading the recognised-local set lives under.
	LocalSetHeading = "## Filesystem types this build classifies local"
)

// costParts are the three things the cost statement must SAY. Each is checked by
// phrases that cannot be satisfied by a heading, an anchor or a promise to
// document it later.
var costParts = []struct {
	Name  string
	Needs []string
}{
	{
		Name:  "that startup traverses the directory tree beneath every configured library root before the first encode",
		Needs: []string{"directory tree beneath every configured library root", "before the first encode"},
	},
	{
		Name:  "that a run which will be refused pays that traversal in full first",
		Needs: []string{"refused", "in full"},
	},
	{
		Name:  "that the cost grows with the size of the directory tree, every entry being read, and not with the size or content of the media files",
		Needs: []string{"grows with the size of the directory tree", "every entry", "no media file is opened"},
	},
}

// CheckCostStatement reports whether the shipped documentation states what
// startup costs, in all three of its parts.
func CheckCostStatement(doc string) error {
	body, ok := section(doc, CostHeading)
	if !ok {
		return fmt.Errorf("%s: the heading %q is missing", DocFile, CostHeading)
	}
	if strings.TrimSpace(stripAnchors(body)) == "" {
		return fmt.Errorf("%s: the section %q is empty; a heading is not a statement", DocFile, CostHeading)
	}
	flat := normalise(body)
	for _, part := range costParts {
		for _, need := range part.Needs {
			if !strings.Contains(flat, normalise(need)) {
				return fmt.Errorf("%s: the cost statement does not say %s (missing %q)", DocFile, part.Name, need)
			}
		}
	}
	return nil
}

// CheckLocalSet reports whether the set of filesystem types the documentation
// states is exactly the set this build classifies local. Two builds recognising
// different sets must be distinguishable from what each prints, so a build whose
// set moves with the documentation left unedited fails here.
func CheckLocalSet(doc string, build []string) error {
	body, ok := section(doc, LocalSetHeading)
	if !ok {
		return fmt.Errorf("%s: the heading %q is missing", DocFile, LocalSetHeading)
	}
	fenced, ok := firstFencedBlock(body)
	if !ok {
		return fmt.Errorf("%s: the section %q carries no fenced block naming the set", DocFile, LocalSetHeading)
	}
	documented := strings.Fields(fenced)
	sort.Strings(documented)
	want := append([]string(nil), build...)
	sort.Strings(want)
	if strings.Join(documented, " ") != strings.Join(want, " ") {
		return fmt.Errorf("%s: the documented local set [%s] is not the set this build classifies local [%s]",
			DocFile, strings.Join(documented, " "), strings.Join(want, " "))
	}
	return nil
}

// section returns the body under a markdown heading, up to the next heading of
// the same or a higher level.
func section(doc, heading string) (string, bool) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "# ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

func firstFencedBlock(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	open := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			if open < 0 {
				open = i
				continue
			}
			return strings.Join(lines[open+1:i], "\n"), true
		}
	}
	return "", false
}

func stripAnchors(s string) string {
	for {
		i := strings.Index(s, "<a ")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</a>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</a>"):]
	}
}

// normalise lowercases and collapses every run of whitespace to one space, so a
// required phrase is found across a line wrap.
func normalise(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

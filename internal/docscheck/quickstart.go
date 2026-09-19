package docscheck

// The quick start's ORDER, graded (S0100 AC-13).
//
// The first `holdfast run` a reader meets is the one they will type, and this tool is
// licensed to replace a source and delete the original. Leading with the bounded form is
// therefore the difference between a first run over one file a person is watching and a
// first run over every file they own - and an order kept only by good intentions is an
// order that reverts the next time somebody tidies the block.
//
// The check is PRESENCE AND SEQUENCE and nothing more, for the same reason Undocumented
// above is presence and nothing more: whether the prose around it is persuasive is not a
// question a mechanical check can answer, and one that pretended to would be a check
// nobody could trust either way.

import (
	"fmt"
	"strings"
)

const (
	// quickStartHeading opens the section this check reads.
	quickStartHeading = "## Quick start"
	// runCommand is the invocation whose first occurrence in that section decides.
	runCommand = "holdfast run "
	// boundedFlag is what makes that first occurrence a bounded one.
	boundedFlag = "--file"
)

// BoundedRunLeadsQuickStart reports whether the README's quick start presents a bounded
// `holdfast run --file` as the FIRST `holdfast run` command in the section, ahead of any
// whole-library run. It returns an error naming what it found otherwise.
//
// Every way this can go wrong is an error rather than a pass, including the section or the
// commands disappearing: a check that answered "fine" when it could not find what it grades
// would report green for ever the moment the README moved.
func BoundedRunLeadsQuickStart(readme string) error {
	section, ok := quickStartSection(readme)
	if !ok {
		return fmt.Errorf("docscheck: no %q section in the README - the check cannot grade an order in a section that is not there", quickStartHeading)
	}
	for _, line := range strings.Split(section, "\n") {
		idx := strings.Index(line, runCommand)
		if idx < 0 {
			continue
		}
		if strings.Contains(line[idx:], boundedFlag) {
			return nil
		}
		return fmt.Errorf("docscheck: the first %q in the %q section is a whole-library run (%q); "+
			"a bounded %s run must come first, because it is the command a reader types before "+
			"they have watched this tool replace anything",
			strings.TrimSpace(runCommand), quickStartHeading, strings.TrimSpace(line), boundedFlag)
	}
	return fmt.Errorf("docscheck: the %q section carries no %q command at all", quickStartHeading, strings.TrimSpace(runCommand))
}

// quickStartSection returns the text of the quick start, from its heading to the next
// heading at the SAME level - so the subsections beneath it are part of it, exactly as
// they are to a reader.
func quickStartSection(doc string) (string, bool) {
	start := strings.Index(doc, quickStartHeading)
	if start < 0 {
		return "", false
	}
	rest := doc[start+len(quickStartHeading):]
	// Walked by OFFSET rather than by searching for the line's text: a line that also
	// appears earlier in the section would otherwise cut it in the wrong place.
	for at := 0; ; {
		nl := strings.Index(rest[at:], "\n")
		if nl < 0 {
			return rest, true
		}
		at += nl + 1
		if strings.HasPrefix(rest[at:], "## ") {
			return rest[:at], true
		}
	}
}

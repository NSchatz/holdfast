package startup

import (
	"fmt"
	"strings"
)

// The shipped documentation's statement about the path filters, checked mechanically
// over the shipped text by a test the aggregate check target runs - like the cost
// statement, the local set and the scratch statement beside it, and for the same reason:
// a documentation obligation nothing enforces is one that quietly lapses.
//
// These two keys decide which files a DELETE-CAPABLE tool is allowed to touch, and every
// part checked below is one an operator who misreads it loses something by. A filter
// believed to be protecting a directory and is not hands that directory to the encoder;
// an include list believed to be additive stops the rest of the library being scanned;
// and a doublestar written mid-component matches far less than it appears to.
const (
	// PathFilterDocFile is where the statement lives today. It is named so a failure
	// points somewhere, and the check is not narrowed to it: the statement is satisfied
	// by the anchor wherever in the corpus it appears.
	PathFilterDocFile = "docs/profiles.md"

	// AnchorPathFilters introduces the statement. The anchor is a FIXED constant,
	// because a check free to pick its own anchor is a check that can be made to pass by
	// moving the goalposts.
	AnchorPathFilters = "path-filters"
)

// pathFilterParts are the things the statement must SAY. Each is checked by phrases a
// heading, an anchor or a promise to document it later cannot satisfy.
var pathFilterParts = []struct {
	Name  string
	Needs []string
}{
	{
		Name:  "which two keys these are",
		Needs: []string{"exclude_paths", "include_paths"},
	},
	{
		Name:  "that both are optional and default to empty, and that an empty list means what an absent key means",
		Needs: []string{"both default to empty", "an absent key", "stays eligible"},
	},
	{
		Name:  "that exclude wins over include, and why that is the fail-safe direction for a tool that deletes sources",
		Needs: []string{"exclude wins", "matched by both", "fewer files"},
	},
	{
		Name:  "what the pattern language is, and that a pattern matches the whole of a path rather than a substring of it",
		Needs: []string{"doublestar", "whole", "never a substring", "movies/tv-archive"},
	},
	{
		Name:  "that a doublestar must be its own path component, since a mid-pattern one silently matches less",
		Needs: []string{"its own path component"},
	},
	{
		Name:  "how a pattern is anchored, and that a directory pattern covers what is under it",
		Needs: []string{"relative to the library root", "does begin with", "covers everything"},
	},
	{
		Name:  "that a malformed pattern is refused at startup and one that can cover nothing is reported",
		Needs: []string{"startup refusal", "covering nothing"},
	},
	{
		Name: "that the walk does not list a directory an exclude pattern reaches, and that a terminal row " +
			"beneath one survives because the retention pass draws no conclusion from a directory this run " +
			"did not list",
		Needs: []string{"does not list", "keeps it", "holdfast export", "draws no conclusion", "did not list"},
	},
}

// pathFilterForbids are the claims the statement may no longer make, in the form the check
// compares them: lowercased, whitespace collapsed and markdown emphasis removed. Both are the
// retired rule (S0168): an excluded directory was once listed so the retention pass would keep
// the rows beneath it, and the walk now prunes it instead. A statement that kept either
// sentence beside the new one would be describing two walks, one of which no longer exists.
var pathFilterForbids = []string{
	"excluded directory is still listed",
	"the walk is unchanged",
}

// CheckPathFilterStatement reports whether one shipped document states what the path
// filters are, in every one of their parts.
func CheckPathFilterStatement(doc string) error {
	body, ok := anchoredSection(doc, AnchorPathFilters)
	if !ok {
		return fmt.Errorf("%s: the anchor %q is missing", PathFilterDocFile, AnchorPathFilters)
	}
	if strings.TrimSpace(stripAnchors(body)) == "" {
		return fmt.Errorf("%s: the section under %q is empty; an anchor is not a statement",
			PathFilterDocFile, AnchorPathFilters)
	}
	flat := normalise(body)
	for _, part := range pathFilterParts {
		for _, need := range part.Needs {
			if !strings.Contains(flat, normalise(need)) {
				return fmt.Errorf("%s: the path filter statement does not say %s (missing %q)",
					PathFilterDocFile, part.Name, need)
			}
		}
	}
	plain := normalise(strings.NewReplacer("*", "", "`", "").Replace(body))
	for _, claim := range pathFilterForbids {
		if strings.Contains(plain, claim) {
			return fmt.Errorf("%s: the path filter statement still claims %q, which the walk no longer does: "+
				"a directory an exclude pattern reaches is not listed", PathFilterDocFile, claim)
		}
	}
	return nil
}

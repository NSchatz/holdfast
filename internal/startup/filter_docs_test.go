package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shippedPathFilterDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", PathFilterDocFile))
	if err != nil {
		t.Fatalf("reading the shipped path filter documentation: %v", err)
	}
	return string(b)
}

// TestPathFilterDocs_TheShippedTextDocumentsBothKeys is [AC-13]: the repository's own
// mechanical documentation check shows that both keys, their empty default, the
// fail-safe direction and the pattern language are documented - enforced by the check
// rather than by review, so the statement cannot quietly lapse.
func TestPathFilterDocs_TheShippedTextDocumentsBothKeys(t *testing.T) {
	if err := CheckPathFilterStatement(shippedPathFilterDoc(t)); err != nil {
		t.Fatalf("the shipped path filter statement: %v", err)
	}
}

// [AC-13], the anti-vacuity half. A check is only worth what it reds on, so every
// mutation here is one a real edit could make and each MUST fail.
func TestPathFilterDocs_TheCheckBites(t *testing.T) {
	doc := shippedPathFilterDoc(t)

	t.Run("the anchor missing entirely", func(t *testing.T) {
		if err := CheckPathFilterStatement("# holdfast\n\nnothing to see\n"); err == nil {
			t.Fatal("a document with no path filter statement at all passed")
		}
	})

	t.Run("an anchor with nothing under it", func(t *testing.T) {
		mutated := `<a id="` + AnchorPathFilters + `"></a>` + "\n\n## Next\n\nbody\n"
		if err := CheckPathFilterStatement(mutated); err == nil {
			t.Fatal("an anchor introducing nothing passed; an anchor is not a statement")
		}
	})

	// Each part, redacted one phrase at a time. The redaction is scoped to the STATEMENT
	// rather than to the whole file, for the reason the scratch check scopes its own:
	// redacting the first occurrence anywhere passes the moment an unrelated section
	// above happens to use the same ordinary words, and the check then goes green while
	// claiming to have been defeated.
	start := strings.Index(doc, `id="`+AnchorPathFilters+`"`)
	if start < 0 {
		t.Fatalf("the shipped documentation has no %q anchor to mutate", AnchorPathFilters)
	}
	head, tail := doc[:start], doc[start:]
	for _, part := range pathFilterParts {
		for _, need := range part.Needs {
			t.Run("a statement missing: "+need, func(t *testing.T) {
				mutatedTail := redactFold(tail, need)
				if mutatedTail == tail {
					t.Fatalf("the phrase %q is not in the shipped path filter STATEMENT, so removing it proves nothing", need)
				}
				if err := CheckPathFilterStatement(head + mutatedTail); err == nil {
					t.Fatalf("a path filter statement missing %q passed", need)
				}
			})
		}
	}
}

// retiredExcludedDirectoryStatement is the section the path filter statement carried before
// the walk pruned an excluded directory (S0168), verbatim. It is kept here, and only here, so
// the check can be shown to refuse it.
const retiredExcludedDirectoryStatement = `### What a filter does NOT change

An excluded directory is still **listed**. The scan lists exactly the directories it
would have listed with no filter configured and records exactly the same evidence about
where it looked - because the ledger's retention pass may only remove a terminal row when
this run LISTED the directory the file should be in and the file was not there. A filter
that skipped the directory instead of its files would make every row beneath it read as a
file that had been deleted, which would discard audit history irreversibly and hand the
excluded subtree back to the encoder on the next scan.

So an excluded file that already has a terminal row keeps it, and ` + "`holdfast export`" + ` still
carries its record. Excluding a path is not a request to forget what was done to it.

Filtering is not a way to make a scan faster: the walk is unchanged, and only the set of
files offered to the pipeline is narrower.

`

// TestS0168AC14_ThePathFilterCheckRefusesTheRetiredWording is the part of S0168 [AC-14] the
// repository's own statement check carries: the statement and its check changed together, and
// the check REDS on the wording it replaced - whether that wording stands in place of the new
// section or beside it. A check that only looked for the new phrases would pass a document
// still claiming an excluded directory is listed, as long as it said the new thing too.
func TestS0168AC14_ThePathFilterCheckRefusesTheRetiredWording(t *testing.T) {
	doc := shippedPathFilterDoc(t)
	if err := CheckPathFilterStatement(doc); err != nil {
		t.Fatalf("the shipped statement fails its own check: %v", err)
	}
	start := strings.Index(doc, "### What a filter does NOT change")
	end := strings.Index(doc, "`holdfast validate` prints, per library root")
	if start < 0 || end < start {
		t.Fatal("the shipped statement has no section on what a filter does not change")
	}

	t.Run("the retired section in place of the new one", func(t *testing.T) {
		if err := CheckPathFilterStatement(doc[:start] + retiredExcludedDirectoryStatement + doc[end:]); err == nil {
			t.Fatal("the statement check passed the retired wording, which claims an excluded directory is listed")
		}
	})
	for _, claim := range []string{
		"An excluded directory is still **listed**.",
		"Filtering is not a way to make a scan faster: the walk is unchanged.",
	} {
		t.Run("the retired claim beside the new section: "+claim, func(t *testing.T) {
			mutated := doc[:start] + claim + "\n\n" + doc[start:]
			if err := CheckPathFilterStatement(mutated); err == nil {
				t.Fatalf("the statement check passed a statement that still says %q", claim)
			}
		})
	}
}

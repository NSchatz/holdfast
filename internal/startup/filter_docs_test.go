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

package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shippedDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", DocFile))
	if err != nil {
		t.Fatalf("reading the shipped documentation: %v", err)
	}
	return string(b)
}

// TestDocs_TheShippedTextSaysWhatStartupCostsAndWhichTypesAreLocal is the check
// itself, run over the shipped file by the aggregate check target. A statement
// this repo owes its users is not a promise to write one: it is checked, or it
// drifts (AC2g, AC3a).
func TestDocs_TheShippedTextSaysWhatStartupCostsAndWhichTypesAreLocal(t *testing.T) {
	doc := shippedDoc(t)
	if err := CheckCostStatement(doc); err != nil {
		t.Fatalf("the shipped cost statement: %v", err)
	}
	if err := CheckLocalSet(doc, LocalTypes()); err != nil {
		t.Fatalf("the shipped local set: %v", err)
	}
}

// TestDocs_TheCheckBites proves the check is not a green light that never turns
// red. Each mutation is one a real edit could make, and each must fail.
func TestDocs_TheCheckBites(t *testing.T) {
	doc := shippedDoc(t)

	t.Run("documentation reduced to the cost heading with no body", func(t *testing.T) {
		if err := CheckCostStatement(CostHeading + "\n\n## Something else\n\nbody\n"); err == nil {
			t.Fatal("a heading with nothing under it passed; a heading is not a statement")
		}
	})

	t.Run("an anchor is not a body either", func(t *testing.T) {
		mutated := CostHeading + "\n\n<a id=\"cost\"></a>\n\n## Next\n"
		if err := CheckCostStatement(mutated); err == nil {
			t.Fatal("a section holding only an anchor passed")
		}
	})

	t.Run("the heading missing entirely", func(t *testing.T) {
		if err := CheckCostStatement("# holdfast\n\nnothing to see\n"); err == nil {
			t.Fatal("a document with no cost section at all passed")
		}
	})

	// Each of the three parts, removed one at a time, must be caught: a cost
	// statement that says two of the three is not the statement this owes.
	for _, part := range costParts {
		for _, need := range part.Needs {
			t.Run("a cost statement missing: "+need, func(t *testing.T) {
				mutated := strings.Replace(doc, need, "REDACTED", 1)
				if mutated == doc {
					t.Fatalf("the phrase %q is not in the shipped text, so removing it proves nothing", need)
				}
				if err := CheckCostStatement(mutated); err == nil {
					t.Fatalf("a cost statement missing %q passed", need)
				}
			})
		}
	}

	t.Run("a documented local set the build disagrees with", func(t *testing.T) {
		// A build whose set GAINS a type with the documentation left unedited.
		if err := CheckLocalSet(doc, append(LocalTypes(), "reiserfs")); err == nil {
			t.Fatal("a build recognising a type the documentation does not name passed")
		}
		// A build whose set LOSES one, likewise.
		short := LocalTypes()[1:]
		if err := CheckLocalSet(doc, short); err == nil {
			t.Fatal("documentation naming a type the build no longer recognises passed")
		}
	})

	t.Run("the local-set section with no fenced set", func(t *testing.T) {
		if err := CheckLocalSet(LocalSetHeading+"\n\nthe usual ones\n", LocalTypes()); err == nil {
			t.Fatal("a section that only gestures at the set passed")
		}
	})
}

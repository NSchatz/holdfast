package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shippedStreamSelectionDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", StreamSelectionDocFile))
	if err != nil {
		t.Fatalf("reading the shipped stream-selection documentation: %v", err)
	}
	return string(b)
}

// TestStreamSelectionDocs_TheShippedCorpusStatesWhatTheFourKeysDo is [AC-18]: the
// repository's own mechanical documentation check carries a NEW fixed anchor introducing
// one statement, and that statement owes a clause for each of the four keys and their
// defaults, the untagged-stream rule, the never-a-silent-file fallback, the remux-only VMAF
// skip with its identity check, and audio transcoding as a non-goal.
//
// It is run over the CORPUS and not over one named file, which is the half a file-scoped
// check cannot do: a statement can be lost by deleting the document that held it just as
// easily as by editing it out.
func TestStreamSelectionDocs_TheShippedCorpusStatesWhatTheFourKeysDo(t *testing.T) {
	files, err := ScratchCorpus(".")
	if err != nil {
		t.Fatalf("building the documentation corpus: %v", err)
	}
	if len(files) < 3 {
		t.Fatalf("the corpus found only %d Markdown file(s) - it is not walking the repository: %v",
			len(files), files)
	}
	if err := CheckStreamSelectionInCorpus(files); err != nil {
		t.Fatalf("the shipped stream-selection statement: %v", err)
	}
}

// [AC-18], the anti-vacuity half: the check is only worth what it REDS on, and the criterion
// names the three ways it must red. Each mutation here is one a real edit could make, and
// every one of them MUST fail.
func TestStreamSelectionDocs_TheCheckBites(t *testing.T) {
	doc := shippedStreamSelectionDoc(t)

	t.Run("the anchor missing entirely", func(t *testing.T) {
		if err := CheckStreamSelectionStatement("# holdfast\n\nnothing to see\n"); err == nil {
			t.Fatal("a document with no stream-selection statement at all passed")
		}
	})

	t.Run("an anchor with nothing under it", func(t *testing.T) {
		mutated := `<a id="` + AnchorStreamSelection + `"></a>` + "\n\n## Next\n\nbody\n"
		if err := CheckStreamSelectionStatement(mutated); err == nil {
			t.Fatal("an anchor introducing nothing passed; an anchor is not a statement")
		}
	})

	t.Run("no shipped document carries the anchor", func(t *testing.T) {
		dir := t.TempDir()
		other := filepath.Join(dir, "other.md")
		// Every required PHRASE, and no anchor. This is the goalpost-moving mutation the
		// fixed-anchor design exists to refuse: prose no anchor introduces does not satisfy
		// the criterion.
		var b strings.Builder
		b.WriteString("# Some document\n\n")
		for _, part := range streamSelectionParts {
			for _, need := range part.Needs {
				b.WriteString(need + " ")
			}
		}
		b.WriteString("\n")
		if err := os.WriteFile(other, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CheckStreamSelectionInCorpus([]string{other}); err == nil {
			t.Fatal("a corpus in which NO document carries the anchor passed, on prose that merely " +
				"happens to contain every phrase - which is exactly the check that can be made to " +
				"pass by moving the goalposts")
		}
	})

	// Each part, redacted one phrase at a time. The redaction is scoped to the STATEMENT
	// rather than to the whole file, for the reason the scratch and path-filter checks scope
	// their own: redacting the first occurrence anywhere passes the moment an unrelated
	// section happens to use the same ordinary words, and the check then goes green while
	// claiming to have been defeated.
	start := strings.Index(doc, `id="`+AnchorStreamSelection+`"`)
	if start < 0 {
		t.Fatalf("the shipped documentation has no %q anchor to mutate", AnchorStreamSelection)
	}
	head, tail := doc[:start], doc[start:]
	for _, part := range streamSelectionParts {
		for _, need := range part.Needs {
			t.Run("a statement missing: "+need, func(t *testing.T) {
				mutatedTail := redactFold(tail, need)
				if mutatedTail == tail {
					t.Fatalf("the phrase %q is not in the shipped stream-selection STATEMENT, so "+
						"removing it proves nothing", need)
				}
				if err := CheckStreamSelectionStatement(head + mutatedTail); err == nil {
					t.Fatalf("a stream-selection statement missing %q passed", need)
				}
			})
		}
	}
}

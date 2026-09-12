package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shippedScratchDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ScratchDocFile))
	if err != nil {
		t.Fatalf("reading the shipped scratch documentation: %v", err)
	}
	return string(b)
}

// AC-B16, first half: the shipped text SAYS what a scratch location buys and what
// it costs. Run over the shipped file by the aggregate check target, because a
// statement this repository owes its users is checked or it drifts.
func TestScratchDocs_TheShippedTextStatesTheBenefitAndTheCostHonestly(t *testing.T) {
	if err := CheckScratchStatement(shippedScratchDoc(t)); err != nil {
		t.Fatalf("the shipped scratch statement: %v", err)
	}
}

// AC-B16, second half: NO document in the repository claims the thing that is not
// true. The corpus is a WALK of every Markdown file, so a claim written into a
// document that did not exist when this was written is still caught.
func TestScratchDocs_NoShippedDocumentClaimsItSparesTheSourceDrive(t *testing.T) {
	files, err := ScratchCorpus(".")
	if err != nil {
		t.Fatalf("building the documentation corpus: %v", err)
	}
	if len(files) < 3 {
		t.Fatalf("the corpus found only %d Markdown file(s) - it is not walking the repository: %v", len(files), files)
	}
	found, err := CheckNoSourceDriveClaim(files)
	if err != nil {
		t.Fatalf("checking the corpus: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("shipped documentation claims a scratch location spares the source drive:\n  %s",
			strings.Join(found, "\n  "))
	}
}

// The anti-vacuity half. Every check above is only worth what it reds on, so each
// mutation here is one a real edit could make and each MUST fail.
func TestScratchDocs_TheChecksBite(t *testing.T) {
	doc := shippedScratchDoc(t)

	t.Run("the anchor missing entirely", func(t *testing.T) {
		if err := CheckScratchStatement("# holdfast\n\nnothing to see\n"); err == nil {
			t.Fatal("a document with no scratch statement at all passed")
		}
	})

	t.Run("an anchor with nothing under it", func(t *testing.T) {
		mutated := `<a id="` + AnchorScratchBenefit + `"></a>` + "\n\n## Next\n\nbody\n"
		if err := CheckScratchStatement(mutated); err == nil {
			t.Fatal("an anchor introducing nothing passed; an anchor is not a statement")
		}
	})

	// The five parts, redacted one phrase at a time. The redaction is scoped to the
	// STATEMENT rather than to the whole file for the reason the cost-statement
	// check scopes its own: redacting the first occurrence anywhere passes the
	// moment an unrelated section above happens to use the same ordinary words, and
	// the check then goes green while claiming to have been defeated.
	start := strings.Index(doc, `id="`+AnchorScratchBenefit+`"`)
	if start < 0 {
		t.Fatalf("the shipped documentation has no %q anchor to mutate", AnchorScratchBenefit)
	}
	head, tail := doc[:start], doc[start:]
	for _, part := range scratchBenefitParts {
		for _, need := range part.Needs {
			t.Run("a scratch statement missing: "+need, func(t *testing.T) {
				mutatedTail := redactFold(tail, need)
				if mutatedTail == tail {
					t.Fatalf("the phrase %q is not in the shipped scratch STATEMENT, so removing it proves nothing", need)
				}
				if err := CheckScratchStatement(head + mutatedTail); err == nil {
					t.Fatalf("a scratch statement missing %q passed", need)
				}
			})
		}
	}

	t.Run("a document that claims it spares the source drive", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.md")
		if err := os.WriteFile(bad, []byte("# Cache\n\nPointing scratch_dir at an SSD reduces the bytes written to the source drive.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		found, err := CheckNoSourceDriveClaim([]string{bad})
		if err != nil {
			t.Fatalf("checking: %v", err)
		}
		if len(found) != 1 {
			t.Fatalf("the claim was not caught: %v", found)
		}
		if !strings.Contains(found[0], "bad.md:3") {
			t.Fatalf("the report does not name the file and line: %q", found[0])
		}
	})

	t.Run("a document that claims it extends the source drive's life", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.md")
		if err := os.WriteFile(bad, []byte("Using a cache drive extends the life of the source disk.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		found, err := CheckNoSourceDriveClaim([]string{bad})
		if err != nil {
			t.Fatalf("checking: %v", err)
		}
		if len(found) != 1 {
			t.Fatalf("the claim was not caught: %v", found)
		}
	})

	// The allowance has to work, or the shipped text could not refuse the claim in
	// the claim's own words - and it has to work ONLY on the line that carries it,
	// or one marker would silence a document.
	t.Run("the allowance exempts its own line and nothing else", func(t *testing.T) {
		dir := t.TempDir()
		mixed := filepath.Join(dir, "mixed.md")
		body := "It does not reduce the bytes written to the source drive. <!-- " + ScratchClaimAllow + " -->\n" +
			"It reduces the bytes written to the source drive.\n"
		if err := os.WriteFile(mixed, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		found, err := CheckNoSourceDriveClaim([]string{mixed})
		if err != nil {
			t.Fatalf("checking: %v", err)
		}
		if len(found) != 1 {
			t.Fatalf("want exactly the unmarked line caught, got %v", found)
		}
		if !strings.Contains(found[0], "mixed.md:2") {
			t.Fatalf("the wrong line was caught: %q", found[0])
		}
	})
}

// redactFold removes EVERY case-insensitive occurrence of need from s.
//
// Every, not the first: the mutation this drives is "a statement that does not say
// this", and a statement that says it twice is not mutated by removing one of them -
// the check would then pass and the test would report a defeat that never happened.
// Case-insensitive so a phrase the document spells with different capitalisation is
// still redacted.
func redactFold(s, need string) string {
	var b strings.Builder
	lowerNeed := strings.ToLower(need)
	for {
		i := strings.Index(strings.ToLower(s), lowerNeed)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("REDACTED")
		s = s[i+len(need):]
	}
}

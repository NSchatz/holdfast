package docscheck_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/docscheck"
	"github.com/NSchatz/holdfast/internal/engine"
)

// shippedCorpus is every document this repository ships, from the package's own one way of
// getting a file set. A check over the corpus that chose its own list would decide the answer
// by deciding the list.
func shippedCorpus(t *testing.T) []string {
	t.Helper()
	files, err := docscheck.Corpus()
	if err != nil {
		t.Fatalf("read the shipped corpus: %v", err)
	}
	if len(files) < 5 {
		t.Fatalf("only %d document(s) were found - the corpus is not being read, so nothing "+
			"below is being checked", len(files))
	}
	// Anti-vacuity on the half that is not Markdown: the example configuration is the
	// document an operator copies, and a corpus silently back down to .md only would report
	// every check below clean without looking at it.
	example := false
	for _, f := range files {
		if filepath.Base(f) == corpus.ExampleConfigName {
			example = true
		}
	}
	if !example {
		t.Fatalf("the corpus of %d document(s) does not carry %s, so nothing below reads the one "+
			"document in this repository where these keys are actually written", len(files),
			corpus.ExampleConfigName)
	}
	return files
}

// TestInterlacingPostureStatementIsPresent grades [AC-15] of
// S0107-holdfast-interlacing-decision: the documentation check fails unless the README
// carries one anchored statement of the interlacing posture, under its own fixed anchor,
// carrying a token for each clause it owes.
//
// This build can deinterlace a source and then delete the original. A reader deciding
// whether to turn that key on needs all four clauses in one place - on request, off by
// default, frame-rate-preserving, telecined content skipped - because any three of them
// without the fourth is a different claim about what will happen to their library.
func TestInterlacingPostureStatementIsPresent(t *testing.T) {
	if err := docscheck.CheckInterlacing(shippedCorpus(t)); err != nil {
		t.Errorf("the shipped documentation does not carry the interlacing posture: %v", err)
	}
}

// TestInterlacingPosture_BitesOnAStatementMissingAClause is [AC-15]'s own self-test: the
// check can FAIL, on each clause separately. A documentation check that cannot fail attests
// to nothing, and this one runs over a corpus it did not choose - so "the posture is
// documented" is exactly what it would report if it had quietly stopped looking.
func TestInterlacingPosture_BitesOnAStatementMissingAClause(t *testing.T) {
	if err := docscheck.CheckInterlacing(nil); err == nil {
		t.Error("an empty corpus passed: a check with no documents passes everything")
	}

	full := make([]string, 0, len(docscheck.InterlacingClauses))
	for _, c := range docscheck.InterlacingClauses {
		full = append(full, "It is "+c.Token+".")
	}
	if err := docscheck.CheckInterlacing(writeCorpus(t,
		statementDoc(docscheck.AnchorInterlacing, full))); err != nil {
		t.Fatalf("a document carrying every clause was refused: %v", err)
	}

	// One clause at a time, removed. Each has to be the reason on its own.
	for i, dropped := range docscheck.InterlacingClauses {
		lines := make([]string, 0, len(full)-1)
		for j, l := range full {
			if j != i {
				lines = append(lines, l)
			}
		}
		err := docscheck.CheckInterlacing(writeCorpus(t,
			statementDoc(docscheck.AnchorInterlacing, lines)))
		if err == nil {
			t.Errorf("a statement missing %q passed - that clause is not being checked", dropped.Token)
			continue
		}
		if !strings.Contains(err.Error(), dropped.Token) {
			t.Errorf("the failure for a missing %q does not name it: %v", dropped.Token, err)
		}
	}

	// An anchor with nothing under it is not a statement.
	if err := docscheck.CheckInterlacing(writeCorpus(t,
		statementDoc(docscheck.AnchorInterlacing, nil))); err == nil {
		t.Error("an anchor introducing no text passed as a statement")
	}
}

// TestGuardTableListsEverySkipToken grades [AC-16] of
// S0107-holdfast-interlacing-decision: the documentation check fails unless EVERY token in
// the engine's closed skip vocabulary has a row in the README's guard table.
//
// The vocabulary is read from the engine at runtime and never restated here. That is the
// whole criterion: a grader with the token list inlined passes on the day a third token
// ships undocumented, and a skip token reaches an operator through a stored row, an API
// payload and a metrics label - so one nobody can look up is a verdict with no explanation
// attached to it.
func TestGuardTableListsEverySkipToken(t *testing.T) {
	tokens := engine.SkipVocabulary
	// Anti-vacuity: an empty vocabulary would make the check below true of any table,
	// including one with no rows at all.
	if len(tokens) < 10 {
		t.Fatalf("the engine's skip vocabulary reads as %d token(s) (%v) - it is not being read, so "+
			"nothing below is being checked", len(tokens), tokens)
	}
	if err := docscheck.CheckGuardTable(tokens, shippedCorpus(t)); err != nil {
		t.Errorf("the shipped guard table is incomplete: %v", err)
	}
}

// TestGuardTable_BitesOnAnUndocumentedToken is [AC-16]'s own self-test, and it is the case
// the criterion exists for: a token the engine can record and the table does not name.
func TestGuardTable_BitesOnAnUndocumentedToken(t *testing.T) {
	files := shippedCorpus(t)
	next := append(append([]string(nil), engine.SkipVocabulary...), "a-guard-nobody-documented")
	err := docscheck.CheckGuardTable(next, files)
	if err == nil {
		t.Fatal("a token with no row passed: the table is not being read against the vocabulary, so " +
			"the next token added to the engine would ship undocumented")
	}
	if !strings.Contains(err.Error(), "a-guard-nobody-documented") {
		t.Errorf("the failure does not name the undocumented token: %v", err)
	}
	// And the other two ways this check can stop checking.
	if err := docscheck.CheckGuardTable(next, nil); err == nil {
		t.Error("an empty corpus passed")
	}
	if err := docscheck.CheckGuardTable(nil, files); err == nil {
		t.Error("an empty vocabulary passed: then the table is graded against nothing")
	}
}

// TestInterlacingNonGoalIsRetired grades [AC-17] of
// S0107-holdfast-interlacing-decision: the documentation check fails if the retired non-goal
// - interlaced sources "skipped, not converted" - survives anywhere in the corpus.
//
// It is an ABSENCE, and nothing else in this repository checks one. The presence check above
// cannot see a contradiction standing next to the statement it passes on, and the rename
// guard in scripts/check-pins.sh matches identifiers and says in as many words that prose is
// not matched. A document that still says interlaced sources are never converted tells a
// reader the opposite of what this build does, in the section they read to decide whether to
// trust it with a library.
func TestInterlacingNonGoalIsRetired(t *testing.T) {
	if err := docscheck.CheckRetired(docscheck.RetiredInterlacingNonGoal, shippedCorpus(t)); err != nil {
		t.Errorf("the retired interlacing non-goal survives: %v", err)
	}
}

// TestRetired_BitesOnTheSentenceItRetired is [AC-17]'s own self-test. An absence check is
// the easiest kind to write so that it can never fail, so both halves are driven here: the
// retired claim in each spelling it survived in, and a sentence that legitimately keeps
// "skipped, not converted" about the sources that really are.
func TestRetired_BitesOnTheSentenceItRetired(t *testing.T) {
	for _, doc := range []string{
		"# Fixture\n\n## Non-goals\n\ninterlaced sources are skipped, not converted.\n",
		// The second spelling it survived in, bolded as the shipped document had it: a check
		// defeated by emphasis or by a line wrap would be a check about formatting.
		"# Fixture\n\n**interlaced**, exotic-chroma and `multi-video-stream` sources are\n" +
			"**skipped, not converted**; HDR10 static metadata is preserved.\n",
	} {
		err := docscheck.CheckRetired(docscheck.RetiredInterlacingNonGoal, writeCorpus(t, doc))
		if err == nil {
			t.Errorf("the retired claim passed in this document:\n%s", doc)
			continue
		}
		if !strings.Contains(err.Error(), "skipped, not converted") {
			t.Errorf("the failure does not quote what it found: %v", err)
		}
	}

	// The claim is retired about INTERLACED sources and about nothing else: the same words
	// about the sources that really are skipped and not converted must still pass, or this
	// check would be forcing a false statement out of the documents.
	kept := "# Fixture\n\nexotic-chroma and `multi-video-stream` sources are **skipped, not converted**.\n" +
		"Interlaced sources are deinterlaced on request.\n"
	if err := docscheck.CheckRetired(docscheck.RetiredInterlacingNonGoal, writeCorpus(t, kept)); err != nil {
		t.Errorf("a document that says the surviving half of the claim was refused: %v", err)
	}

	if err := docscheck.CheckRetired(docscheck.RetiredInterlacingNonGoal, nil); err == nil {
		t.Error("an empty corpus passed: a check with no documents reports everything clean")
	}
	if err := docscheck.CheckRetired(docscheck.RetiredClaim{Claim: "nothing"}, shippedCorpus(t)); err == nil {
		t.Error("a claim with no tokens passed: it matches nothing, so it would report every " +
			"document clean whatever they say")
	}
}

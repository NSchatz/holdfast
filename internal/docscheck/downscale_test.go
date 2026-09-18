package docscheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/docscheck"
	"github.com/NSchatz/holdfast/internal/engine"
)

// The shipped documentation's statement about RESOLUTION DOWNSCALING.
//
// This build can scale a replacement down to fewer pixels than its source and then delete
// that source. A reader deciding whether to turn the key on needs every clause in ONE place,
// and a reader deciding whether to trust the tool at all needs to not find the opposite claim
// standing in the document next door - which is exactly what happened to the flat non-goal
// this key replaced, in three spellings across three files.

// TestDownscalePostureStatementIsPresent grades [AC-8]: the README states in its non-goals
// that resolution downscaling is available and off by default, and states the "same content"
// claim as conditional on it being off.
//
// It runs over the corpus this repository actually ships, not a fixture, because the
// obligation is about the documents an operator reads. The DEFAULT half is read off the build
// rather than written here (see TestDownscaleDefaultClause_TracksTheShippedDefault), so this
// cannot pass on a statement that has stopped describing what ships.
func TestDownscalePostureStatementIsPresent(t *testing.T) {
	if err := docscheck.CheckDownscale(shippedCorpus(t), config.MaxHeightDefault()); err != nil {
		t.Errorf("the shipped documentation does not carry the downscaling posture: %v", err)
	}
}

// publishedIdentifiers is the vocabulary this build PUBLISHES: every key a configuration may
// carry and every token a skip may record, read off the build rather than written down here.
//
// A name is documented wherever it is used, so a check counting CLAIMS has to be able to tell
// a name from a claim in a document that has no markup to mark one with. Reading the set off
// the build is the same instrument CheckGuardTable is handed, and for the same reason: a list
// inlined here would stop matching the build the day a key was added, silently.
func publishedIdentifiers(t *testing.T) []string {
	t.Helper()
	ids := append(config.ProfileKnobs(), config.RuleKnobs()...)
	ids = append(ids, engine.SkipVocabulary...)
	// Anti-vacuity in the direction this one can fail: too FEW names read as claims, so an
	// empty or truncated vocabulary would red on the documents that merely name a key.
	if len(ids) < 20 {
		t.Fatalf("the published vocabulary reads as %d name(s) (%v) - it is not being read off the "+
			"build, so a document naming a key would be counted as a claim about it", len(ids), ids)
	}
	return ids
}

// TestDownscaleNonGoalIsStatedInExactlyOneDocument grades [AC-9]'s first half: the posture is
// stated in exactly one shipped document, and it is the one carrying the anchor.
//
// The corpus is every document this repository ships, config.example.yaml included. That file
// is where an operator meets `max_height` and it is not Markdown, so a check admitting one
// file extension would have counted the documents an operator reads and missed the one they
// copy.
func TestDownscaleNonGoalIsStatedInExactlyOneDocument(t *testing.T) {
	if err := docscheck.CheckDownscaleStatedOnce(shippedCorpus(t), publishedIdentifiers(t)); err != nil {
		t.Errorf("the downscaling posture is not stated in exactly one shipped document: %v", err)
	}
}

// TestFlatSameContentClaimIsRetired grades [AC-8]'s second half: no unqualified same-content
// claim survives anywhere in the corpus.
//
// The QUALIFIED form is the opposite claim and must stay - "the replacement is no longer the
// same content as the source" is what both transformation statements say about the keys that
// make it true - so this is scoped to the sentence that made the claim unconditionally.
func TestFlatSameContentClaimIsRetired(t *testing.T) {
	if err := docscheck.CheckRetired(docscheck.RetiredFlatSameContent, shippedCorpus(t)); err != nil {
		t.Errorf("the shipped documentation still makes the unconditional same-content claim: %v", err)
	}
}

// TestDownscalePosture_BitesOnAStatementMissingAClause is [AC-8]'s own self-test: the check
// can FAIL, on each clause separately. A documentation check that cannot fail attests to
// nothing, and this one runs over a corpus it did not choose - so "the posture is documented"
// is exactly what it would report if it had quietly stopped looking.
func TestDownscalePosture_BitesOnAStatementMissingAClause(t *testing.T) {
	const shipped = 0
	if err := docscheck.CheckDownscale(nil, shipped); err == nil {
		t.Error("an empty corpus passed: a check with no documents passes everything")
	}

	clauses := append(append([]docscheck.Clause(nil), docscheck.DownscaleClauses...),
		docscheck.DownscaleDefaultClause(shipped))
	full := make([]string, 0, len(clauses))
	for _, c := range clauses {
		full = append(full, "It is "+c.Token+".")
	}
	if err := docscheck.CheckDownscale(writeCorpus(t,
		statementDoc(docscheck.AnchorDownscale, full)), shipped); err != nil {
		t.Fatalf("a document carrying every clause was refused: %v", err)
	}

	for i, dropped := range clauses {
		lines := make([]string, 0, len(full)-1)
		for j, l := range full {
			if j != i {
				lines = append(lines, l)
			}
		}
		err := docscheck.CheckDownscale(writeCorpus(t,
			statementDoc(docscheck.AnchorDownscale, lines)), shipped)
		if err == nil {
			t.Errorf("a statement missing %q passed - that clause is not being checked", dropped.Token)
			continue
		}
		if !strings.Contains(err.Error(), dropped.Token) {
			t.Errorf("the failure for a missing %q does not name it: %v", dropped.Token, err)
		}
	}

	// An anchor nobody wrote, and an anchor with nothing under it, are two different repairs.
	if err := docscheck.CheckDownscale(writeCorpus(t,
		"# Fixture\n\nProse with no anchor at all.\n"), shipped); err == nil {
		t.Error("a corpus carrying no anchor passed, so the statement could simply be deleted")
	}
	if err := docscheck.CheckDownscale(writeCorpus(t,
		statementDoc(docscheck.AnchorDownscale, nil)), shipped); err == nil {
		t.Error("an anchor with nothing under it passed, which is an anchor and not a statement")
	}

	// TWO documents carrying the anchor is its own failure, and it is the one this check adds
	// over the two anchored-statement checks beside it.
	twice := writeCorpus(t,
		statementDoc(docscheck.AnchorDownscale, full),
		statementDoc(docscheck.AnchorDownscale, full))
	err := docscheck.CheckDownscale(twice, shipped)
	if err == nil {
		t.Error("two documents carrying the anchor passed: the posture would then be two statements " +
			"to keep true, and one of them stops being true silently")
	} else if !strings.Contains(err.Error(), "exactly ONE") {
		t.Errorf("the failure for a duplicated statement does not say why one is the rule: %v", err)
	}
}

// TestDownscaleDefaultClause_TracksTheShippedDefault grades [AC-9]'s second half: the check
// fails when the single statement stops matching the shipped default.
//
// This is the mutation that matters and it cannot be performed by editing the document: the
// clause the statement owes is DERIVED from the default, so shipping a ceiling moves the
// token and the README that still says "off by default" no longer carries it. The test drives
// that by handing the check a different shipped default, which is exactly what a build with a
// different default would hand it.
func TestDownscaleDefaultClause_TracksTheShippedDefault(t *testing.T) {
	if got := docscheck.DownscaleDefaultClause(0).Token; got != "off by default" {
		t.Errorf("with no ceiling shipped the statement owes %q, got %q", "off by default", got)
	}
	if got := docscheck.DownscaleDefaultClause(1080).Token; !strings.Contains(got, "1080") {
		t.Errorf("with a ceiling of 1080 shipped the owed token does not name it: %q", got)
	}

	// The shipped corpus passes against the default this build actually ships...
	corpus := shippedCorpus(t)
	if err := docscheck.CheckDownscale(corpus, config.MaxHeightDefault()); err != nil {
		t.Fatalf("the shipped documentation does not match the shipped default: %v", err)
	}
	// ...and reds against any other, because the statement would then be describing a build
	// nobody ships. That is what "stops matching the shipped default" means, and it is the
	// half a check holding its own copy of the number could never report.
	err := docscheck.CheckDownscale(corpus, 1080)
	if err == nil {
		t.Fatal("the shipped documentation passed as a description of a build that ships a 1080 " +
			"ceiling, so the statement is not graded against the default at all")
	}
	if !strings.Contains(err.Error(), "1080") {
		t.Errorf("the failure does not name the default it was graded against: %v", err)
	}
}

// TestDownscaleStatedOnce_BitesOnASecondDocument is [AC-9]'s self-test: a second document
// restating the posture is caught, and a document merely NAMING a key or a field is not.
//
// The second half is as load-bearing as the first. `downscaled` and `downscale_scaler` are
// API fields and `downscale-unacknowledged` is a guard token; each is documented wherever it
// is used, and a check that read a field name as a restatement would force the reference
// documentation to describe this build in words that avoid its own vocabulary.
func TestDownscaleStatedOnce_BitesOnASecondDocument(t *testing.T) {
	ids := publishedIdentifiers(t)
	if err := docscheck.CheckDownscaleStatedOnce(nil, ids); err == nil {
		t.Error("an empty corpus passed: a check with no documents passes everything")
	}

	statement := statementDoc(docscheck.AnchorDownscale,
		[]string{"Resolution downscaling is available and off by default."})

	if err := docscheck.CheckDownscaleStatedOnce(writeCorpus(t, statement,
		"# Reference\n\nProse about encoders and floors.\n"), ids); err != nil {
		t.Fatalf("one statement beside a document that says nothing about it was refused: %v", err)
	}

	// A second document restating the posture, in a spelling the first does not use.
	err := docscheck.CheckDownscaleStatedOnce(writeCorpus(t, statement,
		"# Elsewhere\n\nThis build downscales nothing.\n"), ids)
	if err == nil {
		t.Error("a second document restating the posture passed - which is precisely how the flat " +
			"non-goal this key replaced came to survive in three files")
	} else if !strings.Contains(err.Error(), "downscales nothing") {
		t.Errorf("the failure does not quote the restatement it found: %v", err)
	}

	// A document NAMING the identifiers is not restating anything.
	if err := docscheck.CheckDownscaleStatedOnce(writeCorpus(t, statement,
		"# Reference\n\nThe `downscaled` field, the `downscale_scaler` beside it, and the\n"+
			"`downscale-unacknowledged` guard, each null on a job that scaled nothing.\n"), ids); err != nil {
		t.Errorf("a reference document naming the fields and the guard token was read as a second "+
			"statement of the posture, so the reference cannot use this build's own vocabulary: %v", err)
	}

	// The one document making the claim has to be the one carrying the anchor, or a reader
	// following the link arrives somewhere the statement is not.
	err = docscheck.CheckDownscaleStatedOnce(writeCorpus(t,
		"# Elsewhere\n\nResolution downscaling is available and off by default.\n",
		statementDoc(docscheck.AnchorDownscale, []string{"Some unrelated prose."})), ids)
	if err == nil {
		t.Error("the claim in a document with no anchor passed, so the anchor a reader is sent to " +
			"and the statement that exists can be two different things")
	}
}

// TestDownscaleStatedOnce_ReachesTheExampleConfiguration is [AC-9]'s grader half where it was
// actually breached: the second document is not Markdown.
//
// The example configuration has no code spans to mark a key name with, so both halves have to
// hold at once or the check is useless there. A sentence in it that restates the posture must
// RED, and a commented-out key line naming `max_height` or `downscale_acknowledged` - which is
// what that file is FOR - must not.
func TestDownscaleStatedOnce_ReachesTheExampleConfiguration(t *testing.T) {
	ids := publishedIdentifiers(t)
	statement := statementDoc(docscheck.AnchorDownscale,
		[]string{"Resolution downscaling is available and off by default."})

	// The example as this repository ships it: the keys written, the posture deferred to.
	keys := "# A root may override max_height and downscale_acknowledged. See the README.\n" +
		"# max_height: 0\n# downscale_acknowledged: false\n"
	if err := docscheck.CheckDownscaleStatedOnce(
		append(writeCorpus(t, statement), writeExample(t, keys)), ids); err != nil {
		t.Errorf("an example configuration that WRITES the keys and states nothing was read as a "+
			"second statement of the posture: %v\nThe keys have to be writable in the file they are "+
			"written in, or the check forces the example to describe a build in words that avoid the "+
			"build's own vocabulary", err)
	}

	// The sentence that shipped in it, which is the retired non-goal stated a second time.
	err := docscheck.CheckDownscaleStatedOnce(
		append(writeCorpus(t, statement),
			writeExample(t, "# they select which files the rule applies TO and are not an output\n"+
				"# ceiling - nothing here downscales anything.\n")), ids)
	if err == nil {
		t.Fatal("a restatement in the example configuration passed: the check counts Markdown only, " +
			"so the one document an operator COPIES can contradict the one they read")
	}
	if !strings.Contains(err.Error(), "downscales anything") {
		t.Errorf("the failure does not quote the restatement it found: %v", err)
	}
	if !strings.Contains(err.Error(), corpus.ExampleConfigName) {
		t.Errorf("the failure does not name the file to edit: %v", err)
	}
}

// writeExample writes one fixture document under the shipped example's own name, so a case
// about that file is graded through a file with that file's name and extension.
func writeExample(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), corpus.ExampleConfigName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write the fixture example: %v", err)
	}
	return p
}

// TestFlatSameContentClaim_BitesOnTheRetiredSentence is [AC-8]'s second half proving it can
// fail: the retired claim is caught where it survives, and the QUALIFIED sentence beside it -
// which is the opposite claim and the one both transformation statements make - is not.
func TestFlatSameContentClaim_BitesOnTheRetiredSentence(t *testing.T) {
	err := docscheck.CheckRetired(docscheck.RetiredFlatSameContent, writeCorpus(t,
		"# Fixture\n\nholdfast performs codec-only, same-content re-encoding and nothing else.\n"))
	if err == nil {
		t.Error("the retired unconditional same-content claim passed, so it could return to any " +
			"document beside the key that contradicts it")
	}

	if err := docscheck.CheckRetired(docscheck.RetiredFlatSameContent, writeCorpus(t,
		"# Fixture\n\nSet max_height and the replacement is no longer the same content as the "+
			"source.\n")); err != nil {
		t.Errorf("the QUALIFIED statement was read as the retired claim, which would refuse the "+
			"sentence this posture is required to carry: %v", err)
	}
}

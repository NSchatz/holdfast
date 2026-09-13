package docscheck

// S0085 - the swap-metadata statement.
//
// Same shape as the reverse-proxy rule and here for the same reason. docs/docker.md has
// always said what holdfast NEEDS from the filesystem (a `user:` that owns the media, write
// access to the directories); it has never said what a swap CHANGES about the file it
// publishes. Four facts decide whether a deployment's permissions, ownership and library
// ordering survive a pass over the library, and none of them is discoverable from the
// tool's own output - so they are owed in the shipped documentation, and an obligation
// nothing enforces is one that quietly lapses.

import (
	"strings"
	"testing"
)

// metadataSentence is one sentence per swap-metadata clause, each carrying that clause's
// token and no other. Keyed on the token the check looks for, so a token that changes
// without its fixture changing does not silently keep passing.
var metadataSentence = map[string]string{
	"carries the source's mode": "The replacement carries the source's mode, whatever umask holdfast runs under.",
	"only where holdfast is privileged": "Ownership is carried only where holdfast is privileged to carry it; " +
		"a rootless container publishes the replacement under its own uid and says so once per run.",
	"preserve_mtime": "The modification time is carried from the source unless preserve_mtime is false, " +
		"which defaults to true.",
	"acls and xattrs are not carried": "ACLs and xattrs are not carried: the replacement gets whatever " +
		"the filesystem gives a newly created file.",
}

// metadataBlock builds a swap-metadata statement carrying every clause EXCEPT the tokens
// named. metadataBlock() with no arguments is the statement that satisfies the rule, and
// every fixture in this package carries one so that each negative test reports the problem
// it is about rather than that problem plus a missing metadata statement.
func metadataBlock(omit ...string) string {
	dropped := map[string]bool{}
	for _, o := range omit {
		dropped[o] = true
	}
	block := "<a id=\"" + AnchorSwapMetadata + "\"></a>\n\n"
	for _, c := range SwapMetadataClauses {
		if dropped[c.Token] {
			continue
		}
		s, ok := metadataSentence[c.Token]
		if !ok {
			panic("docscheck test: no fixture sentence carries the clause token " + c.Token)
		}
		block += s + "\n\n"
	}
	return block
}

// The anchor is not there at all.
func TestCheck_SwapMetadataAnchorMissingFails(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() + "\n# Volumes\n\nMount your library at /media.\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the missing swap-metadata anchor, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], AnchorSwapMetadata) || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the statement as MISSING by anchor: %q", problems[0])
	}
}

// The anchor is present with nothing under it. An anchor with no text is not a statement.
func TestCheck_SwapMetadataAnchorWithNothingUnderItIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() +
			"\n<a id=\"" + AnchorSwapMetadata + "\"></a>\n\n## Next section\n\nunrelated\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the empty-statement problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "nothing follows it") || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the bare anchor as a MISSING statement: %q", problems[0])
	}
}

// One clause at a time: a statement that says everything except one of the four is not the
// statement that was owed, and the failure names the token it wanted plus the clause in
// words.
func TestCheck_SwapMetadataStatementMissingAClauseIsReportedMissing(t *testing.T) {
	for _, c := range SwapMetadataClauses {
		t.Run(c.Token, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md": residualWindowBlock() + postureBlock() + "\n" + metadataBlock(c.Token),
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly the missing-clause problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], c.Token) {
				t.Errorf("the problem does not name the token it wanted (%q): %q", c.Token, problems[0])
			}
			if !strings.Contains(problems[0], "MISSING") {
				t.Errorf("the problem does not report the clause as MISSING: %q", problems[0])
			}
		})
	}
}

// The four clauses must be carried by ONE statement. A document saying the mode is carried
// and a different document saying ACLs are not has not told a deploying operator, in one
// place, what a swap does to their files.
func TestCheck_SwapMetadataClausesMustBeCarriedByOneStatement(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.md": residualWindowBlock() + postureBlock(),
		"b.md": metadataBlock("preserve_mtime", "acls and xattrs are not carried"),
		"c.md": "<a id=\"" + AnchorSwapMetadata + "\"></a>\n\n" +
			metadataSentence["preserve_mtime"] + "\n\n" + metadataSentence["acls and xattrs are not carried"] + "\n",
	})
	problems := check(t, dir)
	if len(problems) == 0 {
		t.Fatal("clauses split across two documents were accepted as one statement")
	}
}

// The positive direction over a minimal corpus, so the negatives above are not passing for
// the trivial reason that nothing can.
func TestCheck_SwapMetadataStatementWithEveryClausePasses(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() + "\n" + metadataBlock(),
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a metadata statement carrying every clause was reported as failing: %v", problems)
	}
}

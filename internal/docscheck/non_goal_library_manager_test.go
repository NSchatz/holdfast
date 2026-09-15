package docscheck

// S0105 - the library-manager non-goal.
//
// Same shape as the reverse-proxy and swap-metadata rules and here for the same reason.
// holdfast runs inside a library another tool manages, and "not a library manager" names
// the CATEGORY excluded: a reader holding a renaming problem, a folder-layout problem or a
// duplicate problem cannot tell from a category which of them this tool leaves alone. So
// each excluded capability is its own clause, the reason the line is where it is has a
// clause of its own, and so does what to reach for instead. An obligation nothing enforces
// is one that quietly lapses, and this one would lapse the first time somebody read the
// boundary as a scope preference rather than as a consequence of what can be proved.
//
// Every test here names the acceptance criterion it grades.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nonGoalSentence is one sentence per library-manager clause, each carrying every token
// that clause owes and no token of any other. Keyed on the clause's own Token, so a token
// that changes without its fixture changing panics rather than silently keeping a test
// green.
var nonGoalSentence = map[string]string{
	"renaming to a scheme":   "There is no renaming to a scheme.",
	"moving between folders": "There is no moving between folders.",
	"folder organisation":    "There is no folder organisation.",
	"metadata fetch":         "There is no metadata fetch.",
	"duplicate detection":    "There is no duplicate detection.",
	"no deletion":            "There is no deletion of anything but a source whose verified replacement passed.",
	"filesystem mutation": "Each of them is a filesystem mutation whose correctness cannot be established by " +
		"comparing two video files, so the verify gate has nothing to say about it.",
	"instead": "Use Plex, Jellyfin or an arr tool for that work instead.",
}

// nonGoalBlock builds a library-manager statement carrying every clause EXCEPT the tokens
// named. nonGoalBlock() with no arguments is the statement that satisfies the rule, and
// every fixture in this package carries one so that each negative test reports the problem
// it is about rather than that problem plus a missing non-goal statement.
func nonGoalBlock(omit ...string) string {
	dropped := map[string]bool{}
	for _, o := range omit {
		dropped[o] = true
	}
	block := "<a id=\"" + AnchorNonGoalLibraryManager + "\"></a>\n\n"
	for _, c := range NonGoalLibraryManagerClauses {
		if dropped[c.Token] {
			continue
		}
		s, ok := nonGoalSentence[c.Token]
		if !ok {
			panic("docscheck test: no fixture sentence carries the clause token " + c.Token)
		}
		block += s + "\n\n"
	}
	return block
}

// satisfiedExceptNonGoal is every other rule's statement, so a test about this rule reports
// this rule and nothing else.
func satisfiedExceptNonGoal() string {
	return residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks() + differentiatorBlock()
}

// TestCheck_AC1_NonGoalStatementWithEveryClausePasses is AC-1 over a MINIMAL corpus: a
// statement carrying all eight clauses satisfies the gate, so every negative below is not
// passing for the trivial reason that nothing can. AC-1 over the SHIPPED corpus is
// TestShippedDocumentation_AC1_SatisfiesEveryRuleTheGateOwns.
func TestCheck_AC1_NonGoalStatementWithEveryClausePasses(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptNonGoal() + "\n" + nonGoalBlock(),
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a non-goal statement carrying every clause was reported as failing: %v", problems)
	}
}

// TestCheck_AC4_NonGoalStatementMissingAClauseIsReportedMissing is AC-4, one case per
// clause: a statement that says everything except one of the eight is not the statement
// that was owed. The problem has to name the missing clause IN PROSE and not only the token
// it looked for, because a reader of the failure is being asked to write a sentence and a
// token tells them which string to paste instead.
func TestCheck_AC4_NonGoalStatementMissingAClauseIsReportedMissing(t *testing.T) {
	for _, c := range NonGoalLibraryManagerClauses {
		t.Run(c.Token, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md": satisfiedExceptNonGoal() + "\n" + nonGoalBlock(c.Token),
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly the missing-clause problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], c.Clause) {
				t.Errorf("the problem does not name the missing clause in prose (%q): %q", c.Clause, problems[0])
			}
			if !strings.Contains(problems[0], "MISSING") {
				t.Errorf("the problem does not report the clause as MISSING: %q", problems[0])
			}
		})
	}
}

// TestCheck_AC4_NonGoalClausesMustBeCarriedByOneStatement is the other half of AC-4: the
// clauses are all present in the corpus and no ONE statement carries them. A document that
// lists the excluded capabilities and a different document that gives the reason and the
// alternative has not told a reader, in one place, which of their problems this tool does
// not solve - and a check that summed the clauses across the corpus would call that
// arrangement complete.
func TestCheck_AC4_NonGoalClausesMustBeCarriedByOneStatement(t *testing.T) {
	split := []string{"filesystem mutation", "instead"}
	rest := "<a id=\"" + AnchorNonGoalLibraryManager + "\"></a>\n\n"
	for _, token := range split {
		rest += nonGoalSentence[token] + "\n\n"
	}
	dir := writeCorpus(t, map[string]string{
		"a.md": satisfiedExceptNonGoal(),
		"b.md": nonGoalBlock(split...),
		"c.md": rest,
	})
	problems := check(t, dir)
	if len(problems) != len(split) {
		t.Fatalf("want one problem per clause the best statement does not carry, got %d: %v",
			len(problems), problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, "b.md") {
			t.Errorf("the problem does not name the strongest near-miss (b.md): %q", p)
		}
	}
}

// TestCheck_AC5_NonGoalAnchorMissingFromEveryDocumentIsReportedMissing is AC-5's first
// case: no shipped document carries the anchor at all. There is no near-miss file to name
// here and the message says so rather than naming one - see notes.md, which records that
// reading.
func TestCheck_AC5_NonGoalAnchorMissingFromEveryDocumentIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptNonGoal() + "\n# Non-goals\n\nIt transcodes files in a library other tools manage.\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the missing non-goal anchor, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], AnchorNonGoalLibraryManager) || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the statement as MISSING by anchor: %q", problems[0])
	}
}

// TestCheck_AC5_NonGoalAnchorFollowedByAHeadingIsReportedMissing is AC-5's second case: the
// anchor is there and the next thing in the document is a heading, so nothing follows it.
// The failure names the file, because a reader is being asked to write a statement into one
// document and "the documentation is wrong" is not something they can act on.
func TestCheck_AC5_NonGoalAnchorFollowedByAHeadingIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptNonGoal() +
			"\n<a id=\"" + AnchorNonGoalLibraryManager + "\"></a>\n\n## Quick start\n\nunrelated\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the empty-statement problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "nothing follows it") || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the bare anchor as a MISSING statement: %q", problems[0])
	}
	if !strings.Contains(problems[0], "docs.md") {
		t.Errorf("the problem does not name the file the bare anchor is in: %q", problems[0])
	}
}

// TestCheck_AC5_NonGoalAnchorFollowedByAnotherAnchorIsReportedMissing is AC-5's third case.
// Two anchors back to back introduce nothing: the second ends the first one's text before
// any of it exists, which is the failure a marker somebody left behind actually looks like.
func TestCheck_AC5_NonGoalAnchorFollowedByAnotherAnchorIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() + metadataBlock() + differentiatorBlock() +
			"\n<a id=\"" + AnchorNonGoalLibraryManager + "\"></a>\n" + agreeingBlocks(),
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the empty-statement problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "nothing follows it") || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the bare anchor as a MISSING statement: %q", problems[0])
	}
	if !strings.Contains(problems[0], "docs.md") {
		t.Errorf("the problem does not name the file the bare anchor is in: %q", problems[0])
	}
}

// TestShippedDocumentation_AC1_EveryNonGoalTokenIsCarriedByTheREALTEXT is the mutation proof
// for this clause table against the documentation the repository actually ships.
//
// The fixture tests above prove the RULE and prove nothing about the shipped file: a table
// whose tokens had drifted away from the real prose would pass every one of them, because a
// corpus built from the table always matches the table. So this removes the anchor, and
// then redacts each token in turn, out of the real corpus and demands the gate red. A
// redaction that changed nothing is itself a failure - it means the shipped text never
// carried that token, so the clause is satisfied on paper and not in the tree.
func TestShippedDocumentation_AC1_EveryNonGoalTokenIsCarriedByTheREALTEXT(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}

	// Which shipped file carries the anchor is the implementer's choice and is not fixed,
	// so find it rather than assuming it.
	carrier := ""
	for _, f := range files {
		st, err := findStatement(f, AnchorNonGoalLibraryManager)
		if err != nil {
			t.Fatalf("findStatement(%s, %s): %v", f, AnchorNonGoalLibraryManager, err)
		}
		if st.File != "" && st.Present() {
			carrier = f
			break
		}
	}
	if carrier == "" {
		t.Fatalf("no shipped file carries a present statement for %q", AnchorNonGoalLibraryManager)
	}

	mutations := []struct{ name, mutated, problem string }{}
	original, err := os.ReadFile(carrier)
	if err != nil {
		t.Fatal(err)
	}
	mutations = append(mutations, struct{ name, mutated, problem string }{
		name:    "the anchor is removed from the shipped text",
		mutated: strings.Replace(string(original), `<a id="`+AnchorNonGoalLibraryManager+`"></a>`, "", 1),
		problem: AnchorNonGoalLibraryManager,
	})
	for _, c := range NonGoalLibraryManagerClauses {
		for _, token := range append([]string{c.Token}, c.Also...) {
			mutations = append(mutations, struct{ name, mutated, problem string }{
				name:    "the shipped statement stops carrying " + token,
				mutated: loose(token).ReplaceAllString(string(original), "REDACTED"),
				problem: token,
			})
		}
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if m.mutated == string(original) {
				t.Fatalf("the mutation changed nothing in %s - the shipped text never carried %q, "+
					"so the clause is satisfied on paper and not in the tree", carrier, m.problem)
			}

			// The WHOLE corpus is copied: a statement hiding in another shipped file would
			// keep the gate green, and that is the failure this has to catch rather than miss.
			dir := t.TempDir()
			for _, f := range files {
				body := []byte(m.mutated)
				if f != carrier {
					body, err = os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
				}
				dst := filepath.Join(dir, rel(root, f))
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dst, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			problems := check(t, dir)
			if len(problems) == 0 {
				t.Fatalf("breaking the SHIPPED %s statement in %s was not caught - the gate does "+
					"not read the real text", AnchorNonGoalLibraryManager, carrier)
			}
			if !strings.Contains(strings.Join(problems, "\n"), m.problem) {
				t.Errorf("the problem does not name %q: %v", m.problem, problems)
			}
		})
	}
}

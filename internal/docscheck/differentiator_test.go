package docscheck

// S0108 - the differentiator statement, and the widening guard on it.
//
// Same shape as the four clause rules that precede it, plus one thing none of them has. The
// README asks a reader to choose holdfast over four named tools and offers them one claim to
// choose on; every other rule here fails on what a statement STOPS saying, and a competitive
// claim fails the other way. It gets wider. So this rule is checked in both directions, and
// the second direction is the one the anchor was added for.
//
// Every test here names the acceptance criterion it grades (testing T1), each criterion
// graded by `go test ./internal/docscheck/...` has at least one and each unhappy path the
// spec names has one (T2), and every one of them runs the real Check over a real corpus on
// disk - nothing here stands in a double for docscheck itself (T3).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// differentiatorSentence is one sentence per differentiator clause, each carrying every
// token that clause owes and no token of any other. Keyed on the clause's own Token, so a
// token that changes without its fixture changing panics rather than silently keeping a
// test green.
var differentiatorSentence = map[string]string{
	"default-on":                     "The verify gate is default-on.",
	"layered":                        "It is layered, and every layer runs rather than the first one that answers.",
	"fails closed":                   "It fails closed.",
	"structural parity":              "The first of them is structural parity.",
	"full decode-integrity":          "The second of them is full decode-integrity.",
	"min_vmaf":                       "The mean floor is min_vmaf.",
	"vmaf_min_pool":                  "The worst-frame floor is vmaf_min_pool.",
	"vmaf_min_chroma":                "The chroma floor is vmaf_min_chroma.",
	"cannot be measured is rejected": "An output that cannot be measured is rejected rather than assumed good.",
}

// differentiatorBlock builds a differentiator statement carrying every clause EXCEPT the
// tokens named. differentiatorBlock() with no arguments is the statement that satisfies the
// rule, and every fixture in this package carries one so that each negative test reports the
// problem it is about rather than that problem plus a missing differentiator statement.
func differentiatorBlock(omit ...string) string {
	dropped := map[string]bool{}
	for _, o := range omit {
		dropped[o] = true
	}
	block := "<a id=\"" + AnchorDifferentiator + "\"></a>\n\n"
	for _, c := range DifferentiatorClauses {
		if dropped[c.Token] {
			continue
		}
		s, ok := differentiatorSentence[c.Token]
		if !ok {
			panic("docscheck test: no fixture sentence carries the clause token " + c.Token)
		}
		block += s + "\n\n"
	}
	return block
}

// satisfiedExceptDifferentiator is every other rule's statement, so a test about this rule
// reports this rule and nothing else.
func satisfiedExceptDifferentiator() string {
	return residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks() + nonGoalBlock()
}

// TestCheck_AC6_DifferentiatorStatementWithEveryClausePasses is AC-6 over a MINIMAL corpus:
// a statement carrying all nine clauses satisfies the gate, so every negative below is not
// passing for the trivial reason that nothing can. AC-6 over the SHIPPED corpus is
// TestShippedDocumentation_AC1_SatisfiesEveryRuleTheGateOwns.
func TestCheck_AC6_DifferentiatorStatementWithEveryClausePasses(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock(),
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a differentiator statement carrying every clause was reported as failing: %v", problems)
	}
}

// TestCheck_AC6_TheClauseTableCarriesEveryClauseTheCriterionNames is AC-6 asked of the TABLE
// rather than of a corpus. The rule is only as strong as what it enumerates, and a table
// that quietly lost the chroma floor would pass every other test in this file - each of them
// builds its fixture FROM the table, so a corpus built from a weakened table matches it.
//
// The chroma clause is the one this pins hardest. It is a real profile threshold
// (`vmaf_min_chroma`, logged in cmd/holdfast/main.go and stored by internal/store/migrate.go)
// and it is the floor a statement naming the mean and the worst frame drops while still
// reading complete.
func TestCheck_AC6_TheClauseTableCarriesEveryClauseTheCriterionNames(t *testing.T) {
	want := []string{
		"default-on",
		"layered",
		"fails closed",
		"structural parity",
		"full decode-integrity",
		"min_vmaf",
		"vmaf_min_pool",
		"vmaf_min_chroma",
		"cannot be measured is rejected",
	}
	have := map[string]bool{}
	for _, c := range DifferentiatorClauses {
		have[c.Token] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("DifferentiatorClauses does not carry the clause AC-6 names by the token %q - "+
				"the rule cannot enforce a clause it does not list", w)
		}
	}
}

// TestCheck_AC7_DifferentiatorAnchorMissingFromEveryDocumentIsReportedMissing is AC-7's first
// unhappy path: no shipped document carries the anchor at all. There is no near-miss file to
// name, and the message says so rather than naming one.
func TestCheck_AC7_DifferentiatorAnchorMissingFromEveryDocumentIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptDifferentiator() +
			"\n# Why another transcoder\n\nIt verifies before it replaces.\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the missing differentiator anchor, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], AnchorDifferentiator) || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the statement as MISSING by anchor: %q", problems[0])
	}
}

// TestCheck_AC7_DifferentiatorAnchorFollowedByAHeadingIsReportedMissing is AC-7's second
// unhappy path: the anchor is there and the next thing is a heading, so nothing follows it.
// The failure names the file, because a reader is being asked to write a statement into one
// document and "the documentation is wrong" is not something they can act on.
func TestCheck_AC7_DifferentiatorAnchorFollowedByAHeadingIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptDifferentiator() +
			"\n<a id=\"" + AnchorDifferentiator + "\"></a>\n\n## Non-goals\n\nunrelated\n",
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

// TestCheck_AC7_DifferentiatorAnchorFollowedByAnotherAnchorIsReportedMissing is AC-7's third
// unhappy path. Two anchors back to back introduce nothing: the second ends the first one's
// text before any of it exists, which is what a marker somebody left behind looks like.
func TestCheck_AC7_DifferentiatorAnchorFollowedByAnotherAnchorIsReportedMissing(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks() +
			"\n<a id=\"" + AnchorDifferentiator + "\"></a>\n" + nonGoalBlock(),
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the empty-statement problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "nothing follows it") || !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the bare anchor as a MISSING statement: %q", problems[0])
	}
}

// TestCheck_AC7_DifferentiatorStatementMissingAClauseIsReportedMissing is AC-7's fourth
// unhappy path, one case per clause: a statement that says everything except one of the nine
// is not the statement that was owed, the gate reports ONE problem for it, and the problem
// names the file and the clause IN PROSE - a reader of the failure is being asked to write a
// sentence, and a token alone tells them which string to paste instead.
func TestCheck_AC7_DifferentiatorStatementMissingAClauseIsReportedMissing(t *testing.T) {
	for _, c := range DifferentiatorClauses {
		t.Run(c.Token, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock(c.Token),
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly one problem for the one missing clause, got %d: %v",
					len(problems), problems)
			}
			if !strings.Contains(problems[0], "docs.md") {
				t.Errorf("the problem does not name the file: %q", problems[0])
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

// TestCheck_AC7_HalfOfACompoundClauseIsStillAMissingClause is the rest of AC-7's clause case.
// "Layered" is one obligation with two halves - that the gate is layered AND that every layer
// runs rather than the first one that answers - and a single token for it would grade
// whichever half was picked and let the other be dropped in silence.
func TestCheck_AC7_HalfOfACompoundClauseIsStillAMissingClause(t *testing.T) {
	weakened := strings.Replace(differentiatorBlock(),
		differentiatorSentence["layered"], "It is layered.", 1)
	if weakened == differentiatorBlock() {
		t.Fatal("the fixture sentence for the layered clause did not change - this proves nothing")
	}
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptDifferentiator() + "\n" + weakened,
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the missing-half problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "every layer runs") {
		t.Errorf("the problem does not name the half that is actually absent: %q", problems[0])
	}
}

// TestCheck_AC7_DifferentiatorClausesMustBeCarriedByOneStatement is AC-7's last unhappy path:
// the clauses are all present in the corpus and no ONE statement carries them. A document
// that says the gate is default-on and a different document that names the floors has not
// told a reader, in one place, what they are being asked to choose on - and a check that
// summed the clauses across the corpus would call that arrangement complete.
func TestCheck_AC7_DifferentiatorClausesMustBeCarriedByOneStatement(t *testing.T) {
	split := []string{"vmaf_min_chroma", "cannot be measured is rejected"}
	rest := "<a id=\"" + AnchorDifferentiator + "\"></a>\n\n"
	for _, token := range split {
		rest += differentiatorSentence[token] + "\n\n"
	}
	dir := writeCorpus(t, map[string]string{
		"a.md": satisfiedExceptDifferentiator(),
		"b.md": differentiatorBlock(split...),
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

// TestCheck_AC8_AUniquenessAssertionInTheStatementFails is AC-8, one case per guarded phrase.
// The statement is otherwise complete and the prose is plausible - it has just started
// claiming something nobody here has checked, which is how an honest narrow claim becomes a
// dishonest wide one.
func TestCheck_AC8_AUniquenessAssertionInTheStatementFails(t *testing.T) {
	for _, phrase := range DifferentiatorUniqueness {
		t.Run(phrase, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock() +
					"holdfast is " + phrase + " does this.\n\n",
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly the widening problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], phrase) {
				t.Errorf("the problem does not name the phrase it fired on (%q): %q", phrase, problems[0])
			}
			if !strings.Contains(problems[0], "docs.md") {
				t.Errorf("the problem does not name the file: %q", problems[0])
			}
		})
	}
}

// TestCheck_AC8_TheGuardedPhraseFloorIsNeverUndercut is the other half of AC-8's floor. The
// criterion names five phrases the guard must carry and lets the implementer add more; a
// table that shipped fewer would pass every test above, because each of them iterates the
// table it is meant to be checking.
func TestCheck_AC8_TheGuardedPhraseFloorIsNeverUndercut(t *testing.T) {
	floor := []string{
		"the only tool",
		"the only transcoder",
		"the only one that",
		"no other tool",
		"uniquely",
	}
	have := map[string]bool{}
	for _, p := range DifferentiatorUniqueness {
		have[strings.ToLower(p)] = true
	}
	for _, f := range floor {
		if !have[f] {
			t.Errorf("DifferentiatorUniqueness does not guard %q, which AC-8 names as a floor the "+
				"guard may exceed and may not undercut. It has: %v", f, DifferentiatorUniqueness)
		}
	}
}

// TestCheck_AC8_TheTwoDisavowalsTheRepositoryAlreadyCarriesStayGreen is the half of AC-8 that
// keeps the guard from eating the sentences that make the claim honest.
//
// Both strings contain a guarded phrase and neither asserts it: the section heading says
// holdfast is NOT the only tool that verifies, and the closing disclaimer says the narrow
// claim is truer than the wide one. A guard that failed on those would be satisfiable by
// DELETING them, which is the exact outcome it exists to prevent - so each is asserted green
// on its own, not only in the pair the shipped text happens to carry.
func TestCheck_AC8_TheTwoDisavowalsTheRepositoryAlreadyCarriesStayGreen(t *testing.T) {
	for _, allowed := range DifferentiatorUniquenessAllowed {
		t.Run(allowed, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock() +
					allowed + "\n\n",
			})
			if problems := check(t, dir); len(problems) != 0 {
				t.Fatalf("a string that contains a guarded phrase while disowning it was reported "+
					"as a widening: %v", problems)
			}
		})
	}
	t.Run("both together, wrapped the way Markdown wraps them", func(t *testing.T) {
		dir := writeCorpus(t, map[string]string{
			"docs.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock() +
				"We are not the only tool\nthat verifies before it replaces. That is the whole claim,\n" +
				"and it is narrower and truer than \"the only one\nthat checks\".\n\n",
		})
		if problems := check(t, dir); len(problems) != 0 {
			t.Fatalf("the two disavowals were reported as widenings once a line wrap fell inside "+
				"them - the allowance is matching raw bytes rather than normalised text: %v", problems)
		}
	})
}

// TestCheck_AC8_AWideningInASecondOccurrenceIsNotHiddenByTheNarrowCopy is the reading this
// rule takes where AC-8 says "the anchored statement", singular, and the corpus can carry
// two. The clause check is satisfied by ANY occurrence, so a second document could assert the
// wide claim and hide behind the narrow copy that keeps the clauses green. A widening guard
// with a second copy as its exit is not a guard, so this one reads every occurrence.
func TestCheck_AC8_AWideningInASecondOccurrenceIsNotHiddenByTheNarrowCopy(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.md": satisfiedExceptDifferentiator() + "\n" + differentiatorBlock(),
		"b.md": "<a id=\"" + AnchorDifferentiator + "\"></a>\n\nholdfast is the only tool that checks.\n",
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the widening in the second occurrence, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "b.md") {
		t.Errorf("the problem does not name the occurrence that widened: %q", problems[0])
	}
}

// TestShippedDocumentation_AC6_EveryDifferentiatorTokenIsCarriedByTheREALTEXT is the mutation
// proof for this clause table against the documentation the repository actually ships.
//
// Every fixture test above builds its corpus FROM the table, so a table whose tokens had
// drifted away from the real prose would pass all of them: a corpus built from the table
// always matches the table. So this removes the anchor, then redacts each token in turn, out
// of the REAL corpus, and demands the gate red. A redaction that changed nothing is itself a
// failure - it means the shipped text never carried that token, so the clause is satisfied on
// paper and not in the tree.
func TestShippedDocumentation_AC6_EveryDifferentiatorTokenIsCarriedByTheREALTEXT(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	carrier := differentiatorCarrier(t, files)
	original, err := os.ReadFile(carrier)
	if err != nil {
		t.Fatal(err)
	}

	mutations := []struct{ name, mutated, problem string }{{
		name:    "the anchor is removed from the shipped text",
		mutated: strings.Replace(string(original), `<a id="`+AnchorDifferentiator+`"></a>`, "", 1),
		problem: AnchorDifferentiator,
	}}
	for _, c := range DifferentiatorClauses {
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
			problems := check(t, mutatedCorpus(t, root, files, carrier, m.mutated))
			if len(problems) == 0 {
				t.Fatalf("breaking the SHIPPED %s statement in %s was not caught - the gate does "+
					"not read the real text", AnchorDifferentiator, carrier)
			}
			if !strings.Contains(strings.Join(problems, "\n"), m.problem) {
				t.Errorf("the problem does not name %q: %v", m.problem, problems)
			}
		})
	}
}

// TestShippedDocumentation_AC8_WideningTheREALTEXTIsCaught is the same anti-vacuity argument
// for the guard rather than for the table. A guard whose allowances had swallowed the whole
// statement, or whose phrases matched nothing the real prose could ever contain, would pass
// the shipped-documentation test for the same reason an anchor that matches nothing does: "no
// problems" is also what a check looking at the wrong thing returns. So each guarded phrase is
// INSERTED into the shipped statement in turn and the gate is required to red on it.
func TestShippedDocumentation_AC8_WideningTheREALTEXTIsCaught(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	carrier := differentiatorCarrier(t, files)
	original, err := os.ReadFile(carrier)
	if err != nil {
		t.Fatal(err)
	}

	// The sentence the widening is appended to. It is the last sentence of the shipped
	// statement, so the insertion lands inside the statement rather than after the heading
	// that ends it - and if that sentence is ever reworded this test says so rather than
	// quietly inserting nothing.
	const tail = `truer than "the only one that checks".`
	if !strings.Contains(string(original), tail) {
		t.Fatalf("%s no longer carries %q, so this test has nowhere to insert a widening and is "+
			"proving nothing - re-anchor it on the statement's current last sentence", carrier, tail)
	}

	for _, phrase := range DifferentiatorUniqueness {
		t.Run(phrase, func(t *testing.T) {
			mutated := strings.Replace(string(original), tail,
				tail+" holdfast is "+phrase+" does this.", 1)
			problems := check(t, mutatedCorpus(t, root, files, carrier, mutated))
			if len(problems) != 1 {
				t.Fatalf("want exactly the widening problem against the real corpus, got %d: %v",
					len(problems), problems)
			}
			if !strings.Contains(problems[0], phrase) {
				t.Errorf("the problem does not name the phrase it fired on (%q): %q", phrase, problems[0])
			}
		})
	}
}

// differentiatorCarrier is the shipped file carrying a present differentiator statement.
// Which one that is remains the implementer's choice and is not fixed by this package, so it
// is found rather than assumed.
func differentiatorCarrier(t *testing.T, files []string) string {
	t.Helper()
	for _, f := range files {
		st, err := findStatement(f, AnchorDifferentiator)
		if err != nil {
			t.Fatalf("findStatement(%s, %s): %v", f, AnchorDifferentiator, err)
		}
		if st.File != "" && st.Present() {
			return f
		}
	}
	t.Fatalf("no shipped file carries a present statement for %q", AnchorDifferentiator)
	return ""
}

// mutatedCorpus copies the WHOLE shipped corpus to a temp root with one file replaced. The
// whole of it, because a statement hiding in another shipped file would keep the gate green,
// and that is the failure these tests have to catch rather than miss.
func mutatedCorpus(t *testing.T, root string, files []string, carrier, mutated string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		body := []byte(mutated)
		if f != carrier {
			var err error
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
	return dir
}

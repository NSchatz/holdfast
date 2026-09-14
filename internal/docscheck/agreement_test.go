package docscheck

// S0102 - the three anchors held to the AGREEMENT rule, and the rule itself.
//
// The four older anchors enforce that a statement EXISTS in what the repository ships:
// any occurrence that satisfies the rule satisfies it. That is the right rule for an
// obligation about the corpus and the wrong one for an argument the documents restate,
// because the second copy is free to drop a clause and a reader who lands on it is missing
// the clause with nothing to tell them so. These three are therefore checked at EVERY
// occurrence.
//
// Every test here names the acceptance criterion it grades.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// agreeingSentence is one sentence per clause of the three agreeing rules, each carrying
// every token that clause owes and no token of any other. Keyed on the clause's own
// Token, so a token that changes without its fixture changing panics rather than silently
// keeping a test green.
var agreeingSentence = map[string]string{
	"no source is mutated until a replacement has passed every gate": "No source is mutated until a replacement has passed every gate.",
	"same-directory temp": "The encode goes to a same-directory temp, and the swap is the only filesystem " +
		"mutation there is: an atomic same-filesystem rename onto the source.",
	"leaves the source byte-for-byte intact": "Any gate failure discards the temp and leaves the source byte-for-byte intact.",
	"durable rather than merely atomic": "The rename is made durable rather than merely atomic: holdfast fsyncs the parent " +
		"directory after it, and when that fsync fails the source is kept.",

	"bounds the worst frame": "The gate bounds the worst frame and not only the average, because an average hides " +
		"local damage.",
	"min_vmaf": "The mean floor min_vmaf, the pool floor vmaf_min_pool and the chroma floor vmaf_min_chroma " +
		"are all on by default.",
	"luma-only vmaf model cannot see colour": "The luma-only VMAF model cannot see colour at all, which is why the chroma floor " +
		"is separate.",
	"cannot be measured is rejected": "An output that cannot be measured is rejected rather than assumed good.",

	"explicit null":                          "An unreadable figure is reported as an explicit null and never as 0.",
	"a zero would claim the ledger is empty": "A zero would claim the ledger is empty beside rows the caller can already see.",
	"the rows still ship":                    "The rows still ship, so one unreadable figure never costs an operator the records.",
}

// ruleBlock builds one agreeing rule's statement carrying every clause EXCEPT the tokens
// named. ruleBlock(r) satisfies the rule.
func ruleBlock(r Rule, omit ...string) string {
	dropped := map[string]bool{}
	for _, o := range omit {
		dropped[o] = true
	}
	block := "<a id=\"" + r.Anchor + "\"></a>\n\n"
	for _, c := range r.Clauses {
		if dropped[c.Token] {
			continue
		}
		s, ok := agreeingSentence[c.Token]
		if !ok {
			panic("docscheck test: no fixture sentence carries the clause token " + c.Token)
		}
		block += s + "\n\n"
	}
	return block
}

// agreeingBlocks satisfies all three agreeing rules at once. Every fixture in this package
// carries it for the same reason each of them carries a posture block: a test about one
// rule must report that rule's problem and not a pile of unrelated absences.
func agreeingBlocks() string {
	out := ""
	for _, r := range AgreeingRules {
		out += ruleBlock(r)
	}
	return out
}

// ruleFor is the rule an anchor names.
func ruleFor(t *testing.T, anchor string) Rule {
	t.Helper()
	for _, r := range AgreeingRules {
		if r.Anchor == anchor {
			return r
		}
	}
	t.Fatalf("no agreeing rule carries the anchor %q", anchor)
	return Rule{}
}

// TestCheck_AC1_EveryNewAnchorIntroducesAStatement is AC-1 over a minimal corpus: the
// three anchors satisfy the gate when each introduces real text, so every negative below
// is not passing for the trivial reason that nothing can. AC-1 over the SHIPPED corpus is
// TestShippedDocumentation_AC1_SatisfiesEveryRuleTheGateOwns.
func TestCheck_AC1_EveryNewAnchorIntroducesAStatement(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs.md": residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks(),
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a corpus carrying all three statements in full was reported as failing: %v", problems)
	}
}

// TestCheck_AC5_AnchorWithNothingUnderItIsReportedMissing is AC-5: the anchor is there and
// the next line is another anchor or a heading, so nothing follows it. An anchor with no
// text is not a statement, and a gate that accepted one would pass a document that says
// nothing at all while looking anchored.
func TestCheck_AC5_AnchorWithNothingUnderItIsReportedMissing(t *testing.T) {
	for _, r := range AgreeingRules {
		t.Run(r.Anchor, func(t *testing.T) {
			others := ""
			for _, o := range AgreeingRules {
				if o.Anchor != r.Anchor {
					others += ruleBlock(o)
				}
			}
			dir := writeCorpus(t, map[string]string{
				"docs.md": residualWindowBlock() + postureBlock() + metadataBlock() + others +
					"<a id=\"" + r.Anchor + "\"></a>\n\n## Next section\n\nunrelated\n",
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly the empty-statement problem, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], "nothing follows it") || !strings.Contains(problems[0], "MISSING") {
				t.Errorf("the problem does not report the bare anchor as a MISSING statement: %q", problems[0])
			}
		})
	}
}

// TestCheck_AC6_AnchorMissingFromEveryDocumentIsNamed is AC-6: the anchor is in no shipped
// document at all, and the failure has to name which one, because "the documentation is
// wrong" is not something an author can act on.
func TestCheck_AC6_AnchorMissingFromEveryDocumentIsNamed(t *testing.T) {
	for _, r := range AgreeingRules {
		t.Run(r.Anchor, func(t *testing.T) {
			others := ""
			for _, o := range AgreeingRules {
				if o.Anchor != r.Anchor {
					others += ruleBlock(o)
				}
			}
			dir := writeCorpus(t, map[string]string{
				"docs.md": residualWindowBlock() + postureBlock() + metadataBlock() + others,
			})
			problems := check(t, dir)
			if len(problems) != 1 {
				t.Fatalf("want exactly the missing anchor, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], r.Anchor) || !strings.Contains(problems[0], "MISSING") {
				t.Errorf("the problem does not report the statement as MISSING by anchor: %q", problems[0])
			}
		})
	}
}

// TestCheck_AC7_ASecondOccurrenceThatDropsAClauseFails is AC-7, and the check that does
// not exist for any older anchor: two documents carry the same anchor, the first says
// everything and the second drops one clause. Under the any-occurrence rule the strong
// copy would hide the weak one and the gate would report green over two documents that
// disagree. The failure names the file that disagrees and the clause it does not carry.
func TestCheck_AC7_ASecondOccurrenceThatDropsAClauseFails(t *testing.T) {
	for _, r := range AgreeingRules {
		for _, c := range r.Clauses {
			t.Run(r.Anchor+"/"+c.Token, func(t *testing.T) {
				dir := writeCorpus(t, map[string]string{
					"a.md": residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks(),
					"b.md": ruleBlock(r, c.Token),
				})
				problems := check(t, dir)
				if len(problems) != 1 {
					t.Fatalf("want exactly the disagreeing occurrence, got %d: %v", len(problems), problems)
				}
				if !strings.Contains(problems[0], "b.md") {
					t.Errorf("the problem does not name the file that disagrees: %q", problems[0])
				}
				if !strings.Contains(problems[0], c.Token) {
					t.Errorf("the problem does not name the clause token it wanted (%q): %q", c.Token, problems[0])
				}
			})
		}
	}
}

// TestCheck_AC7_AFaithfulSecondCopyPasses is the other half of AC-7. The rule is about
// what each occurrence SAYS, never about how many there are: a document that repeats the
// statement faithfully is a document that agrees, and a gate that failed it would make
// restating an argument impossible rather than making it honest.
func TestCheck_AC7_AFaithfulSecondCopyPasses(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"a.md": residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks(),
		"b.md": agreeingBlocks(),
	})
	if problems := check(t, dir); len(problems) != 0 {
		t.Fatalf("a faithful second copy of every statement was reported as failing: %v", problems)
	}
}

// TestCheck_AC8_TheOlderAnchorsKeepTheAnyOccurrenceRule is AC-8, in the direction that
// matters: the agreement rule must NOT have been applied to the four anchors that predate
// it. Each of them gets a second occurrence that drops a clause - exactly the corpus the
// test above requires to be red for the three new anchors - and the gate must stay green,
// because those four are satisfied by any occurrence and tightening them was never in
// scope. A change that added a check and removed one would fail here.
func TestCheck_AC8_TheOlderAnchorsKeepTheAnyOccurrenceRule(t *testing.T) {
	weak := map[string]string{
		AnchorLocal:        "<a id=\"" + AnchorLocal + "\"></a>\n\nA shorter restatement of the local window.\n\n",
		AnchorNetwork:      "<a id=\"" + AnchorNetwork + "\"></a>\n\nA restatement that never mentions the client's cache.\n\n",
		AnchorReverseProxy: postureBlock(ReverseProxyClauses[0].Token),
		AnchorSwapMetadata: metadataBlock(SwapMetadataClauses[0].Token),
	}
	for anchor, second := range weak {
		t.Run(anchor, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"a.md": residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks(),
				"b.md": second,
			})
			if problems := check(t, dir); len(problems) != 0 {
				t.Fatalf("the agreement rule has been applied to %q, which AC-8 puts out of bounds: %v",
					anchor, problems)
			}
		})
	}
}

// TestShippedDocumentation_AC2AC3AC4_EveryClauseTokenIsCarriedByTheREALTEXT is the
// mutation proof for the three clause tables against the documentation this repository
// actually ships.
//
// The fixture tests above prove the RULE and prove nothing about the shipped file: a table
// whose tokens had drifted away from the real prose would pass every one of them, because
// a corpus built from the table always matches the table. So this redacts each token in
// turn out of the real corpus and demands the gate red. A redaction that changed nothing is
// itself a failure - it means the shipped text never carried that token, so AC-2 to AC-4
// are satisfied on paper and not in the tree.
//
// Whitespace inside a token is matched as any run of whitespace, because in Markdown a
// single newline inside a paragraph is a space: a literal matcher would report "changed
// nothing" for a token the author happened to wrap across two lines, which is a fact about
// the line wrap and not about the documentation.
func TestShippedDocumentation_AC2AC3AC4_EveryClauseTokenIsCarriedByTheREALTEXT(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}

	for _, r := range AgreeingRules {
		for _, c := range r.Clauses {
			for _, token := range append([]string{c.Token}, c.Also...) {
				t.Run(r.Anchor+"/"+token, func(t *testing.T) {
					carrier := ""
					for _, f := range files {
						st, err := findStatement(f, r.Anchor)
						if err != nil {
							t.Fatalf("findStatement(%s, %s): %v", f, r.Anchor, err)
						}
						if st.File != "" && st.Present() {
							carrier = f
							break
						}
					}
					if carrier == "" {
						t.Fatalf("no shipped file carries a present statement for %q", r.Anchor)
					}

					original, err := os.ReadFile(carrier)
					if err != nil {
						t.Fatal(err)
					}
					mutated := loose(token).ReplaceAllString(string(original), "REDACTED")
					if mutated == string(original) {
						t.Fatalf("redacting %q changed nothing in %s - the shipped text never carried it, "+
							"so the clause table is satisfied on paper and not in the tree", token, carrier)
					}

					// The WHOLE corpus is copied: a statement hiding in another shipped file
					// would keep the gate green, and that is the failure this has to catch
					// rather than miss.
					dir := t.TempDir()
					for _, f := range files {
						body := []byte(mutated)
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
						t.Fatalf("redacting %q from the SHIPPED %s statement in %s was not caught - "+
							"the gate does not read the real text", token, r.Anchor, carrier)
					}
					if !strings.Contains(strings.Join(problems, "\n"), token) {
						t.Errorf("the problem does not name the token that was redacted (%q): %v", token, problems)
					}
				})
			}
		}
	}
}

// loose matches a clause token allowing any run of whitespace where the token has a space,
// case-insensitively - the same two liberties normalize() takes before a token is looked
// for, so a redaction removes exactly what the gate would have found.
func loose(token string) *regexp.Regexp {
	parts := strings.Fields(token)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?i)` + strings.Join(parts, `\s+`))
}

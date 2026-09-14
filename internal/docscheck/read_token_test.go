package docscheck

// The three clauses the read token adds to the reverse-proxy statement, and the
// corpus-wide rule that stops a second document contradicting it.
//
// The clause table in docscheck_test.go already generates a missing-clause case per
// entry, so these three are covered by construction. They are ALSO written out by name
// here, because the table proves the rule and a named case proves the CLAUSE: a token
// quietly dropped from ReverseProxyClauses would take its generated case with it and the
// table would keep passing with one fewer obligation, which is exactly the failure mode
// the fixed anchors in this package exist to prevent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// satisfiedExceptPosture is every other rule this package owns, satisfied, so a test about
// one reverse-proxy clause reports that clause and nothing else.
func satisfiedExceptPosture() string {
	return residualWindowBlock() + metadataBlock() + agreeingBlocks() + nonGoalBlock() + differentiatorBlock()
}

// missingClauseFails is the shape all three cases below share: a corpus whose
// reverse-proxy statement carries every clause EXCEPT this one must be reported, and the
// report must name the token so an author learns what to write.
func missingClauseFails(t *testing.T, token string) {
	t.Helper()
	dir := writeCorpus(t, map[string]string{
		"docs.md": satisfiedExceptPosture() + "\n" + postureBlock(token),
	})
	problems := check(t, dir)
	if len(problems) != 1 {
		t.Fatalf("dropping %q produced %d problems, want exactly the missing-clause one: %v",
			token, len(problems), problems)
	}
	if !strings.Contains(problems[0], token) {
		t.Errorf("the problem does not name the dropped token %q: %q", token, problems[0])
	}
	if !strings.Contains(problems[0], "MISSING") {
		t.Errorf("the problem does not report the clause as MISSING: %q", problems[0])
	}
}

// The clause that says what the read token BUYS. Without it the statement still describes
// a daemon whose read API cannot be gated at all, which was true before this key and is
// the reading an operator would act on.
func TestCheck_ReverseProxyStatementWithoutTheDefenceInDepthClauseFails(t *testing.T) {
	missingClauseFails(t, "defence in depth")
}

// The clause that says what the read token does NOT buy. This is the one whose absence is
// actively dangerous: an operator who reads "the read API is gated" and nothing else
// concludes the surface is gated, takes forward auth off the route, and leaves the
// dashboard page - which is still served to anyone - behind nothing at all.
func TestCheck_ReverseProxyStatementWithoutThePageStillOpenClauseFails(t *testing.T) {
	missingClauseFails(t, "the page is still served with no credential")
}

// The clause that says what the half-gated state LOOKS like. Without it the first thing
// an operator meets after setting the key is a dashboard that renders and stays empty,
// with nothing in the documentation to tell them that is the expected shape rather than a
// broken deployment.
func TestCheck_ReverseProxyStatementWithoutThePageDataClauseFails(t *testing.T) {
	missingClauseFails(t, "the page loads but its data does not")
}

// TestCheck_AStaleUnauthenticatedClaimFails is the corpus-wide rule, which is not anchored
// and is not about one statement being incomplete. It is about two shipped documents
// DISAGREEING: one says the read API can be gated, another - written earlier and never
// revisited - still says it carries no authentication of its own, and an operator reads
// whichever they opened first. Both are shipped, so both are the documentation.
func TestCheck_AStaleUnauthenticatedClaimFails(t *testing.T) {
	good := satisfiedExceptPosture() + "\n" + postureBlock()

	for _, tc := range []struct{ name, stale string }{
		{
			name:  "unauthenticated",
			stale: "The read endpoints are unauthenticated and always will be.\n",
		},
		{
			name:  "no authentication",
			stale: "The read endpoints and the dashboard carry no authentication of their own.\n",
		},
		{
			name: "the claim wrapped across lines, because a line break is not a change of meaning",
			stale: "The read endpoints and the dashboard carry no\nauthentication of their own, " +
				"so the proxy is all there is.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs.md":  good,
				"stale.md": "# Elsewhere\n\n" + tc.stale,
			})
			problems := check(t, dir)
			if len(problems) == 0 {
				t.Fatal("a document still calling the read surface unauthenticated was accepted " +
					"beside one that says the key gates it")
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, ReadTokenKey) {
				t.Errorf("the problem does not name %s, so it does not say what to write: %v", ReadTokenKey, problems)
			}
			if !strings.Contains(joined, "stale.md") {
				t.Errorf("the problem does not name the offending file: %v", problems)
			}
		})
	}
}

// The anti-vacuity half, in both of the directions that matter. The rule must not refuse
// the claim itself - it is TRUE on the shipped default and saying so plainly is the whole
// point of the posture statement - only a claim that leaves out which key changes it.
func TestCheck_AnUnauthenticatedClaimNamingTheKeyPasses(t *testing.T) {
	good := satisfiedExceptPosture() + "\n" + postureBlock()

	t.Run("the same claim naming the key is accepted", func(t *testing.T) {
		dir := writeCorpus(t, map[string]string{
			"docs.md": good,
			"other.md": "# Elsewhere\n\nWith " + ReadTokenKey + " unset the read endpoints are " +
				"unauthenticated; set it and they require a bearer credential.\n",
		})
		if problems := check(t, dir); len(problems) != 0 {
			t.Fatalf("a claim that names the key was rejected: %v", problems)
		}
	})

	t.Run("a fenced sample is not a statement", func(t *testing.T) {
		dir := writeCorpus(t, map[string]string{
			"docs.md": good,
			"other.md": "# Elsewhere\n\nRun the probe:\n\n```bash\n" +
				"curl -sS http://holdfast:8080/api/summary   # unauthenticated on the default\n```\n",
		})
		if problems := check(t, dir); len(problems) != 0 {
			t.Fatalf("a fenced command sample was treated as a statement about behaviour: %v", problems)
		}
	})

	t.Run("a corpus with no such claim at all is clean", func(t *testing.T) {
		dir := writeCorpus(t, map[string]string{"docs.md": good})
		if problems := check(t, dir); len(problems) != 0 {
			t.Fatalf("a corpus making no unauthenticated claim was reported: %v", problems)
		}
	})
}

// TestShippedDocumentation_HasNoStaleUnauthenticatedClaim is the rule pointed at the real
// tree, which is the assertion the criterion is actually about: no document this
// repository SHIPS is left saying the read API cannot be gated.
//
// The mutation half proves the assertion can fail against that real text: reinstating the
// pre-S0101 sentence in the shipped README is caught.
func TestShippedDocumentation_HasNoStaleUnauthenticatedClaim(t *testing.T) {
	root := repoRoot(t)
	files, err := Corpus(root)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	problems, err := StaleUnauthenticatedClaims(files)
	if err != nil {
		t.Fatalf("StaleUnauthenticatedClaims: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("a shipped document still calls the read surface unauthenticated without naming %s:\n  %s",
			ReadTokenKey, strings.Join(problems, "\n  "))
	}

	// Anti-vacuity: the walk found the corpus rather than nothing.
	if len(files) < 5 {
		t.Fatalf("the corpus is %d files, which is too few to be this repository's documentation", len(files))
	}

	t.Run("the pre-S0101 sentence reinstated in a shipped file is caught", func(t *testing.T) {
		readme := filepath.Join(root, "README.md")
		original, err := os.ReadFile(readme)
		if err != nil {
			t.Fatal(err)
		}
		// The claim README.md carried before this key existed, put back into the REAL
		// shipped text rather than into a fixture that resembles it.
		mutated := string(original) +
			"\n\nThe bind is the whole of what protects the read endpoints and the dashboard - " +
			"they carry no authentication of their own.\n"
		dst := filepath.Join(t.TempDir(), "README.md")
		if err := os.WriteFile(dst, []byte(mutated), 0o644); err != nil {
			t.Fatal(err)
		}
		problems, err := StaleUnauthenticatedClaims([]string{dst})
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) == 0 {
			t.Fatal("the reinstated sentence was not caught - the rule does not read the real text")
		}
	})
}

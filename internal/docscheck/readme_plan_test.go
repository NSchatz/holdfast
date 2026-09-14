package docscheck

// The README's own half of the plan-versus-dry-run obligation, exercised in BOTH directions.
//
// A check only ever run against documentation that passes proves nothing: it could be
// reading the wrong file, or matching nothing at all, and "no problems" looks identical
// either way. So the shipped README is asserted to PASS - which is what makes `make check`
// red when the statement is later deleted or reworded - and the REAL statement is then
// broken one clause at a time and each break demanded to be caught.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestREADME_StatesPlanVersusDryRun is AC-17 of
// pipeline/active/S0103-holdfast-plan-report/spec.md: the README presents one sentence
// saying what dry_run is and one saying what plan is, and this is the mechanical check that
// asserts both are present, so the pair cannot rot apart from the build.
func TestREADME_StatesPlanVersusDryRun(t *testing.T) {
	problems, err := CheckPlanVersusDryRun(repoReadme(t))
	if err != nil {
		t.Fatalf("checking the README: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("the shipped README no longer states what dry_run is and what plan is:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// TestREADME_TheCheckBitesOnTheREALTEXT is the mutation proof, against the README this
// repository actually ships rather than a fixture that resembles it. Each clause's token is
// removed from a copy of the real file and the check must report exactly that clause.
func TestREADME_TheCheckBitesOnTheREALTEXT(t *testing.T) {
	real, err := os.ReadFile(repoReadme(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range PlanVersusDryRunClauses {
		t.Run(c.Token, func(t *testing.T) {
			broken := removeToken(string(real), c.Token)
			if broken == string(real) {
				t.Fatalf("the token %q does not appear in the shipped README at all, so this check "+
					"has drifted away from the document it grades", c.Token)
			}
			path := filepath.Join(t.TempDir(), "README.md")
			if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
				t.Fatal(err)
			}
			problems, err := CheckPlanVersusDryRun(path)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, p := range problems {
				if strings.Contains(p, c.Token) {
					found = true
				}
			}
			if !found {
				t.Fatalf("a README with %q removed was reported clean of it: %v", c.Token, problems)
			}
		})
	}

	// And the anchor removed entirely, which is what deleting the section produces.
	t.Run("no anchor at all", func(t *testing.T) {
		broken := strings.ReplaceAll(string(real), AnchorPlanVersusDryRun, "some-other-anchor")
		path := filepath.Join(t.TempDir(), "README.md")
		if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		problems, err := CheckPlanVersusDryRun(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], AnchorPlanVersusDryRun) {
			t.Fatalf("a README with no %q anchor was reported clean of it: %v",
				AnchorPlanVersusDryRun, problems)
		}
	})

	// An anchor with nothing under it is not a statement, and must not read as one.
	t.Run("an anchor with nothing under it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "README.md")
		body := "# holdfast\n\n<a id=\"" + AnchorPlanVersusDryRun + "\"></a>\n\n## Next heading\n\nText.\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		problems, err := CheckPlanVersusDryRun(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "nothing follows it") {
			t.Fatalf("a bare anchor was accepted as a statement: %v", problems)
		}
	})
}

// removeToken deletes every case-insensitive occurrence of token from s, leaving the rest of
// the document intact - so the fixture differs from the shipped file in exactly the one way
// the case is about.
func removeToken(s, token string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(strings.ToLower(rest), strings.ToLower(token))
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(token):]
	}
}

// repoReadme is the shipped README, located from the repository root so this test grades the
// same file whatever directory it was invoked from.
func repoReadme(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "README.md")
}

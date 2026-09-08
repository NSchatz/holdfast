//go:build regress0046

package main

// Regression probes written by S0065-holdfast-release-gate-f28, which closed F28: the
// release-cutting step is inside the job that holds the grant and is counted as an
// irreversible act, but it holds no ROLE, so nothing held the values its `env:` hands its
// program. Same harness as every regress_0046_*_test.go on this branch (the helpers live in
// regress_0046_F22_test.go): take the REAL committed inputs, apply ONE mutation a maintainer
// could plausibly make, and report what the committed gate says about it.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0065 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`. Nothing here publishes, reaches a network, or
// executes a workflow step other than the planning script the gate itself runs; every
// mutation lands in t.TempDir().
//
// These are ALL graded on `IMAGE`, deliberately. `VERSION` and `PRERELEASE` on the same step
// are held by the same change, but holding `VERSION` is not what closes this hole: the notes
// a release publishes tell every reader which image to pull, and it is the IMAGE half that a
// maintainer would pin while every other assertion in this gate stayed green.
//
// Four of the five reds and the one pass are the same five questions handed.go already
// answers for a role step - a literal, a traceable-but-different source, an absent value, an
// unfollowable expression, and the honest other direction. They are asked again HERE because
// the answers came from a different table (acts.go) and a check that is only known to work on
// the table it was written for is a check nobody has measured.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// The env block of the step that cuts the release, and the two anchors every mutation below
// hangs off. `IMAGE:` followed by `PRERELEASE:` occurs once in the file: the promotion and the
// resolution both put `VERSION:` after their `IMAGE:`, so this anchor cannot land on them.
const (
	l65CutImage      = "          IMAGE: ${{ needs.build.outputs.image }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n"
	l65BuildOutImage = "      image: ${{ steps.plan.outputs.image }}\n"
)

// l65Refusal runs the gate over a fixture and returns what it REFUSED WITH, not merely
// whether it refused. The gate writes its refusals to os.Stderr (main.go's `bad`) and its
// green notes to `out`, so the shared harness's captured buffer carries none of the refusal
// text - and one of these probes grades the refusal itself, because a gate that reds without
// naming the step and the key sends a maintainer to read the gate's source.
func l65Refusal(t *testing.T, root string) (red bool, refusals string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	drained := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		drained <- sb.String()
	}()

	var stdout bytes.Buffer
	g := &gate{root: root, out: &stdout}
	runErr := g.run()
	if runErr != nil {
		// A hard refusal is still a refusal, and it is reported the way main() reports it.
		g.bad("%v", runErr)
	}

	os.Stderr = orig
	_ = w.Close()
	refusals = <-drained
	_ = r.Close()

	t.Logf("gate stdout:\n%s", stdout.String())
	t.Logf("gate refusals:\n%s", refusals)
	return g.failed, refusals
}

// l65MustRedNaming asserts the gate refused AND that one single refusal names every one of
// the things a maintainer needs in order to act on it without opening the gate's source.
// Spread across two different refusals they would not be an explanation, so the test looks
// for them on one line.
func l65MustRedNaming(t *testing.T, root, what string, needles ...string) {
	t.Helper()
	red, refusals := l65Refusal(t, root)
	if !red {
		t.Fatalf("the gate PASSED %s", what)
	}
	for _, line := range strings.Split(refusals, "\n") {
		hits := 0
		for _, n := range needles {
			if strings.Contains(line, n) {
				hits++
			}
		}
		if hits == len(needles) {
			return
		}
	}
	t.Fatalf("the gate refused %s, but no single refusal names all of %q. It said:\n%s", what, needles, refusals)
}

// --- A. A LITERAL IMAGE ON THE RELEASE CUT ------------------------------------------------
//
// The whole of F28, isolated to the value that matters. `VERSION` and `PRERELEASE` are left
// naming the planning logic's outputs, so nothing here can be satisfied by holding the
// version: a maintainer who pins the image reference publishes release notes telling every
// reader to pull an image this run never gated, on this release and on every one after it,
// while the order holds, the grant holds and the runbook names the act.
//
// The refusal has to name the STEP and the KEY. "Something about the release definition is
// wrong" is what sends the next person to read release-shape-gate's source instead of the two
// characters they have to change.
func TestRegress0065_A_ALiteralImageOnTheReleaseCutIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
		"          IMAGE: ghcr.io/nschatz/holdfast\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
	l65MustRedNaming(t, root,
		"a GitHub release whose notes name a hard-coded image reference rather than the one this run gated",
		"github-release", "IMAGE")
}

// --- B. A DIFFERENT SOURCE THE GATE CAN NONETHELESS TRACE ---------------------------------
//
// `github.repository` is where the planning logic DERIVES the image from - it lowercases it
// and puts `ghcr.io/` in front - so this is the mutation that looks defensible and reads
// almost right. Deny-by-default says it reds: the value has to BE the planning logic's own
// output, and naming the raw event field performs the act on a reference the planning logic
// never produced, with no `ghcr.io/` on it and the owner's case preserved.
//
// The point of this probe is not that the string differs. It is that the gate decides the
// SOURCE, so it would red here even if the two strings happened to coincide.
func TestRegress0065_B_ATraceableButDifferentSourceIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
		"          IMAGE: ${{ github.repository }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
	l8MustRed(t, root, "a GitHub release whose image is the workflow event's own repository rather than the planning logic's image output")
}

// --- C. NO IMAGE AT ALL -------------------------------------------------------------------
//
// Absence must not be graded as satisfaction. `gh release create` would then interpolate an
// empty `${IMAGE}` into the notes and publish "Container image: `:v0.1.2`" - a reference
// nobody can pull - having passed a gate that only looked at the values that were there.
func TestRegress0065_C_NoImageAtAllIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
		"          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
	l8MustRed(t, root, "a GitHub release cut with no IMAGE declared at any of the three levels")
}

// --- D. AN EXPRESSION THE GATE CANNOT FOLLOW ----------------------------------------------
//
// Both shapes the criterion names, because they fail in the trace at different places and an
// unfollowable expression must read CLOSED either way. Neither is an error at release time:
// GitHub hands over the empty string, so the release publishes notes naming nothing at all.
func TestRegress0065_D_AnUnfollowableExpressionIsRefused(t *testing.T) {
	t.Run("an output of a job this one does not need", func(t *testing.T) {
		root := l8Fixture(t)
		l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
			"          IMAGE: ${{ needs.nobody.outputs.image }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
		l65MustRedNaming(t, root,
			"a GitHub release whose image reads the output of a job this workflow does not define",
			"github-release", "CANNOT TRACE")
	})

	t.Run("a value an earlier step in this job invented", func(t *testing.T) {
		root := l8Fixture(t)
		l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
			"          IMAGE: ${{ steps.registry-login.outputs.image }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
		l65MustRedNaming(t, root,
			"a GitHub release whose image is an output some earlier step in the publishing job invented",
			"github-release", "CANNOT TRACE")
	})
}

// --- E. THE HONEST OTHER DIRECTION --------------------------------------------------------
//
// Every red above would be satisfied by a check that accepted exactly one spelling of one
// expression, which is a catalogue of permitted strings by another name and is the shape that
// lost eight times over on the other half of this gate. So: the SAME planning output, reached
// through a job output that did not exist a moment ago, must PASS. Nothing about the text is
// familiar to the gate; it follows the workflow's own `needs:` graph to the step holding the
// `plan` role and finds the same output at the end of it.
func TestRegress0065_E_TheSameOutputThroughAnotherJobOutputPasses(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l65BuildOutImage,
		l65BuildOutImage+"      notes_image: ${{ steps.plan.outputs.image }}\n")
	l8Mutate(t, root, ".github/workflows/release.yml", l65CutImage,
		"          IMAGE: ${{ needs.build.outputs.notes_image }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
	l8MustPass(t, root, "a GitHub release reaching the SAME planning output through a job output this gate has never seen")
}

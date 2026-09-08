//go:build regress0046

package main

// Second half of the impl-gate ordinal 8 probes for S0046-holdfast-release-2.
// Helpers live in regress_0046_F22_test.go; same package, same build tag, same
// rule - a probe whose mutation the gate ACCEPTS is a hole, and every probe that
// asserts a catch has a control beside it.

import "testing"

// F23b. The sharper half of F23, and the one that is a true fail-OPEN rather
// than a false red: handing the resolution step `VERSION: latest` makes
// scripts/resolve-compose-image.sh compare the compose reference's digest
// against `${IMAGE}:latest` - which the gate has ALREADY proved offline (A9) is
// that same compose reference. The comparison becomes a tautology, so A11's
// "SHALL fail the release if that reference does not resolve to the digest the
// run just gated" can never fail again.
func TestRegress0046L8_F23b_ResolutionAgainstItselfIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: ${{ needs.build.outputs.version }}\n        run: ./scripts/resolve-compose-image.sh\n",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: latest\n        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "an A11 resolution that compares the compose reference against itself and can never fail")
}

// F22-P2. The route that IS closed, which is what makes F22 precisely a hole in
// the env VALUES and not in the invocation accounting: handing the re-smoke its
// reference as a `run:` field instead reds, because that field is one the role
// never declared.
func TestRegress0046L8_F22P2_TheSameReferenceAsARunFieldIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        run: ./scripts/release-resmoke.sh\n",
		"        run: ./scripts/release-resmoke.sh ghcr.io/nschatz/holdfast:latest\n")
	l8MustRed(t, root, "the floating reference handed to the re-smoke as a run: field")
}

// F24-P1. The CONTROL that says why F24 is an advisory rather than a demand for
// a route. A step in the same job that simply overwrites the Makefile neuters
// `make check` just as completely, and nothing structural can see it: deciding
// that means reading an ordinary `run:` script, which
// conductor-ruling-a6-capability-route.md retires outright. F24, F24b and this
// probe are one class - an earlier step in the job sabotaging a later one - and
// the honest answer to all three is a stated residue, not a reader.
func TestRegress0046L8_F24P1_AnEarlierStepRewritingTheMakefileIsEquallyInvisible(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n",
		"      - name: patch the build\n        run: printf 'check:\\n\\t@true\\n' > Makefile\n\n      - name: the full gate (make check)\n")
	l8MustRed(t, root, "a release whose Makefile is rewritten by an earlier step so that `check` does nothing")
}

//go:build regress0046

package main

// Regression probes over the release-shape gate. Same harness as every other
// regress_0046_*_test.go (helpers live in regress_0046_F22_test.go): take the REAL
// committed inputs, apply ONE mutation, and report what the committed gate says. A
// probe whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L10 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`. Nothing here publishes, reaches a
// network, or executes a workflow step other than the planning script the gate itself
// runs; every mutation lands in t.TempDir(). An `_F<n>_` name pins a defect, an `_S<n>_`
// name is a control the gate must catch or must accept, so this is a measurement rather
// than a selection.

import "testing"

// --- S11. Anti-vacuity: the unmutated fixture passes --------------------------

func TestRegress0046L10_S11_BaselinePasses(t *testing.T) {
	l8MustPass(t, l8Fixture(t), "the committed release definition")
}

// --- F27. THE PLANNING OUTPUT ITSELF IS NEVER HELD TO THE TAG -----------------
//
// handed.go requires a role step's object to BE the planning logic's own output, and
// nothing then holds that OUTPUT to the ref the run was triggered on. The gate plans its
// real-release shape with sampleTag `v0.1.0`, the version this repository has actually
// published, so a plan step pinning the published version to that literal produces
// exactly the outputs the gate expects and is invisible to every assertion.
//
// The mutation keeps the major-zero refusal and the pre-release derivation keyed on the
// real ref, so those checks still pass; only the value that is pushed, re-smoked,
// promoted and resolved is pinned. Every later tag would then republish v0.1.0, which
// semver says must never be modified, and move `:latest` onto it.
func TestRegress0046L10_F27_APlanThatPinsTheVersionToTheSampleTagIsInvisible(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          prerelease=false\n          case \"$version\" in *-*) prerelease=true ;; esac\n",
		"          prerelease=false\n          case \"$version\" in *-*) prerelease=true ;; esac\n"+
			"          if [ \"$publish\" = \"true\" ] && [ \"$prerelease\" = \"false\" ]; then\n"+
			"            version=\"v0.1.0\"\n"+
			"          fi\n")
	l8MustRed(t, root, "a planning step that pins the published version to the literal v0.1.0, so every later tag republishes the July release and moves :latest onto it")
}

// --- F28. AN IRREVERSIBLE ACT OUTSIDE THE ROLE TABLE HANDS UNHELD VALUES ------
//
// `github-release` cuts the GitHub release: it is in the job that holds the grant, it is
// counted as an act by checkIrreversibleActsLiveBehindAGrant, and docs/release.md names
// it. It is not one of the eight declared ROLES, so nothing holds the values its `env:`
// hands it - the property every other publishing step has.
func TestRegress0046L10_F28_TheReleaseCuttingStepsHandedValuesAreNotHeld(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          VERSION: ${{ needs.build.outputs.version }}\n          IMAGE: ${{ needs.build.outputs.image }}\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n",
		"          VERSION: v0.1.0\n          IMAGE: ghcr.io/nschatz/holdfast\n          PRERELEASE: ${{ needs.build.outputs.prerelease }}\n")
	l8MustRed(t, root, "a GitHub release cut against literals rather than against the values this run produced")
}

// --- S12. The trace really requires the PLAN's output, not any traceable one ---
//
// `github.ref_name` IS the tag on a version-tag push, so this mutation is semantically
// harmless and structurally wrong. Deny-by-default says it reds.
func TestRegress0046L10_S12_ATraceableButDifferentSourceIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ${{ needs.build.outputs.image }}:${{ github.ref_name }}\n")
	l8MustRed(t, root, "a re-smoke whose version half names the workflow event's ref rather than the planning logic's output")
}

// --- S13. And the honest other direction: the graph is followed, not matched ---
//
// The same plan output reached through an unfamiliar job output must PASS, or the trace
// is a list of permitted strings by another name.
func TestRegress0046L10_S13_TheSameOutputThroughANewJobOutputPasses(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      version: ${{ steps.plan.outputs.version }}\n",
		"      version: ${{ steps.plan.outputs.version }}\n      ver2: ${{ steps.plan.outputs.version }}\n")
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.ver2 }}\n")
	l8MustPass(t, root, "a re-smoke reaching the SAME planning output through a different job output")
}

// --- S14. A build-job output rewritten to a literal is refused ----------------
//
// The trace is recursive, so the hole a literal opens one hop away must be closed too.
func TestRegress0046L10_S14_AJobOutputRewrittenToALiteralIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      version: ${{ steps.plan.outputs.version }}\n",
		"      version: v0.1.0\n")
	l8MustRed(t, root, "a build-job output rewritten to the literal v0.1.0, one hop up the trace")
}

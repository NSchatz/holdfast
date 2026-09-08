//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 9). Helpers live in regress_0046_F22_test.go: same harness, same
// rule - take the REAL committed inputs, apply ONE mutation a maintainer could
// plausibly make, and report what the committed gate says about it. A probe whose
// mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L9 -v ./scripts/release-shape-gate/...
//
// Nothing here publishes, reaches a network, or executes a workflow step other
// than the planning script the gate itself runs; every mutation lands in
// t.TempDir().

import "testing"

// --- F26. THE HANDED VALUE IS HELD AGAINST ONE SAMPLE, NOT AGAINST THE PLAN ---
//
// A7: "the floating reference is promoted only after ... THE RE-SMOKE OF THE
// ARTEFACT PULLED BACK FROM THE REGISTRY". A11: "SHALL fail the release if that
// reference does not resolve to THE DIGEST THE RUN JUST GATED."
//
// checkHandedValues compares each declared name whole against `handed`, which is
// built from the planning outputs of ONE planned shape - `sampleTag`, the
// constant `v0.1.0` in main.go. So a literal that happens to equal that constant
// is accepted as "the value the planning logic produced", and every release cut
// at any OTHER version re-smokes v0.1.0 - the release that is already published
// and already passed - while the artefact this run pushed is never pulled back.
// That is F22 exactly, at the one spelling the sample makes invisible.
func TestRegress0046L9_F26_AHardCodedRefEqualToTheSampleTagIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ghcr.io/nschatz/holdfast:v0.1.0\n")
	l8MustRed(t, root, "a re-smoke pinned to the literal v0.1.0, which is the already-published release and not the artefact any later run pushes")
}

// F26b. The same coincidence on A11's resolution step. `VERSION: v0.1.0` makes
// scripts/resolve-compose-image.sh compare the compose reference's digest
// against ghcr.io/nschatz/holdfast:v0.1.0 on every future release.
func TestRegress0046L9_F26b_AHardCodedVersionEqualToTheSampleTagIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: ${{ needs.build.outputs.version }}\n        run: ./scripts/resolve-compose-image.sh\n",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: v0.1.0\n        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "an A11 resolution pinned to the literal v0.1.0 rather than the version this run published")
}

// F26c. And on the promotion, which is the step whose handed values the gate has
// compared since loop 6: `:latest` retagged onto a literal v0.1.0 forever.
func TestRegress0046L9_F26c_AHardCodedPromotionVersionEqualToTheSampleTagIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          VERSION: ${{ needs.build.outputs.version }}\n          FLOATING_TAG: latest\n",
		"          VERSION: v0.1.0\n          FLOATING_TAG: latest\n")
	l8MustRed(t, root, "a promotion pinned to the literal v0.1.0, so the floating reference is retagged onto the previous release on every future run")
}

// F26d. The same coincidence reaches the PUSH, which is not an `env:` value at
// all but the `tags:` input compared against the same one-sample `gated`. Pinned
// end to end, a later release republishes v0.1.0 - which is already published,
// and which semver.org (this spec's own carried source) says must never be
// modified.
func TestRegress0046L9_F26d_AHardCodedPushTagEqualToTheSampleTagIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          tags: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          tags: ghcr.io/nschatz/holdfast:v0.1.0\n")
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ghcr.io/nschatz/holdfast:v0.1.0\n")
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          VERSION: ${{ needs.build.outputs.version }}\n          FLOATING_TAG: latest\n",
		"          VERSION: v0.1.0\n          FLOATING_TAG: latest\n")
	l8MustRed(t, root, "a release pinned end to end to the literal v0.1.0, which republishes an already-released version on every later tag")
}

// --- Controls, so the four above are a measurement and not a selection --------

// S8. The comparison really is whole: one trailing space reds.
func TestRegress0046L9_S8_AWholeComparisonRejectsATrailingSpace(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: \"${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }} \"\n")
	l8MustRed(t, root, "a handed reference carrying a trailing space")
}

// S9. A declared name moved to the JOB's env is still in scope for the OTHER
// role steps in that job, where it is unclassified - so the level a value is set
// at is not an escape.
func TestRegress0046L9_S9_ADeclaredNameHoistedToTheJobEnvIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        env:\n          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n        run: ./scripts/release-resmoke.sh\n",
		"        run: ./scripts/release-resmoke.sh\n")
	l8Mutate(t, root, ".github/workflows/release.yml",
		"    permissions:\n      contents: write # cut the GitHub release\n",
		"    env:\n      REF: ${{ needs.build.outputs.image }}:latest\n    permissions:\n      contents: write # cut the GitHub release\n")
	l8MustRed(t, root, "the re-smoke's reference hoisted to the publish job's env, where it is in scope for every other role step in that job")
}

// S10. An environment name on a role step that no role declared reds by name.
func TestRegress0046L9_S10_AnUndeclaredNameOnARoleStepIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n          SMOKE_PLATFORMS: linux/amd64\n")
	l8MustRed(t, root, "an environment name on a role step that no role declared")
}

//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 11, which grades the S0057 merge and nothing else). Same harness
// as every earlier regress_0046_*_test.go on this branch (helpers live in
// regress_0046_F22_test.go): take the REAL committed inputs, apply ONE mutation,
// and report what the committed gate says about it.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L11 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`. Nothing here publishes, reaches a
// network, or executes a workflow step other than the planning script the gate
// itself runs; every mutation lands in t.TempDir().
//
// Naming: TestRegress0046L11_F<n>_… pins a defect (it FAILS today);
// TestRegress0046L11_S<n>_… are controls the gate must catch or must accept, kept
// so the file is a measurement rather than a selection.
//
// WHY THIS FILE EXISTS. The merge at f24649e added `FLOATING_TAG: latest` to the
// `resolve-compose` step's `env:` block. Three carry-forward probes anchored on
// that block as it was (regress_0046_F22_test.go F23, regress_0046_F24_test.go
// F23b, regress_0046_F26_test.go F26b) now abort with "mutation anchor not
// present" instead of running their mutation - so they report FAIL for a defect
// they never measured. The S-cases below re-anchor those three mutations on the
// merged text and ask the same question the rotted probes asked, which is the only
// way to know whether the properties they pinned survived the merge.

import "testing"

// --- S15. Anti-vacuity: the unmutated merged tree passes ----------------------

func TestRegress0046L11_S15_BaselinePasses(t *testing.T) {
	l8MustPass(t, l8Fixture(t), "the committed release definition after the S0057 merge")
}

// The `resolve-compose` step's `env:` block AFTER the merge. The three rotted
// probes each anchored on this text without the `FLOATING_TAG` line.
const l11ResolveEnv = "        env:\n" +
	"          IMAGE: ${{ needs.build.outputs.image }}\n" +
	"          VERSION: ${{ needs.build.outputs.version }}\n" +
	"          FLOATING_TAG: latest\n" +
	"        run: ./scripts/resolve-compose-image.sh\n"

// --- S16. F23's mutation, re-anchored: a resolution pointed at a version this
// run never published.
//
// A11: "SHALL fail the release if that reference does not resolve to the digest
// THE RUN JUST GATED." This is the mutation TestRegress0046L8_F23 stopped being
// able to apply at f24649e.
func TestRegress0046L11_S16_ResolutionAgainstAnotherVersionIsStillCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: v0.0.1\n"+
			"          FLOATING_TAG: latest\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a release whose A11 resolution is handed a version this run never published")
}

// --- S17. F26b's mutation, re-anchored: the literal that equals the version the
// example deployment pins, which is also the version actually published.
func TestRegress0046L11_S17_ResolutionPinnedToThePublishedVersionIsStillCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: v0.1.0\n"+
			"          FLOATING_TAG: latest\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a release whose A11 resolution is pinned to the literal v0.1.0")
}

// --- S18. F23b's mutation, re-anchored: the resolution handed the floating tag
// as its VERSION, which made the digest comparison a tautology.
func TestRegress0046L11_S18_ResolutionAgainstItselfIsStillCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: latest\n"+
			"          FLOATING_TAG: latest\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a release whose A11 resolution compares the floating reference with itself")
}

// --- S19. And the merge's own new value: FLOATING_TAG on the resolution step
// set to the tag the example deployment PINS.
func TestRegress0046L11_S19_ResolutionHandedThePinnedVersionAsFloatingIsCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: ${{ needs.build.outputs.version }}\n"+
			"          FLOATING_TAG: v0.1.0\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a resolution handed the version the example deployment pins as the floating tag")
}

// --- F29. THE COMPOSE REFERENCE'S TAG IS NO LONGER HELD TO ANYTHING THIS
// REPOSITORY PUBLISHES ---------------------------------------------------------
//
// A9: "WHEN the image reference the example deployment names is compared with the
// reference a release derives from the repository that owns this module THE SYSTEM
// SHALL fail unless THEY ARE THE SAME REFERENCE, and SHALL print both."
//
// Before the merge the gate required exactly `composeRef == ${IMAGE}:${FLOATING_TAG}`,
// so the whole reference was held. After the merge only the NAME half is held; the
// TAG must merely be present, differ from the floating tag, and be followed by a
// well-formed digest. Nothing ties it to a version this repository has ever
// released, so a tag naming a version that does not exist passes `make check` -
// which is A9's own grade route. A reader of docker-compose.yml is told v9.9.9 and
// gets whatever the digest beside it resolves to.
//
// Recorded as a MEASUREMENT of the merge's cost, not as a demand: S0057's landed
// digest pin and A9's literal "same reference" cannot both hold, and
// conductor-ruling-reopen-after-s0057-conflict.md requires the pin to be kept. See
// the verdict, where this is non-blocking.
func TestRegress0046L11_F29_AComposeTagNamingAnUnpublishedVersionIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, "docker-compose.yml",
		"    image: ghcr.io/nschatz/holdfast:v0.1.0@sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2\n",
		"    image: ghcr.io/nschatz/holdfast:v9.9.9@sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2\n")
	l8MustRed(t, root, "an example deployment whose tag names a version this repository has never released")
}

// --- F30. THE TWO `FLOATING_TAG` DECLARATIONS MAY DISAGREE --------------------
//
// The merge made `FLOATING_TAG` a second declared literal, on the resolution step,
// so the reference the release MOVES and the reference it then RESOLVES are two
// independent literals. Each is held only against the tag docker-compose.yml pins -
// as the value it must NOT be - so nothing holds them to each other. A release can
// promote one floating reference and verify another.
//
// docs/release.md states the residue that WHICH floating tag a release moves is no
// longer decided by anything. It does not state this one: that the tag it moves and
// the tag it checks afterwards need not be the same tag, so the assertion "the
// promotion landed on the digest this run gated" can be redirected at a reference
// the promotion never touched.
func TestRegress0046L11_F30_DivergentFloatingTagsAreAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: ${{ needs.build.outputs.version }}\n"+
			"          FLOATING_TAG: stable\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a release that promotes `:latest` and then resolves `:stable`")
}

// --- S20. The residue docs/release.md DOES state, driven for real: both
// declarations moved together to a tag nothing depends on passes.
//
// This is the control that keeps F30 honest - it shows the gate is not simply
// indifferent to the value, it is indifferent to the AGREEMENT.
func TestRegress0046L11_S20_TheStatedResidueIsReal(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          FLOATING_TAG: latest\n        run: ./scripts/release-promote.sh\n",
		"          FLOATING_TAG: stable\n        run: ./scripts/release-promote.sh\n")
	l8Mutate(t, root, ".github/workflows/release.yml", l11ResolveEnv,
		"        env:\n"+
			"          IMAGE: ${{ needs.build.outputs.image }}\n"+
			"          VERSION: ${{ needs.build.outputs.version }}\n"+
			"          FLOATING_TAG: stable\n"+
			"        run: ./scripts/resolve-compose-image.sh\n")
	l8MustPass(t, root, "a release that moves `:stable` throughout, which docs/release.md states is no longer decided")
}

// --- S21. A pin dropped from a `uses:` is not this gate's job, and it must not
// be quietly this gate's job either. The ruling's hazard was a dropped pin, so
// drive it: the gate is indifferent (check-pins.sh owns it), which is why the
// verdict runs `make check-pins` rather than inferring it from a green gate.
func TestRegress0046L11_S21_TheShapeGateIsIndifferentToAPin(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - uses: actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4\n",
		"      - uses: actions/download-artifact@v4\n")
	l8MustPass(t, root, "an unpinned `uses:`, which scripts/check-pins.sh and not this gate decides")
}

// --- S22. The merge's own stated proof, driven rather than taken on trust: the
// readings say the `id:` had to survive every conflicting hunk because "dropping
// one would make an act unnameable (proved: deleting `id: download-dist` reds the
// gate by name)". That is the one thing the merge could have lost silently while
// keeping every pin, so it is the control this ordinal owes.
func TestRegress0046L11_S22_AMovedStepLosingItsIdIsCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - uses: actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4\n        id: download-dist\n",
		"      - uses: actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4\n")
	l8MustRed(t, root, "a step in the publishing job that lost its `id:` in the merge")
}

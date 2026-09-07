//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 2). Same shape and the same harness as regress_0046_F1_test.go:
// take the REAL committed inputs, apply one mutation a maintainer could
// plausibly make, and report what the committed gate says about it. A probe
// whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -v -run TestRegress0046L2 ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file documents defects, it
// is not part of the repository's gate.
//
// Nothing here publishes: every mutation lands in t.TempDir(), and the gate
// stubs every command that could.

import (
	"strings"
	"testing"
)

// mustPass is mustRed's counterpart. The passing probes below are not decoration:
// without them a "the gate reds" assertion could be satisfied by a gate that reds
// on everything, and the fix for F5 must not become "any `outputs:` is a publish".
func mustPass(t *testing.T, root, what string) {
	t.Helper()
	red, out := runGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if red {
		t.Fatalf("the gate RED %s", what)
	}
}

// devStep builds one extra `docker/build-push-action` step, spelled with whatever
// `with:` lines it is handed. No `if:`, so it runs on a manual dispatch.
func devStep(with ...string) string {
	var b strings.Builder
	b.WriteString("      - name: publish a dev image so testers can pull dispatch builds\n")
	b.WriteString("        uses: docker/build-push-action@v6\n")
	b.WriteString("        with:\n")
	b.WriteString("          context: .\n")
	b.WriteString("          platforms: linux/amd64\n")
	for _, l := range with {
		b.WriteString("          " + l + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

const anchor = "      - name: build the release binaries\n"

func withDevStep(t *testing.T, with ...string) string {
	t.Helper()
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml", anchor, devStep(with...)+anchor)
	return root
}

// --- F5. THE HOLE -----------------------------------------------------------
//
// `docker/build-push-action`'s own documentation defines `push:` as "Shorthand
// for `--output=type=registry`", and its README's multi-platform example pushes
// through `outputs:` with NO `push:` input at all:
//
//	outputs: type=image,name=…,push-by-digest=true,name-canonical=true,push=true
//
// The gate models exactly one of those two inputs. `push:` absent is inferred to
// mean "the action's own default, a local build" (workflow.go:252-253), so a step
// that publishes through `outputs:` is classified as performing no published act,
// and a manual dispatch that pushes an image to GHCR is reported as
// "NONE of them publishes anything".

// F5. The action's own README spelling. This dry run pushes
// ghcr.io/nschatz/holdfast:dev.
func TestRegress0046L2_F5_DispatchPublishesThroughTheOutputsInput(t *testing.T) {
	root := withDevStep(t, "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev,push=true")
	mustRed(t, root, "a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev through `outputs: type=image,…,push=true`")
}

// F5b. buildx's shorthand for the same thing: `--output type=registry`.
func TestRegress0046L2_F5b_DispatchPublishesThroughOutputsTypeRegistry(t *testing.T) {
	root := withDevStep(t,
		"tags: ghcr.io/nschatz/holdfast:dev",
		"outputs: type=registry")
	mustRed(t, root, "a release definition whose dry run pushes through `outputs: type=registry`")
}

// F5c. Belt and braces, so nobody can argue the push would merely 401: the GHCR
// login is unguarded too. This dry run authenticates and then publishes.
func TestRegress0046L2_F5c_DispatchLogsInAndPublishesThroughOutputs(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: log in to GHCR\n        if: steps.plan.outputs.publish == 'true'\n",
		"      - name: log in to GHCR\n")
	mutate(t, root, ".github/workflows/release.yml", anchor,
		devStep("outputs: type=image,name=ghcr.io/nschatz/holdfast:dev,push=true")+anchor)
	mustRed(t, root, "a release definition whose dry run logs in to GHCR and publishes through `outputs:`")
}

// F5d. The same input on the step that already exists, which is how this arrives
// in practice: `push: true` is swapped for the `outputs:` form during a
// push-by-digest refactor.
//
// THE MUTATION IS BYTE-IDENTICAL TO THE ONE WRITTEN AT IMPL ORDINAL 2. Its
// EXPECTATION is not, and the reason is the finding itself. At ordinal 2 this
// asserted `mustRed` with the note "nothing then orders it, and the runbook need
// not name it": the version-tag push disappeared from the gate's inventory
// entirely, three acts became two, and the gate red only because this workflow
// happens to carry a separate promotion to complain about ("nothing pushes an
// image, yet step 17 ... moves a floating reference"). The refuter recorded that
// as surviving by accident.
//
// Once BOTH of the action's destination inputs are decided, this mutation is not
// a defect at all: it is a correct release definition, spelled the way the
// action's own multi-platform example spells it. Asserting red on it would be
// the false positive P6 and self-test case 3d exist to forbid. So the honest
// assertion is the one the refuter named as the fix's own success condition -
// the respelled push must APPEAR IN THE ACT INVENTORY properly, which is what
// makes A8 demand a runbook entry for it and gives the ordering property a push
// to order. That is asserted here, and pinned inside `make check` too, by
// release-shape-selftest case 3j.
func TestRegress0046L2_F5d_TheRealPushRespelledAsOutputsStaysInTheInventory(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"          push: true\n          tags: ${{ steps.plan.outputs.image }}:${{ steps.plan.outputs.version }}\n",
		"          outputs: type=image,name=${{ steps.plan.outputs.image }}:${{ steps.plan.outputs.version }},push=true\n")
	red, out := runGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if red {
		t.Fatalf("the gate RED the version-tag push respelled with `outputs:`, which is a correct release definition")
	}
	for _, want := range []string{
		"names every one of the 3 published act(s)",
		"image-push@push-the-multi-arch-image-version-tag-only",
		"version-tag push",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the respelled push is not visible to the gate: nothing in its output mentions %q.\nIt passed, but over an inventory the push had vanished from - which is what ordinal 2 blocked on.", want)
		}
	}
}

// --- probes the gate CATCHES, kept so this file is a measurement -------------

// P0. The committed tree still passes. Without this, every "reds" assertion above
// could be met by a gate that fails on everything.
func TestRegress0046L2_P0_BaselineStillPasses(t *testing.T) {
	mustPass(t, fixture(t), "the committed inputs")
}

// P1. The same dev step spelled with the input the gate DOES model. CAUGHT.
func TestRegress0046L2_P1_PushTrueIsCaught(t *testing.T) {
	root := withDevStep(t, "push: true", "tags: ghcr.io/nschatz/holdfast:dev")
	mustRed(t, root, "a dispatch that publishes through `push: true`")
}

// P2. F1's own case, re-run against the fix: an expression true on a dispatch.
// CAUGHT (this is what ordinal 1 blocked on and 0ec372a closed).
func TestRegress0046L2_P2_ExpressionValuedPushIsDecided(t *testing.T) {
	root := withDevStep(t,
		"push: ${{ github.event_name == 'workflow_dispatch' }}",
		"tags: ghcr.io/nschatz/holdfast:dev")
	mustRed(t, root, "a dispatch that publishes through an expression-valued `push:`")
}

// P3. An undecidable `push:` (a context no planning run here produces). CAUGHT,
// fail-closed.
func TestRegress0046L2_P3_UndecidablePushIsRed(t *testing.T) {
	root := withDevStep(t, "push: ${{ vars.PUBLISH_DEV }}", "tags: ghcr.io/nschatz/holdfast:dev")
	mustRed(t, root, "a `push:` input naming a context the gate cannot decide")
}

// P4. A `push:` that is a YAML integer rather than a boolean. CAUGHT.
func TestRegress0046L2_P4_NonBooleanPushTypeIsRed(t *testing.T) {
	root := withDevStep(t, "push: 1", "tags: ghcr.io/nschatz/holdfast:dev")
	mustRed(t, root, "a `push:` input that is an integer")
}

// P5. An empty `push:`. CAUGHT.
func TestRegress0046L2_P5_EmptyPushIsRed(t *testing.T) {
	root := withDevStep(t, `push: ""`, "tags: ghcr.io/nschatz/holdfast:dev")
	mustRed(t, root, "an empty `push:` input")
}

// P6. The other direction, which matters as much: the real push step respelled
// with the idiomatic expression is a CORRECT definition and must still pass.
func TestRegress0046L2_P6_CorrectExpressionStillPasses(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"          push: true\n",
		"          push: ${{ steps.plan.outputs.publish }}\n")
	mustPass(t, root, "the version-tag push respelled as `push: ${{ steps.plan.outputs.publish }}`")
}

// P7. A guard-level publish on a dispatch, unrelated to any action input. CAUGHT.
func TestRegress0046L2_P7_UnguardedRunLevelPushIsCaught(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml", anchor,
		"      - name: publish a dev image\n        run: docker push ghcr.io/nschatz/holdfast:dev\n\n"+anchor)
	mustRed(t, root, "an unguarded `docker push` on a dispatch")
}

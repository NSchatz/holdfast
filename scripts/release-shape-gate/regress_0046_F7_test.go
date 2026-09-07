//go:build regress0046

package main

// Regression probes for the S0046-holdfast-release-2 impl-gate ordinal 3 finding
// (verdict-impl-3.md, F7). Same harness as regress_0046_F1_test.go and
// regress_0046_F5_test.go: the REAL committed inputs copied into t.TempDir(), one
// mutation, the committed gate run as a library.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L3 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins a defect, it is not
// part of the repository's gate.
//
// Nothing here publishes: every mutation lands in t.TempDir(), and the gate executes
// only the planning steps, under stubbed binaries.
//
// --- THE DEFECT THIS FILE PINS ---------------------------------------------------
//
// Two of the `run:` detectors were confined to ONE PHYSICAL LINE:
//
//	{ActImagePush, `\bdocker\s+(buildx\s+)?(build|bake)\b[^\n]*--push\b`, …}
//	{ActRelease,   `\bgh\s+release\s+(create|edit|upload|delete)\b`, …}
//
// `[^\n]*` cannot cross a newline, and `\s+` matches whitespace but not a backslash.
// So `docker buildx build \` with `--push` on the next line, and `gh release \` with
// `create` on the next line, were classified as performing NO published act at all -
// while the shell runs both as one command. That is not obfuscation: a shell line
// continuation is this repository's own house style for a multi-flag command, and
// .github/workflows/release.yml writes `go build -trimpath \` (line 202) and
// `gh release create "$VERSION" "${args[@]}" \` (line 313) in exactly that shape.
//
// The gate then printed `ok: on a manual dispatch, N step(s) run and NONE of them
// publishes anything`, printed `ok: docs/release.md names every one of the 3
// published act(s)` while a fourth existed, and exited 0 - over a definition whose
// workflow_dispatch would push an image to a public registry.
//
// THE FIX IS ONE NORMALISATION, NOT TWO MORE PATTERNS (the conductor's ruling of
// 2026-09-07). `Step.script()` joins shell line continuations before any detector
// runs, so a command spanning lines is one string to every one of them. The three
// F7 cases below are that fix's proof; P0-P8 are the measurement around it.

import (
	"io"
	"os"
	"strings"
	"testing"
)

// runGateCapturingStderr is runGate plus the half of the gate's output that
// matters to A12. `gate.note` writes to `g.out`, which the ordinal 1 harness
// captures, but `gate.bad` writes the actual refusal - the one that NAMES the
// altered step - straight to os.Stderr. Asserting only on stdout would let a red
// for somebody else's reason count as a catch, which is the mistake this whole
// file exists to stop being made about the gate.
//
// os.Stderr is swapped for a pipe drained by a goroutine (a full pipe buffer
// would otherwise block the gate mid-run) and restored before the assertion.
// Nothing here runs in parallel, so the swap is confined to one test.
func runGateCapturingStderr(t *testing.T, root string) (red bool, output string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	red, out := runGate(t, root)

	os.Stderr = saved
	_ = w.Close()
	stderr := <-done
	_ = r.Close()
	return red, out + stderr
}

// devRunStep builds one extra `run:` step out of the script lines it is handed. No
// `if:`, so it runs on a manual dispatch - which is the whole point: A6 says a
// dispatch publishes nothing, and this step would publish.
func devRunStep(script ...string) string {
	var b strings.Builder
	b.WriteString("      - name: publish a dev image so testers can pull dispatch builds\n")
	b.WriteString("        env:\n")
	b.WriteString("          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n")
	b.WriteString("        run: |\n")
	b.WriteString("          set -euo pipefail\n")
	for _, l := range script {
		b.WriteString("          " + l + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

func withDevRunStep(t *testing.T, script ...string) string {
	t.Helper()
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml", anchor, devRunStep(script...)+anchor)
	return root
}

// mustRedNaming is mustRed with the extra assertion A12 actually makes: the gate has
// to FAIL NAMING THE ALTERED STEP. A red that never says which step moved is a red
// for somebody else's reason, and would count a backstop as a catch.
func mustRedNaming(t *testing.T, root, what string, want ...string) {
	t.Helper()
	red, out := runGateCapturingStderr(t, root)
	t.Logf("gate output:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED %s", what)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("the gate red %s but nothing in its output mentions %q, so it did not name what it saw", what, w)
		}
	}
}

// --- F7. THE HOLE -----------------------------------------------------------

// F7. The whole finding in six lines: `--push` on the line below the build. This
// dry run pushes ghcr.io/nschatz/holdfast:dev to a public registry.
func TestRegress0046L3_F7_DispatchPushesViaAContinuedBuildxPush(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build \`,
		`  --push \`,
		`  --platform linux/amd64 \`,
		`  -t ghcr.io/nschatz/holdfast:dev \`,
		`  .`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev with a line-continued `docker buildx build --push`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F7b. And it cannot be waved away as a push that would merely 401: the same
// mutation with the GHCR login unguarded too. This dry run authenticates and THEN
// publishes.
func TestRegress0046L3_F7b_DispatchLogsInAndPushesViaAContinuedBuildxPush(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: log in to GHCR\n        if: steps.plan.outputs.publish == 'true'\n",
		"      - name: log in to GHCR\n")
	mutate(t, root, ".github/workflows/release.yml", anchor,
		devRunStep(
			`docker buildx build \`,
			`  --push \`,
			`  --platform linux/amd64 \`,
			`  -t ghcr.io/nschatz/holdfast:dev \`,
			`  .`)+anchor)
	mustRedNaming(t, root,
		"a dry run that logs in to GHCR and then pushes via a line-continued build",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F7c. The same confinement on the other command. `gh release create` creates a
// published RELEASE object - irreversible in exactly the sense docs/release.md
// records, and a thing no re-run takes back.
func TestRegress0046L3_F7c_DispatchCutsAReleaseViaAContinuedGhRelease(t *testing.T) {
	root := withDevRunStep(t,
		`gh release \`,
		`  create v0.0.0-dev \`,
		`  --title "dev build" \`,
		`  --notes "for testers"`)
	mustRedNaming(t, root,
		"a dry run that cuts a GitHub release via a line-continued `gh release create`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// --- P0-P8. The measurement around it ---------------------------------------
//
// These pass on purpose, so this file is a measurement rather than a selection of
// only the cases that fail. P1-P4 are the SAME acts written on one line, which is
// what makes F7 a hole in the SPELLING rather than a missing detector; P5 is the
// honest other direction, without which "join the lines" could be satisfied by
// calling every continued build a publish; P6-P8 re-check ordinal 2's finding.

// P0. The committed tree still passes. Without this, every "reds" assertion above
// could be met by a gate that fails on everything.
func TestRegress0046L3_P0_BaselineStillPasses(t *testing.T) {
	mustPass(t, fixture(t), "the committed inputs")
}

// P1. F7's act on ONE line. CAUGHT, with the step named.
func TestRegress0046L3_P1_OneLineBuildxPushIsCaught(t *testing.T) {
	root := withDevRunStep(t, `docker buildx build --push --platform linux/amd64 -t ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root, "a one-line `docker buildx build --push` on a dispatch",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P2. F7c's act on ONE line. CAUGHT, with the step named.
func TestRegress0046L3_P2_OneLineGhReleaseCreateIsCaught(t *testing.T) {
	root := withDevRunStep(t, `gh release create v0.0.0-dev --title "dev build" --notes "for testers"`)
	mustRedNaming(t, root, "a one-line `gh release create` on a dispatch",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P3. The plainest image push of all, on one line. CAUGHT.
func TestRegress0046L3_P3_OneLineDockerPushIsCaught(t *testing.T) {
	root := withDevRunStep(t, `docker push ghcr.io/nschatz/holdfast:dev`)
	mustRedNaming(t, root, "a one-line `docker push` on a dispatch",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P4. A different KIND of act - moving a floating reference, which is what hands
// every `docker compose pull` user a new image. CAUGHT.
func TestRegress0046L3_P4_FloatingReferenceMoveIsCaught(t *testing.T) {
	root := withDevRunStep(t, `docker buildx imagetools create -t ghcr.io/nschatz/holdfast:latest ghcr.io/nschatz/holdfast:dev`)
	mustRedNaming(t, root, "an unguarded floating-reference move on a dispatch",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P5. THE HONEST OTHER DIRECTION, and the reason the fix is a normalisation rather
// than a refusal: joining lines must not turn every continued build into a publish.
// A continued build with no `--push` writes to the local daemon and must still pass.
func TestRegress0046L3_P5_ContinuedBuildWithNoPushStaysLocal(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build \`,
		`  --load \`,
		`  --platform linux/amd64 \`,
		`  -t holdfast:dev \`,
		`  .`)
	mustPass(t, root, "a line-continued build that only loads locally")
}

// P6. Ordinal 2's finding, re-checked in a shape that appears in neither the
// implementation's fixtures nor release-shape-selftest.sh: `type=registry` as the
// SECOND entry of a multi-line list, behind a local one. CAUGHT.
func TestRegress0046L3_P6_RegistryExporterBehindALocalOneIsCaught(t *testing.T) {
	root := withDevStep(t,
		"tags: ghcr.io/nschatz/holdfast:dev",
		"outputs: |",
		"  type=docker",
		"  type=registry")
	mustRedNaming(t, root, "a multi-line `outputs:` whose second entry is the registry exporter",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P7. The same act with its attributes reordered and quoted, which is a spelling no
// fixture in the tree uses. CAUGHT - the entry is parsed, not matched.
func TestRegress0046L3_P7_ReorderedQuotedImageExporterIsCaught(t *testing.T) {
	root := withDevStep(t, `outputs: push=true,name="ghcr.io/nschatz/holdfast:dev",type=image`)
	mustRedNaming(t, root, "an `outputs:` entry with push=true first and a quoted name",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P8. And the other direction one layer in: the `image` exporter with no `push=` at
// all keeps its result locally, so it is NOT a published act and must still pass.
func TestRegress0046L3_P8_ImageExporterWithNoPushAttributePasses(t *testing.T) {
	root := withDevStep(t, "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev")
	mustPass(t, root, "an `outputs: type=image` with no push= attribute")
}

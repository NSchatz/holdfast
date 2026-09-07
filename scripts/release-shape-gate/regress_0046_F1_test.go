//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 1). Each one takes the REAL committed inputs, applies one
// mutation a maintainer could plausibly make, and reports what the committed
// gate says about it. A probe whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file documents defects, it
// is not part of the repository's gate.
//
// Nothing here publishes: the gate stubs every command that could, and every
// mutation lands in t.TempDir().

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateInputs is the set of real files a fixture is built from. It grew in impl ordinal 6:
// the gate no longer READS a `run:` step, it OBSERVES one, so every script a step invokes has
// to exist in the fixture or the step invokes a program that is not there - which the gate
// now refuses rather than passing. Only this list changed; no assertion in any of the
// regression probes was touched.
var gateInputs = []string{
	".github/workflows/release.yml",
	"docker-compose.yml",
	"docs/release.md",
	"go.mod",
	"scripts/resolve-compose-image.sh",
	"scripts/smoke-image.sh",
	"scripts/install-ffmpeg.sh",
}

// fixture copies the real inputs into a scratch root and returns it.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range gateInputs {
		src := filepath.Join("..", "..", rel)
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("cannot read %s: %v", src, err)
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mutate(t *testing.T, root, rel, old, new string) {
	t.Helper()
	p := filepath.Join(root, rel)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, old) {
		t.Fatalf("mutation anchor not present in %s:\n%s", rel, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(s, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runGate runs the committed gate over root and reports whether it red.
func runGate(t *testing.T, root string) (red bool, output string) {
	t.Helper()
	var buf bytes.Buffer
	g := &gate{root: root, out: &buf}
	err := g.run()
	if err != nil {
		buf.WriteString("HARD REFUSAL: " + err.Error() + "\n")
		return true, buf.String()
	}
	return g.failed, buf.String()
}

// mustRed asserts the gate reds, and always prints what it saw so a red for the
// WRONG reason is visible rather than counted as a catch.
func mustRed(t *testing.T, root, what string) {
	t.Helper()
	red, out := runGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED %s", what)
	}
}

// Sanity: the committed tree passes. Without this every "reds" assertion below
// could be satisfied by a gate that fails on everything.
func TestRegress0046_BaselinePasses(t *testing.T) {
	red, out := runGate(t, fixture(t))
	if red {
		t.Fatalf("the committed inputs do not pass the gate:\n%s", out)
	}
}

// The mutation for F1 and F1b: a new step that publishes a `:dev` image, whose
// publish decision is carried by an EXPRESSION-valued `push:` input rather than
// a step guard - which is GitHub's own documented idiom for a conditional push
// (`push: ${{ github.event_name != 'pull_request' }}`). On a workflow_dispatch
// the expression is true, so a DRY RUN pushes an image to GHCR.
const devPushStep = `      - name: publish a dev image so testers can pull dispatch builds
        uses: docker/build-push-action@v6
        with:
          context: .
          platforms: linux/amd64
          push: ${{ github.event_name == 'workflow_dispatch' }}
          tags: ghcr.io/nschatz/holdfast:dev

`

// F1. A6 / A12. The dev push alone: a step that pushes an image runs on a
// manual dispatch, and the gate must fail naming it.
func TestRegress0046_F1_DispatchPushesViaExpressionValuedPushInput(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: build the release binaries\n",
		devPushStep+"      - name: build the release binaries\n")
	mustRed(t, root, "a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev")
}

// F1b. The same, with the GHCR login unguarded too, so there is no room to
// argue the push would merely 401. This dry run authenticates and publishes.
func TestRegress0046_F1b_DispatchLogsInAndPushes(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`      - name: log in to GHCR
        if: steps.plan.outputs.publish == 'true'
`,
		`      - name: log in to GHCR
`)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: build the release binaries\n",
		devPushStep+"      - name: build the release binaries\n")
	mustRed(t, root, "a release definition whose dry run logs in to GHCR and pushes an image")
}

// --- probes the gate CATCHES. Kept so the file is a measurement, not a
// --- selection of only the cases that fail.

// F2. A7/A13: the promotion must follow the re-smoke of BOTH architectures
// pulled back from the registry. CAUGHT.
func TestRegress0046_F2_ReSmokeCoversOnlyOneArch(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`          docker pull --platform linux/arm64 "$REF"
          ./scripts/smoke-image.sh "$REF" linux/arm64 --no-encode
`, "")
	mustRed(t, root, "a release that never pulls the arm64 half back before promoting :latest")
}

// F3. A7: the full gate must run before the promotion. CAUGHT.
func TestRegress0046_F3_NoFullGateAtAll(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`      - name: the full gate (make check)
        run: make check
`, "")
	mustRed(t, root, "a release definition that runs no full gate before publishing")
}

// F4. A6: a dispatch must publish nothing. CAUGHT.
func TestRegress0046_F4_UnguardedGithubReleaseStep(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`      - name: the full gate (make check)
`,
		`      - name: publish a nightly release
        run: gh release create nightly --generate-notes

      - name: the full gate (make check)
`)
	mustRed(t, root, "an unguarded `gh release create` that runs on a dispatch")
}

// F5. A7/A3: `:latest` promoted onto something the run never pushed. CAUGHT.
func TestRegress0046_F5_PromoteFromALocalBuildInsteadOfTheGatedDigest(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`          docker buildx imagetools create -t "${IMAGE}:latest" "${IMAGE}:${VERSION}"`,
		`          docker buildx imagetools create -t "${IMAGE}:latest" holdfast:release`)
	mustRed(t, root, "a promotion that points :latest at a locally-built image the run never pushed or pulled back")
}

// F6. A14: a tolerated failure spelled as an EXPRESSION (the self-test drives
// only the literal `continue-on-error: true`). CAUGHT - tolerates() is
// fail-closed on anything it cannot decide.
func TestRegress0046_F6_ToleratedFailureSpelledAsAnExpression(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		`      - name: the full gate (make check)
        run: make check
`,
		`      - name: the full gate (make check)
        continue-on-error: ${{ github.event_name == 'push' }}
        run: make check
`)
	mustRed(t, root, "a full gate whose failure is tolerated on exactly the event that publishes")
}

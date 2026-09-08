//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 8). Same harness as every earlier regress_0046_*_test.go this
// branch has carried: take the REAL committed inputs, apply ONE mutation a
// maintainer could plausibly make, and report what the committed gate says
// about it. A probe whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L8 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins defects, it is
// not part of the repository's gate.
//
// Nothing here publishes, reaches a network, or executes a workflow step other
// than the planning script the gate itself runs; every mutation lands in
// t.TempDir().
//
// Naming: TestRegress0046L8_F<n>_… pins a defect (it FAILS today);
// TestRegress0046L8_S<n>_… and …_P<n>_… are probes the gate CATCHES or must
// accept, kept so this file is a measurement rather than a selection of only
// the cases that fail.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var l8Inputs = []string{
	".github/workflows/release.yml",
	"docker-compose.yml",
	"docs/release.md",
	"go.mod",
	"scripts/resolve-compose-image.sh",
	"scripts/release-resmoke.sh",
	"scripts/release-promote.sh",
	"scripts/smoke-image.sh",
}

func l8Fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range l8Inputs {
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

// l8Mutate applies one replacement and refuses a mutation that changed nothing -
// the selftest's own `changed()` rule, because a probe that graded the baseline
// would report a catch it never made.
func l8Mutate(t *testing.T, root, rel, old, new string) {
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
	out := strings.Replace(s, old, new, 1)
	if out == s {
		t.Fatalf("the mutation changed nothing in %s - the probe would grade the baseline", rel)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func l8RunGate(t *testing.T, root string) (red bool, output string) {
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

func l8MustRed(t *testing.T, root, what string) {
	t.Helper()
	red, out := l8RunGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED %s", what)
	}
}

func l8MustPass(t *testing.T, root, what string) {
	t.Helper()
	red, out := l8RunGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if red {
		t.Fatalf("the gate REFUSED %s", what)
	}
}

// P0. Sanity: the committed tree passes. Without this every "reds" assertion
// below could be satisfied by a gate that fails on everything.
func TestRegress0046L8_P0_BaselinePasses(t *testing.T) {
	l8MustPass(t, l8Fixture(t), "the committed inputs")
}

// --- F22. THE RE-SMOKE IS NOT HELD TO THE REFERENCE THE RUN JUST PUSHED ------
//
// A7: "THE SYSTEM SHALL report that the floating reference is promoted only
// after the full gate, both architectures' smoke runs, the version-tag push and
// THE RE-SMOKE OF THE ARTEFACT PULLED BACK FROM THE REGISTRY all appear before
// it and all must succeed."
//
// The re-smoke role is located, its program compared whole, and `REF` is
// declared in its mayEnv - but the VALUE of REF is never read. The gate reads
// the promotion's IMAGE/VERSION/FLOATING_TAG and the push step's `tags:` and
// compares those whole (main.go checkComposeReferenceAgreement); it does the
// same for nothing else.
//
// `scripts/release-resmoke.sh` pulls and smokes exactly $REF. So handing it the
// FLOATING reference re-smokes whatever `:latest` already pointed at - the
// PREVIOUS release, which passes - while the artefact this run pushed is never
// pulled back at all. `promote-latest` then moves `:latest` onto it. The push is
// a cache REBUILD (release.yml says so at the push step), which is the entire
// reason the re-smoke exists; this makes the re-smoke grade a different image.
//
// This is not the retired "what does this shell do?" question. REF is an `env:`
// scalar, interpolated against the values the planning logic produced - the
// identical mechanism the promotion's own env already goes through, one
// function call away in the same file.
func TestRegress0046L8_F22_ResmokeOfTheFloatingReferenceIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ${{ needs.build.outputs.image }}:latest\n")
	l8MustRed(t, root, "a release whose re-smoke pulls back `:latest` - the PREVIOUS release - instead of the artefact this run just pushed")
}

// F22b. The same hole with a reference to some other version entirely: nothing
// ties REF to this run at all.
func TestRegress0046L8_F22b_ResmokeOfAnUnrelatedReferenceIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.version }}\n",
		"          REF: ghcr.io/nschatz/holdfast:v0.0.1\n")
	l8MustRed(t, root, "a release whose re-smoke pulls back a hard-coded reference that has nothing to do with this run")
}

// F22-P1. The CONTROL that proves the mechanism exists and is simply not
// applied to the re-smoke: the identical class of mutation on the PROMOTION's
// own env - the one step whose handed values the gate does compare - reds.
func TestRegress0046L8_F22P1_TheSameMutationOnThePromotionIsCaught(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"          VERSION: ${{ needs.build.outputs.version }}\n          FLOATING_TAG: latest\n",
		"          VERSION: v0.0.1\n          FLOATING_TAG: latest\n")
	l8MustRed(t, root, "a promotion pointed at a version this run did not gate")
}

// --- F23. THE RESOLUTION STEP'S OWN VALUES ARE NOT HELD TO THIS RUN EITHER ---
//
// A11: "WHEN a release publishes THE SYSTEM SHALL resolve the exact image
// reference the example deployment names and SHALL fail the release if that
// reference does not resolve to the digest THE RUN JUST GATED."
//
// Same shape as F22: `resolve-compose` declares IMAGE and VERSION in mayEnv and
// neither value is read. scripts/resolve-compose-image.sh compares the compose
// reference's digest against `${IMAGE}:${VERSION}`, so a VERSION that is not the
// one this run published compares against a different image.
func TestRegress0046L8_F23_ResolutionAgainstAnotherVersionIsAccepted(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: ${{ needs.build.outputs.version }}\n        run: ./scripts/resolve-compose-image.sh\n",
		"        env:\n          IMAGE: ${{ needs.build.outputs.image }}\n          VERSION: v0.0.1\n        run: ./scripts/resolve-compose-image.sh\n")
	l8MustRed(t, root, "a release whose A11 resolution compares the compose reference against a version this run never published")
}

// --- F24. A PRECEDING STEP IN THE SAME JOB STILL NEUTERS A ROLE STEP ---------
//
// A7, the full-gate half. roles.go now accounts for a role step's whole
// STRUCTURED surface - every `run:` field, `env:` at three levels, step keys,
// `defaults:` and action inputs - which is what closed F18. GitHub Actions has a
// fourth channel that is none of those: a step writes `NAME=value` to the file
// named by `$GITHUB_ENV`, or a directory to `$GITHUB_PATH`, and the runner
// applies it to every LATER step in the job.
//
// release.yml already uses that channel three steps above the full gate
// (`echo "/opt/ffmpeg/bin" >> "$GITHUB_PATH"` in `install-ffmpeg`), so this is
// house style rather than obfuscation - the same thing that made F7 a real
// finding.
//
// Recorded as a MEASUREMENT rather than a demand: closing it means deciding what
// an ordinary `run:` step does, which conductor-ruling-a6-capability-route.md
// retires outright. See the verdict - this is raised as an advisory and as a gap
// in docs/release.md's "It does NOT" list, not as a route.
func TestRegress0046L8_F24_GithubEnvNeutersTheFullGate(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n",
		"      - name: keep the gate quick on re-runs\n        run: echo \"MAKEFLAGS=-n\" >> \"$GITHUB_ENV\"\n\n      - name: the full gate (make check)\n")
	l8MustRed(t, root, "a release whose full gate is neutered by MAKEFLAGS=-n written to $GITHUB_ENV by an earlier step")
}

// F24b. The same channel through `$GITHUB_PATH`, which is how release.yml
// already puts the pinned ffmpeg in front of the gate.
func TestRegress0046L8_F24b_GithubPathShimsTheFullGate(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n",
		"      - name: use the cached make\n        run: |\n          mkdir -p /tmp/fastbin\n          printf '#!/bin/sh\\nexit 0\\n' > /tmp/fastbin/make\n          chmod +x /tmp/fastbin/make\n          echo \"/tmp/fastbin\" >> \"$GITHUB_PATH\"\n\n      - name: the full gate (make check)\n")
	l8MustRed(t, root, "a release whose `make` is a shim put in front of PATH by an earlier step through $GITHUB_PATH")
}

// --- Controls: what the loop-8 fix DID close, driven here independently ------

// S1. F18 as ordinal 7 wrote it: `make -n check` must now red.
func TestRegress0046L8_S1_MakeDryRunIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make -n check\n")
	l8MustRed(t, root, "a full gate spelled `make -n check`")
}

// S2. A spelling no self-test case names, to prove the refusal is the RULE and
// not a list: `--question` is GNU make's long form of -q.
func TestRegress0046L8_S2_AnUnlistedNeuteringFlagIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make --question check\n")
	l8MustRed(t, root, "a full gate spelled `make --question check`")
}

// S3. And the opposite direction, or S1/S2 would be satisfied by a gate that
// refuses everything: a respelt but REAL invocation still holds the role.
func TestRegress0046L8_S3_ARealRespeltInvocationStillPasses(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make -C . check\n")
	l8MustPass(t, root, "`make -C . check`, which really is the gate")
}

// S4. The environment channel the fix DOES cover: MAKEFLAGS in the workflow's
// own `env:` block reds, which is what makes F24 a gap in the CHANNEL list
// rather than in the idea.
func TestRegress0046L8_S4_MakeflagsInTheWorkflowEnvIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"  GO_VERSION: \"1.25.14\"\n", "  GO_VERSION: \"1.25.14\"\n  MAKEFLAGS: \"-n\"\n")
	l8MustRed(t, root, "MAKEFLAGS: -n in the workflow's own env block")
}

// S5. The event surface, new this loop: a `branches:` filter beside `tags:`
// reds by name.
func TestRegress0046L8_S5_ABranchFilterIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"    tags: [\"v*\"]\n", "    tags: [\"v*\"]\n    branches: [\"v0.**\"]\n")
	l8MustRed(t, root, "a push filter that admits a branch named like a version")
}

// S6. A trigger nothing plans a shape for.
func TestRegress0046L8_S6_AnUnplannedTriggerIsRefused(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"  workflow_dispatch:\n", "  workflow_dispatch:\n  schedule:\n    - cron: \"0 3 * * *\"\n")
	l8MustRed(t, root, "a `schedule:` trigger this gate plans no shape for")
}

// S7. The capability claim the whole route rests on, driven independently of
// the self-test: a publishing spelling in the dispatch-path job PASSES (it holds
// no credential) while the identical job granted packages: write REDS.
func TestRegress0046L8_S7_TheGrantIsWhatDecides(t *testing.T) {
	root := l8Fixture(t)
	l8Mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n",
		"      - name: a step that says it publishes\n        run: exec docker push ghcr.io/nschatz/holdfast:dev\n\n      - name: the full gate (make check)\n")
	l8MustPass(t, root, "a dispatch-path step spelled `exec docker push …`, which holds no credential")

	root2 := l8Fixture(t)
	l8Mutate(t, root2, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n",
		"      - name: a step that says it publishes\n        run: exec docker push ghcr.io/nschatz/holdfast:dev\n\n      - name: the full gate (make check)\n")
	l8Mutate(t, root2, ".github/workflows/release.yml",
		"      contents: read # read the tree; nothing here may write anything anywhere\n",
		"      contents: read\n      packages: write\n")
	l8MustRed(t, root2, "the identical step in a job granted packages: write")
}

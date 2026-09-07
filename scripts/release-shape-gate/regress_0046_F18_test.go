//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 7, the CAPABILITY route). Same harness as the earlier
// regress_0046_*_test.go files this loop deleted: take the REAL committed
// inputs, apply one mutation a maintainer could plausibly make, and report what
// the committed gate says about it. A probe whose mutation the gate ACCEPTS is
// a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L7 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins defects, it is
// not part of the repository's gate.
//
// Nothing here publishes, reaches a network, or executes a workflow step other
// than the planning script the gate itself runs; every mutation lands in
// t.TempDir().
//
// Naming: TestRegress0046L7_F<n>_… pins a defect (it FAILS today);
// TestRegress0046L7_S<n>_… is a probe the gate CATCHES, kept so this file is a
// measurement rather than a selection of only the cases that fail.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var l7Inputs = []string{
	".github/workflows/release.yml",
	"docker-compose.yml",
	"docs/release.md",
	"go.mod",
	"scripts/resolve-compose-image.sh",
	"scripts/release-resmoke.sh",
	"scripts/release-promote.sh",
	"scripts/smoke-image.sh",
}

// l7Fixture copies the real inputs into a scratch root and returns it.
func l7Fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range l7Inputs {
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

// l7Mutate applies one replacement and refuses a mutation that changed nothing -
// the selftest's own `changed()` rule, because a probe that graded the baseline
// would report a catch it never made.
func l7Mutate(t *testing.T, root, rel, old, new string) {
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

func l7RunGate(t *testing.T, root string) (red bool, output string) {
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

func l7MustRed(t *testing.T, root, what string) {
	t.Helper()
	red, out := l7RunGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED %s", what)
	}
}

func l7MustPass(t *testing.T, root, what string) {
	t.Helper()
	red, out := l7RunGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if red {
		t.Fatalf("the gate REFUSED %s", what)
	}
}

// P0. Sanity: the committed tree passes. Without this every "reds" assertion
// below could be satisfied by a gate that fails on everything.
func TestRegress0046L7_P0_BaselinePasses(t *testing.T) {
	l7MustPass(t, l7Fixture(t), "the committed inputs")
}

// --- F18. `make -n check` HOLDS THE FULL-GATE ROLE --------------------------
//
// A7: "THE SYSTEM SHALL report that the floating reference is promoted only
// after the full gate, both architectures' smoke runs, the version-tag push and
// the re-smoke of the artefact pulled back from the registry all appear before
// it and all must succeed."
//
// The full-gate role is `{program: "make", mustField: ["check"]}`, and
// roles.go's comparison is "first field IS the program, and every mustField
// appears as a whole word" - extra fields are allowed. GNU make's `-n` is
// dry-run: it PRINTS every recipe in `check` and executes none of them, exiting
// 0. So `make -n check` holds the full-gate role while running no gate at all,
// and the gate prints "the order holds: the full gate (make check) -> ...".
//
// This is the F13 class the declared-role mechanism exists to close: a step that
// names the gate and runs nothing. It is closed for `echo make check` (whose
// first field is not `make`) and open for every `make` flag that makes make run
// nothing - `-n`/`--dry-run`, `-q`/`--question`, `-t`/`--touch`. The mechanism
// that fixes it is already in this file: `mustNotField`, which the smoke-amd64
// role uses to refuse `--no-encode`.
//
// Reproduce the "runs nothing" half outside this test:
//
//	$ make -n check          # prints ./scripts/check-pins.sh, go test ... and runs none
//	$ echo $?                # 0

func TestRegress0046L7_F18_MakeDryRunHoldsTheFullGateRole(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make -n check\n")
	l7MustRed(t, root, "a release whose full gate is `make -n check`, which prints the recipes and runs none of them")
}

// F18b. The same hole through `-q` (question mode: make runs nothing and reports
// only whether the target is up to date).
func TestRegress0046L7_F18b_MakeQuestionModeHoldsTheFullGateRole(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make -q check\n")
	l7MustRed(t, root, "a release whose full gate is `make -q check`, which runs nothing")
}

// F18-P1. The control that keeps F18 a reading of the invocation rather than a
// demand for exact equality: `make -C . check` IS the gate and must still pass.
// (release-shape-selftest case 18 asserts the same thing.)
func TestRegress0046L7_F18P1_RespeltButRealInvocationStillPasses(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: make -C . check\n")
	l7MustPass(t, root, "a full gate respelt as `make -C . check`")
}

// F18-P2. The control that shows the role check bites at all: `echo make check`
// is refused, because its first field is not `make`. What separates it from F18
// is only which word came second.
func TestRegress0046L7_F18P2_EchoMakeCheckIsRefused(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"        run: make check\n", "        run: echo make check\n")
	l7MustRed(t, root, "a full gate reduced to `echo make check`")
}

// --- F19. THE GATE PRINTS TWO CLAIMS IT DOES NOT CHECK ----------------------
//
// The gate ends a green run with, verbatim:
//
//	ok: the promotion retags ghcr.io/nschatz/holdfast:latest onto
//	    ghcr.io/nschatz/holdfast:v0.1.0 - the same digest, not a rebuild
//	ok: on a version tag push the order holds: ... -> the re-smoke of the
//	    pulled artefact -> ...
//
// Both are statements about what scripts/release-promote.sh and
// scripts/release-resmoke.sh DO, and the gate reads neither file beyond
// os.Stat. The conductor's capability ruling forbids the gate deciding what a
// step's script does, so these probes are NOT a request for a reader - they
// pin the two sentences, which assert a property nothing checked. The
// self-test's own case 61 (expect_absent) sets exactly that standard for A6's
// sentence.
//
// F19a's mutation is impl-gate F5's own probe from ordinal 1, which the gate
// CAUGHT then: the promotion re-pointed at a locally built image the run never
// pushed or pulled back.

func TestRegress0046L7_F19a_PromotionFromALocalBuildIsAcceptedAndReassuredAbout(t *testing.T) {
	root := l7Fixture(t)
	p := filepath.Join(root, "scripts/release-promote.sh")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = `"${image}:${version}"`
	body := string(raw)
	if !strings.Contains(body, anchor) {
		t.Fatalf("anchor %q absent from scripts/release-promote.sh:\n%s", anchor, body)
	}
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, anchor, "holdfast:release")), 0o755); err != nil {
		t.Fatal(err)
	}
	red, out := l7RunGate(t, root)
	t.Logf("gate stdout:\n%s", out)
	if !red && strings.Contains(out, "the same digest, not a rebuild") {
		t.Fatalf("the gate PASSED a promotion that points the floating reference at a locally built image the run never pushed, and printed `the same digest, not a rebuild` over it")
	}
	if !red {
		t.Fatalf("the gate PASSED a promotion that points the floating reference at a locally built image")
	}
}

func TestRegress0046L7_F19b_AGuttedResmokeStillHoldsTheRole(t *testing.T) {
	root := l7Fixture(t)
	if err := os.WriteFile(filepath.Join(root, "scripts/release-resmoke.sh"),
		[]byte("#!/usr/bin/env bash\n# pulls nothing back, smokes nothing.\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l7MustRed(t, root, "a release whose re-smoke of the pulled-back artefact pulls nothing and smokes nothing")
}

// --- probes the gate CATCHES ------------------------------------------------
//
// Kept so this file measures the capability route rather than selecting only
// what fails. Each of these is a way A12 names of handing the dispatch path
// authority, or an expression the evaluator has to refuse rather than guess.

// S1. A repository secret declared in the WORKFLOW-level `env:`, which every job
// inherits. capability.go's secret scan walks the JOB node only, so it does not
// see this one - but the gate still reds, because stepEnv interpolates the
// workflow env for the plan step and `secrets.…` is not a context this run
// produced. Fail-closed by a different route than the one the message names.
func TestRegress0046L7_S1_WorkflowLevelEnvSecretIsRefused(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"env:\n  # No staticcheck/govulncheck pins here",
		"env:\n  GHCR_PAT: ${{ secrets.GHCR_PUBLISH_PAT }}\n  # No staticcheck/govulncheck pins here")
	l7MustRed(t, root, "a workflow-level env that hands every dispatch-path job a repository secret")
}

// S2. The same secret one level down, in the dispatch-path job's own `env:`.
func TestRegress0046L7_S2_JobLevelEnvSecretIsCaught(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"  build:\n    runs-on: ubuntu-latest\n",
		"  build:\n    runs-on: ubuntu-latest\n    env:\n      GHCR_PAT: ${{ secrets.GHCR_PUBLISH_PAT }}\n")
	l7MustRed(t, root, "a build-job env that hands the dispatch path a repository secret")
}

// S3. A secret reaching a dispatch-path step through an action input rather than
// through `env:` - `scalars(job.Node)` is what makes this one visible.
func TestRegress0046L7_S3_SecretInADispatchPathActionInputIsCaught(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"      - name: build the release binaries\n",
		"      - name: log in so testers can pull dispatch builds\n"+
			"        id: dispatch-login\n"+
			"        uses: docker/login-action@v3\n"+
			"        with:\n"+
			"          registry: ghcr.io\n"+
			"          username: ${{ github.actor }}\n"+
			"          password: ${{ secrets.GHCR_PUBLISH_PAT }}\n\n"+
			"      - name: build the release binaries\n")
	l7MustRed(t, root, "a dispatch-path step handed a repository secret through an action input")
}

// S4. The publishing job's guard replaced by a function the evaluator does not
// implement. Fail-closed is the whole design here: an undecidable guard must not
// evaluate to "does not run".
func TestRegress0046L7_S4_UndecidableGuardOnThePublishingJobIsRefused(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"    if: needs.build.outputs.publish == 'true'\n",
		"    if: fromJSON(needs.build.outputs.publish)\n")
	l7MustRed(t, root, "a publishing job whose guard uses an expression function the gate cannot evaluate")
}

// S5. A guard reading a context path the run never produced.
func TestRegress0046L7_S5_GuardOnAnUnproducedContextIsRefused(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"    if: needs.build.outputs.publish == 'true'\n",
		"    if: github.event.inputs.publish == 'true'\n")
	l7MustRed(t, root, "a publishing job guarded on github.event.inputs, which no dispatch here produces")
}

// S6. The workflow's top-level grant widened while the dispatch-path job's own
// block is removed, so the workflow's write-all is what the job inherits.
func TestRegress0046L7_S6_InheritedWriteAllIsCaught(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml",
		"permissions:\n  contents: read\n", "permissions: write-all\n")
	l7Mutate(t, root, ".github/workflows/release.yml",
		"    permissions:\n      contents: read # read the tree; nothing here may write anything anywhere\n", "")
	l7MustRed(t, root, "a dispatch-path job inheriting the workflow's write-all")
}

// S7. A job written as a YAML alias. The node walk sees an AliasNode, so
// `mappingValue`/`mappingKeys`/`scalars` all yield nothing for it and the clone
// is never reported - but the ANCHOR job is a real mapping node, is walked, and
// reds, so the definition as a whole is refused. (GitHub Actions does not accept
// anchors in a workflow file at all, so a definition shaped like this does not
// load there either.)
func TestRegress0046L7_S7_AnAnchoredPublishingJobIsCaught(t *testing.T) {
	root := l7Fixture(t)
	l7Mutate(t, root, ".github/workflows/release.yml", "\n  build:\n", `
  sidecar: &sidecar
    runs-on: ubuntu-latest
    permissions:
      packages: write
    steps:
      - name: push a dev image so testers can pull dispatch builds
        id: sidecar-push
        run: docker push ghcr.io/nschatz/holdfast:dev

  sidecar-clone: *sidecar

  build:
`)
	l7MustRed(t, root, "an anchored dispatch-path job granted packages: write, cloned by an alias")
}

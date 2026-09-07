//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 4). Same harness as regress_0046_F1_test.go,
// regress_0046_F5_test.go and regress_0046_F7_test.go: take the REAL committed
// inputs, apply one mutation a maintainer could plausibly make, and report what
// the committed gate says about it. A probe whose mutation the gate ACCEPTS is a
// hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L4 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins defects, it is
// not part of the repository's gate.
//
// Nothing here publishes: every mutation lands in t.TempDir(), the gate executes
// only the planning steps under stubbed binaries, and the one probe that runs a
// script under a real bash puts a recording stub named `docker` first on PATH.
//
// --- WHAT THESE PROBE ------------------------------------------------------
//
// Ordinal 3's F7 was that the `run:` detectors could not cross a shell line
// continuation. The conductor ruled the fix must be ONE normalisation rather
// than a fourth pattern, and ruled that another member of the same class - a
// publishing act the gate reports as publishing nothing - is a finding about
// the APPROACH.
//
// The normalisation itself holds, and reaches every detector (P3-P7 measure
// that). These probes are the class either side of it:
//
//   - F9/F9b/F9c/F9d: the `run:` half of the act catalogue has no model of a
//     DESTINATION at all. It matches the literal flag `--push` and nothing else,
//     so `docker buildx build --output=type=registry` - which this diff's own
//     CLAUDE.md paragraph calls the thing `push:` is SHORTHAND FOR, and which
//     ordinal 2's F5 closed on the `uses:` half - performs no act. Neither does
//     `docker image push`, the management-command spelling of `docker push`.
//     None of these probes involves a line continuation.
//   - F10/F10s: the normalisation's own two passes interact. StripShellComments
//     resets its quote state at every PHYSICAL line, so a `#` on a continuation
//     line inside a string opened on an EARLIER line is read as a comment,
//     truncating the rest of the LOGICAL line - including the `--push` and the
//     backslash that would have joined it. F10s is the ground truth: the same
//     text under a real bash, which pushes.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- F9. THE RUN HALF HAS NO DESTINATION MODEL ------------------------------

// F9. buildx's own documented spelling for pushing without `--push`.
// `docker/build-push-action`'s input table defines `push:` as "shorthand for
// `--output=type=registry`" - the sentence ordinal 2's fix is built on, quoted
// in this diff's CLAUDE.md. On the `run:` side that sentence buys nothing: the
// only destination the detectors know is the literal flag `--push`. This dry
// run pushes ghcr.io/nschatz/holdfast:dev to a public registry, on ONE physical
// line, with no continuation anywhere.
func TestRegress0046L4_F9_DispatchPushesViaOutputTypeRegistry(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 --output=type=registry,name=ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev with `docker buildx build --output=type=registry`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F9b. The same act in the short flag spelling buildx also accepts, with the
// `type=image,...,push=true` attributes ordinal 2 taught the `uses:` half.
func TestRegress0046L4_F9b_DispatchPushesViaShortOutputFlag(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 -o type=image,name=ghcr.io/nschatz/holdfast:dev,push=true .`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes through `-o type=image,...,push=true`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F9c. And the plainest one: `docker image push` is the management-command form
// of `docker push`, documented and interchangeable with it. The detector is
// `\bdocker\s+push\b`, so the word between them hides the act.
func TestRegress0046L4_F9c_DispatchPushesViaDockerImagePush(t *testing.T) {
	root := withDevRunStep(t,
		`docker image push ghcr.io/nschatz/holdfast:dev`)
	mustRedNaming(t, root,
		"a release definition whose dry run runs `docker image push`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F9d. Belt and braces on F9, so nobody can argue the push would merely 401:
// the GHCR login is unguarded too. This dry run authenticates and THEN
// publishes.
func TestRegress0046L4_F9d_DispatchLogsInAndPushesViaOutputTypeRegistry(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: log in to GHCR\n        if: steps.plan.outputs.publish == 'true'\n",
		"      - name: log in to GHCR\n")
	mutate(t, root, ".github/workflows/release.yml", anchor,
		devRunStep(`docker buildx build --platform linux/amd64 --output=type=registry,name=ghcr.io/nschatz/holdfast:dev .`)+anchor)
	mustRedNaming(t, root,
		"a dry run that logs in to GHCR and then pushes with `--output=type=registry`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// --- F10. THE NORMALISATION'S TWO PASSES INTERACT ---------------------------

// f10Script is the `run:` body used by F10 and F10s, written once so the probe
// the GATE sees and the probe the SHELL sees cannot drift apart.
var f10Script = []string{
	`docker buildx build \`,
	`  --annotation "org.opencontainers.image.description=dev build, \`,
	`  see #123" \`,
	`  --push \`,
	`  --platform linux/amd64 \`,
	`  -t ghcr.io/nschatz/holdfast:dev \`,
	`  .`,
}

// F10. StripShellComments declares its quote state INSIDE the per-line loop
// (workflow.go:657-661), so a double quote opened on one physical line is not
// open on the next. Put a `#` on a continuation line inside such a string and
// the stripper truncates that line as if it were a comment - which also removes
// the trailing backslash, so JoinShellContinuations then does not join, and the
// `--push` below lands on a separate logical line that `[^\n]*` cannot reach.
//
// `#` inside a quoted argument is ordinary text to the shell, and an OCI
// annotation or a release note carrying an issue number (`see #123`) is the
// everyday way it arrives.
func TestRegress0046L4_F10_CommentStripperEatsAContinuedQuotedPush(t *testing.T) {
	root := withDevRunStep(t, f10Script...)
	mustRedNaming(t, root,
		"a dry run whose `--push` sits after a quoted argument containing a `#`, across a continuation",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F10s. The ground truth for F10, so the finding rests on what BASH does with
// that text rather than on a reading of it. The identical script runs under
// `bash -e` - the shell GitHub gives a `run:` step on Linux, and the one
// shell.go itself execs - with a recording stub named `docker` first on PATH.
// The stub's log shows the push.
func TestRegress0046L4_F10s_TheShellReallyPerformsThatPush(t *testing.T) {
	argv := runUnderBash(t, append([]string{"set -euo pipefail"}, f10Script...))
	t.Logf("what bash actually invoked:\n%s", argv)
	for _, want := range []string{"<--push>", "<ghcr.io/nschatz/holdfast:dev>"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("bash did not invoke docker with %s; the F10 probe would then be measuring the wrong thing:\n%s", want, argv)
		}
	}
}

// runUnderBash executes a script with a stub `docker` on PATH that records its
// argv and performs nothing, and returns what the stub was called with.
func runUnderBash(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "argv.log")
	stub := "#!/bin/sh\n" +
		"printf 'docker' >> \"$ARGV_LOG\"\n" +
		"for a in \"$@\"; do printf ' <%s>' \"$a\" >> \"$ARGV_LOG\"; done\n" +
		"printf '\\n' >> \"$ARGV_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(script, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGV_LOG="+log)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script did not run under bash: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("cannot read the stub log: %v", err)
	}
	return string(raw)
}

// --- P0-P7. The measurement around them -------------------------------------
//
// These pass on purpose, so this file is a measurement rather than a selection
// of only the cases that fail.

// P0. The committed tree still passes. Without this, every "reds" assertion
// above could be met by a gate that fails on everything.
func TestRegress0046L4_P0_BaselineStillPasses(t *testing.T) {
	mustPass(t, fixture(t), "the committed inputs")
}

// P1. The control for F9: the SAME act, spelled with the one flag the catalogue
// knows. CAUGHT - so F9 is a hole in the destination model, not in the harness.
func TestRegress0046L4_P1_TheSameActSpelledWithPushIsCaught(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 --push -t ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root, "the same build spelled `--push`",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P2. The honest other direction for F9: a LOCAL exporter must still pass. Any
// fix for F9 that reds every `--output=` would be the false positive this probe
// forbids, exactly as self-test case 3i does for the `uses:` half.
func TestRegress0046L4_P2_LocalExporterInARunStepStaysLocal(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 --output=type=docker,dest=/tmp/img.tar .`)
	mustPass(t, root, "a run step whose buildx output is the local docker exporter")
}

// P3. The continuation normalisation this pass was scoped to, re-measured on
// the plain case. CAUGHT.
func TestRegress0046L4_P3_ContinuedBuildxPushIsCaught(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build \`,
		`  --push \`,
		`  -t ghcr.io/nschatz/holdfast:dev \`,
		`  .`)
	mustRedNaming(t, root, "a line-continued `docker buildx build --push`",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P4. The normalisation's parity rule in the direction that must NOT join: a
// doubled trailing backslash is an escaped literal that ends the command, so
// the build below is its own command line and there is nothing to join. It is
// still caught - which asserts the gate does not red for the WRONG reason here.
func TestRegress0046L4_P4_EscapedTrailingBackslashDoesNotJoin(t *testing.T) {
	root := withDevRunStep(t,
		`printf '%s' 'a\\`,
		`docker buildx build --push -t ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root, "an escaped trailing backslash followed by a real one-line push",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P5. The comment pass in the direction it gets right: a `--push` that appears
// ONLY inside a comment is prose and must not be read as an act.
func TestRegress0046L4_P5_PushOnlyInACommentIsStillProse(t *testing.T) {
	root := withDevRunStep(t,
		`# a real release would add --push here`,
		`docker buildx build --load -t holdfast:dev .`)
	mustPass(t, root, "a `--push` that appears only in a comment")
}

// P6. The claim that the normalisation reaches EVERY detector, graded rather
// than eyeballed: rewrite the full gate as `make \` + `check` and the gate must
// still SEE a full gate. A detector reading physical lines would red here with
// "runs no full gate" - fail-closed, but still the normalisation not reaching
// the ordering half.
func TestRegress0046L4_P6_OrderingPropertiesReadTheJoinedText(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n        run: make check\n",
		"      - name: the full gate (make check)\n        run: |\n          make \\\n            check\n")
	mustPass(t, root, "a full gate written as a continued `make \\` + `check`")
}

// P7. The same claim for PullArches, which decides whether BOTH architectures
// were pulled back and re-smoked before `:latest` moved - the property A7 is
// about. The arm64 pull is respelled across two continuations.
func TestRegress0046L4_P7_PullArchesReadsTheJoinedText(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"          docker pull --platform linux/arm64 \"$REF\"\n",
		"          docker pull \\\n            --platform linux/arm64 \\\n            \"$REF\"\n")
	mustPass(t, root, "an arm64 pull-back written across two continuations")
}

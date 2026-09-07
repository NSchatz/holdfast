//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 5). Same harness as regress_0046_F1_test.go,
// regress_0046_F5_test.go, regress_0046_F7_test.go and regress_0046_F9_test.go:
// take the REAL committed inputs, apply one mutation a maintainer could
// plausibly make, and report what the committed gate says about it. A probe
// whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L5 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins defects, it is
// not part of the repository's gate.
//
// Nothing here publishes: every mutation lands in t.TempDir(), the gate executes
// only the planning steps under stubbed binaries, and the probes that run a
// script under a real bash put a recording stub named `docker` first on PATH.
//
// --- WHAT THESE PROBE ------------------------------------------------------
//
// Ordinal 4's F9 was that the `run:` half of the act catalogue had no
// destination model and no edge. The conductor ruled the spelling catalogue OUT
// and required two things in its place: (1) a destination model that decides a
// `run:` step by where its invocation sends bytes, and (2) an EDGE - "an
// unrecognised publishing-capable invocation must FAIL naming the step and the
// command, never contribute silence".
//
// Both were built, and F9/F9b/F9c/F9d and F10 all pass now. These probes are
// what the new single reader does at three of its own seams:
//
//   - F11/F11b: a command the shell runs from a QUOTED word. `sh -c "docker
//     push …"` and `eval "docker push …"` are one word to the new lexer, and
//     Command.strayRegistryTool skips any word containing whitespace, so the
//     invocation is neither decided nor reported. This is a REGRESSION as well
//     as a hole: the regex catalogue this pass deleted matched raw text, so
//     `\bdocker\s+push\b` found that push inside the quotes.
//   - F12: `-otype=registry`. buildx's `-o` is a pflag shorthand and takes an
//     attached value, so `-otype=registry,name=…` IS `--output=type=registry`.
//     decideBuildDestination only reads `-o`, `-o=` and `--output[=]`, so this
//     spelling falls through its `default: continue` and the build is reported
//     as having no destination flag at all - not an error, a silence.
//   - F13: the SAME quote resolution, in the ORDERING half. Step.script() now
//     renders each command with its quoting removed, so `echo "make check"` is
//     the text `echo make check` by the time reMakeCheck sees it, and a step
//     that only PRINTS the gate's name satisfies A7's "the full gate appears
//     before the promotion". Before this pass the quote blocked that match;
//     F13r measures both sides of that change with the committed regex.
//
// F11s, F12g, F13r and the buildx run recorded in the verdict are the ground
// truths, so the findings rest on what the tools actually do rather than on a
// reading of them.

import (
	"strings"
	"testing"
)

// --- F11. A PUBLISHING COMMAND INSIDE A QUOTED WORD -------------------------

// F11. `sh -c "docker push …"`. The shell runs the push; the gate says nothing
// publishes.
//
// ShellCommands lexes the double-quoted span into the CURRENT WORD, so the
// command the sub-shell will run is a single argv element containing spaces.
// Command.invocation then reports the program as `sh`, which is not in
// registryTools, and Command.strayRegistryTool - the backstop that exists for
// exactly this shape - skips every word containing whitespace ("a quoted
// argument, not a program name"). Neither an act nor an error comes back, and
// the gate prints "NONE of them publishes anything" over a dry run that pushes
// ghcr.io/nschatz/holdfast:dev to a public registry.
func TestRegress0046L5_F11_DispatchPushesFromInsideAQuotedShellCommand(t *testing.T) {
	root := withDevRunStep(t,
		`sh -c "docker push ghcr.io/nschatz/holdfast:dev"`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev via `sh -c \"docker push …\"`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F11b. The same hole through `eval`, which the gate DOES list as a wrapper -
// and stepping over the wrapper is what leaves the quoted command as the
// program name. `eval "docker push …"` is therefore silent for the same reason
// with one fewer excuse: commandWrappers already says the act belongs to what
// comes after `eval`.
func TestRegress0046L5_F11b_DispatchPushesFromInsideAnEvaledString(t *testing.T) {
	root := withDevRunStep(t,
		`eval "docker push ghcr.io/nschatz/holdfast:dev"`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev via `eval \"docker push …\"`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F11s. The ground truth for F11 and F11b: the identical lines run under
// `bash -e` - the shell GitHub gives a `run:` step on Linux, and the one
// shell.go itself execs - with a recording stub named `docker` first on PATH.
// The stub's log shows both pushes.
func TestRegress0046L5_F11s_TheShellReallyPerformsBothPushes(t *testing.T) {
	argv := runUnderBash(t, []string{
		"set -euo pipefail",
		`sh -c "docker push ghcr.io/nschatz/holdfast:dev"`,
		`eval "docker push ghcr.io/nschatz/holdfast:dev"`,
	})
	t.Logf("what bash actually invoked:\n%s", argv)
	if n := strings.Count(argv, "docker <push> <ghcr.io/nschatz/holdfast:dev>"); n != 2 {
		t.Fatalf("bash performed %d push(es), not 2; the F11 probes would then be measuring the wrong thing:\n%s", n, argv)
	}
}

// --- F12. THE ATTACHED SHORT-FLAG SPELLING OF --output ----------------------

// F12. `docker buildx build -otype=registry,name=…` publishes, and the gate
// reports the step as having no destination flag at all.
//
// decideBuildDestination matches `--output`, `-o`, `--output=` and `-o=`. An
// attached shorthand value (`-otype=…`) is none of those, so it reaches the
// `default: continue` branch and the loop ends with (kind "", err nil) - the
// same silence the destination model was built to remove, one flag spelling
// further along. It is not the edge's job to catch this: the invocation DID
// land on a rule (`docker buildx build`), and that rule decided "local".
//
// The ground truth is a real buildx, recorded in the verdict:
//
//	printf 'FROM scratch\n' | docker buildx build \
//	  -otype=registry,name=localhost:1/holdfast-probe:dev -f - <ctx>
//	=> #3 naming to localhost:1/holdfast-probe:dev done
//	   #3 pushing layers done
//	   #3 ERROR: failed to push localhost:1/holdfast-probe:dev: … dial tcp [::1]:1
func TestRegress0046L5_F12_DispatchPushesViaAnAttachedShortOutputFlag(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 -otype=registry,name=ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root,
		"a release definition whose dry run pushes through `-otype=registry`",
		"on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds",
		"WOULD RUN")
}

// F12g. The half of F12 that is measurable without a builder, and the one the
// finding turns on: the gate decides the two spellings of ONE flag differently.
// `--output=type=registry` is a decided publish; the byte-equivalent
// `-otype=registry` is neither an act nor an error.
func TestRegress0046L5_F12g_TheTwoSpellingsOfOneFlagDisagree(t *testing.T) {
	spec := `type=registry,name=ghcr.io/nschatz/holdfast:dev`

	long := Command{Words: strings.Fields(`docker buildx build --platform linux/amd64 --output=` + spec + ` .`)}
	kind, why, err := long.Act()
	if err != nil || kind == "" {
		t.Fatalf("--output=%s should be a decided publish; got kind=%q why=%q err=%v", spec, kind, why, err)
	}

	short := Command{Words: strings.Fields(`docker buildx build --platform linux/amd64 -o` + spec + ` .`)}
	kind, why, err = short.Act()
	if kind != "" || err != nil {
		t.Logf("-o%s is decided: kind=%q why=%q err=%v", spec, kind, why, err)
		return
	}
	t.Fatalf("`-o%s` is buildx's attached-shorthand spelling of `--output=%s`, which the line above proves the gate calls a publish - yet this one is neither an act nor an error, so the step is reported as publishing nothing", spec, spec)
}

// --- F13. QUOTE REMOVAL REACHES THE ORDERING HALF TOO -----------------------

// F13. The full gate satisfied by a step that only PRINTS its name.
//
// Step.script() used to be the step's raw text (comments stripped,
// continuations joined). It is now the reader's command list rendered back out
// with QUOTING REMOVED, and every role detector below it still matches that
// string. reMakeCheck requires `make` to sit at the start of the string or
// after one of `[\s;&|(]`, and a `"` used to be neither - so `echo "make
// check"` did not read as a full gate before this pass and does now.
//
// A3 and A7 are stated in terms of that role: the promotion may only follow the
// full gate, and after a failed gate nothing publishes. A definition whose gate
// step has been reduced to an echo therefore passes both, which is the
// fail-open direction on the property this whole gate exists to hold.
func TestRegress0046L5_F13_AnEchoedGateNameSatisfiesTheFullGateRole(t *testing.T) {
	root := fixture(t)
	mutate(t, root, ".github/workflows/release.yml",
		"      - name: the full gate (make check)\n        run: make check\n",
		"      - name: the full gate (make check)\n        run: echo \"make check\"\n")
	red, out := runGateCapturingStderr(t, root)
	t.Logf("gate output:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED a release definition whose only \"full gate\" step is `echo \"make check\"` - it prints the gate's name and runs nothing, yet the promotion is still reported as gated")
	}
}

// F13r. The ground truth for F13 being a REGRESSION rather than an inherited
// hole, measured with the committed regex against both texts: the raw `run:`
// body (what Step.script() returned before this pass, there being no comment
// and no continuation in it) and what it returns now.
func TestRegress0046L5_F13r_QuoteRemovalIsWhatMadeTheEchoCount(t *testing.T) {
	body := `echo "make check"`
	before := reMakeCheck.MatchString(body)
	after := (Step{Run: body}).RunsFullGate()
	t.Logf("raw body %q -> full gate? %v", body, before)
	t.Logf("script() %q -> full gate? %v", (Step{Run: body}).script(), after)
	if before {
		t.Skip("the quote never blocked this match, so F13 is inherited rather than new")
	}
	if !after {
		t.Fatal("the quoted echo does not read as a full gate; F13 does not reproduce")
	}
}

// F13b. The same seam in the other role the ordering property is stated in: a
// step that merely mentions the smoke script in a quoted message. reSmoke has
// no boundary at all, so this one was already true before the pass and is
// recorded as the control that separates F13 (new) from it (inherited).
func TestRegress0046L5_F13b_AnEchoedSmokeNameWasAlreadyEnough(t *testing.T) {
	s := Step{Run: `echo "smoke-image.sh will run on the next runner"`}
	if !s.RunsSmoke() {
		t.Skip("reSmoke no longer matches a quoted mention; F13's seam is then the only one")
	}
	t.Logf("recorded: a quoted mention of smoke-image.sh reads as a smoke run; script() = %q", s.script())
}

// --- P0-P5. The measurement around them -------------------------------------
//
// These pass on purpose, so this file is a measurement rather than a selection
// of only the cases that fail.

// P0. The committed tree still passes. Without this, every "reds" assertion
// above could be met by a gate that reds on everything.
func TestRegress0046L5_P0_BaselineStillPasses(t *testing.T) {
	mustPass(t, fixture(t), "the committed inputs")
}

// P1. The control for F11: the SAME command, unquoted. CAUGHT - so F11 is the
// quoting, not the harness.
func TestRegress0046L5_P1_ThePlainDockerPushIsCaught(t *testing.T) {
	root := withDevRunStep(t, `docker push ghcr.io/nschatz/holdfast:dev`)
	mustRedNaming(t, root, "a plain `docker push`",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P2. The control for F12: the SAME destination, spelled with a separated
// value. CAUGHT - so F12 is the attached shorthand, not the exporter model.
func TestRegress0046L5_P2_TheSeparatedShortOutputFlagIsCaught(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 -o type=registry,name=ghcr.io/nschatz/holdfast:dev .`)
	mustRedNaming(t, root, "`-o type=registry` with a separated value",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P3. The honest other direction for F12: an attached shorthand naming a LOCAL
// exporter must not become a false positive when F12 is fixed. It passes today
// for the wrong reason (the flag is not read at all); after a fix it must still
// pass for the right one.
func TestRegress0046L5_P3_AnAttachedShortFlagNamingALocalExporterStaysLocal(t *testing.T) {
	root := withDevRunStep(t,
		`docker buildx build --platform linux/amd64 -otype=docker,dest=/tmp/img.tar .`)
	mustPass(t, root, "an attached `-otype=docker,dest=…` local exporter")
}

// P4. The edge itself, re-measured: an unmodelled invocation of a registry tool
// still reds by name. This is what ordinal 4's ruling asked for and what the
// pass delivered, and F11/F12 are the two shapes that get past it rather than
// evidence it is absent.
func TestRegress0046L5_P4_AnUnmodelledRegistryInvocationStillReds(t *testing.T) {
	root := withDevRunStep(t, `crane copy ghcr.io/nschatz/holdfast:dev ghcr.io/nschatz/holdfast:latest`)
	mustRedNaming(t, root, "an unmodelled `crane copy`",
		"publish a dev image so testers can pull dispatch builds",
		"crane")
}

// P5. The honest other direction for F13: the committed full-gate step must
// still read as a full gate, and a fix for F13 must not red the real thing.
func TestRegress0046L5_P5_TheRealFullGateStillReadsAsOne(t *testing.T) {
	if !(Step{Run: "make check"}).RunsFullGate() {
		t.Fatal("`make check` no longer reads as the full gate")
	}
}

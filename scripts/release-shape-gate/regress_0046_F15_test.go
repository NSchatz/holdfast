//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 6), second finding. Harness as regress_0046_F14_test.go.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L6_F15 -v ./scripts/release-shape-gate/...
//
// F15 is the OTHER half of the observation's silence, and unlike F14 it needs no
// PATH, no trap and no path spelling: `exec` is a shell builtin, so the program
// it runs is resolved by the shell itself and NEVER reaches
// command_not_found_handle. The environment records nothing, the shell dies at
// that line, and everything after it goes unobserved as well - while the runner
// would really perform the exec'd command.
//
// The environment already records a marker for "this script reached its end"
// (obsComplete), which would separate a truncated observation from a complete
// one. Observe() throws that marker away: `case obsComplete: continue`. Nothing
// in the gate ever asks whether the script finished.

import (
	"strings"
	"testing"
)

// F15. `exec docker push …` publishes, and the gate reports the step as
// publishing nothing.
//
// command.go lists `exec` in commandWrappers ("the act belongs to what comes
// after them") and in programsCheckedAndLocal ("runs another program ... the act
// belongs to that program"), so the gate believes it decides this shape. It
// cannot: nothing is recorded for it to decide.
func TestRegress0046L6_F15_ExecReachesNoRecorderAtAll(t *testing.T) {
	withObserver(t)
	step := Step{Name: "publish a dev image so testers can pull dispatch builds",
		Run: "exec docker push ghcr.io/nschatz/holdfast:dev"}
	obs, oerr := step.Observed(nil)
	if oerr != nil {
		t.Fatalf("observing: %v", oerr)
	}
	acts, err := step.Acts(nil)
	t.Logf("observed invocations: %q\nrefusals: %q\nacts=%v err=%v", step.script(), obs.Refusals, acts, err)
	if len(acts) > 0 || err != nil {
		t.Skip("the gate decides this shape after all; F15 does not reproduce")
	}
	t.Fatalf("`exec docker push …` performs a push in the runner (F15s measures it under real bash) and this environment recorded NO invocation, raised NO refusal and reported NO act. `exec` is a builtin, so the program never reaches command_not_found_handle - and the shell then exits, leaving anything after it unobserved too")
}

// F15b. The truncation the same builtin causes, made visible: the push below is
// never observed either, because the shell is gone. The environment records a
// completion marker for exactly this, and Observe() discards it.
func TestRegress0046L6_F15b_TheRestOfTheStepIsUnobservedAndNobodyChecks(t *testing.T) {
	withObserver(t)
	run := "exec docker version\ndocker push ghcr.io/nschatz/holdfast:dev\n"
	step := Step{Name: "probe", Run: run}
	obs, err := step.Observed(nil)
	if err != nil {
		t.Fatalf("observing: %v", err)
	}
	t.Logf("observed invocations: %q\nconverged=%v refusals=%q", step.script(), obs.Converged, obs.Refusals)
	if strings.Contains(step.script(), "push") {
		t.Skip("the push after the exec was observed; F15b does not reproduce")
	}
	acts, aerr := step.Acts(nil)
	if len(acts) > 0 || aerr != nil {
		t.Skip("the step was refused; F15b does not reproduce")
	}
	t.Fatalf("the step's second line is a plain `docker push` and the observation ended before it, with no refusal: an observation that stopped early is indistinguishable from one that saw everything")
}

// F15s. The ground truth, on the same harness the earlier ordinals used: the
// identical line under `bash -e` - the shell GitHub gives a Linux `run:` step -
// with a recording stub named `docker` first on PATH.
func TestRegress0046L6_F15s_TheShellReallyPerformsTheExecdPush(t *testing.T) {
	argv := argvUnderRealBash(t, "exec docker push ghcr.io/nschatz/holdfast:dev", "docker")
	t.Logf("what bash actually invoked: %q", argv)
	if len(argv) != 1 || !strings.Contains(argv[0], "docker <push> <ghcr.io/nschatz/holdfast:dev>") {
		t.Fatalf("bash did not perform the push, so F15 would be measuring the wrong thing: %q", argv)
	}
}

// F15g. The finding as A6 and A12 state it, end to end through the committed
// gate: a manual dispatch that pushes through `exec`. The gate must fail naming
// the step; it passes.
func TestRegress0046L6_F15g_TheGatePassesADispatchThatPublishesThroughExec(t *testing.T) {
	root := withDevRunStep(t, "exec docker push ghcr.io/nschatz/holdfast:dev")
	red, out := runGateCapturingStderr(t, root)
	t.Logf("gate output:\n%s", out)
	if !red {
		t.Fatalf("the gate PASSED a release definition whose dry run pushes ghcr.io/nschatz/holdfast:dev via `exec docker push …`")
	}
}

// --- P3-P4. The measurement around F15 --------------------------------------

// P3. The control: the SAME push without `exec`. CAUGHT - so F15 is the builtin,
// not the harness.
func TestRegress0046L6_P3_TheSamePushWithoutExecIsCaught(t *testing.T) {
	root := withDevRunStep(t, "docker push ghcr.io/nschatz/holdfast:dev")
	mustRedNaming(t, root, "a plain `docker push`",
		"on a manual dispatch", "publish a dev image so testers can pull dispatch builds", "WOULD RUN")
}

// P4. The honest other direction: `exec` with only a redirection runs no program
// at all and must not become a false positive when F15 is fixed.
func TestRegress0046L6_P4_ExecWithOnlyARedirectionIsNotAnAct(t *testing.T) {
	withObserver(t)
	acts, err := (Step{Name: "probe", Run: "exec 3>&1\nmake check\n"}).Acts(nil)
	if err != nil {
		t.Fatalf("`exec 3>&1` was refused: %v", err)
	}
	if len(acts) != 0 {
		t.Fatalf("`exec 3>&1` reported acts: %v", acts)
	}
}

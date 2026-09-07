//go:build regress0046

package main

// Regression probes written by the S0046-holdfast-release-2 implementation gate
// (impl ordinal 6). Same harness as the earlier regress_0046_*_test.go files:
// take the REAL committed inputs, apply one mutation a maintainer could
// plausibly make, and report what the committed gate says about it. A probe
// whose mutation the gate ACCEPTS is a hole.
//
// Run:  go test -tags regress0046 -count=1 -run TestRegress0046L6 -v ./scripts/release-shape-gate/...
//
// Build-tagged so it stays OUT of `make check`: this file pins defects, it is
// not part of the repository's gate.
//
// NOTHING HERE REACHES A NETWORK. The act these probes prove really executes is
// a `git push` into a BARE REPOSITORY ON DISK, created by the probe in
// t.TempDir(). That is a genuine ref push performed by the real git binary - the
// property under test - with no remote host involved.
//
// --- WHAT THESE PROBE ------------------------------------------------------
//
// The conductor's ruling (conductor-ruling-a6-grade-route.md) moved A6, A7 and
// A12 off static inspection and onto OBSERVED EXECUTION, on two conditions:
//
//	2. "observe what its script actually invokes, in a hermetic environment
//	   where NO EXTERNAL COMMAND RUNS FOR REAL and every external invocation is
//	   recorded with its full argv"
//	3. "DENY BY DEFAULT. An external invocation the environment does not
//	   recognise FAILS naming the step and the program. Silence must not be
//	   reachable from any spelling, quoting or nesting."
//
// The environment's ONLY barrier against a bare command word is that PATH is a
// single empty directory, so the name reaches command_not_found_handle. A step
// that puts a real directory back on PATH - or that names a program by a
// relative path the DEBUG guard finds executable - is executed by bash for
// real, is recorded nowhere, and is therefore reported as publishing nothing.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localGitPair builds a source repository with one commit and a bare repository
// to push it into, both on disk in t.TempDir(). `git push <dir>` against a bare
// repository is a real publish that touches no network, so it is the safe way to
// measure whether the observation environment really executed a command.
func localGitPair(t *testing.T) (src, remote string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "src")
	remote = filepath.Join(dir, "remote.git")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=probe@example.invalid",
			"GIT_COMMITTER_NAME=probe", "GIT_COMMITTER_EMAIL=probe@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--bare", "-b", "main", remote)
	git("init", "-b", "main", src)
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", src, "add", "a")
	git("-C", src, "commit", "-m", "probe")
	return src, remote
}

// remoteRefs is what the bare repository actually holds. A ref that is there was
// put there by a real `git push`.
func remoteRefs(t *testing.T, remote string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "for-each-ref", "--format=%(refname)").CombinedOutput()
	if err != nil {
		t.Fatalf("cannot read the probe remote's refs: %v\n%s", err, out)
	}
	return string(out)
}

// --- F14. THE OBSERVATION ENVIRONMENT IS NOT HERMETIC -----------------------

// F14. A step that puts a real directory back on PATH runs its commands FOR
// REAL, and the gate reports it as publishing nothing.
//
// observe.go's hermeticity rests on one sentence: "PATH is ONE EMPTY DIRECTORY,
// so every command the shell resolves through PATH therefore reaches
// command_not_found_handle". PATH is an ordinary variable of the step's own
// shell. `export PATH=/usr/bin:/bin` is one line, it is a line real workflows
// contain, and after it bash finds the real programs and executes them. The
// DEBUG guard (__rs_guard) does not look: it inspects only command words that
// CONTAIN A SLASH, and a bare `git` contains none.
//
// So the invocation is performed rather than recorded, obs.Invocations is empty,
// classifyObserved is never called, and Acts() returns no act and no error -
// which is the exact sentence six fail-opens were each an instance of, now
// reachable for EVERY program at once rather than one spelling at a time.
func TestRegress0046L6_F14_APathRestoringStepEscapesTheObservationEntirely(t *testing.T) {
	withObserver(t)
	src, remote := localGitPair(t)
	run := "export PATH=/usr/bin:/bin\n" +
		"cd " + src + "\n" +
		"git push " + remote + " HEAD:refs/heads/published\n"

	step := Step{Name: "publish a dev image so testers can pull dispatch builds", Run: run}
	obs, oerr := step.Observed(nil)
	acts, err := step.Acts(nil)
	refs := remoteRefs(t, remote)

	if oerr == nil {
		t.Logf("what the environment recorded: %q (refusals: %q)", step.script(), obs.Refusals)
	}
	t.Logf("acts=%v err=%v", acts, err)
	t.Logf("refs in the probe remote after the observation:\n%s", refs)

	if !strings.Contains(refs, "refs/heads/published") {
		t.Skip("the real git did not run inside the observation; F14 does not reproduce here")
	}
	if err != nil || len(acts) > 0 {
		t.Fatalf("the push ran for real, but the gate did decide something about it (acts=%v err=%v); F14 would then be a hermeticity finding only", acts, err)
	}
	t.Fatalf("the step's `git push` was PERFORMED FOR REAL - refs/heads/published now exists in %s - and the gate recorded no invocation, raised no refusal and reported no act. `export PATH=…` is one line, and after it every command in the step executes outside the observation and is reported as publishing nothing", remote)
}

// F14b. The same escape without touching PATH at all: a command word that is a
// RELATIVE path bash can resolve outside the repository mirror.
//
// __rs_guard refuses an absolute path it has not shimmed, but for a word that
// merely CONTAINS a slash it asks only `[ -x "$__w" ]` and lets anything
// executable through, on the reasoning that "the repository mirror answers these
// with a recording shim". The mirror answers paths INTO the repository; `cd /usr/bin`
// (a builtin, so nothing intercepts it) followed by `./git` is executable, is not
// a shim, and runs.
func TestRegress0046L6_F14b_ARelativePathOutsideTheMirrorRunsForReal(t *testing.T) {
	withObserver(t)
	src, remote := localGitPair(t)
	gitDir, gitBase := filepath.Split(mustLookPath(t, "git"))
	run := "cd " + strings.TrimSuffix(gitDir, "/") + "\n" +
		"./" + gitBase + " -C " + src + " push " + remote + " HEAD:refs/heads/published-relative\n"

	step := Step{Name: "publish a dev image so testers can pull dispatch builds", Run: run}
	acts, err := step.Acts(nil)
	refs := remoteRefs(t, remote)
	t.Logf("acts=%v err=%v", acts, err)
	t.Logf("refs in the probe remote after the observation:\n%s", refs)

	if !strings.Contains(refs, "refs/heads/published-relative") {
		t.Skip("the real git did not run inside the observation; F14b does not reproduce here")
	}
	t.Fatalf("a `./git … push` reached the real git binary and pushed a ref for real; the gate reported acts=%v err=%v", acts, err)
}

func mustLookPath(t *testing.T, prog string) string {
	t.Helper()
	p, err := exec.LookPath(prog)
	if err != nil {
		t.Skipf("%s is not on PATH in this container, so this probe cannot measure anything", prog)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// F14g. The finding as A6 states it, end to end through the committed gate: a
// release definition whose manual dispatch runs a step that restores PATH and
// pushes. The gate must fail naming the step; it passes.
func TestRegress0046L6_F14g_TheGatePassesADispatchThatPublishesAfterRestoringPATH(t *testing.T) {
	src, remote := localGitPair(t)
	root := withDevRunStep(t,
		"export PATH=/usr/bin:/bin",
		"cd "+src,
		"git push "+remote+" HEAD:refs/heads/dispatch-published")
	red, out := runGateCapturingStderr(t, root)
	t.Logf("gate output:\n%s", out)
	refs := remoteRefs(t, remote)
	t.Logf("refs in the probe remote after the gate ran:\n%s", refs)
	if !strings.Contains(refs, "refs/heads/dispatch-published") {
		t.Skip("the real git did not run inside the gate; F14g does not reproduce here")
	}
	if !red {
		t.Fatalf("the gate PASSED a release definition whose manual dispatch performs a real `git push` - and performed that push itself while grading it. A dry run must publish nothing")
	}
	t.Fatalf("the gate red, but it performed the step's real `git push` while deciding that; grading 71 mutated workflows in an environment that executes their commands is the hazard the ruling's hermeticity condition names")
}

// --- P0-P2. The measurement around them -------------------------------------

// P0. The committed tree still passes, so the assertions above are not being met
// by a gate that reds on everything.
func TestRegress0046L6_P0_BaselineStillPasses(t *testing.T) {
	mustPass(t, fixture(t), "the committed inputs")
}

// P1. The control for F14: the SAME `git push`, with the PATH line removed. The
// gate catches it, so F14 is the PATH restoration and not the harness.
func TestRegress0046L6_P1_TheSamePushWithoutThePathLineIsCaught(t *testing.T) {
	src, remote := localGitPair(t)
	root := withDevRunStep(t,
		"cd "+src,
		"git push "+remote+" HEAD:refs/heads/dispatch-published")
	mustRedNaming(t, root, "a `git push` on a manual dispatch",
		"publish a dev image so testers can pull dispatch builds")
	if strings.Contains(remoteRefs(t, remote), "refs/heads/dispatch-published") {
		t.Error("the control's push ALSO ran for real, so the environment is executing pushes even when it records them")
	}
}

// P2. The honest other direction: a step that legitimately extends PATH with a
// directory that does not exist must keep working, so a fix here cannot simply
// red every step that mentions PATH.
func TestRegress0046L6_P2_AnOrdinaryPathExtensionIsNotAnAct(t *testing.T) {
	withObserver(t)
	acts, err := (Step{Name: "probe", Run: "export PATH=\"$PATH:$HOME/go/bin\"\nmake check\n"}).Acts(nil)
	if err != nil {
		t.Fatalf("an ordinary PATH extension was refused: %v", err)
	}
	if len(acts) != 0 {
		t.Fatalf("an ordinary PATH extension reported acts: %v", acts)
	}
}

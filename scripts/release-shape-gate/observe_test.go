package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE OBSERVATION ENVIRONMENT, GRADED AGAINST THE SHELL ITSELF.
//
// A6, A7 and A12 are decided by what a step was OBSERVED to invoke. Everything below either
// grades that observation against what bash really passes, or pins one of the ways a reader
// of the same text was beaten - because six fail-opens in a row all said the same thing, that
// a spelling nobody had taught the reader contributed silence and silence read as "no".

func withObserver(t *testing.T) {
	t.Helper()
	o, err := NewObserver("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.install())
}

// argvUnderRealBash runs a script under `bash -e` - the shell GitHub gives a Linux `run:`
// step - with recording stubs first on PATH, and returns the argv each stub was handed. It is
// the ground truth every claim about the observer is measured against, and it is the same
// harness the impl-gate refuters built to establish F10s and F11s.
func argvUnderRealBash(t *testing.T, script string, programs ...string) []string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n" +
		"printf '%s' \"$(basename \"$0\")\" >> \"$ARGV_LOG\"\n" +
		"for a in \"$@\"; do printf ' <%s>' \"$a\" >> \"$ARGV_LOG\"; done\n" +
		"printf '\\n' >> \"$ARGV_LOG\"\n" +
		"exit 0\n"
	for _, p := range programs {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "argv.log")
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGV_LOG="+log)
	_, _ = cmd.CombinedOutput() // a non-zero exit is ordinary; the log is the measurement
	raw, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func observedArgv(t *testing.T, run string) []string {
	t.Helper()
	obs, err := (Step{Name: "probe", Run: run}).Observed(nil)
	if err != nil {
		t.Fatalf("observing %q: %v", run, err)
	}
	var out []string
	for _, inv := range obs.Invocations {
		line := inv.Argv[0]
		for _, a := range inv.Argv[1:] {
			line += " <" + a + ">"
		}
		out = append(out, line)
	}
	return out
}

// THE PROPERTY THE WHOLE GRADE ROUTE RESTS ON: what the environment records is what bash
// passes. Every spelling below defeated a reader at some point, and none of them is a
// spelling this environment knows about - it does not read the script, it runs it.
func TestObserve_RecordsTheArgvBashReallyPasses(t *testing.T) {
	withObserver(t)
	cases := []struct {
		name string
		run  string
	}{
		{"a plain push", `docker push ghcr.io/o/r:dev`},
		{"F11: a push inside a quoted word handed to a nested shell", `sh -c "docker push ghcr.io/o/r:dev"`},
		{"F11b: a push inside an evaled string", `eval "docker push ghcr.io/o/r:dev"`},
		{"F12: buildx's attached shorthand", `docker buildx build -otype=registry,name=ghcr.io/o/r:dev .`},
		{"F7: a command spread over continuations", "docker buildx build \\\n  --push \\\n  -t ghcr.io/o/r:dev \\\n  ."},
		{"a push from inside a subshell", `( docker push ghcr.io/o/r:dev )`},
		{"a push from inside a loop", `for t in dev latest; do docker push "ghcr.io/o/r:$t"; done`},
		{"a push from inside a command substitution", `out="$(docker push ghcr.io/o/r:dev)"`},
		{"a push whose reference comes from a variable", "REF=ghcr.io/o/r:dev\ndocker push \"$REF\""},
		{"a push assembled from two variables", "A=ghcr.io/o/r\nB=dev\ndocker push \"$A:$B\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := argvUnderRealBash(t, tc.run, "docker", "sh")
			got := observedArgv(t, tc.run)
			// The nested shell is a program bash really invokes, and the observation
			// follows it rather than stopping there, so it sees one more line than a
			// recording stub for `sh` does. Every line the SHELL performed must be there.
			for _, w := range want {
				if !contains(got, w) {
					t.Fatalf("bash invoked %q and the observation did not record it.\nbash: %q\nobserved: %q", w, want, got)
				}
			}
		})
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// F11 and F12 as ACT decisions, which is the form A6 asks the question in.
func TestObserve_DecidesTheActFromTheObservedArgv(t *testing.T) {
	withObserver(t)
	cases := []struct {
		name      string
		run       string
		publishes bool
	}{
		{"F11: sh -c around a push", `sh -c "docker push ghcr.io/o/r:dev"`, true},
		{"F11b: eval around a push", `eval "docker push ghcr.io/o/r:dev"`, true},
		{"F12: -otype=registry publishes", `docker buildx build -otype=registry,name=ghcr.io/o/r:dev .`, true},
		{"F12's control: -otype=docker,dest= is local", `docker buildx build -otype=docker,dest=/tmp/img.tar .`, false},
		{"a bare --load stays local", `docker buildx build --load -t r:dev .`, false},
		{"a quoted mention of a push is not a push", `echo "remember to docker push ghcr.io/o/r:dev"`, false},
		{"a push only in a comment is prose", "docker buildx build --load .\n# docker push ghcr.io/o/r:dev\n", false},
		{"a push on the far side of a false branch never runs", `if false; then docker push ghcr.io/o/r:dev; fi`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acts, err := (Step{Name: "probe", Run: tc.run}).Acts(nil)
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if got := len(acts) > 0; got != tc.publishes {
				t.Fatalf("Acts found %d act(s) (%+v), want publishes=%v", len(acts), acts, tc.publishes)
			}
		})
	}
}

// THE EXPLORATION. A publish behind a command's FAILURE is on a path the all-succeed run
// never takes, so the environment re-runs the step making each of its commands fail in turn.
// Without that, this step reads as publishing nothing - and it publishes.
func TestObserve_FindsAPublishBehindAFailedCommand(t *testing.T) {
	withObserver(t)
	for _, run := range []string{
		"if ! docker manifest inspect ghcr.io/o/r:dev; then docker push ghcr.io/o/r:dev; fi",
		"docker manifest inspect ghcr.io/o/r:dev || docker push ghcr.io/o/r:dev",
		"if docker manifest inspect ghcr.io/o/r:dev; then :; else docker push ghcr.io/o/r:dev; fi",
	} {
		t.Run(run, func(t *testing.T) {
			acts, err := (Step{Name: "probe", Run: run}).Acts(nil)
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if len(acts) == 0 {
				t.Fatalf("the step publishes on the path where the inspect fails, and the observation found no act. Exploring only the all-succeed path is how a publish behind `if ! …` reads as nothing")
			}
		})
	}
}

// DENY BY DEFAULT. The environment records every invocation; one it cannot classify has to
// FAIL naming the step and the program. This is the property that makes the next unseen
// spelling a loud stop instead of a fail-open, and it is asserted in both directions.
func TestObserve_AnUnclassifiedProgramIsAnErrorNotSilence(t *testing.T) {
	withObserver(t)
	for _, run := range []string{
		"publish-everything --to ghcr.io",
		"rclone copy dist remote:bucket",
		"scp dist/holdfast.tar.gz someone@example.com:/srv",
	} {
		t.Run(run, func(t *testing.T) {
			_, err := (Step{Name: "probe", Run: run}).Acts(nil)
			if err == nil {
				t.Fatalf("a program this gate has never been taught was observed to run and reported no act at all; silence reading as a no is what lost six times")
			}
			if !strings.Contains(err.Error(), strings.Fields(run)[0]) {
				t.Errorf("the refusal does not name the program it saw:\n%v", err)
			}
		})
	}
	// Stating the boundary must not become a refusal of everything ordinary.
	for _, run := range []string{"make check", "go build ./...", "tar -czf x.tar.gz dist", "docker pull ghcr.io/o/r:dev"} {
		if _, err := (Step{Name: "probe", Run: run}).Acts(nil); err != nil {
			t.Errorf("%q was refused, and an edge that refuses the ordinary tooling a release runs is not an edge: %v", run, err)
		}
	}
}

// A COMMAND THIS ENVIRONMENT CANNOT WATCH IS REFUSED. An absolute path is resolved by bash
// itself, so a program at one this gate has not shimmed would run for real and be recorded
// nowhere. It reds by name instead.
func TestObserve_AnUnwatchableInvocationIsRefused(t *testing.T) {
	withObserver(t)
	for _, run := range []string{
		"/opt/vendor/bin/docker push ghcr.io/o/r:dev",
		"/nonexistent/tool --publish",
		"./scripts/there-is-no-such-script.sh",
	} {
		t.Run(run, func(t *testing.T) {
			_, err := (Step{Name: "probe", Run: run}).Acts(nil)
			if err == nil {
				t.Fatalf("an invocation this environment could not watch was reported as publishing nothing")
			}
			if !strings.Contains(err.Error(), "NOT OBSERVE") {
				t.Errorf("the refusal does not say it could not observe:\n%v", err)
			}
		})
	}
	// And the paths it CAN watch stay watched: a program on PATH is recorded by the
	// recorder, one at an absolute path this gate shims is recorded by its shim.
	acts, err := (Step{Name: "probe", Run: "/usr/bin/env docker push ghcr.io/o/r:dev"}).Acts(nil)
	if err != nil {
		t.Fatalf("an absolute path this gate shims should be observed, not refused: %v", err)
	}
	if len(acts) == 0 {
		t.Fatal("`/usr/bin/env docker push` publishes and was observed to perform no act")
	}
}

// THE ONE-FILE-AWAY NESTING. A step that moves the command into a repository script must not
// thereby become invisible, so the environment descends into a shell script it ships.
func TestObserve_DescendsIntoARepositoryScript(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/usr/bin/env bash\nset -euo pipefail\ndocker push \"$1\"\n"
	if err := os.WriteFile(filepath.Join(root, "scripts", "ship.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	o, err := NewObserver(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.install())

	acts, err := (Step{Name: "probe", Run: "./scripts/ship.sh ghcr.io/o/r:dev"}).Acts(nil)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if len(acts) == 0 {
		t.Fatal("a repository script that pushes was invoked and the step was reported as publishing nothing. A gate a command can hide from by moving one file away is not a gate")
	}
}

// THE ROLES A7 IS STATED IN. F13: once the reader resolved quoting, a step that PRINTS the
// gate's name satisfied the full-gate role and the promotion read as gated. A role is now
// what the step was observed to invoke.
func TestObserve_RolesAreWhatTheStepInvoked(t *testing.T) {
	withObserver(t)
	cases := []struct {
		run  string
		gate bool
	}{
		{"make check", true},
		{"make -C . check", true},
		{"make \\\n  check", true},
		{`echo "make check"`, false},
		{`printf '%s\n' "make check"`, false},
		{`echo "the full gate is make check; run it yourself"`, false},
		{"make build", false},
		{`gh release create v0.0.0 --notes "run make check before tagging"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.run, func(t *testing.T) {
			if got := (Step{Run: tc.run}).RunsFullGate(); got != tc.gate {
				t.Fatalf("RunsFullGate() = %v, want %v (observed: %q)", got, tc.gate, (Step{Run: tc.run}).script())
			}
		})
	}
	if (Step{Run: `echo "smoke-image.sh is what you want"`}).RunsSmoke() {
		t.Fatal("a step that merely names the smoke script reads as a smoke run")
	}
}

// A SCRIPT BASH WOULD REFUSE cannot be observed - it never starts - so the lexical reader
// takes over for that one case. It reads MORE than any shell would run, so it can add an act
// and hide none, and a gate that reported "publishes nothing" over an unparseable script
// would be the vacuous pass in a new place.
func TestObserve_AnUnparseableScriptFallsBackToTheReader(t *testing.T) {
	withObserver(t)
	// An escaped literal backslash ends the line, so the push below is its own command; the
	// single quote before it is never closed, so bash refuses the whole script.
	run := `printf '%s' 'a\\` + "\n" + `docker push ghcr.io/o/r:dev` + "\n"
	obs, err := (Step{Name: "probe", Run: run}).Observed(nil)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Unparseable == "" {
		t.Fatalf("bash accepts this script, so the case measures nothing: %q", run)
	}
	acts, err := (Step{Name: "probe", Run: run}).Acts(nil)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if len(acts) == 0 {
		t.Fatal("an unparseable script carrying a push reported no act")
	}
}

// THE FLAG READER that closes F12, in the form the finding turned on: the two spellings of
// ONE flag must be one answer. buildx's `-o` is a pflag shorthand and pflag takes an ATTACHED
// value, measured against a real buildx by the impl-gate refuter.
func TestTokeniseBuildFlags_ReadsAttachedShorthandValues(t *testing.T) {
	spec := "type=registry,name=ghcr.io/o/r:dev"
	for _, spelling := range []string{
		"--output=" + spec, "--output " + spec, "-o " + spec, "-o=" + spec, "-o" + spec,
	} {
		run := "docker buildx build --platform linux/amd64 " + spelling + " ."
		kind, why, err := Command{Words: strings.Fields(run)}.Act()
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", spelling, err)
			continue
		}
		if kind != ActImagePush {
			t.Errorf("%s: Act() = %q (%s), want a publish - every one of these is the same destination", spelling, kind, why)
		}
	}
	// And the same shorthands naming a LOCAL exporter must stay local, or the fix is a
	// refusal of every `-o` rather than a reading of it.
	for _, spelling := range []string{
		"--output=type=docker,dest=/tmp/img.tar", "-otype=docker,dest=/tmp/img.tar",
		"-o type=docker,dest=/tmp/img.tar", "--load", "-t r:dev",
	} {
		run := "docker buildx build " + spelling + " ."
		kind, why, err := Command{Words: strings.Fields(run)}.Act()
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", spelling, err)
			continue
		}
		if kind != "" {
			t.Errorf("%s: Act() = %q (%s), want no act", spelling, kind, why)
		}
	}
	// A short cluster this gate cannot tokenise is an ERROR, not a word it skips: skipping
	// what it did not recognise is exactly how `-otype=registry` became a local build.
	if _, _, err := (Command{Words: strings.Fields("docker buildx build -Zq type=registry .")}).Act(); err == nil {
		t.Error("an untokenisable short flag was skipped rather than refused")
	}
}

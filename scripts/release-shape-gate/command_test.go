package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE READER, GRADED AGAINST THE SHELL ITSELF.
//
// Every assertion this gate makes about a `run:` step rests on one claim: that the commands
// it read out of that block are the commands the runner would execute. Every way that claim
// has been wrong has been wrong in the same direction - the reader saw FEWER words than bash
// passed, so a `--push` fell off the end of what it looked at and a dry run that publishes
// was reported as publishing nothing. Asserting the reader against a fixture only ever pins
// what its author already believed; asserting it against bash pins what is true.
//
// So each script below is run by `bash -e` - the shell GitHub gives a Linux `run:` step, and
// the one shell.go itself execs - with a recording stub named `docker` first on PATH, and the
// argv the stub was handed is compared word for word with what the reader produced. Nothing
// publishes: the stub performs nothing and every script lives in t.TempDir().
//
// The fixtures carry no variables and no globs on purpose. The reader deliberately does not
// expand either (it decides an act from the DEFINITION, before any run), so those are the one
// place it and the shell are meant to differ, and including them here would grade the wrong
// thing.
func TestShellCommands_ReadTheSameArgvBashPasses(t *testing.T) {
	cases := []struct {
		name   string
		script []string
	}{
		{
			// The shape that defeated the previous reader. A `#` inside a quoted argument on
			// a continuation line was read as a comment, so the rest of the logical line was
			// deleted - the `--push` and the backslash that would have joined it included.
			// bash performs the push.
			name: "a quoted # across a continuation",
			script: []string{
				`docker buildx build \`,
				`  --annotation "org.opencontainers.image.description=dev build, \`,
				`  see #123" \`,
				`  --push \`,
				`  --platform linux/amd64 \`,
				`  -t ghcr.io/nschatz/holdfast:dev \`,
				`  .`,
			},
		},
		{
			name: "the same argument on a build that stays local",
			script: []string{
				`docker buildx build \`,
				`  --annotation "org.opencontainers.image.description=dev build, \`,
				`  see #123" \`,
				`  --load \`,
				`  -t holdfast:dev \`,
				`  .`,
			},
		},
		{
			name:   "the destination spelled as an exporter rather than a flag",
			script: []string{`docker buildx build --platform linux/amd64 --output=type=registry,name=ghcr.io/nschatz/holdfast:dev .`},
		},
		{
			// The shell puts nothing in a continuation's place, so this is `docker push`.
			name:   "a continuation inside the command word",
			script: []string{`docker \`, ` push ghcr.io/nschatz/holdfast:dev`},
		},
		{
			name:   "a trailing comment is not an argument",
			script: []string{`docker image push ghcr.io/nschatz/holdfast:dev   # publish it`},
		},
		{
			name:   "a # that begins no word is an ordinary character",
			script: []string{`docker buildx build --load -t holdfast:dev#1 .`},
		},
		{
			name:   "single quotes take everything literally",
			script: []string{`docker buildx build --load -t 'holdfast:dev # one' .`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := dockerArgvUnderBash(t, tc.script)
			var got []string
			for _, c := range ShellCommands(strings.Join(tc.script, "\n") + "\n") {
				if len(c.Words) > 0 && filepath.Base(c.Words[0]) == "docker" {
					got = c.Words
				}
			}
			if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
				t.Fatalf("the reader and bash disagree about what this step runs.\nbash passed:  %q\nthe reader read: %q\nA reader that sees fewer words than the shell passes is how a --push goes unseen.", want, got)
			}
		})
	}
}

// dockerArgvUnderBash runs a script under `bash -e` with a stub `docker` first on PATH that
// records its argv and performs nothing, and returns the words docker was actually handed.
func dockerArgvUnderBash(t *testing.T, script []string) []string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "argv.log")
	stub := "#!/bin/sh\n" +
		"printf 'docker' >> \"$ARGV_LOG\"\n" +
		"for a in \"$@\"; do printf '\\037%s' \"$a\" >> \"$ARGV_LOG\"; done\n" +
		"printf '\\n' >> \"$ARGV_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(strings.Join(script, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-e", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGV_LOG="+log)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the fixture does not run under bash, so it grades nothing: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("cannot read the stub log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("the fixture invoked docker %d time(s); these cases each invoke it exactly once:\n%s", len(lines), raw)
	}
	return strings.Split(lines[0], "\x1f")
}

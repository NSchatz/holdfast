package main

// Running a workflow step's shell for real.
//
// This is the mechanism behind "decide from the values that logic actually produces". The
// planning step's script is written to a file and executed by BASH - the same interpreter
// the runner uses - with the GitHub environment variables it reads, and the key/value
// pairs it appends to $GITHUB_OUTPUT are read back. Nothing here re-implements the
// script's logic, so the gate cannot drift from it: change the branch that sets `publish`
// and this gate sees the new value on the next run.
//
// EXACTLY ONE STEP IS EVER EXECUTED: the one declaring `id: plan` (main.go refuses a
// definition in which anything else writes to $GITHUB_OUTPUT). That bound is the lesson of
// this spec's sixth impl-gate ordinal, which defeated a design that ran EVERY step's script
// in a stubbed environment: the environment's controls all lived inside the shell they were
// watching, so one `export PATH=/usr/bin:/bin` put a real `git push` beyond them. The gate
// no longer needs to know what a step's shell does - see capability.go - so it no longer
// runs one.
//
// What the planning step gets is still hardened, because it is repository code and `make
// check` runs it:
//
//   - PATH is the stub directory ALONE. It carries recording stubs for every tool that
//     could reach a registry, a remote or a package index (they perform nothing), plus
//     shims for a declared set of pure utilities an ordinary planning script needs. A
//     program outside both lists is "command not found", which fails the step LOUDLY.
//   - HOME, the working directory and every GitHub path point into a throwaway directory.
//   - A 90-second timeout.
//
// The residue, stated rather than left to be found: a planning script that sets PATH back
// itself can still run a program. That is a property of executing repository code at all,
// which `make check` does when it runs the test suite; what it is NOT is a way to make this
// gate report the wrong answer, because nothing about the grade depends on what the script
// invokes - only on what it appends to $GITHUB_OUTPUT.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// stubbedTools are neutralised on PATH while the planning step runs: each records its argv
// and performs nothing. The planning step invoking one of these is itself an error (main.go)
// - a step that reaches a registry is not planning - and the recording is how that is
// noticed rather than performed.
var stubbedTools = []string{
	"docker", "podman", "nerdctl", "buildah", "skopeo",
	"crane", "regctl", "oras", "helm",
	"gh", "git", "hub", "glab",
	"npm", "pnpm", "yarn", "cargo", "twine", "poetry", "gem", "mvn", "gradle",
	"aws", "gcloud", "az", "doctl", "flyctl",
	"curl", "wget", "rclone", "rsync", "scp", "sftp", "ssh", "nc",
}

// utilitiesCheckedAndPure are the ordinary programs a planning script computes with. Each
// one was read and found unable to publish anything: they transform text, report the date,
// or test a condition. The list is deliberately short - a program outside it does not run,
// which fails the step by name rather than letting it reach the host.
var utilitiesCheckedAndPure = []string{
	"date", "tr", "grep", "egrep", "fgrep", "sed", "awk", "gawk", "mawk",
	"cut", "cat", "head", "tail", "sort", "uniq", "wc", "tr",
	"basename", "dirname", "expr", "printf", "echo", "test", "[",
	"true", "false", "env", "uname", "seq", "id", "sleep",
}

const argvSep = "\x1f"

// Runner owns the stub directory shared by every step execution.
type Runner struct {
	stubDir string
	root    string
	home    string
}

func NewRunner() (*Runner, error) {
	root, err := os.MkdirTemp("", "release-shape-")
	if err != nil {
		return nil, fmt.Errorf("cannot create a scratch directory: %w", err)
	}
	stub := filepath.Join(root, "stub-bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{stub, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cannot create %s: %w", d, err)
		}
	}
	for _, name := range stubbedTools {
		body := fmt.Sprintf(`#!/bin/sh
# Stubbed by the release-shape gate: records its argv and performs nothing.
{ printf '%%s' %q; for a in "$@"; do printf '%s%%s' "$a"; done; printf '\n'; } >> "$RELEASE_SHAPE_ARGV_LOG"
exit 0
`, name, argvSep)
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil {
			return nil, fmt.Errorf("cannot write the %s stub: %w", name, err)
		}
	}
	// The pure utilities are symlinked from the host so the planning script computes with
	// the real ones. A missing utility is simply absent: a script that needs it fails by
	// name, which is the loud direction.
	for _, name := range utilitiesCheckedAndPure {
		real, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		link := filepath.Join(stub, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(real, link); err != nil {
			return nil, fmt.Errorf("cannot link the %s utility: %w", name, err)
		}
	}
	return &Runner{stubDir: stub, root: root, home: home}, nil
}

func (r *Runner) Close() {
	if r.root != "" {
		_ = os.RemoveAll(r.root)
	}
}

// StepRun is what one execution produced.
type StepRun struct {
	ExitCode int
	Output   string            // stdout and stderr, interleaved as the runner would show them
	Outputs  map[string]string // what the step appended to $GITHUB_OUTPUT
	Argv     [][]string        // every stubbed publishing command it invoked, in order
}

// Run executes one shell script with the given environment.
func (r *Runner) Run(script string, env map[string]string) (*StepRun, error) {
	dir, err := os.MkdirTemp(r.root, "step-")
	if err != nil {
		return nil, fmt.Errorf("cannot create a step directory: %w", err)
	}
	var (
		scriptPath = filepath.Join(dir, "step.sh")
		outPath    = filepath.Join(dir, "github_output")
		argvPath   = filepath.Join(dir, "argv.log")
	)
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return nil, fmt.Errorf("cannot write the step script: %w", err)
	}
	for _, p := range []string{outPath, argvPath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			return nil, fmt.Errorf("cannot create %s: %w", p, err)
		}
	}

	full := map[string]string{
		// The stub directory ALONE. Nothing of the host's PATH is appended: a program
		// that is neither stubbed nor a declared pure utility is not found, and the step
		// fails saying so.
		"PATH":                   r.stubDir,
		"HOME":                   r.home,
		"LANG":                   "C",
		"LC_ALL":                 "C",
		"TZ":                     "UTC",
		"CI":                     "true",
		"GITHUB_ACTIONS":         "true",
		"RUNNER_OS":              "Linux",
		"RUNNER_TEMP":            dir,
		"GITHUB_WORKSPACE":       dir,
		"GITHUB_OUTPUT":          outPath,
		"GITHUB_ENV":             filepath.Join(dir, "github_env"),
		"GITHUB_PATH":            filepath.Join(dir, "github_path"),
		"GITHUB_STEP_SUMMARY":    filepath.Join(dir, "github_step_summary"),
		"RELEASE_SHAPE_ARGV_LOG": argvPath,
	}
	for k, v := range env {
		full[k] = v
	}
	envv := make([]string, 0, len(full))
	for k, v := range full {
		envv = append(envv, k+"="+v)
	}

	// A step that hangs must red the gate rather than hang `make check` behind it.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// `bash -e {0}` is the default shell GitHub gives a `run:` step on Linux.
	cmd := exec.CommandContext(ctx, "bash", "-e", scriptPath)
	cmd.Dir = dir
	cmd.Env = envv
	out, err := cmd.CombinedOutput()
	run := &StepRun{Output: string(out)}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, fmt.Errorf("could not execute the step's shell: %w", err)
		}
		run.ExitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("the step's shell did not finish within 90s; the gate refuses to guess what it would have decided")
	}

	run.Outputs, err = parseStepOutput(outPath)
	if err != nil {
		return nil, err
	}
	run.Argv, err = parseArgvLog(argvPath)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// parseStepOutput reads a $GITHUB_OUTPUT file: `key=value` lines plus the heredoc form
// (`key<<DELIM` … `DELIM`) that a multi-line value has to use.
func parseStepOutput(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the step's $GITHUB_OUTPUT: %w", err)
	}
	out := map[string]string{}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if k, delim, ok := strings.Cut(line, "<<"); ok && !strings.Contains(k, "=") {
			var body []string
			i++
			for ; i < len(lines) && lines[i] != delim; i++ {
				body = append(body, lines[i])
			}
			out[strings.TrimSpace(k)] = strings.Join(body, "\n")
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = v
		}
	}
	return out, nil
}

func parseArgvLog(path string) ([][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read the stub command log: %w", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Split(line, argvSep))
	}
	return out, nil
}

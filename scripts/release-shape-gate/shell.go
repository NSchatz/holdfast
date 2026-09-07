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
// The step runs in a scratch directory with the publishing binaries REPLACED by stubs that
// record their argv and exit 0. That is what makes it safe to execute a step whose whole
// purpose is to publish: `docker buildx imagetools create -t …` performs nothing and yet
// tells the gate the exact reference it would have moved - a reference the workflow
// computes at runtime out of the repository name, which no amount of reading the YAML
// would produce.

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

// stubbed are the commands replaced on PATH while a step's shell runs. A step is only ever
// executed here for its DECISIONS, so anything that could reach a registry, a remote or a
// package index is neutralised. An unlisted command runs for real, which is why only the
// planning steps and the tag-move step are ever executed (see main.go).
var stubbed = []string{
	"docker", "podman", "buildx", "gh", "git", "curl", "wget",
	"npm", "pnpm", "cargo", "helm", "skopeo", "oras", "crane", "regctl", "aws",
}

const argvSep = "\x1f"

// Runner owns the stub directory shared by every step execution.
type Runner struct {
	stubDir string
	root    string
}

func NewRunner() (*Runner, error) {
	root, err := os.MkdirTemp("", "release-shape-")
	if err != nil {
		return nil, fmt.Errorf("cannot create a scratch directory: %w", err)
	}
	stub := filepath.Join(root, "stub-bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the stub bin directory: %w", err)
	}
	for _, name := range stubbed {
		body := fmt.Sprintf(`#!/bin/sh
# Stubbed by the release-shape gate: records its argv and performs nothing.
{ printf '%%s' %q; for a in "$@"; do printf '%s%%s' "$a"; done; printf '\n'; } >> "$RELEASE_SHAPE_ARGV_LOG"
exit 0
`, name, argvSep)
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil {
			return nil, fmt.Errorf("cannot write the %s stub: %w", name, err)
		}
	}
	return &Runner{stubDir: stub, root: root}, nil
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
		"PATH":                   r.stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME":                   os.Getenv("HOME"),
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

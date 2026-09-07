package main

// OBSERVING WHAT A `run:` STEP INVOKES, RATHER THAN READING WHAT IT SAYS.
//
// This file is the grade route for A6, A7 and A12, and it exists because reading the text
// lost six times running. Every one of those losses was the same sentence: a spelling the
// reader had not been taught contributed SILENCE, and silence read as "this step publishes
// nothing".
//
//	F1, F5   a publishing INPUT the act decision could not see
//	F7       the detectors could not cross a shell line continuation
//	F9       the command catalogue knew spellings and no destination
//	F11      a publishing command inside a quoted word (`sh -c "…"`, `eval "…"`)
//	F12      buildx's attached shorthand `-otype=registry` decided "local"
//	F13      quote removal made `echo "make check"` satisfy the full-gate role
//
// Four different mechanisms, one direction. The reader got cleverer each time and ordinary
// shell beat it each time, so the conductor ruled that deciding what arbitrary shell text
// does is the wrong question: the steps a dispatch RUNS are still decided by executing the
// workflow's own planning logic (shell.go, expr.go - that half every ordinal has found
// sound), and what each of those steps INVOKES is now OBSERVED.
//
// The environment, and why each piece of it is there:
//
//   - PATH is ONE EMPTY DIRECTORY. Every command the shell resolves through PATH therefore
//     reaches `command_not_found_handle`, which records the full argv BASH BUILT - after
//     quote removal, after expansion, after `eval`, from inside a pipeline, a subshell, a
//     loop or a command substitution - and performs nothing. There is no lexer here to
//     defeat: the words are the words the runner would have handed the program.
//   - The repository is MIRRORED as directories plus a recording shim per executable file,
//     and that mirror is the working directory. `./scripts/smoke-image.sh` is therefore
//     recorded with its arguments and never runs, and a shell script in the repository is
//     descended into, so a step cannot reach a registry by putting the command one file away.
//   - Every executable of a tool this gate classifies is also shimmed at its ABSOLUTE path,
//     so `/usr/bin/docker push` - and a variable that expands to it - is recorded rather
//     than executed. A command word that begins with `/` and has no shim is REFUSED by a
//     DEBUG trap before it runs, and reds the gate by name: the environment says out loud
//     that it could not observe something instead of letting it through.
//   - DENY BY DEFAULT. Every recorded invocation has to be classified (command.go). One that
//     is not fails, naming the step and the program. Silence is not reachable.
//
// Two deliberate departures from the runner, both in the direction that cannot hide an act:
//
//   - `set -e` and `set -u` are stripped from the observed script, because the commands that
//     would have created a file or a directory were recorded rather than run, so a later
//     `cd dist` fails for a reason the real runner would never have. Stopping there would
//     leave everything after it unobserved. Running past a failure observes MORE than the
//     runner would reach, never less.
//   - The exit status of every recorded invocation is a value this harness chooses, and it
//     re-runs the step varying those choices until the set of invocations stops growing.
//     `if ! docker manifest inspect X; then docker push X; fi` publishes on exactly one of
//     those paths, and one run down the all-succeed path would never see it.
//
// What it does NOT reach, stated rather than left to be found: a command word that expands
// from a variable to an absolute path this gate has not shimmed. Everything else - every
// quoting, every nesting, every spelling - arrives here as argv.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// The separators the recorder writes with. Both are control characters no argv this
	// gate cares about contains, and both are stripped out of any word that does.
	obsArgSep = "\x1f"
	obsSetSep = "\x1e"

	// Markers the environment records about ITSELF rather than about a command. Each one
	// is a refusal: the gate reds naming it rather than reporting a step it could not see.
	obsUnobservable = "__release_shape_unobservable"
	obsNestedShell  = "__release_shape_nested_shell"
	obsComplete     = "__release_shape_complete"

	// How many times one step may be re-run while the exploration is still finding new
	// invocations. A step that has not converged by then is UNDECIDED, not clean.
	obsMaxRuns = 400

	// How many of a step's commands may be made to fail at once. One is enough to reach the
	// far side of every branch a single command's success guards, which is the shape a
	// release path uses; a conjunction of two would need two, and a step whose exploration
	// has not settled says so rather than passing.
	obsMaxFlips = 1

	// How far a repository script may be descended into. A step calling a script that calls
	// a script is observed; deeper is recorded and named.
	obsMaxDepth = 3

	obsTimeout = 60 * time.Second
)

// Invocation is one command the observed shell actually invoked, with the argv it built.
type Invocation struct {
	Argv []string
}

func (i Invocation) String() string { return strings.Join(i.Argv, " ") }

// Observation is what the environment saw across every path it explored.
type Observation struct {
	Invocations []Invocation // union over every explored path, in first-seen order
	Runs        int
	Converged   bool     // the exploration ran out of new paths within the budget
	Refusals    []string // things the environment could not observe (each reds the gate)
	Output      string   // the first run's combined output, for a message
	Unparseable string   // bash would refuse this script; the invocations are the lexer's
}

// Observer owns the scratch tree every observation shares: the empty PATH directory, the
// repository mirror, and the prelude that installs the recorder.
type Observer struct {
	root    string // the repository being gated
	dir     string // scratch root
	empty   string // the one directory on PATH
	mirror  string // the working directory: a recording mirror of the repository
	prelude string // path to the prelude every observed shell sources
	bash    string // the absolute bash the shims run under; PATH inside is empty by design
	cache   map[string]*Observation
}

// defaultObserver lets Step.Acts observe without every caller threading one through. A gate
// run installs one rooted at the repository under test and puts back whatever was there when
// it finishes, so a process that gates several trees in turn never observes one of them
// through another's mirror. A caller that never sets it gets one rooted at the working
// directory.
var defaultObserver *Observer

// install makes o the observer every Step observes through, and returns the undo. The undo
// closes o, so a scratch tree never outlives the run that made it.
func (o *Observer) install() func() {
	prev := defaultObserver
	defaultObserver = o
	return func() {
		defaultObserver = prev
		o.Close()
	}
}

func observerFor(root string) (*Observer, error) {
	if defaultObserver != nil {
		return defaultObserver, nil
	}
	o, err := NewObserver(root)
	if err != nil {
		return nil, err
	}
	defaultObserver = o
	return o, nil
}

func NewObserver(root string) (*Observer, error) {
	dir, err := os.MkdirTemp("", "release-shape-observe-")
	if err != nil {
		return nil, fmt.Errorf("cannot create the observation scratch directory: %w", err)
	}
	// The shims run under an absolute bash, because PATH inside the observation is a single
	// empty directory on purpose and `#!/usr/bin/env bash` would find nothing there.
	sh, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("cannot find bash, which this gate observes every step with: %w", err)
	}
	if sh, err = filepath.Abs(sh); err != nil {
		return nil, err
	}
	o := &Observer{
		root:   root,
		dir:    dir,
		empty:  filepath.Join(dir, "empty-path"),
		mirror: filepath.Join(dir, "mirror"),
		bash:   sh,
		cache:  map[string]*Observation{},
	}
	if err := os.MkdirAll(o.empty, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.mirror, 0o755); err != nil {
		return nil, err
	}
	if err := o.buildMirror(); err != nil {
		return nil, err
	}
	o.prelude = filepath.Join(dir, "prelude.bash")
	if err := os.WriteFile(o.prelude, []byte(o.preludeText()), 0o644); err != nil {
		return nil, fmt.Errorf("cannot write the observation prelude: %w", err)
	}
	return o, nil
}

func (o *Observer) Close() {
	if o != nil && o.dir != "" {
		_ = os.RemoveAll(o.dir)
	}
}

// --- the mirror -------------------------------------------------------------------------

// buildMirror recreates the repository's directory tree with a RECORDING SHIM in place of
// every executable file, and makes that tree the working directory of every observation.
//
// It is what closes the one-file-away nesting. `./scripts/smoke-image.sh` is a program word
// with a slash in it, so no PATH interception can see it; here it resolves to a shim that
// records the invocation with its arguments and then, when the real file is a shell script,
// SOURCES that file so everything it would invoke is observed too. A step cannot reach a
// registry by moving the command into a script the repository already ships.
func (o *Observer) buildMirror() error {
	return filepath.WalkDir(o.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the tree is not this gate's business
		}
		rel, rerr := filepath.Rel(o.root, p)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(o.mirror, rel), 0o755)
		}
		info, ierr := d.Info()
		if ierr != nil || info.Mode()&0o111 == 0 {
			return nil
		}
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			abs = p
		}
		return os.WriteFile(filepath.Join(o.mirror, rel), []byte(o.shimText("./"+filepath.ToSlash(rel), abs, isShellScript(p))), 0o755)
	})
}

// isShellScript reports whether a repository file is a shell script this harness can descend
// into. A compiled binary or a script in another language is recorded and named, never run.
func isShellScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 128)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if !strings.HasPrefix(line, "#!") {
		return strings.HasSuffix(path, ".sh")
	}
	return strings.Contains(line, "sh")
}

// shimText is the recording shim the mirror puts in place of an executable repository file.
func (o *Observer) shimText(name, real string, descend bool) string {
	var b strings.Builder
	b.WriteString("#!" + o.bash + "\n")
	b.WriteString("# Recording shim written by the release-shape gate. It performs nothing.\n")
	b.WriteString(". \"$RELEASE_SHAPE_OBS_PRELUDE\"\n")
	fmt.Fprintf(&b, "__rs_invoke %s \"$@\" || exit $?\n", shQuote(name))
	if descend {
		b.WriteString("__rs_depth=$(( ${RELEASE_SHAPE_OBS_DEPTH:-0} + 1 ))\n")
		fmt.Fprintf(&b, "if [ \"$__rs_depth\" -le %d ]; then\n", obsMaxDepth)
		b.WriteString("  export RELEASE_SHAPE_OBS_DEPTH=$__rs_depth\n")
		fmt.Fprintf(&b, "  . %s\n", shQuote(real))
		b.WriteString("else\n")
		fmt.Fprintf(&b, "  __rs_invoke %s %s\n", obsNestedShell, shQuote(name))
		b.WriteString("fi\n")
	}
	b.WriteString("exit 0\n")
	return b.String()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// --- the prelude ------------------------------------------------------------------------

// preludeText is the environment itself: the recorder, the refusals, and the two departures
// from the runner. Every line of it is written here rather than inside a step so that one
// file is the single writer of what "observed" means.
func (o *Observer) preludeText() string {
	var b strings.Builder
	b.WriteString(`# The release-shape gate's hermetic observation environment.
#
# Nothing external runs. Every invocation is recorded with the argv bash built.

__rs_log=${RELEASE_SHAPE_OBS_LOG}

declare -A __rs_fail=()
if [ -n "${RELEASE_SHAPE_OBS_FAIL:-}" ]; then
  __rs_oldifs=$IFS
  IFS=$'\036'
  for __rs_k in ${RELEASE_SHAPE_OBS_FAIL}; do __rs_fail[$__rs_k]=1; done
  IFS=$__rs_oldifs
  unset __rs_k __rs_oldifs
fi

# __rs_invoke RECORDS one invocation and performs nothing. The exit status it returns is
# chosen by the caller (the exploration below), because a command whose failure decides a
# branch has to be able to fail here or that branch is never observed.
__rs_invoke() {
  local __k=d${RELEASE_SHAPE_OBS_DEPTH:-0}$'\037'$1 __a
  shift
  for __a in "$@"; do
    __a=${__a//$'\n'/ }
    __a=${__a//$'\037'/ }
    __k=$__k$'\037'$__a
  done
  printf '%s\n' "$__k" >> "$__rs_log"
  if [ -n "${__rs_fail[$__k]:-}" ]; then return 1; fi
  return 0
}

# PATH is one empty directory, so every command the shell resolves through PATH lands here.
command_not_found_handle() { __rs_invoke "$@"; }

# A nested shell is not a wall. Its -c argument is the script bash would have handed it, so
# it is evaluated HERE, in this same environment, and everything it invokes is recorded. A
# nested shell invoked any other way is recorded as unobserved and reds the gate.
__rs_shell() {
  local __prog=$1 __i __script=
  shift
  __rs_invoke "$__prog" "$@" || return $?
  for (( __i=1; __i<=$#; __i++ )); do
    if [ "${!__i}" = "-c" ]; then
      __i=$(( __i + 1 ))
      if [ "$__i" -le "$#" ]; then __script=${!__i}; fi
      break
    fi
  done
  if [ -n "$__script" ]; then
    ( eval "$__script" )
    return 0
  fi
  __rs_invoke '` + obsNestedShell + `' "$__prog"
  return 0
}
sh()   { __rs_shell sh "$@"; }
bash() { __rs_shell bash "$@"; }
dash() { __rs_shell dash "$@"; }
zsh()  { __rs_shell zsh "$@"; }
ksh()  { __rs_shell ksh "$@"; }

# THE FIRST DEPARTURE FROM THE RUNNER. The commands that would have created a file or a
# directory were recorded rather than run, so a later command fails for a reason the runner
# would never produce - and under set -e everything after it would go unobserved. Stripping
# -e and -u observes MORE of the step than the runner reaches, which cannot hide an act.
set() {
  local -a __a=()
  local __x __f __c
  while [ $# -gt 0 ]; do
    __x=$1
    case "$__x" in
      -o)
        case "${2:-}" in
          errexit|nounset) shift 2; continue ;;
        esac
        __a+=("$__x"); shift; continue ;;
      -[a-zA-Z]*)
        __f=${__x#-}
        __c=${__f//e/}
        __c=${__c//u/}
        if [ -n "$__c" ]; then __a+=("-$__c"); fi
        shift; continue ;;
      *) __a+=("$__x"); shift; continue ;;
    esac
  done
  if [ ${#__a[@]} -gt 0 ]; then builtin set "${__a[@]}"; fi
}

# THE REFUSAL. A command word that begins with a slash is resolved by bash directly, so no PATH
# interception can see it. The ones this gate shims are declared below and are allowed
# through to their shim; anything else is SKIPPED and recorded as unobservable, which reds
# the gate by name. This decides nothing about what the command does - it says the
# environment could not watch it, which is the opposite of the silence that lost six times.
__rs_guard() {
  local __c=$BASH_COMMAND __w
  case "$__c" in
    */*) ;;
    *) return 0 ;;
  esac
  while :; do
    __w=${__c%% *}
    case "$__w" in
      # An assignment prefix or a keyword is not the program. Tested FIRST, because an
      # assignment whose value is a registry reference contains a slash and is not a path.
      [A-Za-z_]*=*|if|then|elif|else|while|until|for|do|!|time|'{'|'('|'[[')
        case "$__c" in
          *' '*) __c=${__c#* } ;;
          *) return 0 ;;
        esac
        continue
        ;;
      /*)
        # An absolute path. Only the shims declared below may run; anything else would be
        # executed by bash directly, where nothing can watch it.
        if declare -F -- "$__w" >/dev/null 2>&1; then return 0; fi
        __rs_invoke '` + obsUnobservable + `' "$__w"
        return 1
        ;;
      */*)
        # A path into the working tree. The repository mirror answers these with a
        # recording shim, so one that is not there is a program this environment has no
        # copy of - and a step whose command silently vanished is the failure mode this
        # whole file exists to remove.
        if [ -x "$__w" ]; then return 0; fi
        __rs_invoke '` + obsUnobservable + `' "$__w"
        return 1
        ;;
      *) return 0 ;;
    esac
  done
}
`)
	// Absolute-path shims. A program this gate classifies is recorded wherever it really
	// lives, so `/usr/bin/docker push` - and a variable that expands to that path - is
	// observed rather than executed. Measured, not assumed: bash resolves a function name
	// containing slashes ahead of the file at that path, and it does so AFTER expansion.
	for _, p := range absoluteShimPaths() {
		fmt.Fprintf(&b, "function %s { __rs_invoke %s \"$@\"; }\n", p, shQuote(p))
	}
	b.WriteString("\nshopt -s extdebug\ntrap __rs_guard DEBUG\n")
	return b.String()
}

// absoluteShimPaths is every place on the real PATH where a program this gate classifies
// actually lives. Only paths that exist are shimmed, so the prelude stays small.
func absoluteShimPaths() []string {
	names := map[string]bool{}
	for n := range registryTools {
		names[n] = true
	}
	for n := range clientsCheckedAndNotInventoried {
		names[n] = true
	}
	for n := range programsCheckedAndLocal {
		names[n] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for n := range names {
			p := filepath.Join(dir, n)
			if seen[p] {
				continue
			}
			st, err := os.Stat(p)
			if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// --- observing --------------------------------------------------------------------------

// Observe runs one `run:` script in the environment above, exploring the exit statuses of
// the commands it records until the set of invocations stops growing, and returns the union.
func (o *Observer) Observe(script string, env map[string]string) (*Observation, error) {
	key := observationKey(script, env)
	if got, ok := o.cache[key]; ok {
		return got, nil
	}
	// A script bash REFUSES cannot be observed at all: it never starts, so an environment
	// that watches execution sees an empty set and would report a step that publishes as
	// publishing nothing. That is the vacuous pass in a new place, so the lexical reader takes
	// over for exactly this case. It reads MORE commands than any shell would run - an
	// unterminated quote is demoted rather than trusted - so it can add an act and can hide
	// none, which is the only direction a fallback here may be wrong in.
	if why, err := o.syntaxError(script); err != nil {
		return nil, err
	} else if why != "" {
		obs := &Observation{Converged: true, Unparseable: why}
		for _, c := range ShellCommands(script) {
			obs.Invocations = append(obs.Invocations, Invocation{Argv: c.Words})
		}
		o.cache[key] = obs
		return obs, nil
	}

	obs := &Observation{}
	seenInv := map[string]bool{}
	queue := [][]string{nil}
	queued := map[string]bool{"": true}

	for len(queue) > 0 && obs.Runs < obsMaxRuns {
		batch := queue
		if room := obsMaxRuns - obs.Runs; len(batch) > room {
			batch, queue = batch[:room], batch[room:]
		} else {
			queue = nil
		}
		results, err := o.runBatch(script, env, batch)
		if err != nil {
			return nil, err
		}
		obs.Runs += len(batch)
		for i, res := range results {
			fail := batch[i]
			if obs.Output == "" {
				obs.Output = res.output
			}
			fresh := 0
			for _, k := range res.keys {
				if seenInv[k] {
					continue
				}
				seenInv[k] = true
				fresh++
				words := strings.Split(k, obsArgSep)[1:] // the leading field is the nesting depth
				if len(words) == 0 {
					continue
				}
				switch words[0] {
				case obsComplete:
					continue
				case obsUnobservable, obsNestedShell:
					obs.Refusals = append(obs.Refusals, strings.Join(words, " "))
					continue
				}
				obs.Invocations = append(obs.Invocations, Invocation{Argv: words})
			}
			// Expand only a path that FOUND something. Every invocation is made to fail in
			// turn, because a branch guarded by a command's success is otherwise only ever
			// observed on one side of it; a run that reveals nothing new is a leaf, which
			// is what keeps this from enumerating the powerset of a long script.
			if (fresh == 0 && len(fail) > 0) || len(fail) >= obsMaxFlips {
				continue
			}
			for _, k := range res.keys {
				// Only the STEP's own commands are varied. A repository script the step
				// calls is observed along the path its own logic takes, which is a
				// property of that script rather than of this release definition - and
				// exploring inside it would re-run the whole script once per command it
				// contains.
				if !strings.HasPrefix(k, "d0"+obsArgSep) || strings.Contains(k, obsArgSep+"__release_shape_") {
					continue
				}
				next := append(append([]string{}, fail...), k)
				sort.Strings(next)
				id := strings.Join(next, obsSetSep)
				if queued[id] {
					continue
				}
				queued[id] = true
				queue = append(queue, next)
			}
		}
	}
	obs.Converged = len(queue) == 0
	o.cache[key] = obs
	return obs, nil
}

// syntaxError asks bash whether it would accept this script at all, without running any of
// it. An empty first result means it would.
func (o *Observer) syntaxError(script string) (string, error) {
	dir, err := os.MkdirTemp(o.dir, "parse-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(p, []byte(script+"\n"), 0o600); err != nil {
		return "", err
	}
	cmd := exec.Command(o.bash, "-n", p)
	cmd.Env = []string{"PATH=" + o.empty, "HOME=" + dir}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return "", nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return msg, nil
}

// runResult is what one execution of the observed script produced.
type runResult struct {
	keys   []string
	output string
}

// runBatch executes one generation of the exploration. The runs in a generation do not depend
// on each other, so they go in parallel; each gets its own working directory, so a step that
// writes a file cannot change what a sibling run sees.
func (o *Observer) runBatch(script string, env map[string]string, batch [][]string) ([]runResult, error) {
	out := make([]runResult, len(batch))
	errs := make([]error, len(batch))
	workers := runtime.NumCPU()
	if workers > len(batch) {
		workers = len(batch)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				keys, output, err := o.runOnce(script, env, batch[i])
				out[i], errs[i] = runResult{keys: keys, output: output}, err
			}
		}()
	}
	for i := range batch {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// runOnce executes the script once, with the named invocations made to fail.
func (o *Observer) runOnce(script string, env map[string]string, fail []string) ([]string, string, error) {
	dir, err := os.MkdirTemp(o.dir, "run-")
	if err != nil {
		return nil, "", err
	}
	// The working directory is a fresh view of the repository mirror: one symlink per
	// top-level entry, so `./scripts/x.sh` resolves to its recording shim while anything the
	// step CREATES lands here and cannot be seen by another run.
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(o.mirror)
	if err != nil {
		return nil, "", err
	}
	for _, e := range entries {
		if err := os.Symlink(filepath.Join(o.mirror, e.Name()), filepath.Join(work, e.Name())); err != nil {
			return nil, "", err
		}
	}
	log := filepath.Join(dir, "argv.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		return nil, "", err
	}
	body := ". \"$RELEASE_SHAPE_OBS_PRELUDE\"\n" + script + "\n__rs_invoke " + obsComplete + "\n"
	path := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return nil, "", err
	}

	full := map[string]string{
		"PATH":                      o.empty,
		"HOME":                      dir,
		"USER":                      "runner",
		"LOGNAME":                   "runner",
		"SHELL":                     "/bin/bash",
		"LANG":                      "C",
		"LC_ALL":                    "C",
		"TZ":                        "UTC",
		"TMPDIR":                    dir,
		"CI":                        "true",
		"GITHUB_ACTIONS":            "true",
		"RUNNER_OS":                 "Linux",
		"RUNNER_ARCH":               "X64",
		"RUNNER_TEMP":               dir,
		"GITHUB_WORKSPACE":          work,
		"GITHUB_OUTPUT":             filepath.Join(dir, "github_output"),
		"GITHUB_ENV":                filepath.Join(dir, "github_env"),
		"GITHUB_PATH":               filepath.Join(dir, "github_path"),
		"GITHUB_STEP_SUMMARY":       filepath.Join(dir, "github_step_summary"),
		"RELEASE_SHAPE_OBS_LOG":     log,
		"RELEASE_SHAPE_OBS_FAIL":    strings.Join(fail, obsSetSep),
		"RELEASE_SHAPE_OBS_DEPTH":   "0",
		"RELEASE_SHAPE_OBS_PRELUDE": o.prelude,
	}
	for k, v := range env {
		full[k] = v
	}
	envv := make([]string, 0, len(full))
	for k, v := range full {
		envv = append(envv, k+"="+v)
	}

	cmd := exec.Command(o.bash, path)
	cmd.Dir = work
	cmd.Env = envv
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(obsTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return nil, "", fmt.Errorf("the step's shell did not finish within %s while being observed; this gate refuses to guess what it would have invoked", obsTimeout)
	}
	_ = runErr // a non-zero exit is ordinary here: the step is being watched, not graded

	raw, err := os.ReadFile(log)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read what the observed step invoked: %w", err)
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line != "" {
			keys = append(keys, line)
		}
	}
	return keys, string(out), nil
}

func signature(keys []string) string {
	c := append([]string{}, keys...)
	sort.Strings(c)
	return strings.Join(c, obsSetSep)
}

func observationKey(script string, env map[string]string) string {
	h := sha256.New()
	h.Write([]byte(script))
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		h.Write([]byte("\x00" + k + "\x00" + env[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

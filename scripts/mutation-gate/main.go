// Command mutation-gate runs the pinned mutation runner over this module's mutation
// domain, decides the result against the committed floor, and tells a human when the
// scheduled run goes red (testing T5).
//
// Coverage proves that a line EXECUTED. It says nothing about whether anything asserted
// on it, and this is the repository whose whole safety property is that a source file is
// not destroyed until its replacement is provably faithful. A mutation score answers the
// other question: deliberate faults are introduced into the domain one at a time, the
// suite is re-run, and the score is the percentage of them the suite noticed.
//
// The floor is applied to EFFICACY, KILLED / (KILLED + LIVED) - the faults the suite ran
// and caught, out of the faults it ran. It is NOT applied to mutant coverage,
// (KILLED + LIVED) / (KILLED + LIVED + NOT_COVERED), which measures reach and is what the
// line-coverage figure already claims.
//
// The figure is computed HERE from the runner's own mutant counts rather than taken from
// the percentage the runner prints, so the definition the floor is applied to lives in
// this repository. The runner's figure is read as well and a disagreement is RED: a
// runner that changed what it means by efficacy must not be able to move this gate by
// changing a number nobody re-derives.
//
// Exit codes are distinct per failure mode (pinning P7); scripts/mutation.sh propagates
// them and its header lists them.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/scripts/mutation"
)

// Exit codes. Each names one failure, so a caller can tell "the suite is not asserting
// enough" from "the gate could not run at all" without reading the message.
const (
	exitOK            = 0
	exitUsage         = 2
	exitBelowFloor    = 3
	exitRunner        = 4 // the pinned runner could not be obtained, or could not execute
	exitReport        = 5 // the runner ran but its report cannot be believed
	exitConfig        = 6 // the committed configuration could not be read
	exitDiffReference = 7 // the diff reference could not be resolved to a merge base
	exitNotify        = 8 // the tracking issue could not be opened or updated
	exitBaseline      = 9 // the domain's own suite is red, so nothing can be measured
)

// heartbeat is how often a run says it is still working (observability O5). The unscoped
// run re-runs a package's suite once per mutant and can take hours; a run that goes
// silent under a supervisor is a run that gets killed and reported as a gate failure.
const heartbeat = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mutation-gate - run the pinned mutation runner and apply the committed floor

  mutation-gate run    --root <dir> --mode diff|full [--ref <git ref>]
                       --runner-version <vX.Y.Z> --out <report.json>
  mutation-gate grade  --root <dir> --mode diff|full [--ref <git ref>]
                       --raw <runner report.json> --out <report.json>
                       [--timed-out <n>] [--runner-version <vX.Y.Z>]
  mutation-gate notify --root <dir> --repo <owner/name> --assignee <login>
                       --run-url <url> [--report <report.json>] [--failure <text>]
                       [--api <base url>]

  run     obtains the runner, mutates the domain (diff mode: only the files in it that
          the change touched), writes the machine-readable report, applies the floor.
  grade   applies the floor to a runner report that already exists. This is the same
          decision run makes; it is reachable on its own so the self-test can put a
          report below the floor in front of it and prove the gate still bites.
  notify  opens or updates the ONE tracking issue for a red scheduled run.

exit: 0 ok · 2 bad invocation · 3 below the floor · 4 runner unobtainable or unable to
run · 5 unusable report · 6 unreadable configuration · 7 unresolvable diff reference ·
8 the tracking issue could not be opened or updated · 9 the domain's suite is red
`)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "grade":
		return cmdGrade(args[1:], stdout, stderr)
	case "notify":
		return cmdNotify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "mutation-gate: unknown subcommand %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

// --- the runner's own report -------------------------------------------------------

type runnerReport struct {
	GoModule          string   `json:"go_module"`
	TestEfficacy      *float64 `json:"test_efficacy"`
	MutationsCoverage *float64 `json:"mutations_coverage"`
	MutantsTotal      int      `json:"mutants_total"`
	MutantsKilled     int      `json:"mutants_killed"`
	MutantsLived      int      `json:"mutants_lived"`
	MutantsNotViable  int      `json:"mutants_not_viable"`
	MutantsNotCovered int      `json:"mutants_not_covered"`
	// The runner does not report this one. It is counted from the per-mutant statuses,
	// because a timed-out mutant is in neither side of the score and a reader is owed the
	// number.
	MutantsTimedOut int     `json:"mutants_timed_out,omitempty"`
	ElapsedTime     float64 `json:"elapsed_time"`
	Files           []struct {
		FileName  string `json:"file_name"`
		Mutations []struct {
			Line   int    `json:"line"`
			Column int    `json:"column"`
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"mutations"`
	} `json:"files"`
}

func readRunnerReport(path string) (runnerReport, error) {
	var rr runnerReport
	b, err := os.ReadFile(path)
	if err != nil {
		return rr, fmt.Errorf("read the runner's report %s: %w", path, err)
	}
	if len(b) == 0 {
		return rr, fmt.Errorf("the runner's report %s is empty - it did not measure anything", path)
	}
	if err := json.Unmarshal(b, &rr); err != nil {
		return rr, fmt.Errorf("parse the runner's report %s: %w", path, err)
	}
	return rr, nil
}

// --- grading ------------------------------------------------------------------------

type gradeInput struct {
	root      string
	mode      string
	ref       string
	rawPath   string
	outPath   string
	timedOut  int
	runnerVer string
	scoped    []string // the in-domain files the diff touched; empty in full mode
}

// grade reads a runner report off disk and applies the floor. The self-test drives this
// path directly with a fabricated report, so what it defeats is what runs in CI.
func grade(in gradeInput, stdout, stderr io.Writer) int {
	cfg, err := mutation.ReadConfig(in.root)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		return exitConfig
	}
	rr, err := readRunnerReport(in.rawPath)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		fmt.Fprintf(stderr, "       A report that cannot be read is not a clean run, and no score is being reported in its place.\n")
		return exitReport
	}
	return gradeReport(cfg, rr, in, stdout, stderr)
}

// gradeReport applies the floor to the mutant counts and writes the machine-readable
// report. It is the whole decision, and both routes into the gate come through it.
func gradeReport(cfg mutation.Config, rr runnerReport, in gradeInput, stdout, stderr io.Writer) int {
	floor := cfg.Floor()

	rep := mutation.Report{
		Mode:        in.mode,
		DiffRef:     in.ref,
		InScope:     true,
		ScopedFiles: in.scoped,
		Floor:       floor,
		Mutants: mutation.MutantCounts{
			Total:      rr.MutantsTotal,
			Killed:     rr.MutantsKilled,
			Lived:      rr.MutantsLived,
			NotCovered: rr.MutantsNotCovered,
			NotViable:  rr.MutantsNotViable,
			TimedOut:   in.timedOut,
		},
		Runner: mutation.RunnerInfo{
			Module:                    mutation.RunnerModule,
			Version:                   in.runnerVer,
			ReportedEfficacy:          rr.TestEfficacy,
			ReportedMutationsCoverage: rr.MutationsCoverage,
		},
		Files: fileReports(rr),
		Domain: mutation.Domain{
			Config:       mutation.ConfigName,
			ExcludeFiles: cfg.ExcludeFiles(),
		},
	}

	ran := rr.MutantsKilled + rr.MutantsLived
	if ran == 0 {
		// Nothing was RUN. In diff mode that is the ordinary case of a change the runner
		// found no mutable construct in, and the answer is a null score and a pass. In
		// full mode it means the whole domain produced no runnable mutant, which is a
		// gate that measured nothing and must not report green.
		rep.Score = nil
		rep.Verdict = mutation.VerdictNoMutants
		if err := mutation.WriteReport(in.outPath, rep); err != nil {
			fmt.Fprintf(stderr, "::error::mutation gate: could not write %s: %v\n", in.outPath, err)
			return exitReport
		}
		if in.mode == mutation.ModeFull {
			fmt.Fprintf(stderr, "::error::mutation gate: the runner ran over the whole domain and produced NO runnable mutant (killed 0, lived 0, not covered %d). The gate measured nothing; refusing to report that as a pass.\n", rr.MutantsNotCovered)
			return exitReport
		}
		fmt.Fprintf(stdout, "mutation gate: no mutant was in scope - the changed lines inside the domain carry no mutable construct. Report: %s\n", in.outPath)
		return exitOK
	}

	score := 100 * float64(rr.MutantsKilled) / float64(ran)
	rep.Score = &score

	// The runner's own percentage must agree with the definition the floor is applied to.
	// It comes out of the same report, so this costs nothing and closes the route where
	// the runner's meaning of efficacy moves and this gate never notices.
	if rr.TestEfficacy != nil && math.Abs(*rr.TestEfficacy-score) > 0.05 {
		fmt.Fprintf(stderr, "::error::mutation gate: the runner reports efficacy %.2f%% but KILLED/(KILLED+LIVED) over the same counts is %.2f%% (killed %d, lived %d). The figure the floor is applied to is the second one, and a runner whose own figure no longer means that cannot be graded against this floor.\n", *rr.TestEfficacy, score, rr.MutantsKilled, rr.MutantsLived)
		return exitReport
	}

	if score+1e-9 < floor {
		rep.Verdict = mutation.VerdictBelowFloor
	} else {
		rep.Verdict = mutation.VerdictPass
	}
	if err := mutation.WriteReport(in.outPath, rep); err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not write %s: %v\n", in.outPath, err)
		return exitReport
	}

	printSummary(stdout, rep, in.outPath)
	if rep.Verdict == mutation.VerdictBelowFloor {
		fmt.Fprintf(stderr, "::error::mutation score %.2f%% is BELOW the floor of %.2f%% (%s mode). %d of the %d mutants the suite RAN survived it: those lines execute under test and nothing asserts on what the mutation changed. The remedy is an assertion that kills a survivor, never a lower floor - %s owns the figure and %s says why.\n",
			score, floor, rep.Mode, rr.MutantsLived, ran, mutation.ConfigName, mutation.DocName)
		return exitBelowFloor
	}
	return exitOK
}

func printSummary(w io.Writer, rep mutation.Report, outPath string) {
	score := "null"
	if rep.Score != nil {
		score = fmt.Sprintf("%.2f%%", *rep.Score)
	}
	fmt.Fprintf(w, "mutation score %s against a floor of %.2f%% (%s mode)\n", score, rep.Floor, rep.Mode)
	fmt.Fprintf(w, "  killed %d, lived %d, not covered %d, not viable %d, timed out %d, files mutated %d\n",
		rep.Mutants.Killed, rep.Mutants.Lived, rep.Mutants.NotCovered, rep.Mutants.NotViable, rep.Mutants.TimedOut, len(rep.Files))
	if rep.Mutants.TimedOut > 0 {
		fmt.Fprintf(w, "  note: %d mutant(s) TIMED OUT and are in neither figure. A run with many of those measured the machine as much as the suite; %s carries the timeout budget.\n", rep.Mutants.TimedOut, mutation.ConfigName)
	}
	fmt.Fprintf(w, "  report: %s\n", outPath)
}

func fileReports(rr runnerReport) []mutation.FileReport {
	out := make([]mutation.FileReport, 0, len(rr.Files))
	for _, f := range rr.Files {
		fr := mutation.FileReport{FileName: f.FileName}
		for _, m := range f.Mutations {
			switch strings.ToUpper(strings.ReplaceAll(m.Status, " ", "_")) {
			case "KILLED":
				fr.Killed++
			case "LIVED":
				fr.Lived++
			case "NOT_COVERED":
				fr.NotCovered++
			case "NOT_VIABLE":
				fr.NotViable++
			case "TIMED_OUT":
				fr.TimedOut++
			}
		}
		out = append(out, fr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FileName < out[j].FileName })
	return out
}

// --- subcommands --------------------------------------------------------------------

func cmdGrade(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the module root holding "+mutation.ConfigName)
	mode := fs.String("mode", "", "diff or full")
	ref := fs.String("ref", "", "the git reference a diff-scoped run was taken against")
	raw := fs.String("raw", "", "the runner's own JSON report")
	out := fs.String("out", "", "where to write the machine-readable report")
	timedOut := fs.Int("timed-out", 0, "how many mutants timed out, counted off the runner's output")
	version := fs.String("runner-version", "", "the pinned runner version this report came from")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *mode != mutation.ModeDiff && *mode != mutation.ModeFull {
		fmt.Fprintf(stderr, "mutation-gate grade: --mode must be %q or %q\n", mutation.ModeDiff, mutation.ModeFull)
		return exitUsage
	}
	if *raw == "" || *out == "" {
		fmt.Fprintf(stderr, "mutation-gate grade: --raw and --out are both required\n")
		return exitUsage
	}
	return grade(gradeInput{
		root: *root, mode: *mode, ref: *ref, rawPath: *raw, outPath: *out,
		timedOut: *timedOut, runnerVer: *version,
	}, stdout, stderr)
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the module root holding "+mutation.ConfigName)
	mode := fs.String("mode", "", "diff or full")
	ref := fs.String("ref", "", "the git reference to take the diff scope against (diff mode)")
	version := fs.String("runner-version", "", "the pinned runner version; the Makefile owns it")
	out := fs.String("out", "", "where to write the machine-readable report")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case *mode != mutation.ModeDiff && *mode != mutation.ModeFull:
		fmt.Fprintf(stderr, "mutation-gate run: --mode must be %q or %q\n", mutation.ModeDiff, mutation.ModeFull)
		return exitUsage
	case *mode == mutation.ModeDiff && *ref == "":
		fmt.Fprintf(stderr, "mutation-gate run: --mode diff needs the --ref it scopes against\n")
		return exitUsage
	case *version == "":
		fmt.Fprintf(stderr, "mutation-gate run: --runner-version is required; the Makefile owns the pin\n")
		return exitUsage
	case *out == "":
		fmt.Fprintf(stderr, "mutation-gate run: --out is required\n")
		return exitUsage
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(stderr, "mutation-gate run: %v\n", err)
		return exitUsage
	}

	cfg, err := mutation.ReadConfig(absRoot)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		return exitConfig
	}
	excludes, err := compileExcludes(cfg.ExcludeFiles())
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		return exitConfig
	}

	var scoped []string
	if *mode == mutation.ModeDiff {
		scoped, err = scopeFiles(absRoot, *ref, excludes)
		if err != nil {
			fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
			fmt.Fprintf(stderr, "       A diff-scoped run cannot decide what changed without it. In a pull request that reference is the base branch, and resolving it needs the full history (actions/checkout with fetch-depth: 0).\n")
			return exitDiffReference
		}
		if len(scoped) == 0 {
			rep := mutation.Report{
				Mode: *mode, DiffRef: *ref, InScope: false, ScopedFiles: []string{},
				Floor: cfg.Floor(), Score: nil, Verdict: mutation.VerdictNoMutants,
				Runner: mutation.RunnerInfo{Module: mutation.RunnerModule, Version: *version},
				Domain: mutation.Domain{Config: mutation.ConfigName, ExcludeFiles: cfg.ExcludeFiles()},
			}
			if err := mutation.WriteReport(*out, rep); err != nil {
				fmt.Fprintf(stderr, "::error::mutation gate: could not write %s: %v\n", *out, err)
				return exitReport
			}
			fmt.Fprintf(stdout, "mutation gate: NO MUTANT IN SCOPE - this change touches no Go file inside the mutation domain, measured against %s. Nothing to mutate is not a score of zero, so this passes. Report: %s\n", *ref, *out)
			return exitOK
		}
		fmt.Fprintf(stdout, "mutation gate: %d file(s) inside the mutation domain changed against %s:\n", len(scoped), *ref)
		for _, f := range scoped {
			fmt.Fprintf(stdout, "  %s\n", f)
		}
	}

	// WHICH PACKAGES the runner is pointed at is derived from the same exclusion list, so
	// the domain has one definition. It matters that this is a list rather than "the
	// module": the runner gathers coverage for whatever it is given, and handed the module
	// root it would run the suites the exclusion list exists to keep out - the real encodes
	// behind the verify gate among them.
	pkgs, err := domainPackages(absRoot, excludes, scoped, *mode)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		return exitConfig
	}
	if len(pkgs) == 0 {
		fmt.Fprintf(stderr, "::error::mutation gate: the mutation domain is EMPTY - every package in the module matches an exclusion in %s. A gate with nothing to mutate measures nothing.\n", mutation.ConfigName)
		return exitConfig
	}

	if code := requireGreenSuite(absRoot, pkgs, stdout, stderr); code != exitOK {
		return code
	}

	runnerBin, code := obtainRunner(absRoot, *version, stdout, stderr)
	if code != exitOK {
		return code
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(runnerBin)) }()

	var changed map[string][]lineRange
	if *mode == mutation.ModeDiff {
		changed, err = changedLines(absRoot, *ref, scoped)
		if err != nil {
			fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
			return exitDiffReference
		}
	}

	rawPath := filepath.Join(filepath.Dir(mustAbs(*out)), "mutation-runner-report.json")
	rr, timedOut, code := invokeRunner(absRoot, runnerBin, *version, *mode, *ref, pkgs, scoped, changed, rawPath, stdout, stderr)
	if code != exitOK {
		return code
	}

	return gradeReport(cfg, rr, gradeInput{
		root: absRoot, mode: *mode, ref: *ref, rawPath: rawPath, outPath: *out,
		timedOut: timedOut, runnerVer: *version, scoped: scoped,
	}, stdout, stderr)
}

// domainPackages lists the package directories the runner may be pointed at: every
// package in the module whose path is not covered by the exclusion list. In diff mode it
// is narrowed again to the packages the changed files are in.
func domainPackages(root string, excludes []*regexp.Regexp, scoped []string, mode string) ([]string, error) {
	list := exec.Command("go", "list", "-f", "{{.Dir}}", "./...")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("could not list this module's packages: %w", err)
	}
	wanted := map[string]bool{}
	if mode == mutation.ModeDiff {
		for _, f := range scoped {
			wanted[filepath.Dir(f)] = true
		}
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dir := strings.TrimSpace(line)
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		// The exclusion patterns are written against file paths, so a package is matched
		// as the prefix its files share.
		if excluded(rel+"/", excludes) {
			continue
		}
		if mode == mutation.ModeDiff && !wanted[rel] {
			continue
		}
		pkgs = append(pkgs, rel)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func compileExcludes(patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: exclude-files pattern %q does not compile: %w", mutation.ConfigName, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func excluded(path string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// scopeFiles returns the Go files inside the mutation domain that differ from the merge
// base with ref, sorted. Test files are not in the domain: the runner does not mutate
// them, and a change that touches only tests has nothing to mutate.
func scopeFiles(root, ref string, excludes []*regexp.Regexp) ([]string, error) {
	base, err := git(root, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, fmt.Errorf("could not resolve a merge base between HEAD and %q: %w", ref, err)
	}
	base = strings.TrimSpace(base)
	changed, err := git(root, "diff", "--name-only", base, "--")
	if err != nil {
		return nil, fmt.Errorf("could not list the files changed since %s (%s): %w", ref, base, err)
	}
	var out []string
	for _, line := range strings.Split(changed, "\n") {
		f := strings.TrimSpace(line)
		switch {
		case f == "", !strings.HasSuffix(f, ".go"), strings.HasSuffix(f, "_test.go"):
			continue
		case excluded(f, excludes):
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// lineRange is a span of lines in a file, as the merge base sees it changed.
type lineRange struct{ from, to int }

// changedLines returns, per changed file, the lines that differ from the merge base with
// ref. It is what makes a diff-scoped run a statement about THIS change: a mutant on a
// line the change did not touch is not this pull request's to answer for.
//
// The filtering is done here, on the runner's report, rather than through the runner's own
// diff flag. That flag matches the diff against the mutant's file path, and the paths the
// runner reports are relative to the package it was pointed at, so with the per-package
// invocation this gate uses it matches nothing and skips every mutant. Reading the hunk
// headers is the same question asked where the answer is unambiguous.
func changedLines(root, ref string, files []string) (map[string][]lineRange, error) {
	base, err := git(root, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, fmt.Errorf("could not resolve a merge base between HEAD and %q: %w", ref, err)
	}
	base = strings.TrimSpace(base)
	out := map[string][]lineRange{}
	for _, f := range files {
		patch, err := git(root, "diff", "-U0", base, "--", f)
		if err != nil {
			return nil, fmt.Errorf("could not read the changed lines of %s: %w", f, err)
		}
		for _, line := range strings.Split(patch, "\n") {
			if !strings.HasPrefix(line, "@@") {
				continue
			}
			r, ok := parseHunk(line)
			if ok {
				out[f] = append(out[f], r)
			}
		}
	}
	return out, nil
}

var hunkRe = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func parseHunk(line string) (lineRange, bool) {
	m := hunkRe.FindStringSubmatch(line)
	if m == nil {
		return lineRange{}, false
	}
	start, err := strconv.Atoi(m[1])
	if err != nil {
		return lineRange{}, false
	}
	count := 1
	if m[2] != "" {
		count, err = strconv.Atoi(m[2])
		if err != nil {
			return lineRange{}, false
		}
	}
	if count == 0 {
		// A pure deletion adds no line to answer for.
		return lineRange{}, false
	}
	return lineRange{from: start, to: start + count - 1}, true
}

func inRanges(line int, ranges []lineRange) bool {
	for _, r := range ranges {
		if line >= r.from && line <= r.to {
			return true
		}
	}
	return false
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// requireGreenSuite refuses to measure a domain whose tests are already failing.
//
// This is the cheapest way a mutation gate can go silently green, and it is not a
// hypothetical: a mutant is judged KILLED when the package's test command FAILS, so on a
// tree where that command already fails - a red test, a package that does not compile -
// every single mutant is judged caught and the run reports a perfect score it never
// measured. The suites in the domain are the fast ones by construction, so asking them
// first costs seconds and removes the whole failure mode.
func requireGreenSuite(root string, pkgs []string, stdout, stderr io.Writer) int {
	args := append([]string{"test", "-count=1"}, packagePatterns(pkgs)...)
	fmt.Fprintf(stdout, "mutation gate: the domain's own suite must be green before anything is mutated (go %s)\n", strings.Join(args[:2], " "))
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: THE DOMAIN'S OWN SUITE IS NOT GREEN, so no mutation score can be measured.\n")
		fmt.Fprintf(stderr, "       tried: go %s (in %s)\n", strings.Join(args, " "), root)
		fmt.Fprintf(stderr, "       error: %v\n", err)
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				fmt.Fprintf(stderr, "       %s\n", line)
			}
		}
		fmt.Fprintf(stderr, "       A mutant is judged by whether the package's tests FAIL, so on a tree where they already fail every mutant is judged caught and the run reports a perfect score it never measured.\n")
		fmt.Fprintf(stderr, "       next: STOPPING, with no score reported. Fix the suite and run this again.\n")
		return exitBaseline
	}
	fmt.Fprintf(stdout, "  the domain's suite is green across %d package(s)\n", len(pkgs))
	return exitOK
}

func packagePatterns(pkgs []string) []string {
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, "./"+p)
	}
	return out
}

// obtainRunner installs the pinned runner into a temporary GOBIN and returns the binary.
//
// This is the dependency-failure route (observability O4): it names WHICH dependency,
// WHAT was tried and WHAT HAPPENS NEXT, which is that this gate stops. It emits no score,
// no coverage figure and no other number in place of one - a gate that could not run has
// not passed, and the one thing a mutation gate must never do is answer the question it
// did not measure.
//
// It is installed rather than `go run` per invocation because the runner is pointed at
// one package at a time, and obtaining it once is also what makes "could not be obtained"
// a single, early, unambiguous failure rather than one that could surface halfway through
// a domain.
func obtainRunner(root, version string, stdout, stderr io.Writer) (string, int) {
	spec := mutation.RunnerModule + "@" + version
	fmt.Fprintf(stdout, "mutation gate: obtaining the pinned runner %s\n", spec)

	gobin, err := os.MkdirTemp("", "holdfast-mutation-runner")
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not create a directory for the runner: %v\n", err)
		return "", exitRunner
	}

	stop := func(what string, err error, output string) (string, int) {
		fmt.Fprintf(stderr, "::error::mutation gate: THE PINNED MUTATION RUNNER COULD NOT BE OBTAINED.\n")
		fmt.Fprintf(stderr, "       dependency: %s\n", mutation.RunnerModule)
		fmt.Fprintf(stderr, "       pinned version: %s\n", version)
		fmt.Fprintf(stderr, "       command that failed: %s (in %s)\n", what, root)
		fmt.Fprintf(stderr, "       error: %v\n", err)
		for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				fmt.Fprintf(stderr, "       %s\n", line)
			}
		}
		fmt.Fprintf(stderr, "       next: STOPPING. No mutation score, no coverage figure and no other number is being reported in its place; a gate that could not run is red, not green. The pin lives in the Makefile (GREMLINS_VERSION).\n")
		_ = os.RemoveAll(gobin)
		return "", exitRunner
	}

	install := exec.Command("go", "install", spec)
	install.Dir = root
	install.Env = append(os.Environ(), "GOBIN="+gobin)
	var buf strings.Builder
	install.Stdout = &buf
	install.Stderr = &buf
	if err := install.Run(); err != nil {
		return stop("go install "+spec, err, buf.String())
	}

	bin := filepath.Join(gobin, path.Base(mutation.RunnerModule))
	if _, err := os.Stat(bin); err != nil {
		return stop("go install "+spec, fmt.Errorf("the install left no binary at %s: %w", bin, err), buf.String())
	}

	// Installed is not the same as runnable. Ask it what it is, so that a binary that
	// cannot execute here fails now, with the pin named, rather than mid-domain.
	buf.Reset()
	check := exec.Command(bin, "--version")
	check.Dir = root
	check.Stdout = &buf
	check.Stderr = &buf
	if err := check.Run(); err != nil {
		return stop(bin+" --version", err, buf.String())
	}
	fmt.Fprintf(stdout, "  %s\n", strings.TrimSpace(buf.String()))
	return bin, exitOK
}

// progress carries what the run has seen so far, for the heartbeat and for the timed-out
// count the runner's report does not carry.
type progress struct {
	mu       sync.Mutex
	seen     int
	killed   int
	lived    int
	timedOut int
}

func (p *progress) observe(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.HasPrefix(line, "KILLED"):
		p.seen++
		p.killed++
	case strings.HasPrefix(line, "LIVED"):
		p.seen++
		p.lived++
	case strings.HasPrefix(line, "TIMED OUT"):
		p.seen++
		p.timedOut++
	case strings.HasPrefix(line, "NOT COVERED"), strings.HasPrefix(line, "NOT VIABLE"), strings.HasPrefix(line, "SKIPPED"):
		p.seen++
	}
}

func (p *progress) snapshot() (int, int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen, p.killed, p.lived, p.timedOut
}

// invokeRunner mutates each package of the domain in turn and returns the merged report.
//
// One package at a time, and not the module root, because the runner gathers coverage for
// whatever it is pointed at: handed the module it would run every suite in it, including
// the real-encode suites the exclusion list exists to keep out, and the run would need the
// pinned ffmpeg to measure packages that have nothing to do with it.
//
// The runner's own exit codes are part of its interface: 10 means the efficacy threshold
// in the committed configuration was not met for that package, which is a RESULT and not a
// failure to run. The floor is a statement about the DOMAIN, so that verdict is ignored
// here and re-derived once over the merged counts. Anything else non-zero means the runner
// did not complete, and that is exit 4.
func invokeRunner(root, bin, version, mode, ref string, pkgs, scoped []string, changed map[string][]lineRange, rawPath string, stdout, stderr io.Writer) (runnerReport, int, int) {
	var merged runnerReport
	merged.GoModule = ""

	// A stale report from an earlier run must never be read as this one's.
	_ = os.Remove(rawPath)

	work, err := os.MkdirTemp("", "holdfast-mutation-reports")
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not create a directory for the runner's reports: %v\n", err)
		return merged, 0, exitRunner
	}
	defer func() { _ = os.RemoveAll(work) }()

	p := &progress{}
	started := time.Now()
	done := make(chan struct{})

	// observability O5: this is the operation that can take hours. It says where it is at
	// least every thirty seconds, so a reader (and a supervisor) can tell a long run from
	// a hung one.
	go func() {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s, k, l, to := p.snapshot()
				fmt.Fprintf(stdout, "mutation gate: still running after %s - %d mutant(s) reported so far (killed %d, lived %d, timed out %d)\n",
					time.Since(started).Round(time.Second), s, k, l, to)
			}
		}
	}()
	defer close(done)

	fmt.Fprintf(stdout, "mutation gate: mutating %d package(s) of the domain\n", len(pkgs))
	for i, pkg := range pkgs {
		reportPath := filepath.Join(work, fmt.Sprintf("%03d.json", i))
		args := []string{"unleash", "--output", reportPath}
		// In diff mode the package's OTHER files are kept out of the run, so the runner
		// only mutates what the change touched. Which of those mutants count is decided
		// again on the report, by line.
		if mode == mutation.ModeDiff {
			others, err := otherFiles(root, pkg, scoped)
			if err != nil {
				fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
				return merged, 0, exitReport
			}
			for _, f := range others {
				args = append(args, "--exclude-files", "(^|/)"+regexp.QuoteMeta(f)+"$")
			}
		}
		args = append(args, "./"+pkg)

		fmt.Fprintf(stdout, "mutation gate: [%d/%d] %s\n", i+1, len(pkgs), pkg)
		if code := runOne(root, bin, version, pkg, args, p, stdout, stderr); code != exitOK {
			_ = os.Remove(rawPath)
			return merged, p.timedOut, code
		}

		// No file means the runner found nothing to report for that package - a package
		// with no tests at all, most often. That is zero mutants, not a failure.
		if _, err := os.Stat(reportPath); err != nil {
			fmt.Fprintf(stdout, "  no mutants reported for %s\n", pkg)
			continue
		}
		one, err := readRunnerReport(reportPath)
		if err != nil {
			fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
			fmt.Fprintf(stderr, "       A report that cannot be read is not a clean run, and no score is being reported in its place.\n")
			return merged, p.timedOut, exitReport
		}
		mergeReport(&merged, one, pkg, mode, changed)
	}

	// The merged figure is the definition the floor is applied to, over the whole domain.
	ran := merged.MutantsKilled + merged.MutantsLived
	if ran > 0 {
		e := 100 * float64(merged.MutantsKilled) / float64(ran)
		merged.TestEfficacy = &e
	}
	merged.MutantsTotal = ran
	if b, err := json.MarshalIndent(merged, "", "  "); err == nil {
		_ = os.WriteFile(rawPath, append(b, '\n'), 0o644)
	}

	s, _, _, _ := p.snapshot()
	fmt.Fprintf(stdout, "mutation gate: the runner finished in %s (%d mutant(s) reported, %d of them in scope and timed out)\n",
		time.Since(started).Round(time.Second), s, merged.MutantsTimedOut)
	return merged, merged.MutantsTimedOut, exitOK
}

// runOne drives the runner over a single package, streaming its output so the heartbeat
// has something to count.
func runOne(root, bin, version, pkg string, args []string, p *progress, stdout, stderr io.Writer) int {
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not read the runner's output: %v\n", err)
		return exitRunner
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: THE PINNED MUTATION RUNNER COULD NOT EXECUTE.\n")
		fmt.Fprintf(stderr, "       dependency: %s\n       pinned version: %s\n       command that failed: %s %s\n       error: %v\n       next: STOPPING, with no score reported.\n",
			mutation.RunnerModule, version, bin, strings.Join(args, " "), err)
		return exitRunner
	}

	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(stdout, line)
		p.observe(strings.TrimSpace(line))
	}
	waitErr := cmd.Wait()
	if scanErr := sc.Err(); scanErr != nil {
		fmt.Fprintf(stderr, "mutation gate: the runner's output could not be read to the end: %v\n", scanErr)
	}
	if waitErr == nil {
		return exitOK
	}

	var ee *exec.ExitError
	if errors.As(waitErr, &ee) && ee.ExitCode() == 10 {
		// The per-package efficacy threshold. The floor is about the domain, so this is
		// not the verdict; the merged counts are graded once, at the end.
		return exitOK
	}
	code := -1
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
	}
	fmt.Fprintf(stderr, "::error::mutation gate: THE PINNED MUTATION RUNNER DID NOT COMPLETE (exit %d) while mutating %s.\n", code, pkg)
	fmt.Fprintf(stderr, "       dependency: %s\n       pinned version: %s\n       command that failed: %s %s\n       next: STOPPING. No mutation score, no coverage figure and no other number is being reported in its place.\n",
		mutation.RunnerModule, version, bin, strings.Join(args, " "))
	return exitRunner
}

// mergeReport folds one package's report into the run's.
//
// The counts are re-derived from the per-mutant list rather than taken from the summary
// the runner wrote, because in diff mode the mutants that count are only those ON THE
// CHANGED LINES - the rest are in the file the change touched but are not what it did.
// File names come back relative to the package the runner was pointed at, so they are
// qualified here: a report naming three different "config.go" would name nothing.
func mergeReport(into *runnerReport, one runnerReport, pkg, mode string, changed map[string][]lineRange) {
	if into.GoModule == "" {
		into.GoModule = one.GoModule
	}
	into.ElapsedTime += one.ElapsedTime
	for _, f := range one.Files {
		name := f.FileName
		if !strings.HasPrefix(name, pkg+"/") {
			name = pkg + "/" + name
		}
		kept := f
		kept.FileName = name
		kept.Mutations = kept.Mutations[:0]
		for _, m := range f.Mutations {
			if mode == mutation.ModeDiff && !inRanges(m.Line, changed[name]) {
				continue
			}
			kept.Mutations = append(kept.Mutations, m)
			switch normalizeStatus(m.Status) {
			case "KILLED":
				into.MutantsKilled++
			case "LIVED":
				into.MutantsLived++
			case "NOT_COVERED":
				into.MutantsNotCovered++
			case "NOT_VIABLE":
				into.MutantsNotViable++
			case "TIMED_OUT":
				into.MutantsTimedOut++
			}
		}
		if len(kept.Mutations) > 0 {
			into.Files = append(into.Files, kept)
		}
	}
}

func normalizeStatus(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", "_"))
}

// otherFiles lists the Go files of a package that the change did NOT touch, so the runner
// can be told to leave them alone.
func otherFiles(root, pkg string, scoped []string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, pkg))
	if err != nil {
		return nil, fmt.Errorf("could not read the package %s: %w", pkg, err)
	}
	inScope := map[string]bool{}
	for _, f := range scoped {
		inScope[f] = true
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if inScope[pkg+"/"+name] {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// Command mutation-shape is the hermetic half of the mutation gate, and the only half
// that rides `make check`.
//
// It answers two questions, neither of which needs a runner, a network or a merge base:
//
//  1. Do the committed configuration and the committed document still AGREE? The floor
//     and the exclusion list exist in .gremlins.yaml, which the gate reads, and in
//     docs/mutation-testing.md, which a human reads. A value restated in two files and
//     kept in step by nothing is this repository's recurring failure, and here it would
//     mean the document tells a reader a floor the gate is not holding, or hides an
//     exclusion the gate is honouring. An excluded path is an unmeasured path; a
//     SILENTLY excluded path is worse.
//
//  2. Does the workflow still PLAN the two runs the way it says it does? The scheduled
//     run has to be unscoped, has to take its floor from the same committed file the
//     pull-request run takes it from, and has to publish its report as an artifact of the
//     run. Those are decided by EXECUTING the workflow's own planning shell with an event
//     name in hand and reading what it emits - never by matching the text of the file,
//     because a text matcher stays green when somebody flips the logic underneath it.
//
// Exit is 0 or 1: `make check` needs a verdict, not a taxonomy.
package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/scripts/mutation"
	yaml "go.yaml.in/yaml/v3"
)

const workflowPath = ".github/workflows/mutation.yml"

// The anchors docs/mutation-testing.md carries so this gate can find the two values it
// has to compare. They are HTML comments: invisible to a reader, and stable across any
// rewording of the prose around them.
const (
	floorAnchor      = "<!-- mutation-floor -->"
	exclusionsAnchor = "<!-- mutation-exclusions -->"
)

var fail = false

func bad(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "::error::mutation shape: "+format+"\n", args...)
	fail = true
}

func note(format string, args ...any) {
	fmt.Printf("  "+format+"\n", args...)
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::mutation shape: %v\n", err)
		os.Exit(1)
	}

	cfg, err := mutation.ReadConfig(root)
	if err != nil {
		// A check that could not read its own subject has NOT passed.
		fmt.Fprintf(os.Stderr, "::error::mutation shape: %v\n", err)
		os.Exit(1)
	}

	checkAgreement(root, cfg)
	wf := checkWorkflow(root)
	if wf != nil {
		checkPlanning(root, wf)
	}
	checkInvocations(root, cfg, wf)

	if fail {
		fmt.Fprintln(os.Stderr, "::error::mutation shape: the mutation gate's declarations do not agree (named above). Refusing to report that they do.")
		os.Exit(1)
	}
	fmt.Println("mutation shape: configuration, document and workflow agree")
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	d := wd
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", fmt.Errorf("no go.mod above %s - cannot locate the repository root", wd)
		}
		d = parent
	}
}

// --- 1. the configuration and the document must agree (AC-8) ------------------------

func checkAgreement(root string, cfg mutation.Config) {
	docPath := filepath.Join(root, mutation.DocName)
	b, err := os.ReadFile(docPath)
	if err != nil {
		bad("MISSING: %s - the floor and the domain have to be readable by a human, not only by the runner. %v", mutation.DocName, err)
		return
	}
	doc := string(b)

	docFloor, err := floorFromDoc(doc)
	if err != nil {
		bad("%s: %v", mutation.DocName, err)
	} else if math.Abs(docFloor-cfg.Floor()) > 1e-9 {
		bad(`THE FLOOR DISAGREES between the configuration the gate reads and the document a human reads.
       %s (unleash.threshold.efficacy): %g
       %s (under %s): %g
       One of the two is telling somebody a number that is not being held.`,
			mutation.ConfigName, cfg.Floor(), mutation.DocName, floorAnchor, docFloor)
	} else {
		note("ok: the floor is %g%% in both %s and %s", cfg.Floor(), mutation.ConfigName, mutation.DocName)
	}

	docExcl, err := exclusionsFromDoc(doc)
	if err != nil {
		bad("%s: %v", mutation.DocName, err)
		return
	}
	cfgExcl := append([]string(nil), cfg.ExcludeFiles()...)
	sort.Strings(cfgExcl)
	sort.Strings(docExcl)
	missingFromDoc := difference(cfgExcl, docExcl)
	missingFromConfig := difference(docExcl, cfgExcl)
	switch {
	case len(missingFromDoc) > 0 && len(missingFromConfig) > 0:
		bad(`THE EXCLUSION LIST DISAGREES between the two files.
       %s (unleash.exclude-files): %s
       %s (under %s): %s`,
			mutation.ConfigName, strings.Join(cfgExcl, " "), mutation.DocName, exclusionsAnchor, strings.Join(docExcl, " "))
	case len(missingFromDoc) > 0:
		bad(`AN EXCLUDED PATH IS NOT IN THE DOCUMENT. An excluded path is an unmeasured path, and one nobody wrote down is unmeasured in secret.
       %s (unleash.exclude-files): %s
       %s (under %s): %s
       not written down: %s`,
			mutation.ConfigName, strings.Join(cfgExcl, " "), mutation.DocName, exclusionsAnchor, strings.Join(docExcl, " "), strings.Join(missingFromDoc, " "))
	case len(missingFromConfig) > 0:
		bad(`THE DOCUMENT NAMES AN EXCLUSION THE RUNNER DOES NOT HAVE, so a reader is told a package is out of the domain while the gate mutates it.
       %s (unleash.exclude-files): %s
       %s (under %s): %s
       claimed but not configured: %s`,
			mutation.ConfigName, strings.Join(cfgExcl, " "), mutation.DocName, exclusionsAnchor, strings.Join(docExcl, " "), strings.Join(missingFromConfig, " "))
	default:
		note("ok: all %d exclusion(s) appear in both %s and %s", len(cfgExcl), mutation.ConfigName, mutation.DocName)
	}
}

var floorRe = regexp.MustCompile(`\*\*([0-9]+(?:\.[0-9]+)?)%\*\*`)

func floorFromDoc(doc string) (float64, error) {
	idx := strings.Index(doc, floorAnchor)
	if idx < 0 {
		return 0, fmt.Errorf("carries no %s anchor, so this gate cannot find the floor a reader is being told. Put the anchor on its own line above the sentence that states the figure", floorAnchor)
	}
	m := floorRe.FindStringSubmatch(doc[idx:])
	if m == nil {
		return 0, fmt.Errorf("carries the %s anchor but no **NN%%** figure after it", floorAnchor)
	}
	return strconv.ParseFloat(m[1], 64)
}

func exclusionsFromDoc(doc string) ([]string, error) {
	idx := strings.Index(doc, exclusionsAnchor)
	if idx < 0 {
		return nil, fmt.Errorf("carries no %s anchor, so this gate cannot find the exclusion list a reader is being shown", exclusionsAnchor)
	}
	var out []string
	started := false
	for _, line := range strings.Split(doc[idx+len(exclusionsAnchor):], "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "|") {
			if started {
				break
			}
			continue
		}
		started = true
		cells := strings.Split(strings.Trim(t, "|"), "|")
		if len(cells) == 0 {
			continue
		}
		first := strings.TrimSpace(cells[0])
		if first == "" || strings.Trim(first, "-: ") == "" {
			continue // the header separator row
		}
		first = strings.Trim(first, "`")
		if first == "excluded path" {
			continue // the header row
		}
		out = append(out, first)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("carries the %s anchor but no table rows under it", exclusionsAnchor)
	}
	return out, nil
}

func difference(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	return out
}

// --- 2. the workflow ----------------------------------------------------------------

type step struct {
	Name            string         `yaml:"name"`
	ID              string         `yaml:"id"`
	Uses            string         `yaml:"uses"`
	If              string         `yaml:"if"`
	Run             string         `yaml:"run"`
	With            map[string]any `yaml:"with"`
	ContinueOnError any            `yaml:"continue-on-error"`
}

type job struct {
	Permissions map[string]string `yaml:"permissions"`
	Steps       []step            `yaml:"steps"`
}

type workflow struct {
	On struct {
		Schedule []struct {
			Cron string `yaml:"cron"`
		} `yaml:"schedule"`
	} `yaml:"on"`
	Jobs map[string]job `yaml:"jobs"`
}

func (w *workflow) step(pred func(step) bool) (step, bool) {
	for _, j := range w.Jobs {
		for _, s := range j.Steps {
			if pred(s) {
				return s, true
			}
		}
	}
	return step{}, false
}

func checkWorkflow(root string) *workflow {
	b, err := os.ReadFile(filepath.Join(root, workflowPath))
	if err != nil {
		bad("MISSING: %s - the mutation runs are wired there and this gate reads it. %v", workflowPath, err)
		return nil
	}
	var wf workflow
	if err := yaml.Unmarshal(b, &wf); err != nil {
		bad("%s does not parse as YAML: %v", workflowPath, err)
		return nil
	}

	// A DECLARED SCHEDULE. Without it the unscoped run happens when somebody remembers.
	if len(wf.On.Schedule) == 0 || strings.TrimSpace(wf.On.Schedule[0].Cron) == "" {
		bad("%s declares no schedule. The unscoped run is the one the floor is a statement about, and it only happens if something starts it.", workflowPath)
	} else {
		note("ok: the unscoped run is on a declared schedule (cron '%s'), which GitHub runs against the default branch", wf.On.Schedule[0].Cron)
	}

	// The job has to be able to file the tracking issue. This repository's default
	// workflow permission is read, so a job that does not ask cannot open an issue, and
	// the red scheduled run would go nowhere.
	issues := ""
	for _, j := range wf.Jobs {
		if v, ok := j.Permissions["issues"]; ok {
			issues = v
		}
	}
	if issues != "write" {
		bad("the mutation job does not request `issues: write` in %s (found %q). This repository's default workflow permission is read, so a red scheduled run could not open or update its tracking issue.", workflowPath, issues)
	} else {
		note("ok: the job requests issues: write, which is what lets a red scheduled run file its one tracking issue")
	}

	// The report is published EVEN WHEN THE RUN IS RED: the mutants that survived are
	// most worth reading on the run that failed.
	upload, ok := wf.step(func(s step) bool { return strings.HasPrefix(s.Uses, "actions/upload-artifact@") })
	if !ok {
		bad("%s publishes no artifact. The unscoped run's machine-readable report has to be retrievable from the run, or the only record of what survived is a log that ages out.", workflowPath)
	} else {
		if !strings.Contains(strings.ReplaceAll(upload.If, " ", ""), "always()") {
			bad("the artifact upload in %s is conditional on %q rather than always(). A report published only when the gate passed is the report nobody needs.", workflowPath, upload.If)
		}
		if p, _ := upload.With["path"].(string); strings.TrimSpace(p) == "" {
			bad("the artifact upload in %s names no path.", workflowPath)
		} else {
			note("ok: the report at %s is published as an artifact of every run, red or green", strings.TrimSpace(p))
		}
	}

	// The notification fires on a FAILED run and nothing about it makes the run green.
	notify, ok := wf.step(func(s step) bool { return strings.Contains(s.Run, "mutation-gate notify") })
	switch {
	case !ok:
		bad("%s has no step that files the tracking issue (`mutation-gate notify`). A red scheduled run with nobody watching is the case this whole route exists for.", workflowPath)
	case !strings.Contains(notify.If, "failure()"):
		bad("the notification step in %s is guarded by %q rather than failure(). It has to fire when the run went red, and only then.", workflowPath, notify.If)

	// A failure "FOR ANY REASON" includes the reasons that arrive before the job has run
	// anything of its own: the checkout, the toolchain, the ffmpeg install. A step output is
	// EMPTY for a run that never reached the step that sets it, so a guard reading one is a
	// guard that silently skips exactly the failures nobody else is watching. The event name
	// exists before the first step starts.
	case strings.Contains(notify.If, "steps."):
		bad(`the notification step in %s is guarded by a STEP OUTPUT (%q).
       That output is empty for a run that failed before the step which sets it - a checkout,
       a toolchain or an install failure on the schedule - so the one run nobody is watching
       would file no tracking issue at all. Guard on github.event_name, which exists before
       any step of the job has run.`, workflowPath, notify.If)
	case !strings.Contains(notify.If, "github.event_name"):
		bad(`the notification step in %s is guarded by %q, which does not read github.event_name.
       The pull-request run must not file an issue (its red run is already in front of a
       human) and every other run must, whatever stopped it. That distinction has to be made
       from something the job has before its first step.`, workflowPath, notify.If)
	default:
		note("ok: a failed run files the tracking issue, guarded on failure() and the event name - never on an output a dead run never wrote")
	}

	gate, ok := wf.step(func(s step) bool { return s.ID == "gate" })
	if !ok {
		bad("%s has no step with id: gate - this check executes that step's own shell to decide what it would invoke.", workflowPath)
	} else if truthy(gate.ContinueOnError) {
		bad("the mutation gate step in %s sets continue-on-error, so a run below the floor would report success. The run must stay RED.", workflowPath)
	}

	return &wf
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	default:
		return false
	}
}

// --- 3. the workflow's own planning shell, EXECUTED ----------------------------------

// checkPlanning runs the workflow's planning step, and then the gate step, exactly as
// written in the file, with an event name in hand - and reads what they would do.
//
// This is the part a text matcher cannot do. The planning step decides the mode and the
// scope; the gate step turns that into a `make` invocation. Both are executed here: the
// first against a temporary GITHUB_OUTPUT, the second with a `make` on PATH that records
// its arguments instead of running anything. Flipping either one's logic changes what
// this check reads.
func checkPlanning(root string, wf *workflow) {
	plan, ok := wf.step(func(s step) bool { return s.ID == "plan" })
	if !ok || strings.TrimSpace(plan.Run) == "" {
		bad("%s has no step with id: plan carrying a run block - this check executes that block to decide what each event would run.", workflowPath)
		return
	}
	gate, ok := wf.step(func(s step) bool { return s.ID == "gate" })
	if !ok || strings.TrimSpace(gate.Run) == "" {
		return // already reported
	}

	for _, tc := range []struct {
		event    string
		baseRef  string
		wantMode string
		wantRef  string
		wantArgv string
		why      string
	}{
		{
			event: "schedule", wantMode: mutation.ModeFull, wantRef: "",
			wantArgv: "mutation-full",
			why:      "the scheduled run mutates the WHOLE domain: no diff scope",
		},
		{
			event: "workflow_dispatch", wantMode: mutation.ModeFull, wantRef: "",
			wantArgv: "mutation-full",
			why:      "a run started by hand is the unscoped one too",
		},
		{
			event: "pull_request", baseRef: "main", wantMode: mutation.ModeDiff, wantRef: "origin/main",
			wantArgv: "mutation-diff REF=origin/main",
			why:      "a pull request mutates only what it changed, measured against its base branch",
		},
	} {
		outputs, err := runPlan(root, plan.Run, tc.event, tc.baseRef)
		if err != nil {
			bad("the planning step in %s could not be executed for event %q: %v", workflowPath, tc.event, err)
			continue
		}
		if outputs["mode"] != tc.wantMode || outputs["ref"] != tc.wantRef {
			bad(`the planning step plans the WRONG RUN for event %q.
       got:  mode=%q ref=%q
       want: mode=%q ref=%q
       %s`,
				tc.event, outputs["mode"], outputs["ref"], tc.wantMode, tc.wantRef, tc.why)
			continue
		}
		argv, err := runGateStep(root, gate.Run, outputs["mode"], outputs["ref"])
		if err != nil {
			bad("the gate step in %s could not be executed for event %q: %v", workflowPath, tc.event, err)
			continue
		}
		if argv != tc.wantArgv {
			bad(`the gate step would invoke the WRONG TARGET for event %q.
       got:  make %s
       want: make %s
       %s`, tc.event, argv, tc.wantArgv, tc.why)
			continue
		}
		note("ok: event %q plans mode=%s and invokes `make %s`", tc.event, outputs["mode"], argv)
	}
}

func runPlan(root, script, event, baseRef string) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "mutation-shape-plan")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	outFile := filepath.Join(dir, "github_output")
	if err := os.WriteFile(outFile, nil, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"EVENT_NAME="+event,
		"BASE_REF="+baseRef,
		"GITHUB_OUTPUT="+outFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		return nil, err
	}
	outputs := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			outputs[k] = v
		}
	}
	return outputs, nil
}

// runGateStep executes the gate step's shell with a `make` that records its arguments and
// runs nothing, and returns what it was asked to run.
func runGateStep(root, script, mode, ref string) (string, error) {
	dir, err := os.MkdirTemp("", "mutation-shape-gate")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	argvFile := filepath.Join(dir, "argv")
	stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > " + argvFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "make"), []byte(stub), 0o755); err != nil {
		return "", err
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"MODE="+mode,
		"REF="+ref,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(argvFile)
	if err != nil {
		return "", fmt.Errorf("the gate step ran without invoking make: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// --- 4. what the Makefile would actually invoke --------------------------------------

// checkInvocations asks MAKE what each target would run, rather than reading the
// Makefile's text, and holds the answer to three things: both runs go through the one
// invocation, neither carries the floor on its command line - so both take it from the
// committed configuration - and the report they write is the file the workflow uploads.
func checkInvocations(root string, cfg mutation.Config, wf *workflow) {
	floorTokens := []string{
		strconv.FormatFloat(cfg.Floor(), 'f', -1, 64),
		fmt.Sprintf("%.1f", cfg.Floor()),
		fmt.Sprintf("%.2f", cfg.Floor()),
	}

	full, errFull := makeDryRun(root, "mutation-full")
	diff, errDiff := makeDryRun(root, "mutation-diff", "REF=origin/main")
	if errFull != nil || errDiff != nil {
		bad("could not ask make what the mutation targets would run (%v / %v). A check that could not run has not passed.", errFull, errDiff)
		return
	}

	for name, line := range map[string]string{"mutation-full": full, "mutation-diff": diff} {
		if !strings.Contains(line, "scripts/mutation.sh") {
			bad("`make %s` does not go through scripts/mutation.sh, which is where the exit codes and the configuration reading live:\n       %s", name, line)
		}
		for _, tok := range floorTokens {
			if hasStandaloneToken(line, tok) {
				bad(`the floor appears on the command line of `+"`make %s`"+`:
       %s
       Both runs take the floor from %s. A threshold passed as an argument is a threshold that can differ between the pull request, the schedule and the person reproducing it.`, name, line, mutation.ConfigName)
			}
		}
		if strings.Contains(line, "--threshold") || strings.Contains(line, "--floor") {
			bad("`make %s` passes a threshold flag:\n       %s\n       The floor comes from %s and nowhere else.", name, line, mutation.ConfigName)
		}
	}

	// The unscoped run carries NO diff scope: that is what makes it a statement about the
	// whole domain rather than about one change.
	if strings.Contains(full, "--ref") || strings.Contains(full, "--diff") || !strings.Contains(full, "--mode full") {
		bad("`make mutation-full` is not an unscoped run:\n       %s", full)
	} else {
		note("ok: `make mutation-full` mutates the whole domain, with no diff scope")
	}
	if !strings.Contains(diff, "--mode diff") || !strings.Contains(diff, "--ref origin/main") {
		bad("`make mutation-diff REF=origin/main` is not scoped to that reference:\n       %s", diff)
	} else {
		note("ok: `make mutation-diff REF=...` scopes the run to what changed against that reference")
	}
	note("ok: neither run carries the floor on its command line; both read %s", mutation.ConfigName)

	// The report the runs write is the file the workflow publishes.
	if wf == nil {
		return
	}
	upload, ok := wf.step(func(s step) bool { return strings.HasPrefix(s.Uses, "actions/upload-artifact@") })
	if !ok {
		return // already reported
	}
	artifact, _ := upload.With["path"].(string)
	artifact = strings.TrimSpace(artifact)
	written := outValue(full)
	if written == "" {
		bad("`make mutation-full` names no --out report:\n       %s", full)
		return
	}
	if artifact != written {
		bad(`THE PUBLISHED ARTIFACT IS NOT THE REPORT THE RUN WRITES.
       %s writes: %s
       %s uploads: %s
       The artifact would be empty, or somebody else's file.`, "make mutation-full", written, workflowPath, artifact)
		return
	}
	if outValue(diff) != written {
		bad("the two runs write different reports (%s and %s), so only one of them can be the artifact.", written, outValue(diff))
		return
	}
	note("ok: both runs write %s, which is the path the workflow uploads", written)
}

var outRe = regexp.MustCompile(`--out\s+(\S+)`)

func outValue(line string) string {
	m := outRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// hasStandaloneToken reports whether tok appears in line as its own word, so that a floor
// of 70 is not found inside a version, a path or a port.
func hasStandaloneToken(line, tok string) bool {
	re, err := regexp.Compile(`(^|[\s=])` + regexp.QuoteMeta(tok) + `($|[\s%])`)
	if err != nil {
		return false
	}
	return re.MatchString(line)
}

func makeDryRun(root, target string, extra ...string) (string, error) {
	args := append([]string{"-n", target}, extra...)
	cmd := exec.Command("make", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("make %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "make[") {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, " "), nil
}

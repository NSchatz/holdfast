// Command release-shape-gate decides whether .github/workflows/release.yml still has the
// shape the first release depends on. Part of `make check`.
//
// Every property that decides whether the release path opens the one-way door correctly
// used to be asserted only by COMMENTS inside the workflow: that a manual dispatch
// publishes nothing, that the version tag is pushed before `:latest` moves, that `:latest`
// is promoted only onto a digest that was pulled back and re-smoked. That door is now open
// (docs/release.md carries the record), which raises the stakes rather than lowering them:
// every later release moves a `:latest` real users pull. This repository has twice learned
// that prose cannot enforce an invariant
// (scripts/check-pins.sh, scripts/install-ffmpeg.sh), and both times the answer was a
// committed gate plus a self-test that proves the gate still bites. This is that answer
// for the release path.
//
// It does NOT match text in release.yml. It RUNS the workflow's own planning shell - once
// per event shape - and decides each step's guard from the values that run produced, with
// a real GitHub-expression evaluator. The difference is not academic: flip the planning
// script so a manual dispatch sets publish=true and every `if:` in the file is unchanged,
// so a text matcher stays green while a dispatch would push an image. That mutation is
// case 3 of scripts/release-shape-selftest.sh.
//
// What it asserts, and the acceptance criteria each answers:
//
//	A6      a manual dispatch runs no step that publishes anything
//	A7/A3   on a tag push the floating reference moves last, after the full gate, both
//	        smoke runs, the version-tag push and the re-smoke of the pulled artefact -
//	        and no published act runs at all once something has failed
//	A14     no step before the promotion tolerates its own failure
//	A4/A10  the planning logic itself refuses a tag whose major version is not zero,
//	        naming the record that must first declare the surface stable
//	A8/A16  every published act the definition names is named by the operator runbook
//	A9/A17  the example deployment's image reference IS the reference this repository's
//	        own release would promote
//	A15     an unreadable, unparseable or step-less definition is red, and says which
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const (
	releaseWorkflow = ".github/workflows/release.yml"
	composeFile     = "docker-compose.yml"
	runbookFile     = "docs/release.md"
	goModFile       = "go.mod"

	// The tag the gate plans a real release with. Any 0.y.z would do; this one is a
	// version shape, not a claim about which version comes next.
	sampleTag = "v0.1.0"
	// A tag whose major is not zero. The planning logic must refuse it (A4, A10).
	sampleMajorTag = "v1.0.0"
	// A pre-release must publish without becoming the floating reference.
	samplePreTag = "v0.1.0-rc1"

	fakeSHA = "0123456789abcdef0123456789abcdef01234567"
)

func main() {
	root := flag.String("root", ".", "repository root whose release definition is checked")
	printComposeRef := flag.Bool("print-compose-ref", false,
		"print the one image reference the example deployment names, and exit; a non-zero exit means it could not be read")
	flag.Parse()

	// The example deployment's image reference has exactly ONE reader, and this is how the
	// release-time check (scripts/resolve-compose-image.sh, which needs the same value with
	// a registry in front of it) gets at it. A second reader written in sed would agree with
	// this one on today's file and disagree on a quoted scalar, a folded one, a second
	// service, or an `image:` key nested outside `services:` - the "one value, two readers
	// held in step by hope" shape this repository refuses for the ffmpeg pin.
	if *printComposeRef {
		ref, err := composeImageRef(filepath.Join(*root, composeFile))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Println(ref)
		return
	}

	g := &gate{root: *root, out: os.Stdout}
	if err := g.run(); err != nil {
		g.bad("%v", err)
	}
	if g.failed {
		fmt.Fprintln(os.Stderr, "::error::the release definition is NOT in the shape the first release depends on (see above)")
		os.Exit(1)
	}
	fmt.Fprintln(g.out, "release shape ok")
}

type gate struct {
	root     string
	out      io.Writer
	failed   bool
	reported map[string]bool
}

func (g *gate) path(rel string) string { return filepath.Join(g.root, rel) }

func (g *gate) note(format string, a ...any) {
	fmt.Fprintf(g.out, "  ok: %s\n", fmt.Sprintf(format, a...))
}

// bad records a failure and keeps going: every independent property should report itself
// on one run, so a fix is one edit rather than one edit per re-run. A message identical to
// one already printed is counted and suppressed - several properties ask the same step the
// same question, and one undecidable step should read as one problem.
func (g *gate) bad(format string, a ...any) {
	g.failed = true
	msg := strings.TrimRight(fmt.Sprintf(format, a...), "\n")
	if g.reported == nil {
		g.reported = map[string]bool{}
	}
	if g.reported[msg] {
		return
	}
	g.reported[msg] = true
	lines := strings.Split(msg, "\n")
	fmt.Fprintf(os.Stderr, "::error::%s\n", lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(os.Stderr, "       %s\n", l)
	}
}

// --- the run --------------------------------------------------------------------------

type shape struct {
	label   string
	event   string
	refName string
}

// planned is one event shape, planned for real.
type planned struct {
	shape    shape
	ctx      evalCtx
	failed   bool
	failStep Step
	exitCode int
	output   string
}

func (g *gate) run() error {
	wf, err := LoadWorkflow(g.path(releaseWorkflow))
	if err != nil {
		return err // A15: read / parse / empty / no step, each naming itself
	}
	g.note("%s parses, and names %d job(s)", releaseWorkflow, len(wf.Jobs))

	allActs, err := actsIn(wf)
	if err != nil {
		return err // an input that decides a publish and cannot be decided
	}
	if len(allActs) == 0 {
		return fmt.Errorf("%s names NO step that performs a published act. Every assertion below would then pass over nothing, which is not a gate. If the release genuinely no longer publishes, delete this gate deliberately rather than letting it report green", releaseWorkflow)
	}
	jobID, job, err := releaseJobOf(wf, allActs)
	if err != nil {
		return err
	}
	g.note("the published acts all live in job %q (%d step(s))", jobID, len(job.Steps))

	repo, err := repoFromModule(g.path(goModFile))
	if err != nil {
		return err
	}
	g.note("this module's own repository is %s (derived from %s, not restated)", repo, goModFile)

	runner, err := NewRunner()
	if err != nil {
		return err
	}
	defer runner.Close()

	// Every shape below is PLANNED by executing the workflow's planning steps. Nothing
	// downstream re-reads the script.
	dispatch, err := g.plan(runner, wf, job, repo, shape{"a manual dispatch", "workflow_dispatch", "main"})
	if err != nil {
		return err
	}
	if dispatch.failed {
		return fmt.Errorf("the planning logic FAILED on %s (exit %d) - a dry run must be able to plan itself:\n%s", dispatch.shape.label, dispatch.exitCode, indent(dispatch.output))
	}
	tag, err := g.plan(runner, wf, job, repo, shape{"a version tag push (" + sampleTag + ")", "push", sampleTag})
	if err != nil {
		return err
	}
	if tag.failed {
		return fmt.Errorf("the planning logic FAILED on %s (exit %d):\n%s", tag.shape.label, tag.exitCode, indent(tag.output))
	}
	g.note("the planning logic RAN for both event shapes; dispatch produced %s, %s produced %s",
		outputsOf(dispatch), sampleTag, outputsOf(tag))

	g.checkPlanningPrecedesActs(jobID, job, allActs)
	g.checkDispatchPublishesNothing(job, dispatch)
	promoteIdx := g.checkPublishOrdering(job, tag)
	g.checkNothingPublishesAfterAFailure(job, tag)
	g.checkPreReleaseDoesNotPromote(runner, wf, job, repo)
	g.checkMajorVersionZero(runner, wf, job, repo, allActs)
	g.checkRunbookNamesEveryAct(allActs)
	g.checkComposeReferenceAgreement(runner, wf, job, tag, promoteIdx)
	return nil
}

// --- planning -------------------------------------------------------------------------

// plan executes, in order, every planning step of the job: a `run:` step that appends to
// $GITHUB_OUTPUT. Their outputs become the `steps` context every guard is decided against.
func (g *gate) plan(r *Runner, wf *Workflow, job Job, repo string, sh shape) (*planned, error) {
	ctx := evalCtx{success: true, vars: map[string]any{
		"github": map[string]any{
			"event_name": sh.event,
			"ref_name":   sh.refName,
			"ref":        refFor(sh),
			"repository": repo,
			"repository_owner": func() string {
				owner, _, _ := strings.Cut(repo, "/")
				return owner
			}(),
			"sha":   fakeSHA,
			"actor": "release-shape-gate",
		},
		"env":   mergeEnv(wf.Env, job.Env),
		"steps": map[string]any{},
	}}
	if ok, err := ConditionRuns(job.If, ctx); err != nil {
		return nil, fmt.Errorf("the release job's own `if:` cannot be decided: %w", err)
	} else if !ok {
		return nil, fmt.Errorf("the release job would NOT RUN for %s; this gate cannot judge a release that never starts", sh.label)
	}

	res := &planned{shape: sh, ctx: ctx}
	ran := 0
	for _, s := range job.Steps {
		if !isPlanningStep(s) {
			continue
		}
		if s.ID == "" {
			return nil, fmt.Errorf("%s writes to $GITHUB_OUTPUT but carries no `id:`, so nothing can read what it decided", s.Label())
		}
		runs, err := ConditionRuns(s.If, res.ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Label(), err)
		}
		if !runs {
			continue
		}
		env, err := stepEnv(wf, job, s, res.ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Label(), err)
		}
		script, err := Interpolate(s.Run, res.ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Label(), err)
		}
		out, err := r.Run(script, env)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Label(), err)
		}
		ran++
		if out.ExitCode != 0 {
			res.failed, res.failStep, res.exitCode, res.output = true, s, out.ExitCode, out.Output
			return res, nil
		}
		outs := map[string]any{}
		for k, v := range out.Outputs {
			outs[k] = v
		}
		res.ctx.vars["steps"].(map[string]any)[s.ID] = map[string]any{
			"outputs":    outs,
			"conclusion": "success",
			"outcome":    "success",
		}
		res.output += out.Output
	}
	if ran == 0 {
		return nil, fmt.Errorf("%s has NO planning step: no `run:` step writes to $GITHUB_OUTPUT, so there is no runtime value to decide any guard from and this gate would be reduced to reading text", releaseWorkflow)
	}
	return res, nil
}

func isPlanningStep(s Step) bool {
	return s.Run != "" && strings.Contains(s.Run, "GITHUB_OUTPUT")
}

func refFor(sh shape) string {
	if sh.event == "push" {
		return "refs/tags/" + sh.refName
	}
	return "refs/heads/" + sh.refName
}

func outputsOf(p *planned) string {
	steps, _ := p.ctx.vars["steps"].(map[string]any)
	var parts []string
	for _, id := range sortedKeys(steps) {
		outs, _ := steps[id].(map[string]any)["outputs"].(map[string]any)
		for _, k := range sortedKeys(outs) {
			parts = append(parts, fmt.Sprintf("%s=%v", k, outs[k]))
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- properties -----------------------------------------------------------------------

// A15's companion: a refusal that runs after the first publishing step is not a refusal.
func (g *gate) checkPlanningPrecedesActs(jobID string, job Job, acts []Act) {
	first := -1
	for _, s := range job.Steps {
		if isPlanningStep(s) {
			first = s.Index
			break
		}
	}
	for _, a := range acts {
		if a.Step.JobID == jobID && a.Step.Index < first {
			g.bad("%s performs a published act (%s) BEFORE the planning step that decides whether this run may publish at all. Nothing the planning logic refuses could stop it.", a.Step.Label(), a.Why)
			return
		}
	}
	g.note("every published act is declared after the planning step that gates it")
}

// A6.
func (g *gate) checkDispatchPublishesNothing(job Job, p *planned) {
	running, err := runningSteps(job, p.ctx)
	if err != nil {
		g.bad("cannot decide which steps run on %s: %v", p.shape.label, err)
		return
	}
	found := 0
	for _, a := range g.actsOf(running, &p.ctx) {
		found++
		g.bad("on %s, %s WOULD RUN and it performs a published act: %s (%s).\nA dry run must publish nothing. The planning logic produced %s, and this step's guard (`%s`) is true against those values.",
			p.shape.label, a.Step.Label(), a.Why, a.Kind, outputsOf(p), strings.TrimSpace(a.Step.If))
	}
	if found == 0 {
		g.note("on %s, %d step(s) run and NONE of them publishes anything", p.shape.label, len(running))
	}
}

// A7, A3, A14. Returns the promotion step's index, or -1.
func (g *gate) checkPublishOrdering(job Job, p *planned) int {
	running, err := runningSteps(job, p.ctx)
	if err != nil {
		g.bad("cannot decide which steps run on %s: %v", p.shape.label, err)
		return -1
	}

	var (
		pushes, moves, releases []Act
		gateIdx                 = -1
		smokeArch               = map[string]int{}
		reSmokeArch             = map[string]int{}
		firstReSmoke            = -1
		lastReSmoke             = -1
	)
	for _, a := range g.actsOf(running, &p.ctx) {
		switch a.Kind {
		case ActImagePush:
			pushes = append(pushes, a)
		case ActTagMove:
			moves = append(moves, a)
		case ActRelease:
			releases = append(releases, a)
		}
	}
	for _, s := range running {
		if gateIdx < 0 && s.RunsFullGate() {
			gateIdx = s.Index
		}
		if !s.RunsSmoke() {
			continue
		}
		if s.PullsFromRegistry() {
			for _, a := range s.PullArches() {
				reSmokeArch[a] = s.Index
			}
			if firstReSmoke < 0 {
				firstReSmoke = s.Index
			}
			lastReSmoke = s.Index
			continue
		}
		for _, a := range s.SmokeArches() {
			if _, seen := smokeArch[a]; !seen {
				smokeArch[a] = s.Index
			}
		}
	}

	if len(moves) != 1 {
		g.bad("on %s, %d step(s) move a floating reference; this gate models exactly one promotion and refuses to guess which of several moves last.\n%s",
			p.shape.label, len(moves), actList(moves))
		return -1
	}
	move := moves[0]
	if len(pushes) == 0 {
		g.bad("on %s nothing pushes an image, yet %s moves a floating reference. A promotion with no gated artefact behind it points `:latest` at something this run never proved.",
			p.shape.label, move.Step.Label())
		return move.Step.Index
	}

	problems := 0
	must := func(what string, idx int, label string) {
		switch {
		case idx < 0:
			problems++
			g.bad("on %s, %s does not run before %s moves the floating reference. The promotion would point `:latest` at an image this run never %s.",
				p.shape.label, what, move.Step.Label(), label)
		case idx > move.Step.Index:
			problems++
			g.bad("on %s, %s runs AFTER %s moves the floating reference (%s is step %d, the promotion is step %d). `:latest` would be handed to every `docker compose pull` user before it was proved.",
				p.shape.label, what, move.Step.Label(), what, idx+1, move.Step.Index+1)
		}
	}
	must("the full gate (make check)", gateIdx, "gated")
	for _, arch := range []string{"linux/amd64", "linux/arm64"} {
		idx, ok := smokeArch[arch]
		if !ok {
			idx = -1
		}
		must("the "+arch+" smoke run", idx, "smoked")
		ridx, rok := reSmokeArch[arch]
		if !rok {
			ridx = -1
		}
		must("the re-smoke of the "+arch+" artefact pulled back from the registry", ridx, "pulled back and re-smoked")
	}
	for _, a := range pushes {
		must("the version-tag push ("+a.Step.Label()+")", a.Step.Index, "pushed")
		if firstReSmoke >= 0 && firstReSmoke < a.Step.Index {
			problems++
			g.bad("on %s, the artefact is re-smoked (step %d) BEFORE it is pushed (step %d). A re-smoke that runs first grades a local build, which is the gate-by-equivalence this ordering exists to reject.",
				p.shape.label, firstReSmoke+1, a.Step.Index+1)
		}
		if gateIdx >= 0 && gateIdx > a.Step.Index {
			problems++
			g.bad("on %s, the full gate (step %d) runs AFTER the image is pushed (step %d). The pushed image is immediately pullable, so the gate would be judging something already published.",
				p.shape.label, gateIdx+1, a.Step.Index+1)
		}
	}
	for _, a := range releases {
		if a.Step.Index < move.Step.Index {
			problems++
			g.bad("on %s, %s creates a published release (step %d) before the floating reference is promoted (step %d). A release announcing an image that has not been promoted advertises a reference that does not resolve.",
				p.shape.label, a.Step.Label(), a.Step.Index+1, move.Step.Index+1)
		}
	}
	if lastReSmoke > move.Step.Index {
		problems++
		g.bad("on %s, the last re-smoke of a pulled artefact (step %d) runs AFTER the promotion (step %d).",
			p.shape.label, lastReSmoke+1, move.Step.Index+1)
	}

	if problems == 0 {
		g.note("on %s the order holds: gate -> both smoke runs -> version-tag push -> re-smoke of both pulled artefacts -> %s", p.shape.label, move.Step.Label())
	}

	// A14: nothing before the promotion may be allowed to fail quietly.
	tolerated := 0
	if tolerant, why := tolerates(job.ContinueOnError); tolerant {
		tolerated++
		g.bad("the release job itself is marked `%s`, so every failure before the promotion is tolerated and `:latest` moves anyway.", why)
	}
	for _, s := range running {
		if s.Index > move.Step.Index {
			continue
		}
		if tolerant, why := s.TolerateFailure(); tolerant {
			tolerated++
			g.bad("%s runs before the floating reference moves and is marked `%s`, so its failure would NOT fail the run. `:latest` would be promoted over the top of it.", s.Label(), why)
		}
	}
	if tolerated == 0 {
		g.note("no step before the promotion tolerates its own failure")
	}
	return move.Step.Index
}

// A3, stated as its own property: once anything has failed, nothing publishes.
func (g *gate) checkNothingPublishesAfterAFailure(job Job, p *planned) {
	failing := evalCtx{vars: p.ctx.vars, success: false}
	running, err := runningSteps(job, failing)
	if err != nil {
		g.bad("cannot decide which steps run after a failure: %v", err)
		return
	}
	bad := 0
	for _, a := range g.actsOf(running, &failing) {
		bad++
		g.bad("%s still runs after an earlier step has FAILED, and it performs a published act: %s (%s). Its guard is `%s`, which does not defer to the run's success. A failed gate or smoke run must leave the floating reference exactly where it was.",
			a.Step.Label(), a.Why, a.Kind, strings.TrimSpace(a.Step.If))
	}
	if bad == 0 {
		g.note("after a failed step, NO published act runs at all - the floating reference stays where it was")
	}
}

// The pre-release invariant the workflow already claims: a suffixed tag publishes but must
// not become the floating reference.
func (g *gate) checkPreReleaseDoesNotPromote(r *Runner, wf *Workflow, job Job, repo string) {
	pre, err := g.plan(r, wf, job, repo, shape{"a pre-release tag push (" + samplePreTag + ")", "push", samplePreTag})
	if err != nil {
		g.bad("cannot plan a pre-release tag: %v", err)
		return
	}
	if pre.failed {
		g.bad("the planning logic refuses the pre-release tag %s (exit %d). A pre-release is a supported release shape:\n%s", samplePreTag, pre.exitCode, indent(pre.output))
		return
	}
	running, err := runningSteps(job, pre.ctx)
	if err != nil {
		g.bad("cannot decide which steps run for a pre-release tag: %v", err)
		return
	}
	moved := false
	for _, a := range g.actsOf(running, &pre.ctx) {
		if a.Kind == ActTagMove {
			moved = true
			g.bad("on a pre-release tag (%s), %s would move the floating reference. `docker pull` would hand a release candidate to everyone who did not ask for one.", samplePreTag, a.Step.Label())
		}
	}
	if !moved {
		g.note("a pre-release tag (%s) publishes but does NOT move the floating reference", samplePreTag)
	}
}

// A4, A10.
func (g *gate) checkMajorVersionZero(r *Runner, wf *Workflow, job Job, repo string, acts []Act) {
	major, err := g.plan(r, wf, job, repo, shape{"a tag whose major version is not zero (" + sampleMajorTag + ")", "push", sampleMajorTag})
	if err != nil {
		g.bad("cannot plan %s: %v", sampleMajorTag, err)
		return
	}
	if !major.failed {
		g.bad("the release path ACCEPTS %s. Major version zero is for initial development and promises nothing (semver.org clause 4); the first non-zero major DEFINES the public API (clause 5), and this project has not yet decided that its configuration keys, HTTP surface and metric names will not change without one. The planning logic must refuse it before anything is published.", sampleMajorTag)
		return
	}
	for _, a := range acts {
		if a.Step.Index < major.failStep.Index {
			g.bad("%s refuses %s, but %s performs a published act before it. The refusal must come first.", major.failStep.Label(), sampleMajorTag, a.Step.Label())
		}
	}
	named := reDocPath.FindAllString(major.output, -1)
	if len(named) == 0 {
		g.bad("the refusal of %s names no record. It must name the document that has to declare the configuration keys, the HTTP surface and the metric names stable before a non-zero major is cut, or the next person has nothing to go and read. It said:\n%s", sampleMajorTag, indent(major.output))
		return
	}
	missing := 0
	for _, p := range named {
		if st, err := os.Stat(g.path(p)); err != nil || st.Size() == 0 {
			missing++
			g.bad("the refusal of %s names %s, which does not exist in this repository. A refusal pointing at a document nobody can read is prose again.", sampleMajorTag, p)
		}
	}
	if missing == 0 {
		g.note("the planning logic refuses %s (exit %d) and names %s", sampleMajorTag, major.exitCode, strings.Join(unique(named), ", "))
	}
}

var reDocPath = regexp.MustCompile(`\bdocs/[A-Za-z0-9._/-]+\.md\b`)

// A8, A16.
func (g *gate) checkRunbookNamesEveryAct(acts []Act) {
	raw, err := os.ReadFile(g.path(runbookFile))
	if err != nil {
		g.bad("the operator runbook %s CANNOT BE READ (%v). %d published act(s) in %s would then be checked against nothing:\n%s",
			runbookFile, err, len(acts), releaseWorkflow, actList(acts))
		return
	}
	text := string(raw)
	declared := reActID.FindAllString(text, -1)
	if len(declared) == 0 {
		g.bad("the operator runbook %s names NO irreversible act (no `<kind>@<step>` id anywhere in it). An empty document must not satisfy this check - that is the vacuous pass the whole gate exists to refuse. It must name:\n%s",
			runbookFile, actList(acts))
		return
	}
	missing := 0
	for _, a := range acts {
		if !strings.Contains(text, "`"+a.ID()+"`") {
			missing++
			g.bad("%s performs a published act (%s) that the operator runbook does not name.\nAdd `%s` to %s, with what it does, whether it can be undone, and by what. A publishing step nobody wrote down is a publishing step nobody reviewed.",
				a.Step.Label(), a.Why, a.ID(), runbookFile)
		}
	}
	if missing == 0 {
		g.note("%s names every one of the %d published act(s) in %s: %s", runbookFile, len(acts), releaseWorkflow, strings.Join(actIDs(acts), ", "))
	}
}

var reActID = regexp.MustCompile("`[a-z-]+@[a-z0-9-]+`")

// A9, A17, plus the A11 wiring check.
func (g *gate) checkComposeReferenceAgreement(r *Runner, wf *Workflow, job Job, p *planned, promoteIdx int) {
	composeRef, err := composeImageRef(g.path(composeFile))
	if err != nil {
		g.bad("%v", err) // A17: the file is named in every one of these
		return
	}
	if promoteIdx < 0 {
		return // the ordering check already said why it could not find the promotion
	}
	var promote Step
	for _, s := range job.Steps {
		if s.Index == promoteIdx {
			promote = s
		}
	}

	// Run the promotion for real, with docker stubbed, and read the reference it moved
	// out of the argv it actually invoked. The workflow derives that reference at runtime
	// from the repository name; reading the YAML would only ever produce `${IMAGE}:latest`.
	env, err := stepEnv(wf, job, promote, p.ctx)
	if err != nil {
		g.bad("cannot build the promotion step's environment: %v", err)
		return
	}
	script, err := Interpolate(promote.Run, p.ctx)
	if err != nil {
		g.bad("cannot interpolate the promotion step: %v", err)
		return
	}
	out, err := r.Run(script, env)
	if err != nil {
		g.bad("cannot execute the promotion step: %v", err)
		return
	}
	if out.ExitCode != 0 {
		g.bad("the promotion step exits %d when run with the values the planning logic produced:\n%s", out.ExitCode, indent(out.Output))
		return
	}
	targets, sources := tagMoveRefs(out.Argv)
	if len(targets) != 1 {
		g.bad("could not read a single floating reference out of what %s actually ran (%d found). The gate refuses to guess which reference a release moves:\n%s",
			promote.Label(), len(targets), indent(formatArgv(out.Argv)))
		return
	}
	floating := targets[0]

	if floating != composeRef {
		g.bad("the example deployment names an image reference this repository's own release would NEVER produce.\n%s names:  %s\n%s promotes: %s\nA user who runs the published compose file would pull a reference nothing publishes. The release image is derived from the repository name at run time, so a repository rename moves this reference too.",
			composeFile, composeRef, promote.Label(), floating)
		return
	}
	g.note("the example deployment's image reference agrees with the one a release promotes\n      %s: %s\n      %s: %s", composeFile, composeRef, promote.Label(), floating)

	// The floating reference must be retagged onto the version this run gated, never
	// rebuilt or pointed at something else.
	steps, _ := p.ctx.vars["steps"].(map[string]any)
	image, version := "", ""
	for _, id := range sortedKeys(steps) {
		outs, _ := steps[id].(map[string]any)["outputs"].(map[string]any)
		if v, ok := outs["image"].(string); ok {
			image = v
		}
		if v, ok := outs["version"].(string); ok {
			version = v
		}
	}
	if image != "" && version != "" {
		want := image + ":" + version
		if len(sources) != 1 || sources[0] != want {
			g.bad("%s does not promote the floating reference onto the version this run gated.\nexpected source: %s\nactually ran:    %s", promote.Label(), want, strings.Join(sources, " "))
		} else {
			g.note("the promotion retags %s onto %s - the same digest, not a rebuild", floating, want)
		}
		// And no earlier publishing step may produce the floating reference itself.
		var earlier []Step
		for _, s := range job.Steps {
			if s.Index < promoteIdx {
				earlier = append(earlier, s)
			}
		}
		for _, a := range g.actsOf(earlier, &p.ctx) {
			if a.Kind != ActImagePush {
				continue
			}
			raw, ok := a.Step.With["tags"]
			if !ok {
				continue
			}
			tags, err := Interpolate(yamlString(raw), p.ctx)
			if err != nil {
				g.bad("cannot read the tags %s pushes: %v", a.Step.Label(), err)
				continue
			}
			for _, t := range strings.Fields(strings.ReplaceAll(tags, ",", " ")) {
				if t == floating {
					g.bad("%s pushes %s directly. The floating reference would then be pullable before the pushed artefact has been pulled back and re-smoked, which is exactly what promoting it separately, last, avoids.", a.Step.Label(), floating)
				}
			}
		}
	}

	// A11's wiring: something must resolve that reference against the registry after the
	// promotion, or A5 has no enforcement behind it on any release after the first.
	running, err := runningSteps(job, p.ctx)
	if err != nil {
		return
	}
	resolved := -1
	for _, s := range running {
		if s.ResolvesComposeRef() {
			resolved = s.Index
		}
	}
	switch {
	case resolved < 0:
		g.bad("no step resolves the example deployment's image reference (%s) against the registry after the promotion. Without it, a release that leaves %s pointing at a reference nobody publishes is only ever discovered by a user.", composeRef, composeFile)
	case resolved < promoteIdx:
		g.bad("the step that resolves %s runs (step %d) BEFORE the promotion (step %d), so it would resolve the previous release's digest.", composeRef, resolved+1, promoteIdx+1)
	default:
		g.note("the example deployment's reference is resolved against the registry after the promotion (step %d)", resolved+1)
	}
}

// --- helpers --------------------------------------------------------------------------

func runningSteps(job Job, ctx evalCtx) ([]Step, error) {
	var out []Step
	for _, s := range job.Steps {
		ok, err := ConditionRuns(s.If, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Label(), err)
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// actsOf reports the published acts these steps perform under ctx. A step whose publish
// decision cannot be decided reds the gate and contributes no act, which is why every
// caller must go through here rather than dropping the error.
func (g *gate) actsOf(steps []Step, ctx *evalCtx) []Act {
	var out []Act
	for _, s := range steps {
		acts, err := s.Acts(ctx)
		if err != nil {
			g.bad("%v", err)
			continue
		}
		out = append(out, acts...)
	}
	return out
}

func actsIn(wf *Workflow) ([]Act, error) {
	var out []Act
	for _, id := range wf.JobIDs() {
		for _, s := range wf.Jobs[id].Steps {
			acts, err := s.Acts(nil)
			if err != nil {
				return nil, err
			}
			out = append(out, acts...)
		}
	}
	return out, nil
}

func releaseJobOf(wf *Workflow, acts []Act) (string, Job, error) {
	jobs := map[string]bool{}
	for _, a := range acts {
		jobs[a.Step.JobID] = true
	}
	if len(jobs) != 1 {
		var names []string
		for j := range jobs {
			names = append(names, j)
		}
		sort.Strings(names)
		return "", Job{}, fmt.Errorf("published acts are spread across %d jobs (%s). This gate models the ordering of ONE job, because ordering across jobs is a `needs:` graph it does not read - it refuses rather than reporting an order it did not check", len(jobs), strings.Join(names, ", "))
	}
	for id := range jobs {
		return id, wf.Jobs[id], nil
	}
	return "", Job{}, fmt.Errorf("unreachable")
}

func mergeEnv(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = yamlString(v)
		}
	}
	return out
}

func yamlString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(t)
	}
}

// stepEnv is the environment a step's shell sees: the workflow's env, the job's, then the
// step's own, each value interpolated against the values the planning logic produced.
func stepEnv(wf *Workflow, job Job, s Step, ctx evalCtx) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range []map[string]any{wf.Env, job.Env, s.Env} {
		for k, v := range m {
			val, err := Interpolate(yamlString(v), ctx)
			if err != nil {
				return nil, fmt.Errorf("env %s: %w", k, err)
			}
			out[k] = val
		}
	}
	gh, _ := ctx.vars["github"].(map[string]any)
	out["GITHUB_SHA"] = yamlString(gh["sha"])
	out["GITHUB_REPOSITORY"] = yamlString(gh["repository"])
	out["GITHUB_REF_NAME"] = yamlString(gh["ref_name"])
	out["GITHUB_REF"] = yamlString(gh["ref"])
	out["GITHUB_EVENT_NAME"] = yamlString(gh["event_name"])
	out["GITHUB_ACTOR"] = yamlString(gh["actor"])
	return out, nil
}

// tagMoveRefs reads the references a promotion actually moved out of the argv the stubbed
// docker recorded: `docker buildx imagetools create -t <target> <source…>`.
func tagMoveRefs(argv [][]string) (targets, sources []string) {
	for _, cmd := range argv {
		if len(cmd) < 2 {
			continue
		}
		joined := strings.Join(cmd, " ")
		if !strings.Contains(joined, "imagetools") || !strings.Contains(joined, " create") {
			continue
		}
		rest := cmd
		for i := 0; i < len(rest); i++ {
			if rest[i] == "create" {
				rest = rest[i+1:]
				break
			}
		}
		for i := 0; i < len(rest); i++ {
			switch {
			case rest[i] == "-t" || rest[i] == "--tag":
				if i+1 < len(rest) {
					targets = append(targets, rest[i+1])
					i++
				}
			case strings.HasPrefix(rest[i], "--tag="):
				targets = append(targets, strings.TrimPrefix(rest[i], "--tag="))
			case strings.HasPrefix(rest[i], "-"):
				// another flag; not a reference
			default:
				sources = append(sources, rest[i])
			}
		}
	}
	return targets, sources
}

func formatArgv(argv [][]string) string {
	var lines []string
	for _, cmd := range argv {
		lines = append(lines, strings.Join(cmd, " "))
	}
	if len(lines) == 0 {
		return "(it ran no publishing command at all)"
	}
	return strings.Join(lines, "\n")
}

// composeImageRef reads the one image reference the example deployment names. Every
// failure here names the file (A17): an absent reference is not agreement.
func composeImageRef(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the example deployment %s CANNOT BE READ (%v). An image reference that is not there cannot agree with anything", filepath.Base(path), err)
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("the example deployment %s CANNOT BE PARSED (%v), so the image reference it names is unknown", filepath.Base(path), err)
	}
	var refs []string
	for _, name := range sortedServiceNames(doc.Services) {
		if img := strings.TrimSpace(doc.Services[name].Image); img != "" {
			refs = append(refs, img)
		}
	}
	switch len(refs) {
	case 1:
		return refs[0], nil
	case 0:
		return "", fmt.Errorf("the example deployment %s NAMES NO IMAGE REFERENCE. A user copying it would build rather than pull, and the reference a release publishes would be checked against nothing", filepath.Base(path))
	default:
		return "", fmt.Errorf("the example deployment %s names %d image references (%s); this gate models one service and refuses to guess which one a release is supposed to publish", filepath.Base(path), len(refs), strings.Join(refs, ", "))
	}
}

func sortedServiceNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	reModule      = regexp.MustCompile(`(?m)^module\s+(\S+)\s*$`)
	reMajorSuffix = regexp.MustCompile(`^v[0-9]+$`)
)

// repoFromModule derives the GitHub repository that owns this module from go.mod. The
// release workflow derives the image from `github.repository`, so this is the one honest
// way to know what reference a release of THIS repository produces - and it is why a
// repository rename is part of the irreversible set.
func repoFromModule(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s to learn which repository owns this module: %v", filepath.Base(path), err)
	}
	m := reModule.FindStringSubmatch(string(raw))
	if m == nil {
		return "", fmt.Errorf("%s declares no module path, so the repository a release publishes from cannot be derived", filepath.Base(path))
	}
	mod := m[1]
	if !strings.HasPrefix(mod, "github.com/") {
		return "", fmt.Errorf("the module path %q is not a github.com path; this gate derives the published image from the GitHub repository and cannot do so here", mod)
	}
	parts := strings.Split(strings.TrimPrefix(mod, "github.com/"), "/")
	if len(parts) > 2 && reMajorSuffix.MatchString(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	if len(parts) != 2 {
		return "", fmt.Errorf("the module path %q is not owner/repo, so the published image reference cannot be derived from it", mod)
	}
	return parts[0] + "/" + parts[1], nil
}

func actList(acts []Act) string {
	var lines []string
	for _, a := range acts {
		lines = append(lines, fmt.Sprintf("  `%s`  (%s, %s)", a.ID(), a.Why, a.Step.Label()))
	}
	return strings.Join(lines, "\n")
}

func actIDs(acts []Act) []string {
	var out []string
	for _, a := range acts {
		out = append(out, a.ID())
	}
	return out
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func indent(s string) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		lines = append(lines, "  | "+l)
	}
	return strings.Join(lines, "\n")
}

// Command release-shape-gate decides whether .github/workflows/release.yml still has the
// shape the first release depends on. Part of `make check`.
//
// Every property that decides whether the release path opens the one-way door correctly
// used to be asserted only by COMMENTS inside the workflow: that a manual dispatch
// publishes nothing, that the version tag is pushed before `:latest` moves, that `:latest`
// is promoted only onto a digest that was pulled back and re-smoked. That door is now open
// (docs/release.md carries the record), which raises the stakes rather than lowering them:
// every later release moves a `:latest` real users pull. This repository has twice learned
// that prose cannot enforce an invariant (scripts/check-pins.sh, scripts/install-ffmpeg.sh),
// and both times the answer was a committed gate plus a self-test that proves the gate still
// bites. This is that answer for the release path.
//
// IT DOES NOT DECIDE WHAT A `run:` STEP DOES, and that is the design rather than a gap.
// Six ordinals of adversarial review found six fail-opens in one direction in a reader that
// tried (F1, F5, F7, F9, F11, F12), and an observer that RAN each step in a stubbed
// environment was beaten in one line by `export PATH=/usr/bin:/bin` and by `exec docker
// push` (F14, F15) - because every control such an environment has is an ordinary shell
// object the step it is observing owns. The question is undecidable over arbitrary shell.
//
// So the gate asks a decidable one. A step publishes nothing it holds no credential for, so
// the release definition is CONSTRAINED - every irreversible act lives in a job that runs
// only on a tag push, and that job is the only one granted a write permission - and the gate
// decides that constraint from `permissions:`, `secrets:`, `needs:` and `if:`, which are
// structured YAML with no shell in the question. A dry run's steps may then say `docker
// push` in any spelling at all and publish nothing.
//
// The one thing it still EXECUTES is the workflow's own planning logic, which every review
// has found sound and which is what makes the guards real rather than restated: flip the
// planning script so a dispatch sets publish=true and not one `if:` in the file changes, so
// a text matcher stays green while the publishing job runs. That is case 3 of
// scripts/release-shape-selftest.sh. Exactly one step is executed - the step declaring
// `id: plan` - and it runs with the publishing binaries stubbed, with HOME and PATH pointed
// at a scratch directory, and with no other step's script ever run at all.
//
// What it asserts, and the acceptance criteria each answers:
//
//	A6/A12  on a manual dispatch, every job that runs holds NO capability that could
//	        authorise a published act, and every job that does hold one does not run
//	A7/A3   on a tag push the floating reference moves last, after the full gate, both
//	        smoke runs, the version-tag push and the re-smoke of the pulled artefact -
//	        and no capability-bearing job runs at all once something has failed
//	A14     no step before the promotion tolerates its own failure
//	A4/A10  the planning logic itself refuses a tag whose major version is not zero,
//	        naming the record that must first declare the surface stable
//	A8/A16  every step in a capability-bearing job is named by the operator runbook
//	A9/A17  the example deployment's image reference IS the reference this repository's
//	        own release would promote
//	A15     an unreadable, unparseable or step-less definition is red, and says which
//
// Every value a release step HANDS its program - the reference the re-smoke pulls back, the
// version `:latest` is retagged onto, the references the push publishes, the image the
// release notes name - has to BE the planning logic's own output rather than equal one: the
// expression is traced back through the `needs:` graph and every literal reds. Comparing the
// value with what a planned release produced bought back exactly one literal, the one equal to
// that sample, and the sample was `v0.1.0` - the version this repository has actually
// published (S0046 F26). See handed.go, and acts.go for the same hold on the irreversible acts
// the role table does not name (S0046 F28).
//
// And that trace ENDS at the planning logic's output, so the last thing holding a release to
// its tag is that the output IS the tag: a plan that pins the published version to a literal
// leaves every one of those references structurally perfect and republishes the July release on
// every later tag (S0046 F27). The version is therefore held to the ref the run was triggered
// on, over tags this gate DRAWS rather than names - so there is no sample for a literal to
// coincide with and no list for a planning script to special-case. See version.go.
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

	// The step env key through which the promotion declares which floating reference it
	// moves. It is declared in release.yml, read here, and read by
	// scripts/release-promote.sh - one value, one writer, no copy to drift.
	floatingTagEnv = "FLOATING_TAG"
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
// one already printed is counted and suppressed - several properties ask the same job the
// same question, and one undecidable job should read as one problem.
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

// jobPlan is one job under one event shape.
type jobPlan struct {
	id   string
	job  Job
	runs bool
	why  string  // why it does not run, when it does not
	ctx  evalCtx // the context its steps are decided against
	outs map[string]string
}

// planned is one event shape, planned for real.
type planned struct {
	shape    shape
	jobs     map[string]*jobPlan
	order    []string
	failed   bool
	failStep Step
	exitCode int
	output   string
}

func (p *planned) running() []string {
	var out []string
	for _, id := range p.order {
		if p.jobs[id].runs {
			out = append(out, id)
		}
	}
	return out
}

func (g *gate) run() error {
	wf, err := LoadWorkflow(g.path(releaseWorkflow))
	if err != nil {
		return err // A15: read / parse / empty / no step, each naming itself
	}
	g.note("%s parses, and names %d job(s): %s", releaseWorkflow, len(wf.Jobs), strings.Join(wf.JobIDs(), ", "))

	// Which events can reach this workflow at all - graded before anything is planned for
	// them, because every shape below is one this gate INVENTS and they are only the right
	// shapes if `on:` says so. See triggers.go.
	g.checkTriggerSurface(wf)
	g.checkWorkflowKeys(wf)

	roles, err := locateRoles(wf, g.root)
	if err != nil {
		return err
	}
	g.note("every position a release has is declared by a step `id:` and invokes what that role names (%d roles)", len(releaseRoles))

	if err := g.checkOnlyThePlanStepIsExecuted(wf, roles); err != nil {
		return err
	}

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

	// Every shape below is PLANNED by executing the workflow's planning step. Nothing
	// downstream re-reads a script.
	dispatch, err := g.plan(runner, wf, roles, repo, shape{"a manual dispatch", "workflow_dispatch", "main"})
	if err != nil {
		return err
	}
	if dispatch.failed {
		return fmt.Errorf("the planning logic FAILED on %s (exit %d) - a dry run must be able to plan itself:\n%s", dispatch.shape.label, dispatch.exitCode, indent(dispatch.output))
	}
	tag, err := g.plan(runner, wf, roles, repo, shape{"a version tag push (" + sampleTag + ")", "push", sampleTag})
	if err != nil {
		return err
	}
	if tag.failed {
		return fmt.Errorf("the planning logic FAILED on %s (exit %d):\n%s", tag.shape.label, tag.exitCode, indent(tag.output))
	}
	g.note("the planning logic RAN for both event shapes; dispatch produced %s, %s produced %s",
		outputsOf(dispatch), sampleTag, outputsOf(tag))

	g.checkDispatchHoldsNoCapability(wf, dispatch)
	g.checkIrreversibleActsLiveBehindAGrant(wf, roles)
	g.checkOrder(wf, roles, tag)
	g.checkNothingToleratesAFailure(wf, roles, tag)
	g.checkNothingPublishesAfterAFailure(wf, tag)
	g.checkPreReleaseDoesNotPromote(runner, wf, roles, repo)
	g.checkMajorVersionZero(runner, wf, roles, repo)
	g.checkRunbookNamesEveryAct(wf)
	// Structured YAML and the workflow's own outputs graph; no planned run, and no sample. See
	// handed.go.
	g.checkHandedValues(wf, roles)
	// The same trace, for the irreversible acts that hold no role - the eight steps of the
	// publishing job the role table does not name. See acts.go.
	g.checkActsHandedValues(wf, roles)
	g.checkPlanProducesEverythingAReleaseStepIsHeldTo(roles, tag)
	// The other end of that trace. The three above say a release step's object IS the planning
	// logic's own output and that the output is really written; not one of them holds the
	// OUTPUT to anything, so a plan that pins the published version to a literal satisfies
	// every one of them. See version.go.
	g.checkPublishedVersionIsTheTriggerTag(runner, wf, roles, repo)
	g.checkComposeReferenceAgreement(wf, roles, tag, repo)
	return nil
}

// An image reference has three parts and this gate needs each of them separately, because
// they answer three different questions: the NAME says which repository publishes it (a
// repository rename moves it), the TAG says which release a reader is looking at, and the
// DIGEST says which bytes actually resolve. `docker-compose.yml` pins all three, and it is
// the file the questions are asked of, so the decomposition lives beside its one reader
// rather than being repeated in shell (the same rule that gave the file one reader at all).

// refDigest splits the `@sha256:…` half off. It is the half that names the artefact rather
// than a label, so it is separated FIRST: everything below reads the name half only.
func refDigest(ref string) (rest, digest string) {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// refTag splits an image reference's tag off. The LAST colon of the name half, because a
// registry may carry a port (`localhost:5000/x:tag`), a colon inside the path half is not a
// tag at all, and the digest half carries a colon of its own (`@sha256:…`).
func refTag(ref string) (string, bool) {
	name, _ := refDigest(ref)
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return "", false
	}
	tag := name[i+1:]
	if tag == "" || strings.Contains(tag, "/") {
		return "", false
	}
	return tag, true
}

// refName is the reference with its tag and digest removed: the repository half alone, which
// is what a release derives from `github.repository` and what a repository rename moves.
func refName(ref string) string {
	name, _ := refDigest(ref)
	if tag, ok := refTag(ref); ok {
		return strings.TrimSuffix(name, ":"+tag)
	}
	return name
}

// The gate executes shell, so what it executes is bounded and declared. Exactly one step -
// the one holding the `plan` role - is ever run, and it is run with the publishing binaries
// stubbed and with HOME and PATH pointed at a scratch directory. A second step writing to
// $GITHUB_OUTPUT would be a second script this gate would have to execute to know what the
// guards see, and running a step whose purpose is to publish is the mistake ordinal 6
// caught (F14's second consequence). It reds instead.
func (g *gate) checkOnlyThePlanStepIsExecuted(wf *Workflow, roles *Roles) error {
	plan := roles.step("plan")
	for _, jid := range wf.JobIDs() {
		for _, s := range wf.Jobs[jid].Steps {
			if s.Run == "" || !strings.Contains(s.Run, "GITHUB_OUTPUT") {
				continue
			}
			if s.JobID == plan.JobID && s.Index == plan.Index {
				continue
			}
			return fmt.Errorf("%s writes to $GITHUB_OUTPUT, but the only step this gate executes is the one holding the `plan` role (%s). A second planning script would have to be executed too, and executing a workflow's step scripts to find out what they do is the mechanism ordinal 6 of this spec's impl gate defeated. Fold the decision into the `plan` step, or give this one an `id:` and stop it writing outputs", s.Label(), plan.Label())
		}
	}
	g.note("exactly ONE step is ever executed by this gate: %s. No other step's script runs, here or in the self-test", plan.Label())
	return nil
}

// checkWorkflowKeys is deny-by-default at the level that did not have it. capability.go
// classifies every JOB key and every STEP key and reds on one nobody has, and this gate's
// whole standard is that silence must be unreachable - but the WORKFLOW's own keys were
// handled one at a time (`permissions`, `defaults`, `on`, `env`, `jobs`) and anything else
// was never looked at. GitHub's top-level vocabulary is small and closed today and none of
// the unhandled members can hand a job a credential, so this is a boundary rather than a
// live hole; it is closed because two of the three levels said so when they met something
// new and the third did not, and that asymmetry is invisible from the output.
func (g *gate) checkWorkflowKeys(wf *Workflow) {
	unclassified := 0
	for _, k := range mappingKeys(wf.Node) {
		if _, ok := workflowKeysCheckedAndCapabilityFree[k]; ok {
			continue
		}
		unclassified++
		g.bad("%s declares the top-level key `%s:`, which this gate has not classified.\nAn unclassified key reads CLOSED at every level this gate reads - the job's, the step's and here - because a key nobody looked at contributing silence is the failure mode the whole design exists to delete. Decide what `%s:` can hand a job and classify it in workflowKeysCheckedAndCapabilityFree with the reason, or refuse it.\nclassified: %s",
			releaseWorkflow, k, k, strings.Join(sortedStringsOf(workflowKeysCheckedAndCapabilityFree), ", "))
	}
	if unclassified == 0 {
		g.note("every top-level key in %s is classified (%s), so a key GitHub adds arrives as a refusal rather than as silence", releaseWorkflow, strings.Join(mappingKeys(wf.Node), ", "))
	}
}

// --- planning -------------------------------------------------------------------------

// plan walks the jobs in `needs:` order, decides which run for this event shape, and
// executes the planning step of each that does. The values that run produces are what every
// guard downstream is decided against - the workflow's own logic, not a restatement of it.
func (g *gate) plan(r *Runner, wf *Workflow, roles *Roles, repo string, sh shape) (*planned, error) {
	order, err := topoJobs(wf)
	if err != nil {
		return nil, err
	}
	res := &planned{shape: sh, jobs: map[string]*jobPlan{}, order: order}
	needs := map[string]any{}
	plan := roles.step("plan")

	for _, id := range order {
		job := wf.Jobs[id]
		ctx := evalCtx{success: true, vars: map[string]any{
			"github": githubCtx(repo, sh),
			"env":    mergeEnv(wf.Env, job.Env),
			"steps":  map[string]any{},
			"needs":  needs,
		}}
		jp := &jobPlan{id: id, job: job, ctx: ctx, outs: map[string]string{}}
		res.jobs[id] = jp

		jobNeeds, err := job.NeedsOf()
		if err != nil {
			return nil, err
		}
		blocked := ""
		for _, n := range jobNeeds {
			if np, ok := res.jobs[n]; !ok || !np.runs {
				blocked = n
				break
			}
		}
		if blocked != "" {
			jp.why = fmt.Sprintf("it needs job %q, which does not run", blocked)
			continue
		}
		runs, err := ConditionRuns(job.If, ctx)
		if err != nil {
			return nil, fmt.Errorf("job %q's own `if:` cannot be decided for %s: %w", id, sh.label, err)
		}
		if !runs {
			jp.why = fmt.Sprintf("its `if:` (`%s`) is false against the values the planning logic produced", strings.TrimSpace(job.If))
			continue
		}
		jp.runs = true

		// Execute the planning step, if this is the job that carries it.
		if plan.JobID == id {
			runsStep, err := ConditionRuns(plan.If, jp.ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", plan.Label(), err)
			}
			if !runsStep {
				return nil, fmt.Errorf("%s does not run for %s, so nothing decides whether this run may publish", plan.Label(), sh.label)
			}
			env, err := stepEnv(wf, job, plan, jp.ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", plan.Label(), err)
			}
			script, err := Interpolate(plan.Run, jp.ctx)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", plan.Label(), err)
			}
			out, err := r.Run(script, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", plan.Label(), err)
			}
			res.output += out.Output
			if out.ExitCode != 0 {
				res.failed, res.failStep, res.exitCode = true, plan, out.ExitCode
				return res, nil
			}
			if len(out.Argv) > 0 {
				return nil, fmt.Errorf("%s invoked %s. The planning step DECIDES; a step that reaches a registry, a remote or a package index is not planning, and this gate executes only the planning step precisely so that it never runs one that does", plan.Label(), formatArgv(out.Argv))
			}
			outs := map[string]any{}
			for k, v := range out.Outputs {
				outs[k] = v
				jp.outs[k] = v
			}
			jp.ctx.vars["steps"].(map[string]any)[plan.ID] = map[string]any{
				"outputs":    outs,
				"conclusion": "success",
				"outcome":    "success",
			}
		}

		// Publish this job's declared outputs into the `needs` context downstream jobs read.
		jobOut := map[string]any{}
		for k, expr := range job.Outputs {
			v, err := Interpolate(expr, jp.ctx)
			if err != nil {
				return nil, fmt.Errorf("job %q's output %q cannot be decided: %w", id, k, err)
			}
			jobOut[k] = v
		}
		needs[id] = map[string]any{"outputs": jobOut, "result": "success"}
	}
	return res, nil
}

func githubCtx(repo string, sh shape) map[string]any {
	owner, _, _ := strings.Cut(repo, "/")
	return map[string]any{
		"event_name":       sh.event,
		"ref_name":         sh.refName,
		"ref":              refFor(sh),
		"repository":       repo,
		"repository_owner": owner,
		"sha":              fakeSHA,
		"actor":            "release-shape-gate",
	}
}

// topoJobs orders the jobs so every job comes after everything it needs.
func topoJobs(wf *Workflow) ([]string, error) {
	var out []string
	done := map[string]bool{}
	for len(out) < len(wf.Jobs) {
		progress := false
		for _, id := range wf.JobIDs() {
			if done[id] {
				continue
			}
			needs, err := wf.Jobs[id].NeedsOf()
			if err != nil {
				return nil, err
			}
			ready := true
			for _, n := range needs {
				if _, ok := wf.Jobs[n]; !ok {
					return nil, fmt.Errorf("job %q needs %q, which %s does not define", id, n, releaseWorkflow)
				}
				if !done[n] {
					ready = false
				}
			}
			if !ready {
				continue
			}
			done[id] = true
			out = append(out, id)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("the jobs in %s form a `needs:` CYCLE, so there is no order to grade", releaseWorkflow)
		}
	}
	return out, nil
}

func refFor(sh shape) string {
	if sh.event == "push" {
		return "refs/tags/" + sh.refName
	}
	return "refs/heads/" + sh.refName
}

func outputsOf(p *planned) string {
	var parts []string
	for _, id := range p.order {
		jp := p.jobs[id]
		for _, k := range sortedStrings(jp.outs) {
			parts = append(parts, fmt.Sprintf("%s=%v", k, jp.outs[k]))
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

func sortedStrings(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- A6, A12: capability -----------------------------------------------------------------

// checkDispatchHoldsNoCapability is the criterion, in its decidable form. Every job that
// runs on a manual dispatch must hold nothing that could authorise something leaving this
// machine, and every job that DOES hold such a thing must not run.
//
// Note what is NOT asked: what any step's script says. That question is undecidable over
// arbitrary shell and cost this gate six fail-opens; a step in a job with no write scope and
// no secret publishes nothing however it is spelled.
func (g *gate) checkDispatchHoldsNoCapability(wf *Workflow, p *planned) {
	safe, unsafe := 0, 0
	for _, id := range p.order {
		jp := p.jobs[id]
		problems, err := CanPublish(wf, jp.job)
		if err != nil {
			g.bad("on %s, what job %q may do CANNOT BE DECIDED: %v", p.shape.label, id, err)
			unsafe++
			continue
		}
		if !jp.runs {
			if len(problems) > 0 {
				g.note("job %q holds a publishing grant and does NOT run on %s (%s)", id, p.shape.label, jp.why)
			}
			continue
		}
		if len(problems) == 0 {
			safe++
			continue
		}
		unsafe++
		for _, pr := range problems {
			g.bad("on %s, job %q RUNS and it holds a capability that could authorise a published act:\n%s\nA dry run must publish nothing, and the way that is guaranteed is that nothing which runs on a dispatch is handed anything a registry, a ref or a release would accept. The planning logic produced %s.",
				p.shape.label, id, pr.Detail, outputsOf(p))
		}
	}
	if unsafe == 0 {
		var descriptions []string
		for _, id := range p.running() {
			gr, err := EffectiveGrants(wf, p.jobs[id].job)
			if err != nil {
				continue
			}
			descriptions = append(descriptions, fmt.Sprintf("%s %s", id, gr.String()))
		}
		g.note("on %s, %d job(s) run and NONE of them holds a capability that could publish anything: %s. A step in them may say `docker push` in any spelling and still publish nothing",
			p.shape.label, safe, strings.Join(descriptions, ", "))
	}
}

// checkIrreversibleActsLiveBehindAGrant is the other half of the same property: a step that
// really does perform an irreversible act must live in a job that CAN, or the capability
// split is decorative - the act would have been moved somewhere that holds no grant, where
// this gate stops asking about it and the release fails at run time instead.
func (g *gate) checkIrreversibleActsLiveBehindAGrant(wf *Workflow, roles *Roles) {
	named := 0
	for _, r := range releaseRoles {
		if !r.needsGrant {
			continue
		}
		s := roles.step(r.id)
		problems, err := CanPublish(wf, wf.Jobs[s.JobID])
		if err != nil {
			g.bad("cannot decide what job %q may do, and it carries %s: %v", s.JobID, r.what, err)
			continue
		}
		if len(problems) == 0 {
			g.bad("%s performs %s, which is irreversible, but job %q holds NO capability that could carry it out. Either the act is in the wrong job - it would fail at run time, having passed this gate - or a grant it needs has been removed. An irreversible act must sit in a job that visibly holds the authority for it, so that this gate can grade the authority instead of the shell.",
				s.Label(), r.what, s.JobID)
			continue
		}
		named++
	}
	if named == len(grantRoles()) {
		g.note("every irreversible act (%s) lives in a job that visibly holds the grant it needs", strings.Join(grantRoles(), ", "))
	}
}

func grantRoles() []string {
	var out []string
	for _, r := range releaseRoles {
		if r.needsGrant {
			out = append(out, r.id)
		}
	}
	return out
}

// --- A7, A13: the order ------------------------------------------------------------------

func (g *gate) checkOrder(wf *Workflow, roles *Roles, p *planned) {
	problems := 0
	for _, pair := range mustPrecede {
		before, after := roles.step(pair[0]), roles.step(pair[1])
		if !g.stepRuns(p, before) || !g.stepRuns(p, after) {
			problems++
			missing, other := pair[0], pair[1]
			if g.stepRuns(p, before) {
				missing, other = pair[1], pair[0]
			}
			g.bad("on %s, %s (`%s`) does not run, so the order this release depends on cannot hold: %s must come before %s.",
				p.shape.label, roleWhat(missing), missing, roleWhat(pair[0]), roleWhat(pair[1]))
			_ = other
			continue
		}
		ok, err := g.precedes(wf, before, after)
		if err != nil {
			problems++
			g.bad("on %s, this gate cannot order %s (`%s`) against %s (`%s`): %v",
				p.shape.label, roleWhat(pair[0]), pair[0], roleWhat(pair[1]), pair[1], err)
			continue
		}
		if !ok {
			problems++
			g.bad("on %s, %s does NOT run before %s.\n%s is %s; %s is %s.\nThe floating reference would end up pointing at an image this run never proved, for every user running `docker compose pull`.",
				p.shape.label, roleWhat(pair[0]), roleWhat(pair[1]),
				pair[0], before.Label(), pair[1], after.Label())
		}
	}
	if problems == 0 {
		// WHAT THIS SENTENCE CLAIMS, AND WHAT IT DOES NOT. It names the STEPS, by declared
		// id and by the program each invokes, in the order the `needs:` graph and
		// declaration order put them. It does not say what any of those programs then does
		// - this gate does not read scripts/release-resmoke.sh, scripts/release-promote.sh
		// or scripts/smoke-image.sh, and it must not (the conductor's capability ruling:
		// deciding what a step's `run:` script DOES is not an acceptable route). Those
		// scripts' behaviour is driven for real, against a stubbed registry, by
		// `make release-shape-selftest`. A sentence here that described their EFFECT would
		// reassure every reader who skims stdout over a question nothing asked, which is
		// what expect_absent exists to refuse.
		var chain []string
		for _, id := range orderedRoles {
			chain = append(chain, fmt.Sprintf("%s (%s)", roles.step(id).Label(), roleInvocation(id)))
		}
		g.note("on %s the order holds, by declared id, `needs:` and declaration order:\n      %s\n      What each of those programs DOES is not read here; `make release-shape-selftest` drives the release scripts for real against a stubbed registry.",
			p.shape.label, strings.Join(chain, "\n      -> "))
	}
}

// orderedRoles is the chain the order sentence prints, in the sequence mustPrecede requires.
var orderedRoles = []string{"full-gate", "smoke-amd64", "smoke-arm64", "push-version", "resmoke", "promote-latest", "resolve-compose"}

// precedes decides whether `a` is guaranteed to have finished before `b` starts. Within one
// job that is declaration order. Across jobs it is the `needs:` graph, and two jobs with no
// path between them are CONCURRENT: the gate refuses to order them rather than reporting an
// order it did not check.
func (g *gate) precedes(wf *Workflow, a, b Step) (bool, error) {
	if a.JobID == b.JobID {
		return a.Index < b.Index, nil
	}
	fwd, err := wf.reaches(b.JobID, a.JobID)
	if err != nil {
		return false, err
	}
	if fwd {
		return true, nil
	}
	rev, err := wf.reaches(a.JobID, b.JobID)
	if err != nil {
		return false, err
	}
	if rev {
		return false, nil
	}
	return false, fmt.Errorf("jobs %q and %q are CONCURRENT: neither `needs:` the other, so they may run in either order or at the same time. An order that is not in the `needs:` graph is not an order", a.JobID, b.JobID)
}

func (g *gate) stepRuns(p *planned, s Step) bool {
	jp, ok := p.jobs[s.JobID]
	if !ok || !jp.runs {
		return false
	}
	runs, err := ConditionRuns(s.If, jp.ctx)
	if err != nil {
		g.bad("cannot decide whether %s runs: %v", s.Label(), err)
		return false
	}
	return runs
}

// --- A14: nothing before the promotion may fail quietly ----------------------------------

func (g *gate) checkNothingToleratesAFailure(wf *Workflow, roles *Roles, p *planned) {
	promote := roles.step("promote-latest")
	tolerated := 0
	for _, id := range p.order {
		job := wf.Jobs[id]
		if tolerant, why := tolerates(job.ContinueOnError); tolerant {
			tolerated++
			g.bad("job %q is marked `%s`, so a failure inside it is tolerated and everything downstream of it - including the promotion - proceeds anyway.", id, why)
		}
		if !p.jobs[id].runs {
			continue
		}
		for _, s := range job.Steps {
			before, err := g.precedes(wf, s, promote)
			if err != nil || !before {
				continue
			}
			if !g.stepRuns(p, s) {
				continue
			}
			if tolerant, why := s.TolerateFailure(); tolerant {
				tolerated++
				g.bad("%s runs before the floating reference moves and is marked `%s`, so its failure would NOT fail the run. `:latest` would be promoted over the top of it.", s.Label(), why)
			}
		}
	}
	if tolerated == 0 {
		g.note("no step and no job before the promotion tolerates its own failure")
	}
}

// --- A3: once anything has failed, nothing publishes -------------------------------------

func (g *gate) checkNothingPublishesAfterAFailure(wf *Workflow, p *planned) {
	bad := 0
	for _, id := range p.order {
		jp := p.jobs[id]
		problems, err := CanPublish(wf, jp.job)
		if err != nil || len(problems) == 0 {
			continue // a job that can publish nothing is not the question here
		}
		needs, err := jp.job.NeedsOf()
		if err != nil {
			g.bad("%v", err)
			continue
		}
		failing := evalCtx{vars: jp.ctx.vars, success: false}
		runs, err := ConditionRuns(jp.job.If, failing)
		if err != nil {
			g.bad("cannot decide whether job %q runs after a failure: %v", id, err)
			continue
		}
		if len(needs) == 0 {
			bad++
			g.bad("job %q holds a publishing grant and `needs:` nothing, so nothing sequences it behind the gate at all: it starts immediately, in parallel with the job that would have proved the artefact.", id)
			continue
		}
		if runs {
			bad++
			g.bad("job %q holds a publishing grant and would STILL RUN after an earlier job has FAILED (its `if:` is `%s`, which does not defer to the run's success). A failed gate or smoke run must leave the floating reference exactly where it was.", id, strings.TrimSpace(jp.job.If))
		}
	}
	if bad == 0 {
		g.note("after a failure, NO job holding a publishing grant runs at all - the floating reference stays where it was")
	}
}

// --- the pre-release invariant -----------------------------------------------------------

func (g *gate) checkPreReleaseDoesNotPromote(r *Runner, wf *Workflow, roles *Roles, repo string) {
	pre, err := g.plan(r, wf, roles, repo, shape{"a pre-release tag push (" + samplePreTag + ")", "push", samplePreTag})
	if err != nil {
		g.bad("cannot plan a pre-release tag: %v", err)
		return
	}
	if pre.failed {
		g.bad("the planning logic refuses the pre-release tag %s (exit %d). A pre-release is a supported release shape:\n%s", samplePreTag, pre.exitCode, indent(pre.output))
		return
	}
	promote := roles.step("promote-latest")
	if g.stepRuns(pre, promote) {
		g.bad("on a pre-release tag (%s), %s would move the floating reference. `docker pull` would hand a release candidate to everyone who did not ask for one.", samplePreTag, promote.Label())
		return
	}
	if !g.stepRuns(pre, roles.step("push-version")) {
		g.bad("on a pre-release tag (%s), the version-tag push does not run either. A pre-release is meant to PUBLISH under its own tag and merely not become the floating reference.", samplePreTag)
		return
	}
	g.note("a pre-release tag (%s) publishes but does NOT move the floating reference", samplePreTag)
}

// --- A4, A10 ------------------------------------------------------------------------------

func (g *gate) checkMajorVersionZero(r *Runner, wf *Workflow, roles *Roles, repo string) {
	major, err := g.plan(r, wf, roles, repo, shape{"a tag whose major version is not zero (" + sampleMajorTag + ")", "push", sampleMajorTag})
	if err != nil {
		g.bad("cannot plan %s: %v", sampleMajorTag, err)
		return
	}
	if !major.failed {
		g.bad("the release path ACCEPTS %s. Major version zero is for initial development and promises nothing (semver.org clause 4); the first non-zero major DEFINES the public API (clause 5), and this project has not yet decided that its configuration keys, HTTP surface and metric names will not change without one. The planning logic must refuse it before anything is published.", sampleMajorTag)
		return
	}
	// The refusal has to come before anything could publish. It lives in the `plan` role,
	// and every irreversible act is in a job that `needs:` the job holding it.
	planStep := roles.step("plan")
	for _, r := range releaseRoles {
		if !r.needsGrant {
			continue
		}
		act := roles.step(r.id)
		ok, err := g.precedes(wf, planStep, act)
		if err != nil || !ok {
			g.bad("%s refuses %s, but %s (%s) is not sequenced behind it. The refusal must come first.", planStep.Label(), sampleMajorTag, act.Label(), r.what)
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

// --- A8, A16: the runbook ------------------------------------------------------------------

// An ACT is now every step in a job that holds a publishing grant, identified by the step's
// own `id:`. That is stronger than the catalogue it replaces and needs no reading: a step
// added to that job cannot avoid the runbook by being spelled unrecognisably, because
// nothing about what it says is consulted. It is in the job that can publish, so it counts.
func (g *gate) checkRunbookNamesEveryAct(wf *Workflow) {
	var acts []Step
	for _, jid := range wf.JobIDs() {
		job := wf.Jobs[jid]
		problems, err := CanPublish(wf, job)
		if err != nil || len(problems) == 0 {
			continue
		}
		for _, s := range job.Steps {
			if s.ID == "" {
				g.bad("%s is in job %q, which holds a publishing grant, and carries no `id:`. Every step in a job that can publish is an act the operator runbook has to name, and an act with no id cannot be named. Give it one.", s.Label(), jid)
				continue
			}
			acts = append(acts, s)
		}
	}
	if len(acts) == 0 {
		g.bad("no job in %s holds a publishing grant, so there is no irreversible act to check the runbook against. Every assertion here would then pass over nothing, which is not a gate. If the release genuinely no longer publishes, delete this gate deliberately rather than letting it report green.", releaseWorkflow)
		return
	}

	raw, err := os.ReadFile(g.path(runbookFile))
	if err != nil {
		g.bad("the operator runbook %s CANNOT BE READ (%v). %d irreversible act(s) in %s would then be checked against nothing:\n%s",
			runbookFile, err, len(acts), releaseWorkflow, actList(acts))
		return
	}
	text := string(raw)
	if len(reActID.FindAllString(text, -1)) == 0 {
		g.bad("the operator runbook %s names NO irreversible act (no `<job>/<step id>` anywhere in it). An empty document must not satisfy this check - that is the vacuous pass the whole gate exists to refuse. It must name:\n%s",
			runbookFile, actList(acts))
		return
	}
	missing := 0
	for _, s := range acts {
		if !strings.Contains(text, "`"+s.ActID()+"`") {
			missing++
			g.bad("%s runs in a job that holds a publishing grant, and the operator runbook does not name it.\nAdd `%s` to %s, with what it does, whether it can be undone, and by what. A step that can publish and that nobody wrote down is a step nobody reviewed.",
				s.Label(), s.ActID(), runbookFile)
		}
	}
	if missing == 0 {
		g.note("%s names every one of the %d irreversible act(s) in %s: %s", runbookFile, len(acts), releaseWorkflow, strings.Join(actIDs(acts), ", "))
	}
}

var reActID = regexp.MustCompile("`[a-z0-9_-]+/[a-z0-9-]+`")

// A digest is 64 lowercase hex characters behind `sha256:`. Anything else - a truncated one,
// an empty one, a tag that merely looks like one - is not a pin.
var reSha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// --- A9, A17, A11 ---------------------------------------------------------------------------

// THE EXAMPLE DEPLOYMENT NAMES ONE REFERENCE, AND IT USED TO PLAY TWO ROLES AT ONCE. Until
// S0057 pinned it, docker-compose.yml said `ghcr.io/nschatz/holdfast:latest`, which was both
// the reference a stranger PULLS and the reference a release MOVES - so a single comparison
// (compose == `${IMAGE}:${FLOATING_TAG}`) decided both at once. P1 severed them: an example
// deployment may not depend on a mutable reference, so it now pins a version tag AND the
// digest scripts/smoke-image.sh gated, while `:latest` goes on being PUBLISHED and is no
// longer depended on. Publishing a floating reference is not depending on one.
//
// So each half is held against the thing that now carries it, and none of them is dropped:
//
//   - the NAME is held against the image THIS module's repository produces. That is what A9
//     is actually about: the reference is derived from `github.repository` at run time, so a
//     repository rename moves it, and nothing rewrites a reference already sitting in a
//     user's compose file.
//   - the DIGEST must be there at all. A reference without one names a LABEL, and the label
//     is exactly what a release moves, so an unpinned example deployment silently changes the
//     encoder - and the libvmaf instrument the no-loss verdict is measured with - under a
//     deployment nobody touched.
//   - the TAG must NOT be the one this release MOVES. Pin the tag `promote-latest` retags and
//     the example deployment's own tag and digest disagree the moment the next release lands,
//     which is the drift the pin exists to prevent; it would also modify the contents of a
//     released version, which semver.org forbids outright.
//
// The registry half - that the reference RESOLVES at all, and that what the promotion moved
// resolves to the digest this run gated - cannot be decided offline. That is A11, and it is
// scripts/resolve-compose-image.sh, driven for real by `make release-shape-selftest`.
func (g *gate) checkComposeReferenceAgreement(wf *Workflow, roles *Roles, p *planned, repo string) {
	composeRef, err := composeImageRef(g.path(composeFile))
	if err != nil {
		g.bad("%v", err) // A17: the file is named in every one of these
		return
	}

	promote := roles.step("promote-latest")
	jp := p.jobs[promote.JobID]
	env, err := stepEnv(wf, wf.Jobs[promote.JobID], promote, jp.ctx)
	if err != nil {
		g.bad("cannot decide the environment %s runs with: %v", promote.Label(), err)
		return
	}
	image, floating := env["IMAGE"], env[floatingTagEnv]
	if image == "" || floating == "" {
		g.bad("%s does not declare both IMAGE and %s in its `env:`, so the reference it moves is unknown to this gate. The floating tag is declared THERE, once, and read here and by scripts/release-promote.sh: it is the value docker-compose.yml is held against, and a value spelled in two places is the shape this repository refuses for the ffmpeg pin. Its env is %v",
			promote.Label(), floatingTagEnv, sortedStrings(env))
		return
	}
	promoted := image + ":" + floating

	// The image the workflow derives has to be the image THIS module's repository produces.
	// It is derived at run time from `github.repository`, so a repository rename moves it -
	// which is why the rename is part of the irreversible set.
	wantImage := "ghcr.io/" + strings.ToLower(repo)
	if image != wantImage {
		g.bad("the reference %s promotes is not the one this repository's own release produces.\n%s derives: %s\nthis module (%s) implies: %s",
			promote.Label(), promote.Label(), image, goModFile, wantImage)
		return
	}

	composeName := refName(composeRef)
	composeTag, hasTag := refTag(composeRef)
	_, composeDigest := refDigest(composeRef)

	if composeName != image {
		g.bad("the example deployment names an image reference this repository's own release would NEVER produce.\n%s names:  %s\n%s publishes from: %s\nA user who runs the published compose file would pull a reference nothing publishes. The release image is derived from the repository name at run time, so a repository rename moves this reference too.",
			composeFile, composeRef, promote.Label(), image)
		return
	}
	if !hasTag {
		g.bad("the example deployment %s names %q, which carries NO TAG. A reference with no tag is `:latest` by default, which is the reference this release MOVES - so the example deployment would silently change under every user who pulled it, and there would be nothing for a reader to compare against the release they meant to run.",
			composeFile, composeRef)
		return
	}
	if !reSha256Digest.MatchString(composeDigest) {
		g.bad("the example deployment %s names %q, which is NOT pinned to an `@sha256:` digest.\nA tag is a LABEL and this release path MOVES one (%s), so a reference without a digest names whatever the registry serves on the day a stranger pulls - not the artefact scripts/smoke-image.sh gated. Pin both halves:\n  image: %s:%s@sha256:<64 hex>\nResolve one with: docker buildx imagetools inspect %s:%s",
			composeFile, composeRef, promoted, image, composeTag, image, composeTag)
		return
	}
	if composeTag == floating {
		g.bad("the example deployment %s PINS the tag this release MOVES.\n%s names:    %s\n%s promotes: %s\nThat tag is retagged onto each newly gated release, so the tag and the digest beside it would disagree the moment the next release lands - and a release that retags a version the example deployment pins MODIFIES the contents of an already-released version, which semver.org forbids outright. The example deployment pins a version tag; the floating reference is published, never depended on.",
			composeFile, composeFile, composeRef, promote.Label(), promoted)
		return
	}
	g.note("the example deployment's image reference is published by this repository's own release, and is not the reference a release MOVES\n      %s: %s\n        name %s = the image %s publishes from\n        tag  %s, pinned to %s\n      %s promotes: %s (moved, never depended on)",
		composeFile, composeRef, composeName, promote.Label(), composeTag, composeDigest, promote.Label(), promoted)

	// The floating reference must be retagged onto the version this run gated, and no
	// earlier step may push the floating reference itself.
	version := env["VERSION"]
	if version == "" {
		g.bad("%s does not declare VERSION in its `env:`, so which version the floating reference is moved onto is unknown to this gate.", promote.Label())
		return
	}
	// checkHandedValues (handed.go) has already required this step's IMAGE and VERSION to BE
	// the planning logic's own outputs, traced through the `needs:` graph, so `gated` here is
	// this run's own reference and not a second notion of it read out of the same file.
	gated := image + ":" + version
	push := roles.step("push-version")
	pushed, err := interpolatedInput(wf, wf.Jobs[push.JobID], push, p.jobs[push.JobID].ctx, "tags")
	if err != nil {
		g.bad("cannot read the references %s pushes: %v", push.Label(), err)
		return
	}
	refs := splitRefs(pushed)
	if len(refs) == 0 {
		g.bad("%s names no `tags:` this gate can read, so whether it pushes the floating reference %s is UNDECIDABLE - and an undecidable publish is not a harmless one.", push.Label(), promoted)
		return
	}
	for _, ref := range refs {
		if ref == promoted {
			g.bad("%s pushes %s directly. The floating reference would then be pullable before the pushed artefact has been pulled back and re-smoked, which is exactly what promoting it separately, last, avoids.", push.Label(), promoted)
			return
		}
	}
	if len(refs) != 1 || refs[0] != gated {
		g.bad("%s does not push exactly the version this run gated.\nexpected: %s\nactually: %s", push.Label(), gated, strings.Join(refs, " "))
		return
	}
	// WHAT WAS CHECKED, AND BY WHOM. Everything above is structured YAML this gate read:
	// the promotion step's `env:` names the floating reference it is handed, the push step's
	// `tags:` input names the references it publishes, and those references are compared
	// whole. Whether `scripts/release-promote.sh` then retags rather than rebuilds is a
	// property of that script, which this gate does not read - it is driven for real,
	// against a stubbed registry with its argv recorded, by `make release-shape-selftest`.
	// The sentence used to say "the same digest, not a rebuild", which stated the script's
	// EFFECT over a question nothing here asked.
	g.note("%s is handed %s to move and %s to move it onto, and %s publishes %s and nothing else. What %s does with them is driven by `make release-shape-selftest`, not read here",
		promote.Label(), promoted, gated, push.Label(), gated, roles.step("promote-latest").Run)

	// A11's wiring: a step declaring `id: resolve-compose` runs after the promotion, or A5
	// has no enforcement behind it on any release after the first. The order is checked in
	// checkOrder; this states which step carries it. What that script does against a
	// registry is driven, four exit codes at a time, by `make release-shape-selftest`.
	g.note("after the promotion, %s runs, invoking %s", roles.step("resolve-compose").Label(), roles.step("resolve-compose").Run)
}

// interpolatedInput reads one `with:` input, with every expression in it decided against the
// values the planning logic produced.
func interpolatedInput(wf *Workflow, job Job, s Step, ctx evalCtx, key string) (string, error) {
	raw, ok := s.With[key]
	if !ok {
		return "", fmt.Errorf("it declares no `%s:` input", key)
	}
	return Interpolate(yamlString(raw), ctx)
}

func splitRefs(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for _, f := range strings.Split(line, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

// --- helpers --------------------------------------------------------------------------

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

func formatArgv(argv [][]string) string {
	var lines []string
	for _, cmd := range argv {
		lines = append(lines, strings.Join(cmd, " "))
	}
	return strings.Join(lines, "; ")
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

func actList(acts []Step) string {
	var lines []string
	for _, s := range acts {
		lines = append(lines, fmt.Sprintf("  `%s`  (%s)", s.ActID(), s.Label()))
	}
	return strings.Join(lines, "\n")
}

func actIDs(acts []Step) []string {
	var out []string
	for _, s := range acts {
		out = append(out, s.ActID())
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

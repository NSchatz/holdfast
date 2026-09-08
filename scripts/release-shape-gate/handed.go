package main

// WHAT OBJECT DOES A ROLE STEP PERFORM ITS ACT ON? - answered STRUCTURALLY, from the
// workflow's own data flow, never by comparing the value with a sample.
//
// roles.go decides WHICH step holds each position and accounts for its invocation over the
// step's whole structured surface. It did not, at first, look at the VALUES that invocation is
// handed, and that is a hole of the same shape as every other one this gate has closed: a step
// can invoke the right program against the WRONG OBJECT while every other assertion here stays
// green. `REF: ${IMAGE}:latest` on the re-smoke is one line - the role still holds, the
// invocation is untouched, the order sentence still prints - and the release pulls back the
// PREVIOUS artefact, which passes because it was gated last time, while the one this run just
// pushed is never pulled back at all and `:latest` is then promoted onto it (S0046 F22).
// `VERSION: latest` on the resolver turns A11's digest comparison into a comparison of the
// compose reference with itself, which cannot fail, and A11 is the only enforcement A5 has on
// every release after the first (F23).
//
// THE FIRST ANSWER WAS A COMPARISON, AND A COMPARISON NEEDS SOMETHING TO COMPARE AGAINST.
// It held each value against the outputs of a planned release - which meant a value equal to
// THAT release's version was accepted as "the value the planning logic produced". The sample
// was `v0.1.0`, which is not an arbitrary string: it is the version this repository has
// actually published, it is named throughout docs/release.md, CLAUDE.md and README.md, and it
// is what a maintainer copies out of a green run's log. `REF: ghcr.io/nschatz/holdfast:v0.1.0`
// passed, and every later release would then re-smoke the July artefact while `:latest` moved
// onto one nothing pulled back (S0046 F26). Widening the comparison - two samples, three -
// only moves the coincidence; it is deciding whether a value is right by looking at the value,
// which is the shape that lost eight times over on the other half of this gate.
//
// SO THE VALUE IS NOT COMPARED AT ALL. A role step's handed value must BE the planning logic's
// own output, not equal one: the `env:` scalar has to be an EXPRESSION naming that output, and
// this file traces the reference back through `needs.<job>.outputs.*` to the step holding the
// `plan` role. Any literal is refused, and so is any expression the trace cannot follow. There
// is no sample anywhere in the question, so there is no sample for a literal to coincide with,
// and the refusal names the step and the key rather than the spelling. This is the shape the
// conductor's capability ruling asks for throughout: structured YAML, deny by default, nothing
// decided by reading a value.
//
// THE ONE VALUE THAT IS NOT A PLANNING OUTPUT is the floating tag, and it is declared rather
// than derived on purpose: `FLOATING_TAG: latest` is the single place a release says which
// reference it moves, read here and by scripts/release-promote.sh and
// scripts/resolve-compose-image.sh instead of being spelled three times. It is a literal by
// construction, so it gets the other treatment - held against the tag `docker-compose.yml`
// itself pins, which is a committed file rather than a sample.
//
// THAT HOLD IS AN EXCLUSION, and it used to be an equality. It was an equality while the
// example deployment pulled `:latest`: the reference a user pulls and the reference a release
// MOVES were the same string, so a release moving anything else moved one nobody pulled.
// S0057's P1 severed them - an example deployment may not DEPEND on a mutable reference, so
// it pins a version tag and the digest that was gated, while `:latest` goes on being
// published. Publishing a floating reference is not depending on one. So the value held
// against that file is now the one it must NOT be: retagging the version the example
// deployment pins would leave its tag and its digest disagreeing on the day the next release
// lands, and would modify the contents of an already-released version. The file is the same
// committed anchor either way; what it anchors is stated below and in the gate's output
// rather than smuggled in as though it were the same rule.

import (
	"fmt"
	"strings"
)

// --- what a declared value has to BE ------------------------------------------------------

// Canonical source ids. A trace resolves one `${{ … }}` expression to one of these, or fails.
const (
	srcPlanOutput = "the planning logic's own output " // + <key>
	srcGithub     = "the workflow event's own "        // + <field>
)

// heldPart is one element of the sequence a value must be: either a literal segment that must
// appear exactly, or a reference that must TRACE to a named source. Nothing else may appear -
// an extra character outside the declared parts is a different object.
type heldPart struct {
	lit string // a literal segment, exactly
	ref string // a canonical source id the reference must trace to
}

// heldForm is how one declared name is decided.
type heldForm struct {
	parts []heldPart // the sequence the value must BE, for a value produced by the run
	// A value that is NOT produced by the run at all: it is declared in the workflow, and
	// held against the tag this committed file PINS - which it must not be. The floating
	// reference is the only one.
	literalFrom string
	what        string // how the source is described in every message
}

// heldAs declares, for every source a role can name, what a value holding it must BE. Deny by
// default: a source with no row here reds by name rather than being graded as whichever kind
// the zero value happens to be.
var heldAs = map[envHeld]heldForm{
	heldPlannedImage: {
		parts: []heldPart{{ref: srcPlanOutput + "image"}},
		what:  "the image reference the planning logic produced",
	},
	heldPlannedVersion: {
		parts: []heldPart{{ref: srcPlanOutput + "version"}},
		what:  "the version the planning logic produced",
	},
	heldGatedRef: {
		parts: []heldPart{{ref: srcPlanOutput + "image"}, {lit: ":"}, {ref: srcPlanOutput + "version"}},
		what:  "the reference this run pushes and gates, built from the planning logic's own image and version and nothing else",
	},
	heldEventName: {
		parts: []heldPart{{ref: srcGithub + "event_name"}},
		what:  "the event this run was triggered by",
	},
	heldRefName: {
		parts: []heldPart{{ref: srcGithub + "ref_name"}},
		what:  "the ref this run was triggered on",
	},
	heldRepository: {
		parts: []heldPart{{ref: srcGithub + "repository"}},
		what:  "the repository this run belongs to, which the image reference is derived from",
	},
	heldPlannedPrerelease: {
		parts: []heldPart{{ref: srcPlanOutput + "prerelease"}},
		what:  "whether the planning logic decided this release is a pre-release",
	},
	heldFloatingTag: {
		literalFrom: composeFile,
		what:        "the floating reference a release moves, declared here once and held against the tag " + composeFile + " pins, which it must NOT be",
	},
}

// heldPosition is the position a value is held FOR. There are two: a declared ROLE
// (roles.go), and an irreversible ACT that holds no role (acts.go). Everything below serves
// both, because the question - is this value the planning logic's own output, traced through
// the `needs:` graph? - is the same question either way, and answering it twice would be two
// traces to keep in step. What differs is only how the position is named in the refusal.
type heldPosition struct {
	id   string
	what string
	// phrase is how the position is described after the step's label: "holds the role `x`
	// (y)" or "is the irreversible act `x` (y)".
	phrase string
}

func rolePosition(r role) heldPosition {
	return heldPosition{id: r.id, what: r.what, phrase: fmt.Sprintf("holds the role `%s` (%s)", r.id, r.what)}
}

// --- the check ----------------------------------------------------------------------------

// checkHandedValues decides every value a role step hands its program. It reads structured
// YAML and the workflow's own outputs graph; it plans nothing, executes nothing, and compares
// no value with a sample.
func (g *gate) checkHandedValues(wf *Workflow, roles *Roles) {
	plan := roles.step("plan")
	pinned, pinErr := g.composePinnedTag()

	var held []string
	for _, r := range releaseRoles {
		if len(r.handsEnv) == 0 && len(r.handsInput) == 0 {
			continue
		}
		s := roles.step(r.id)
		job := wf.Jobs[s.JobID]
		pos := rolePosition(r)

		for _, name := range sortedEnvNames(r.handsEnv) {
			raw, where, present := rawEnvValue(wf, job, s, name)
			if !present {
				g.badUnset(s, pos, name, r.handsEnv[name])
				continue
			}
			if line, ok := g.holdOne(wf, plan, s, pos, name, "`env:`"+where, raw, r.handsEnv[name], pinned, pinErr); ok {
				held = append(held, line)
			}
		}
		for _, name := range sortedInputNames(r.handsInput) {
			v, ok := s.With[name]
			if !ok {
				g.badUnset(s, pos, name, r.handsInput[name])
				continue
			}
			if line, ok := g.holdOne(wf, plan, s, pos, name, "`with:`", yamlString(v), r.handsInput[name], pinned, pinErr); ok {
				held = append(held, line)
			}
		}
	}
	if len(held) > 0 {
		g.note("every value a role step hands its program IS the planning logic's own output, traced through the `needs:` graph rather than compared with a sample:\n      %s", strings.Join(held, "\n      "))
	}
}

func (g *gate) badUnset(s Step, pos heldPosition, name string, spec envSpec) {
	form, ok := heldAs[spec.holds]
	what := "a value this gate has not classified"
	if ok {
		what = form.what
	}
	g.bad("%s %s, whose act is performed on `%s` - and nothing sets it, at any of the three levels.\nIt has to be %s (%s). A role step handed nothing is a release step that fails in the middle of a release, or worse, one whose program falls back to a default nobody chose.",
		s.Label(), pos.phrase, name, what, spec.what)
}

// holdOne decides one declared name and returns the line the green note prints for it.
func (g *gate) holdOne(wf *Workflow, plan, s Step, pos heldPosition, name, where, raw string, spec envSpec, pinned string, pinErr error) (string, bool) {
	form, declared := heldAs[spec.holds]
	if !declared {
		g.bad("%s %s and declares `%s`, and this gate has said NOTHING about what that value must be.\nA name whose value nobody decides is the hole this check exists to refuse: the position holds, the invocation is untouched, and the step does the right thing to the wrong object. Give it a row in heldAs, or stop declaring it",
			s.Label(), pos.phrase, name)
		return "", false
	}

	// The floating reference: the one value a release DECLARES rather than derives. It is a
	// literal on purpose and is held against a committed file, not against a planned run.
	if form.literalFrom != "" {
		if pinErr != nil {
			g.bad("%v", pinErr)
			return "", false
		}
		parts, err := splitTemplate(raw)
		if err != nil {
			g.bad("%s %s and its `%s` cannot be read: %v", s.Label(), pos.phrase, name, err)
			return "", false
		}
		if len(parts) != 1 || parts[0].ref != "" {
			g.bad("%s %s and sets `%s: %s`, which carries an expression.\n`%s` is %s: it is the one value a release DECLARES rather than derives, so it is a plain literal here and it is held against %s. An expression would move it somewhere this gate cannot follow.",
				s.Label(), pos.phrase, name, raw, name, form.what, form.literalFrom)
			return "", false
		}
		if raw == pinned {
			g.bad("%s %s and sets `%s: %s`, which is the very tag %s PINS.\n`%s` is %s. That tag is RETAGGED onto each newly gated release, so the example deployment's tag and the digest pinned beside it would disagree the moment the next release lands - and retagging a version somebody already pulled modifies the contents of a released version, which semver.org forbids. The example deployment pins a version; the floating reference is published, never depended on.",
				s.Label(), pos.phrase, name, raw, form.literalFrom, name, form.what)
			return "", false
		}
		return fmt.Sprintf("%s %s `%s` = %s (%s)", s.Label(), where, name, raw, form.what), true
	}

	parts, err := splitTemplate(raw)
	if err != nil {
		g.bad("%s %s and its `%s` cannot be read: %v", s.Label(), pos.phrase, name, err)
		return "", false
	}
	if len(parts) != len(form.parts) {
		g.bad("%s %s and is handed `%s: %s`, which is not %s.\nit has to BE: %s\nit is:       %s\nA release step's object is not COMPARED with a value, it has to BE the planning logic's own output: this gate traces the expression back through the `needs:` graph to the step holding the `plan` role. Nothing here is compared with a sample, so no sample can be copied into it (S0046 F22, F23, F26).",
			s.Label(), pos.phrase, name, raw, form.what, formSketch(form), sketchOf(parts))
		return "", false
	}
	var traced []string
	for i, want := range form.parts {
		got := parts[i]
		if want.ref == "" {
			if got.ref != "" || got.lit != want.lit {
				g.bad("%s %s and is handed `%s: %s`, which is not %s.\nit has to BE: %s\nit is:       %s\nThe separator is part of the object: %q was expected here.",
					s.Label(), pos.phrase, name, raw, form.what, formSketch(form), sketchOf(parts), want.lit)
				return "", false
			}
			continue
		}
		if got.ref == "" {
			g.bad("%s %s and is handed `%s: %s`, which is not %s: %q is a LITERAL where %s belongs.\nit has to BE: %s\nit is:       %s\nA literal is not held to anything - it is a value the step spells for itself, so a release cut at any other version performs this act on whatever the literal names, for ever, with every other assertion in this gate still green. Name the output instead of copying its value (S0046 F22, F23, F26).",
				s.Label(), pos.phrase, name, raw, form.what, got.lit, want.ref, formSketch(form), sketchOf(parts))
			return "", false
		}
		src, err := traceRef(wf, plan, s.JobID, got.ref, 0)
		if err != nil {
			g.bad("%s %s and is handed `%s: %s`, and this gate CANNOT TRACE `${{ %s }}` back to the planning logic: %v\nAn expression this gate cannot follow reads CLOSED, exactly as an unclassified field, key or input does: it may name anything at all, including a value some earlier step invented.",
				s.Label(), pos.phrase, name, raw, got.ref, err)
			return "", false
		}
		if src != want.ref {
			g.bad("%s %s and is handed `%s: %s`, which names %s where %s belongs.\nit has to BE: %s\nit is:       %s\nThis position's act is performed on %s, and naming a different output performs it on a different object while the invocation reads exactly as it should.",
				s.Label(), pos.phrase, name, raw, src, want.ref, formSketch(form), sketchOf(parts), form.what)
			return "", false
		}
		traced = append(traced, fmt.Sprintf("${{ %s }} -> %s", got.ref, src))
	}
	return fmt.Sprintf("%s %s `%s` = %s\n        %s (%s)", s.Label(), where, name, raw, strings.Join(traced, ", "), form.what), true
}

// checkPlanProducesEverythingAReleaseStepIsHeldTo closes the other end of the trace. The
// references above are resolved against the workflow's own outputs graph, which says a value
// EXISTS; only the executed planning logic says it was actually written. An output a role's or
// an act's object is built from and that the plan never writes is an empty string at release
// time.
func (g *gate) checkPlanProducesEverythingAReleaseStepIsHeldTo(roles *Roles, p *planned) {
	plan := roles.step("plan")
	jp, ok := p.jobs[plan.JobID]
	if !ok {
		g.bad("%s is in job %q, which %s does not plan, so nothing produced the outputs every other release step's object is built from", plan.Label(), plan.JobID, p.shape.label)
		return
	}
	var named []string
	for _, e := range heldSources() {
		form, ok := heldAs[e]
		if !ok {
			continue
		}
		for _, part := range form.parts {
			key, isPlanOutput := strings.CutPrefix(part.ref, srcPlanOutput)
			if !isPlanOutput {
				continue
			}
			if jp.outs[key] == "" {
				g.bad("a release step's object is built from `%s`, and on %s the planning logic wrote no `%s` output (it wrote %s).\nEvery reference this release pushes, re-smokes, promotes, resolves and cuts a release at is built from those outputs, so an unwritten one is an EMPTY string in the middle of a release. %s must write it to $GITHUB_OUTPUT",
					part.ref, p.shape.label, key, outputsOf(p), plan.Label())
				continue
			}
			named = unique(append(named, key))
		}
	}
	if len(named) > 0 && !g.failed {
		g.note("the planning logic really writes every output a release step's object is traced to (%s), so no traced reference is an empty string at release time", strings.Join(named, ", "))
	}
}

// --- reading a value ----------------------------------------------------------------------

// rawEnvValue is the RAW scalar a name carries, before any interpolation, at the level that
// actually decides it: the step's own `env:` wins over the job's, which wins over the
// workflow's. Raw, because the question here is what the value IS, not what it evaluates to.
func rawEnvValue(wf *Workflow, job Job, s Step, name string) (raw, where string, present bool) {
	for _, src := range []struct {
		where string
		m     map[string]any
	}{
		{" (its own)", s.Env},
		{" (job " + job.ID + "'s)", job.Env},
		{" (the workflow's)", wf.Env},
	} {
		if v, ok := src.m[name]; ok {
			return yamlString(v), src.where, true
		}
	}
	return "", "", false
}

// splitTemplate breaks a scalar into the alternating literal and `${{ … }}` parts it is made
// of. Empty literal segments are dropped, so `${{ a }}${{ b }}` is two references and not
// three parts with an empty one between them.
func splitTemplate(s string) ([]heldPart, error) {
	var out []heldPart
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			if s != "" {
				out = append(out, heldPart{lit: s})
			}
			return out, nil
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unterminated ${{ … }} in %q", s)
		}
		if i > 0 {
			out = append(out, heldPart{lit: s[:i]})
		}
		body := strings.TrimSpace(s[i+3 : i+j])
		if body == "" {
			return nil, fmt.Errorf("an empty ${{ }} in %q", s)
		}
		out = append(out, heldPart{ref: body})
		s = s[i+j+2:]
	}
}

// traceRef answers WHERE one expression's value comes from, as a canonical source id, by
// following the workflow's own data flow. It is deliberately narrow: a bare context path and
// nothing else. A function call, an operator, a string literal or a path shape it has not been
// taught reds by name - an expression this gate cannot follow may name anything at all.
func traceRef(wf *Workflow, plan Step, jobID, expr string, depth int) (string, error) {
	if depth > 4 {
		return "", fmt.Errorf("`%s` is reached through more than four job outputs; a chain this long is not a reference to the planning logic, it is a place to hide one", expr)
	}
	toks, err := lex(expr)
	if err != nil {
		return "", fmt.Errorf("it cannot be parsed (%v)", err)
	}
	if len(toks) != 2 || toks[0].kind != tokIdent {
		return "", fmt.Errorf("it is not a bare context path. A role step's object must NAME a value - `needs.<job>.outputs.<key>`, `steps.<plan>.outputs.<key>` or `github.<field>` - so that this gate can follow it. An expression that computes one cannot be followed, and this gate does not evaluate a value in order to accept it")
	}
	path := strings.Split(toks[0].text, ".")
	switch {
	case len(path) == 2 && path[0] == "github":
		return srcGithub + path[1], nil

	case len(path) == 4 && path[0] == "steps" && path[2] == "outputs":
		if jobID != plan.JobID || path[1] != plan.ID {
			return "", fmt.Errorf("`%s` names step %q's output, and the step holding the `plan` role is %s. Only the planning logic's outputs are traceable: another step's are a value this gate never saw produced", expr, path[1], plan.Label())
		}
		return srcPlanOutput + path[3], nil

	case len(path) == 4 && path[0] == "needs" && path[2] == "outputs":
		producer, ok := wf.Jobs[path[1]]
		if !ok {
			return "", fmt.Errorf("`%s` names job %q, which %s does not define", expr, path[1], releaseWorkflow)
		}
		needs, err := wf.Jobs[jobID].NeedsOf()
		if err != nil {
			return "", err
		}
		if !hasField(needs, path[1]) {
			return "", fmt.Errorf("`%s` reads job %q's output, and job %q does not `needs:` it. GitHub gives a job the outputs of the jobs it needs and NOTHING else, so this is the empty string at release time - not an error, just an act performed on nothing", expr, path[1], jobID)
		}
		out, ok := producer.Outputs[path[3]]
		if !ok {
			return "", fmt.Errorf("`%s` reads job %q's output %q, and that job declares no such output, so it is the empty string at release time", expr, path[1], path[3])
		}
		parts, err := splitTemplate(out)
		if err != nil {
			return "", err
		}
		if len(parts) != 1 || parts[0].ref == "" {
			return "", fmt.Errorf("`%s` reads job %q's output %q, which is %q - not a single expression, so what it carries is decided by text this gate would have to read rather than follow", expr, path[1], path[3], out)
		}
		return traceRef(wf, plan, path[1], parts[0].ref, depth+1)

	default:
		return "", fmt.Errorf("`%s` is a context path this gate has not been taught to follow. It knows `github.<field>`, `steps.<plan>.outputs.<key>` and `needs.<job>.outputs.<key>`; anything else - an `env.`, an `inputs.`, a `vars.` - is a value that could have been put there by something this gate never looked at", expr)
	}
}

// --- how a value and a required form are printed ------------------------------------------

func formSketch(f heldForm) string {
	var sb strings.Builder
	for _, p := range f.parts {
		if p.ref != "" {
			sb.WriteString("<" + p.ref + ">")
			continue
		}
		sb.WriteString(p.lit)
	}
	return sb.String()
}

func sketchOf(parts []heldPart) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.ref != "" {
			sb.WriteString("${{ " + p.ref + " }}")
			continue
		}
		sb.WriteString("[literal " + p.lit + "]")
	}
	return sb.String()
}

// composePinnedTag reads the tag the example deployment PINS. It is the only value in this
// file held against a committed file rather than traced, because it is the only one a release
// declares rather than derives.
func (g *gate) composePinnedTag() (string, error) {
	ref, err := composeImageRef(g.path(composeFile))
	if err != nil {
		return "", err
	}
	tag, ok := refTag(ref)
	if !ok {
		return "", fmt.Errorf("the example deployment %s names %q, which carries no tag, so the floating reference a release moves is held against nothing", composeFile, ref)
	}
	return tag, nil
}

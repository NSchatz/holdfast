package main

// THE VERSION A RELEASE PUBLISHES IS THE TAG THE RUN WAS TRIGGERED ON - decided by holding
// the planning logic's OUTPUT to its INPUT, never by comparing that output with a sample.
//
// handed.go and acts.go answer "what object does a release step perform its act on?": every
// value a role step and every value an irreversible act hands its program must BE the planning
// logic's own output, traced through the `needs:` graph, so no literal can sit where a derived
// value belongs. What nothing then held was that OUTPUT. The trace ends at
// `steps.plan.outputs.version` and stops; the gate never asked what the planning logic PUT
// there.
//
// So a plan that pins the published version to a literal is invisible to every other assertion
// in this gate. The reference the push publishes, the reference the re-smoke pulls back out of
// the registry, the reference `:latest` is promoted onto, the version A11's resolution compares
// and the version the release notes carry all name `${{ needs.build.outputs.version }}` -
// structurally perfect, every one of them, and every one of them names whatever the plan
// decided. The order holds. The grant holds. The runbook names every act. Not one line of the
// workflow's YAML has to change (S0046 F27).
//
// AND THE LITERAL THAT MAKES IT INVISIBLE IS THE ONE A MAINTAINER WOULD ACTUALLY WRITE. This
// gate plans its real-release shape with `sampleTag`, and sampleTag is `v0.1.0` - not an
// arbitrary string but the version this repository has ACTUALLY PUBLISHED, named throughout
// docs/release.md, CLAUDE.md and README.md, and the one a maintainer copies out of a green
// run's log. A plan pinned to it produces, on the one release tag this gate names, exactly the
// outputs this gate expects to see. Every later tag would then republish the July release and
// move `:latest` onto it - and "once a versioned package has been released, the contents of
// that version MUST NOT be modified" (semver.org clause 3), so no later release can take it
// back.
//
// THE ANSWER IS NOT ANOTHER SAMPLE. Asking "is the version equal to v0.1.0?" is the shape that
// lost eight times over on the other half of this gate: deciding whether a value is right by
// looking at the value. Widening it to two samples, or ten, only moves the coincidence, and a
// fixed LIST of tags is a lookup table waiting to be written into the planning script.
//
// SO THE TAGS ARE DRAWN, NOT NAMED. This check triggers a release on tags it draws for this
// run - pairwise distinct by construction, and never one of the tags this gate names anywhere
// else - and requires the version the planning logic produces for each to BE the tag that
// produced it. The right-hand side of that comparison is the run's own trigger rather than a
// sample: it changes with every witness, so no fixed value satisfies it more than once, and two
// distinct witnesses refuse EVERY constant with certainty rather than with probability. Drawing
// them rather than listing them is what refuses the lookup table as well, because a planning
// script cannot special-case a tag nobody has chosen yet.
//
// WHAT IT DOES NOT DO IS READ THE PLANNING SCRIPT. The version is observed by RUNNING that
// script - the one step this gate executes, in the sandbox shell.go describes - exactly as
// `publish` and the major-zero refusal already are. There is no reader here and there will not
// be one.
//
// AN UNDECIDED TAG IS NOT A PASSED TAG. If the planning logic refuses one of the drawn tags,
// invokes a program for it, publishes nothing for it, or writes no version for it, this check
// has learned nothing about that tag - and a hold that quietly skips the tags it could not
// decide is a hold over whichever tags happen to be left. Every one of those reds, naming the
// tag and what the planning logic did, and the satisfied note is not printed at all.

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

const (
	// How many plain version tags a release is triggered on. TWO at least, and that is the
	// load-bearing number rather than a taste: a constant version IS the trigger tag for at
	// most one tag, so two pairwise-distinct witnesses refuse every constant with certainty.
	// Randomness is what additionally refuses a lookup table; it is not what refuses the
	// constant.
	plainTriggerWitnesses = 2
	// A pre-release is a supported release shape and derives its version from the ref too, so
	// the hold covers it rather than stopping at the shapes that promote.
	preReleaseTriggerWitnesses = 1

	// The bounds a drawn tag's numbers come from. `v0.<0..999>.<0..999>`, with `-rc<1..100>`
	// when a pre-release is wanted: the shape release.yml's own version check accepts.
	triggerMinorBound = 1000
	triggerPatchBound = 1000
	triggerRCBound    = 100

	// A draw that keeps landing on a tag already drawn is a random source that is not one.
	triggerDrawCeiling = 64
)

// whyTheVersionIsTheTag is the harm, stated once and carried by every refusal here, so a
// maintainer fixes it from the message rather than from this file.
const whyTheVersionIsTheTag = "The version a release publishes is the tag the run was triggered on, and it is not one label among several: it is the image tag this run pushes, the reference the re-smoke pulls back out of the registry, the reference `:latest` is promoted onto, the name of the GitHub release that is cut and the version stamped into every binary in the tarballs. A version that is not the trigger tag republishes some OTHER release's number - and \"once a versioned package has been released, the contents of that version MUST NOT be modified\" (semver.org clause 3), so it is not a mistake the next release can correct."

// triggerWitness is one tag a release is triggered on so that the version it would publish can
// be observed. It is a WITNESS and not a sample: nothing is ever compared against it. The
// planning logic's output is compared against the tag that produced that output.
type triggerWitness struct {
	tag  string
	what string // how the shape is described in every message about it
}

// checkPublishedVersionIsTheTriggerTag holds the planning logic's own `version` output to the
// tag the run was triggered on, over tags drawn for this run.
func (g *gate) checkPublishedVersionIsTheTriggerTag(r *Runner, wf *Workflow, roles *Roles, repo string) {
	plan := roles.step("plan")

	witnesses, err := drawTriggerWitnesses()
	if err != nil {
		g.bad("this gate cannot draw the tags it triggers a release on, so it cannot decide whether the version a release publishes is the tag the run was triggered on: %v\n%s\nThe tags are DRAWN rather than listed on purpose - a fixed list is a lookup table waiting to be written into the planning script - so an unusable random source is a refusal here, never a fall back to naming them.",
			err, whyTheVersionIsTheTag)
		return
	}

	held := make([]string, 0, len(witnesses))
	for _, w := range witnesses {
		version, decided := g.versionARunWouldPublish(r, wf, roles, repo, plan, w)
		if !decided {
			continue // versionARunWouldPublish has already said which tag, and what it did
		}
		if version != w.tag {
			g.bad("%s was triggered on the tag %s and the version that release would publish is %q - NOT the tag it was triggered on.\n%s\nThis gate does not compare that version with a sample. It triggers a release on %d pairwise-distinct tags it DREW for this run (%s) and holds the planning logic's output to the tag that produced it, so a version the plan decides for itself reds however it is spelled - including `%s`, the version this repository has actually published and the one this gate's own sample names, which is the single spelling every other assertion here is blind to. Derive the version from the ref the run was triggered on.",
				plan.Label(), w.tag, version, whyTheVersionIsTheTag, len(witnesses), strings.Join(tagsOf(witnesses), ", "), sampleTag)
			continue
		}
		held = append(held, w.tag)
	}

	// A note printed over tags this gate could not decide would be the silent green the whole
	// design refuses. Every witness held, or nothing is claimed.
	if len(held) != len(witnesses) {
		return
	}
	g.note("the version a release publishes is the tag the run was triggered on: %s was triggered on %d pairwise-distinct tags DRAWN for this run (%s), and each time it produced exactly the tag that triggered it\n      No fixed version can do that - a constant is the trigger tag for at most one of them - so a plan that decides the published version for itself reds here whatever it spells, including `%s`: the version this repository has actually published, the tag this gate's own sample names, and the one spelling every other assertion in this gate is blind to. Nothing here is compared with a sample; the OUTPUT is held to the INPUT that produced it, and the tags are drawn rather than listed so that no planning script can special-case them",
		plan.Label(), len(held), strings.Join(held, ", "), sampleTag)
}

// versionARunWouldPublish triggers a release on one drawn tag and reports the version the
// planning logic said that release publishes. Every way it can fail to learn that is a refusal
// naming the tag and what the planning logic did, never a silent skip.
func (g *gate) versionARunWouldPublish(r *Runner, wf *Workflow, roles *Roles, repo string, plan Step, w triggerWitness) (string, bool) {
	undecided := func(what string, a ...any) {
		g.bad("this gate triggered a release on the tag %s and CANNOT DECIDE what version that release would publish: %s\n%s\nA tag this gate cannot decide must not be passed over: this hold is made over tags it DRAWS rather than over one it names, so a draw it silently skipped would leave the version held over whichever tags happened to be left, which is the vacuous green this whole gate exists to refuse. %s is an ordinary `v0.MINOR.PATCH` tag of exactly the shape this release path accepts.",
			w.tag, fmt.Sprintf(what, a...), whyTheVersionIsTheTag, w.tag)
	}

	p, err := g.plan(r, wf, roles, repo, shape{w.what, "push", w.tag})
	if err != nil {
		undecided("%v", err)
		return "", false
	}
	if p.failed {
		undecided("%s EXITED %d for it. It said:\n%s", plan.Label(), p.exitCode, indent(p.output))
		return "", false
	}
	jp, ok := p.jobs[plan.JobID]
	if !ok {
		undecided("job %q, which carries %s, does not run for it, so nothing planned that release at all", plan.JobID, plan.Label())
		return "", false
	}
	if publish := jp.outs["publish"]; publish != "true" {
		undecided("the planning logic wrote `publish=%s` for it, so a push of that tag publishes nothing and there is no published version to hold. Every OTHER tag shape this gate plans publishes; a release path that publishes only the tags this gate happens to name is one whose published version nothing holds", publish)
		return "", false
	}
	version, written := jp.outs["version"]
	if !written || version == "" {
		undecided("the planning logic wrote NO `version` output for it (it wrote %s), so the reference this run pushes, re-smokes, promotes and resolves is the empty string", outputsOf(p))
		return "", false
	}
	return version, true
}

// --- drawing the tags ---------------------------------------------------------------------

// drawTriggerWitnesses draws the tags this run holds the planning logic to. They are pairwise
// distinct BY CONSTRUCTION - that is what refuses every constant version with certainty rather
// than with probability - and none of them is a tag this gate names anywhere else, so the one
// coincidence that made F27 invisible cannot be drawn back into the question by accident.
func drawTriggerWitnesses() ([]triggerWitness, error) {
	taken := map[string]bool{sampleTag: true, samplePreTag: true, sampleMajorTag: true}
	out := make([]triggerWitness, 0, plainTriggerWitnesses+preReleaseTriggerWitnesses)

	draw := func(preRelease bool) error {
		for attempt := 0; attempt < triggerDrawCeiling; attempt++ {
			tag, err := drawVersionTag(preRelease)
			if err != nil {
				return err
			}
			if taken[tag] {
				continue
			}
			taken[tag] = true
			what := "a version tag push drawn for this run (" + tag + ")"
			if preRelease {
				what = "a pre-release tag push drawn for this run (" + tag + ")"
			}
			out = append(out, triggerWitness{tag: tag, what: what})
			return nil
		}
		return fmt.Errorf("%d draws in a row landed on a tag that had already been drawn, which a working random source does not do", triggerDrawCeiling)
	}

	for i := 0; i < plainTriggerWitnesses; i++ {
		if err := draw(false); err != nil {
			return nil, err
		}
	}
	for i := 0; i < preReleaseTriggerWitnesses; i++ {
		if err := draw(true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// drawVersionTag draws one tag of the shape this release path accepts: `v0.MINOR.PATCH`, with a
// pre-release suffix when asked. The major is zero because a non-zero major is REFUSED by the
// planning logic (A4/A10), and a tag the release path rejects would tell this check nothing
// about the version a release publishes.
func drawVersionTag(preRelease bool) (string, error) {
	minor, err := drawBelow(triggerMinorBound)
	if err != nil {
		return "", err
	}
	patch, err := drawBelow(triggerPatchBound)
	if err != nil {
		return "", err
	}
	tag := fmt.Sprintf("v0.%d.%d", minor, patch)
	if preRelease {
		rc, err := drawBelow(triggerRCBound)
		if err != nil {
			return "", err
		}
		tag += fmt.Sprintf("-rc%d", rc+1)
	}
	return tag, nil
}

// drawBelow reads the system's own random source rather than a seeded generator: a gate whose
// witnesses a reader can predict is a gate whose witnesses a planning script can name.
func drawBelow(n int64) (int64, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, fmt.Errorf("the system random source failed (%w)", err)
	}
	return v.Int64(), nil
}

func tagsOf(ws []triggerWitness) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.tag)
	}
	return out
}

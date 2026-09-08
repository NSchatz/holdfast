package main

// AND THE ACTS THAT HOLD NO ROLE? - the same question as handed.go, asked at the boundary
// that actually decides which steps are dangerous: the GRANT.
//
// roles.go declares the eight named positions a release has and accounts for each one's
// invocation; handed.go holds the values those invocations are handed. Both are keyed on the
// ROLE TABLE, and the role table is not the inventory of irreversible acts. The inventory is
// `checkRunbookNamesEveryAct`'s: every step in a job that holds a publishing grant, whatever
// it says it does. Eleven steps are in that job today and eight of them hold no role, so for
// eight of them nothing looked at the values their `env:` hands their programs at all.
//
// `github-release` is the one where that is not academic. It cuts the GitHub release with
// `gh`, and its notes tell every reader which container image this release published. Pin
// `IMAGE` to a literal and every later release names that image in its notes - the July
// artefact, for ever - while the order holds, the grant holds, the runbook names the act and
// every other assertion in this gate stays green (S0046 F28). That is the identical harm
// `REF: ${IMAGE}:latest` did to the re-smoke, one step further down the job.
//
// SO THE HOLD IS EXTENDED TO THE ACTS, NOT THE ROLE TABLE. An act is not a role: this file
// does NOT account for its invocation, because the release cut is a multi-line script and a
// role's whole-value comparison exists precisely so that no script has to be searched. What
// it does is the half that can be decided from structured YAML - the same trace, through the
// same `needs:` graph, to the same `plan` step - and it is DENY BY DEFAULT over the whole
// environment in scope for the act, so a name nobody classified reds by name rather than
// being read as harmless.
//
// WHY NOT MAKE IT A ROLE INSTEAD? That was the other route and it costs an edit to the thing
// this gate guards. A role step's `run:` must be ONE line whose first field IS the declared
// program, so the release cut would have to move into a new `scripts/release-cut.sh` - a new
// file, on the one-way-door path, running for real on the next tag push, to close a hole
// that is entirely about a value. This route changes no character of what a release
// EXECUTES. What it does not buy, stated rather than left to be found: the act's invocation
// is not accounted for field by field the way a role's is, so a literal written into the
// release notes text inside that `run:` script is still invisible here - that is the
// standing residue of "the gate does not read a step's text", not a new one, and
// docs/release.md carries it.
//
// THE CREDENTIAL IS NOT AN OBJECT. `GH_TOKEN` is what authorises the act rather than what
// the act is performed on, so it is not traced to a planning output - there is none to trace
// it to. It is held to the ONE credential this gate's capability model bounds:
// `secrets.GITHUB_TOKEN`, whose scope IS the `permissions:` block capability.go reads. A
// repository secret put there instead is a credential whose scope nothing in this file
// bounds, and it would sit inside the one job where holding a capability is expected, so
// nothing else here would notice it.

import (
	"fmt"
	"strings"
)

// act is one irreversible act that holds no ROLE: a step in the job that carries the grant,
// identified by its own declared `id:`, exactly as the operator runbook identifies it.
type act struct {
	id   string
	what string // how the act is described in every message

	// handsEnv are the environment names this act's own program reads, and the value each
	// must carry. Same rule and same trace as a role's handsEnv: the value must BE the
	// planning logic's own output, not equal one.
	handsEnv map[string]envSpec
}

// releaseActs is every irreversible act this gate holds values for and that holds no role.
// A step in a capability-bearing job that is NOT here still has every environment name in
// scope for it classified (checkActsHandedValues) - it simply hands its program no object
// this gate has been told about.
var releaseActs = []act{
	{
		id:   "github-release",
		what: "the GitHub release this run cuts, its notes and the tarballs it serves",
		handsEnv: map[string]envSpec{
			// THE ONE THIS EXISTS FOR. The notes tell every reader of the release which
			// image reference to pull, so a literal here publishes release notes naming an
			// image this run never gated - not once, but on every release after the edit.
			"IMAGE": {heldPlannedImage, "the image reference this release's notes tell a reader to pull"},
			// The release is CREATED at this value: `gh release create "$VERSION"`. A
			// literal cuts every later release at the same tag, and a released version's
			// contents must never be modified (semver.org, this repository's own authority
			// for its version scheme).
			"VERSION": {heldPlannedVersion, "the tag this release is cut at, and its title"},
			// It decides `--prerelease`, which is what keeps a release candidate out of
			// everyone's "latest release". Pinned to `false`, a pre-release is published as
			// an ordinary one; pinned to `true`, no release ever stops being a draft-shaped
			// one. Either way the planning logic's own decision stops reaching the act.
			"PRERELEASE": {heldPlannedPrerelease, "whether this release is marked a pre-release"},
		},
	},
}

// actEnvCredentials are environment names on an act that carry a CREDENTIAL rather than an
// object the act is performed on. They are not traced - the planning logic produces no
// credential - and they are not inert either, so they get their own treatment: the value must
// be the scoped token, the one credential `permissions:` bounds.
var actEnvCredentials = map[string]string{
	"GH_TOKEN": "the token `gh` authenticates to GitHub with",
}

// scopedTokenRefs are the two spellings of that one credential. `github.token` IS
// `secrets.GITHUB_TOKEN` (capability.go matches them together for the same reason).
var scopedTokenRefs = map[string]bool{
	"secrets.GITHUB_TOKEN": true,
	"github.token":         true,
}

func actByID(id string) (act, bool) {
	for _, a := range releaseActs {
		if a.id == id {
			return a, true
		}
	}
	return act{}, false
}

func actPosition(a act) heldPosition {
	return heldPosition{id: a.id, what: a.what, phrase: fmt.Sprintf("is the irreversible act `%s` (%s)", a.id, a.what)}
}

// checkActsHandedValues holds the values every irreversible act OUTSIDE the role table hands
// its program, and refuses an environment name in scope for one that nobody has classified.
//
// The inventory is the same one A8 uses - every step in a job that holds a publishing grant,
// identified by its declared `id:` - so an act cannot leave this check by being spelled
// unrecognisably, and a NEW step in that job arrives here as well as in the runbook.
func (g *gate) checkActsHandedValues(wf *Workflow, roles *Roles) {
	plan := roles.step("plan")
	pinned, pinErr := g.composePinnedTag()

	holdsARole := map[string]bool{}
	for _, r := range releaseRoles {
		s := roles.step(r.id)
		holdsARole[stepKey(s)] = true
	}

	found := map[string]bool{}
	var held []string
	for _, jid := range wf.JobIDs() {
		job := wf.Jobs[jid]
		problems, err := CanPublish(wf, job)
		if err != nil || len(problems) == 0 {
			// A job that can publish nothing carries no irreversible act. An undecidable
			// one is already refused, by name, wherever its decidability is the question.
			continue
		}
		// The workflow's and the job's `env:` are in scope for every step here and carry
		// the same value for each, so they are decided once per job rather than once per
		// step - a wall of the same refusal reads as many problems instead of one.
		g.classifyEnvInScopeOfActs(wf.Env, "the workflow's top-level `env:`", jid)
		g.classifyEnvInScopeOfActs(job.Env, fmt.Sprintf("job %q's `env:`", jid), jid)

		for _, s := range job.Steps {
			if holdsARole[stepKey(s)] {
				continue // a role step's names are accounted for by roles.go's checkEnv
			}
			a, declared := actByID(s.ID)
			if declared {
				found[s.ID] = true
			}
			for _, k := range sortedKeys(s.Env) {
				if _, ok := a.handsEnv[k]; ok {
					continue
				}
				if _, ok := envCheckedAndInertForReleaseSteps[k]; ok {
					continue
				}
				if what, ok := actEnvCredentials[k]; ok {
					g.holdCredential(s, k, yamlString(s.Env[k]), what)
					continue
				}
				g.bad("%s sets `%s` in its own `env:`, and this gate has not classified it. That step is an irreversible act: job %q holds a publishing grant, so every step in it counts, whatever it says it does.\nAn environment name in scope for an act reads CLOSED, the same way one in scope for a role step does: it is how the act's own program is told WHICH object to perform it on, and a name nobody decides is a value the step spells for itself. Declare it in that act's handsEnv (acts.go) with the planning output it must name, in actEnvCredentials if it carries a credential, or in envCheckedAndInertForReleaseSteps with the reason it can do neither.",
					s.Label(), k, jid)
			}
			if !declared {
				continue
			}
			pos := actPosition(a)
			for _, name := range sortedEnvNames(a.handsEnv) {
				raw, where, present := rawEnvValue(wf, job, s, name)
				if !present {
					g.badUnset(s, pos, name, a.handsEnv[name])
					continue
				}
				if line, ok := g.holdOne(wf, plan, s, pos, name, "`env:`"+where, raw, a.handsEnv[name], pinned, pinErr); ok {
					held = append(held, line)
				}
			}
		}
	}

	// A declaration that applies to nothing is the vacuous pass this gate exists to refuse:
	// rename the step, move it out of the job that holds the grant, or delete it, and every
	// hold above silently stops being asked.
	for _, a := range releaseActs {
		if found[a.id] {
			continue
		}
		g.bad("no step in a job holding a publishing grant declares `id: %s`, and this gate holds the values %s is handed.\nEvery hold declared for that act would then be asked of nothing at all, which is a pass over an empty set rather than a gate. Either the act was renamed - in which case rename it in releaseActs (acts.go) and in %s too - or it left the job that holds the grant, in which case it can no longer perform the act at all.",
			a.id, a.what, runbookFile)
	}

	if len(held) > 0 {
		g.note("every value an irreversible act outside the role table hands its program IS the planning logic's own output, traced the same way:\n      %s", strings.Join(held, "\n      "))
	}
}

// classifyEnvInScopeOfActs is the deny-by-default half at the two levels that are shared by
// every step of the job. It reports at most once per name per job, because the value is the
// same one for all of them.
func (g *gate) classifyEnvInScopeOfActs(env map[string]any, where, jobID string) {
	for _, k := range sortedKeys(env) {
		if _, ok := envCheckedAndInertForReleaseSteps[k]; ok {
			continue
		}
		if _, ok := actEnvCredentials[k]; ok {
			// A credential set for the whole job or workflow is in scope for steps that are
			// not acts at all, and this gate does not model those. It is refused here rather
			// than held: the act declares its own.
			g.bad("%s sets `%s`, which carries a credential, and job %q holds a publishing grant.\nA credential declared at that level is in scope for every step of the job, including ones this gate holds no values for. Declare it on the step that uses it, where it is held against the one credential `permissions:` bounds.",
				where, k, jobID)
			continue
		}
		g.bad("%s sets `%s`, which this gate has not classified, and job %q holds a publishing grant.\nAn environment name in scope for an irreversible act reads CLOSED: it is in scope for every step of the job that carries every one-way door in this release. Classify it in envCheckedAndInertForReleaseSteps with the reason it cannot change which object an act is performed on, or set it on the step that reads it and declare it there.",
			where, k, jobID)
	}
}

// holdCredential decides an environment name that carries a credential. There is no trace to
// run: what it must BE is the scoped token, because that is the only credential whose scope
// the `permissions:` block capability.go reads actually bounds.
func (g *gate) holdCredential(s Step, name, raw, what string) {
	parts, err := splitTemplate(raw)
	if err != nil {
		g.bad("%s sets `%s`, which is %s, and it cannot be read: %v", s.Label(), name, what, err)
		return
	}
	if len(parts) != 1 || parts[0].ref == "" || !scopedTokenRefs[strings.TrimSpace(parts[0].ref)] {
		g.bad("%s sets `%s: %s`, and `%s` is %s.\nIt has to BE the scoped token - `${{ secrets.%s }}` or `${{ github.token }}` - and nothing else. That token's scope IS the `permissions:` block this gate already reads, so a read-only grant makes it useless for a published act; ANY other secret is a value whose scope nothing here bounds, sitting in the one job where holding a capability is expected, so no other assertion in this gate would notice it.",
			s.Label(), name, raw, name, what, scopedToken)
	}
}

// stepKey identifies one step within the workflow, for the "does this step hold a role?"
// question. A step's id is not enough: two jobs may each declare one.
func stepKey(s Step) string { return fmt.Sprintf("%s\x00%d", s.JobID, s.Index) }

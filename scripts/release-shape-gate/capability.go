package main

// CAN this job publish? - the question that replaced "does this text publish?".
//
// Six ordinals of adversarial review settled that the second question cannot be answered.
// A `run:` step is arbitrary shell, and bash decides what a word means with quoting,
// expansion, `eval`, command substitution, nested shells and the contents of a variable, so
// no reader decides it from text (F1, F5, F7, F9, F11, F12) and no observer decides it from
// inside the same shell either, because every control such an observer has - a recording
// function, a DEBUG trap, an emptied PATH - is an ordinary shell object the step being
// observed owns and can undo in one line (F14, F15).
//
// This file asks the decidable question instead. A step publishes nothing it holds no
// credential for. GitHub decides what a job's token may do from `permissions:`, and decides
// what other credentials a job holds from `secrets:` references - both structured YAML, no
// shell anywhere in the question. So a job granted no write scope and handed no secret
// beyond the scoped GITHUB_TOKEN may contain `docker push` in any spelling, quoting or
// nesting at all, and publish nothing. That is the property, and it is the one the gate
// asserts about every job that runs on a manual dispatch (A6, A12).
//
// DENY BY DEFAULT is what makes it a gate rather than a list. An absent `permissions:`
// block reads CLOSED and reds - the repository default may be write-all and this file
// cannot see it. A scope name outside GitHub's vocabulary reds. A value outside
// read/write/none reds. A job key this gate has not classified reds by name. Silence is
// unreachable, which is the single sentence every earlier hole was an instance of.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// permissionScopes is GitHub's own vocabulary for `permissions:`. Each entry says what a
// WRITE on it would authorise, because the message a gate prints is the whole difference
// between a refusal somebody fixes and a refusal somebody deletes.
//
// The list is deliberately complete rather than "the ones that can publish": a scope this
// gate does not know is a scope nobody classified, and the rule below is that no scope may
// be write on the dispatch path at all. Knowing the vocabulary is how an unknown scope -
// a typo, or a scope GitHub added since - reds by name instead of being read as harmless.
var permissionScopes = map[string]string{
	"actions":             "re-run, cancel and delete workflow runs and artifacts",
	"attestations":        "publish build attestations, which reach a public transparency log",
	"checks":              "create and update check runs",
	"contents":            "push refs and tags, and create releases",
	"deployments":         "create deployments",
	"discussions":         "write repository discussions",
	"id-token":            "mint an OIDC token, which an external registry or cloud will accept as a credential",
	"issues":              "write issues",
	"models":              "use GitHub Models",
	"packages":            "push packages and container images to GHCR",
	"pages":               "publish GitHub Pages",
	"pull-requests":       "write pull requests",
	"repository-projects": "write repository projects",
	"security-events":     "write code-scanning results",
	"statuses":            "write commit statuses",
}

var permissionValues = map[string]bool{"read": true, "write": true, "none": true}

// Grants is one job's effective `permissions:`, and whether it was stated at all.
type Grants struct {
	Where  string            // where the block was read from, for the message
	Scopes map[string]string // scope -> read|write|none
	All    string            // "read-all" / "write-all" when the shorthand was used
}

// Writes lists the scopes this grant would let a step write, sorted.
func (g Grants) Writes() []string {
	if g.All == "write-all" {
		out := make([]string, 0, len(permissionScopes))
		for s := range permissionScopes {
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}
	var out []string
	for s, v := range g.Scopes {
		if v == "write" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (g Grants) String() string {
	if g.All != "" {
		return g.All
	}
	if len(g.Scopes) == 0 {
		return "{} (nothing)"
	}
	keys := make([]string, 0, len(g.Scopes))
	for k := range g.Scopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+g.Scopes[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// EffectiveGrants reads what a job's token may do. A job's own `permissions:` replaces the
// workflow's outright (GitHub does not merge them); with neither present the REPOSITORY
// default applies, which is not in this file and may be write-all, so that case is an
// error rather than an assumed "none".
func EffectiveGrants(wf *Workflow, job Job) (*Grants, error) {
	switch {
	case job.HasPermissions:
		return parseGrants(job.Permissions, fmt.Sprintf("job %q", job.ID))
	case wf.HasPermissions:
		return parseGrants(wf.Permissions, "the workflow's top-level `permissions:`")
	default:
		return nil, fmt.Errorf("job %q declares no `permissions:` and neither does the workflow, so the token it runs with is whatever the REPOSITORY default is - a setting this file does not contain and which may be write-all. An unstated grant reads CLOSED: declare `permissions:` on the job (or on the workflow) so what this job may write is a fact in the definition rather than a setting somebody has to go and look up", job.ID)
	}
}

func parseGrants(raw any, where string) (*Grants, error) {
	g := &Grants{Where: where, Scopes: map[string]string{}}
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s != "read-all" && s != "write-all" {
			return nil, fmt.Errorf("%s sets `permissions: %s`, which is not one of GitHub's shorthands (`read-all`, `write-all`) nor a scope map. This gate refuses a grant it cannot read rather than assuming it grants nothing", where, s)
		}
		g.All = s
		return g, nil
	case map[string]any:
		for _, k := range sortedKeys(v) {
			if _, known := permissionScopes[k]; !known {
				return nil, fmt.Errorf("%s grants %q, which is not one of the permission scopes this gate knows. An unrecognised scope reads CLOSED: it may be a scope GitHub added (in which case classify it in permissionScopes, with what a write on it would authorise) or a typo (in which case the grant somebody meant to make is not being made). Known scopes: %s", where, k, strings.Join(sortedKeys(scopesAsAny()), ", "))
			}
			val := strings.TrimSpace(yamlString(v[k]))
			if !permissionValues[val] {
				return nil, fmt.Errorf("%s grants `%s: %s`, which is not one of read / write / none. A grant this gate cannot read is not a grant it may treat as harmless", where, k, val)
			}
			g.Scopes[k] = val
		}
		return g, nil
	case nil:
		// `permissions:` with an empty value is GitHub's "grant nothing".
		return g, nil
	default:
		return nil, fmt.Errorf("%s sets a `permissions:` of a shape this gate cannot read (%T)", where, raw)
	}
}

func scopesAsAny() map[string]any {
	out := map[string]any{}
	for k := range permissionScopes {
		out[k] = nil
	}
	return out
}

// --- what else can carry a capability ---------------------------------------------------

// jobKeysCheckedAndCapabilityFree are the job-level keys a human has read and found unable
// to hand a job a credential. A key outside this map is not assumed harmless: on the
// dispatch path it reds by name, so the classification is made deliberately rather than by
// omission. The DANGEROUS ones are listed separately, with what they would hand over.
var jobKeysCheckedAndCapabilityFree = map[string]string{
	"name":              "a label",
	"runs-on":           "which runner executes it; a runner label grants nothing",
	"if":                "a guard, decided here from the planning logic's real values",
	"needs":             "the ordering graph, decided here",
	"permissions":       "the grant itself, read above",
	"env":               "values, scanned for secret references below",
	"outputs":           "values passed to a later job, scanned below",
	"steps":             "the work, each step's keys checked below",
	"timeout-minutes":   "a clock",
	"defaults":          "the shell and working directory a `run:` step gets",
	"strategy":          "matrix/fail-fast; it multiplies the job, it does not widen the grant",
	"concurrency":       "queueing",
	"continue-on-error": "whether a failure fails the run, checked separately for A14",
}

// jobKeysThatCarryCapability are the ones that DO hand a job something, each with what.
var jobKeysThatCarryCapability = map[string]string{
	"environment": "a deployment environment, whose environment SECRETS and variables the job then holds - a credential this file cannot see the value of",
	"container":   "a job container, which may carry `credentials:` for a private registry",
	"services":    "service containers, which may each carry `credentials:` for a private registry",
	"uses":        "a reusable workflow, whose permissions and secrets are delegated at the call site rather than declared here",
	"secrets":     "secrets passed to a called workflow, including `inherit`, which hands over every secret the repository has",
}

// workflowKeysCheckedAndCapabilityFree is the same rule one level UP, and it was the level
// this gate did not have it at: the top-level keys it reasons about were handled one at a
// time and anything else was simply never looked at. Two of the three levels said so when
// they met something new and the third did not, which is an asymmetry nobody reading the
// output could see. GitHub's top-level vocabulary is small and closed, so classifying it is
// cheap; the point is that the NEXT key GitHub adds arrives as a refusal by name rather than
// as silence.
var workflowKeysCheckedAndCapabilityFree = map[string]string{
	"name":        "a label",
	"run-name":    "a label for the run",
	"on":          "the event surface, held to the shapes this gate plans (triggers.go)",
	"permissions": "the default grant every job inherits unless it states its own, read by EffectiveGrants",
	"env":         "values every job inherits; scanned for secret references, and refused for a role step unless classified",
	"defaults":    "the shell and working directory every `run:` step gets - refused outright over a role step",
	"concurrency": "queueing; it decides when a run happens, not what it may do",
	"jobs":        "the work, every job's keys checked above",
}

// stepKeysCheckedAndCapabilityFree, same rule one level down.
var stepKeysCheckedAndCapabilityFree = map[string]string{
	"name":              "a label",
	"id":                "the step's declared identity, which is what roles and act ids are read from",
	"if":                "a guard, decided here",
	"uses":              "which action runs; an action holds only what it is passed",
	"run":               "a shell script - deliberately NOT read, see the file header",
	"with":              "inputs to an action, scanned for secret references below",
	"env":               "values, scanned below",
	"shell":             "which interpreter runs the script",
	"working-directory": "where it runs",
	"continue-on-error": "whether a failure fails the run, checked separately for A14",
	"timeout-minutes":   "a clock",
}

// reSecretRef finds a reference to the secrets context. It is matched against the RAW
// scalar text of every node in the job, so a reference in a key this gate does not model is
// found too. `github.token` is the same token `secrets.GITHUB_TOKEN` names and is matched
// with it.
var reSecretRef = regexp.MustCompile(`\bsecrets\s*\.\s*([A-Za-z_][A-Za-z0-9_-]*)|\bsecrets\s*\[\s*['"]([^'"]+)['"]\s*\]|\bgithub\s*\.\s*(token)\b|\b(secrets)\b`)

// scopedToken is the one credential a job may hold while still being unable to publish:
// GITHUB_TOKEN is bounded by the very `permissions:` block checked above, so a read-only
// grant makes it useless for a published act. Every OTHER secret is a value this file
// cannot see and whose scope `permissions:` does not constrain.
const scopedToken = "GITHUB_TOKEN"

type secretRef struct {
	name  string
	where string
}

// secretsReachedBy lists every credential reference a job can see. That is the job's own
// node AND the workflow's top-level `env:`, which every job inherits: a secret declared
// there reaches a dispatch-path step without appearing anywhere inside the job, so a scan
// that walked only the job node would have to be rescued by something else noticing.
func secretsReachedBy(wf *Workflow, job Job) []secretRef {
	var out []secretRef
	seen := map[string]bool{}
	scan := func(where, value string) {
		for _, m := range reSecretRef.FindAllStringSubmatch(value, -1) {
			name := ""
			switch {
			case m[1] != "":
				name = m[1]
			case m[2] != "":
				name = m[2]
			case m[3] != "":
				name = scopedToken // github.token IS secrets.GITHUB_TOKEN
			default:
				name = "* (the whole secrets context)"
			}
			key := name + "\x00" + where
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, secretRef{name: name, where: where})
		}
	}
	scalars(job.Node, "", scan)
	if wf != nil {
		for _, k := range sortedKeys(wf.Env) {
			scan("the workflow's top-level `env:`."+k, yamlString(wf.Env[k]))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].where < out[j].where
	})
	return out
}

// CapabilityProblem is one reason a job could authorise a published act.
type CapabilityProblem struct {
	Detail string
}

// CanPublish reports every capability a job holds that could authorise an irreversible
// published act. An empty result means a step in this job may say anything it likes and
// still publish nothing.
//
// It is deliberately not "does it look like it publishes". It is "what has it been handed".
func CanPublish(wf *Workflow, job Job) ([]CapabilityProblem, error) {
	var problems []CapabilityProblem

	grants, err := EffectiveGrants(wf, job)
	if err != nil {
		return nil, err
	}
	if writes := grants.Writes(); len(writes) > 0 {
		var parts []string
		for _, s := range writes {
			parts = append(parts, fmt.Sprintf("%s (%s)", s, permissionScopes[s]))
		}
		problems = append(problems, CapabilityProblem{Detail: fmt.Sprintf(
			"%s grants WRITE on %s. A token with any write scope can authorise something that leaves this machine, whatever the steps that hold it say they do",
			grants.Where, strings.Join(parts, ", "))})
	}

	for _, ref := range secretsReachedBy(wf, job) {
		if ref.name == scopedToken {
			continue // bounded by the grant checked immediately above
		}
		problems = append(problems, CapabilityProblem{Detail: fmt.Sprintf(
			"job %q reads `secrets.%s` (at %s). `permissions:` does not bound a repository secret - its scope is whatever was put in it - so a step holding one can publish regardless of what this workflow grants",
			job.ID, ref.name, ref.where)})
	}

	for _, k := range mappingKeys(job.Node) {
		if _, ok := jobKeysCheckedAndCapabilityFree[k]; ok {
			continue
		}
		if why, ok := jobKeysThatCarryCapability[k]; ok {
			problems = append(problems, CapabilityProblem{Detail: fmt.Sprintf(
				"job %q uses `%s:`, which hands it %s", job.ID, k, why)})
			continue
		}
		problems = append(problems, CapabilityProblem{Detail: fmt.Sprintf(
			"job %q uses the job key `%s:`, which this gate has not classified. An unclassified key reads CLOSED rather than harmless: decide what it can hand a job and put it in jobKeysCheckedAndCapabilityFree with the reason, or in jobKeysThatCarryCapability with what it grants",
			job.ID, k)})
	}

	for _, s := range job.Steps {
		for _, k := range mappingKeys(s.Node) {
			if _, ok := stepKeysCheckedAndCapabilityFree[k]; ok {
				continue
			}
			problems = append(problems, CapabilityProblem{Detail: fmt.Sprintf(
				"%s uses the step key `%s:`, which this gate has not classified. An unclassified key reads CLOSED: classify it in stepKeysCheckedAndCapabilityFree with the reason it cannot hand a step a credential",
				s.Label(), k)})
		}
	}

	return problems, nil
}

package main

// WHICH STEP IS THE FULL GATE? - answered from a step's declared identity, never from
// prose in its text, and answered FAIL-CLOSED.
//
// A7 is about an ORDER: the floating reference moves only after the full gate, both
// architectures' smoke runs, the version-tag push and the re-smoke of the artefact pulled
// back from the registry. To check an order you must first know which step is which, and
// the way that was done before was to search a step for a mention of the thing. It cost a
// finding: `echo "make check"` satisfied the full-gate role, so a step that PRINTS the
// gate's name and runs nothing passed A7 (S0046 F13). Every tightening of that search has
// the same shape - it decides what a step does by looking for a substring - and every one
// can be satisfied by a step that mentions the substring and does nothing.
//
// So a role is DECLARED. The step carries an `id:`, the gate names the id, and the two must
// agree about what the step invokes - decided by WHOLE-VALUE equality, not by search:
//
//   - the `run:` is ONE line (a role step that needs a script puts the script in
//     scripts/ and invokes it, which is why release-resmoke.sh and release-promote.sh
//     exist as files);
//   - that line's FIELDS are compared as whole words. The first field must BE the
//     program. `echo ./scripts/smoke-image.sh` has first field `echo`; `"make" check` has
//     first field `"make"`; `printf '%s' "make check"` has first field `printf`. None of
//     them can claim a role, and no quoting, nesting or spelling reaches the comparison,
//     because nothing is being searched for inside anything.
//
// THAT WAS NOT ENOUGH, AND THE REASON IS THE WHOLE RULE BELOW. "The first field is the
// program, and every field the role requires is present" is satisfied by `make -n check`,
// which is GNU make's dry-run mode: it PRINTS every recipe in `check`, executes not one of
// them, and exits 0 (S0046 F18). The invocation names the gate perfectly and runs no gate.
// So do `make -q check`, `make --dry-run check`, `make -Bn check`, `make check SHELL=true`
// (SHELL is a make variable, and overriding it from the command line replaces the
// interpreter of every recipe), `env: MAKEFLAGS: -n` with the `run:` untouched, and
// `shell: cat`, which makes GitHub print the script instead of running it. Enumerating
// those is the mistake this specification has already died of six times, in the other half
// of this gate: a catalogue buys exactly the spellings it names and the next one is already
// written.
//
// DENY BY DEFAULT, then - the same standard capability.go holds for `permissions:` and for
// job and step keys. A role's invocation is accounted for FIELD BY FIELD, and a field the
// role has not classified reds BY NAME. So does an environment name in scope for it that
// nobody classified, a step key that changes what runs or where, a `defaults:` block, and
// an action input outside the ones declared. Nothing is searched for; everything is either
// declared, with the reason it cannot make the invocation do less than it says, or refused.
// The next unseen spelling is a loud stop rather than a silent pass.
//
// AND NAMING AN ENVIRONMENT VARIABLE IS NOT ACCOUNTING FOR IT. That table used to carry a
// name and a sentence about the value's purpose, and nothing ever compared the value with
// anything (S0046 F22, F23). `REF: ${{ … }}:latest` on the re-smoke is one line: the role
// still holds, the invocation is untouched, the order sentence still prints - and the step
// pulls back the PREVIOUS release, which passes, because it was gated last time, while the
// artefact this run pushed is never pulled back at all and `:latest` is then moved onto it.
// `VERSION: latest` on the resolver is the same line again and turns A11 into a comparison
// of the compose reference's digest with its own, which can never fail.
//
// So every name a role declares is HELD: it resolves to a value produced outside the step -
// the planning logic's own outputs, this module's repository, the event shape being planned,
// or the tag `docker-compose.yml` names - and what the step hands over is compared whole
// against it (checkHandedValues in main.go). A name that resolves to nothing is refused, and
// that refusal is what makes this the rule rather than the two spellings a review happened
// to find. There is no shell in the question: these are `env:` scalars, interpolated against
// the values the planning logic produced.
//
// AND IT IS NOT COMPARED WITH A VALUE, IT HAS TO BE ONE. That rule first shipped comparing
// each name against the outputs of a single planned release, so a literal equal to that one
// sample - `v0.1.0`, the version this repository has actually published and the string a
// maintainer copies out of a green run's log - was accepted as "the value the planning logic
// produced" (S0046 F26). Widening the comparison only moves the coincidence. So a role step's
// object must BE the planning logic's own output: the value is an EXPRESSION naming it, traced
// back through the `needs:` graph, and any literal is refused. There is no sample in the
// question, so no sample can be copied into it. handed.go is that trace.
//
// The cost is stated: this constrains the release definition. A role step may not be an
// inline multi-line script, it may not carry a flag or an environment variable nobody has
// classified, and renaming one of these scripts means editing this table. That is the trade
// the conductor's ruling called for - make the question decidable instead of solving it -
// and a drift between the two reds loudly rather than quietly.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fieldSpec is one field a role's invocation MAY carry, and the reason a human read it and
// found that it cannot make the invocation do less than the role says it does. A field with
// `values` takes exactly one value, and that value has to be one of them: `-C .` is the
// directory make would have run in anyway, while `-C /somewhere/else` is a different
// Makefile's `check`.
type fieldSpec struct {
	field  string
	values []string
	why    string
}

// envHeld names the value a role step's `env:` entry must HAND its program. Every one of
// these is produced OUTSIDE the step that carries it, which is the whole point: a value the
// step spells for itself is a value nothing constrains. The zero value is deliberate - a
// declared name that resolves to no source at all reads CLOSED and reds by name, exactly as
// an unclassified field, key or input does.
type envHeld int

const (
	heldNothing      envHeld = iota // the zero value: declared, held against nothing. Refused.
	heldPlannedImage                // the image reference the planning logic produced
	heldPlannedVersion
	heldGatedRef    // image:version - the reference this run pushes, gates and publishes
	heldFloatingTag // the tag a release MOVES, which is the one docker-compose.yml must not pin
	heldEventName   // the event the shape being planned is
	heldRefName     // the ref that shape carries
	heldRepository  // this module's own repository, derived from go.mod
	// whether the planning logic decided this release is a pre-release. It is not a
	// reference, but it decides how an irreversible act is performed - marking a GitHub
	// release `--prerelease` or not - so it is held the same way every other object is.
	heldPlannedPrerelease
)

// What a value holding each of those sources must BE is declared in handed.go's `heldAs`, and
// it is a FORM rather than a value: the expression naming that source, traced through the
// workflow's own `needs:` graph. Nothing is compared with a sample. See handed.go's header for
// why comparing was the wrong shape (S0046 F26).

// heldSources is every source any role OR act declares, so the checks over that table cover
// exactly what is actually decided and nothing they invented. The acts are in here for the
// same reason the roles are: an output an act's object is traced to and that the planning
// logic never writes is an empty string in the middle of a release, whether the step holding
// it declares a role or not (checkPlanProducesEverythingAReleaseStepIsHeldTo).
func heldSources() []envHeld {
	seen := map[envHeld]bool{}
	var out []envHeld
	add := func(m map[string]envSpec) {
		for _, name := range sortedEnvNames(m) {
			if e := m[name].holds; !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	for _, r := range releaseRoles {
		add(r.handsEnv)
		add(r.handsInput)
	}
	for _, a := range releaseActs {
		add(a.handsEnv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// envSpec is one environment name a role's own program reads: what the value is for, and
// the value it must be. `what` is prose for the messages; `holds` is the comparison.
type envSpec struct {
	holds envHeld
	what  string
}

// role is one named position in the release, and what a step must invoke to hold it.
type role struct {
	id   string
	what string // how the role is described in every message

	planning bool // the planning step: the one script this gate EXECUTES

	program      string      // the first field the one-line `run:` must be
	mustField    []string    // fields that must appear, compared WHOLE
	mustNotField []string    // fields that must not, each with its own diagnosis
	mayField     []fieldSpec // the ONLY other fields the invocation may carry

	action    string            // for a `uses:` step, the action before its `@version`
	mustInput map[string]string // `with:` inputs that must carry exactly this value
	mayInput  map[string]string // the other inputs it may carry, each with why it is inert

	// handsEnv are the environment names this role's own invocation reads, and the value
	// each must carry. Every other name in scope for the step must be classified in
	// envCheckedAndInertForReleaseSteps; every name HERE must be set for the step and must
	// equal the value it is held against, or the role is held by a step that invokes the
	// right program against the wrong object.
	handsEnv map[string]envSpec

	// handsInput is the same rule for a role performed by an ACTION: an input whose VALUE
	// decides which object the act is performed on. `tags:` is the whole of it - it names the
	// references docker/build-push-action publishes, so a literal there publishes something
	// other than what this run gated, exactly as a literal `REF:` re-smokes something other
	// than what this run pushed. mustInput carries a FIXED value; this carries a value that
	// has to track the run.
	handsInput map[string]envSpec

	needsGrant bool // it performs an irreversible act, so it must live in a job granted one
}

// releaseRoles is the shape of a release, declared. Every entry is required: a definition
// missing one of these is not a release this gate can grade, and says so rather than
// grading the subset it found.
var releaseRoles = []role{
	{
		id:       "plan",
		what:     "the planning logic (which decides whether this run publishes at all)",
		planning: true,
		handsEnv: map[string]envSpec{
			"EVENT":    {heldEventName, "the event name the plan keys on"},
			"REF_NAME": {heldRefName, "the ref the plan keys on"},
			"REPO":     {heldRepository, "the repository the image reference is derived from"},
		},
	},
	{
		id:        "full-gate",
		what:      "the full gate (make check)",
		program:   "make",
		mustField: []string{"check"},
		mayField: []fieldSpec{
			{
				field:  "-C",
				values: []string{"."},
				why:    "make changes to the directory it is given before reading a Makefile, and `.` is the directory it would have run in anyway. Any OTHER directory is a different Makefile's `check`, which is why this field's value is declared rather than free",
			},
		},
	},
	{
		id:      "smoke-amd64",
		what:    "the linux/amd64 smoke run (a real encode inside the built image)",
		program: "./scripts/smoke-image.sh",
		// The image the amd64 build step loads. Without it the role was satisfied by
		// `./scripts/smoke-image.sh` with no argument at all, which exits 2 having smoked
		// nothing.
		mustField:    []string{"holdfast:release"},
		mustNotField: []string{"--no-encode", "linux/arm64"},
	},
	{
		id:        "smoke-arm64",
		what:      "the linux/arm64 smoke run",
		program:   "./scripts/smoke-image.sh",
		mustField: []string{"holdfast:release-arm64", "linux/arm64"},
		mayField: []fieldSpec{
			{
				field: "--no-encode",
				why:   "the arm64 run is exec-only under QEMU, so it may skip the encode. It is OPTIONAL rather than required: dropping it makes that run stricter, and a role must not refuse a step that does MORE than it promises",
			},
		},
	},
	{
		id:     "push-version",
		what:   "the version-tag push",
		action: "docker/build-push-action",
		mustInput: map[string]string{
			// Without this the action builds and exports nothing to a registry, so the
			// version tag is never published while the step still holds the role.
			"push": "true",
		},
		mayInput: map[string]string{
			"context":    "which directory is built; it cannot stop the result being pushed",
			"platforms":  "which architectures are built; the re-smoke pulls both back by name",
			"build-args": "values baked into the image",
			"cache-from": "where layers are read from; a cache is not a destination",
		},
		handsInput: map[string]envSpec{
			// The references this step PUBLISHES, and the one act in the release whose
			// object is an action input rather than an `env:` scalar. Pinned to a literal,
			// a later tag republishes whatever that literal names - and sources/semver.org,
			// this repository's own authority for the version scheme, is explicit that a
			// released version's contents must never be modified.
			"tags": {heldGatedRef, "the references this push publishes"},
		},
		needsGrant: true,
	},
	{
		id:      "resmoke",
		what:    "the re-smoke of the artefact pulled back from the registry",
		program: "./scripts/release-resmoke.sh",
		handsEnv: map[string]envSpec{
			// Held against the reference this run PUSHED, and that is the whole of this
			// role: a re-smoke of any other reference grades an artefact this run did not
			// produce, passes, and lets the promotion move `:latest` onto one nothing
			// pulled back. The push is a cache rebuild - release.yml says so at the push
			// step - which is the reason the pull-back exists at all.
			"REF": {heldGatedRef, "the reference release-resmoke.sh pulls back and smokes"},
		},
	},
	{
		id:      "promote-latest",
		what:    "the promotion of the floating reference",
		program: "./scripts/release-promote.sh",
		handsEnv: map[string]envSpec{
			"IMAGE":   {heldPlannedImage, "the image the promotion moves"},
			"VERSION": {heldPlannedVersion, "the version it is retagged onto"},
			// Declared HERE once and read by the two scripts rather than spelled three
			// times, and held against the tag docker-compose.yml PINS - which it must not
			// be, because retagging the version the example deployment pins would leave
			// that file's tag and digest disagreeing on the day the next release lands.
			floatingTagEnv: {heldFloatingTag, "the floating reference it moves"},
		},
		needsGrant: true,
	},
	{
		id:      "resolve-compose",
		what:    "the resolution of the example deployment's reference against the registry",
		program: "./scripts/resolve-compose-image.sh",
		handsEnv: map[string]envSpec{
			// A11 is "resolve … to the digest the run just gated", so these ARE the
			// criterion. `${IMAGE}:${VERSION}` is the reference this run gated, and it is
			// what the FLOATING reference has to resolve to once the promotion has moved
			// it: hand the step `VERSION: latest` and that comparison is the floating
			// reference against itself, which cannot fail, and A11 is the only enforcement
			// A5 has after the first release.
			//
			// FLOATING_TAG is here because the two references parted company when the
			// example deployment stopped depending on a mutable one (S0057's P1). What a
			// user pulls is now a PINNED reference and is resolved on its own - that is
			// A5's half, "find an image at that exact reference" - while the reference the
			// promotion just moved is the one that must carry this run's digest. One step
			// resolves both, because both are a property of the release that just
			// published, and neither can be decided without a registry.
			"IMAGE":        {heldPlannedImage, "the image whose gated digest the floating reference must resolve to"},
			"VERSION":      {heldPlannedVersion, "the version just published"},
			floatingTagEnv: {heldFloatingTag, "the floating reference whose digest this run just moved"},
		},
	},
}

// envCheckedAndInertForReleaseSteps are environment names that may be in scope for ANY step
// this gate grades - a role step or an irreversible act (acts.go) - because a human has read
// each and found it unable to change what that step's program does. Everything else -
// MAKEFLAGS, GOFLAGS, PATH, SHELL, BASH_ENV, and whatever is invented next - reds by name,
// which is the point: `env: MAKEFLAGS: -n` neuters `run: make check` without touching one
// character of the invocation. One table for both positions, because a name that cannot
// change what a role's program does cannot change what an act's does either, and two copies
// of that judgement would drift.
var envCheckedAndInertForReleaseSteps = map[string]string{
	"GO_VERSION": "which Go toolchain actions/setup-go installs. It selects a compiler; it cannot make a program run less than its invocation says",
}

// roleStepKeysCheckedAndInert are the step keys a role step may carry, each with the reason
// it cannot change what the invocation does.
var roleStepKeysCheckedAndInert = map[string]string{
	"name":              "a label",
	"id":                "the declared identity this role is located by",
	"run":               "the invocation itself, accounted for field by field",
	"uses":              "the action an action role names, compared whole",
	"with":              "an action's inputs, accounted for above",
	"env":               "values, every name of which is classified above",
	"if":                "a guard, decided from the values the planning logic produced",
	"continue-on-error": "whether a failure fails the run; refused before the promotion by A14",
	"timeout-minutes":   "a clock. It can make a step FAIL, which is loud; it cannot make one pass having done less",
}

// roleStepKeysThatChangeAnInvocation are the ones that DO, each with how. They are listed
// separately from the unclassified case only so the refusal can say what the key would have
// done - the verdict is the same.
var roleStepKeysThatChangeAnInvocation = map[string]string{
	"shell":             "which interpreter runs the script, so the first field of the `run:` line need not be executed as a program at all: GitHub appends the script to the command given, and `shell: cat` prints it and exits 0",
	"working-directory": "which directory the script runs in, so `make check` becomes some other Makefile's `check`",
}

// mustPrecede is A7's order, written once. Each pair is "a must have finished before b".
var mustPrecede = [][2]string{
	{"plan", "push-version"},
	{"full-gate", "push-version"},
	{"smoke-amd64", "push-version"},
	{"smoke-arm64", "push-version"},
	{"push-version", "resmoke"},
	{"resmoke", "promote-latest"},
	{"promote-latest", "resolve-compose"},
}

// roleWhat is the human description of a role id.
func roleWhat(id string) string {
	for _, r := range releaseRoles {
		if r.id == id {
			return r.what
		}
	}
	return id
}

// roleInvocation is how a role is named in the order sentence: what the step INVOKES, which
// is what this gate compared, rather than what that program then goes on to do, which it
// does not read. See the note above checkOrder.
func roleInvocation(id string) string {
	for _, r := range releaseRoles {
		if r.id != id {
			continue
		}
		switch {
		case r.planning:
			return "the planning script, executed by this gate"
		case r.action != "":
			return "uses " + r.action
		default:
			return "invokes " + r.program
		}
	}
	return id
}

// Roles is where each declared role was found.
type Roles struct {
	steps map[string]Step
}

func (r Roles) step(id string) Step { return r.steps[id] }

// locateRoles finds every declared role and proves each step really invokes what the role
// says it does. Everything here is a whole-value comparison against a DECLARED set;
// nothing searches inside a value, and anything undeclared reds by name. An unfound or
// miscast role is a hard error: the gate refuses to grade an order over steps it could not
// identify (A15's family - a pass over a set it could not build is the vacuous pass this
// gate exists to refuse).
func locateRoles(wf *Workflow, root string) (*Roles, error) {
	found := map[string][]Step{}
	for _, jid := range wf.JobIDs() {
		for _, s := range wf.Jobs[jid].Steps {
			if s.ID == "" {
				continue
			}
			found[s.ID] = append(found[s.ID], s)
		}
	}
	out := &Roles{steps: map[string]Step{}}
	for _, r := range releaseRoles {
		steps := found[r.id]
		switch len(steps) {
		case 0:
			return nil, fmt.Errorf("no step declares `id: %s`, so %s cannot be located. This gate grades an ORDER, and it identifies each position by the step's own declared id rather than by searching its text for a mention of the thing - a step that MENTIONS the gate is not the gate (S0046 F13). Give the step that %s an `id: %s`", r.id, r.what, r.what, r.id)
		case 1:
		default:
			return nil, fmt.Errorf("%d steps declare `id: %s`. A role has to be one step or the order is not defined; this gate refuses to guess which one is %s", len(steps), r.id, r.what)
		}
		s := steps[0]
		if err := r.matches(s); err != nil {
			return nil, err
		}
		if err := r.checkStepKeys(s); err != nil {
			return nil, err
		}
		if err := r.checkEnv(wf, wf.Jobs[s.JobID], s); err != nil {
			return nil, err
		}
		if err := checkNoDefaults(wf, wf.Jobs[s.JobID], s); err != nil {
			return nil, err
		}
		if err := r.programExists(root); err != nil {
			return nil, fmt.Errorf("%s holds the role `%s` (%s), and %w", s.Label(), r.id, r.what, err)
		}
		out.steps[r.id] = s
	}
	return out, nil
}

// programExists is the other half of "invokes what the role says": a role whose script is a
// file in this repository has to BE a file in this repository, executable. A step invoking a
// script that is not there fails at run time, in the middle of a release, having passed a
// gate that only compared strings.
func (r role) programExists(root string) error {
	if !strings.HasPrefix(r.program, "./") {
		return nil
	}
	p := filepath.Join(root, strings.TrimPrefix(r.program, "./"))
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("the script it invokes (%s) is not in this repository: %v. A release step that invokes a file which is not there fails in the middle of a release", r.program, err)
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("the script it invokes (%s) is not executable (mode %v), so the step would fail at run time", r.program, st.Mode().Perm())
	}
	return nil
}

// matches proves the step holding a role invokes what the role says.
func (r role) matches(s Step) error {
	switch {
	case r.planning:
		if strings.TrimSpace(s.Run) == "" {
			return fmt.Errorf("%s holds the role `%s` (%s) but has no `run:` script to execute", s.Label(), r.id, r.what)
		}
		if !strings.Contains(s.Run, "GITHUB_OUTPUT") {
			return fmt.Errorf("%s holds the role `%s` (%s) but writes nothing to $GITHUB_OUTPUT, so there is no runtime value to decide any guard from and this gate would be reduced to reading text", s.Label(), r.id, r.what)
		}
		return nil

	case r.action != "":
		if s.Uses == "" {
			return fmt.Errorf("%s holds the role `%s` (%s), which is performed by the action %s, but the step has no `uses:`", s.Label(), r.id, r.what, r.action)
		}
		name := s.Uses
		if at := strings.LastIndex(name, "@"); at >= 0 {
			name = name[:at]
		}
		if name != r.action {
			return fmt.Errorf("%s holds the role `%s` (%s) but uses %q, not %q. The role names the action that performs it; a different action is a different act, and this gate will not grade an order over a step it cannot identify", s.Label(), r.id, r.what, name, r.action)
		}
		return r.accountForInputs(s)

	default:
		fields, err := runFields(s.Run)
		if err != nil {
			return fmt.Errorf("%s holds the role `%s` (%s), and %w", s.Label(), r.id, r.what, err)
		}
		if fields[0] != r.program {
			return fmt.Errorf("%s holds the role `%s` (%s), so it must INVOKE %s: the first word of its script has to BE that program. It is %q.\nThis is compared as a whole word on purpose. A role decided by searching a step for a mention of the thing is a role any step can claim by mentioning it - `echo \"make check\"` satisfied the full-gate role once (S0046 F13) - and no quoting or nesting survives a whole-word comparison",
				s.Label(), r.id, r.what, r.program, fields[0])
		}
		for _, want := range r.mustField {
			if !hasField(fields, want) {
				return fmt.Errorf("%s holds the role `%s` (%s) but its script passes no argument %q. The fields are %v", s.Label(), r.id, r.what, want, fields)
			}
		}
		for _, forbid := range r.mustNotField {
			if hasField(fields, forbid) {
				return fmt.Errorf("%s holds the role `%s` (%s) but its script passes %q, which that role must not: the fields are %v", s.Label(), r.id, r.what, forbid, fields)
			}
		}
		return r.accountForFields(s, fields)
	}
}

// accountForFields is the deny-by-default half, and it is what closes S0046 F18. Naming the
// program and carrying the fields the role requires is NOT enough, because a flag can make
// that program do nothing at all: `make -n check` prints every recipe in the gate and runs
// none of them, exiting 0. So every remaining field must be one the role has DECLARED, with
// the reason it cannot make the invocation do less. Anything else reds by name.
//
// This deliberately does not enumerate the bad fields. -n, -q, -t, --dry-run, --touch,
// --question, -Bn and `SHELL=true` are all refused by the same sentence as the flag nobody
// has thought of yet, which is the difference between this and the six catalogues that lost.
func (r role) accountForFields(s Step, fields []string) error {
	for i := 1; i < len(fields); {
		f := fields[i]
		if hasField(r.mustField, f) {
			i++
			continue
		}
		spec, ok := r.maySpec(f)
		if !ok {
			return fmt.Errorf("%s holds the role `%s` (%s), and its script passes %q - a field this role has not classified.\n"+
				"An unrecognised field reads CLOSED. Naming the program and carrying the fields the role requires is not enough: `make -n check` names the full gate, prints every recipe in it, executes not one of them and exits 0 (S0046 F18), and so do -q, -t, --dry-run, a clustered -Bn and `check SHELL=true`. Enumerating those buys exactly the spellings it names.\n"+
				"So the fields a role's invocation may carry are DECLARED. If %q genuinely cannot make this invocation do less than %q says it does, add it to this role's mayField with that reason; if it can, this refusal is the gate working.\n"+
				"fields: %v\nrequired: %v\npermitted: %v",
				s.Label(), r.id, r.what, f, f, r.id, fields, r.mustField, r.permittedFields())
		}
		if len(spec.values) == 0 {
			i++
			continue
		}
		if i+1 >= len(fields) {
			return fmt.Errorf("%s holds the role `%s` (%s), and its script ends with %q, which takes a value (one of %v) and has none. The fields are %v",
				s.Label(), r.id, r.what, f, spec.values, fields)
		}
		if v := fields[i+1]; !hasField(spec.values, v) {
			return fmt.Errorf("%s holds the role `%s` (%s), and its script passes `%s %s`. That field's value is DECLARED rather than free, and %q is not one of %v: %s.\nThe fields are %v",
				s.Label(), r.id, r.what, f, v, v, spec.values, spec.why, fields)
		}
		i += 2
	}
	return nil
}

// accountForInputs is the same rule for a role performed by an action: an input the role
// requires must carry exactly the value it names (`push: true`, without which
// docker/build-push-action builds and publishes nothing while the step still holds the
// role), and an input nobody has classified reds - `outputs: type=local,dest=…` would send
// the build to a directory instead of the registry.
func (r role) accountForInputs(s Step) error {
	for _, k := range sortedStringsOf(r.mustInput) {
		got, ok := s.With[k]
		if !ok {
			return fmt.Errorf("%s holds the role `%s` (%s) but declares no `%s:` input. That role requires `%s: %s`: without it the action performs no published act at all, and a role that is held by a step which does nothing is the whole defect this comparison exists to refuse", s.Label(), r.id, r.what, k, k, r.mustInput[k])
		}
		if v := strings.TrimSpace(yamlString(got)); v != r.mustInput[k] {
			return fmt.Errorf("%s holds the role `%s` (%s) but passes `%s: %s`, and that role requires `%s: %s`. Compared whole, because the difference between the two is the difference between a release and a build nobody published", s.Label(), r.id, r.what, k, v, k, r.mustInput[k])
		}
	}
	for _, k := range sortedInputNames(r.handsInput) {
		if _, ok := s.With[k]; !ok {
			return fmt.Errorf("%s holds the role `%s` (%s) but declares no `%s:` input, and that input names %s. A role step whose act has no object is not that act: the value is HELD against what the run produced (checkHandedValues), so it cannot simply be absent", s.Label(), r.id, r.what, k, r.handsInput[k].what)
		}
	}
	for _, k := range sortedKeys(s.With) {
		if _, ok := r.mustInput[k]; ok {
			continue
		}
		if _, ok := r.mayInput[k]; ok {
			continue
		}
		if _, ok := r.handsInput[k]; ok {
			continue
		}
		return fmt.Errorf("%s holds the role `%s` (%s) and passes the input `%s:`, which this role has not classified.\nAn unclassified input reads CLOSED: `outputs: type=local,dest=./out` would send this build to a directory rather than to the registry while every other input still reads like a push. Decide what `%s:` can do to this act and add it to that role's mayInput with the reason, or to mustInput with the value it must carry.\ndeclared inputs: %v",
			s.Label(), r.id, r.what, k, k, r.declaredInputs())
	}
	return nil
}

// checkStepKeys refuses a key on a role step that changes what the invocation does or that
// nobody has classified. `shell:` and `working-directory:` are the worked examples: neither
// touches one character of the `run:` line, and either makes it do something else.
func (r role) checkStepKeys(s Step) error {
	for _, k := range mappingKeys(s.Node) {
		if _, ok := roleStepKeysCheckedAndInert[k]; ok {
			continue
		}
		if why, ok := roleStepKeysThatChangeAnInvocation[k]; ok {
			return fmt.Errorf("%s holds the role `%s` (%s) and carries `%s:`, which decides %s.\nA role is held by a step INVOKING what the role names, and this key changes what that invocation does without changing one character of it. Move the step, or drop the key", s.Label(), r.id, r.what, k, why)
		}
		return fmt.Errorf("%s holds the role `%s` (%s) and carries the step key `%s:`, which this gate has not classified.\nAn unclassified key on a role step reads CLOSED: a key that decides which interpreter runs the script, or where it runs, changes what the invocation does while the invocation itself still reads correctly. Classify it in roleStepKeysCheckedAndInert with the reason it cannot, or in roleStepKeysThatChangeAnInvocation with what it decides", s.Label(), r.id, r.what, k)
	}
	return nil
}

// checkEnv refuses an environment name in scope for a role step that nobody has classified.
// This is not fussiness: `MAKEFLAGS: -n` in the workflow's own `env:` block makes
// `run: make check` print the gate's recipes and execute none of them, with the `run:` line
// and every field of it untouched.
//
// This is the NAME half. The VALUE half is checkHandedValues, which runs once the planning
// logic has produced the values a declared name is held against; a name that passes here and
// carries the wrong value is a role held by a step doing the right thing to the wrong object.
func (r role) checkEnv(wf *Workflow, job Job, s Step) error {
	sources := []struct {
		where string
		m     map[string]any
	}{
		{"the workflow's top-level `env:`", wf.Env},
		{fmt.Sprintf("job %q's `env:`", job.ID), job.Env},
		{"its own `env:`", s.Env},
	}
	for _, src := range sources {
		for _, k := range sortedKeys(src.m) {
			if _, ok := envCheckedAndInertForReleaseSteps[k]; ok {
				continue
			}
			if _, ok := r.handsEnv[k]; ok {
				continue
			}
			return fmt.Errorf("%s holds the role `%s` (%s), and %s sets `%s`, which this gate has not classified.\nAn environment name in scope for a role step reads CLOSED, because it can change what the invocation does while the invocation reads exactly as it did: `MAKEFLAGS: -n` makes `run: make check` print the gate's recipes and run none of them.\nIf `%s` genuinely cannot, classify it in envCheckedAndInertForReleaseSteps with the reason; if this role's own script reads it, declare it in that role's handsEnv with the value it must carry. This role reads %v",
				s.Label(), r.id, r.what, src.where, k, k, r.declaredEnv())
		}
	}
	return nil
}

// checkNoDefaults refuses a `defaults:` block over a role step. `defaults: run: shell:` and
// `defaults: run: working-directory:` are the same two neuters as the step keys above, set
// one or two levels further away, where nobody reading the step would see them.
func checkNoDefaults(wf *Workflow, job Job, s Step) error {
	if wf.HasDefaults {
		return fmt.Errorf("%s holds a release role, and the workflow declares a top-level `defaults:` block. That block sets the shell and the working directory every `run:` step gets, so it decides what a role's invocation does from two levels away: `defaults: run: shell: cat` makes every step print its script and exit 0. A release definition this gate grades declares no `defaults:`", s.Label())
	}
	if job.Node != nil && mappingValue(job.Node, "defaults") != nil {
		return fmt.Errorf("%s holds a release role, and job %q declares a `defaults:` block. That block sets the shell and the working directory its `run:` steps get, so it decides what this invocation does without appearing anywhere near it. A job carrying a release role declares no `defaults:`", s.Label(), job.ID)
	}
	return nil
}

// runFields splits a role step's script into whole words, refusing anything but one line.
// One line is what makes the comparison total: a multi-line script would have to be
// searched, and searching is what lost.
func runFields(run string) ([]string, error) {
	body := strings.TrimSpace(run)
	if body == "" {
		return nil, fmt.Errorf("its `run:` is empty")
	}
	if strings.Contains(body, "\n") {
		return nil, fmt.Errorf("its `run:` is more than one line. A role step invokes ONE program so the gate can compare the invocation whole rather than searching a script for a mention of it; put the script in scripts/ and invoke it (that is what scripts/release-resmoke.sh and scripts/release-promote.sh are). The script it carries is:\n%s", indent(body))
	}
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return nil, fmt.Errorf("its `run:` is empty")
	}
	return fields, nil
}

func (r role) maySpec(field string) (fieldSpec, bool) {
	for _, m := range r.mayField {
		if m.field == field {
			return m, true
		}
	}
	return fieldSpec{}, false
}

func (r role) permittedFields() []string {
	var out []string
	for _, m := range r.mayField {
		if len(m.values) == 0 {
			out = append(out, m.field)
			continue
		}
		out = append(out, fmt.Sprintf("%s %v", m.field, m.values))
	}
	if out == nil {
		return []string{"(none)"}
	}
	return out
}

func (r role) declaredEnv() []string {
	out := append(sortedEnvNames(r.handsEnv), sortedStringsOf(envCheckedAndInertForReleaseSteps)...)
	sort.Strings(out)
	if out == nil {
		return []string{"(nothing)"}
	}
	return out
}

func sortedEnvNames(m map[string]envSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r role) declaredInputs() []string {
	out := append(sortedStringsOf(r.mustInput), sortedStringsOf(r.mayInput)...)
	out = append(out, sortedInputNames(r.handsInput)...)
	sort.Strings(out)
	if out == nil {
		return []string{"(none)"}
	}
	return out
}

// sortedInputNames is sortedEnvNames for the input half; the two maps carry the same type and
// are kept apart because one is read from `env:` and the other from `with:`.
func sortedInputNames(m map[string]envSpec) []string { return sortedEnvNames(m) }

func sortedStringsOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

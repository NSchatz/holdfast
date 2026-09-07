package main

// WHICH STEP IS THE FULL GATE? - answered from a step's declared identity, never from
// prose in its text.
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
// The cost is stated: this constrains the release definition. A role step may not be an
// inline multi-line script, and renaming one of these scripts means editing this table.
// That is the trade the conductor's ruling called for - make the question decidable
// instead of solving it - and a drift between the two reds loudly rather than quietly.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// role is one named position in the release, and what a step must invoke to hold it.
type role struct {
	id   string
	what string // how the role is described in every message

	planning bool // the planning step: the one script this gate EXECUTES

	program      string   // the first field the one-line `run:` must be
	mustField    []string // fields that must appear, compared WHOLE
	mustNotField []string // fields that must not

	action string // for a `uses:` step, the action before its `@version`

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
	},
	{
		id:        "full-gate",
		what:      "the full gate (make check)",
		program:   "make",
		mustField: []string{"check"},
	},
	{
		id:           "smoke-amd64",
		what:         "the linux/amd64 smoke run (a real encode inside the built image)",
		program:      "./scripts/smoke-image.sh",
		mustNotField: []string{"--no-encode", "linux/arm64"},
	},
	{
		id:        "smoke-arm64",
		what:      "the linux/arm64 smoke run",
		program:   "./scripts/smoke-image.sh",
		mustField: []string{"linux/arm64"},
	},
	{
		id:         "push-version",
		what:       "the version-tag push",
		action:     "docker/build-push-action",
		needsGrant: true,
	},
	{
		id:      "resmoke",
		what:    "the re-smoke of the artefact pulled back from the registry",
		program: "./scripts/release-resmoke.sh",
	},
	{
		id:         "promote-latest",
		what:       "the promotion of the floating reference",
		program:    "./scripts/release-promote.sh",
		needsGrant: true,
	},
	{
		id:      "resolve-compose",
		what:    "the resolution of the example deployment's reference against the registry",
		program: "./scripts/resolve-compose-image.sh",
	},
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

// Roles is where each declared role was found.
type Roles struct {
	steps map[string]Step
}

func (r Roles) step(id string) Step { return r.steps[id] }

// locateRoles finds every declared role and proves each step really invokes what the role
// says it does. Everything here is a whole-value comparison; nothing searches inside a
// value. An unfound or miscast role is a hard error: the gate refuses to grade an order
// over steps it could not identify (A15's family - a pass over a set it could not build is
// the vacuous pass this gate exists to refuse).
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
		return nil

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
		return nil
	}
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

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

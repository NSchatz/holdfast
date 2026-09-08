package main

// WHICH EVENTS CAN REACH THIS WORKFLOW AT ALL? - the question that has to be answered
// before "what happens on a dispatch" and "what happens on a tag push" mean anything.
//
// The gate plans event shapes: a manual dispatch, a version tag push, a pre-release tag
// push, a tag whose major is not zero. Those four are shapes it INVENTS, and for six
// ordinals nothing read `on:` to check they were the shapes this workflow actually has.
// The hole that leaves is quiet and total: add
//
//	on:
//	  push:
//	    tags: ["v*"]
//	    branches: ["v0.**"]
//
// and a push to a BRANCH named `v0.9.9` is a `push` event whose `github.ref_name` is
// `v0.9.9`, so the planning logic - which keys on the event name and the ref name, and has
// no way to tell a branch from a tag - sets `publish=true`. The publishing job runs, from a
// branch head, and every assertion in this gate stays green because the shape it graded is
// still the shape it invented. `git push origin HEAD:v0.9.9` publishes a release.
//
// So the event surface is DECLARED here and graded, deny-by-default, the same way
// `permissions:` and every job and step key already are. A trigger nobody classified reds
// by name; a `push:` filter that admits anything but a tag reds by name; and an
// `on:` that is not a mapping at all reds, because `on: push` carries no filter and fires
// on every branch. Silence is unreachable, which is the one sentence every hole this gate
// has ever had was an instance of.

import (
	"fmt"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// triggersPlanned are the events this gate plans, each with the shape it plans for it. A
// trigger outside this map is one no plan covers, so nothing here says anything about what
// it would do.
var triggersPlanned = map[string]string{
	"push":              "planned as a version-tag push, a pre-release tag push and a tag whose major is not zero",
	"workflow_dispatch": "planned as a manual dispatch, which must publish nothing",
}

// pushFiltersCheckedAndTagOnly are the keys `on: push:` may carry. `tags` is required: it
// is what makes a push event a RELEASE rather than a merge. Everything else either widens
// the trigger past a tag or narrows it in a way nobody has read.
var pushFiltersCheckedAndTagOnly = map[string]string{
	"tags": "the tag patterns a release is cut from; this is the release trigger itself",
}

// pushFiltersThatWidenPastATag are the ones that DO admit a non-tag push, each with what it
// would let through. They red exactly as an unclassified key does; they are separate only
// so the message can say what would have happened.
var pushFiltersThatWidenPastATag = map[string]string{
	"branches":        "a push to a BRANCH. The planning logic keys on the event name and the ref name and cannot tell a branch from a tag, so a branch named `v0.9.9` would set publish=true and release from a branch head",
	"branches-ignore": "every branch except the ones named - which is every other branch, so a push to any of them would set publish=true",
	"paths":           "it does not widen the ref filter by itself, but it is a filter nobody has read against the plan; a `paths:` beside no `tags:` fires on every branch",
	"paths-ignore":    "the same, inverted",
	"tags-ignore":     "it narrows which tags release, silently, so a tag the runbook says publishes would not",
}

// checkTriggerSurface grades `on:` against the shapes this gate plans. A6 and A7 each name
// their event; this is what makes "the shape it graded" and "the shapes this workflow has"
// the same set rather than two sets that happen to overlap today.
func (g *gate) checkTriggerSurface(wf *Workflow) {
	n := wf.OnNode
	if n == nil {
		g.bad("%s declares no `on:` block, so which events reach this workflow is unknown to this gate. Every shape planned below would then be a shape this gate invented, checked against a workflow that may fire on something else entirely.", releaseWorkflow)
		return
	}
	if n.Kind != yaml.MappingNode {
		g.bad("%s declares an `on:` that is not a mapping of events to their filters (it is %s). The short forms - `on: push` and `on: [push]` - carry NO filter, so the workflow fires on every push to every branch, and the planning logic cannot tell a branch push from a tag push: it would set publish=true from a branch head. Write `on:` as a mapping and give `push:` a `tags:` filter.", releaseWorkflow, kindName(n.Kind))
		return
	}

	var planned []string
	before := g.failed
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		why, ok := triggersPlanned[key]
		if !ok {
			g.bad("%s triggers on `%s:`, which this gate plans no shape for. Every assertion here is made about a shape the gate PLANNED - %s - so an event outside that set is an entry to this workflow that nothing below says anything about, publishing job included. Either remove the trigger, or plan the shape and classify it in triggersPlanned.", releaseWorkflow, key, strings.Join(sortedStringsOf(triggersPlanned), " and "))
			continue
		}
		switch key {
		case "push":
			g.checkPushFilter(val)
		case "workflow_dispatch":
			g.checkDispatchFilter(val)
		}
		planned = append(planned, fmt.Sprintf("`%s:` (%s)", key, why))
	}
	if len(planned) == 0 {
		g.bad("%s declares an `on:` block that names no event at all.", releaseWorkflow)
		return
	}
	if g.failed == before {
		g.note("the event surface is exactly what this gate plans: %s", strings.Join(planned, ", "))
	}
}

func (g *gate) checkPushFilter(n *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		g.bad("%s declares `on: push:` with no filter mapping under it, so EVERY push to every branch runs this workflow. The planning logic keys on the event name and the ref name and cannot tell a branch from a tag, so a push to a branch named like a version would set publish=true and cut a release from a branch head. Give it `tags:`.", releaseWorkflow)
		return
	}
	seenTags := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if _, ok := pushFiltersCheckedAndTagOnly[key]; ok {
			seenTags = seenTags || key == "tags"
			continue
		}
		if why, ok := pushFiltersThatWidenPastATag[key]; ok {
			g.bad("%s declares `on: push:` with a `%s:` filter, which admits %s.\nA release is cut by pushing a TAG and by nothing else - that is what A6 and A7 are each about, and it is what makes `workflow_dispatch` the only other way in. A push filter that admits anything else is a second, ungraded route to the publishing job.", releaseWorkflow, key, why)
			continue
		}
		g.bad("%s declares `on: push:` with a `%s:` filter, which this gate has not classified. An unclassified filter reads CLOSED: decide whether it can admit a push that is not a tag and classify it in pushFiltersCheckedAndTagOnly with the reason it cannot, or in pushFiltersThatWidenPastATag with what it lets through.", releaseWorkflow, key)
	}
	if !seenTags {
		g.bad("%s declares `on: push:` with no `tags:` filter, so every push it matches is one the planning logic will read as a release. The tag filter is the release trigger.", releaseWorkflow)
	}
}

func (g *gate) checkDispatchFilter(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && strings.TrimSpace(n.Value) == "" {
		return // `workflow_dispatch:` with nothing under it - the shape this gate plans
	}
	if n.Kind != yaml.MappingNode {
		g.bad("%s declares a `workflow_dispatch:` of a shape this gate cannot read (%s).", releaseWorkflow, kindName(n.Kind))
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		g.bad("%s declares `workflow_dispatch:` with a `%s:` key. A dispatch is planned as ONE shape here - the dry run - and `inputs:` is how a second one gets added: a tick-box that a dispatch can publish. release.yml's own header says there is deliberately no such input, because the only thing that can publish is a tag and a dispatch-publish could only ever push `0.0.0-dev-<sha>`. Remove it, or plan the shape it creates.", releaseWorkflow, n.Content[i].Value)
	}
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.AliasNode:
		return "a YAML alias"
	default:
		return "an unreadable node"
	}
}

package main

import (
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// The `with:` input that decides whether an action publishes is the one place this gate has
// to answer "does this step publish?" from the definition rather than from a run, and it is
// the place where being wrong in one direction is invisible: a step read as harmless is how
// an unreviewed publish ships. So every shape it can take is pinned here, and the table is
// written in YAML rather than in Go literals, because the decoder's own view of `push: yes`
// is part of what is being asserted.
func TestDecideRequiredInput(t *testing.T) {
	dispatch := ctxFor("workflow_dispatch")
	tagPush := ctxFor("push")

	cases := []struct {
		name      string
		yaml      string // the `with:` block
		ctx       *evalCtx
		publishes bool
		wantErr   string // substring; empty = must not error
	}{
		{name: "absent is the action's own default, and not an act", yaml: "context: .", ctx: &dispatch},
		{name: "a literal true publishes", yaml: "push: true", ctx: &dispatch, publishes: true},
		{name: "a literal false does not", yaml: "push: false", ctx: &dispatch},
		{name: "a quoted true publishes", yaml: `push: "true"`, ctx: &dispatch, publishes: true},
		{name: "a quoted false does not", yaml: `push: "false"`, ctx: &dispatch},

		// The defect this table exists for: GitHub's documented idiom for a conditional
		// push. It is never the string "true", and it is true on a dispatch.
		{
			name:      "an expression that is TRUE on the event under test publishes",
			yaml:      "push: ${{ github.event_name == 'workflow_dispatch' }}",
			ctx:       &dispatch,
			publishes: true,
		},
		{
			name: "the same expression on a tag push does not",
			yaml: "push: ${{ github.event_name == 'workflow_dispatch' }}",
			ctx:  &tagPush,
		},
		{
			name:      "an expression reading a planning output publishes when that output says so",
			yaml:      "push: ${{ steps.plan.outputs.publish }}",
			ctx:       &tagPush,
			publishes: true,
		},
		{
			name: "and does not when it says otherwise",
			yaml: "push: ${{ steps.plan.outputs.publish }}",
			ctx:  &dispatch,
		},

		// Everything undecidable. None of these may come back as a quiet "no".
		{
			name:    "a context the run did not produce is an error, not a false",
			yaml:    "push: ${{ vars.PUBLISH_DEV }}",
			ctx:     &dispatch,
			wantErr: "cannot be decided",
		},
		{
			name:    "a function this evaluator does not implement is an error",
			yaml:    "push: ${{ fromJSON(github.event.inputs.opts).push }}",
			ctx:     &dispatch,
			wantErr: "cannot be decided",
		},
		{
			name:    "an expression that evaluates to something that is not a boolean is an error",
			yaml:    "push: ${{ github.ref_name }}",
			ctx:     &tagPush,
			wantErr: "is not a boolean",
		},
		{
			name:    "`push: yes` is a STRING in YAML 1.2 and is not a boolean anywhere",
			yaml:    "push: yes",
			ctx:     &dispatch,
			wantErr: "is not a boolean",
		},
		{
			name:    "a number is not a boolean either",
			yaml:    "push: 1",
			ctx:     &dispatch,
			wantErr: "not a boolean and not an expression",
		},
		{
			name:    "an empty value says nothing and must not be read as no",
			yaml:    `push: ""`,
			ctx:     &dispatch,
			wantErr: "is empty",
		},

		// The static question, asked by the runbook cross-check: no event decides it, so an
		// expression counts as publishing rather than disappearing.
		{
			name:      "with no event to decide against, an expression counts as publishing",
			yaml:      "push: ${{ github.event_name == 'workflow_dispatch' }}",
			ctx:       nil,
			publishes: true,
		},
		{name: "a literal is still decided with no event", yaml: "push: false", ctx: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var with map[string]any
			if err := yaml.Unmarshal([]byte(tc.yaml), &with); err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			got, detail, err := decideRequiredInput("push", with["push"], tc.ctx)
			switch {
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("wanted an error mentioning %q, got publishes=%v detail=%q and no error.\nAn undecidable input read as a decision is the fail-open this function exists to close.", tc.wantErr, got, detail)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error does not mention %q: %v", tc.wantErr, err)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case got != tc.publishes:
				t.Fatalf("publishes = %v, want %v (detail %q)", got, tc.publishes, detail)
			}
		})
	}
}

// A step whose publish decision cannot be decided must produce an ERROR from Acts, not an
// empty act list - an empty list is indistinguishable from "this step is harmless".
func TestActs_UndecidablePushInputIsAnErrorNotAnEmptyList(t *testing.T) {
	s := Step{
		Name:  "publish a dev image",
		Uses:  "docker/build-push-action@v6",
		With:  map[string]any{"push": "${{ vars.PUBLISH_DEV }}"},
		Index: 3,
	}
	ctx := ctxFor("workflow_dispatch")
	acts, err := s.Acts(&ctx)
	if err == nil {
		t.Fatalf("Acts returned %d act(s) and no error for an input it cannot decide", len(acts))
	}
	for _, want := range []string{`step 4 "publish a dev image"`, "docker/build-push-action@v6", "vars.PUBLISH_DEV"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}

func ctxFor(event string) evalCtx {
	ref, refName := "refs/heads/main", "main"
	publish := "false"
	if event == "push" {
		ref, refName, publish = "refs/tags/v0.1.0", "v0.1.0", "true"
	}
	return evalCtx{success: true, vars: map[string]any{
		"github": map[string]any{"event_name": event, "ref": ref, "ref_name": refName},
		"steps": map[string]any{
			"plan": map[string]any{"outputs": map[string]any{"publish": publish}},
		},
	}}
}

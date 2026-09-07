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

// `push:` is the SHORTHAND, not the only route. The action's own input table defines it as
// "shorthand for `--output=type=registry`" and defines `outputs:` as the list of output
// destinations, so both keys decide the same act and both have to be decided - modelling
// only `push:` turned the absence of that one key into an inferred "local build", which is
// false whenever the longhand carries the act and false in the direction that ships an
// unreviewed publish. Every shape `outputs:` can take is pinned here for the same reason the
// table above exists, and in the same YAML-first form.
func TestDecideOutputsInput(t *testing.T) {
	dispatch := ctxFor("workflow_dispatch")
	tagPush := ctxFor("push")

	cases := []struct {
		name      string
		yaml      string // the `with:` block
		ctx       *evalCtx
		publishes bool
		wantErr   string // substring; empty = must not error
	}{
		{name: "absent sets no destination, and is not an act", yaml: "context: .", ctx: &dispatch},

		// PUBLISHES. The two spellings of a registry destination, and the action's own
		// multi-platform example is the first of them.
		{
			name:      "the image exporter with push=true is a registry push",
			yaml:      "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev,push=true",
			ctx:       &dispatch,
			publishes: true,
		},
		{
			name:      "the registry exporter is buildx's shorthand for the same thing",
			yaml:      "outputs: type=registry",
			ctx:       &dispatch,
			publishes: true,
		},
		{
			name:      "a push-by-digest spelling publishes too",
			yaml:      "outputs: type=image,name=ghcr.io/nschatz/holdfast,push-by-digest=true,name-canonical=true,push=true",
			ctx:       &dispatch,
			publishes: true,
		},
		{
			name:      "one publishing line among several publishes",
			yaml:      "outputs: |\n  type=local,dest=out\n  type=image,name=ghcr.io/nschatz/holdfast:dev,push=true\n",
			ctx:       &dispatch,
			publishes: true,
		},
		{
			name:      "an expression that resolves into a publishing spec publishes",
			yaml:      "outputs: type=image,name=ghcr.io/nschatz/holdfast:${{ github.ref_name }},push=true",
			ctx:       &tagPush,
			publishes: true,
		},

		// DOES NOT PUBLISH. The other direction matters as much: `outputs:` is not a synonym
		// for publishing, and a fix that made every one of them an act would be a gate that
		// reds on a correct release definition.
		{name: "the local exporter writes to disk", yaml: "outputs: type=local,dest=./out", ctx: &dispatch},
		{name: "the tar exporter writes to disk", yaml: "outputs: type=tar,dest=out.tar", ctx: &dispatch},
		{name: "the docker exporter is what `load: true` means", yaml: "outputs: type=docker", ctx: &dispatch},
		{name: "the oci exporter writes a layout", yaml: "outputs: type=oci,dest=out.tar", ctx: &dispatch},
		{name: "cacheonly writes no image at all", yaml: "outputs: type=cacheonly", ctx: &dispatch},
		{
			name: "the image exporter keeps its result locally unless told to push",
			yaml: "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev",
			ctx:  &dispatch,
		},
		{
			name: "and an explicit push=false does not publish",
			yaml: "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev,push=false",
			ctx:  &dispatch,
		},
		{
			name: "several local lines are still local",
			yaml: "outputs: |\n  type=local,dest=out\n  type=cacheonly\n",
			ctx:  &dispatch,
		},

		// UNDECIDABLE. None of these may come back as a quiet "no".
		{
			name:    "a context the run did not produce is an error, not a false",
			yaml:    "outputs: ${{ vars.DEV_OUTPUT }}",
			ctx:     &dispatch,
			wantErr: "cannot be decided",
		},
		{
			name:    "an exporter this gate does not model is an error, not a local build",
			yaml:    "outputs: type=quay-direct,name=ghcr.io/nschatz/holdfast:dev",
			ctx:     &dispatch,
			wantErr: "does not model",
		},
		{
			name:    "a line that names no type= selects an unknown exporter",
			yaml:    "outputs: dest=./out",
			ctx:     &dispatch,
			wantErr: "names no `type=`",
		},
		{
			name:    "a push= inside the spec that is not a boolean is an error",
			yaml:    "outputs: type=image,name=ghcr.io/nschatz/holdfast:dev,push=yes",
			ctx:     &dispatch,
			wantErr: "is not a boolean",
		},
		{
			name:    "a line that is not key=value at all is an error",
			yaml:    "outputs: registry",
			ctx:     &dispatch,
			wantErr: "cannot be read as buildx output attributes",
		},
		{
			name:    "an empty value says nothing and must not be read as no",
			yaml:    `outputs: ""`,
			ctx:     &dispatch,
			wantErr: "is empty",
		},
		{
			name:    "a non-string value is an error",
			yaml:    "outputs: true",
			ctx:     &dispatch,
			wantErr: "not a list of buildx output specifications",
		},

		// The static question, asked by the runbook cross-check: no event decides it, so an
		// expression counts as publishing rather than disappearing.
		{
			name:      "with no event to decide against, an expression counts as publishing",
			yaml:      "outputs: type=image,name=ghcr.io/nschatz/holdfast:${{ github.ref_name }},push=true",
			ctx:       nil,
			publishes: true,
		},
		{name: "a literal local spec is still decided with no event", yaml: "outputs: type=local,dest=out", ctx: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var with map[string]any
			if err := yaml.Unmarshal([]byte(tc.yaml), &with); err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			got, detail, err := decideOutputsInput("outputs", with["outputs"], tc.ctx)
			switch {
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("wanted an error mentioning %q, got publishes=%v detail=%q and no error.\nAn undecidable destination read as a decision is the fail-open this function exists to close.", tc.wantErr, got, detail)
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

// The two inputs are ALTERNATIVES, not a conjunction: a step publishes if either says so.
// The case that shipped the hole is the third one - no `push:` key at all.
func TestActs_EitherDestinationInputMakesTheStepPublish(t *testing.T) {
	ctx := ctxFor("workflow_dispatch")
	cases := []struct {
		name      string
		with      map[string]any
		publishes bool
	}{
		{"push: true alone", map[string]any{"push": true, "tags": "ghcr.io/nschatz/holdfast:dev"}, true},
		{"outputs: type=registry alone", map[string]any{"outputs": "type=registry"}, true},
		{"outputs with push=true and NO push: key", map[string]any{"outputs": "type=image,name=ghcr.io/nschatz/holdfast:dev,push=true"}, true},
		{"push: false but outputs publishes", map[string]any{"push": false, "outputs": "type=registry"}, true},
		{"both spellings at once is still ONE act", map[string]any{"push": true, "outputs": "type=registry"}, true},
		{"neither publishes", map[string]any{"load": true, "tags": "holdfast:release"}, false},
		{"a local outputs does not publish", map[string]any{"outputs": "type=docker"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Step{Name: "build", Uses: "docker/build-push-action@v6", With: tc.with}
			acts, err := s.Acts(&ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := 0
			if tc.publishes {
				want = 1
			}
			if len(acts) != want {
				t.Fatalf("got %d act(s), want %d: %+v", len(acts), want, acts)
			}
		})
	}
}

// The references a push publishes have the same two spellings as the destination does, and
// reading only `tags:` made an absent `tags:` key mean "this push names nothing I need to
// check" - the same absent-key inference, one property along.
func TestPushedRefs_ReadsBothSpellings(t *testing.T) {
	ctx := ctxFor("push")
	cases := []struct {
		name string
		with map[string]any
		want []string
	}{
		{"tags only", map[string]any{"tags": "ghcr.io/nschatz/holdfast:v0.1.0"}, []string{"ghcr.io/nschatz/holdfast:v0.1.0"}},
		{"comma-separated tags", map[string]any{"tags": "a:1,b:2"}, []string{"a:1", "b:2"}},
		{
			"an outputs name=, with no tags: key at all",
			map[string]any{"outputs": "type=image,name=ghcr.io/nschatz/holdfast:latest,push=true"},
			[]string{"ghcr.io/nschatz/holdfast:latest"},
		},
		{
			"several outputs lines",
			map[string]any{"outputs": "type=image,name=a:1,push=true\ntype=image,name=b:2,push=true\n"},
			[]string{"a:1", "b:2"},
		},
		{
			"an interpolated name",
			map[string]any{"outputs": "type=image,name=ghcr.io/nschatz/holdfast:${{ github.ref_name }},push=true"},
			[]string{"ghcr.io/nschatz/holdfast:v0.1.0"},
		},
		{"a local output names nothing pushed", map[string]any{"outputs": "type=local,dest=out"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Step{Name: "build", Uses: "docker/build-push-action@v6", With: tc.with}
			got, err := s.PushedRefs(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("PushedRefs = %v, want %v", got, tc.want)
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

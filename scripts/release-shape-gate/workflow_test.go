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

// The `run:` half of the catalogue is read by ONE function, and having one is the property
// being pinned here. Two readers - a comment stripper per physical line, then a continuation
// joiner - disagreed about what a line is: the stripper reset its quote state at exactly the
// boundary the joiner erased, so a `#` inside a quoted argument on a continuation line was
// read as a comment and the rest of the LOGICAL line was deleted, backslash and `--push`
// included. The shell runs that text and pushes.
//
// So the reader is asserted against what the shell does, one shape at a time, in both
// directions: a command spread over continuations is one command, and a comment is still
// prose.
func TestShellCommands(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"nothing to join", "docker push a\n", []string{"docker push a"}},
		{
			"a flag on the line below its command is the same command",
			"docker buildx build \\\n  --push \\\n  -t ghcr.io/o/r:dev \\\n  .\n",
			[]string{"docker buildx build --push -t ghcr.io/o/r:dev ."},
		},
		{
			// The shell puts NOTHING in the backslash's place, so a token split across a
			// continuation joins into one word. `push\` + `foo` is `pushfoo`, which is not a
			// push - inventing a word boundary here would report an act the definition
			// cannot perform.
			"no separator is invented",
			"docker\\\n push a\n",
			[]string{"docker push a"},
		},
		{
			"a token split across the join stays one token",
			"docker pu\\\nsh a\n",
			[]string{"docker push a"},
		},
		{
			// PARITY, not presence, and it falls out of reading `\` as an escape rather than
			// being a rule of its own: `\\` is an escaped literal backslash, so the newline
			// after it ends the command and the next line is its own.
			"an escaped backslash ends the command",
			"docker buildx build -t x . \\\\\n  --push\n",
			[]string{`docker buildx build -t x . \`, "--push"},
		},
		{
			"three backslashes is an escaped one plus a continuation",
			"a \\\\\\\nb\n",
			[]string{`a \b`},
		},
		{"a continuation on the last line continues into nothing", "docker push a \\", []string{"docker push a"}},
		{"a lone backslash line", "\\\nb\n", []string{"b"}},
		{"CRLF joins too", "docker buildx build \\\r\n  --push\r\n", []string{"docker buildx build --push"}},
		{"empty input", "", nil},

		// THE DEFECT THE ONE-READER SHAPE CLOSES. A quote opened on one physical line is
		// still open on the next, so the `#` is text and the whole command survives.
		{
			"a quote opened before a continuation is still open after it",
			"docker buildx build \\\n  --annotation \"org.opencontainers.image.description=dev, \\\n  see #123\" \\\n  --push \\\n  -t ghcr.io/o/r:dev .\n",
			[]string{"docker buildx build --annotation org.opencontainers.image.description=dev, see #123 --push -t ghcr.io/o/r:dev ."},
		},
		{
			"and the other direction: a # that really does start a word is a comment",
			"docker buildx build --load . \\\n  -t r:dev\n  # --push is not used here\n",
			[]string{"docker buildx build --load . -t r:dev"},
		},
		{
			"a # inside a word is not a comment",
			"os=${target#*/}\ndocker push a\n",
			[]string{"os=${target#*/}", "docker push a"},
		},

		// A quote that is never closed is not a quote: the shell would refuse the whole
		// script, so reading its contents as inert is the fail-open direction.
		{
			"an unterminated quote does not swallow the commands after it",
			"printf '%s' 'a\ndocker push ghcr.io/o/r:dev\n",
			[]string{"printf %s 'a", "docker push ghcr.io/o/r:dev"},
		},

		// A command hidden in a substitution is still a command.
		{
			"a command substitution is a script in its own right",
			"ref=\"$(docker push ghcr.io/o/r:dev)\"\n",
			[]string{"docker push ghcr.io/o/r:dev", "ref=$(...)"},
		},

		{
			"redirections are plumbing, not argv",
			"docker image rm -f \"$REF\" >/dev/null 2>&1 || true\n",
			[]string{"docker image rm -f $REF", "true"},
		},
		{
			"separators end a command",
			"a; b && c || d | e\n",
			[]string{"a", "b", "c", "d", "e"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range ShellCommands(tc.in) {
				got = append(got, c.String())
			}
			if strings.Join(got, " | ") != strings.Join(tc.want, " | ") {
				t.Fatalf("ShellCommands(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// THE DESTINATION MODEL, which the `run:` half had none of. Every row here is a command
// whose act is decided from WHERE IT SENDS WHAT IT BUILT rather than from whether its text
// happens to match a spelling somebody wrote a pattern for, and the local rows matter as
// much as the publishing ones: a fix that red every `--output=` would be the false positive
// this table forbids.
func TestCommandAct_TheRunHalfDecidesADestination(t *testing.T) {
	cases := []struct {
		name string
		run  string
		kind ActKind
	}{
		// Publishing, in every spelling of the one destination.
		{"the flag the old catalogue knew", "docker buildx build --push -t ghcr.io/o/r:dev .", ActImagePush},
		{"the longhand it is shorthand FOR", "docker buildx build --output=type=registry,name=ghcr.io/o/r:dev .", ActImagePush},
		{"the short flag, with the image exporter", "docker buildx build -o type=image,name=ghcr.io/o/r:dev,push=true .", ActImagePush},
		{"the space-separated form", "docker buildx build --output type=registry .", ActImagePush},
		{"the plainest push", "docker push ghcr.io/o/r:dev", ActImagePush},
		{"the management-command spelling of it", "docker image push ghcr.io/o/r:dev", ActImagePush},
		{"podman takes docker's command surface", "podman manifest push ghcr.io/o/r:dev", ActTagMove},
		{"a floating reference being moved", "docker buildx imagetools create -t ghcr.io/o/r:latest ghcr.io/o/r:v1", ActTagMove},
		{"a published release", `gh release create v0.0.0 --notes x`, ActRelease},
		{"a ref reaching the remote", "git push origin v0.0.0", ActRefPush},
		{"a wrapper does not hide the act", "sudo docker push ghcr.io/o/r:dev", ActImagePush},
		{"nor does another one", "xargs -n1 docker push", ActImagePush},

		// Local, and it has to stay local.
		{"the docker exporter writes a tar", "docker buildx build --output=type=docker,dest=/tmp/img.tar .", ""},
		{"--load IS the docker exporter", "docker buildx build --load -t r:dev .", ""},
		{"an image exporter with no push= keeps its result", "docker buildx build -o type=image,name=r:dev .", ""},
		{"an explicit push=false does not publish", "docker buildx build --push=false -t r:dev .", ""},
		{"no destination flag at all", "docker buildx build -t r:dev .", ""},
		{"a pull reads FROM a registry", `docker pull --platform linux/arm64 "$REF"`, ""},
		{"a local image removal", `docker image rm -f "$REF"`, ""},
		{"inspecting a manifest reads it", `docker buildx imagetools inspect "$REF"`, ""},
		{"authenticating is not publishing", "docker login ghcr.io -u x --password-stdin", ""},
		{"a local tag is not a push", "docker tag a b", ""},
		{"a program outside the tool set is not an act", "./scripts/smoke-image.sh holdfast:release", ""},
		{"nor is the gate itself", "make check", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmds := ShellCommands(tc.run)
			if len(cmds) != 1 {
				t.Fatalf("fixture is not one command: %v", cmds)
			}
			kind, why, err := cmds[0].Act()
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if kind != tc.kind {
				t.Fatalf("Act() = %q (%s), want %q", kind, why, tc.kind)
			}
		})
	}
}

// THE EDGE, which is the whole difference between this catalogue and the one it replaced.
// An invocation of a tool that can reach a registry has to land on a rule; one that does not
// is UNDECIDED, and undecided reds by name. None of the first four commands below appears
// anywhere in command.go, which is the point - a catalogue that has to grow a row per
// spelling has already lost to the next spelling.
func TestCommandAct_AnUnmodelledRegistryCommandIsAnErrorNotSilence(t *testing.T) {
	for _, run := range []string{
		"crane push image.tar ghcr.io/o/r:dev",
		"crane copy ghcr.io/o/r:dev ghcr.io/o/r:latest",
		"regctl image copy ghcr.io/o/r:dev ghcr.io/o/r:latest",
		"skopeo delete docker://ghcr.io/o/r:dev",
		"docker frobnicate ghcr.io/o/r:dev",
		"gh api -X POST /repos/o/r/git/refs",
		"docker buildx bake release",
		"docker buildx build --output=type=quay-direct,name=ghcr.io/o/r:dev .",
	} {
		t.Run(run, func(t *testing.T) {
			cmds := ShellCommands(run)
			if len(cmds) != 1 {
				t.Fatalf("fixture is not one command: %v", cmds)
			}
			kind, why, err := cmds[0].Act()
			if err == nil {
				t.Fatalf("Act() = %q (%s) and no error, so an invocation this gate has never been taught reads as harmless", kind, why)
			}
			if !strings.Contains(err.Error(), strings.Fields(run)[0]) {
				t.Errorf("the refusal does not name what it saw:\n%v", err)
			}
		})
	}
	// The OTHER direction, so "everything reds" cannot pass for an edge.
	for _, run := range []string{"docker pull x", "gh release view v0.1.0", "git status", "npm ci"} {
		cmds := ShellCommands(run)
		if kind, _, err := cmds[0].Act(); err != nil || kind != "" {
			t.Errorf("%q gave (%q, %v), want no act and no error - stating the boundary must not become a refusal of every command", run, kind, err)
		}
	}
}

// The two lists are one list. shell.go stubs a binary because it could reach a registry;
// command.go requires every invocation of one to be decided. A tool in one and not the other
// is either a command the gate decides and then EXECUTES for real, or a command it
// neutralises and then reads as harmless - both of which are how this gate fails silently.
func TestStubbedCommandsAreExactlyTheToolsTheGateClassifies(t *testing.T) {
	want := map[string]bool{}
	for name := range registryTools {
		want[name] = true
	}
	for name := range clientsCheckedAndNotInventoried {
		want[name] = true
	}
	got := map[string]bool{}
	for _, name := range stubbed {
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%q is classified by the act catalogue but not stubbed, so a step invoking it would run for real", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%q is stubbed but classified nowhere, so an invocation of it would read as harmless", name)
		}
	}
}

// The normalisation's whole point is what the DETECTORS then see, so that is asserted too -
// in both directions, because "join the lines" must not turn every continued build into a
// publish.
func TestActs_ReadsTheLogicalLineNotThePhysicalOne(t *testing.T) {
	cases := []struct {
		name      string
		run       string
		publishes bool
	}{
		{"a continued buildx --push publishes", "docker buildx build \\\n  --push \\\n  -t ghcr.io/o/r:dev .\n", true},
		{"a continued gh release create publishes", "gh release \\\n  create v0.0.0 \\\n  --notes x\n", true},
		{"a continued docker push publishes", "docker \\\n push ghcr.io/o/r:dev\n", true},
		{"a continued build with no --push is local", "docker buildx build \\\n  --load \\\n  -t r:dev .\n", false},
		{"an escaped backslash does not splice --push onto the build", "docker buildx build -t r:dev . \\\\\n  --push\n", false},
		{"`--push` only in a comment is still prose", "docker buildx build --load . \\\n  -t r:dev\n# and never --push\n", false},

		// A `#` inside a quoted argument, on a continuation line, is TEXT. An OCI annotation
		// or a release note carrying an issue number is the everyday way it arrives, and
		// reading it as a comment deleted the rest of the logical line - the `--push`
		// included, and the backslash that would have joined it.
		{
			"a quoted # on a continuation line does not eat the push below it",
			"docker buildx build \\\n  --annotation \"org.opencontainers.image.description=dev, \\\n  see #123\" \\\n  --push \\\n  -t ghcr.io/o/r:dev .\n",
			true,
		},
		{
			"and the same argument on a build that stays local is still local",
			"docker buildx build \\\n  --annotation \"org.opencontainers.image.description=dev, \\\n  see #123\" \\\n  --load \\\n  -t r:dev .\n",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Step{Name: "dev", Run: tc.run}
			acts, err := s.Acts(nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := len(acts) > 0; got != tc.publishes {
				t.Fatalf("Acts found %d act(s) (%+v), want publishes=%v", len(acts), acts, tc.publishes)
			}
		})
	}
}

// The catalogue's EDGE. `usesDetectors` answers "does this action publish?" with yes or with
// silence, and silence read as no - so an action in neither half contributed no act AND no
// message, and "NONE of them publishes anything" covered a step nobody had asked about.
func TestActs_AnUnclassifiedActionIsAnErrorNotSilence(t *testing.T) {
	s := Step{Name: "publish a dev image", Uses: "docker/bake-action@v5", With: map[string]any{"push": true}, Index: 3}
	acts, err := s.Acts(nil)
	if err == nil {
		t.Fatalf("Acts returned %d act(s) and no error for an action in neither half of the catalogue", len(acts))
	}
	for _, want := range []string{`step 4 "publish a dev image"`, "docker/bake-action@v5", "does not classify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
	// The other direction: stating the boundary must not become a refusal of every `uses:`.
	local := Step{Name: "log in to GHCR", Uses: "docker/login-action@v3"}
	if got, err := local.Acts(nil); err != nil || len(got) != 0 {
		t.Fatalf("a classified non-publishing action gave (%v, %v), want no acts and no error", got, err)
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

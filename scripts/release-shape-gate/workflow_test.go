package main

// The gate's own unit tests. They are about the two questions that replaced "what does this
// step's text do?", because that question is undecidable over arbitrary shell and six
// ordinals of adversarial review proved it one spelling at a time:
//
//   1. CAN this job publish? - decided from `permissions:`, `secrets:` and the job's keys.
//   2. WHICH step is this? - decided from the step's declared `id:` and a WHOLE-VALUE
//      comparison of what it invokes.
//
// Every test below is bidirectional on purpose: a check that cannot fail is not evidence,
// so each property is asserted in both directions - the thing that must red, and the
// legitimate spelling that must not.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// mustNode parses a YAML mapping and returns its node, which is what the key checks read.
func mustNode(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("cannot parse %q: %v", body, err)
	}
	return doc.Content[0]
}

// load parses a workflow from source, failing the test if it will not parse.
func load(t *testing.T, body string) *Workflow {
	t.Helper()
	p := filepath.Join(t.TempDir(), "release.yml")
	if err := writeFile(p, body); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadWorkflow(p)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	return wf
}

// --- CAN it publish? --------------------------------------------------------------------

// The claim the whole design rests on: a job that holds nothing cannot publish, whatever
// its steps say. Every spelling that beat the readers of ordinals 1 through 6 is in here,
// and none of them may red - because none of them can publish from a job with no grant.
func TestCanPublish_WhatAStepSaysIsIrrelevantWithoutAGrant(t *testing.T) {
	spellings := []string{
		`docker push ghcr.io/x/y:latest`,                        // F1
		`docker buildx build --push .`,                          // F5
		"docker buildx build \\\n  --push .",                    // F7 (a line continuation)
		`docker buildx build --output=type=registry .`,          // F9
		`sh -c "docker push ghcr.io/x/y:latest"`,                // F11
		`eval "docker push ghcr.io/x/y:latest"`,                 // F11b
		`docker buildx build -otype=registry .`,                 // F12
		`export PATH=/usr/bin:/bin; docker push ghcr.io/x/y:v1`, // F14
		`exec docker push ghcr.io/x/y:v1`,                       // F15
		`gh release create v1.0.0`,
		`git push origin v1.0.0`,
		`rclone copy dist remote:bucket`,
	}
	for _, s := range spellings {
		wf := load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: x
        run: `+"|\n          "+strings.ReplaceAll(s, "\n", "\n          ")+`
`)
		problems, err := CanPublish(wf, wf.Jobs["build"])
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if len(problems) != 0 {
			t.Fatalf("%q was reported as able to publish from a job granted `contents: read` and no secret: %v.\nThat is the reader coming back. A step publishes nothing it holds no credential for, and this job holds none.", s, problems)
		}
	}
}

// The other direction, or the property above would be satisfied by a function that always
// returns nothing.
func TestCanPublish_AWriteGrantIsACapability(t *testing.T) {
	for _, perms := range []string{
		"      contents: write\n",
		"      packages: write\n",
		"      id-token: write\n",
	} {
		wf := load(t, `
name: t
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
`+perms+`    steps:
      - id: x
        run: echo hello
`)
		problems, err := CanPublish(wf, wf.Jobs["build"])
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) == 0 {
			t.Fatalf("a job granted %q was reported as unable to publish, even though its step says nothing at all - the grant is the capability", strings.TrimSpace(perms))
		}
	}
}

func TestEffectiveGrants_AnUnstatedGrantReadsClosed(t *testing.T) {
	wf := load(t, `
name: t
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - id: x
        run: echo hello
`)
	if _, err := EffectiveGrants(wf, wf.Jobs["build"]); err == nil {
		t.Fatal("neither the workflow nor the job declares `permissions:`, so the repository default applies - which this file cannot see and which may be write-all. That must be an error, not an assumed `none`")
	}

	// Stated at the workflow level and inherited: decidable, so not an error.
	wf = load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - id: x
        run: echo hello
`)
	g, err := EffectiveGrants(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatalf("an inherited workflow-level grant is decidable: %v", err)
	}
	if len(g.Writes()) != 0 {
		t.Fatalf("contents: read is not a write: %v", g.Writes())
	}

	// A job's own block REPLACES the workflow's; GitHub does not merge them.
	wf = load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      packages: write
    steps:
      - id: x
        run: echo hello
`)
	g, err = EffectiveGrants(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Writes(); len(got) != 1 || got[0] != "packages" {
		t.Fatalf("the job's own permissions must replace the workflow's; got %v", got)
	}
}

func TestParseGrants_UnknownScopeOrValueReds(t *testing.T) {
	cases := []struct{ name, perms string }{
		{"an unknown scope", "      contentz: read\n"},
		{"an unknown value", "      contents: maybe\n"},
		{"an unknown shorthand", "    permissions: some-all\n"},
	}
	for _, tc := range cases {
		body := `
name: t
jobs:
  build:
    runs-on: ubuntu-latest
`
		if strings.HasPrefix(tc.perms, "    permissions:") {
			body += tc.perms
		} else {
			body += "    permissions:\n" + tc.perms
		}
		body += `    steps:
      - id: x
        run: echo hello
`
		wf := load(t, body)
		if _, err := EffectiveGrants(wf, wf.Jobs["build"]); err == nil {
			t.Fatalf("%s must red by name; a grant this gate cannot read is not one it may treat as harmless", tc.name)
		}
	}

	// write-all is readable and IS a write on everything.
	wf := load(t, `
name: t
jobs:
  build:
    runs-on: ubuntu-latest
    permissions: write-all
    steps:
      - id: x
        run: echo hello
`)
	g, err := EffectiveGrants(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Writes()) != len(permissionScopes) {
		t.Fatalf("write-all grants a write on every scope; got %v", g.Writes())
	}
}

func TestCanPublish_ASecretOtherThanTheScopedTokenIsACapability(t *testing.T) {
	// GITHUB_TOKEN alone is bounded by the very permissions block above it.
	wf := load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: x
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: echo hello
`)
	problems, err := CanPublish(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("secrets.GITHUB_TOKEN is scoped by `permissions:`, which is read-only here, so it authorises nothing: %v", problems)
	}

	// Any other secret is a value this file cannot see and `permissions:` does not bound.
	for _, ref := range []string{
		"${{ secrets.RELEASE_PAT }}",
		"${{ secrets['RELEASE_PAT'] }}",
		"${{ toJSON(secrets) }}",
	} {
		wf := load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: x
        env:
          TOKEN: "`+ref+`"
        run: echo hello
`)
		problems, err := CanPublish(wf, wf.Jobs["build"])
		if err != nil {
			t.Fatal(err)
		}
		if len(problems) == 0 {
			t.Fatalf("%s hands the job a credential whose scope `permissions:` does not bound; it must red", ref)
		}
	}
}

// The three places a secret reaches a job, all of which must be seen. The workflow's own
// `env:` is the one a scan of the JOB node alone would miss: it appears nowhere inside the
// job and every job inherits it.
func TestCanPublish_ASecretIsSeenWhereverItReachesTheJob(t *testing.T) {
	cases := map[string]string{
		"the workflow's top-level env": `
name: t
permissions:
  contents: read
env:
  GHCR_PAT: ${{ secrets.GHCR_PUBLISH_PAT }}
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: x
        run: echo hello
`,
		"the job's own env": `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    env:
      GHCR_PAT: ${{ secrets.GHCR_PUBLISH_PAT }}
    steps:
      - id: x
        run: echo hello
`,
		"an action input, which is not a key this gate models": `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: login
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          password: ${{ secrets.GHCR_PUBLISH_PAT }}
`,
	}
	for where, body := range cases {
		wf := load(t, body)
		problems, err := CanPublish(wf, wf.Jobs["build"])
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		named := false
		for _, p := range problems {
			if strings.Contains(p.Detail, "GHCR_PUBLISH_PAT") {
				named = true
			}
		}
		if !named {
			t.Fatalf("a repository secret in %s reaches this job and was not named: %v", where, problems)
		}
	}
}

func TestCanPublish_AnUnclassifiedKeyReadsClosed(t *testing.T) {
	// A key that carries a capability, named with what it hands over.
	wf := load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    environment: production
    steps:
      - id: x
        run: echo hello
`)
	problems, err := CanPublish(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "environment") {
		t.Fatalf("`environment:` hands a job that environment's secrets; it must red by name. Got %v", problems)
	}

	// A key nobody has classified at all reds too, rather than contributing silence.
	wf = load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    some-future-key: whatever
    steps:
      - id: x
        run: echo hello
`)
	problems, err = CanPublish(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "some-future-key") {
		t.Fatalf("an unclassified job key must red by name, not read as harmless. Got %v", problems)
	}

	// And the classified ones must NOT red, or the rule reds every workflow.
	wf = load(t, `
name: t
permissions:
  contents: read
jobs:
  build:
    name: build it
    runs-on: ubuntu-latest
    timeout-minutes: 30
    concurrency: release
    permissions:
      contents: read
    outputs:
      v: ${{ steps.x.outputs.v }}
    env:
      A: b
    steps:
      - id: x
        name: a step
        if: always()
        shell: bash
        working-directory: .
        timeout-minutes: 5
        run: echo hello
`)
	problems, err = CanPublish(wf, wf.Jobs["build"])
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("ordinary job and step keys must not red: %v", problems)
	}
}

// --- WHICH step is this? ------------------------------------------------------------------

// F13, closed by construction: a role is what a step DECLARES and INVOKES, not what its
// text mentions. `echo "make check"` cannot be the full gate, and no quoting reaches the
// comparison because nothing is searched for inside anything.
// roleByID returns the SHIPPED declaration, so every test below grades the table this gate
// actually runs with rather than a hand-built lookalike that can drift away from it.
func roleByID(t *testing.T, id string) role {
	t.Helper()
	for _, r := range releaseRoles {
		if r.id == id {
			return r
		}
	}
	t.Fatalf("no role declares id %q", id)
	return role{}
}

func TestRoleMatch_ARoleCannotBeClaimedByMentioningIt(t *testing.T) {
	gate := roleByID(t, "full-gate")
	mustFail := []string{
		`echo "make check"`,
		`printf '%s\n' "make check"`,
		`echo make check`,
		`"make" check`,
		`make build`,
		`make check-foo`,
		"make check\nmake vet",
		``,
	}
	for _, run := range mustFail {
		if err := gate.matches(Step{Run: run, ID: "full-gate"}); err == nil {
			t.Fatalf("%q was accepted as the full gate", run)
		}
	}
	mustPass := []string{
		`make check`,
		`make -C . check`,
		`  make check  `,
	}
	for _, run := range mustPass {
		if err := gate.matches(Step{Run: run, ID: "full-gate"}); err != nil {
			t.Fatalf("%q really is the full gate, and was refused: %v", run, err)
		}
	}
}

// S0046 F18. Naming the program and carrying the fields the role requires is NOT enough:
// `make -n check` is GNU make's dry-run mode, so it prints every recipe in the gate,
// executes none of them and exits 0. The role held, the gate printed its order sentence, and
// a tag push would have published an image whose `make check` never ran.
//
// The fix is not a list of bad flags - that is the shape that lost six times in this gate's
// other half. Every field a role's invocation carries must be one the role DECLARED, so the
// spellings below are refused by one sentence, along with the one nobody has thought of.
func TestRoleFields_AFieldTheRoleNeverDeclaredIsRefused(t *testing.T) {
	gate := roleByID(t, "full-gate")
	neutered := []string{
		`make -n check`,              // dry run: prints the recipes, runs none
		`make --dry-run check`,       // the same, spelled long
		`make -q check`,              // question mode: runs nothing, answers up-to-date
		`make -t check`,              // touch mode: marks the targets made, runs nothing
		`make --touch check`,         //
		`make -Bn check`,             // clustered, so no single-flag comparison sees the -n
		`make -n -C . check`,         // beside a field that IS declared
		`make check SHELL=/bin/true`, // SHELL is a make variable; every recipe becomes a no-op
		`make check MAKEFLAGS=-n`,    // the same neuter as an operand
		`make -f /dev/null check`,    // a different Makefile's check, which is empty
		`make -C /tmp check`,         // a declared field, an undeclared VALUE
		`make -C`,                    // a declared field with no value at all
	}
	for _, run := range neutered {
		if err := gate.matches(Step{Run: run, ID: "full-gate"}); err == nil {
			t.Fatalf("%q holds the full-gate role while running no gate", run)
		}
	}
	// And the other direction, or the above would be satisfied by a role that refuses
	// everything: a real invocation of the gate, respelt, still holds it.
	for _, run := range []string{`make check`, `make -C . check`} {
		if err := gate.matches(Step{Run: run, ID: "full-gate"}); err != nil {
			t.Fatalf("%q really is the full gate, and was refused: %v", run, err)
		}
	}
}

func TestRoleMatch_TheSmokeRunsAreToldApartByWhatTheyPass(t *testing.T) {
	amd := roleByID(t, "smoke-amd64")
	arm := roleByID(t, "smoke-arm64")

	if err := amd.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release"}); err != nil {
		t.Fatalf("the amd64 smoke run was refused: %v", err)
	}
	if err := amd.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release --no-encode"}); err == nil {
		t.Fatal("the amd64 role must drive a REAL encode; --no-encode makes it an exec check")
	}
	// The hole a mustNotField-only role leaves: with no required field, the role was held by
	// an invocation that smokes no image at all and exits 2 on its own usage message.
	if err := amd.matches(Step{Run: "./scripts/smoke-image.sh"}); err == nil {
		t.Fatal("a smoke run with no image argument smokes nothing and must not hold the role")
	}
	if err := arm.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release-arm64 linux/arm64 --no-encode"}); err != nil {
		t.Fatalf("the arm64 smoke run was refused: %v", err)
	}
	// --no-encode is OPTIONAL there. Dropping it makes the arm64 run STRICTER, and a role
	// must not refuse a step that does more than it promises.
	if err := arm.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release-arm64 linux/arm64"}); err != nil {
		t.Fatalf("an arm64 smoke run that also encodes was refused: %v", err)
	}
	if err := arm.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release"}); err == nil {
		t.Fatal("a run that names no architecture cannot be the arm64 smoke run")
	}
}

func TestRoleMatch_AnActionRoleIsTheActionItNames(t *testing.T) {
	push := roleByID(t, "push-version")
	real := map[string]any{
		"context":   ".",
		"platforms": "linux/amd64,linux/arm64",
		"push":      true,
		"tags":      "ghcr.io/x/y:v0.1.0",
	}
	if err := push.matches(Step{Uses: "docker/build-push-action@v6", With: real}); err != nil {
		t.Fatalf("the real push step was refused: %v", err)
	}
	for _, uses := range []string{"", "actions/checkout@v4", "evil/docker-build-push-action@v6"} {
		if err := push.matches(Step{Uses: uses, With: real}); err == nil {
			t.Fatalf("%q is not docker/build-push-action and must not hold that role", uses)
		}
	}
}

// The action-role half of F18's class. `push: false` and an unclassified input each leave
// the step holding the version-tag-push role while nothing is published, and neither
// changes the action's name.
func TestRoleInputs_AnActionRoleIsAccountedForInputByInput(t *testing.T) {
	push := roleByID(t, "push-version")
	base := func() map[string]any {
		return map[string]any{
			"context":   ".",
			"platforms": "linux/amd64,linux/arm64",
			"push":      true,
			"tags":      "ghcr.io/x/y:v0.1.0",
		}
	}
	off := base()
	off["push"] = false
	if err := push.matches(Step{Uses: "docker/build-push-action@v6", With: off}); err == nil {
		t.Fatal("`push: false` publishes nothing and must not hold the version-tag-push role")
	}
	none := base()
	delete(none, "push")
	if err := push.matches(Step{Uses: "docker/build-push-action@v6", With: none}); err == nil {
		t.Fatal("an action role with no `push:` input at all must be refused")
	}
	diverted := base()
	diverted["outputs"] = "type=local,dest=./out"
	if err := push.matches(Step{Uses: "docker/build-push-action@v6", With: diverted}); err == nil {
		t.Fatal("an unclassified action input reads CLOSED: `outputs: type=local` sends the build to a directory")
	}
}

// A role step's ENVIRONMENT is part of what its invocation does. `MAKEFLAGS: -n` neuters
// `run: make check` without touching one character of the `run:` line, so an environment
// name nobody classified reds - at any of the three levels that reach the step.
func TestRoleEnv_AnUnclassifiedNameInScopeReadsClosed(t *testing.T) {
	gate := roleByID(t, "full-gate")
	step := Step{Run: "make check", ID: "full-gate", JobID: "build"}
	inert := map[string]any{"GO_VERSION": "1.25.14"}

	if err := gate.checkEnv(&Workflow{Env: inert}, Job{ID: "build"}, step); err != nil {
		t.Fatalf("a classified, inert name was refused: %v", err)
	}
	for _, at := range []string{"workflow", "job", "step"} {
		wf, job, s := &Workflow{Env: inert}, Job{ID: "build"}, step
		hostile := map[string]any{"MAKEFLAGS": "-n"}
		switch at {
		case "workflow":
			wf = &Workflow{Env: map[string]any{"GO_VERSION": "1.25.14", "MAKEFLAGS": "-n"}}
		case "job":
			job.Env = hostile
		case "step":
			s.Env = hostile
		}
		if err := gate.checkEnv(wf, job, s); err == nil {
			t.Fatalf("MAKEFLAGS at the %s level neuters `make check` and must red", at)
		}
	}
	// The names a role's own script reads are declared by that role, and only by it.
	promote := roleByID(t, "promote-latest")
	withEnv := Step{Run: "./scripts/release-promote.sh", ID: "promote-latest", JobID: "publish",
		Env: map[string]any{"IMAGE": "x", "VERSION": "v0.1.0", floatingTagEnv: "latest"}}
	if err := promote.checkEnv(&Workflow{Env: inert}, Job{ID: "publish"}, withEnv); err != nil {
		t.Fatalf("the promotion's own declared env was refused: %v", err)
	}
	if err := gate.checkEnv(&Workflow{Env: inert}, Job{ID: "build"}, Step{Run: "make check", ID: "full-gate", JobID: "build",
		Env: map[string]any{"IMAGE": "x"}}); err == nil {
		t.Fatal("a name declared by ANOTHER role must not be accepted here")
	}
}

// The step-key half. Neither of these touches the `run:` line, and either makes it do
// something else: `shell: cat` prints the script and exits 0.
func TestRoleStepKeys_AKeyThatChangesTheInvocationIsRefused(t *testing.T) {
	gate := roleByID(t, "full-gate")
	for _, body := range []string{
		"name: g\nid: full-gate\nrun: make check\n",
		"name: g\nid: full-gate\nrun: make check\nif: always()\n",
		"name: g\nid: full-gate\nrun: make check\ntimeout-minutes: 30\n",
	} {
		if err := gate.checkStepKeys(Step{Node: mustNode(t, body)}); err != nil {
			t.Fatalf("an inert set of keys was refused (%s): %v", body, err)
		}
	}
	for _, body := range []string{
		"name: g\nid: full-gate\nrun: make check\nshell: cat\n",
		"name: g\nid: full-gate\nrun: make check\nworking-directory: /tmp\n",
		"name: g\nid: full-gate\nrun: make check\nsome-future-key: x\n",
	} {
		if err := gate.checkStepKeys(Step{Node: mustNode(t, body)}); err == nil {
			t.Fatalf("a key that changes what the invocation does was accepted: %s", body)
		}
	}
}

// A `defaults:` block sets the shell and working directory of every `run:` step from one or
// two levels away, where nobody reading the step would see it.
func TestRoleDefaults_ADefaultsBlockOverARoleStepIsRefused(t *testing.T) {
	s := Step{Run: "make check", ID: "full-gate", JobID: "build"}
	if err := checkNoDefaults(&Workflow{}, Job{ID: "build"}, s); err != nil {
		t.Fatalf("a definition with no defaults was refused: %v", err)
	}
	if err := checkNoDefaults(&Workflow{HasDefaults: true}, Job{ID: "build"}, s); err == nil {
		t.Fatal("a workflow-level `defaults:` decides what every role invocation does and must red")
	}
	job := Job{ID: "build", Node: mustNode(t, "runs-on: ubuntu-latest\ndefaults:\n  run:\n    shell: cat\n")}
	if err := checkNoDefaults(&Workflow{}, job, s); err == nil {
		t.Fatal("a job-level `defaults:` over a role step must red")
	}
}

func TestLocateRoles_AMissingOrDoubledRoleIsRefused(t *testing.T) {
	body := `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: full-gate
        run: make check
`
	if _, err := locateRoles(load(t, body), t.TempDir()); err == nil {
		t.Fatal("a definition missing every other role must be refused, not graded over the subset it found")
	}

	body = `
name: t
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - id: plan
        run: echo "publish=false" >> "$GITHUB_OUTPUT"
      - id: full-gate
        run: make check
      - id: full-gate
        run: make check
`
	if _, err := locateRoles(load(t, body), t.TempDir()); err == nil || !strings.Contains(err.Error(), "2 steps declare") {
		t.Fatalf("two steps holding one role leaves the order undefined and must be refused; got %v", err)
	}
}

// --- the `needs:` graph -------------------------------------------------------------------

func TestPrecedes_ConcurrentJobsAreNotOrdered(t *testing.T) {
	wf := load(t, `
name: t
permissions:
  contents: read
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - id: x
        run: echo a
  b:
    runs-on: ubuntu-latest
    steps:
      - id: y
        run: echo b
  c:
    needs: b
    runs-on: ubuntu-latest
    steps:
      - id: z
        run: echo c
`)
	g := &gate{root: ".", out: os.Stdout}
	x := wf.Jobs["a"].Steps[0]
	y := wf.Jobs["b"].Steps[0]
	z := wf.Jobs["c"].Steps[0]

	if _, err := g.precedes(wf, x, y); err == nil {
		t.Fatal("jobs a and b are concurrent; an order that is not in the `needs:` graph is not an order and must be refused rather than guessed")
	}
	ok, err := g.precedes(wf, y, z)
	if err != nil || !ok {
		t.Fatalf("c needs b, so b's step precedes c's: %v %v", ok, err)
	}
	ok, err = g.precedes(wf, z, y)
	if err != nil || ok {
		t.Fatalf("and not the other way round: %v %v", ok, err)
	}
}

// --- A15: every way of reading nothing ------------------------------------------------------

func TestLoadWorkflow_EveryFailureSaysWhichItIs(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, body, want string }{
		{"empty", "", "IS EMPTY"},
		{"unparseable", "jobs:\n  x:\n   - [\n", "CANNOT BE PARSED"},
		{"no jobs", "name: t\non: push\n", "NAMES NO JOB"},
		{"no steps", "name: t\njobs:\n  x:\n    runs-on: ubuntu-latest\n    steps: []\n", "NAMES NO STEP AT ALL"},
	}
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name+".yml")
		if err := writeFile(p, tc.body); err != nil {
			t.Fatal(err)
		}
		_, err := LoadWorkflow(p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want a message containing %q", tc.name, err, tc.want)
		}
	}
	if _, err := LoadWorkflow(filepath.Join(dir, "absent.yml")); err == nil || !strings.Contains(err.Error(), "CANNOT BE READ") {
		t.Fatalf("an absent definition must say so: %v", err)
	}
}

func TestTolerates_AnExpressionIsNotADecidedFalse(t *testing.T) {
	if ok, _ := tolerates(nil); ok {
		t.Fatal("an absent continue-on-error tolerates nothing")
	}
	if ok, _ := tolerates(false); ok {
		t.Fatal("false tolerates nothing")
	}
	if ok, _ := tolerates(true); !ok {
		t.Fatal("true tolerates a failure")
	}
	if ok, why := tolerates("${{ github.event_name == 'push' }}"); !ok || !strings.Contains(why, "${{") {
		t.Fatalf("an expression decides at run time whether a failure counts, and `may not fail the run` is the whole hazard: %v %q", ok, why)
	}
}

// --- which events reach this workflow at all? ---------------------------------------------

// The gate plans four event shapes, and for six ordinals it read `on:` to check they were
// the shapes this workflow has exactly never. `on: push: branches: ["v0.**"]` beside the tag
// filter makes a push to a BRANCH named `v0.9.9` a `push` event whose ref_name is `v0.9.9`,
// which the planning logic - which cannot tell a branch from a tag - reads as a release, and
// every assertion this gate makes stays green because the shape it graded is still the shape
// it invented.
func TestTriggerSurface_AnEventNoShapePlansIsRefused(t *testing.T) {
	shaped := func(on string) *Workflow {
		return load(t, on+`
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - id: plan
        run: echo "publish=false" >> "$GITHUB_OUTPUT"
`)
	}
	committed := "\non:\n  push:\n    tags: [\"v*\"]\n  workflow_dispatch:\n"
	g := &gate{out: io.Discard}
	g.checkTriggerSurface(shaped(committed))
	if g.failed {
		t.Fatal("the committed event surface must pass")
	}

	refused := map[string]string{
		"a branch filter beside the tag filter": "\non:\n  push:\n    tags: [\"v*\"]\n    branches: [\"v0.**\"]\n  workflow_dispatch:\n",
		"a branches-ignore filter":              "\non:\n  push:\n    tags: [\"v*\"]\n    branches-ignore: [\"nope\"]\n  workflow_dispatch:\n",
		"a push with no tag filter":             "\non:\n  push:\n  workflow_dispatch:\n",
		"the short sequence form":               "\non: [push, workflow_dispatch]\n",
		"the short scalar form":                 "\non: push\n",
		"an event nothing plans":                "\non:\n  push:\n    tags: [\"v*\"]\n  workflow_dispatch:\n  schedule:\n    - cron: \"0 3 * * *\"\n",
		"a dispatch input":                      "\non:\n  push:\n    tags: [\"v*\"]\n  workflow_dispatch:\n    inputs:\n      publish:\n        type: boolean\n",
		"a push filter nobody classified":       "\non:\n  push:\n    tags: [\"v*\"]\n    some-future-filter: x\n  workflow_dispatch:\n",
	}
	for name, on := range refused {
		g := &gate{out: io.Discard}
		g.checkTriggerSurface(shaped(on))
		if !g.failed {
			t.Fatalf("%s reaches the publishing path through a shape nothing planned, and was accepted", name)
		}
	}

	// An absent `on:` is not "it triggers on nothing".
	g = &gate{out: io.Discard}
	g.checkTriggerSurface(&Workflow{})
	if !g.failed {
		t.Fatal("a definition with no `on:` block must red rather than be graded against invented shapes")
	}
}

// --- what a role step is HANDED ----------------------------------------------------------

// The table property, asserted where the hole lived. Declaring an environment name on a role
// and writing down what the value is FOR held it to nothing: `REF: …:latest` on the re-smoke
// left the role held and the invocation untouched while the step pulled back the PREVIOUS
// release (S0046 F22). So every name a role declares must resolve to a value produced OUTSIDE
// the step, and a name that resolves to nothing reads CLOSED - the zero value of envHeld is
// exactly that case, and it must not be reachable from the table.
func TestHandsEnv_EveryDeclaredNameResolvesToAValueProducedOutsideTheStep(t *testing.T) {
	h := sampleHanded()
	for _, r := range releaseRoles {
		for name, spec := range r.handsEnv {
			want, from, ok := h.expected(spec.holds)
			if !ok {
				t.Fatalf("role %q declares `%s` and holds it against NOTHING. A name whose value nobody compares is the hole this check exists to refuse", r.id, name)
			}
			if want == "" || from == "" {
				t.Fatalf("role %q declares `%s`, which resolves to an EMPTY expectation (%q from %q): a comparison against nothing passes on anything", r.id, name, want, from)
			}
			if spec.what == "" {
				t.Fatalf("role %q declares `%s` with no description, so its refusal cannot say what the value is for", r.id, name)
			}
		}
	}
	if _, _, ok := h.expected(heldNothing); ok {
		t.Fatal("the zero value of envHeld must read CLOSED, or a forgotten `holds:` silently passes")
	}
	if _, _, ok := h.expected(envHeld(9999)); ok {
		t.Fatal("an envHeld nobody wired up must read CLOSED")
	}
}

// The comparison itself, over the two values ordinal 8 found unheld. `REF` decides which
// artefact the re-smoke grades and `VERSION` decides whether A11 compares anything at all;
// a wrong value in either leaves every other assertion in this gate green.
func TestHandedValues_AValueIsComparedWholeAgainstWhatTheRunProduced(t *testing.T) {
	h := sampleHanded()
	cases := []struct {
		held envHeld
		want string
		bad  []string
	}{
		{heldGatedRef, "ghcr.io/o/r:v0.1.0", []string{"ghcr.io/o/r:latest", "ghcr.io/o/r:v0.0.1", "ghcr.io/o/r", ""}},
		{heldPlannedVersion, "v0.1.0", []string{"latest", "v0.0.1", ""}},
		{heldPlannedImage, "ghcr.io/o/r", []string{"ghcr.io/someone-else/r", ""}},
		{heldFloatingTag, "latest", []string{"stable", "v0.1.0", ""}},
	}
	for _, c := range cases {
		got, _, ok := h.expected(c.held)
		if !ok || got != c.want {
			t.Fatalf("expected(%d) = %q, %v; want %q", c.held, got, ok, c.want)
		}
		for _, b := range c.bad {
			if b == got {
				t.Fatalf("expected(%d) accepts %q, which is not the value the run produced", c.held, b)
			}
		}
	}
}

// refTag is what holds the floating reference to the one a user actually pulls, so it has to
// be right about a registry port: `localhost:5000/x` carries a colon and no tag.
func TestRefTag_TheTagIsTheLastColonAndNotAPort(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/nschatz/holdfast:latest": "latest",
		"localhost:5000/x:v1":             "v1",
	} {
		got, ok := refTag(ref)
		if !ok || got != want {
			t.Fatalf("refTag(%q) = %q, %v; want %q", ref, got, ok, want)
		}
	}
	for _, ref := range []string{"ghcr.io/nschatz/holdfast", "localhost:5000/x", "ghcr.io/x:"} {
		if got, ok := refTag(ref); ok {
			t.Fatalf("refTag(%q) = %q, true; a reference with no tag must not resolve to one", ref, got)
		}
	}
}

func sampleHanded() handed {
	return handed{
		image: "ghcr.io/o/r", version: "v0.1.0", gated: "ghcr.io/o/r:v0.1.0",
		floating: "latest", event: "push", refName: "v0.1.0", repo: "O/R",
	}
}

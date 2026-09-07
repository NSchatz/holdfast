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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
func TestRoleMatch_ARoleCannotBeClaimedByMentioningIt(t *testing.T) {
	gate := role{id: "full-gate", what: "the full gate", program: "make", mustField: []string{"check"}}
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

func TestRoleMatch_TheSmokeRunsAreToldApartByWhatTheyPass(t *testing.T) {
	amd := role{id: "smoke-amd64", program: "./scripts/smoke-image.sh", mustNotField: []string{"--no-encode", "linux/arm64"}}
	arm := role{id: "smoke-arm64", program: "./scripts/smoke-image.sh", mustField: []string{"linux/arm64"}}

	if err := amd.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release"}); err != nil {
		t.Fatalf("the amd64 smoke run was refused: %v", err)
	}
	if err := amd.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release --no-encode"}); err == nil {
		t.Fatal("the amd64 role must drive a REAL encode; --no-encode makes it an exec check")
	}
	if err := arm.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release-arm64 linux/arm64 --no-encode"}); err != nil {
		t.Fatalf("the arm64 smoke run was refused: %v", err)
	}
	if err := arm.matches(Step{Run: "./scripts/smoke-image.sh holdfast:release"}); err == nil {
		t.Fatal("a run that names no architecture cannot be the arm64 smoke run")
	}
}

func TestRoleMatch_AnActionRoleIsTheActionItNames(t *testing.T) {
	push := role{id: "push-version", action: "docker/build-push-action"}
	if err := push.matches(Step{Uses: "docker/build-push-action@v6"}); err != nil {
		t.Fatalf("the real push step was refused: %v", err)
	}
	for _, uses := range []string{"", "actions/checkout@v4", "evil/docker-build-push-action@v6"} {
		if err := push.matches(Step{Uses: uses}); err == nil {
			t.Fatalf("%q is not docker/build-push-action and must not hold that role", uses)
		}
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

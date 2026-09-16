package webui

// regress_0126_F1 - the evidence for S0126's impl-gate finding F1 (ordinal 1).
//
// AC-17: "WHEN the mutation route runs THE SYSTEM SHALL defeat on purpose EVERY refusal
// this spec's criteria name, ONE MUTATION PER REFUSAL against a mutated copy of the tree,
// require each defeat to be red and to name what it saw, and fail the run if any defeat did
// not execute. A refusal graded only by a run expected green is a refusal nobody knows
// bites (testing T5)."
//
// Two refusals the criteria name carry no mutation on the route as it stands.
//
//	AC-13's FIRST limb - "exiting non-zero on a state that did not render". Every C7 cell on
//	scripts/webui-graders-selftest.sh reaches the grader's "renders VISUALLY IDENTICAL to its
//	default" branch; nothing reaches its "could not enter that state at the engine" branch,
//	which is the other half of the same criterion.
//
//	AC-18's refusal - "refuse a run in which either [new grader] did not execute". That
//	refusal is NEW code: the two greps for "c4: measured" and "c7: derived" this change adds
//	to scripts/webui-check.sh. No case on the mutation route runs that script at all, so the
//	refusal is graded only by a run expected green.
//
// TheRefusalBites performs the missing AC-13 defeat by hand against a copy of the tree, so
// the gap is shown to be COVERAGE and not capability - the grader reds, and naming the
// defeat costs one sed. TheRouteCarriesIt then asks the repository's own mutation route for
// the two defeats, and that is the half that fails today.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// The grader's own words for AC-13's first limb, which a defeat has to assert.
	notEnteredMessage = "could not enter that state at the engine"
	// The C7 case title, as the runner's -g filter matches it.
	matrixCase = "every interactive component proves its whole state matrix"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	return root
}

// TestRegress0126F1_TheRefusalBites drives the defeat the route is missing, so the finding
// is about coverage rather than about a branch that could never fire.
func TestRegress0126F1_TheRefusalBites(t *testing.T) {
	node := nodeRuntime(t)
	browser := chromium(t)
	goBin := goRuntime(t)
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "internal", "webui", "e2e", "node_modules", "@playwright", "test")); err != nil {
		t.Skipf("the runner is not installed (run `npm ci` in internal/webui/e2e): %v", err)
	}

	// A copy, mutated - never the tree this is graded from. Same shape as the copy
	// scripts/webui-graders-selftest.sh makes, for the same reason.
	dst := t.TempDir()
	copyTree := exec.Command("bash", "-c",
		`tar -C "$1" --exclude=./.git --exclude=./internal/webui/e2e/test-results `+
			`--exclude=./internal/webui/e2e/playwright-report -cf - . | tar -C "$2" -xf -`,
		"bash", root, dst)
	if out, err := copyTree.CombinedOutput(); err != nil {
		t.Fatalf("copying the tree failed: %v\n%s", err, out)
	}

	// The mutation: a state this surface has no way to enter at the engine, DECLARED proved.
	// It is the one shape of AC-13 failure the mutation route never produces.
	record := filepath.Join(dst, "docs", "state-matrix.md")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("reading the copy's record: %v", err)
	}
	sed := exec.Command("sed", "-i",
		`s#^| button | loading | not applicable |.*#| button | loading | proved |  |#`, record)
	if out, err := sed.CombinedOutput(); err != nil {
		t.Fatalf("mutating the copy's record failed: %v\n%s", err, out)
	}
	after, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("re-reading the copy's record: %v", err)
	}
	if string(before) == string(after) {
		t.Fatal("the mutation changed nothing in the record, so this case did NOT run")
	}

	port, err := exec.Command(node, "-e",
		`const s=require("node:net").createServer();s.listen(0,"127.0.0.1",()=>{process.stdout.write(String(s.address().port));s.close();});`).Output()
	if err != nil {
		t.Fatalf("asking the operating system for a free port: %v", err)
	}

	run := exec.Command(node, filepath.Join("node_modules", "@playwright", "test", "cli.js"),
		"test", "--workers=1", "--reporter=list", "-g", matrixCase)
	run.Dir = filepath.Join(dst, "internal", "webui", "e2e")
	run.Env = append(os.Environ(),
		"CI=1", "NO_COLOR=1",
		"HOLDFAST_BROWSER="+browser,
		"HOLDFAST_E2E_PORT="+strings.TrimSpace(string(port)),
		"PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, runErr := run.CombinedOutput()
	if runErr == nil {
		t.Fatalf("the C7 grader exited 0 on a record declaring an unenterable state proved:\n%s", out)
	}
	if !strings.Contains(string(out), notEnteredMessage) {
		t.Fatalf("the C7 grader redded for another reason; nothing said %q:\n%s", notEnteredMessage, out)
	}
	t.Logf("the refusal bites: the grader reds and names what it saw (%q)", notEnteredMessage)
}

// TestRegress0126F1_TheRouteCarriesIt is AC-17 itself: one defeat per refusal the criteria
// name. It reads the two scripts and nothing else, because "the mutation route carries this
// defeat" is a property of the route.
func TestRegress0126F1_TheRouteCarriesIt(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		return string(b)
	}
	route := read(filepath.Join("scripts", "webui-graders-selftest.sh"))
	gate := read(filepath.Join("scripts", "webui-check.sh"))

	// The control: AC-18's refusal really is in the gate, so the gap below is coverage.
	for _, mark := range []string{"c4: measured", "c7: derived"} {
		if !strings.Contains(gate, mark) {
			t.Errorf("scripts/webui-check.sh carries no refusal keyed on %q, so AC-18's "+
				"'either did not execute' half is not implemented at all", mark)
		}
	}

	if !strings.Contains(route, notEnteredMessage) {
		t.Errorf("AC-17: AC-13 names TWO refusals - 'a state that did not render' and one "+
			"'that rendered visually identical to default' - and only the second is defeated. "+
			"No case in scripts/webui-graders-selftest.sh reaches the grader's %q branch, "+
			"which TestRegress0126F1_TheRefusalBites shows is one sed away.", notEnteredMessage)
	}

	if !strings.Contains(route, "webui-check.sh") && !strings.Contains(route, "make webui-check") {
		t.Errorf("AC-17: AC-18 requires the required-mode run to 'refuse a run in which either " +
			"did not execute'. This change ADDS that refusal to scripts/webui-check.sh, and " +
			"scripts/webui-graders-selftest.sh never runs that script, so the new refusal is " +
			"graded only by a run expected green - which is the shape AC-17's own sentence forbids.")
	}
}

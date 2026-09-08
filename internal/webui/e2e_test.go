package webui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The dashboard's PLAYWRIGHT graders.
//
// They decide the same class of question the hand-driven graders in this package do -
// what a real engine RENDERED, after the whole cascade, with real layout and a real hit
// test - and they decide it against the same served document: `internal/webui/e2e`'s
// fixture server mounts `webui.HandlerFor`, so there is still exactly ONE reader of that
// document and one Content-Security-Policy in this repository.
//
// What the runner buys, and why it is worth a dependency the rest of this repository does
// not take: the theme, the viewport and the reading are the runner's own vocabulary
// rather than several hundred lines of protocol driving, so a new criterion costs a few
// lines instead of a probe script; a failure arrives as the assertion that failed with a
// trace beside it rather than as a JSON verdict a Go test has to unpick; and the suite
// runs its projects in parallel.
//
// WHERE THE DEPENDENCY IS ALLOWED TO REACH, which is the load-bearing half:
//
//	the BUILD PATH and the SHIPPED PAGE take none of it. `internal/webui/gen` is still Go
//	and the standard library alone, `make build` is still a plain `go build`, the image
//	still gains no stage and no tool, and the served document still resolves nothing at
//	load time. Playwright is TEST tooling and lives under internal/webui/e2e; nothing it
//	brings can reach an artifact a user runs. That boundary is enforced rather than asked
//	for on trust, by TestBuild_NoThirdPartyJavaScriptEntersThePageOrTheTooling (where a
//	dependency may live) and TestBuild_TheTestOnlyDependencyCannotReachTheBuiltArtifact
//	(that nothing it installs reaches the generator, the page, the binary or the image).
//	Both names are written out whole: an abbreviated one names no test, and a citation
//	that resolves to nothing is how a comment outlives the assertion it points at.
//
// This wrapper is what puts the suite inside `go test`, so `make check` runs it (skipping
// where a runtime is absent, as the docker gate does) and `make webui-check` requires it.

// playwrightTimeout is this test's own deadline for the child runner, for the same reason
// every other suite here owns one: a wedged run must fail with its output rather than
// hang until the CI runner kills the job.
const playwrightTimeout = 10 * time.Minute

func TestPlaywright_TheRenderedGradersRunInARealEngine(t *testing.T) {
	// Every runtime this suite needs, located the way the rest of the package locates
	// one: named, and a skip that becomes a failure under required mode.
	node := nodeRuntime(t)
	browser := chromium(t)
	goBin := goRuntime(t)

	dir := filepath.Join("e2e")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "@playwright", "test")); err != nil {
		missingRuntime(t, "@playwright/test",
			"the dashboard's Playwright graders need their project installed: run `npm ci` (or `npm install`) in internal/webui/e2e")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), playwrightTimeout)
	defer cancel()

	// A JSON reporter, so the count of executed cases is the RUNNER's own and not one
	// parsed out of prose. A suite that ran nothing must not be able to report success.
	report := filepath.Join(t.TempDir(), "results.json")
	cmd := exec.CommandContext(ctx, node,
		filepath.Join("node_modules", "@playwright", "test", "cli.js"), "test",
		"--reporter=list,json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOLDFAST_BROWSER="+browser,
		"PLAYWRIGHT_JSON_OUTPUT_NAME="+report,
		// The fixture server is `go run`, so the child needs the toolchain this test
		// found rather than whatever a bare PATH happens to hold.
		"PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NO_COLOR=1",
	)
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the Playwright graders did not finish within %s; their output so far:\n%s", playwrightTimeout, out)
	}
	if runErr != nil {
		t.Fatalf("the dashboard's Playwright graders failed (%v):\n%s", runErr, out)
	}

	passed, failed, skipped := playwrightCounts(t, report)
	if failed != 0 {
		t.Fatalf("the Playwright graders reported %d failures:\n%s", failed, out)
	}
	// A skip here is a criterion that did not get decided. This suite has no runtime of
	// its own to be missing - the wrapper already proved every one is present - so a skip
	// can only be a `test.skip` somebody left behind.
	if skipped != 0 && requiredMode() {
		t.Fatalf("the Playwright graders reported %d SKIPPED cases under required mode; "+
			"a criterion nobody decided is not a criterion:\n%s", skipped, out)
	}
	// The anti-vacuity floor. A suite filtered down to nothing, or one whose specs failed
	// to load, exits 0 and measures nothing; the committed project is larger than this.
	// The committed project is far larger than this; the floor exists so a suite filtered
	// down to nothing, or one whose specs failed to load, cannot exit 0 and report success.
	if passed < 60 {
		t.Fatalf("the Playwright graders reported only %d passing cases; the committed project is larger than that, "+
			"so something did not run:\n%s", passed, out)
	}
	t.Logf("playwright ran %d rendered cases across every theme and width project", passed)
}

// goRuntime locates the Go toolchain the fixture server is started with. The suite's
// server is the REAL handler compiled from this tree, which is the whole reason these
// graders read the same document `holdfast serve` produces - so a run without a toolchain
// has nothing to grade, and says so by name.
func goRuntime(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	missingRuntime(t, "go", "the Playwright graders serve the real webui handler, which is compiled from this tree")
	return ""
}

// playwrightCounts reads the runner's own JSON report. Parsing the report rather than the
// list output means the numbers are the runner's, and a change in its prose cannot make a
// suite that ran nothing look like one that passed.
func playwrightCounts(t *testing.T, report string) (passed, failed, skipped int) {
	t.Helper()
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the Playwright graders produced no JSON report at %s: %v", report, err)
	}
	var doc struct {
		Suites []playwrightSuite `json:"suites"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the Playwright report is not valid JSON: %v", err)
	}
	for _, s := range doc.Suites {
		p, f, sk := countSuite(s)
		passed, failed, skipped = passed+p, failed+f, skipped+sk
	}
	return passed, failed, skipped
}

type playwrightSuite struct {
	Suites []playwrightSuite `json:"suites"`
	Specs  []struct {
		Tests []struct {
			Status string `json:"status"`
		} `json:"tests"`
	} `json:"specs"`
}

func countSuite(s playwrightSuite) (passed, failed, skipped int) {
	for _, spec := range s.Specs {
		for _, tc := range spec.Tests {
			switch strings.ToLower(tc.Status) {
			case "expected":
				passed++
			case "skipped":
				skipped++
			default:
				failed++
			}
		}
	}
	for _, sub := range s.Suites {
		p, f, sk := countSuite(sub)
		passed, failed, skipped = passed+p, failed+f, skipped+sk
	}
	return passed, failed, skipped
}

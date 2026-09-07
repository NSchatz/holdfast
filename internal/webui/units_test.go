package webui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The dashboard's DERIVATION UNITS (WEBUI-10, B5-B7).
//
// Runtime: node's BUILT-IN test runner. That choice is the whole of B18 in one line - the
// runner, the assertion library and the module loader are all part of the runtime, so the
// unit suite introduces no registry package, no lockfile and nothing third-party into the
// test tooling, exactly as the served page introduces none into itself.
//
// Why node and not the browser: node has no DOM, and that is a feature here. The
// derivations under test map wire data to the value a cell shows and touch no DOM at all,
// so they are exercisable one input at a time with nothing standing up. Everything that
// BUILDS nodes needs a DOM, and the only DOM available without a dependency is the
// browser - which is where the rendered graders in dashboard_rendered_test.go decide it.
//
// This wrapper is what puts that suite inside `go test`, so `make check` runs it (skipping
// where node is absent, as the docker gate does) and `make webui-check` requires it.

// nodeTestTimeout is this test's own deadline for the child runner, for the same reason
// the rendered graders own theirs: a wedged child must fail here, with its output, rather
// than hang until the CI runner kills the job.
const nodeTestTimeout = 3 * time.Minute

var tapCount = regexp.MustCompile(`(?m)^#\s+(pass|fail)\s+(\d+)\s*$`)

func TestUnit_DerivationsRunInNodesOwnTestRunner(t *testing.T) {
	node := nodeRuntime(t)

	files, err := filepath.Glob(filepath.Join("src", "test", "*.test.js"))
	if err != nil {
		t.Fatalf("globbing the unit suite: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no unit suite found under internal/webui/src/test: a suite that does not exist cannot fail")
	}

	ctx, cancel := context.WithTimeout(context.Background(), nodeTestTimeout)
	defer cancel()
	args := append([]string{"--test", "--test-reporter=tap"}, files...)
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=", "NO_COLOR=1")
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the derivation units did not finish within %s; the runner's output so far:\n%s", nodeTestTimeout, out)
	}

	// The runner's own TAP summary, so a suite that ran nothing cannot report success.
	counts := map[string]int{}
	for _, m := range tapCount.FindAllStringSubmatch(string(out), -1) {
		n, _ := strconv.Atoi(m[2])
		counts[m[1]] = n
	}
	if runErr != nil {
		t.Fatalf("the dashboard's derivation units failed (%v):\n%s", runErr, out)
	}
	if counts["fail"] != 0 {
		t.Fatalf("the derivation units reported %d failures:\n%s", counts["fail"], out)
	}
	if counts["pass"] < 20 {
		t.Fatalf("the derivation units reported only %d passing cases; the committed suite is larger than that, "+
			"so something did not run:\n%s", counts["pass"], out)
	}
	t.Logf("node %s ran %d derivation cases from %s", nodeVersion(t, node), counts["pass"], strings.Join(files, " "))
}

func nodeVersion(t *testing.T, node string) string {
	t.Helper()
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		return "(unknown version)"
	}
	return strings.TrimSpace(string(out))
}

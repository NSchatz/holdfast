package main

import (
	"os"
	"testing"
)

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func ctx(publish, prerelease string, success bool) evalCtx {
	return evalCtx{
		success: success,
		vars: map[string]any{
			"github": map[string]any{"event_name": "push", "ref_name": "v0.1.0"},
			"env":    map[string]any{"GO_VERSION": "1.25.14"},
			"steps": map[string]any{
				"plan": map[string]any{"outputs": map[string]any{
					"publish": publish, "prerelease": prerelease,
				}},
			},
		},
	}
}

// The guards this gate has to decide correctly, taken verbatim from release.yml.
func TestConditionRuns_TheGuardsInReleaseYml(t *testing.T) {
	const (
		publishGuard = "steps.plan.outputs.publish == 'true'"
		promoteGuard = "steps.plan.outputs.publish == 'true' && steps.plan.outputs.prerelease == 'false'"
	)
	cases := []struct {
		name    string
		cond    string
		publish string
		pre     string
		success bool
		want    bool
	}{
		{"a dispatch does not publish", publishGuard, "false", "true", true, false},
		{"a tag push publishes", publishGuard, "true", "false", true, true},
		{"a pre-release does not promote", promoteGuard, "true", "true", true, false},
		{"a release promotes", promoteGuard, "true", "false", true, true},
		// The implicit `success() &&` is the whole reason a failed gate leaves the
		// floating reference alone. Without it, both of these would still run.
		{"a failed run does not publish", publishGuard, "true", "false", false, false},
		{"a failed run does not promote", promoteGuard, "true", "false", false, false},
		// ... and always() opts out of it, which is why the gate must catch always().
		{"always() survives a failure", "always() && " + publishGuard, "true", "false", false, true},
		{"an empty condition runs on success", "", "true", "false", true, true},
		{"an empty condition does not run after a failure", "", "true", "false", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ConditionRuns(c.cond, ctx(c.publish, c.pre, c.success))
			if err != nil {
				t.Fatalf("ConditionRuns(%q) errored: %v", c.cond, err)
			}
			if got != c.want {
				t.Fatalf("ConditionRuns(%q) with publish=%s prerelease=%s success=%v = %v, want %v",
					c.cond, c.publish, c.pre, c.success, got, c.want)
			}
		})
	}
}

// Fail-closed: anything this evaluator cannot decide must be an ERROR, never a false that
// reads as "that publishing step does not run".
func TestConditionRuns_RefusesWhatItCannotDecide(t *testing.T) {
	for _, cond := range []string{
		"steps.plan.outputs.publsih == 'true'",       // a typo GitHub would treat as null
		"steps.build.outputs.publish == 'true'",      // an output no step produced
		"hashFiles('go.sum') != ''",                  // a function this gate does not model
		"github.event.repository.private == 'false'", // a context it does not carry
		"steps.plan.outputs.publish ==",              // not parseable
	} {
		if _, err := ConditionRuns(cond, ctx("false", "true", true)); err == nil {
			t.Fatalf("ConditionRuns(%q) returned no error; an undecidable guard must be red, not false", cond)
		}
	}
}

func TestConditionRuns_HandlesTheWrappedForm(t *testing.T) {
	got, err := ConditionRuns("${{ steps.plan.outputs.publish == 'true' }}", ctx("true", "false", true))
	if err != nil || !got {
		t.Fatalf("wrapped condition = %v, %v; want true, nil", got, err)
	}
}

func TestInterpolate(t *testing.T) {
	c := ctx("true", "false", true)
	got, err := Interpolate("go-version: ${{ env.GO_VERSION }} publish=${{ steps.plan.outputs.publish }}", c)
	if err != nil {
		t.Fatalf("Interpolate errored: %v", err)
	}
	if want := "go-version: 1.25.14 publish=true"; got != want {
		t.Fatalf("Interpolate = %q, want %q", got, want)
	}
	if _, err := Interpolate("${{ steps.plan.outputs.nope }}", c); err == nil {
		t.Fatal("interpolating an output nothing produced must be an error")
	}
}

func TestEvaluate_Operators(t *testing.T) {
	c := ctx("true", "false", true)
	cases := []struct {
		expr string
		want any
	}{
		{"'a' == 'A'", true}, // GitHub ignores case when comparing strings
		{"'a' != 'b'", true},
		{"!('a' == 'b')", true},
		{"1 < 2", true},
		{"startsWith('v0.1.0', 'v0.')", true},
		{"endsWith('v0.1.0-rc1', '-rc1')", true},
		{"contains('ghcr.io/x/y:latest', ':latest')", true},
		{"true && false", false},
		{"false || true", true},
	}
	for _, tc := range cases {
		got, err := Evaluate(tc.expr, c)
		if err != nil {
			t.Fatalf("Evaluate(%q) errored: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("Evaluate(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// The references a push publishes are read out of a structured `tags:` input, in either of
// the two shapes GitHub accepts for a multi-line scalar.
func TestSplitRefs(t *testing.T) {
	got := splitRefs("ghcr.io/x/y:v0.1.0\nghcr.io/x/y:latest\n")
	if len(got) != 2 || got[0] != "ghcr.io/x/y:v0.1.0" || got[1] != "ghcr.io/x/y:latest" {
		t.Fatalf("newline-separated tags = %v", got)
	}
	got = splitRefs("ghcr.io/x/y:v0.1.0, ghcr.io/x/y:latest")
	if len(got) != 2 || got[1] != "ghcr.io/x/y:latest" {
		t.Fatalf("comma-separated tags = %v", got)
	}
	if got := splitRefs("  \n , \n"); len(got) != 0 {
		t.Fatalf("a blank input names no reference, and must not be read as one: %v", got)
	}
}

func TestRepoFromModule(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := dir + "/go.mod"
		if err := writeFile(p, body); err != nil {
			t.Fatal(err)
		}
		return p
	}
	got, err := repoFromModule(write("module github.com/NSchatz/holdfast\n\ngo 1.25.0\n"))
	if err != nil || got != "NSchatz/holdfast" {
		t.Fatalf("repoFromModule = %q, %v; want NSchatz/holdfast", got, err)
	}
	if _, err := repoFromModule(write("module example.com/holdfast\n")); err == nil {
		t.Fatal("a non-github module path must be an error, not a guessed repository")
	}
}

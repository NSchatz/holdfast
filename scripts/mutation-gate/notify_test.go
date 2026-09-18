package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tracking-issue route (AC-5) is driven here against a STUBBED issues API, which is
// the only way to decide what it would send: the real one is outside this repository, and
// a self-test that merely watched the step being invoked would pass whether the second
// failure updated the open issue or opened a second one.
//
// The stub stands in for GitHub, never for the notifier: the code under test is the same
// cmdNotify the workflow step runs.

// testConfig writes a runner configuration carrying a floor that is deliberately NOT the
// repository's own, so a floor that reached the issue body by being hardcoded would be
// visible.
func testConfig(t *testing.T, floor string) string {
	t.Helper()
	root := t.TempDir()
	cfg := "unleash:\n  threshold:\n    efficacy: " + floor + "\n  exclude-files:\n    - '^internal/engine/'\n"
	if err := os.WriteFile(filepath.Join(root, ".gremlins.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return root
}

func testReport(t *testing.T, root string, score float64) string {
	t.Helper()
	path := filepath.Join(root, "mutation-report.json")
	body := fmt.Sprintf(`{"mode":"full","in_scope":true,"floor":55,"score":%v,"verdict":"below-floor","mutants":{"total":80,"killed":40,"lived":40}}`, score)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

type captured struct {
	method string
	path   string
	body   map[string]any
}

// stubAPI records every request and answers the issue list with whatever the caller set.
func stubAPI(t *testing.T, openIssues []map[string]any, got *[]captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &payload)
		}
		*got = append(*got, captured{method: r.Method, path: r.URL.Path, body: payload})
		if r.Header.Get("Authorization") == "" {
			t.Errorf("request carried no credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(openIssues)
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AC-5: a red scheduled run opens ONE tracking issue, assigned to the repository owner,
// carrying the measured score, the floor and the URL of the failing run.
func TestNotify_AC5_OpensOneIssueWithScoreFloorAndRunURL(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "a-token")
	root := testConfig(t, "55")
	report := testReport(t, root, 50)
	var got []captured
	srv := stubAPI(t, nil, &got)

	var out, errOut strings.Builder
	code := cmdNotify([]string{
		"--root", root, "--repo", "NSchatz/holdfast", "--assignee", "NSchatz",
		"--run-url", "https://github.com/NSchatz/holdfast/actions/runs/424242",
		"--report", report, "--api", srv.URL,
	}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("notify exited %d, want 0: %s", code, errOut.String())
	}

	var posts []captured
	for _, c := range got {
		if c.method == http.MethodPost {
			posts = append(posts, c)
		}
		if c.method == http.MethodPatch {
			t.Errorf("updated an issue when none existed: %s", c.path)
		}
	}
	if len(posts) != 1 {
		t.Fatalf("opened %d issues, want exactly 1", len(posts))
	}
	body, _ := posts[0].body["body"].(string)
	for _, want := range []string{
		"50.00%", // the measured score
		"55.00%", // the floor, read from the committed configuration
		"https://github.com/NSchatz/holdfast/actions/runs/424242", // the failing run
		issueMarker, // the stable marker a second failure finds
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the issue body does not carry %q:\n%s", want, body)
		}
	}
	assignees, _ := posts[0].body["assignees"].([]any)
	if len(assignees) != 1 || assignees[0] != "NSchatz" {
		t.Errorf("assignees = %v, want [NSchatz] - a tracking issue nobody owns is one nobody reads", assignees)
	}
}

// AC-5: a SECOND consecutive failure updates the open issue rather than opening another.
// The marker in the body is what makes that decision, so an issue whose title somebody
// edited is still found, and a pull request in the same listing is not mistaken for it.
func TestNotify_AC5_SecondFailureUpdatesRatherThanOpensAnother(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "a-token")
	root := testConfig(t, "55")
	report := testReport(t, root, 41)
	open := []map[string]any{
		{"number": 11, "body": "an unrelated issue", "state": "open"},
		{"number": 12, "body": "a pull request carrying the marker " + issueMarker, "state": "open", "pull_request": map[string]any{"url": "x"}},
		{"number": 13, "body": "somebody renamed this\n" + issueMarker + "\nolder text", "state": "open"},
	}
	var got []captured
	srv := stubAPI(t, open, &got)

	var out, errOut strings.Builder
	code := cmdNotify([]string{
		"--root", root, "--repo", "NSchatz/holdfast", "--assignee", "NSchatz",
		"--run-url", "https://github.com/NSchatz/holdfast/actions/runs/424243",
		"--report", report, "--api", srv.URL,
	}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("notify exited %d, want 0: %s", code, errOut.String())
	}

	var patched []captured
	for _, c := range got {
		if c.method == http.MethodPost {
			t.Errorf("opened a SECOND issue while one was already open: %v", c.body)
		}
		if c.method == http.MethodPatch {
			patched = append(patched, c)
		}
	}
	if len(patched) != 1 {
		t.Fatalf("updated %d issues, want exactly 1", len(patched))
	}
	if !strings.HasSuffix(patched[0].path, "/issues/13") {
		t.Errorf("updated %s, want the issue carrying the marker (#13)", patched[0].path)
	}
	if body, _ := patched[0].body["body"].(string); !strings.Contains(body, "41.00%") {
		t.Errorf("the update does not carry this run's score:\n%s", body)
	}
}

// AC-5: when the run failed BEFORE it could measure anything, the issue says what stopped
// it. It never carries a number in place of the score it does not have.
func TestNotify_AC5_NoScoreNamesTheFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "a-token")
	root := testConfig(t, "55")
	var got []captured
	srv := stubAPI(t, nil, &got)

	var out, errOut strings.Builder
	code := cmdNotify([]string{
		"--root", root, "--repo", "NSchatz/holdfast", "--assignee", "NSchatz",
		"--run-url", "https://github.com/NSchatz/holdfast/actions/runs/424244",
		"--report", filepath.Join(root, "does-not-exist.json"),
		"--failure", "the pinned runner could not be obtained",
		"--api", srv.URL,
	}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("notify exited %d, want 0: %s", code, errOut.String())
	}
	if len(got) < 2 || got[len(got)-1].method != http.MethodPost {
		t.Fatalf("no issue was opened: %v", got)
	}
	body, _ := got[len(got)-1].body["body"].(string)
	if !strings.Contains(body, "the pinned runner could not be obtained") {
		t.Errorf("the issue does not say what prevented a score:\n%s", body)
	}
	if !strings.Contains(body, "score: NONE") {
		t.Errorf("the issue should report NO score rather than a number:\n%s", body)
	}
	if !strings.Contains(body, "55.00%") {
		t.Errorf("the issue does not carry the floor:\n%s", body)
	}
}

// AC-5: with no credential there is no notification, and the step SAYS so rather than
// exiting quietly. A run that failed and told nobody is the case this route exists for.
func TestNotify_AC5_NoCredentialIsStatedNotSwallowed(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	root := testConfig(t, "55")
	var out, errOut strings.Builder
	code := cmdNotify([]string{
		"--root", root, "--repo", "NSchatz/holdfast", "--assignee", "NSchatz",
		"--run-url", "https://example.invalid/run", "--api", "https://example.invalid",
	}, &out, &errOut)
	if code != exitNotify {
		t.Fatalf("notify exited %d, want %d", code, exitNotify)
	}
	msg := errOut.String()
	for _, want := range []string{"GITHUB_TOKEN", "issues: write", "STOPPING"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure does not name %q:\n%s", want, msg)
		}
	}
}

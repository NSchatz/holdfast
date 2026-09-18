// The notification route for a red SCHEDULED run.
//
// A pull-request run reds the pull request and a human is already looking at it. The
// unscoped run happens on a schedule, against the default branch, with nobody watching -
// and a red job in a list nobody opens is the same as no gate at all. So a failed
// scheduled run stays red AND says so where the work is tracked: one issue on this
// repository, assigned to its owner, carrying the measured score (or the failure that
// prevented one), the floor, and the URL of the run that failed.
//
// Exactly ONE issue. The marker below is written into the body, and the next consecutive
// failure finds it and UPDATES that issue rather than opening a second: a weekly job that
// opens a new issue every Saturday teaches its reader to close them unread.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/scripts/mutation"
)

// issueMarker is the stable identity of the tracking issue. It is an HTML comment, so it
// does not render, and it is matched on the BODY rather than the title: a title is
// something a human edits, and an edited title must not cause a second issue to open.
const issueMarker = "<!-- holdfast-mutation-gate -->"

const issueTitle = "mutation gate: the scheduled unscoped run is red"

// defaultAPI is GitHub's REST root. The self-test points this at a stub so the request
// this step WOULD send can be asserted without reaching GitHub.
const defaultAPI = "https://api.github.com"

const httpTimeout = 30 * time.Second

func cmdNotify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the module root holding "+mutation.ConfigName)
	repo := fs.String("repo", "", "owner/name of the repository the issue belongs to")
	assignee := fs.String("assignee", "", "the login the issue is assigned to (the repository owner)")
	runURL := fs.String("run-url", "", "the URL of the failing run")
	reportPath := fs.String("report", "", "the machine-readable report, if the run got far enough to write one")
	failure := fs.String("failure", "", "what prevented a score, when there is no report")
	api := fs.String("api", defaultAPI, "the issues API root")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case *repo == "" || !strings.Contains(*repo, "/"):
		fmt.Fprintf(stderr, "mutation-gate notify: --repo must be owner/name\n")
		return exitUsage
	case *assignee == "":
		fmt.Fprintf(stderr, "mutation-gate notify: --assignee is required; a tracking issue nobody owns is a tracking issue nobody reads\n")
		return exitUsage
	case *runURL == "":
		fmt.Fprintf(stderr, "mutation-gate notify: --run-url is required; the issue has to say which run failed\n")
		return exitUsage
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		// observability O4: which dependency, what was tried, what happens next.
		fmt.Fprintf(stderr, "::error::mutation gate: the tracking issue CANNOT BE FILED.\n")
		fmt.Fprintf(stderr, "       dependency: the repository's issues API (%s)\n", *api)
		fmt.Fprintf(stderr, "       tried: reading a credential from GITHUB_TOKEN, which is empty or unset\n")
		fmt.Fprintf(stderr, "       next: STOPPING, and this step is failing. The run stays red either way; what is lost is the notification, so this must not be swallowed. The scheduled job grants itself issues: write, because this repository's default workflow permission is read.\n")
		return exitNotify
	}

	cfg, err := mutation.ReadConfig(*root)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: %v\n", err)
		return exitConfig
	}

	body := issueBody(cfg.Floor(), *reportPath, *failure, *runURL)

	c := &client{api: strings.TrimRight(*api, "/"), repo: *repo, token: token, http: &http.Client{Timeout: httpTimeout}}
	existing, err := c.findTracking()
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not read this repository's open issues to see whether the tracking issue already exists: %v\n", err)
		fmt.Fprintf(stderr, "       next: STOPPING without filing anything. Opening one blind would risk a second copy, and this issue is meant to be the only one.\n")
		return exitNotify
	}
	if existing > 0 {
		if err := c.updateIssue(existing, body, *assignee); err != nil {
			fmt.Fprintf(stderr, "::error::mutation gate: could not update the tracking issue #%d: %v\n", existing, err)
			return exitNotify
		}
		fmt.Fprintf(stdout, "mutation gate: updated the tracking issue #%d (the scheduled run is red again; no second issue opened)\n", existing)
		return exitOK
	}
	number, err := c.createIssue(issueTitle, body, *assignee)
	if err != nil {
		fmt.Fprintf(stderr, "::error::mutation gate: could not open the tracking issue: %v\n", err)
		return exitNotify
	}
	fmt.Fprintf(stdout, "mutation gate: opened the tracking issue #%d, assigned to %s\n", number, *assignee)
	return exitOK
}

// issueBody says the three things a reader needs before opening anything else: what was
// measured (or what stopped the measurement), what it had to reach, and which run it was.
func issueBody(floor float64, reportPath, failure, runURL string) string {
	var b strings.Builder
	b.WriteString(issueMarker)
	b.WriteString("\n\nThe scheduled unscoped mutation run is RED.\n\n")

	measured := ""
	if reportPath != "" {
		if rep, err := mutation.ReadReport(reportPath); err == nil && rep.Score != nil {
			measured = fmt.Sprintf("- measured mutation score: **%.2f%%** (killed %d, lived %d, over the whole mutation domain)\n",
				*rep.Score, rep.Mutants.Killed, rep.Mutants.Lived)
		}
	}
	if measured == "" {
		what := failure
		if strings.TrimSpace(what) == "" {
			what = "the run produced no report, so nothing was measured"
		}
		measured = fmt.Sprintf("- measured mutation score: NONE. %s\n", what)
	}
	b.WriteString(measured)
	b.WriteString(fmt.Sprintf("- floor: **%.2f%%**, from `%s` (`%s` says what it means and why every exclusion is there)\n", floor, mutation.ConfigName, mutation.DocName))
	b.WriteString(fmt.Sprintf("- failing run: %s\n", runURL))
	b.WriteString("\nThe remedy is an assertion that kills a surviving mutant, never a lower floor. ")
	b.WriteString("This issue is updated in place for as long as the scheduled run stays red, and is left open for a human to close.\n")
	return b.String()
}

// --- the issues API -----------------------------------------------------------------

type client struct {
	api   string
	repo  string
	token string
	http  *http.Client
}

type issue struct {
	Number      int             `json:"number"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

func (c *client) do(method, url string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	// The credential is set on the request and never printed: a log line carries
	// identifiers and shapes, never a token (observability O6).
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// findTracking returns the number of the open issue carrying the marker, or 0.
//
// The issues endpoint returns pull requests as well - they are issues to GitHub - so an
// entry carrying a pull_request member is skipped rather than updated.
func (c *client) findTracking() (int, error) {
	url := fmt.Sprintf("%s/repos/%s/issues?state=open&per_page=100", c.api, c.repo)
	b, err := c.do(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	var issues []issue
	if err := json.Unmarshal(b, &issues); err != nil {
		return 0, fmt.Errorf("parse the issue list: %w", err)
	}
	for _, i := range issues {
		if len(i.PullRequest) > 0 {
			continue
		}
		if strings.Contains(i.Body, issueMarker) {
			return i.Number, nil
		}
	}
	return 0, nil
}

func (c *client) createIssue(title, body, assignee string) (int, error) {
	url := fmt.Sprintf("%s/repos/%s/issues", c.api, c.repo)
	payload := map[string]any{
		"title":     title,
		"body":      body,
		"assignees": []string{assignee},
	}
	b, err := c.do(http.MethodPost, url, payload)
	if err != nil {
		return 0, err
	}
	var created issue
	if err := json.Unmarshal(b, &created); err != nil {
		return 0, fmt.Errorf("parse the created issue: %w", err)
	}
	return created.Number, nil
}

func (c *client) updateIssue(number int, body, assignee string) error {
	url := fmt.Sprintf("%s/repos/%s/issues/%d", c.api, c.repo, number)
	payload := map[string]any{
		"body":      body,
		"assignees": []string{assignee},
		"state":     "open",
	}
	_, err := c.do(http.MethodPatch, url, payload)
	return err
}

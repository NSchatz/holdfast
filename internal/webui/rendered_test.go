package webui

// The dashboard's half of AC15j, graded against a RENDERED page.
//
// Every other test in this package reads index.html as TEXT. That is enough to say what
// the source contains and it is not enough to say what an operator SEES: the rows are
// built by JavaScript from a snapshot that arrives over SSE, so the string
// `"indeterminate"` appearing in the file proves only that somebody typed it. What the
// criterion is about is the state a job is REPORTED IN - "as the state it is in, and
// SHALL NOT ... as a success or as a failure that left the source intact" - and only a
// browser that has actually run the page's script can answer that.
//
// So this file loads the served page in a real browser engine, feeds it one snapshot
// carrying all four terminal states, and reads the DOM the page produced. It then proves
// the grader BITES by rendering a MUTATED page - one whose result cell reports a parked
// job as `failed`, which is exactly the pre-existing lie this whole item removes - and
// asserting the same reading of the same DOM fails.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// browserCandidates are the binaries this grader will drive, in order. HOLDFAST_BROWSER
// overrides the search.
//
// There is deliberately NO skip when none is found. A criterion about what the page
// shows, silently unproven because a machine had no browser, is the "false green" this
// repo refuses everywhere else - the fixture suite fails loud without ffmpeg for exactly
// the same reason, and CI installs what the proof needs rather than skipping past it.
var browserCandidates = []string{
	"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome",
}

func browserBin(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("HOLDFAST_BROWSER"); v != "" {
		if _, err := exec.LookPath(v); err != nil {
			t.Fatalf("::error:: HOLDFAST_BROWSER=%q is not executable: %v", v, err)
		}
		return v
	}
	for _, c := range browserCandidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	t.Fatalf("::error:: no browser found (tried %v) - the dashboard graders load the page in a "+
		"real engine because what the page SHOWS cannot be read off its source text; "+
		"install chromium or set HOLDFAST_BROWSER", browserCandidates)
	return ""
}

// jobRow is one history row in the snapshot the page renders.
type jobRow struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

// snapshotJSON is the wire shape the page's SSE listener parses. Only the fields the
// render path reads are set; the rest are absent, which is what a real snapshot's
// "not recorded" looks like.
func snapshotJSON(t *testing.T, history []jobRow, summary map[string]int) string {
	t.Helper()
	snap := map[string]any{
		"now":     time.Now().UnixMilli(),
		"summary": summary,
		"queue":   []jobRow{},
		"history": history,
		"paused":  false,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// renderPage serves pageHTML at "/" and one SSE snapshot at "/api/events", drives the
// browser at it, and returns the DOM the page produced.
func renderPage(t *testing.T, pageHTML, snapshot string) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", snapshot)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the stream open briefly so the page stays "live" while the DOM is
		// dumped, then let it close.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	profile := t.TempDir()
	cmd := exec.Command(browserBin(t),
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--user-data-dir="+filepath.Join(profile, "profile"),
		"--virtual-time-budget=6000",
		"--run-all-compositor-stages-before-draw",
		"--dump-dom",
		srv.URL+"/",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("::error:: rendering the dashboard failed: %v\n%s", err, out)
	}
	dom := string(out)
	if !strings.Contains(dom, "<table") {
		t.Fatalf("the browser produced no rendered dashboard (%d bytes):\n%s", len(dom), truncate(dom))
	}
	return dom
}

func truncate(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "\n...[truncated]"
	}
	return s
}

// rowRe matches one rendered table row.
var rowRe = regexp.MustCompile(`(?s)<tr\b.*?</tr>`)

// statusOf reads, OUT OF THE RENDERED DOM, the state the page reports for a path: the
// text of the row's status span, and the st-<state> class it carries. Both come from the
// page's own output, so a page that renders one thing and names another is caught.
func statusOf(dom, path string) (text, class string, found bool) {
	classRe := regexp.MustCompile(`class="st st-([a-z-]+)"`)
	for _, row := range rowRe.FindAllString(dom, -1) {
		if !strings.Contains(row, path) {
			continue
		}
		m := classRe.FindStringSubmatch(row)
		if m == nil {
			return "", "", false
		}
		// The status word is the text node the page appends after the dot span.
		span := regexp.MustCompile(`(?s)<span class="st st-[a-z-]+">.*?</span>\s*([a-z-]+)`).FindStringSubmatch(row)
		if span == nil {
			return "", m[1], true
		}
		return strings.TrimSpace(span[1]), m[1], true
	}
	return "", "", false
}

// readOutcomes is the assertion, factored out so it can be run against the shipped page
// (where it must pass) and against a mutated one (where it must fail). It returns the
// problems it found rather than calling t.Error, which is what makes the mutation proof
// possible at all.
func readOutcomes(dom string) []string {
	var problems []string
	want := map[string]string{
		"/lib/parked.mkv":  "indeterminate",
		"/lib/applied.mkv": "applied-despite-error",
		"/lib/failed.mkv":  "failed",
		"/lib/done.mkv":    "done",
	}
	for path, state := range want {
		text, class, found := statusOf(dom, path)
		if !found {
			problems = append(problems, fmt.Sprintf("%s: no rendered row for it at all", path))
			continue
		}
		if text != state {
			problems = append(problems, fmt.Sprintf("%s: the page reports %q, want %q", path, text, state))
		}
		if class != state {
			problems = append(problems, fmt.Sprintf("%s: the rendered row carries st-%s, want st-%s", path, class, state))
		}
	}
	return problems
}

// TestRendered_TheTwoNewOutcomesShowAsThemselves is AC15j on the page: a job parked
// indeterminate and one applied despite an error are RENDERED as the states they are in,
// never as a success and never as "failed" - which on this dashboard has always carried
// the second half of the sentence, "and your source is fine".
func TestRendered_TheTwoNewOutcomesShowAsThemselves(t *testing.T) {
	now := time.Now().UnixMilli()
	history := []jobRow{
		{Path: "/lib/done.mkv", Status: "done", UpdatedAt: now},
		{Path: "/lib/failed.mkv", Status: "failed", Reason: "encode error", UpdatedAt: now},
		{Path: "/lib/parked.mkv", Status: "indeterminate",
			Reason: "swap failed and the outcome could not be established", UpdatedAt: now},
		{Path: "/lib/applied.mkv", Status: "applied-despite-error",
			Reason: "the rename took effect despite the error", UpdatedAt: now},
	}
	summary := map[string]int{
		"done": 1, "failed": 1, "indeterminate": 1, "applied-despite-error": 1,
	}
	dom := renderPage(t, string(indexHTML), snapshotJSON(t, history, summary))

	if problems := readOutcomes(dom); len(problems) > 0 {
		t.Fatalf("the RENDERED dashboard misreports a swap outcome:\n  %s", strings.Join(problems, "\n  "))
	}

	// The parked row also has to tell the operator what to do, and it must not carry
	// the intact-source reassurance a `failed` row carries.
	parkedRow := rowFor(dom, "/lib/parked.mkv")
	if !strings.Contains(parkedRow, "holdfast resolve") {
		t.Errorf("the rendered parked row does not say how to resolve it:\n%s", parkedRow)
	}
	if strings.Contains(strings.ToLower(parkedRow), "source untouched") ||
		strings.Contains(strings.ToLower(parkedRow), "source left untouched") {
		t.Errorf("the rendered parked row claims the source is untouched - which is the one thing nobody knows:\n%s", parkedRow)
	}

	// And the summary chips count them as themselves rather than folding them in.
	for _, state := range []string{"indeterminate", "applied-despite-error"} {
		if !strings.Contains(dom, `class="chip `+state+`"`) {
			t.Errorf("the rendered page has no summary chip for %q", state)
		}
	}
}

// TestRendered_TheGraderBitesWhenTheDashboardLies is the mutation proof. A grader that
// cannot fail is not evidence, and this one is doing the job a text grader could not, so
// it has to be shown failing on the specific lie: a build whose result cell reports a
// parked job as `failed`. The mutation is a one-line change to the page's own render
// path, and the SAME reading of the SAME DOM must then report the problem.
func TestRendered_TheGraderBitesWhenTheDashboardLies(t *testing.T) {
	const shipped = `const st = mk("span", "st st-" + j.status);`
	page := string(indexHTML)
	if !strings.Contains(page, shipped) {
		t.Fatalf("the page no longer builds the status cell as %q - the mutation would prove nothing", shipped)
	}
	// Report every terminal state as "failed", which is exactly the pre-existing lie:
	// "failed" on this dashboard has always meant the source survived.
	lying := strings.Replace(page, shipped,
		`const st = mk("span", "st st-failed"); j = Object.assign({}, j, { status: "failed" });`, 1)

	now := time.Now().UnixMilli()
	history := []jobRow{
		{Path: "/lib/done.mkv", Status: "done", UpdatedAt: now},
		{Path: "/lib/failed.mkv", Status: "failed", Reason: "encode error", UpdatedAt: now},
		{Path: "/lib/parked.mkv", Status: "indeterminate", Reason: "unknown", UpdatedAt: now},
		{Path: "/lib/applied.mkv", Status: "applied-despite-error", Reason: "applied", UpdatedAt: now},
	}
	dom := renderPage(t, lying, snapshotJSON(t, history, map[string]int{
		"done": 1, "failed": 1, "indeterminate": 1, "applied-despite-error": 1,
	}))

	problems := readOutcomes(dom)
	if len(problems) == 0 {
		t.Fatal("the rendered grader passed a dashboard that reports every job as failed - it cannot fail, so it is not evidence")
	}
	joined := strings.Join(problems, "\n  ")
	for _, want := range []string{"/lib/parked.mkv", "/lib/applied.mkv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the grader did not catch the lie about %s:\n  %s", want, joined)
		}
	}
}

// rowFor returns the rendered row containing path, or "".
func rowFor(dom, path string) string {
	for _, row := range rowRe.FindAllString(dom, -1) {
		if strings.Contains(row, path) {
			return row
		}
	}
	return ""
}

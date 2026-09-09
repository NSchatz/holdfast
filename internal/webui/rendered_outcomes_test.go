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
//
// It sits beside rendered_test.go rather than inside it. Both drive a real browser and
// they arrived independently - LICENSE-3's grades what the SOURCE OFFER shows, this one
// grades what a JOB'S OUTCOME shows - so they share a technique and nothing else: two
// probes, two subjects, two sets of assertions. Merging them would give one file two
// unrelated reasons to change. The file name says which subject is which.
//
// It renders the GENERATED, COMMITTED index.html (WEBUI-10), which is the document the
// binary embeds and therefore the one an operator loads. The page's SOURCE of the two
// new statuses is `src/js/10-constants.js`, `src/js/40-cells.js`, `src/js/20-derive.js`
// and `src/dashboard.css`; `make webui-stale` is what keeps the two in step, so this
// grader can never be reading a document the sources no longer produce.
//
// HOW the DOM is obtained is not this file's own business, and used to be. It drove the
// browser itself and read the document out of `--dump-dom` under a virtual time budget,
// which is the ONE thing this package had already decided against: an SSE page holds a
// fetch open, virtual time cannot advance while one is pending, and the dashboard's own
// EventSource reconnects the moment the stream ends - so whether the dump ever happens is
// a race between a reconnect and a budget. rendered_test.go's probePage carries the
// history in as many words ("Two CI runs were lost to exactly that"), and this file was
// the third: it read the DOM out of --dump-dom for 90 seconds and was killed, on the one
// CI job whose engine is not the one the other job proved. It takes its measurement
// through the package's ONE harness now - the served document in a same-origin iframe, a
// stream held open so the page is not reconnecting while it is read, and a verdict the
// page POSTs back, so the TEST owns the deadline and the browser has no say in when the
// measurement is available. What the grader DECIDES is unchanged.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

func browserBin(t *testing.T) string {
	t.Helper()
	return chromium(t)
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
func snapshotJSON(t *testing.T, history []jobRow, summary map[string]int) []byte {
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
	return b
}

// fixturePaths are the four jobs every case here renders. They are what makes the verdict
// READY: the snapshot arrives over SSE, which is after `load`, so a reading taken in the
// probe's load handler can be taken before the rows it is about exist.
var fixturePaths = []string{"/lib/done.mkv", "/lib/failed.mkv", "/lib/parked.mkv", "/lib/applied.mkv"}

// outcomesProbeJS runs inside the probe page and hands back the DOM the dashboard
// produced, exactly as it stands after the page's own script has rendered the snapshot.
// It decides nothing: every assertion is Go's, over the same bytes as before.
const outcomesProbeJS = `
var WANT = %PATHS%;
function verdict(doc, win) {
  var rows = Array.prototype.slice.call(doc.querySelectorAll("tr"));
  var rendered = WANT.filter(function (p) {
    return rows.some(function (r) { return r.textContent.indexOf(p) !== -1; });
  });
  return { ready: rendered.length === WANT.length, rendered: rendered,
           dom: doc.documentElement.outerHTML };
}
`

// outcomesVerdict is what the probe posts back.
type outcomesVerdict struct {
	Ready    bool     `json:"ready"`
	Rendered []string `json:"rendered"`
	DOM      string   `json:"dom"`
	Error    string   `json:"error"`
}

// renderPage serves the REAL document (mutate rewrites the served bytes to build a
// counterexample), pushes it one real SSE snapshot, and returns the DOM the page produced.
//
// The stream the harness opens is held until the test finishes, which is the property the
// old driver did not have: a page whose EventSource is reconnecting is a page with a fetch
// permanently pending, and nothing that waits on the browser to decide it is done can
// terminate against one.
func renderPage(t *testing.T, mutate func([]byte) []byte, snapshot []byte) string {
	t.Helper()
	bin := browserBin(t)

	paths, err := json.Marshal(fixturePaths)
	if err != nil {
		t.Fatal(err)
	}
	ps := serveDocumentWith(t, serveOpts{
		url:      sourceoffer.Upstream,
		mutate:   mutate,
		probe:    strings.Replace(outcomesProbeJS, "%PATHS%", string(paths), 1),
		snapshot: snapshot,
	})

	raw, log, err := runProbe(bin, ps, verdictDeadline, t.TempDir())
	if err != nil {
		t.Fatalf("::error:: %v", err)
	}
	var v outcomesVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("the verdict is not JSON (%v): %s\nbrowser output:\n%s", err, truncate(string(raw)), log)
	}
	if v.Error != "" {
		t.Fatalf("the probe failed inside the browser: %s\nbrowser output:\n%s", v.Error, log)
	}
	// A page that never rendered the rows is a page with nothing to decide, and reading a
	// missing row as a caught lie would make the mutation proof pass against a build that
	// simply died. Say which rows arrived.
	if !v.Ready {
		t.Fatalf("the rendered page carries only %d of the %d fixture rows (%v)\nbrowser output:\n%s\n%s",
			len(v.Rendered), len(fixturePaths), v.Rendered, log, truncate(v.DOM))
	}
	if !strings.Contains(v.DOM, "<table") {
		t.Fatalf("the browser produced no rendered dashboard (%d bytes):\n%s", len(v.DOM), truncate(v.DOM))
	}
	return v.DOM
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
	dom := renderPage(t, nil, snapshotJSON(t, history, summary))

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

	// And the summary chips count them as themselves rather than folding them in. The
	// chip is found by the CLASS it carries and not by its whole class attribute: a chip
	// also records which group it is in (work in hand, or finished), and a check that
	// matched the attribute whole would red on any class added beside the state without
	// anything having stopped being counted as itself.
	for _, state := range []string{"indeterminate", "applied-despite-error"} {
		re := regexp.MustCompile(`class="chip[^"]*\b` + regexp.QuoteMeta(state) + `\b[^"]*"`)
		if !re.MatchString(dom) {
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
	if !strings.Contains(string(indexHTML), shipped) {
		t.Fatalf("the page no longer builds the status cell as %q - the mutation would prove nothing", shipped)
	}
	// Report every terminal state as "failed", which is exactly the pre-existing lie:
	// "failed" on this dashboard has always meant the source survived. The rewrite is
	// applied to the bytes the REAL handler served, so what is measured is the shipped
	// document with one line changed and nothing else.
	// `applied` is checked after the render rather than inside the rewrite: the harness
	// routes every unmatched path through the same handler, so the rewrite also sees the
	// 404 body of a stray favicon request, and refusing THAT would fail a run in which the
	// document was mutated perfectly well.
	const lie = `const st = mk("span", "st st-failed"); j = Object.assign({}, j, { status: "failed" });`
	var applied atomic.Bool
	lying := func(b []byte) []byte {
		if !bytes.Contains(b, []byte(shipped)) {
			return b
		}
		applied.Store(true)
		return bytes.Replace(b, []byte(shipped), []byte(lie), 1)
	}

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
	if !applied.Load() {
		t.Fatal("the served document never carried the status cell the mutation rewrites - the proof measured the shipped page")
	}

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

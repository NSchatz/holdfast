package webui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// The RENDERED graders for the per-file surface (S0094).
//
// Every criterion here is about what an operator SEES and what happens when they OPERATE
// the page, so every one is decided by loading the SERVED document in a real browser
// engine and reading what the engine rendered: computed style after the whole cascade,
// real layout geometry, the text innerText says a reader can see, a hit test at the
// subject's own centre, and - where the claim is about what a control ANNOUNCES - the
// accessibility tree the engine itself computed.
//
// None of it is decided by matching HTML or CSS source text. A text grader cannot decide
// what a rule applies to, what wins the cascade, or what is shown rather than merely
// built, and hardening one only ever closes the hole it was shown.
//
// The engine is driven through internal/webui/e2e/driver.mjs, this repository's ONE
// driver, exactly as the prose graders drive it. The page under measurement is the one the
// real handler serves, and the endpoints it calls are stood up beside it, so what a grader
// measures is the page's own fetch, its own render and its own refusal handling.

// pfProbeJS is the measuring script for this file. It MEASURES and decides nothing: every
// verdict below is taken in Go, from what this reports.
const pfProbeJS = `
function pfShown(el) {
  if (!el) return { present: false, visible: false, hit: false };
  for (let n = el; n; n = n.parentElement) {
    const cs = getComputedStyle(n);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.visibility === "collapse") {
      return { present: true, visible: false, hit: false, why: n.tagName.toLowerCase() + " is " + cs.display + "/" + cs.visibility };
    }
    if (Number(cs.opacity) === 0) return { present: true, visible: false, hit: false, why: "opacity 0" };
  }
  // Deliberately NO check of the hidden ATTRIBUTE. What the engine did with it is already
  // in the computed display above, and asking the attribute as well would make this a
  // reading of the markup: a rule that puts a [hidden] row back on the screen would then
  // be reported as a hidden row, which is the one case a rendered grader exists to catch.
  try { el.scrollIntoView({ block: "center" }); } catch (e) { el.scrollIntoView(); }
  const r = el.getBoundingClientRect();
  const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
  return {
    present: true,
    visible: r.width > 0 && r.height > 0 && el.offsetParent !== null,
    hit: !!hit && (hit === el || el.contains(hit) || hit.contains(el)),
    box: { x: r.x, y: r.y, w: r.width, h: r.height }
  };
}
function pfText(sel) { const e = document.querySelector(sel); return e ? String(e.innerText || "").trim() : ""; }
function pfRows(id) {
  const body = document.getElementById(id);
  if (!body) return [];
  return Array.prototype.map.call(body.children, function (tr) {
    const path = tr.querySelector("td.path");
    const remedy = tr.querySelector("td.remedy");
    return {
      state: tr.dataset.state || "",
      stateText: tr.dataset.state ? String(tr.innerText || "").trim() : "",
      path: path ? String(path.textContent || "").trim() : "",
      remedy: remedy ? String(remedy.innerText || "").trim() : "",
      remedyButtons: remedy ? remedy.querySelectorAll("button").length : 0,
      remedyDisabled: remedy ? remedy.querySelectorAll("button:disabled").length : 0,
      shown: pfShown(tr)
    };
  });
}
function pfHeld() {
  const ul = document.getElementById("held-list");
  const entries = ul ? Array.prototype.map.call(ul.children, function (li) {
    const b = li.querySelector("button");
    return { path: String(li.querySelector(".heldpath").textContent || "").trim(),
             button: b ? String(b.textContent || "").trim() : "", shown: pfShown(li) };
  }) : [];
  return {
    region: pfShown(document.getElementById("held")),
    heading: pfText("#h-held"),
    scope: pfText("#held-scope"),
    scopeShown: pfShown(document.getElementById("held-scope")),
    msg: pfText("#held-msg"),
    msgShown: pfShown(document.getElementById("held-msg")),
    entries: entries
  };
}
function pfFound() {
  const body = document.getElementById("search-results");
  const stateEl = body ? body.querySelector("[data-state]") : null;
  return {
    region: pfShown(document.getElementById("found")),
    heading: pfText("#h-found"),
    count: pfText("#found-count"),
    countShown: pfShown(document.getElementById("found-count")),
    msg: pfText("#search-msg"),
    state: stateEl ? stateEl.dataset.state : "",
    stateText: stateEl ? String(stateEl.innerText || "").trim() : "",
    stateShown: pfShown(stateEl),
    rows: pfRows("search-results"),
    insideHistory: !!(body && document.getElementById("history") &&
      (document.getElementById("history").contains(body) || body.contains(document.getElementById("history")))),
    insideQueue: !!(body && document.getElementById("queue") &&
      (document.getElementById("queue").contains(body) || body.contains(document.getElementById("queue"))))
  };
}
function pfControl(id) {
  const el = document.getElementById(id);
  const lab = el ? document.querySelector('label[for="' + id + '"]') : null;
  return {
    present: !!el,
    labelText: lab ? String(lab.innerText || "").trim() : "",
    labelShown: pfShown(lab),
    shown: pfShown(el),
    placeholder: el ? (el.getAttribute("placeholder") || "") : ""
  };
}
function pfRequests() { return performance.getEntriesByType("resource").map(function (e) { return e.name; }); }
function pfType(id, value) {
  const el = document.getElementById(id);
  el.value = value;
  el.dispatchEvent(new Event("input", { bubbles: true }));
  return true;
}
function pfClick(sel) { const e = document.querySelector(sel); if (!e) return false; e.click(); return true; }
;true
`

// pfReading is one whole reading of the per-file surface.
type pfReading struct {
	Queue   []pfRow  `json:"queue"`
	History []pfRow  `json:"history"`
	Found   pfFound  `json:"found"`
	Held    pfHeld   `json:"held"`
	Body    string   `json:"body"`
	Reqs    []string `json:"reqs"`
}

type pfShownRec struct {
	Present bool   `json:"present"`
	Visible bool   `json:"visible"`
	Hit     bool   `json:"hit"`
	Why     string `json:"why"`
	Box     struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		W float64 `json:"w"`
		H float64 `json:"h"`
	} `json:"box"`
}

func (s pfShownRec) reached() bool { return s.Present && s.Visible && s.Hit }

func (s pfShownRec) problem(what string) string {
	if !s.Present {
		return what + " is not in the rendered document"
	}
	if !s.Visible {
		return what + " is in the document but never reached the screen: " + s.Why
	}
	if !s.Hit {
		return what + " is laid out but something is painted over it"
	}
	return ""
}

type pfRow struct {
	State          string     `json:"state"`
	StateText      string     `json:"stateText"`
	Path           string     `json:"path"`
	Remedy         string     `json:"remedy"`
	RemedyButtons  int        `json:"remedyButtons"`
	RemedyDisabled int        `json:"remedyDisabled"`
	Shown          pfShownRec `json:"shown"`
}

type pfFound struct {
	Region        pfShownRec `json:"region"`
	Heading       string     `json:"heading"`
	Count         string     `json:"count"`
	CountShown    pfShownRec `json:"countShown"`
	Msg           string     `json:"msg"`
	State         string     `json:"state"`
	StateText     string     `json:"stateText"`
	StateShown    pfShownRec `json:"stateShown"`
	Rows          []pfRow    `json:"rows"`
	InsideHistory bool       `json:"insideHistory"`
	InsideQueue   bool       `json:"insideQueue"`
}

type pfHeldEntry struct {
	Path   string     `json:"path"`
	Button string     `json:"button"`
	Shown  pfShownRec `json:"shown"`
}

type pfHeld struct {
	Region     pfShownRec    `json:"region"`
	Heading    string        `json:"heading"`
	Scope      string        `json:"scope"`
	ScopeShown pfShownRec    `json:"scopeShown"`
	Msg        string        `json:"msg"`
	MsgShown   pfShownRec    `json:"msgShown"`
	Entries    []pfHeldEntry `json:"entries"`
}

type pfControlRec struct {
	Present     bool       `json:"present"`
	LabelText   string     `json:"labelText"`
	LabelShown  pfShownRec `json:"labelShown"`
	Shown       pfShownRec `json:"shown"`
	Placeholder string     `json:"placeholder"`
}

// pfPage opens the dashboard in the engine with a per-file world around it and waits until
// a whole snapshot has been through the page. It returns the tab, so a grader can go on
// operating it.
func pfPage(t *testing.T, b *engineBrowser, o serveOpts) *enginePage {
	t.Helper()
	if o.url == "" {
		o.url = sourceoffer.Upstream
	}
	if o.snapshot == nil && o.rawSnapshot == "" && !o.holdStream && o.eventsStatus == 0 {
		o.snapshot = mixedSnapshot()
	}
	ps := serveDocumentWith(t, o)
	p := b.newPage()
	p.viewport(1280, 1024)
	p.emulate("dark", false)
	p.navigate(ps.url + "/")
	p.mustEval(pfProbeJS, nil)
	// The page's OWN record that a whole snapshot went through it: the screen-reader
	// summary is written at the end of every render and nowhere else.
	if err := p.waitUntil(`document.getElementById("sr-status").textContent.trim() !== ""`, 60*time.Second); err != nil {
		t.Fatalf("%v\nbrowser output:\n%s", err, b.output())
	}
	return p
}

// pfRead takes one whole reading.
func pfRead(t *testing.T, p *enginePage) pfReading {
	t.Helper()
	var r pfReading
	p.mustEval(`({queue: pfRows("queue"), history: pfRows("history"), found: pfFound(), held: pfHeld(),
		body: document.body.innerText, reqs: pfRequests()})`, &r)
	return r
}

// pfSetToken puts a control token in the page's own field and fires the change the page
// listens for, which is what makes the token-gated reads reachable.
func pfSetToken(t *testing.T, p *enginePage, token string) {
	t.Helper()
	p.mustEval(fmt.Sprintf(`(function(){var e=document.getElementById("token");e.value=%q;
		e.dispatchEvent(new Event("change",{bubbles:true}));return true;})()`, token), nil)
}

// pfSearch types a term into the page's OWN search control and clicks its OWN button.
func pfSearch(t *testing.T, p *enginePage, term string) {
	t.Helper()
	p.mustEval(fmt.Sprintf(`(function(){var e=document.getElementById("ledger-search");e.value=%q;
		document.getElementById("search-go").click();return true;})()`, term), nil)
}

// --- criterion 1: the filter hides loaded rows and asks the server nothing -----------

func TestRendered_FilterHidesOnlyLoadedRowsAndQueriesNothing(t *testing.T) {
	b := launchEngine(t)
	p := pfPage(t, b, serveOpts{snapshot: mixedSnapshot()})

	before := pfRead(t, p)
	if len(before.Queue) != 2 || len(before.History) != 3 {
		t.Fatalf("the fixture did not render: %d queue rows, %d history rows", len(before.Queue), len(before.History))
	}
	for _, r := range append(append([]pfRow{}, before.Queue...), before.History...) {
		if prob := r.Shown.problem("the row " + r.Path + " before filtering"); prob != "" {
			t.Fatalf("%s", prob)
		}
	}
	requestsBefore := len(before.Reqs)

	// The operator types into the page's own control, and the page's own handler runs.
	p.mustEval(`pfType("filter", "shows")`, nil)
	after := pfRead(t, p)

	if len(after.Queue) != 2 || len(after.History) != 3 {
		t.Fatalf("filtering removed rows from the document; the criterion is about what is RENDERED: %d queue, %d history",
			len(after.Queue), len(after.History))
	}
	for _, r := range append(append([]pfRow{}, after.Queue...), after.History...) {
		want := strings.Contains(r.Path, "/media/shows/")
		if got := r.Shown.reached(); got != want {
			t.Errorf("with the filter %q the row %q is shown=%v, want %v (%s)",
				"shows", r.Path, got, want, r.Shown.problem(r.Path))
		}
	}
	// BOTH TABLES. A filter that reached only one of them would pass every assertion above
	// if the fixture happened to put every match in that one, so each table is required to
	// have hidden something of its own.
	hidQueue, hidHistory := 0, 0
	for _, r := range after.Queue {
		if !r.Shown.reached() {
			hidQueue++
		}
	}
	for _, r := range after.History {
		if !r.Shown.reached() {
			hidHistory++
		}
	}
	if hidQueue == 0 || hidHistory == 0 {
		t.Errorf("the filter hid %d queue row(s) and %d history row(s); it must cover BOTH tables", hidQueue, hidHistory)
	}

	// AND IT ASKED THE SERVER NOTHING. The reading is the engine's own record of every
	// resource the page fetched, so a request issued by the filter is visible here whatever
	// the page did with the answer.
	if len(after.Reqs) != requestsBefore {
		t.Errorf("filtering issued %d request(s) to the server: %v",
			len(after.Reqs)-requestsBefore, after.Reqs[requestsBefore:])
	}

	// A term that matches nothing hides every row, and one that matches everything hides
	// none, so the grader cannot be satisfied by a filter that does nothing either way.
	p.mustEval(`pfType("filter", "no-such-path")`, nil)
	none := pfRead(t, p)
	for _, r := range append(append([]pfRow{}, none.Queue...), none.History...) {
		if r.Shown.reached() {
			t.Errorf("the row %q is still shown under a filter that matches no path", r.Path)
		}
	}
	p.mustEval(`pfType("filter", "/media/")`, nil)
	all := pfRead(t, p)
	for _, r := range append(append([]pfRow{}, all.Queue...), all.History...) {
		if prob := r.Shown.problem("the row " + r.Path + " under a filter matching every path"); prob != "" {
			t.Errorf("%s", prob)
		}
	}
	if len(all.Reqs) != requestsBefore {
		t.Errorf("the filter issued %d request(s) in total: %v", len(all.Reqs)-requestsBefore, all.Reqs[requestsBefore:])
	}

	// THE GRADER'S OWN PROOF. This criterion describes behaviour the page ALREADY HAD, so
	// unlike every other grader here it does not red merely from the feature being absent -
	// which makes "it passed" worth nothing until the reading is shown to bite. Each half
	// is driven against a document that defeats exactly that half.

	// (1) A rule that puts a hidden row back on the screen. The page still sets `hidden`,
	// so a grader reading the ATTRIBUTE would report a clean filter; only a reading of what
	// the engine rendered can tell.
	blind := pfPage(t, b, serveOpts{
		snapshot: mixedSnapshot(),
		mutate: func(doc []byte) []byte {
			return []byte(strings.Replace(string(doc), "</style>",
				"tbody#history tr[hidden], tbody#queue tr[hidden] { display: table-row !important; }\n</style>", 1))
		},
	})
	blind.mustEval(`pfType("filter", "no-such-path")`, nil)
	blindRead := pfRead(t, blind)
	stillShown := 0
	for _, r := range append(append([]pfRow{}, blindRead.Queue...), blindRead.History...) {
		if r.Shown.reached() {
			stillShown++
		}
	}
	if stillShown == 0 {
		t.Error("the reading passed a document in which every filtered-out row was forced back onto the screen, so it is reading the attribute rather than the render")
	}

	// (2) A filter that TALKS TO THE SERVER. The page's own handler still runs and the rows
	// still hide correctly; the only difference is a request, which is the half the
	// criterion adds and the half a rendering assertion alone would never see.
	chatty := pfPage(t, b, serveOpts{
		snapshot: mixedSnapshot(),
		mutate: func(doc []byte) []byte {
			return []byte(strings.Replace(string(doc), "</body>",
				`<script>document.getElementById("filter").addEventListener("input",function(){fetch("/api/history?limit=1");});</script></body>`, 1))
		},
	})
	quiet := len(pfRead(t, chatty).Reqs)
	chatty.mustEval(`pfType("filter", "shows")`, nil)
	if err := chatty.waitUntil(`performance.getEntriesByType("resource").length > `+fmt.Sprint(quiet), 15*time.Second); err != nil {
		t.Logf("the mutated page's request had not landed within the wait (%v); the assertion below says what was seen", err)
	}
	if got := len(pfRead(t, chatty).Reqs); got == quiet {
		t.Errorf("the reading passed a filter that issued a request to the server (%d requests before, %d after)", quiet, got)
	}
}

// --- criterion 2: the two controls read as two different questions -------------------

func TestRendered_FilterAndLedgerSearchReadAsDifferentQuestions(t *testing.T) {
	b := launchEngine(t)
	// A CAPPED table, which is the world the criterion names: the page is showing fewer
	// rows than the ledger holds, so the distinction between filtering what is on screen
	// and searching what is not actually matters.
	p := pfPage(t, b, serveOpts{snapshot: cappedSnapshot()})

	var filter, search pfControlRec
	p.mustEval(`pfControl("filter")`, &filter)
	p.mustEval(`pfControl("ledger-search")`, &search)

	for _, c := range []struct {
		what string
		rec  pfControlRec
	}{{"the filter", filter}, {"the ledger search", search}} {
		if !c.rec.Present {
			t.Fatalf("%s control is not in the rendered document", c.what)
		}
		if prob := c.rec.Shown.problem(c.what + " control"); prob != "" {
			t.Errorf("%s", prob)
		}
		if prob := c.rec.LabelShown.problem(c.what + " control's label"); prob != "" {
			t.Errorf("%s", prob)
		}
	}

	// IN TEXT A READER CAN SEE. The label of each control is the text that says which
	// question it answers, and both are in what innerText reports as visible.
	r := pfRead(t, p)
	if !strings.Contains(collapseSpace(r.Body), collapseSpace(filter.LabelText)) {
		t.Errorf("the filter's label %q is in the document but not in the text a reader can see", filter.LabelText)
	}
	if !strings.Contains(collapseSpace(r.Body), collapseSpace(search.LabelText)) {
		t.Errorf("the search's label %q is in the document but not in the text a reader can see", search.LabelText)
	}
	// The filter's covers the ROWS ON SCREEN; the search's covers the LEDGER. Each is
	// required to name its own scope, because two controls labelled "Filter" and "Search"
	// would be two words for what a reader still has to guess at.
	if !strings.Contains(strings.ToLower(filter.LabelText), "rows on screen") {
		t.Errorf("the filter is labelled %q, which does not say it covers only the rows on screen", filter.LabelText)
	}
	if !strings.Contains(strings.ToLower(search.LabelText), "ledger") {
		t.Errorf("the ledger search is labelled %q, which does not say it covers the ledger", search.LabelText)
	}

	// AND THE ENGINE GIVES THEM DISTINCT ACCESSIBLE NAMES. That is not a property of the
	// markup - it is the engine's own answer over labels, ARIA, native semantics and
	// content - so it is read from the tree the engine computed and from nowhere else.
	names := map[string]string{}
	for _, n := range p.proseAXTree() {
		if n.Ignored {
			continue
		}
		name := n.Name.text()
		if name == "" {
			continue
		}
		role := n.Role.text()
		if role != "textbox" && role != "searchbox" && role != "combobox" {
			continue
		}
		names[name] = role
	}
	filterNamed, searchNamed := false, false
	for name := range names {
		if collapseSpace(name) == collapseSpace(filter.LabelText) {
			filterNamed = true
		}
		if collapseSpace(name) == collapseSpace(search.LabelText) {
			searchNamed = true
		}
	}
	if !filterNamed {
		t.Errorf("the accessibility tree gives no text control the name %q; the names it does give are %v",
			filter.LabelText, names)
	}
	if !searchNamed {
		t.Errorf("the accessibility tree gives no text control the name %q; the names it does give are %v",
			search.LabelText, names)
	}
	if collapseSpace(filter.LabelText) == collapseSpace(search.LabelText) {
		t.Errorf("both controls announce themselves as %q, so a reader at a screen reader meets one question twice",
			filter.LabelText)
	}

	// And the page really is showing a capped view, or the distinction the criterion is
	// about would be one nobody has to make.
	if !strings.Contains(r.Body, "capped") {
		t.Errorf("the fixture rendered no cap notice, so the page under measurement is not the capped view the criterion names:\n%s", r.Body)
	}
}

// --- criterion 4: the results are their own region ----------------------------------

// searchAnsweredJS is how a grader waits for the ledger search to have ANSWERED, and it is
// ONE constant on purpose: the grader below waits on it and
// TestRendered_TheLedgerSearchWaitTellsAskedFromAnswered proves it can tell the two apart,
// so weakening it back reds that proof rather than silently re-opening the race.
//
// It asks for a child that is NOT a state row. The obvious `children.length > 0` is
// satisfied by the LOADING row, because `#search-results` IS the `data-view="search"` host
// and setViewState puts its <tr data-state="loading"> inside that same tbody - so that wait
// returns the instant the search is dispatched, and a grader reading then counts the
// placeholder as its one result. This is the page's own viewHasContent predicate, asked
// from outside.
const searchAnsweredJS = `(function () {
	const b = document.getElementById("search-results");
	if (!b) return false;
	for (const el of b.children) { if (!(el.dataset && el.dataset.state)) return true; }
	return false;
})()`

func TestRendered_LedgerSearchResultsAreTheirOwnRegion(t *testing.T) {
	b := launchEngine(t)
	p := pfPage(t, b, serveOpts{
		snapshot:      mixedSnapshot(),
		searchResults: []string{deepRow("/media/archive/2011/golf.mkv"), deepRow("/media/archive/2011/hotel.mkv")},
		searchTotal:   2,
		// The answer is held back a little ON PURPOSE, so the loading window this grader
		// has to wait THROUGH exists on every run rather than only on a loaded machine.
		// Without it the race that produced the one red here was invisible until a
		// full-package run went wide, which is the worst way to meet a wait that is wrong.
		searchDelay: 400 * time.Millisecond,
	})
	pfSetToken(t, p, "secret")

	before := pfRead(t, p)
	if before.Found.Region.reached() {
		t.Fatalf("the results region is on screen before any search was made: %+v", before.Found)
	}
	historyBefore := len(before.History)
	queueBefore := len(before.Queue)

	pfSearch(t, p, "archive")
	if err := p.waitUntil(searchAnsweredJS, 30*time.Second); err != nil {
		t.Fatalf("%v\nbrowser output:\n%s", err, b.output())
	}
	after := pfRead(t, p)

	if prob := after.Found.Region.problem("the ledger search's results region"); prob != "" {
		t.Fatalf("%s", prob)
	}
	if after.Found.Heading == "" {
		t.Error("the results region carries no heading a reader can see, so nothing tells it apart from the tables")
	}
	if len(after.Found.Rows) != 2 {
		t.Fatalf("the results region holds %d rows, want the 2 the search returned: %+v", len(after.Found.Rows), after.Found.Rows)
	}
	for _, r := range after.Found.Rows {
		if prob := r.Shown.problem("the search result " + r.Path); prob != "" {
			t.Errorf("%s", prob)
		}
		if !showsText(after.Body, r.Path) {
			t.Errorf("the search result %q is in the document but not in the text a reader can see", r.Path)
		}
	}

	// AND NOT SILENTLY MERGED. Neither capped table gained a row, and neither contains the
	// results body - so a reader scanning the history table cannot meet a row that came
	// from somewhere else.
	if len(after.History) != historyBefore {
		t.Errorf("the history table went from %d rows to %d after a ledger search; the results were merged into it",
			historyBefore, len(after.History))
	}
	if len(after.Queue) != queueBefore {
		t.Errorf("the queue table went from %d rows to %d after a ledger search", queueBefore, len(after.Queue))
	}
	if after.Found.InsideHistory || after.Found.InsideQueue {
		t.Error("the results are rendered INSIDE one of the capped tables")
	}
	for _, r := range after.History {
		if strings.Contains(r.Path, "/media/archive/") {
			t.Errorf("a ledger search result appeared in the history table: %q", r.Path)
		}
	}
	// The count it found is stated beside them, over the whole ledger.
	if prob := after.Found.CountShown.problem("the match count"); prob != "" {
		t.Errorf("%s", prob)
	}
	if !strings.Contains(after.Found.Count, "2") {
		t.Errorf("the results region states the match count as %q, want the 2 the server reported", after.Found.Count)
	}
}

// The wait the grader above uses has to tell "the search has been ASKED" from "the search
// has ANSWERED", and this proves it does by driving the one moment where the two look the
// same.
//
// The failure it exists to stop is not hypothetical: `#search-results` is the view host,
// so setViewState puts the LOADING row inside that same tbody, and a wait on
// `children.length > 0` is satisfied the instant the search is dispatched. Under a loaded
// full-package run that raced, and criterion 4's grader read one row - the placeholder,
// `State:loading StateText:"Searching every recorded row"` - where two results were owed.
//
// So the answer is held back here, the two predicates are asked in the one window where
// the region holds the placeholder and nothing else, and the case asserts they DISAGREE:
// the old one is already true and the new one is not. Then the answer lands and the new
// one becomes true over the real rows. A wait that could not tell them apart fails this.
func TestRendered_TheLedgerSearchWaitTellsAskedFromAnswered(t *testing.T) {
	// The weak predicate is written out here, because it is the counterexample. The strong
	// one is NOT: it is searchAnsweredJS, the very constant the grader above waits on, so
	// this case grades the wait that actually runs rather than a copy of it that could
	// stay right while the original went wrong.
	const anyChild = `document.getElementById("search-results").children.length > 0`
	const answered = searchAnsweredJS

	b := launchEngine(t)
	p := pfPage(t, b, serveOpts{
		snapshot:      mixedSnapshot(),
		searchResults: []string{deepRow("/media/archive/2011/golf.mkv"), deepRow("/media/archive/2011/hotel.mkv")},
		searchTotal:   2,
		searchDelay:   3 * time.Second,
	})
	pfSetToken(t, p, "secret")
	pfSearch(t, p, "archive")

	// The window: the region is showing its loading row and the answer has not arrived.
	if err := p.waitUntil(`!!document.querySelector('[data-view="search"] [data-state="loading"]')`,
		20*time.Second); err != nil {
		t.Fatalf("the search never reached its loading state, so the window this case measures in "+
			"never opened: %v\nbrowser output:\n%s", err, b.output())
	}
	var weak, strong bool
	p.mustEval(anyChild, &weak)
	p.mustEval(answered, &strong)
	if !weak {
		t.Error("the LOADING row did not satisfy `children.length > 0`, so this case is measuring " +
			"a window in which the two predicates were never going to differ")
	}
	if strong {
		t.Error("the wait predicate is already true while the region holds nothing but its loading row; " +
			"it cannot tell an asked search from an answered one, which is the whole of what it is for")
	}

	// And it turns true on the answer, over the rows the search really returned.
	if err := p.waitUntil(answered, 30*time.Second); err != nil {
		t.Fatalf("the wait predicate never turned true after the search answered: %v\nbrowser output:\n%s",
			err, b.output())
	}
	got := pfRead(t, p)
	if len(got.Found.Rows) != 2 {
		t.Errorf("the wait returned with %d rows on screen, want the 2 the search returned: %+v",
			len(got.Found.Rows), got.Found.Rows)
	}
	for _, r := range got.Found.Rows {
		if r.State != "" {
			t.Errorf("the wait returned with a STATE row still in the results region: %+v", r)
		}
	}
}

// --- criterion 5: the search view owes three states ---------------------------------

func TestRendered_LedgerSearchOwesThreeStates(t *testing.T) {
	b := launchEngine(t)
	words := map[string]string{}

	// EMPTY: a search that matched no row. It says so, and it does NOT render an empty
	// table a reader could take for "no such file" - the search covers the ledger, and the
	// ledger is not the library.
	empty := pfPage(t, b, serveOpts{snapshot: mixedSnapshot(), searchResults: nil, searchTotal: 0})
	pfSetToken(t, empty, "secret")
	pfSearch(t, empty, "no-such-file")
	if err := empty.waitUntil(`!!document.querySelector('[data-view="search"] [data-state="empty"]')`, 30*time.Second); err != nil {
		t.Fatalf("the search never reached its empty state: %v\nbrowser output:\n%s", err, b.output())
	}
	e := pfRead(t, empty)
	if prob := e.Found.StateShown.problem("the search's empty state"); prob != "" {
		t.Errorf("%s", prob)
	}
	if len(strings.Fields(e.Found.StateText)) < 2 {
		t.Errorf("the search's empty state reads %q, which is not a state in words", e.Found.StateText)
	}
	if !strings.Contains(e.Body, e.Found.StateText) {
		t.Error("the search's empty state is in the document but not in the text a reader can see")
	}
	for _, row := range e.Found.Rows {
		if row.State == "" {
			t.Errorf("the empty search rendered a result row: %+v", row)
		}
	}
	words["empty"] = e.Found.StateText

	// LOADING and UNREADABLE, taken off the page's own vocabulary as the modules write it,
	// which is the same object setViewState reads. Reading them here rather than driving a
	// slow response is what keeps the comparison about the WORDS the criterion is about.
	var vocab map[string]map[string]string
	empty.mustEval(`VIEW_STATES`, &vocab)
	for _, state := range []string{"loading", "unreadable"} {
		words[state] = vocab["search"][state]
	}

	// UNREADABLE, driven for real: a refused search puts the view there, and the words are
	// the vocabulary's own.
	refused := pfPage(t, b, serveOpts{snapshot: mixedSnapshot(), searchStatus: 403})
	pfSetToken(t, refused, "secret")
	pfSearch(t, refused, "anything")
	if err := refused.waitUntil(`!!document.querySelector('[data-view="search"] [data-state="unreadable"]')`, 30*time.Second); err != nil {
		t.Fatalf("a refused search never reached its unreadable state: %v\nbrowser output:\n%s", err, b.output())
	}
	u := pfRead(t, refused)
	if prob := u.Found.StateShown.problem("the search's unreadable state"); prob != "" {
		t.Errorf("%s", prob)
	}
	if u.Found.StateText != words["unreadable"] {
		t.Errorf("the search's unreadable state reads %q while the vocabulary the modules write from says %q",
			u.Found.StateText, words["unreadable"])
	}

	// THE THREE ARE DISTINCT, which is the whole of what the criterion asks: a view that
	// said the same thing for "nothing matched" and "could not be read" would be lying
	// about one of them.
	seen := map[string]string{}
	for _, state := range []string{"loading", "empty", "unreadable"} {
		text := words[state]
		if strings.TrimSpace(text) == "" {
			t.Errorf("the search view says nothing at all in its %s state", state)
			continue
		}
		if prev, ok := seen[text]; ok {
			t.Errorf("the search view says %q for BOTH its %s and its %s state", text, prev, state)
		}
		seen[text] = state
	}
	if len(seen) != 3 {
		t.Errorf("the search view produced %d distinct state phrasings, want 3: %v", len(seen), words)
	}
	// And the empty wording does not say the FILE does not exist. The search reaches the
	// ledger; a file nobody has scanned has no row and is not absent from the library.
	if strings.Contains(strings.ToLower(words["empty"]), "no such file") {
		t.Errorf("the search's empty state reads %q, which claims something about the LIBRARY", words["empty"])
	}
}

// --- criterion 7: a refusal is not an empty result ----------------------------------

func TestRendered_LedgerSearchRefusalIsNotAnEmptyResult(t *testing.T) {
	b := launchEngine(t)

	for _, tc := range []struct {
		what   string
		status int
		says   string
	}{
		{"control is disabled on this server", 403, "disabled"},
		{"the control token was refused", 401, "token"},
	} {
		p := pfPage(t, b, serveOpts{snapshot: mixedSnapshot(), searchStatus: tc.status})
		pfSetToken(t, p, "whatever")
		pfSearch(t, p, "alpha")
		if err := p.waitUntil(`document.getElementById("found-count").textContent.trim() !== ""`, 30*time.Second); err != nil {
			t.Fatalf("[%s] the page never rendered an answer to the refused search: %v\nbrowser output:\n%s",
				tc.what, err, b.output())
		}
		r := pfRead(t, p)

		if prob := r.Found.Region.problem("[" + tc.what + "] the results region"); prob != "" {
			t.Errorf("%s", prob)
		}
		// IT SAYS THE SEARCH IS UNAVAILABLE, AND WHY.
		if prob := r.Found.CountShown.problem("[" + tc.what + "] the refusal"); prob != "" {
			t.Errorf("%s", prob)
		}
		if !strings.Contains(strings.ToLower(r.Found.Count), "unavailable") {
			t.Errorf("[%s] the page answers a refused search with %q, which does not say the search is unavailable",
				tc.what, r.Found.Count)
		}
		if !strings.Contains(strings.ToLower(r.Found.Count), tc.says) {
			t.Errorf("[%s] the page says %q, which does not say WHY", tc.what, r.Found.Count)
		}
		if !strings.Contains(r.Body, r.Found.Count) {
			t.Errorf("[%s] the refusal is in the document but not in the text a reader can see", tc.what)
		}

		// AND IT IS NOT AN EMPTY RESULT SET. The view is not in its empty state and the
		// empty state's own words are nowhere on the page: a reader must not be able to
		// take "nobody was allowed to ask" for "nothing matched".
		if r.Found.State == "empty" {
			t.Errorf("[%s] a refused search rendered the EMPTY state, which a reader would take for \"nothing matched\"", tc.what)
		}
		var emptyWords string
		p.mustEval(`VIEW_STATES.search.empty`, &emptyWords)
		if strings.Contains(r.Body, emptyWords) {
			t.Errorf("[%s] a refused search put the empty state's words %q on the page", tc.what, emptyWords)
		}
		for _, row := range r.Found.Rows {
			if row.State == "" {
				t.Errorf("[%s] a refused search rendered a result row: %+v", tc.what, row)
			}
		}
		// Every other control is still operable: a refused action costs nothing but itself.
		for _, id := range []string{"rescan", "pause", "resume", "filter", "ledger-search", "search-go"} {
			var rec pfShownRec
			p.mustEval(fmt.Sprintf(`pfShown(document.getElementById(%q))`, id), &rec)
			if prob := rec.problem("[" + tc.what + "] the control " + id + " after a refused search"); prob != "" {
				t.Errorf("%s", prob)
			}
		}
	}
}

// --- criterion 11: a withholding says it is runtime state ---------------------------

func TestRendered_ExclusionSaysItIsRuntimeState(t *testing.T) {
	b := launchEngine(t)
	p := pfPage(t, b, serveOpts{snapshot: mixedSnapshot(), exclusions: []string{"/media/films/delta.mkv"}})
	pfSetToken(t, p, "secret")
	if err := p.waitUntil(`document.getElementById("held-list").children.length > 0`, 30*time.Second); err != nil {
		t.Fatalf("the withheld paths never rendered: %v\nbrowser output:\n%s", err, b.output())
	}
	r := pfRead(t, p)

	// The withheld path is rendered.
	if len(r.Held.Entries) != 1 || r.Held.Entries[0].Path != "/media/films/delta.mkv" {
		t.Fatalf("the withheld paths render as %+v, want the one the server reported", r.Held.Entries)
	}
	if prob := r.Held.Entries[0].Shown.problem("the withheld path"); prob != "" {
		t.Errorf("%s", prob)
	}

	// And so is the sentence, IN VISIBLE TEXT, saying what kind of thing it is and that the
	// configuration file is not written. Both halves, because an operator who takes a
	// withholding for configuration will go looking for it in a file that does not mention
	// it - and one who is told only "runtime" learns nothing about the file.
	if prob := r.Held.ScopeShown.problem("the runtime-state sentence"); prob != "" {
		t.Fatalf("%s", prob)
	}
	visible := strings.ToLower(collapseSpace(r.Held.Heading + " " + r.Held.Scope))
	for _, want := range []string{"runtime", "config.yaml", "daemon"} {
		if !strings.Contains(visible, want) {
			t.Errorf("the withheld paths are rendered without saying %q; what a reader sees is %q", want, visible)
		}
	}
	if !strings.Contains(collapseSpace(r.Body), collapseSpace(r.Held.Scope)) {
		t.Error("the runtime-state sentence is in the document but not in the text a reader can see")
	}
	// It must not point an operator at a configuration key: this build accepts none, and a
	// page that named one would send them to a loader that refuses to start.
	for _, forbidden := range []string{"exclude_paths", "include_paths"} {
		if strings.Contains(strings.ToLower(r.Body), forbidden) {
			t.Errorf("the page names %q, a configuration key this build does not accept", forbidden)
		}
	}

	// The EXCLUDE ACTION is rendered too, which is the other half of the criterion's
	// trigger: every terminal row carries one, and it is a control a reader can operate.
	if len(r.History) == 0 {
		t.Fatal("no history row rendered, so the exclude action was not under measurement")
	}
	for _, row := range r.History {
		if row.RemedyButtons == 0 {
			t.Errorf("the row %q carries no withholding control at all", row.Path)
		}
		if row.RemedyDisabled != 0 {
			t.Errorf("the row %q carries a DISABLED control, which is the dead end this surface exists to end", row.Path)
		}
	}
}

// --- criterion 16: a failed withholding says nothing changed -------------------------

func TestRendered_AFailedExclusionSaysNothingChanged(t *testing.T) {
	b := launchEngine(t)
	// The store refuses: every request to the withheld paths answers 500.
	p := pfPage(t, b, serveOpts{snapshot: mixedSnapshot(), exclusionsStatus: 500})
	pfSetToken(t, p, "secret")

	before := pfRead(t, p)
	if len(before.History) == 0 {
		t.Fatal("no history row rendered, so there was no withholding control to click")
	}

	// A REAL click on the page's own control.
	var clicked bool
	p.mustEval(`pfClick("#history td.remedy button")`, &clicked)
	if !clicked {
		t.Fatal("the page rendered no withholding control to click")
	}
	// Waited for by CONTENT and not merely by "something is there": the page already had a
	// message on screen before the click, because reading the list failed too, so a wait on
	// a non-empty message would return the answer to a different request. A wait that times
	// out is not the failure - the assertions below are, and they say what was on screen.
	if err := p.waitUntil(
		`document.getElementById("held-msg").textContent.toLowerCase().indexOf("nothing changed") >= 0`,
		20*time.Second); err != nil {
		t.Logf("the page never said nothing changed within the wait (%v); what it did say is asserted below", err)
	}
	after := pfRead(t, p)

	// IT SAYS NOTHING CHANGED, in words, on the screen.
	if prob := after.Held.MsgShown.problem("the failed withholding's message"); prob != "" {
		t.Fatalf("%s", prob)
	}
	if !strings.Contains(strings.ToLower(after.Held.Msg), "nothing changed") {
		t.Errorf("a failed withholding answers %q, which does not say nothing changed", after.Held.Msg)
	}
	if !strings.Contains(after.Body, after.Held.Msg) {
		t.Error("the refusal is in the document but not in the text a reader can see")
	}
	// AND NOTHING A READER COULD TAKE FOR SUCCESS. No withheld path was rendered, because
	// none was recorded.
	if len(after.Held.Entries) != 0 {
		t.Errorf("a failed withholding rendered %d withheld path(s): %+v", len(after.Held.Entries), after.Held.Entries)
	}
	for _, word := range []string{"done", "withheld:", "recorded"} {
		if strings.Contains(strings.ToLower(after.Held.Msg), word) {
			t.Errorf("the refusal reads %q, which carries %q - a reader could take that for success", after.Held.Msg, word)
		}
	}

	// EVERY OTHER CONTROL IS STILL OPERABLE (F1, F5), and the rest of the page is untouched.
	for _, id := range []string{"rescan", "pause", "resume", "filter", "ledger-search", "search-go", "token"} {
		var rec pfShownRec
		p.mustEval(fmt.Sprintf(`pfShown(document.getElementById(%q))`, id), &rec)
		if prob := rec.problem("the control " + id + " after a failed withholding"); prob != "" {
			t.Errorf("%s", prob)
		}
	}
	if len(after.History) != len(before.History) || len(after.Queue) != len(before.Queue) {
		t.Errorf("a failed withholding changed the tables: queue %d -> %d, history %d -> %d",
			len(before.Queue), len(after.Queue), len(before.History), len(after.History))
	}
	for _, row := range after.History {
		if prob := row.Shown.problem("the row " + row.Path + " after a failed withholding"); prob != "" {
			t.Errorf("%s", prob)
		}
	}
}

// --- criterion 17: a terminal row names `holdfast requeue` ---------------------------

func TestRendered_ATerminalRowNamesTheRequeueCommand(t *testing.T) {
	b := launchEngine(t)
	p := pfPage(t, b, serveOpts{snapshot: remedySnapshot()})
	r := pfRead(t, p)

	byPath := map[string]pfRow{}
	for _, row := range r.History {
		byPath[row.Path] = row
	}
	// The rows whose remedy IS re-opening the decision. A parked failure is the case the
	// item was filed about: the file failed three times, parked, and the ledger says why.
	for _, path := range []string{
		"/media/shows/foxtrot.mkv", // failed and parked
		"/media/films/delta.mkv",   // done
		"/media/shows/echo.mkv",    // skipped by a guard
	} {
		row, ok := byPath[path]
		if !ok {
			t.Fatalf("the fixture did not render the row %q: %v", path, byPath)
		}
		if prob := row.Shown.problem("the row " + path); prob != "" {
			t.Errorf("%s", prob)
		}
		want := "holdfast requeue " + path
		if !showsText(row.Remedy, want) {
			t.Errorf("the row %q shows the remedy %q, want it to name %q on the row's own surface", path, row.Remedy, want)
		}
		if !showsText(r.Body, want) {
			t.Errorf("%q is in the document but not in the text a reader can see", want)
		}
		if row.RemedyDisabled != 0 {
			t.Errorf("the row %q presents a DISABLED control; a disabled control carrying no explanation is the dead end this criterion refuses", path)
		}
	}

	// The three rows nothing re-opens do NOT name it - and they say why rather than showing
	// a reader an empty cell they would read as a row nobody had thought about.
	for _, tc := range []struct{ path, says string }{
		{"/media/films/indy.mkv", "resolve"},
		{"/media/films/juliet.mkv", "already in place"},
		{"/media/films/kilo.mkv", "restored"},
	} {
		row, ok := byPath[tc.path]
		if !ok {
			t.Fatalf("the fixture did not render the row %q", tc.path)
		}
		if strings.Contains(row.Remedy, "holdfast requeue") {
			t.Errorf("the row %q names `holdfast requeue`, which cannot re-open it: %q", tc.path, row.Remedy)
		}
		if !strings.Contains(strings.ToLower(row.Remedy), tc.says) {
			t.Errorf("the row %q shows %q, which does not say why nothing re-opens it", tc.path, row.Remedy)
		}
		if row.RemedyDisabled != 0 {
			t.Errorf("the row %q presents a disabled control carrying no explanation", tc.path)
		}
	}

	// THE GRADER BITES. With the remedy cell emptied by a rule that wins the cascade, the
	// same reading must report the command as absent - otherwise every assertion above is
	// one nobody has tried to defeat.
	blind := pfPage(t, b, serveOpts{
		snapshot: remedySnapshot(),
		mutate: func(doc []byte) []byte {
			return []byte(strings.Replace(string(doc), "</style>",
				"td.remedy .cmd { display: none !important; }\n</style>", 1))
		},
	})
	mutated := pfRead(t, blind)
	for _, row := range mutated.History {
		if showsText(row.Remedy, "holdfast requeue") {
			t.Errorf("the reading found the command in a cell a rule hid: %q", row.Remedy)
		}
	}
	if showsText(mutated.Body, "holdfast requeue") {
		t.Error("the reading found the command in the visible text of a page whose remedy cells are hidden, so it is reading the markup rather than the render")
	}
}

// --- the fixtures ------------------------------------------------------------------

// deepRow is one terminal row in the history projection, for a ledger search result.
func deepRow(path string) string {
	return fmt.Sprintf(`{"path":%q,"status":"done","worker":"w1","updated_at":%d,
		"encoder":"cpu","vmaf_mean":97.1,"vmaf_min":90.2,"vmaf_model":"version=vmaf_v0.6.1",
		"source_bytes":2147483648,"output_bytes":1073741824,"encode_ms":600000}`, path, snapNow-100000)
}

// cappedSnapshot is a page whose tables hold fewer rows than the ledger does, which is the
// world the filter-versus-search distinction is actually about.
func cappedSnapshot() []byte {
	return []byte(strings.Replace(string(mixedSnapshot()),
		`"history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":3}`,
		`"history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":9000}`, 1))
}

// remedySnapshot carries one row of every terminal shape the remedy cell has to answer
// for: the three a requeue re-opens, and the three nothing does.
func remedySnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":0,"verifying":0,"done":1,"skipped":2,"failed":1,"indeterminate":1,"applied-despite-error":1},
  "queue": [],
  "history": [
    {"path":"/media/films/delta.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":"cpu","vmaf_mean":98.24,"vmaf_min":91.53,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":4294967296,"output_bytes":1073741824,"encode_ms":5430000},
    {"path":"/media/shows/echo.mkv","status":"skipped","worker":"","updated_at":%d,"reason":"hardlinked",
     "vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/shows/foxtrot.mkv","status":"failed","worker":"w5","updated_at":%d,
     "reason":"vmaf worst frame 43.2 below the floor 60","vmaf_mean":null,"vmaf_min":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/films/indy.mkv","status":"indeterminate","worker":"w2","updated_at":%d,
     "reason":"rename reported EIO and the re-stat could not be completed","vmaf_mean":null,"vmaf_min":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/films/juliet.mkv","status":"applied-despite-error","worker":"w3","updated_at":%d,
     "reason":"rename returned an error the re-stat established it had nonetheless applied","vmaf_mean":null,"vmaf_min":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/films/kilo.mkv","status":"skipped","worker":"","updated_at":%d,"reason":"restored-original",
     "vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":0},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":6},
  "bytes_reclaimed_session": 0, "bytes_reclaimed_lifetime": 3221225472,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": %s
}`, snapNow-7200, snapNow-8000, snapNow-9000, snapNow-10000, snapNow-11000, snapNow-12000, snapNow, healthyAggregates))
}

package webui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// The PAGE-COPY graders (S0052).
//
// This item is an editorial pass: it cuts the prose the dashboard puts between an
// operator and the numbers, and it must prove both halves of that - that the words went,
// and that no operational fact went with them. Every criterion here is about what a
// reader SEES, so every one of them is decided by laying the SERVED document out in a
// real browser engine and reading what the engine says is visible. None of it is decided
// by matching template text: a word count taken off the source cannot tell a rendered
// paragraph from one a rule hid, cannot see text a module writes at render time, and
// cannot say what survives at 360px. That is the operator's ruling of 2026-09-06 and the
// reason S0035 was killed.
//
// Two things these graders need that the probe-page harness cannot give them - the
// accessibility tree, and a real viewport width and colour scheme - come from the
// DevTools protocol driver in cdp_test.go.

// --- what counts as page copy --------------------------------------------------

// copyExclusion is one selector whose text is NOT page copy, together with the clause of
// the specification's own page-copy definition that excludes it. The clause travels with
// the selector deliberately: an exclusion nobody can trace back to the definition is how
// a word budget gets met by reclassifying prose instead of removing it.
type copyExclusion struct {
	Sel    string `json:"sel"`
	Clause string `json:"clause"`
}

// pageCopyExclusions is the whole of what the definition subtracts. Everything else that
// renders is page copy.
//
// Clause 1 is "text whose value comes from the snapshot the server published". It is
// applied to a published value TOGETHER with the fixed label that names it in place (the
// `min`/`mean`/`max` beside a spread, the word `reclaimed` beside a byte figure), because
// the definition's own reason for the subtraction is that these are "not prose the
// operator is being asked to read" - and a datum's inline label is not prose, it is the
// datum's name, which is the same role clause 2 grants a table column header. The reading
// is recorded in the item's notes; it is the only place the definition admitted two.
var pageCopyExclusions = []copyExclusion{
	{"#conn", "1: the rendered connection state"},
	{"#msg", "1: the refusal message a rejected control action produces"},
	{"#sr-status", "1: the per-status counts, announced"},
	{".badge", "1: whether this holdfast is running or paused, and whether a scan is under way"},
	{".reclaimed", "1: the reclaimed figures"},
	{"#chips", "1: the per-status count of files"},
	{"table tbody tr:not([data-state])", "1: every value a queue or history row renders"},
	{".note.cap", "1: the capped-table notice"},
	{".agg .agg-v", "1: an aggregate's value, with the fixed labels that name it in place"},
	{".agg .agg-cov", "1: the set an aggregate was computed over"},
	{".agg .agg-ex", "1: the rows an aggregate excluded"},
	{"h1", "2: headings"},
	{"h2", "2: headings"},
	{"h3", "2: headings"},
	{"th", "2: table column headers"},
	{"label", "3: the visible label of an interactive control"},
	{"button", "3: the visible label of an interactive control"},
	{".source-offer", "4: the AGPL section 13 Corresponding Source offer"},
	{"a.doclink", "AC15: the documentation link's own text"},
}

// proseJS is the measuring script. It runs in the page's own context, over the document
// the browser laid out, and classifies every VISIBLE text node into page copy and the
// categories the definition subtracts.
const proseJS = `
function _visibleEl(el, win) {
  for (let n = el; n; n = n.parentElement) {
    const cs = win.getComputedStyle(n);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.visibility === "collapse") return false;
    if (cs.opacity === "0") return false;
    if (n.hasAttribute && n.hasAttribute("hidden")) return false;
  }
  return true;
}

// A block of page copy is a text node's nearest BLOCK-LEVEL ancestor, which is a property
// of the computed display after the whole cascade - never of the tag the author wrote.
function _blockOf(node, doc, win) {
  for (let n = node.parentElement; n; n = n.parentElement) {
    const d = win.getComputedStyle(n).display;
    if (d.indexOf("inline") !== 0 && d !== "contents" && d !== "none") return n;
  }
  return doc.body;
}

// A stable identity for one block, so the SAME set of blocks can be compared between two
// viewport widths without depending on the order the walk happened to find them in.
function _blockKey(el, doc) {
  const parts = [];
  for (let n = el; n && n !== doc.documentElement; n = n.parentElement) {
    let s = n.tagName.toLowerCase();
    if (n.id) { parts.unshift(s + "#" + n.id); break; }
    if (typeof n.className === "string" && n.className.trim() !== "") {
      s += "." + n.className.trim().split(/\s+/).filter(Boolean).sort().join(".");
    }
    let i = 0;
    if (n.parentElement) {
      for (const sib of n.parentElement.children) { if (sib === n) break; if (sib.tagName === n.tagName) i++; }
    }
    parts.unshift(s + "[" + i + "]");
  }
  return parts.join(">");
}

function _norm(s) { return String(s).replace(/\s+/g, " ").trim(); }

// The excluded set, element by element, each carrying the clause that excluded it.
function _excludedMap(doc, table) {
  const m = new Map();
  for (const row of table) {
    for (const el of doc.querySelectorAll(row.sel)) {
      if (!m.has(el)) m.set(el, row.clause);
      for (const d of el.querySelectorAll("*")) if (!m.has(d)) m.set(d, row.clause);
    }
  }
  return m;
}

// The nearest heading ABOVE a block, in document order, that a reader can see.
function _headingAbove(el, headings, win) {
  let best = null;
  for (const h of headings) {
    if (!_visibleEl(h, win)) continue;
    if (h === el || h.contains(el)) continue;
    if (h.compareDocumentPosition(el) & win.Node.DOCUMENT_POSITION_FOLLOWING) best = h;
  }
  return best;
}

// The table a block introduces: the one it is inside, or the next one in document order
// before any further heading.
function _tableFor(el) {
  const inside = el.closest("table");
  if (inside) return inside;
  for (let n = el; n && n.tagName.toLowerCase() !== "body"; n = n.parentElement) {
    for (let s = n.nextElementSibling; s; s = s.nextElementSibling) {
      const tag = s.tagName.toLowerCase();
      if (tag === "h1" || tag === "h2" || tag === "h3") return null;
      if (tag === "table") return s;
      if (s.querySelector && s.querySelector("h1,h2,h3")) return null;
      if (s.querySelector) { const t = s.querySelector("table"); if (t) return t; }
    }
  }
  return null;
}

function _linksIn(root, win) {
  return Array.prototype.map.call(root.querySelectorAll("a"), function (a) {
    return { href: a.href, raw: a.getAttribute("href") || "", text: _norm(a.textContent),
             name: _norm(a.getAttribute("aria-label") || a.getAttribute("title") || a.textContent),
             shown: renderedFlag(a, win) };
  });
}

// One interactive control, as the browser laid it out. "label" is what a <label for=…>
// puts on it; "placeholder" is the hint inside it. Both are excluded from page copy by
// the definition, and both are still facts an operator needs the page to keep.
function _control(doc, win, id) {
  const el = doc.getElementById(id);
  if (!el) return { present: false };
  const label = doc.querySelector('label[for="' + id + '"]');
  return {
    present: true,
    tag: el.tagName.toLowerCase(),
    type: el.getAttribute("type") || "",
    shown: renderedFlag(el, win),
    enabled: !el.disabled,
    label: label ? _norm(visText(label)) : "",
    text: _norm(visText(el)),
    placeholder: el.getAttribute("placeholder") || ""
  };
}

// The RAW character data of the cells that carry attacker-influencable values. textContent
// rather than innerText, deliberately: the question is whether the page rendered the
// value the server published character for character, and innerText would collapse the
// whitespace inside it before the comparison could see it.
function _rawCells(doc, sel) {
  return Array.prototype.map.call(doc.querySelectorAll(sel), function (el) { return el.textContent; });
}

// _part is one named piece of a row or a card, with BOTH questions answered: what it
// says, and whether it reached the screen. Only the second one catches a value that is
// in the document and hidden by a rule, which is the way a fact leaves a page without
// leaving its markup.
function _part(root, sel, win) {
  const el = root.querySelector(sel);
  return { text: el ? _norm(visText(el)) : "", shown: !!el && renderedFlag(el, win) };
}

// Every cell of every data row, keyed by the class the renderer gives it.
function _rowParts(doc, win, id) {
  const body = doc.getElementById(id);
  if (!body) return [];
  const out = [];
  for (const tr of body.children) {
    if (tr.dataset.state) continue;
    const cells = {};
    for (const td of tr.children) {
      const cls = String(td.className || "").trim().split(/\s+/)[0] || "cell";
      cells[cls] = { text: _norm(visText(td)), shown: renderedFlag(td, win) };
    }
    out.push({ table: id, cells: cells });
  }
  return out;
}

function proseReading(doc, win, table) {
  const excluded = _excludedMap(doc, table);
  const headings = Array.prototype.slice.call(doc.querySelectorAll("h1,h2,h3"));
  const blocks = new Map();
  const dropped = [];
  const walker = doc.createTreeWalker(doc.body, win.NodeFilter.SHOW_TEXT, null);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    const raw = n.nodeValue;
    if (!raw || !/\S/.test(raw)) continue;
    const p = n.parentElement;
    if (!p) continue;
    const tag = p.tagName.toLowerCase();
    if (tag === "script" || tag === "style" || tag === "title" || tag === "template") continue;
    if (!_visibleEl(p, win)) continue;
    // The engine's own answer to "does this text occupy space on the screen".
    const r = doc.createRange();
    r.selectNodeContents(n);
    const box = r.getBoundingClientRect();
    if (box.width <= 0 || box.height <= 0) continue;

    const clause = excluded.get(p);
    if (clause) { dropped.push({ text: _norm(raw), clause: clause }); continue; }

    const blk = _blockOf(n, doc, win);
    const key = _blockKey(blk, doc);
    if (!blocks.has(key)) {
      const h = _headingAbove(blk, headings, win);
      const tbl = _tableFor(blk);
      blocks.set(key, {
        key: key,
        tag: blk.tagName.toLowerCase(),
        texts: [],
        heading: h ? _norm(h.innerText || h.textContent) : "",
        columns: tbl ? Array.prototype.map.call(tbl.querySelectorAll("th"),
          function (th) { return _norm(th.innerText || th.textContent); }) : []
      });
    }
    blocks.get(key).texts.push(_norm(raw));
  }
  const out = [];
  for (const b of blocks.values()) {
    b.text = _norm(b.texts.join(" "));
    delete b.texts;
    if (b.text !== "") out.push(b);
  }
  out.sort(function (a, b) { return a.key < b.key ? -1 : (a.key > b.key ? 1 : 0); });

  const sections = Array.prototype.map.call(doc.querySelectorAll("section"), function (s) {
    return { id: s.id, heading: _norm(visText(s.querySelector("h2"))), links: _linksIn(s, win) };
  });

  const offer = doc.querySelector("p.source-offer");

  // Every view's own state, as the page reports it: the view's name, which of the three
  // states it is in, and the words it says so in. A view showing its own content carries
  // no state element and reports the empty string, which is a fourth answer and not one of
  // the three.
  const views = Array.prototype.map.call(doc.querySelectorAll("[data-view]"), function (host) {
    const el = host.querySelector("[data-state]");
    return { view: host.dataset.view, state: el ? el.dataset.state : "",
             text: el ? _norm(visText(el)) : "", shown: !!el && renderedFlag(el, win) };
  });

  return {
    // The page's OWN state vocabulary, read out of the running document rather than out
    // of the source: the shell ships a loading element with its words already in it, and
    // the modules write the same words from this object, so the two are a pair nothing
    // else holds in step. null means the page has no such object, which is a failure
    // rather than a vocabulary with nothing in it.
    viewVocabulary: (typeof VIEW_STATES === "undefined") ? null : VIEW_STATES,
    blocks: out,
    excludedText: dropped,
    sections: sections,
    documentLinks: _linksIn(doc, win),
    controls: {
      token: _control(doc, win, "token"),
      rescan: _control(doc, win, "rescan"),
      pause: _control(doc, win, "pause"),
      resume: _control(doc, win, "resume"),
      filter: _control(doc, win, "filter")
    },
    sourceOffer: {
      present: !!offer,
      text: offer ? offer.textContent : "",
      shown: renderedFlag(offer, win)
    },
    queueParts: _rowParts(doc, win, "queue"),
    historyParts: _rowParts(doc, win, "history"),
    aggregateParts: Array.prototype.map.call(doc.querySelectorAll("#aggregates .agg"), function (el) {
      return { name: _part(el, ".agg-k", win), value: _part(el, ".agg-v", win),
               coverage: _part(el, ".agg-cov", win), excluded: _part(el, ".agg-ex", win) };
    }),
    queuePaths: _rawCells(doc, "#queue td.path"),
    historyPaths: _rawCells(doc, "#history td.path"),
    failureReasons: _rawCells(doc, "#history td.st .reason"),
    absencePhrases: _rawCells(doc, ".nr"),
    views: views,
    elementCount: doc.getElementsByTagName("*").length,
    imgCount: doc.getElementsByTagName("img").length,
    handlerAttrs: doc.querySelectorAll("[onerror],[onload],[onclick],[onmouseover]").length,
    controlMessage: _norm(visText(doc.getElementById("msg"))),
    bodyText: doc.body ? doc.body.innerText : "",
    viewportWidth: win.innerWidth,
    colourScheme: win.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light",
    bodyScrollWidth: doc.body ? doc.body.scrollWidth : 0,
    bodyClientWidth: doc.documentElement ? doc.documentElement.clientWidth : 0
  };
}
`

// --- the reading, decoded ------------------------------------------------------

type copyBlock struct {
	Key     string   `json:"key"`
	Tag     string   `json:"tag"`
	Text    string   `json:"text"`
	Heading string   `json:"heading"`
	Columns []string `json:"columns"`
}

// words applies the definition: a maximal run of non-whitespace characters, after
// collapsing runs of whitespace.
func (b copyBlock) words() []string { return strings.Fields(b.Text) }

type excludedText struct {
	Text   string `json:"text"`
	Clause string `json:"clause"`
}

// A link's NAME is what tells a reader what it is for. Text is one way of carrying one
// and it is not the only way: the documentation link is a MARK, so its name is on the
// element, and its accessible name is what a screen reader announces either way. What
// must never happen is a link with no name at all - one that announces itself as "link"
// and nothing more - so the name is collected from wherever it is carried and the
// graders require it rather than requiring text.
//
// The authority on an accessible name is the ENGINE, not this reading: the accessibility
// tree the browser computed is checked separately by the DevTools-protocol graders. This
// is the cheap reading that keeps every other grader honest about the difference between
// "has no text" and "has no name".
type renderedLink struct {
	Href  string `json:"href"`
	Raw   string `json:"raw"`
	Text  string `json:"text"`
	Name  string `json:"name"`
	Shown bool   `json:"shown"`
}

type proseSection struct {
	ID      string         `json:"id"`
	Heading string         `json:"heading"`
	Links   []renderedLink `json:"links"`
}

// renderedPart is one named piece of a row or a card: what it says, and whether the
// engine put it on the screen. Both are needed - a value hidden by a rule is still in
// the document's text content, and a check that read only the text would pass a page
// that shows a reader none of it.
type renderedPart struct {
	Text  string `json:"text"`
	Shown bool   `json:"shown"`
}

type rowParts struct {
	Table string                  `json:"table"`
	Cells map[string]renderedPart `json:"cells"`
}

type aggParts struct {
	Name     renderedPart `json:"name"`
	Value    renderedPart `json:"value"`
	Coverage renderedPart `json:"coverage"`
	Excluded renderedPart `json:"excluded"`
}

// renderedControl is one interactive control as the browser laid it out.
type renderedControl struct {
	Present     bool   `json:"present"`
	Tag         string `json:"tag"`
	Type        string `json:"type"`
	Shown       bool   `json:"shown"`
	Enabled     bool   `json:"enabled"`
	Label       string `json:"label"`
	Text        string `json:"text"`
	Placeholder string `json:"placeholder"`
}

// named is the control's own name to a reader: its visible label, its text, or the hint
// inside it. A control with none of the three is a control nobody can identify.
func (c renderedControl) named() string {
	for _, s := range []string{c.Label, c.Text, c.Placeholder} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

type proseReading struct {
	Blocks        []copyBlock    `json:"blocks"`
	ExcludedText  []excludedText `json:"excludedText"`
	Sections      []proseSection `json:"sections"`
	DocumentLinks []renderedLink `json:"documentLinks"`
	Controls      struct {
		Token  renderedControl `json:"token"`
		Rescan renderedControl `json:"rescan"`
		Pause  renderedControl `json:"pause"`
		Resume renderedControl `json:"resume"`
		Filter renderedControl `json:"filter"`
	} `json:"controls"`
	SourceOffer struct {
		Present bool   `json:"present"`
		Text    string `json:"text"`
		Shown   bool   `json:"shown"`
	} `json:"sourceOffer"`
	QueueParts      []rowParts                   `json:"queueParts"`
	HistoryParts    []rowParts                   `json:"historyParts"`
	AggregateParts  []aggParts                   `json:"aggregateParts"`
	QueuePaths      []string                     `json:"queuePaths"`
	HistoryPaths    []string                     `json:"historyPaths"`
	FailureReasons  []string                     `json:"failureReasons"`
	AbsencePhrases  []string                     `json:"absencePhrases"`
	Views           []viewState                  `json:"views"`
	Vocabulary      map[string]map[string]string `json:"viewVocabulary"`
	ElementCount    int                          `json:"elementCount"`
	ImgCount        int                          `json:"imgCount"`
	HandlerAttrs    int                          `json:"handlerAttrs"`
	ControlMessage  string                       `json:"controlMessage"`
	BodyText        string                       `json:"bodyText"`
	ViewportWidth   int                          `json:"viewportWidth"`
	ColourScheme    string                       `json:"colourScheme"`
	BodyScrollWidth float64                      `json:"bodyScrollWidth"`
	BodyClientWidth float64                      `json:"bodyClientWidth"`
}

// viewState is one data view's own answer to "what am I showing": which of the three
// states F7 names it is in, the words it says so in, and whether those words reached the
// screen. A view showing its own content carries no state element and reports "".
type viewState struct {
	View  string `json:"view"`
	State string `json:"state"`
	Text  string `json:"text"`
	Shown bool   `json:"shown"`
}

// view returns one view's state by name, and whether the reading found that view at all.
func (p proseReading) view(name string) (viewState, bool) {
	for _, v := range p.Views {
		if v.View == name {
			return v, true
		}
	}
	return viewState{}, false
}

// isPageCopy reports whether some text was counted against the budget. The preservation
// criteria each end with "and the grader SHALL NOT count that text as page copy", which
// is a claim about the CLASSIFIER and has to be asserted rather than assumed - a
// classifier that quietly counted a published value would make the budget a fiction.
func (p proseReading) isPageCopy(text string) bool {
	want := normalisedSentence(text)
	if want == "" {
		return false
	}
	for _, b := range p.Blocks {
		if strings.Contains(normalisedSentence(b.Text), want) {
			return true
		}
	}
	return false
}

// excludedUnder returns the clause the classifier subtracted some text under, or "".
func (p proseReading) excludedUnder(text string) string {
	want := normalisedSentence(text)
	for _, e := range p.ExcludedText {
		if want != "" && strings.Contains(normalisedSentence(e.Text), want) {
			return e.Clause
		}
	}
	return ""
}

// total is the whole document's page-copy word count.
func (p proseReading) total() int {
	n := 0
	for _, b := range p.Blocks {
		n += len(b.words())
	}
	return n
}

// blockKeys is the set AC3 compares between two viewport widths.
func (p proseReading) blockKeys() []string {
	out := make([]string, 0, len(p.Blocks))
	for _, b := range p.Blocks {
		out = append(out, b.Key)
	}
	sort.Strings(out)
	return out
}

func (p proseReading) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d page-copy words at %dpx (%s):\n", p.total(), p.ViewportWidth, p.ColourScheme)
	for _, blk := range p.Blocks {
		fmt.Fprintf(&b, "  [%2d] %-46s %q\n", len(blk.words()), blk.Key, blk.Text)
	}
	return b.String()
}

// --- driving one reading -------------------------------------------------------

// proseOpts is everything one reading can vary. The DOCUMENT is always the one the real
// handler produces; only the world around it, and the deliberate counterexample
// mutations, move.
type proseOpts struct {
	snapshot []byte
	mutate   func([]byte) []byte
	// width and height are the real viewport the page is laid out at.
	width, height int
	// scheme is the colour scheme the cascade resolves `prefers-color-scheme` against.
	scheme string
	// mode is the connection state the reading waits for: "live" (default) or "down".
	mode string
	// noSnapshot leaves the page with nothing pushed to it, which is the state every
	// view is in before its first snapshot arrives. The stream is OPENED and delivers
	// nothing, which is what the loading state means: connected, with no snapshot.
	noSnapshot  bool
	streamFails bool
	// controlStatus, when non-zero, is the status every mutating API endpoint answers
	// with; clickRescan then drives a real control action against it.
	controlStatus int
	clickRescan   bool
	// badSnapshot pushes a snapshot event whose data cannot be read at all.
	badSnapshot bool
}

type proseVerdict struct {
	prose proseReading
	dash  dashVerdict
	ax    []proseAXNode
	log   string
}

const proseDefaultWidth = 1280
const proseDefaultHeight = 1024

// unreadablePayload is a `data:` field that is not JSON at all, which is how a snapshot
// that cannot be read is delivered down a stream that is otherwise perfectly healthy.
const unreadablePayload = `this is not a snapshot`

// proseBrowser launches ONE browser for a whole test. Each reading gets its own tab, so a
// suite of engine-driven graders costs one browser launch rather than one per reading.
func proseBrowser(t *testing.T) *cdpBrowser {
	t.Helper()
	return launchCDP(t)
}

// renderProse lays the served document out in the engine at a real viewport and colour
// scheme, waits for the page to reach the state the reading is about, and returns the
// page-copy classification, the dashboard reading the existing graders use, and the
// accessibility tree.
func renderProse(t *testing.T, b *cdpBrowser, o proseOpts) proseVerdict {
	t.Helper()
	if o.width == 0 {
		o.width = proseDefaultWidth
	}
	if o.height == 0 {
		o.height = proseDefaultHeight
	}
	if o.scheme == "" {
		o.scheme = "dark"
	}
	mode := o.mode
	if mode == "" {
		mode = "live"
	}
	so := serveOpts{
		url:           sourceoffer.Upstream,
		mutate:        o.mutate,
		probe:         probeJS,
		snapshot:      o.snapshot,
		streamFails:   o.streamFails,
		controlStatus: o.controlStatus,
	}
	switch {
	case o.noSnapshot:
		// A stream that opens and delivers nothing. The page is connected and has no
		// snapshot, which is exactly the loading state.
		so.snapshot, so.holdStream = nil, true
	case o.badSnapshot:
		so.snapshot, so.rawSnapshot = nil, unreadablePayload
	case so.snapshot == nil:
		so.snapshot = fixtureSnapshot()
	}
	ps := serveDocumentWith(t, so)

	p := b.newPage()
	p.viewport(o.width, o.height)
	p.emulate(o.scheme, false)
	p.navigate(ps.url + "/")

	// The measuring scripts, defined once in this tab's own context.
	js := strings.Replace(dashProbeJS, "%MODE%", mode, 1)
	js = strings.Replace(js, "%FILTER%", "", 1)
	js = strings.Replace(js, "%STRIP%", "0", 1)
	p.mustEval(js+"\n"+proseJS+"\n;true", nil)

	// Wait for the state the reading is about. Before its first snapshot the page has
	// rendered nothing to wait for, so the wait is the load itself; an unreadable payload
	// is waited for at the view that says so.
	switch {
	case o.noSnapshot:
	case o.badSnapshot:
		if err := p.waitUntil(`!!document.querySelector('[data-state="unreadable"]')`, 60*time.Second); err != nil {
			t.Fatalf("%v\nbrowser output:\n%s", err, b.browserLog())
		}
	default:
		if err := p.waitUntil(`isReady(document)`, 60*time.Second); err != nil {
			var state string
			_ = p.eval(`connText(document)`, &state)
			t.Fatalf("%v (connection state %q)\nbrowser output:\n%s", err, state, b.browserLog())
		}
	}
	if o.clickRescan {
		p.mustEval(`document.getElementById("rescan").click(); true`, nil)
		if err := p.waitUntil(`document.getElementById("msg").textContent.trim() !== "" &&
			document.getElementById("msg").textContent.trim() !== "working"`, 30*time.Second); err != nil {
			t.Fatalf("%v\nbrowser output:\n%s", err, b.browserLog())
		}
	}

	table, err := json.Marshal(pageCopyExclusions)
	if err != nil {
		t.Fatalf("encoding the exclusion table: %v", err)
	}
	var v proseVerdict
	p.mustEval(fmt.Sprintf("proseReading(document, window, %s)", table), &v.prose)
	if !o.noSnapshot && !o.badSnapshot {
		p.mustEval("verdict(document, window)", &v.dash)
	}
	v.ax = p.proseAXTree()
	v.log = b.browserLog()
	return v
}

// --- the accessibility tree, with the sources AC4 is decided on --------------------

// proseAXValueSource is where one accessible name or description came FROM. It is the
// field that makes AC4 gradeable at all: a name computed from an element's own visible
// text is the text AC1 and AC2 already count, while a name that came from an attribute or
// a related element is copy a reader meets only through a screen reader or a hover.
type proseAXValueSource struct {
	Type      string `json:"type"`
	Attribute string `json:"attribute"`
	Value     *struct {
		Value json.RawMessage `json:"value"`
	} `json:"value"`
	Superseded bool `json:"superseded"`
	Invalid    bool `json:"invalid"`
}

type proseAXValue struct {
	Type    string               `json:"type"`
	Value   json.RawMessage      `json:"value"`
	Sources []proseAXValueSource `json:"sources"`
}

// text is the string an AXValue carries, or "" when it carries something else.
func (v *proseAXValue) text() string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Value, &s); err != nil {
		return ""
	}
	return s
}

// fromContentsOnly reports whether every source that actually contributed this value was
// the element's own text content. Such a value is the visible text under another name.
func (v *proseAXValue) fromContentsOnly() bool {
	if v == nil || len(v.Sources) == 0 {
		return false
	}
	contributed := 0
	for _, s := range v.Sources {
		if s.Superseded || s.Invalid || s.Value == nil || len(s.Value.Value) == 0 {
			continue
		}
		contributed++
		if s.Type != "contents" {
			return false
		}
	}
	return contributed > 0
}

type proseAXNode struct {
	NodeID      string        `json:"nodeId"`
	Ignored     bool          `json:"ignored"`
	Role        *proseAXValue `json:"role"`
	Name        *proseAXValue `json:"name"`
	Description *proseAXValue `json:"description"`
}

// proseAXTree reads the tree the ENGINE computed, with each value's SOURCES, which the
// driver's own axTree does not decode. There is no web API for any of it: an accessible
// name is the engine's answer, not a property of the markup.
func (p *cdpPage) proseAXTree() []proseAXNode {
	p.b.t.Helper()
	var out struct {
		Nodes []proseAXNode `json:"nodes"`
	}
	mustJSON(p.b.t, p.b.mustCall(p.sid, "Accessibility.getFullAXTree", nil), &out)
	return out.Nodes
}

// --- the committed word lists and records --------------------------------------

// proseDir holds the two records these graders read: the function-word list the
// content-word definition subtracts, and the removal record that maps every sentence
// this item took off the surface to the heading it now lives under.
const proseDir = "prose"

func proseFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(proseDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(proseDir, name), err)
	}
	return string(b)
}

// loadFunctionWords reads the committed list. It is committed so the measurement is
// reproducible by anyone who runs the gate, and so the mutation proof below can run the
// same graders against an EMPTY one.
func loadFunctionWords(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(proseFile(t, "function-words.txt"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, w := range strings.Fields(line) {
			out[strings.ToLower(w)] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("the committed function-word list is empty")
	}
	return out
}

// foldWord is the normalisation the content-word comparisons are made under: case
// folding, which the definition names, plus stripping the punctuation a word carries at
// its edges. The second half is not decoration - without it a full stop on the end of a
// word would defeat every overlap and repetition check below, which is a hole a grader
// cannot be hardened out of afterwards. Word COUNTING is unaffected: a count is over
// maximal runs of non-whitespace, exactly as the definition says.
func foldWord(w string) string {
	return strings.ToLower(strings.Trim(w, ".,;:!?()[]{}\"'`’“”·…—–&/"))
}

// contentWords is a block's page-copy words, folded, with the committed function words
// removed.
func contentWords(text string, fn map[string]bool) []string {
	var out []string
	for _, w := range strings.Fields(text) {
		f := foldWord(w)
		if f == "" || fn[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func contentWordSet(text string, fn map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, w := range contentWords(text, fn) {
		out[w] = true
	}
	return out
}

// --- the graders, each written to be provable against a mutation ----------------

// pageCopyBudget is AC1's ceiling on the whole document.
const pageCopyBudget = 60

// blockCopyCeiling is AC2's ceiling on one block.
const blockCopyCeiling = 8

func gradeTotalBudget(p proseReading) []string {
	if n := p.total(); n > pageCopyBudget {
		return []string{fmt.Sprintf("the rendered document carries %d words of page copy at %dpx in the %s scheme, want at most %d\n%s",
			n, p.ViewportWidth, p.ColourScheme, pageCopyBudget, p)}
	}
	return nil
}

func gradeBlockCeiling(p proseReading) []string {
	var out []string
	for _, b := range p.Blocks {
		if n := len(b.words()); n > blockCopyCeiling {
			out = append(out, fmt.Sprintf("the block %s renders %d words of page copy, want at most %d: %q",
				b.Key, n, blockCopyCeiling, b.Text))
		}
	}
	return out
}

func gradeSameBlocksAtBothWidths(narrow, wide proseReading) []string {
	a, b := narrow.blockKeys(), wide.blockKeys()
	if strings.Join(a, "\n") == strings.Join(b, "\n") {
		return nil
	}
	in := func(set []string, k string) bool {
		for _, s := range set {
			if s == k {
				return true
			}
		}
		return false
	}
	var out []string
	for _, k := range b {
		if !in(a, k) {
			out = append(out, fmt.Sprintf("the block %s renders at %dpx but not at %dpx", k, wide.ViewportWidth, narrow.ViewportWidth))
		}
	}
	for _, k := range a {
		if !in(b, k) {
			out = append(out, fmt.Sprintf("the block %s renders at %dpx but not at %dpx", k, narrow.ViewportWidth, wide.ViewportWidth))
		}
	}
	return out
}

func gradeHeadingOverlap(p proseReading, fn map[string]bool) []string {
	var out []string
	for _, b := range p.Blocks {
		head := contentWordSet(b.Heading, fn)
		for _, w := range contentWords(b.Text, fn) {
			if head[w] {
				out = append(out, fmt.Sprintf("the block %s repeats the content word %q from the heading above it (%q): %q",
					b.Key, w, b.Heading, b.Text))
			}
		}
		cols := map[string]bool{}
		for _, c := range b.Columns {
			for _, w := range contentWords(c, fn) {
				cols[w] = true
			}
		}
		for _, w := range contentWords(b.Text, fn) {
			if cols[w] {
				out = append(out, fmt.Sprintf("the block %s repeats the content word %q from a column header of the table it introduces (%v): %q",
					b.Key, w, b.Columns, b.Text))
			}
		}
	}
	return out
}

// gradeRepeatedRuns is AC6: no run of three or more consecutive content words may occur
// in two different blocks of page copy.
func gradeRepeatedRuns(p proseReading, fn map[string]bool) []string {
	const runLen = 3
	where := map[string][]string{}
	for _, b := range p.Blocks {
		cw := contentWords(b.Text, fn)
		seen := map[string]bool{}
		for i := 0; i+runLen <= len(cw); i++ {
			run := strings.Join(cw[i:i+runLen], " ")
			if seen[run] {
				continue
			}
			seen[run] = true
			where[run] = append(where[run], b.Key)
		}
	}
	var runs []string
	for run, keys := range where {
		if len(keys) > 1 {
			sort.Strings(keys)
			runs = append(runs, fmt.Sprintf("the run %q occurs in %d different blocks of page copy (%v)", run, len(keys), keys))
		}
	}
	sort.Strings(runs)
	return runs
}

// --- AC1, AC2, AC3: the budget, the ceiling, and the two widths -----------------

func TestRendered_PageCopyIsInsideItsBudgetAtBothWidthsAndInBothThemes(t *testing.T) {
	b := proseBrowser(t)
	for _, scheme := range []string{"dark", "light"} {
		for _, w := range []struct {
			px int
			h  int
		}{{360, 800}, {1280, 1024}} {
			p := renderProse(t, b, proseOpts{width: w.px, height: w.h, scheme: scheme}).prose
			if p.ViewportWidth != w.px {
				t.Fatalf("the reading was taken at %dpx, want %dpx", p.ViewportWidth, w.px)
			}
			if p.ColourScheme != scheme {
				t.Fatalf("the reading was taken in the %s scheme, want %s", p.ColourScheme, scheme)
			}
			for _, f := range gradeTotalBudget(p) {
				t.Error(f)
			}
			for _, f := range gradeBlockCeiling(p) {
				t.Error(f)
			}
			t.Logf("%s", p)
		}
	}
}

func TestRendered_TheSamePageCopyBlocksRenderAtBothWidths(t *testing.T) {
	b := proseBrowser(t)
	narrow := renderProse(t, b, proseOpts{width: 360, height: 800}).prose
	wide := renderProse(t, b, proseOpts{width: 1280, height: 1024}).prose
	for _, f := range gradeSameBlocksAtBothWidths(narrow, wide) {
		t.Error(f)
	}
	if len(wide.Blocks) == 0 {
		t.Fatal("the reading found no blocks of page copy at all, so the two-width comparison is vacuous")
	}
}

// --- AC5, AC6: redundancy against the headings, the columns and itself ----------

func TestRendered_NoBlockRepeatsItsHeadingItsColumnsOrAnotherBlock(t *testing.T) {
	b := proseBrowser(t)
	fn := loadFunctionWords(t)
	for _, o := range []proseOpts{
		{},
		{snapshot: emptySnapshot()},
		{snapshot: mixedSnapshot()},
	} {
		p := renderProse(t, b, o).prose
		for _, f := range gradeHeadingOverlap(p, fn) {
			t.Error(f)
		}
		for _, f := range gradeRepeatedRuns(p, fn) {
			t.Error(f)
		}
	}
}

// --- AC4: the accessibility tree ------------------------------------------------

// axNameCeiling is AC4's ceiling, in words, on an accessible name or description.
const axNameCeiling = 8

// axTextRoles are the tree nodes that ARE the rendered text rather than a name for
// something: a StaticText node's accessible name is its own character data, and an
// InlineTextBox is a fragment of one. Capping those at eight words would be capping the
// page's data - a 300-character media path is one StaticText node - which is the opposite
// of what this criterion is for, and AC1 and AC2 already govern the visible text they
// carry. Their names are still read for the second limb below.
var axTextRoles = map[string]bool{"StaticText": true, "InlineTextBox": true, "text": true}

// gradeAccessibleNames is AC4. It reads the tree the ENGINE computed, which is the only
// place an accessible name exists at all.
//
// The ceiling is applied to every accessible DESCRIPTION, and to every accessible NAME
// that is not simply the rendered text under another heading - the name of a text node,
// or a name the engine says it computed from the element's own contents. A name from an
// aria-label, a title, a placeholder or a related element is exactly the copy this
// criterion exists to catch: the reader who meets it is at a screen reader or a hover,
// and nowhere else, so a paragraph moved into one would be a paragraph re-hidden rather
// than removed.
//
// The second limb applies to EVERY name and description whatever its source: none of
// them may carry a sentence the removal record says was taken off the visible surface.
func gradeAccessibleNameLength(nodes []proseAXNode, removed []string) []string {
	var out []string
	long := func(kind, s string) {
		if n := len(strings.Fields(s)); n > axNameCeiling {
			out = append(out, fmt.Sprintf("the accessibility tree exposes an accessible %s of %d words, want at most %d: %q",
				kind, n, axNameCeiling, s))
		}
	}
	carries := func(kind, s string) {
		flat := normalisedSentence(s)
		if flat == "" {
			return
		}
		for _, r := range removed {
			if strings.Contains(flat, normalisedSentence(r)) {
				out = append(out, fmt.Sprintf("the accessible %s %q carries a sentence the removal record says was taken off the visible surface: %q",
					kind, s, r))
			}
		}
	}
	for _, n := range nodes {
		if n.Ignored {
			continue
		}
		isText := axTextRoles[n.Role.text()]
		if name := n.Name.text(); name != "" {
			if !isText && !n.Name.fromContentsOnly() {
				long("name of a "+n.Role.text(), name)
			}
			carries("name", name)
		}
		if desc := n.Description.text(); desc != "" {
			long("description of a "+n.Role.text(), desc)
			carries("description", desc)
		}
	}
	return out
}

func TestRendered_NoAccessibleNameOrDescriptionHidesTheCopyThePageLost(t *testing.T) {
	b := proseBrowser(t)
	removed := removedSentences(t)
	if len(removed) == 0 {
		t.Fatal("the removal record names no removed sentence, so this grader cannot fail")
	}
	v := renderProse(t, b, proseOpts{})
	if len(v.ax) == 0 {
		t.Fatal("the engine returned an empty accessibility tree; nothing was graded")
	}
	// The tree must actually carry names the ceiling APPLIES to, or the assertions below
	// are vacuous: a run in which every named node happened to be a text node would have
	// graded nothing at all and reported a pass for it.
	named, capped := 0, 0
	for _, n := range v.ax {
		if n.Ignored || n.Name.text() == "" {
			continue
		}
		named++
		if !axTextRoles[n.Role.text()] && !n.Name.fromContentsOnly() {
			capped++
		}
	}
	if named < 10 {
		t.Fatalf("the accessibility tree carries only %d named nodes; the reading is not of a rendered dashboard", named)
	}
	if capped < 4 {
		t.Fatalf("only %d accessible names are subject to the ceiling; the page's own controls should put at least the token field, the filter and the two regions there", capped)
	}
	for _, f := range gradeAccessibleNameLength(v.ax, removed) {
		t.Error(f)
	}
	t.Logf("graded %d accessibility-tree nodes: %d named, %d of those subject to the %d-word ceiling",
		len(v.ax), named, capped, axNameCeiling)
}

// --- the shipped words and the vocabulary that writes them ----------------------

// gradeShippedLoadingWords holds the page's TWO writers of the same words in step. The
// shell ships each view already in its loading state, with the words in the markup, so a
// view says what it is doing before a byte of script has run; the modules write the same
// words from VIEW_STATES whenever they set a state afterwards. Two writers of one string
// is how one of them quietly stops being edited, and nothing else on this page compares
// them - the loading element the shell ships is never replaced by an identical one, so a
// drift between the two would show up only as a view that changed its words the first
// time a snapshot arrived.
//
// It is read off the RUNNING document, not the source: the vocabulary object comes back
// from the page's own execution context and the shipped text from what the engine laid
// out, so a page whose script never ran fails here rather than passing on its markup.
func gradeShippedLoadingWords(p proseReading) []string {
	if len(p.Vocabulary) == 0 {
		return []string{"the rendered page exposes no view-state vocabulary at all, so nothing could be compared"}
	}
	var out []string
	for _, v := range p.Views {
		if v.State != "loading" {
			out = append(out, fmt.Sprintf("the %s view is in the %q state before any snapshot arrived, want loading", v.View, v.State))
			continue
		}
		want := p.Vocabulary[v.View]["loading"]
		if want == "" {
			out = append(out, fmt.Sprintf("the view-state vocabulary has no loading wording for the %s view", v.View))
			continue
		}
		if v.Text != want {
			out = append(out, fmt.Sprintf("the shell ships the %s view reading %q while the vocabulary the modules write from says %q; two writers of one string have drifted",
				v.View, v.Text, want))
		}
	}
	return out
}

func TestRendered_TheShellShipsTheWordsTheViewVocabularyHolds(t *testing.T) {
	b := proseBrowser(t)
	p := renderProse(t, b, proseOpts{noSnapshot: true}).prose
	if len(p.Views) != len(dataViews) {
		t.Fatalf("the rendered page carries %d views, want %d", len(p.Views), len(dataViews))
	}
	for _, f := range gradeShippedLoadingWords(p) {
		t.Error(f)
	}
	// And it BITES: the shell says something the vocabulary does not (AC17).
	drifted := renderProse(t, b, proseOpts{
		noSnapshot: true,
		mutate:     injectInto(t, `<td class="empty" colspan="5">Loading the work in hand.</td>`, `<td class="empty" colspan="5">Loading.</td>`),
	}).prose
	if probs := gradeShippedLoadingWords(drifted); probs == nil {
		t.Error("the check passed a shell whose shipped words are not the ones the vocabulary holds")
	}
}

// --- AC15: one documentation link per section -----------------------------------

// docLinkProblems is AC15: every section of the dashboard renders exactly one link into
// this repository's own documentation, and it is a link a reader can see.
func docLinkProblems(p proseReading) []string {
	var out []string
	if len(p.Sections) == 0 {
		return []string{"the rendered document has no sections at all"}
	}
	for _, s := range p.Sections {
		var docs []renderedLink
		for _, l := range s.Links {
			if isDocumentationLink(l.Raw) {
				docs = append(docs, l)
			}
		}
		switch {
		case len(docs) == 0:
			out = append(out, fmt.Sprintf("the section %q (%s) renders no link to this repository's documentation", s.Heading, s.ID))
		case len(docs) > 1:
			out = append(out, fmt.Sprintf("the section %q (%s) renders %d documentation links, want exactly one: %v", s.Heading, s.ID, len(docs), docs))
		default:
			if !docs[0].Shown {
				out = append(out, fmt.Sprintf("the section %q renders a documentation link a reader cannot see: %+v", s.Heading, docs[0]))
			}
			if docs[0].Name == "" {
				out = append(out, fmt.Sprintf("the section %q renders a documentation link with no name at all - it announces itself as \"link\" and nothing more: %+v", s.Heading, docs[0]))
			}
		}
	}
	return out
}

func TestRendered_EverySectionCarriesExactlyOneDocumentationLink(t *testing.T) {
	b := proseBrowser(t)
	p := renderProse(t, b, proseOpts{}).prose
	for _, f := range docLinkProblems(p) {
		t.Error(f)
	}
	// The links the browser RESOLVED must be the ones the committed-document gate
	// resolves against this repository's files, so the two halves of AC15/AC16 cannot
	// drift apart.
	for _, s := range p.Sections {
		for _, l := range s.Links {
			if !isDocumentationLink(l.Raw) {
				continue
			}
			if !strings.HasPrefix(l.Href, sourceoffer.Upstream) {
				t.Errorf("the section %q resolves its documentation link to %q, which is not under the source URL in effect", s.Heading, l.Href)
			}
			path, anchor := splitDocRef(l.Raw)
			if err := documentationTargetProblem(path, anchor); err != nil {
				t.Errorf("the rendered documentation link of section %q does not resolve into this repository: %v", s.Heading, err)
			}
		}
	}
}

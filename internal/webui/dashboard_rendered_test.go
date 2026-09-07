package webui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// The RENDERED graders for the dashboard itself (WEBUI-10).
//
// Every criterion decided here concerns what the page SHOWS, so every one of them is
// decided by loading the SERVED document in a real browser engine, pushing it a real SSE
// snapshot, and reading the rendered result: computed style after the whole cascade, real
// layout geometry, the text innerText says a reader can see, and a hit test at the
// subject's own centre point. None of it is decided by matching HTML or CSS source text.
// That is the operator's ruling of 2026-09-06 and the reason S0035 was killed: a text
// grader cannot decide what a rule applies to, what wins the cascade, or what is SHOWN
// rather than merely built, and hardening one only closes the hole it was shown.
//
// The harness is the one rendered_test.go already provides - chromium(), serveDocument,
// renderAndRead, the probe page and its own deadline. Two of its rules are load-bearing
// and are not relaxed here: no --dump-dom and no --virtual-time-budget, each of which
// cost this repository a CI run.

// dashProbeJS is the measuring script. It runs in the PARENT page and reaches into the
// same-origin iframe holding the real served document.
//
// MODE decides when a measurement is meaningful, because the dashboard fills its tables
// from an SSE snapshot that lands after load: "live" waits for the page to report a
// connected stream, "down" waits for it to report a broken one with rows still on screen.
// FILTER, when set, is typed into the page's own filter control before the reading is
// taken, so what is measured is what the page did with a real input event.
const dashProbeJS = `
const MODE = "%MODE%";
const FILTER = "%FILTER%";

function connText(doc) { const c = doc.getElementById("conn"); return c ? c.textContent.trim() : ""; }

// A measurement is meaningful once the page has RENDERED a snapshot, which is a different
// moment from the stream opening: the page reports "live" on connect, and the snapshot
// that fills the tables arrives after it. The screen-reader summary is written at the end
// of every render and nowhere else, so a non-empty one is the page's own record that a
// whole snapshot went through it.
function rendered(doc) {
  const sr = doc.getElementById("sr-status");
  return !!sr && sr.textContent.trim() !== "";
}

function isReady(doc) {
  const t = connText(doc);
  if (MODE === "down") return t.indexOf("reconnecting") === 0 && rendered(doc);
  return t === "live" && rendered(doc);
}

function textOf(el) { return el ? el.textContent.trim() : ""; }
function cellText(tr, sel) { const td = tr.querySelector(sel); return td ? td.textContent.trim() : ""; }

// shownRecord is the whole of "did a reader see this": the computed style of the element
// and of every ancestor after the real cascade, its real layout box, whether it has an
// offsetParent at all, and a hit test at its own centre - which is the only way to catch
// something painted on top of it.
function shownRecord(el, doc, win) {
  if (!el) return { present: false, chain: [] };
  const chain = [];
  for (let n = el; n; n = n.parentElement) {
    const cs = win.getComputedStyle(n);
    chain.push({ tag: n.tagName.toLowerCase(), display: cs.display, visibility: cs.visibility,
                 opacity: cs.opacity, hidden: n.hasAttribute("hidden") });
  }
  try { el.scrollIntoView({ block: "center" }); } catch (e) { el.scrollIntoView(); }
  const r = el.getBoundingClientRect();
  const hit = doc.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
  return {
    present: true,
    width: r.width, height: r.height,
    offsetParent: el.offsetParent !== null,
    hit: !!hit && (hit === el || el.contains(hit)),
    chain: chain
  };
}

function rowsOf(doc, win, id) {
  const body = doc.getElementById(id);
  if (!body) return [];
  return Array.prototype.map.call(body.children, function (tr) {
    return {
      empty: tr.dataset.empty === "1",
      path: cellText(tr, "td.path"),
      status: cellText(tr, "td.st"),
      elapsed: cellText(tr, "td.elapsed"),
      progress: cellText(tr, "td.prog"),
      worker: cellText(tr, "td.worker"),
      size: cellText(tr, "td.size"),
      vmaf: cellText(tr, "td.vmaf"),
      enc: cellText(tr, "td.enc"),
      dur: cellText(tr, "td.dur"),
      upd: cellText(tr, "td.upd"),
      text: tr.textContent.trim(),
      shown: shownRecord(tr, doc, win)
    };
  });
}

function aggsOf(doc, win) {
  const host = doc.getElementById("aggregates");
  if (!host) return [];
  return Array.prototype.map.call(host.children, function (el) {
    return {
      title: textOf(el.querySelector(".agg-k")),
      value: textOf(el.querySelector(".agg-v")),
      coverage: textOf(el.querySelector(".agg-cov")),
      excluded: textOf(el.querySelector(".agg-ex")),
      out: el.classList.contains("out"),
      shown: shownRecord(el, doc, win)
    };
  });
}

// Everything on the page that names a URL the browser would FETCH. An <a href> is
// deliberately NOT in this set: a hyperlink is a navigation target the reader chooses,
// not a resource the page loads, and the AGPL section 13 source offer this page must
// carry IS such a link.
const SUBRESOURCE = [
  ["script","src"],["link","href"],["img","src"],["img","srcset"],["iframe","src"],
  ["frame","src"],["embed","src"],["object","data"],["source","src"],["source","srcset"],
  ["video","src"],["video","poster"],["audio","src"],["track","src"],["input","src"],
  ["form","action"],["image","href"],["use","href"],["applet","code"]
];

function offOriginRefs(doc, win) {
  const out = [];
  const origin = win.location.origin;
  const base = doc.baseURI;
  function check(where, raw) {
    if (!raw) return;
    let u;
    try { u = new win.URL(raw, base); } catch (e) { out.push(where + " -> unresolvable " + raw); return; }
    if (u.origin !== origin) out.push(where + " -> " + u.href);
  }
  for (const pair of SUBRESOURCE) {
    const sel = pair[0] + "[" + pair[1] + "]";
    for (const el of doc.querySelectorAll(sel)) {
      const raw = el.getAttribute(pair[1]);
      if (pair[1] === "srcset") {
        for (const piece of String(raw).split(",")) check(sel, piece.trim().split(/\s+/)[0]);
      } else {
        check(sel, raw);
      }
    }
  }
  // Every url() the cascade actually applied, and every one any rule in the page's own
  // stylesheets names. A background image, a font, a mask or a cursor is a fetch too.
  const re = /url\(\s*(['"]?)([^'")]*)\1\s*\)/g;
  function scan(text, where) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(text)) !== null) check("style " + where, m[2]);
  }
  for (const sheet of doc.styleSheets) {
    if (sheet.href) check("external stylesheet", sheet.href);
    let rules = null;
    try { rules = sheet.cssRules; } catch (e) { out.push("unreadable stylesheet " + String(sheet.href)); continue; }
    for (const r of rules) scan(r.cssText, "rule");
  }
  const props = ["backgroundImage","borderImageSource","listStyleImage","maskImage","cursor","content"];
  for (const el of doc.querySelectorAll("*")) {
    for (const pseudo of [null, "::before", "::after"]) {
      const cs = win.getComputedStyle(el, pseudo);
      for (const p of props) {
        const v = cs[p];
        if (v && v.indexOf("url(") >= 0) scan(v, el.tagName.toLowerCase() + (pseudo || "") + " " + p);
      }
    }
  }
  // And every resource the browser ACTUALLY fetched while rendering, which catches
  // anything the enumeration above forgot.
  for (const e of win.performance.getEntriesByType("resource")) check("network", e.name);
  return out;
}

function verdict(doc, win) {
  if (!isReady(doc)) return { ready: false, connText: connText(doc), connClass: "" };
  if (FILTER !== "") {
    const f = doc.getElementById("filter");
    if (f) { f.value = FILTER; f.dispatchEvent(new win.Event("input", { bubbles: true })); }
  }
  const conn = doc.getElementById("conn");
  const chips = doc.getElementById("chips");
  return {
    ready: true,
    origin: win.location.origin,
    connText: connText(doc),
    connClass: conn ? conn.className : "",
    badges: { paused: textOf(doc.getElementById("b-paused")), scan: textOf(doc.getElementById("b-scan")) },
    reclaimedLifetime: textOf(doc.getElementById("reclaimed-lifetime")),
    reclaimedSession: textOf(doc.getElementById("reclaimed-session")),
    srStatus: textOf(doc.getElementById("sr-status")),
    chips: chips ? Array.prototype.map.call(chips.children, function (c) {
      return { n: textOf(c.querySelector(".n")), k: textOf(c.querySelector(".k")) };
    }) : [],
    queue: rowsOf(doc, win, "queue"),
    history: rowsOf(doc, win, "history"),
    aggregates: aggsOf(doc, win),
    queueCap: { text: textOf(doc.getElementById("queue-cap")), shown: shownRecord(doc.getElementById("queue-cap"), doc, win) },
    histCap: { text: textOf(doc.getElementById("hist-cap")), shown: shownRecord(doc.getElementById("hist-cap"), doc, win) },
    offOrigin: offOriginRefs(doc, win),
    elementCount: doc.getElementsByTagName("*").length,
    // What a reader can SEE, as the engine reports it: innerText excludes what the
    // cascade hid, which textContent would happily hand back anyway.
    bodyText: doc.body ? doc.body.innerText : ""
  };
}
`

// --- the verdict, decoded ------------------------------------------------------

type styleNode struct {
	Tag        string `json:"tag"`
	Display    string `json:"display"`
	Visibility string `json:"visibility"`
	Opacity    string `json:"opacity"`
	Hidden     bool   `json:"hidden"`
}

type shownRec struct {
	Present      bool        `json:"present"`
	Width        float64     `json:"width"`
	Height       float64     `json:"height"`
	OffsetParent bool        `json:"offsetParent"`
	Hit          bool        `json:"hit"`
	Chain        []styleNode `json:"chain"`
}

type dashRow struct {
	Empty    bool     `json:"empty"`
	Path     string   `json:"path"`
	Status   string   `json:"status"`
	Elapsed  string   `json:"elapsed"`
	Progress string   `json:"progress"`
	Worker   string   `json:"worker"`
	Size     string   `json:"size"`
	Vmaf     string   `json:"vmaf"`
	Enc      string   `json:"enc"`
	Dur      string   `json:"dur"`
	Upd      string   `json:"upd"`
	Text     string   `json:"text"`
	Shown    shownRec `json:"shown"`
}

type dashAgg struct {
	Title    string   `json:"title"`
	Value    string   `json:"value"`
	Coverage string   `json:"coverage"`
	Excluded string   `json:"excluded"`
	Out      bool     `json:"out"`
	Shown    shownRec `json:"shown"`
}

type dashCap struct {
	Text  string   `json:"text"`
	Shown shownRec `json:"shown"`
}

type dashVerdict struct {
	Ready     bool   `json:"ready"`
	Error     string `json:"error"`
	Origin    string `json:"origin"`
	ConnText  string `json:"connText"`
	ConnClass string `json:"connClass"`
	Badges    struct {
		Paused string `json:"paused"`
		Scan   string `json:"scan"`
	} `json:"badges"`
	ReclaimedLifetime string `json:"reclaimedLifetime"`
	ReclaimedSession  string `json:"reclaimedSession"`
	SRStatus          string `json:"srStatus"`
	Chips             []struct {
		N string `json:"n"`
		K string `json:"k"`
	} `json:"chips"`
	Queue        []dashRow `json:"queue"`
	History      []dashRow `json:"history"`
	Aggregates   []dashAgg `json:"aggregates"`
	QueueCap     dashCap   `json:"queueCap"`
	HistCap      dashCap   `json:"histCap"`
	OffOrigin    []string  `json:"offOrigin"`
	ElementCount int       `json:"elementCount"`
	BodyText     string    `json:"bodyText"`
}

// shownProblems reports every way the browser says this subject did NOT reach a reader.
// It is the dashboard's counterpart of hiddenProblems, and B15 proves it BITES against
// each hiding mutation a rendered assertion can be defeated by.
func shownProblems(what string, r shownRec) []string {
	if !r.Present {
		return []string{what + ": the browser found no such element in the rendered document"}
	}
	var out []string
	if r.Width <= 0 || r.Height <= 0 {
		out = append(out, fmt.Sprintf("%s: has no rendered box (%.1fx%.1f)", what, r.Width, r.Height))
	}
	if !r.OffsetParent {
		out = append(out, what+": has no offsetParent, so it or an ancestor is display:none")
	}
	if !r.Hit {
		out = append(out, what+": a hit test at its own centre does not reach it")
	}
	for _, n := range r.Chain {
		switch {
		case n.Display == "none":
			out = append(out, what+": the computed display of the "+n.Tag+" ancestor is none")
		case n.Visibility == "hidden" || n.Visibility == "collapse":
			out = append(out, what+": the computed visibility of the "+n.Tag+" ancestor is "+n.Visibility)
		case n.Opacity == "0":
			out = append(out, what+": the computed opacity of the "+n.Tag+" ancestor is 0")
		case n.Hidden:
			out = append(out, what+": the "+n.Tag+" ancestor carries a hidden attribute")
		}
	}
	return out
}

// isShown is shownProblems inverted, for the rows a filter is supposed to hide.
func isShown(r shownRec) bool { return len(shownProblems("x", r)) == 0 }

// --- driving the page ----------------------------------------------------------

type dashOpts struct {
	snapshot    []byte
	mutate      func([]byte) []byte
	mode        string // "live" (default) or "down"
	filter      string
	streamFails bool
	wait        time.Duration
}

// renderDashboard serves the real document, pushes it one real SSE snapshot, and returns
// what the browser made of it together with everything the browser itself printed.
func renderDashboard(t *testing.T, bin string, o dashOpts) (dashVerdict, string) {
	t.Helper()
	mode := o.mode
	if mode == "" {
		mode = "live"
	}
	js := strings.Replace(dashProbeJS, "%MODE%", mode, 1)
	js = strings.Replace(js, "%FILTER%", o.filter, 1)
	wait := o.wait
	if wait <= 0 {
		wait = 15 * time.Second
	}
	ps := serveDocumentWith(t, serveOpts{
		url:         sourceoffer.Upstream,
		mutate:      o.mutate,
		probe:       js,
		wait:        wait,
		snapshot:    o.snapshot,
		streamFails: o.streamFails,
	})
	raw, log, err := runProbe(bin, ps, verdictDeadline, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var v dashVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("verdict is not JSON (%v): %s\nbrowser output:\n%s", err, raw, log)
	}
	if v.Error != "" {
		t.Fatalf("the probe failed inside the browser: %s\nbrowser output:\n%s", v.Error, log)
	}
	return v, log
}

// mustRender is renderDashboard for a grader that requires the page to have rendered.
func mustRender(t *testing.T, bin string, o dashOpts) (dashVerdict, string) {
	t.Helper()
	v, log := renderDashboard(t, bin, o)
	if !v.Ready {
		t.Fatalf("the page never reached the state the grader measures (mode=%q, connection state %q)\nbrowser output:\n%s",
			o.mode, v.ConnText, log)
	}
	return v, log
}

// --- the snapshots the page is measured against --------------------------------

// snapNow is the server clock the fixture snapshots are stamped with. The page derives
// every elapsed figure from it, so a row's age is (snapNow - updated_at) plus whatever
// real time passes before the measurement is taken.
const snapNow = 1_700_000_000

// fixtureSnapshot is B9's own case: a pending job, a running encode and completed jobs,
// with the summary reporting more of each than the capped tables were handed.
func fixtureSnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":5,"probing":0,"encoding":1,"verifying":1,"done":9,"skipped":3,"failed":2},
  "queue": [
    {"path":"/media/films/alpha.mkv","status":"pending","worker":"","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null},
    {"path":"/media/films/bravo.mkv","status":"encoding","worker":"w2","updated_at":%d,
     "progress_seconds":1200,"progress_duration_seconds":3600,"progress_fraction":0.3333},
    {"path":"/media/films/charlie.mkv","status":"verifying","worker":"w3","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null}
  ],
  "history": [
    {"path":"/media/films/delta.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":"cpu","vmaf_mean":98.24,"vmaf_min":91.53,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":4294967296,"output_bytes":1073741824,"encode_ms":5430000},
    {"path":"/media/films/echo.mkv","status":"done","worker":"w4","updated_at":%d,
     "encoder":"svtav1","vmaf_mean":96.10,"vmaf_min":88.40,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":2147483648,"output_bytes":1610612736,"encode_ms":900000}
  ],
  "bytes_reclaimed_session": 1073741824,
  "bytes_reclaimed_lifetime": 5368709120,
  "paused": false, "scanning": true, "now": %d,
  "aggregates": %s
}`, snapNow-3600, snapNow-90, snapNow-45, snapNow-7200, snapNow-9000, snapNow, healthyAggregates))
}

// healthyAggregates is every published figure, available, each stating the set it covers
// and the rows it had to leave out.
const healthyAggregates = `{
  "outcomes": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","window":"",
    "counted":14,"excluded":2,"buckets":[{"key":"done","count":9},{"key":"skipped","count":3},{"key":"failed","count":2}]},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":3,"excluded":1,"buckets":[{"key":"hardlinked","count":2},{"key":"low-bitrate","count":1}]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":9,"excluded":4,"min":0.21,"mean":0.38,"max":0.74},
  "encode_ms": {"available":true,"unavailable":"","covers":"every done row that recorded an encode duration","window":"",
    "counted":9,"excluded":3,"min":120000,"mean":900000,"max":5430000},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a pooled mean","window":"the last 90 days",
    "counted":8,"excluded":5,"min":95.1,"mean":97.4,"max":99.2},
  "vmaf_min": {"available":true,"unavailable":"","covers":"every done row that recorded a worst frame","window":"",
    "counted":8,"excluded":6,"min":81.2,"mean":90.6,"max":96.3}
}`

// brokenAggregates is B11's case: figures that could not be read at all, a figure over
// nothing, and a figure that reports itself available while carrying no numbers.
const brokenAggregates = `{
  "outcomes": {"available":false,"unavailable":"this figure could not be read from the ledger",
    "covers":"every terminal row in the ledger","window":"","counted":0,"excluded":0,"buckets":null},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":0,"excluded":7,"buckets":[]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":12,"excluded":3,"min":null,"mean":null,"max":null},
  "encode_ms": {"available":false,"unavailable":"this figure could not be read from the ledger",
    "covers":"every done row that recorded an encode duration","window":"","counted":0,"excluded":0,"min":null,"mean":null,"max":null},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a pooled mean","window":"",
    "counted":8,"excluded":5,"min":95.1,"mean":97.4,"max":99.2},
  "vmaf_min": {"available":false,"unavailable":"this figure could not be read from the ledger",
    "covers":"every done row that recorded a worst frame","window":"","counted":0,"excluded":0,"min":null,"mean":null,"max":null}
}`

func brokenAggregatesSnapshot() []byte {
	return []byte(strings.Replace(string(fixtureSnapshot()), healthyAggregates, brokenAggregates, 1))
}

// emptySnapshot is a ledger with nothing in it.
func emptySnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":0,"verifying":0,"done":0,"skipped":0,"failed":0},
  "queue": [], "history": [],
  "bytes_reclaimed_session": 0, "bytes_reclaimed_lifetime": 0,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": %s
}`, snapNow, healthyAggregates))
}

// mixedSnapshot carries a skipped row and a failed row beside a done one, and two queue
// rows whose paths differ, which is what the filter is measured against.
func mixedSnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":1,"probing":0,"encoding":1,"verifying":0,"done":1,"skipped":1,"failed":1},
  "queue": [
    {"path":"/media/films/alpha.mkv","status":"pending","worker":"","updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null},
    {"path":"/media/shows/bravo.mkv","status":"encoding","worker":"w2","updated_at":%d,
     "progress_seconds":600,"progress_duration_seconds":1200,"progress_fraction":0.5}
  ],
  "history": [
    {"path":"/media/films/delta.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":"cpu","vmaf_mean":98.24,"vmaf_min":91.53,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":4294967296,"output_bytes":1073741824,"encode_ms":5430000},
    {"path":"/media/shows/echo.mkv","status":"skipped","worker":"w4","updated_at":%d,"reason":"hardlinked",
     "vmaf_mean":null,"vmaf_min":null,"source_bytes":null,"output_bytes":null,"encode_ms":null},
    {"path":"/media/shows/foxtrot.mkv","status":"failed","worker":"w5","updated_at":%d,
     "reason":"vmaf worst frame 43.2 below the floor 60","vmaf_mean":null,"vmaf_min":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "bytes_reclaimed_session": 0, "bytes_reclaimed_lifetime": 3221225472,
  "paused": true, "scanning": false, "now": %d,
  "aggregates": %s
}`, snapNow-30, snapNow-60, snapNow-7200, snapNow-8000, snapNow-9000, snapNow, healthyAggregates))
}

// --- B9: the queue, the history, and the figures each row carries ---------------

func TestRendered_DashboardShowsQueueRowsHistoryRowsAndTheirFigures(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	// One queue row per pending or active job, each SHOWN.
	if len(v.Queue) != 3 {
		t.Fatalf("the rendered queue has %d rows, want one per pending/active job (3)\nrows: %+v\nbrowser output:\n%s",
			len(v.Queue), v.Queue, log)
	}
	wantQueue := []struct{ path, status, worker string }{
		{"/media/films/alpha.mkv", "pending", ""},
		{"/media/films/bravo.mkv", "encoding", "w2"},
		{"/media/films/charlie.mkv", "verifying", "w3"},
	}
	for i, w := range wantQueue {
		got := v.Queue[i]
		if probs := shownProblems("queue row "+w.path, got.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
		if got.Path != w.path {
			t.Errorf("queue row %d shows path %q, want %q", i, got.Path, w.path)
		}
		if got.Status != w.status {
			t.Errorf("queue row %d shows state %q, want %q", i, got.Status, w.status)
		}
		if got.Worker != w.worker {
			t.Errorf("queue row %d shows worker %q, want %q", i, got.Worker, w.worker)
		}
		if !strings.Contains(v.BodyText, w.path) {
			t.Errorf("the path %q is in the DOM but not in the text a reader can see", w.path)
		}
	}

	// The elapsed figure is DERIVED from each row's own wire timestamp against the
	// snapshot's own clock: three rows stamped 3600s, 90s and 45s before it read three
	// different ages, in that order, none of them counted in the page.
	ages := []int{
		elapsedSeconds(t, v.Queue[0].Elapsed),
		elapsedSeconds(t, v.Queue[1].Elapsed),
		elapsedSeconds(t, v.Queue[2].Elapsed),
	}
	for i, want := range []struct{ lo, hi int }{{3600, 3720}, {90, 150}, {45, 105}} {
		if ages[i] < want.lo || ages[i] > want.hi {
			t.Errorf("queue row %d (%s, stamped %ds before the snapshot) shows an age of %q (%ds), want between %ds and %ds",
				i, v.Queue[i].Status, []int{3600, 90, 45}[i], v.Queue[i].Elapsed, ages[i], want.lo, want.hi)
		}
	}
	if !(ages[0] > ages[1] && ages[1] > ages[2]) {
		t.Errorf("the three rows' ages are %v: each must come from its OWN wire timestamp, not one figure for the table", ages)
	}

	// A progress figure for the running encode, and for NO other row.
	if got := v.Queue[1].Progress; !strings.Contains(got, "33%") || !strings.Contains(got, "20m 0s of 1h 0m") {
		t.Errorf("the running encode shows progress %q, want the encoder's own figure and its position", got)
	}
	for _, i := range []int{0, 2} {
		if v.Queue[i].Progress != "" {
			t.Errorf("queue row %d (%s) shows a progress figure %q; only a running encode has one",
				i, v.Queue[i].Status, v.Queue[i].Progress)
		}
	}

	// One history row per completed job, each carrying its result, its size change and its
	// VMAF score, each SHOWN.
	if len(v.History) != 2 {
		t.Fatalf("the rendered history has %d rows, want one per completed job (2): %+v", len(v.History), v.History)
	}
	wantHist := []struct{ path, result, size, mean, worst, enc, dur string }{
		{"/media/films/delta.mkv", "done", "4.0 GB", "98.2", "91.5", "cpu", "1h 30m"},
		{"/media/films/echo.mkv", "done", "2.0 GB", "96.1", "88.4", "svtav1", "15m 0s"},
	}
	for i, w := range wantHist {
		got := v.History[i]
		if probs := shownProblems("history row "+w.path, got.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
		if got.Path != w.path {
			t.Errorf("history row %d shows path %q, want %q", i, got.Path, w.path)
		}
		if got.Status != w.result {
			t.Errorf("history row %d shows result %q, want %q", i, got.Status, w.result)
		}
		if !strings.Contains(got.Size, w.size) || !strings.Contains(got.Size, "smaller") {
			t.Errorf("history row %d shows size %q, want the change from %s and the percent reclaimed", i, got.Size, w.size)
		}
		if !strings.Contains(got.Vmaf, w.mean) || !strings.Contains(got.Vmaf, w.worst) {
			t.Errorf("history row %d shows VMAF %q, want the pooled mean %s and the worst frame %s", i, got.Vmaf, w.mean, w.worst)
		}
		if !strings.Contains(got.Vmaf, "luma-only") || !strings.Contains(got.Vmaf, "measured vs your source") {
			t.Errorf("history row %d shows a VMAF score without its viewing condition: %q", i, got.Vmaf)
		}
		if got.Enc != w.enc {
			t.Errorf("history row %d shows encoder %q, want %q", i, got.Enc, w.enc)
		}
		if got.Dur != w.dur {
			t.Errorf("history row %d shows encode time %q, want %q", i, got.Dur, w.dur)
		}
	}
	if got, want := v.History[0].Size, "75% smaller"; !strings.Contains(got, want) {
		t.Errorf("the size cell shows %q, want it to carry %q", got, want)
	}

	// The rest of the frame the same snapshot drives, all of it SHOWN.
	if v.Badges.Scan != "scanning" || v.Badges.Paused != "running" {
		t.Errorf("the badges show paused=%q scan=%q, want running/scanning", v.Badges.Paused, v.Badges.Scan)
	}
	if v.ReclaimedLifetime != "5.0 GB" {
		t.Errorf("the lifetime reclaimed figure reads %q, want 5.0 GB", v.ReclaimedLifetime)
	}
	// One chip per status, counted against the vocabulary THE PAGE ITSELF declares rather
	// than against a literal. The literal was 7 and FILESYSTEM-1 made it 9 (indeterminate
	// and applied-despite-error), which is the second time a number in a test had to be
	// chased; derived, it cannot go stale, and it still fails if a chip goes missing.
	if want := declaredStatusCount(t); len(v.Chips) != want {
		t.Errorf("the summary shows %d chips, want one per status the page declares (%d)", len(v.Chips), want)
	}
	// Both tables are capped by the API and the page must say so, visibly.
	for _, c := range []struct {
		what string
		cap  dashCap
		want string
	}{
		{"the queue cap notice", v.QueueCap, "Showing the most recent 3 of 7"},
		{"the history cap notice", v.HistCap, "Showing the most recent 2 of 14"},
	} {
		if !strings.Contains(c.cap.Text, c.want) {
			t.Errorf("%s reads %q, want it to carry %q", c.what, c.cap.Text, c.want)
		}
		if probs := shownProblems(c.what, c.cap.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
}

var elapsedRe = regexp.MustCompile(`^(?:(\d+)h)?\s*(?:(\d+)m)?\s*(?:(\d+)s)?$`)

func elapsedSeconds(t *testing.T, s string) int {
	t.Helper()
	m := elapsedRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		t.Fatalf("%q is not an elapsed span the page renders", s)
	}
	n := func(x string) int {
		if x == "" {
			return 0
		}
		v, _ := strconv.Atoi(x)
		return v
	}
	return n(m[1])*3600 + n(m[2])*60 + n(m[3])
}

// --- B10: each aggregate card states its set and its exclusions -----------------

func TestRendered_AggregateCardsStateTheirSetAndTheirExclusions(t *testing.T) {
	bin := chromium(t)
	v, _ := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	want := []struct {
		title    string
		covers   string
		excluded string
	}{
		{"Outcomes", "over every terminal row in the ledger", "2 rows excluded: no recorded value"},
		{"Skips by guard", "over every skipped row in the ledger", "1 row excluded: no recorded value"},
		{"Replacement size", "over every done row that recorded both sizes", "4 rows excluded: no recorded value"},
		{"Encode time", "over every done row that recorded an encode duration", "3 rows excluded: no recorded value"},
		{"VMAF pooled mean", "over every done row that recorded a pooled mean · window: the last 90 days", "5 rows excluded: no recorded value"},
		{"VMAF worst frame", "over every done row that recorded a worst frame", "6 rows excluded: no recorded value"},
	}
	if len(v.Aggregates) != len(want) {
		t.Fatalf("the page rendered %d aggregate cards, want %d: %+v", len(v.Aggregates), len(want), v.Aggregates)
	}
	for i, w := range want {
		got := v.Aggregates[i]
		if probs := shownProblems("aggregate card "+w.title, got.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
		if got.Title != w.title {
			t.Errorf("aggregate card %d is titled %q, want %q", i, got.Title, w.title)
		}
		if got.Coverage != w.covers {
			t.Errorf("card %q states its set as %q, want %q", w.title, got.Coverage, w.covers)
		}
		if got.Excluded != w.excluded {
			t.Errorf("card %q states its exclusions as %q, want %q", w.title, got.Excluded, w.excluded)
		}
		// Both must be TEXT A READER CAN SEE, not merely text in the DOM.
		for _, s := range []string{w.covers, w.excluded} {
			if !strings.Contains(v.BodyText, s) {
				t.Errorf("card %q: %q is in the DOM but not in the text the browser says is visible", w.title, s)
			}
		}
	}
	// And the figures themselves, so a card cannot pass by stating a set and no number.
	if got := v.Aggregates[2].Value; !strings.Contains(got, "38%") || !strings.Contains(got, "range 21% to 74% across 9 files") {
		t.Errorf("the Replacement size card shows %q, want the mean and its range across the counted files", got)
	}
	if got := v.Aggregates[0].Value; !strings.Contains(got, "done") || !strings.Contains(got, "9") {
		t.Errorf("the Outcomes card shows %q, want its per-outcome counts", got)
	}
}

// --- B11: an unreadable figure is shown AS unreadable, and costs nothing else ----

func TestRendered_AnAbsentAggregateIsShownAsSuchAndTheRestOfThePageStillRenders(t *testing.T) {
	bin := chromium(t)
	v, _ := mustRender(t, bin, dashOpts{snapshot: brokenAggregatesSnapshot()})

	if len(v.Aggregates) != 6 {
		t.Fatalf("the page rendered %d aggregate cards, want all 6 even when figures are missing: %+v", len(v.Aggregates), v.Aggregates)
	}
	// EVERY card is still on the page and still shown - a card that vanished would leave
	// the page looking complete with a number missing from it.
	for _, a := range v.Aggregates {
		if probs := shownProblems("aggregate card "+a.Title, a.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
	unreadable := map[string]bool{"Outcomes": true, "Encode time": true, "VMAF worst frame": true}
	for _, a := range v.Aggregates {
		switch {
		case unreadable[a.Title]:
			if !a.Out || !strings.Contains(a.Value, "unavailable") {
				t.Errorf("the unreadable card %q shows %q, want it drawn as unavailable", a.Title, a.Value)
			}
			if !strings.Contains(a.Excluded, "could not be read") {
				t.Errorf("the unreadable card %q does not say why: %q", a.Title, a.Excluded)
			}
		case a.Title == "Skips by guard":
			// Available, but no row contributed a value.
			if a.Value != "not recorded" {
				t.Errorf("a figure no row contributed to shows %q, want %q", a.Value, "not recorded")
			}
		case a.Title == "Replacement size":
			// Available and counted, but every figure came over as null.
			if !strings.Contains(a.Value, "not recorded") {
				t.Errorf("a card whose figures are null shows %q, want them as not recorded", a.Value)
			}
		case a.Title == "VMAF pooled mean":
			if !strings.Contains(a.Value, "97.4") {
				t.Errorf("the one readable figure did not render: %q", a.Value)
			}
		}
		// The specific lie this repository exists to refuse: a fact nobody measured
		// rendered as a zero.
		for _, lie := range []string{"0%", "0.0", "0 ms", "0 B"} {
			if unreadable[a.Title] || a.Title == "Skips by guard" || a.Title == "Replacement size" {
				if strings.Contains(a.Value, lie) {
					t.Errorf("card %q renders an unrecorded figure as %q: %q", a.Title, lie, a.Value)
				}
			}
		}
	}

	// And every other card and BOTH tables are still rendered.
	if len(v.Queue) != 3 {
		t.Errorf("a missing aggregate cost the queue its rows: %d rendered", len(v.Queue))
	}
	if len(v.History) != 2 {
		t.Errorf("a missing aggregate cost the history its rows: %d rendered", len(v.History))
	}
	for _, r := range append(append([]dashRow{}, v.Queue...), v.History...) {
		if probs := shownProblems("row "+r.Path, r.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
}

// --- B12: an empty ledger shows its empty states --------------------------------

func TestRendered_AnEmptySnapshotShowsBothEmptyStateRows(t *testing.T) {
	bin := chromium(t)
	v, _ := mustRender(t, bin, dashOpts{snapshot: emptySnapshot()})

	for _, c := range []struct {
		what string
		rows []dashRow
		want string
	}{
		{"queue", v.Queue, "Nothing queued."},
		{"history", v.History, "No history yet."},
	} {
		if len(c.rows) != 1 {
			t.Fatalf("the rendered %s has %d rows for an empty ledger, want exactly its empty-state row: %+v",
				c.what, len(c.rows), c.rows)
		}
		r := c.rows[0]
		if !r.Empty {
			t.Errorf("the %s's only row is not the empty-state row: %+v", c.what, r)
		}
		if strings.TrimSpace(r.Text) != c.want {
			t.Errorf("the %s empty state reads %q, want %q", c.what, r.Text, c.want)
		}
		if probs := shownProblems("the "+c.what+" empty-state row", r.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
		if !strings.Contains(v.BodyText, c.want) {
			t.Errorf("the %s empty state is in the DOM but not in the text a reader can see", c.what)
		}
	}
	// An empty ledger caps nothing, so neither notice is shown.
	for _, c := range []struct {
		what string
		cap  dashCap
	}{{"queue", v.QueueCap}, {"history", v.HistCap}} {
		if isShown(c.cap.Shown) && c.cap.Text != "" {
			t.Errorf("the %s cap notice is shown (%q) for a ledger with nothing in it", c.what, c.cap.Text)
		}
	}
}

// --- B13: a broken stream leaves the connection down and the rows on screen ------

func TestRendered_AFailedEventStreamLeavesTheConnectionDownAndKeepsTheRows(t *testing.T) {
	bin := chromium(t)
	// The stream delivers one snapshot and then drops, and every reconnection - the page's
	// only API call - is answered 500. Both halves of B13's antecedent hold at once.
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot(), streamFails: true, mode: "down", wait: 30 * time.Second})

	if v.ConnText == "live" || v.ConnText == "" {
		t.Errorf("the page reports its connection as %q after the stream failed", v.ConnText)
	}
	if !strings.Contains(v.ConnClass, "down") {
		t.Errorf("the connection indicator's class is %q, want the page's not-connected state\nbrowser output:\n%s",
			v.ConnClass, log)
	}
	// It keeps the rows it last rendered ON SCREEN, not merely in the DOM.
	if len(v.Queue) != 3 || len(v.History) != 2 {
		t.Fatalf("the page dropped the rows it had rendered when the stream failed: %d queue, %d history",
			len(v.Queue), len(v.History))
	}
	for _, r := range append(append([]dashRow{}, v.Queue...), v.History...) {
		if probs := shownProblems("row "+r.Path+" after the stream failed", r.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
	if !strings.Contains(v.BodyText, "/media/films/delta.mkv") {
		t.Error("a row the page had rendered is no longer text a reader can see after the stream failed")
	}
	// And the aggregates it had are still there too.
	if len(v.Aggregates) != 6 {
		t.Errorf("the page dropped %d aggregate cards when the stream failed", 6-len(v.Aggregates))
	}
}

// --- B14: the filter, judged by what the browser reports as rendered -------------

func TestRendered_FilterLeavesOnlyMatchingRowsVisible(t *testing.T) {
	bin := chromium(t)
	// Before: every row is on screen.
	before, _ := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot()})
	if len(before.Queue) != 2 || len(before.History) != 3 {
		t.Fatalf("the fixture did not render: %d queue rows, %d history rows", len(before.Queue), len(before.History))
	}
	for _, r := range append(append([]dashRow{}, before.Queue...), before.History...) {
		if probs := shownProblems("row "+r.Path+" before filtering", r.Shown); probs != nil {
			t.Fatalf("%v", probs)
		}
	}

	// After: the same page, with "shows" typed into its own filter control.
	after, _ := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot(), filter: "shows"})
	if len(after.Queue) != 2 || len(after.History) != 3 {
		t.Fatalf("filtering removed rows from the DOM; the criterion is about what is RENDERED: %d queue, %d history",
			len(after.Queue), len(after.History))
	}
	for _, r := range append(append([]dashRow{}, after.Queue...), after.History...) {
		want := strings.Contains(r.Path, "/media/shows/")
		got := isShown(r.Shown)
		if got != want {
			t.Errorf("with the filter %q the row %q is shown=%v, want %v (problems: %v)",
				"shows", r.Path, got, want, shownProblems(r.Path, r.Shown))
		}
		if want && !strings.Contains(after.BodyText, r.Path) {
			t.Errorf("the matching row %q is not in the text a reader can see", r.Path)
		}
		if !want && strings.Contains(after.BodyText, r.Path) {
			t.Errorf("the non-matching row %q is still in the text a reader can see", r.Path)
		}
	}

	// A filter that matches nothing hides every row, and one that matches everything hides
	// none - so the grader cannot be satisfied by a filter that does nothing either way.
	none, _ := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot(), filter: "no-such-path"})
	for _, r := range append(append([]dashRow{}, none.Queue...), none.History...) {
		if isShown(r.Shown) {
			t.Errorf("the row %q is still shown under a filter that matches no path", r.Path)
		}
	}
	all, _ := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot(), filter: "/media/"})
	for _, r := range append(append([]dashRow{}, all.Queue...), all.History...) {
		if probs := shownProblems("row "+r.Path+" under a filter that matches every path", r.Shown); probs != nil {
			t.Errorf("%v", probs)
		}
	}
}

// A skipped row names the guard that held the file back and a failed one shows why, both
// as text a reader can see. (Not a criterion of its own; the rows exist in the fixture
// the filter is measured against, and grading them here costs one browser run less.)
func TestRendered_ASkippedRowNamesItsGuardAndAFailedRowItsReason(t *testing.T) {
	bin := chromium(t)
	v, _ := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot()})

	byPath := map[string]dashRow{}
	for _, r := range v.History {
		byPath[r.Path] = r
	}
	skipped := byPath["/media/shows/echo.mkv"]
	if !strings.Contains(skipped.Status, "skipped") || !strings.Contains(skipped.Status, "hardlinked (would break a seed)") {
		t.Errorf("the skipped row shows %q, want the guard named in words", skipped.Status)
	}
	failed := byPath["/media/shows/foxtrot.mkv"]
	if !strings.Contains(failed.Status, "failed") || !strings.Contains(failed.Status, "below the floor 60") {
		t.Errorf("the failed row shows %q, want the failure reason verbatim", failed.Status)
	}
	// Neither invents a size or a score it never had.
	for _, r := range []dashRow{skipped, failed} {
		if r.Size != "" || r.Vmaf != "" {
			t.Errorf("the %q row shows size %q and VMAF %q for an encode that never happened", r.Path, r.Size, r.Vmaf)
		}
	}
	if !strings.Contains(v.BodyText, "hardlinked (would break a seed)") {
		t.Error("the guard label is in the DOM but not in the text a reader can see")
	}
}

// --- B15 / A2: every rendered grader FAILS when its subject is hidden ------------

// hidingMutations is one counterexample per way a subject can be in the served bytes and
// still never reach a reader. A grader that passes any of these is a grader that cannot
// fail, and this repository has already lost a whole spec to exactly that.
func hidingMutations() map[string]func([]byte) []byte {
	css := func(rule string) func([]byte) []byte {
		return func(b []byte) []byte {
			return []byte(strings.Replace(string(b), "</style>", rule+"\n</style>", 1))
		}
	}
	return map[string]func([]byte) []byte{
		"display:none on the rows":                   css("#queue tr, #history tr { display:none; }"),
		"display:none on the aggregate cards":        css(".agg { display:none; }"),
		"visibility:hidden on an ancestor":           css(".tablewrap, .aggs { visibility:hidden; }"),
		"opacity:0 on an ancestor":                   css("main { opacity:0; }"),
		"an ancestor collapsed":                      css("main { display:none; }"),
		"a zero-size box":                            css("#queue tr, #history tr, .agg { position:absolute; width:0; height:0; overflow:hidden; }"),
		"a selector naming nothing the rows carry":   css("section > div > table > tbody > tr { display:none; }"),
		"a specificity fight the hiding rule wins":   css("body main section .tablewrap table tbody tr { display:none !important; }"),
		"an opaque overlay painted over the page":    css("body::after { content:''; position:fixed; inset:0; background:#000; z-index:9999; }"),
		"a hidden attribute on the table bodies":     domReplace(`<tbody id="queue">`, `<tbody id="queue" hidden>`, `<tbody id="history">`, `<tbody id="history" hidden>`),
		"a hidden attribute on the aggregate host":   domReplace(`<div class="aggs" id="aggregates">`, `<div class="aggs" id="aggregates" hidden>`),
		"the aggregate host removed from the markup": domReplace(`<div class="aggs" id="aggregates"></div>`, ``),
	}
}

func domReplace(pairs ...string) func([]byte) []byte {
	return func(b []byte) []byte {
		s := string(b)
		for i := 0; i+1 < len(pairs); i += 2 {
			s = strings.Replace(s, pairs[i], pairs[i+1], 1)
		}
		return []byte(s)
	}
}

func TestRendered_EveryDashboardGraderFailsAgainstEveryHidingMutation(t *testing.T) {
	bin := chromium(t)

	// The unmutated document first, so a report below is a signal and not the baseline.
	base, _ := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})
	if probs := dashProblems(base); probs != nil {
		t.Fatalf("the shipped document was reported as hiding its own subjects: %v", probs)
	}

	plain := servedDocument(t)
	for name, mutate := range hidingMutations() {
		if string(mutate([]byte(plain))) == plain {
			t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", name)
		}
		v, log := renderDashboard(t, bin, dashOpts{snapshot: fixtureSnapshot(), mutate: mutate, wait: 8 * time.Second})
		probs := dashProblems(v)
		if probs == nil {
			t.Errorf("the rendered graders passed a mutation that hides their subject from a reader (%s)\nverdict: %+v\nbrowser output:\n%s",
				name, v, log)
		}
	}
}

// dashProblems is every rendered subject the graders above assert on, asked the one
// question B15 is about: did a reader see it?
func dashProblems(v dashVerdict) []string {
	if !v.Ready {
		return []string{"the page never reached the state the graders measure (connection state " + v.ConnText + ")"}
	}
	var out []string
	if len(v.Queue) == 0 {
		out = append(out, "no queue row was rendered at all")
	}
	if len(v.History) == 0 {
		out = append(out, "no history row was rendered at all")
	}
	if len(v.Aggregates) != 6 {
		out = append(out, fmt.Sprintf("%d aggregate cards were rendered, want 6", len(v.Aggregates)))
	}
	for _, r := range append(append([]dashRow{}, v.Queue...), v.History...) {
		out = append(out, shownProblems("row "+r.Path, r.Shown)...)
		if r.Path != "" && !strings.Contains(v.BodyText, r.Path) {
			out = append(out, "the row "+r.Path+" is not in the text a reader can see")
		}
	}
	for _, a := range v.Aggregates {
		out = append(out, shownProblems("aggregate card "+a.Title, a.Shown)...)
		if a.Coverage != "" && !strings.Contains(v.BodyText, a.Coverage) {
			out = append(out, "the card "+a.Title+" states a set that is not text a reader can see")
		}
	}
	out = append(out, shownProblems("the queue cap notice", v.QueueCap.Shown)...)
	out = append(out, shownProblems("the history cap notice", v.HistCap.Shown)...)
	return out
}

// servedDocument is the document the real handler produces, for asserting a mutation
// actually changes it.
func servedDocument(t *testing.T) string {
	t.Helper()
	_, doc := fetchRoot(t, HandlerFor(offerFor(sourceoffer.Upstream)))
	return doc
}

// --- B1: nothing is fetched from outside the server that served the page ---------

func TestRendered_PageFetchesNothingFromOutsideTheServerThatServedIt(t *testing.T) {
	bin := chromium(t)
	v, _ := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})
	if len(v.OffOrigin) != 0 {
		t.Errorf("the rendered page reaches outside the server that served it: %v", v.OffOrigin)
	}

	// The grader must BITE. Each mutation is a way a page reaches off-origin, and the
	// browser is asked about each in turn.
	for name, mutate := range map[string]func([]byte) []byte{
		"an off-origin image element": domReplace(
			`<div class="chips" id="chips"></div>`,
			`<div class="chips" id="chips"></div><img src="https://example.invalid/pixel.png" alt="">`),
		"an off-origin stylesheet link": domReplace(
			`</head>`, `<link rel="stylesheet" href="https://example.invalid/theme.css"></head>`),
		"an off-origin background image in a rule the cascade applies": func(b []byte) []byte {
			return []byte(strings.Replace(string(b), "</style>",
				"body { background-image:url(https://example.invalid/bg.png); }\n</style>", 1))
		},
		"an off-origin font": func(b []byte) []byte {
			return []byte(strings.Replace(string(b), "</style>",
				"@font-face { font-family:x; src:url(https://example.invalid/x.woff2); }\n</style>", 1))
		},
		"an off-origin iframe": domReplace(
			`</main>`, `<iframe src="https://example.invalid/frame"></iframe></main>`),
	} {
		v, log := renderDashboard(t, bin, dashOpts{snapshot: fixtureSnapshot(), mutate: mutate, wait: 15 * time.Second})
		if !v.Ready {
			t.Fatalf("%s: the page did not render at all\nbrowser output:\n%s", name, log)
		}
		if len(v.OffOrigin) == 0 {
			t.Errorf("the off-origin grader passed a page that reaches off-origin (%s)", name)
		}
	}
}

// --- B17: no CSP and no Trusted Types violation while rendering real data --------

// policyRefusals reports every line of the browser's own output in which it refused
// something under the page's Content-Security-Policy, Trusted Types included.
//
// This is the browser's report, not the page's: a Trusted Types violation fires in the
// SERVED document, and a listener attached from the parent probe at load time arrives
// after the initial render has already happened. The engine's own log has no such
// ordering problem, which is why B17 is graded here.
func policyRefusals(browserLog string) []string {
	var out []string
	for _, line := range strings.Split(browserLog, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "content security policy") ||
			strings.Contains(l, "refused to") ||
			strings.Contains(l, "trustedhtml") ||
			strings.Contains(l, "trustedscript") ||
			strings.Contains(l, "trusted type") ||
			strings.Contains(l, "require-trusted-types-for") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// declaredStatusCount reads the STATUSES vocabulary out of the SERVED document - the same
// bytes the browser just rendered - so "one chip per status" is checked against what the
// page says it has rather than against a number a reader has to keep in step by hand. It
// fails loudly if the constant cannot be found, because a silently-zero count would make
// the assertion it feeds unable to fail.
func declaredStatusCount(t *testing.T) int {
	t.Helper()
	m := regexp.MustCompile(`const STATUSES = \[([^\]]*)\]`).FindSubmatch(indexHTML)
	if m == nil {
		t.Fatal("the served document declares no `const STATUSES = [...]`, so the chip count cannot be derived")
	}
	n := 0
	for _, s := range strings.Split(string(m[1]), ",") {
		if strings.TrimSpace(s) != "" {
			n++
		}
	}
	if n == 0 {
		t.Fatal("the served document's STATUSES list is empty")
	}
	return n
}

func TestRendered_NoPolicyViolationWhileRenderingRealData(t *testing.T) {
	bin := chromium(t)

	// First: the grader BITES. Two mutations, each a way the page could reach the DOM
	// that its own policy refuses, and the browser must be seen refusing each.
	for name, mutate := range map[string]func([]byte) []byte{
		"a string assigned to an HTML sink (Trusted Types)": domReplace(
			`</body>`, `<script>document.body.innerHTML = "<b>x</b>";</script></body>`),
		"a resource from an origin default-src 'none' refuses": domReplace(
			`</body>`, `<img src="https://example.invalid/pixel.png" alt=""></body>`),
	} {
		_, log := renderDashboard(t, bin, dashOpts{snapshot: fixtureSnapshot(), mutate: mutate, wait: 15 * time.Second})
		refusals := policyRefusals(log)
		if len(refusals) == 0 {
			t.Fatalf("the browser reported no refusal for %q, so this grader cannot fail.\nbrowser output:\n%s", name, log)
		}
		t.Logf("the browser refused %s:\n  %s", name, strings.Join(refusals, "\n  "))
	}

	// Then: the shipped page, rendering a snapshot carrying queue rows, history rows and
	// aggregates, under its own policy, with the browser reporting nothing refused.
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})
	if refusals := policyRefusals(log); len(refusals) != 0 {
		t.Errorf("the browser refused something while the page rendered real data:\n%s\nfull browser output:\n%s",
			strings.Join(refusals, "\n"), log)
	}
	// The observable consequence of the same property, asserted as well: a sink assignment
	// under `require-trusted-types-for 'script'` THROWS, so a render that reached one would
	// stop short and the rows, cards and badges after it would be missing.
	if probs := dashProblems(v); probs != nil {
		t.Errorf("the render did not complete: %v", probs)
	}
	if v.Badges.Scan == "" || v.ReclaimedLifetime == "" || v.SRStatus == "" {
		t.Errorf("the render stopped short: badges=%+v reclaimed=%q sr=%q", v.Badges, v.ReclaimedLifetime, v.SRStatus)
	}
}

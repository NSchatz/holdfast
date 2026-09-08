package webui

import (
	"encoding/json"
	"fmt"
	"math"
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
// STRIP removes every drawing from the rendered document before the reading is taken.
// It is how B24 asks the one question a text equivalent exists to answer: with the
// graphic gone and nothing else touched, does the card still show every value the
// graphic encoded?
const STRIP = "%STRIP%" === "1";

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

// --- DASH-9: the drawings, the colours, and the order of the page ------------------

// visText is what a READER can see INSIDE a rendered element: innerText is computed from
// the layout, so text the cascade hid is absent from it while textContent would hand it
// back regardless.
//
// It has one edge, and it is the edge a grader gets caught on: innerText falls back to
// the descendant text content when the element it is asked about is ITSELF not rendered.
// So visText answers "what does this box show" and never "is this box shown" - that is
// renderedFlag's question, and every text equivalent below is asked both.
function visText(el) { return el ? String(el.innerText || "").trim() : ""; }

// renderedFlag is "did this element reach the screen at all": a real box, and nothing in
// its ancestry that removes it from the render.
function renderedFlag(el, win) {
  if (!el) return false;
  const r = el.getBoundingClientRect();
  if (r.width <= 0 || r.height <= 0) return false;
  for (let n = el; n; n = n.parentElement) {
    const cs = win.getComputedStyle(n);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.visibility === "collapse") return false;
    if (cs.opacity === "0") return false;
    if (n.hasAttribute("hidden")) return false;
  }
  return true;
}

// parseColor / relLum / contrastOf are WCAG 2.2's own definitions, applied to the colours
// the BROWSER computed. The contrast ratio is (L1 + 0.05) / (L2 + 0.05) with L1 the
// lighter relative luminance; relative luminance is the 0.2126/0.7152/0.0722 weighting of
// the linearised channels, with the 0.03928 knee. Both are transcribed from the committed
// copy of the specification, and the Go side recomputes every ratio from the two colours
// reported here rather than trusting this arithmetic.
function parseColor(s) {
  const m = /^rgba?\(([^)]*)\)$/.exec(String(s).trim());
  if (!m) return null;
  const p = m[1].split(/[,\s\/]+/).filter(function (x) { return x !== ""; }).map(Number);
  if (p.length < 3) return null;
  for (const v of p) { if (!isFinite(v)) return null; }
  return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
}
function relLum(c) {
  function chan(v) {
    v = v / 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  }
  return 0.2126 * chan(c.r) + 0.7152 * chan(c.g) + 0.0722 * chan(c.b);
}
function contrastOf(a, b) {
  const la = relLum(a), lb = relLum(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}
function rgbText(c) { return "rgb(" + c.r + ", " + c.g + ", " + c.b + ")"; }

// behind is what a mark is actually drawn ON: the nearest ancestor that paints an opaque
// background. A mark's own element paints nothing behind itself, so the walk starts at
// its parent.
function behind(el, doc, win) {
  for (let n = el; n; n = n.parentElement) {
    const c = parseColor(win.getComputedStyle(n).backgroundColor);
    if (c && c.a >= 0.999) return c;
  }
  const root = parseColor(win.getComputedStyle(doc.documentElement).backgroundColor);
  return root && root.a >= 0.999 ? root : { r: 255, g: 255, b: 255, a: 1 };
}

// contrastSubjects is every graphical element on this page a reader has to tell apart
// from what is drawn behind or beside it in order to read the figure it presents: the
// bars of a distribution, the scale and ticks of a spread, a status dot, the edge of a
// badge, of a count chip and of a figure card. Each is reported with the colour it is
// PAINTED in, the colour it is painted ON, and the ratio between them.
//
// Table-row rules and section separators are deliberately not here. They divide the page
// but carry no figure, and WCAG 2.2's non-text floor is scoped to what must be perceived
// to understand a component - which is the same line this page's --line token already draws.
function contrastSubjects(doc, win) {
  const out = [];
  function add(what, el, prop) {
    const cs = win.getComputedStyle(el);
    const fg = parseColor(cs[prop]);
    const bg = behind(el.parentElement, doc, win);
    if (!fg) { out.push({ what: what, prop: prop, fg: String(cs[prop]), bg: "", ratio: 0, readable: false }); return; }
    out.push({ what: what, prop: prop, fg: rgbText(fg), bg: rgbText(bg),
               ratio: contrastOf(fg, bg), readable: true });
  }
  function addAll(sel, prop, label) {
    let i = 0;
    for (const el of doc.querySelectorAll(sel)) add(label + " " + (i++), el, prop);
  }
  addAll(".agg .fig .mark", "fill", "distribution bar");
  addAll(".agg .fig .axis", "stroke", "spread scale");
  addAll(".agg .fig .tick", "stroke", "spread tick");
  addAll("td.st .dot", "backgroundColor", "status dot");
  addAll(".badges .badge", "borderTopColor", "badge edge");
  addAll("#chips .chip", "borderTopColor", "count chip edge");
  addAll("#aggregates .agg", "borderTopColor", "figure card edge");
  return out;
}

// figuresOf reports each aggregate card as a FIGURE: what it drew, where every mark
// landed on the screen, and the text the card shows in document order. The two together
// are what decides whether a drawing carries a value the text does not.
function figuresOf(doc, win) {
  const host = doc.getElementById("aggregates");
  if (!host) return [];
  function box(el) {
    const r = el.getBoundingClientRect();
    return { x: r.left, y: r.top, w: r.width, h: r.height };
  }
  return Array.prototype.map.call(host.children, function (card) {
    const drawings = card.querySelectorAll("svg.fig");
    const first = drawings.length ? drawings[0] : null;
    const bars = Array.prototype.map.call(card.querySelectorAll(".fig.bar .mark"), box);
    const ticks = Array.prototype.map.call(card.querySelectorAll(".fig.spread .tick"), function (t) {
      const b = box(t);
      b.cls = t.getAttribute("class") || "";
      b.strokeWidth = win.getComputedStyle(t).strokeWidth;
      return b;
    });
    const keys = Array.prototype.map.call(card.querySelectorAll(".buckets .bk"), visText);
    const counts = Array.prototype.map.call(card.querySelectorAll(".buckets .bc"), visText);
    const keysShown = Array.prototype.map.call(card.querySelectorAll(".buckets .bk"),
      function (e) { return renderedFlag(e, win); });
    const countsShown = Array.prototype.map.call(card.querySelectorAll(".buckets .bc"),
      function (e) { return renderedFlag(e, win); });
    const spread = Array.prototype.map.call(card.querySelectorAll(".spreadkeys .sk"), function (k) {
      return { name: visText(k.querySelector(".sn")), value: visText(k.querySelector(".sv")),
               shown: renderedFlag(k, win) };
    });
    return {
      title: textOf(card.querySelector(".agg-k")),
      out: card.classList.contains("out"),
      drawings: drawings.length,
      kind: card.querySelector(".fig.bar") ? "dist" : (card.querySelector(".fig.spread") ? "spread" : ""),
      drawingShown: first ? shownRecord(first, doc, win) : { present: false, chain: [] },
      bars: bars,
      ticks: ticks,
      keys: keys,
      counts: counts,
      keysShown: keysShown,
      countsShown: countsShown,
      spread: spread,
      valueText: visText(card.querySelector(".agg-v")),
      cardText: visText(card),
      titled: card.querySelectorAll("[title]").length,
      shown: shownRecord(card, doc, win)
    };
  });
}

// carriersOf is everything on the reorganised page whose COLOUR says something, each with
// the rendered text or rendered shape that says the same thing without it. A carrier whose
// second means is missing shows up here as an empty text and an indistinguishable shape.
function carriersOf(doc, win) {
  const out = [];
  // add(what, kind, state, coloured, prop, says): "coloured" is the element whose COLOUR
  // carries the meaning; "says" is the element that carries the same meaning without it.
  // Both are reported, and "says" is asked whether it reached the screen at all, because
  // a hidden label still has text content and would otherwise read as a second means.
  function add(what, kind, state, coloured, prop, says, shape) {
    out.push({
      what: what, kind: kind, state: state,
      colour: coloured ? String(win.getComputedStyle(coloured)[prop]) : "",
      text: visText(says), shown: renderedFlag(says, win), shape: shape || ""
    });
  }
  const conn = doc.getElementById("conn");
  if (conn) add("the connection state", "conn", "", conn, "color", conn, "");
  for (const id of ["b-paused", "b-scan"]) {
    const b = doc.getElementById(id);
    if (b) add("the " + id + " badge", "badge", "", b, "backgroundColor", b, "");
  }
  const chips = doc.getElementById("chips");
  if (chips) {
    for (const c of chips.children) {
      const n = c.querySelector(".n"), k = c.querySelector(".k");
      // The chip's KEY is the state in words; the number's colour is the second reading.
      add("the " + visText(k) + " count chip", "chip", visText(k), n, "color", k, "");
    }
  }
  for (const st of doc.querySelectorAll("td.st")) {
    const dot = st.querySelector(".dot");
    if (!dot) continue;
    const holder = st.querySelector("[class*=st-]") || st;
    const m = /st-([a-z]+)/.exec(holder.getAttribute("class") || "");
    const cs = win.getComputedStyle(dot);
    add("the status dot of a " + (m ? m[1] : "?") + " row", "dot", m ? m[1] : "",
        dot, "backgroundColor", st,
        cs.borderTopLeftRadius + " " + cs.borderTopRightRadius + " " + cs.transform +
        " " + cs.width + "x" + cs.height);
  }
  for (const a of doc.querySelectorAll("#aggregates .agg.out")) {
    const v = a.querySelector(".agg-v");
    add("the unavailable figure " + textOf(a.querySelector(".agg-k")), "out", "", v, "color", a, "");
  }
  return out;
}

// regionsOf is the page's own answer to "which question does this page answer first".
// Document position and the DOCUMENT-SPACE top edge of each region are both read here,
// before anything scrolls the page, so neither reading depends on the other.
function regionsOf(doc, win) {
  function rec(id) {
    const el = doc.getElementById(id);
    if (!el) return { present: false, top: 0, heading: "", holds: {} };
    const r = el.getBoundingClientRect();
    return {
      present: true,
      top: r.top + win.scrollY,
      heading: visText(el.querySelector("h2")),
      holds: {
        badges: !!el.querySelector("#b-paused") && !!el.querySelector("#b-scan"),
        counts: !!el.querySelector("#chips"),
        queue: !!el.querySelector("#queue"),
        aggregates: !!el.querySelector("#aggregates"),
        history: !!el.querySelector("#history")
      }
    };
  }
  const now = doc.getElementById("region-now"), hist = doc.getElementById("region-history");
  const conn = doc.getElementById("conn");
  return {
    now: rec("region-now"),
    history: rec("region-history"),
    nowPrecedesHistory: !!(now && hist &&
      (now.compareDocumentPosition(hist) & win.Node.DOCUMENT_POSITION_FOLLOWING) !== 0),
    connTop: conn ? conn.getBoundingClientRect().top + win.scrollY : 0
  };
}

// Every attribute anywhere in the document that can name a URL the browser would FETCH,
// vector attributes included: an <image href>, a <use href> pointing into another
// document, an xlink:href from the older SVG dialect, and a filter, mask or marker
// reference. An <a href> is deliberately NOT in this set: a hyperlink is a navigation
// target the reader chooses, not a resource the page loads, and the AGPL section 13
// source offer this page must carry IS such a link.
//
// It is a sweep over EVERY element rather than a table of tag/attribute pairs, because a
// table only ever covers the fetch surfaces somebody remembered - and DASH-9 adds a whole
// dialect of them at once.
const URL_ATTRS = ["src","srcset","data","poster","action","formaction","background",
  "href","xlink:href","cite","longdesc","icon","manifest","profile","codebase","archive",
  "fill","stroke","filter","mask","clip-path","marker-start","marker-mid","marker-end"];

function offOriginRefs(doc, win) {
  const out = [];
  const origin = win.location.origin;
  const base = doc.baseURI;
  function check(where, raw) {
    if (!raw) return;
    const text = String(raw).trim();
    if (text === "") return;
    if (/^data:/i.test(text)) { out.push(where + " -> a data: URI (" + text.slice(0, 48) + ")"); return; }
    let u;
    try { u = new win.URL(text, base); } catch (e) { out.push(where + " -> unresolvable " + text); return; }
    if (u.protocol === "data:") { out.push(where + " -> a data: URI"); return; }
    if (u.origin !== origin) out.push(where + " -> " + u.href);
  }
  const re = /url\(\s*(['"]?)([^'")]*)\1\s*\)/g;
  function scan(text, where) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(text)) !== null) check(where, m[2]);
  }
  for (const el of doc.querySelectorAll("*")) {
    const tag = el.tagName.toLowerCase();
    for (const attr of URL_ATTRS) {
      if (!el.hasAttribute(attr)) continue;
      if (attr === "href" && (tag === "a" || tag === "area")) continue;
      const raw = el.getAttribute(attr);
      const where = tag + "[" + attr + "]";
      if (attr === "srcset") {
        for (const piece of String(raw).split(",")) check(where, piece.trim().split(/\s+/)[0]);
      } else if (String(raw).indexOf("url(") >= 0) {
        scan(String(raw), where);
      } else {
        // Everything else is resolved against the page's own base. A value that is not a
        // URL at all (fill="none") resolves same-origin and is silent; only something
        // that leaves this origin, or names a data: URI, is reported.
        check(where, raw);
      }
    }
    // Any other attribute carrying a url() token - a style attribute, an SVG paint
    // written inline, an attribute nobody enumerated above.
    for (const a of el.attributes) {
      if (String(a.value).indexOf("url(") >= 0) scan(String(a.value), tag + "@" + a.name);
    }
  }
  // Every url() any rule in the page's own stylesheets names, @font-face included (its
  // src is part of the rule's own text), and every url() the cascade actually applied to
  // an element or one of its pseudo-elements. A background image, a font, a mask, a
  // filter, a clip path, a cursor or an SVG paint server is a fetch too.
  for (const sheet of doc.styleSheets) {
    if (sheet.href) check("external stylesheet", sheet.href);
    let rules = null;
    try { rules = sheet.cssRules; } catch (e) { out.push("unreadable stylesheet " + String(sheet.href)); continue; }
    for (const r of rules) scan(r.cssText, "style rule");
  }
  const props = ["backgroundImage","borderImageSource","listStyleImage","maskImage",
    "webkitMaskImage","cursor","content","fill","stroke","filter","clipPath",
    "offsetPath","shapeOutside","strokeDasharray"];
  for (const el of doc.querySelectorAll("*")) {
    for (const pseudo of [null, "::before", "::after"]) {
      const cs = win.getComputedStyle(el, pseudo);
      for (const p of props) {
        const v = cs[p];
        if (v && String(v).indexOf("url(") >= 0) {
          scan(String(v), "computed " + el.tagName.toLowerCase() + (pseudo || "") + " " + p);
        }
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
  // The page's own reading order, taken BEFORE anything below scrolls the document.
  const regions = regionsOf(doc, win);
  // No pointer event has been dispatched into this document and none ever is: nothing
  // below hovers, clicks, focuses or moves a pointer over anything. Whatever the figures
  // show at this instant is what they show to a reader who never points at them.
  if (STRIP) { for (const g of doc.querySelectorAll("svg.fig")) g.remove(); }
  const conn = doc.getElementById("conn");
  const chips = doc.getElementById("chips");
  return {
    ready: true,
    origin: win.location.origin,
    regions: regions,
    figures: figuresOf(doc, win),
    carriers: carriersOf(doc, win),
    contrast: contrastSubjects(doc, win),
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

// --- DASH-9: the drawings, the colour carriers and the page's order --------------

// markBox is one rendered mark, in CSS pixels, exactly where the browser put it.
type markBox struct {
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	W           float64 `json:"w"`
	H           float64 `json:"h"`
	Class       string  `json:"cls"`
	StrokeWidth string  `json:"strokeWidth"`
}

type spreadKey struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Shown bool   `json:"shown"`
}

// dashFigure is one aggregate card seen as a FIGURE: what it drew, where each mark
// landed, and the text a reader can see in it.
type dashFigure struct {
	Title        string      `json:"title"`
	Out          bool        `json:"out"`
	Drawings     int         `json:"drawings"`
	Kind         string      `json:"kind"`
	DrawingShown shownRec    `json:"drawingShown"`
	Bars         []markBox   `json:"bars"`
	Ticks        []markBox   `json:"ticks"`
	Keys         []string    `json:"keys"`
	Counts       []string    `json:"counts"`
	KeysShown    []bool      `json:"keysShown"`
	CountsShown  []bool      `json:"countsShown"`
	Spread       []spreadKey `json:"spread"`
	ValueText    string      `json:"valueText"`
	CardText     string      `json:"cardText"`
	Titled       int         `json:"titled"`
	Shown        shownRec    `json:"shown"`
}

// dashCarrier is one thing on the page whose colour says something, with whatever says
// the same thing WITHOUT colour beside it.
type dashCarrier struct {
	What   string `json:"what"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
	Colour string `json:"colour"`
	Text   string `json:"text"`
	Shown  bool   `json:"shown"`
	Shape  string `json:"shape"`
}

// dashContrast is one graphical element, the colour it was painted in, the colour it was
// painted on, and the browser-side ratio between them. The Go side recomputes that ratio
// from Fg and Bg rather than trusting it.
type dashContrast struct {
	What     string  `json:"what"`
	Prop     string  `json:"prop"`
	Fg       string  `json:"fg"`
	Bg       string  `json:"bg"`
	Ratio    float64 `json:"ratio"`
	Readable bool    `json:"readable"`
}

type dashRegion struct {
	Present bool    `json:"present"`
	Top     float64 `json:"top"`
	Heading string  `json:"heading"`
	Holds   struct {
		Badges     bool `json:"badges"`
		Counts     bool `json:"counts"`
		Queue      bool `json:"queue"`
		Aggregates bool `json:"aggregates"`
		History    bool `json:"history"`
	} `json:"holds"`
}

type dashRegions struct {
	Now                dashRegion `json:"now"`
	History            dashRegion `json:"history"`
	NowPrecedesHistory bool       `json:"nowPrecedesHistory"`
	ConnTop            float64    `json:"connTop"`
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

	Regions  dashRegions    `json:"regions"`
	Figures  []dashFigure   `json:"figures"`
	Carriers []dashCarrier  `json:"carriers"`
	Contrast []dashContrast `json:"contrast"`
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
	// strip removes every drawing from the RENDERED document before the reading is
	// taken, and changes nothing else. It is how the text equivalents are asked whether
	// they carry the figure on their own.
	strip bool
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
	strip := "0"
	if o.strip {
		strip = "1"
	}
	js = strings.Replace(js, "%STRIP%", strip, 1)
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
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":7},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":14},
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
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":0},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":0},
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
  "queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":2},
  "history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":3},
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
	// A pending row has no worker yet, and since S0053 that reads as the page's one
	// absence phrase rather than as a blank cell (F3).
	wantQueue := []struct{ path, status, worker string }{
		{"/media/films/alpha.mkv", "pending", absencePhrase},
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

	// A progress figure for the running encode, and for NO other row. Since S0053 a row
	// with no progress measurement does not show an EMPTY cell - it shows the page's one
	// absence phrase (F3), because a blank is a fact a reader has to guess at.
	if got := v.Queue[1].Progress; !strings.Contains(got, "33%") || !strings.Contains(got, "20m 0s of 1h 0m") {
		t.Errorf("the running encode shows progress %q, want the encoder's own figure and its position", got)
	}
	for _, i := range []int{0, 2} {
		if v.Queue[i].Progress != absencePhrase {
			t.Errorf("queue row %d (%s) shows a progress figure %q; only a running encode has one, and every other row must read %q",
				i, v.Queue[i].Status, v.Queue[i].Progress, absencePhrase)
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
	if len(v.Chips) != 7 {
		t.Errorf("the summary shows %d chips, want one per status (7)", len(v.Chips))
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
		// The WORDING is S0052's to set: a state row sits inside its own table, so it may
		// repeat neither the heading above it nor a column header beneath it. What B12
		// asserts is that each view says, in its own words, that it has nothing to show.
		{"queue", v.Queue, "No work in hand."},
		{"history", v.History, "No swap finished yet."},
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
	// Neither invents a size or a score it never had. Since S0053 the cell is not left
	// blank either: an encode that never happened recorded no size and no score, and F3
	// says the field says so in words, in the page's ONE absence phrase.
	for _, r := range []dashRow{skipped, failed} {
		if r.Size != absencePhrase || r.Vmaf != absencePhrase {
			t.Errorf("the %q row shows size %q and VMAF %q for an encode that never happened, want %q in both",
				r.Path, r.Size, r.Vmaf, absencePhrase)
		}
	}
	if !strings.Contains(v.BodyText, "hardlinked (would break a seed)") {
		t.Error("the guard label is in the DOM but not in the text a reader can see")
	}
}

// absencePhrase is the page's ONE absence phrase (clause F3). A fact nobody recorded
// reads as this, in words, in EVERY field that can carry one - never 0, never a bare
// dash, never a blank cell a reader has to interpret.
const absencePhrase = "not recorded"

// aggHostMarkup is the whole-ledger figures view exactly as the page shell writes it,
// including the loading state it ships in. The mutations that move or remove that view
// need the block verbatim, and every one of them is asserted to have CHANGED the served
// document before it is used, so a drift here fails loudly rather than silently
// mutating nothing.
const aggHostMarkup = `<div class="aggs" id="aggregates" data-view="aggs">
      <p class="state" data-state="loading">Loading these figures.</p>
    </div>`

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
		"a hidden attribute on the table bodies":     domReplace(`<tbody id="queue" data-view="queue">`, `<tbody id="queue" data-view="queue" hidden>`, `<tbody id="history" data-view="history">`, `<tbody id="history" data-view="history" hidden>`),
		"a hidden attribute on the aggregate host":   domReplace(`<div class="aggs" id="aggregates" data-view="aggs">`, `<div class="aggs" id="aggregates" data-view="aggs" hidden>`),
		"the aggregate host removed from the markup": domReplace(aggHostMarkup, ``),
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

// =================================================================================
// DASH-9: the page rebuilt around the two questions an operator has.
//
// Everything below decides a claim about a DRAWING - what it encodes, what colour it is
// painted in, where its marks landed, what text stands beside it, and which of the page's
// two questions is answered first. Every one of those is a fact about a document a
// browser rendered, so every one is read out of a real browser engine: computed style
// after the whole cascade, real layout geometry in CSS pixels, and innerText, which is
// what a reader can SEE rather than what is merely in the DOM.
//
// No assertion here matches HTML, CSS or JavaScript source text. That is the operator's
// ruling of 2026-09-06 and the reason S0035 was killed over five refute ordinals: a text
// grader cannot decide what a rule applies to, what wins the cascade, or what is SHOWN
// rather than merely built. TestRendered_EveryDASH9GraderFailsAgainstItsOwnMutation at
// the bottom of this file proves each grader below BITES, by serving a document
// deliberately mutated to defeat exactly that grader and requiring it to report.
// =================================================================================

// --- the graders, one per property, each usable on any verdict --------------------

// dash9Grader is one property of the rendered page, asked as a function so the mutation
// self-test can run every one of them against every counterexample.
type dash9Grader struct {
	name  string
	probe func(dashVerdict) []string
}

func dash9Graders() []dash9Grader {
	return []dash9Grader{
		{"the current run is presented before the history", gradeOrder},
		{"every drawing reached the screen", gradeDrawingsShown},
		{"every bucket figure states its labels and counts as text", gradeBucketText},
		{"every spread figure states its minimum, mean and maximum as text", gradeSpreadText},
		{"every bar is in proportion to its own count", gradeProportion},
		{"the marks of a spread are told apart by position", gradeSpreadMarks},
		{"every colour carrier is paired with text or shape", gradeCarriers},
		{"every graphical element clears 3:1 against what is behind it", gradeContrast},
		{"nothing is fetched from another origin", gradeOffOrigin},
		{"no figure is readable only by pointing at it", gradeNoTooltip},
	}
}

// gradeOrder: the current-run region comes first in the document AND first on the screen,
// each region is under its own heading, and neither region holds the other's subject.
func gradeOrder(v dashVerdict) []string {
	var out []string
	r := v.Regions
	if !r.Now.Present {
		out = append(out, "the rendered page has no current-run region")
	}
	if !r.History.Present {
		out = append(out, "the rendered page has no history region")
	}
	if !r.Now.Present || !r.History.Present {
		return out
	}
	if strings.TrimSpace(r.Now.Heading) == "" {
		out = append(out, "the current-run region carries no heading a reader can see")
	}
	if strings.TrimSpace(r.History.Heading) == "" {
		out = append(out, "the history region carries no heading a reader can see")
	}
	if !r.NowPrecedesHistory {
		out = append(out, "the history region comes BEFORE the current-run region in document order")
	}
	if r.Now.Top >= r.History.Top {
		out = append(out, fmt.Sprintf("the current-run region's rendered top edge is %.1f and the history region's is %.1f: the current run is not above the history on screen",
			r.Now.Top, r.History.Top))
	}
	if r.ConnTop >= r.History.Top {
		out = append(out, fmt.Sprintf("the connection state is rendered at %.1f, below the history region at %.1f",
			r.ConnTop, r.History.Top))
	}
	for _, c := range []struct {
		what string
		got  bool
		want bool
	}{
		{"the current-run region holds the live badges", r.Now.Holds.Badges, true},
		{"the current-run region holds the counts", r.Now.Holds.Counts, true},
		{"the current-run region holds the queue", r.Now.Holds.Queue, true},
		{"the current-run region holds the whole-ledger aggregates", r.Now.Holds.Aggregates, false},
		{"the current-run region holds the recent history", r.Now.Holds.History, false},
		{"the history region holds the whole-ledger aggregates", r.History.Holds.Aggregates, true},
		{"the history region holds the recent history", r.History.Holds.History, true},
		{"the history region holds the queue", r.History.Holds.Queue, false},
	} {
		if c.got != c.want {
			out = append(out, fmt.Sprintf("%s = %v, want %v", c.what, c.got, c.want))
		}
	}
	return out
}

// drawingProblems is shownProblems for a VECTOR drawing: the same computed-style and
// layout questions, without the hit test. A drawing does not paint its whole viewport -
// a short bar leaves most of its box unpainted - so a hit test at its centre answers a
// question about ink, not about whether a reader can see it.
func drawingProblems(what string, r shownRec) []string {
	if !r.Present {
		return []string{what + ": the browser found no drawing in the rendered document"}
	}
	var out []string
	if r.Width <= 0 || r.Height <= 0 {
		out = append(out, fmt.Sprintf("%s: has no rendered box (%.1fx%.1f)", what, r.Width, r.Height))
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

// gradeDrawingsShown: every figure the healthy fixture publishes drew something, and what
// it drew reached the screen.
func gradeDrawingsShown(v dashVerdict) []string {
	var out []string
	if len(v.Figures) == 0 {
		return []string{"the page rendered no figures at all"}
	}
	for _, f := range v.Figures {
		if f.Drawings == 0 {
			out = append(out, "the figure "+f.Title+" drew nothing")
			continue
		}
		out = append(out, drawingProblems("the drawing of "+f.Title, f.DrawingShown)...)
	}
	return out
}

// gradeBucketText: a distribution's labels and counts are text a reader can SEE, one pair
// per mark, in the order the marks are drawn, and inside the figure's own card.
func gradeBucketText(v dashVerdict) []string {
	var out []string
	seen := 0
	for _, f := range v.Figures {
		if len(f.Keys) == 0 && len(f.Counts) == 0 {
			continue
		}
		seen++
		if len(f.Keys) != len(f.Counts) {
			out = append(out, fmt.Sprintf("the figure %s shows %d labels and %d counts", f.Title, len(f.Keys), len(f.Counts)))
		}
		if len(f.Bars) > 0 && len(f.Bars) != len(f.Keys) {
			out = append(out, fmt.Sprintf("the figure %s draws %d marks and states %d labels", f.Title, len(f.Bars), len(f.Keys)))
		}
		for i, k := range f.Keys {
			if i >= len(f.KeysShown) || !f.KeysShown[i] {
				out = append(out, fmt.Sprintf("the figure %s: the label for mark %d (%q) never reached the screen", f.Title, i, k))
				continue
			}
			if strings.TrimSpace(k) == "" {
				out = append(out, fmt.Sprintf("the figure %s shows no label a reader can see for mark %d", f.Title, i))
				continue
			}
			if !strings.Contains(f.CardText, k) {
				out = append(out, fmt.Sprintf("the figure %s: the label %q is not in the text of its own card", f.Title, k))
			}
		}
		for i, c := range f.Counts {
			if i >= len(f.CountsShown) || !f.CountsShown[i] {
				out = append(out, fmt.Sprintf("the figure %s: the count for mark %d (%q) never reached the screen", f.Title, i, c))
				continue
			}
			if strings.TrimSpace(c) == "" {
				out = append(out, fmt.Sprintf("the figure %s shows no count a reader can see for mark %d", f.Title, i))
				continue
			}
			if !strings.Contains(f.CardText, c) {
				out = append(out, fmt.Sprintf("the figure %s: the count %q is not in the text of its own card", f.Title, c))
			}
		}
	}
	if seen == 0 {
		out = append(out, "no bucket figure stated a single label or count")
	}
	return out
}

// gradeSpreadText: a spread names its minimum, its mean and its maximum, in that order,
// each with a value a reader can see, inside the figure's own card.
func gradeSpreadText(v dashVerdict) []string {
	var out []string
	seen := 0
	for _, f := range v.Figures {
		if len(f.Spread) == 0 {
			continue
		}
		seen++
		want := []string{"min", "mean", "max"}
		if len(f.Spread) != len(want) {
			out = append(out, fmt.Sprintf("the figure %s names %d values on its scale, want min, mean and max", f.Title, len(f.Spread)))
			continue
		}
		for i, k := range f.Spread {
			if !k.Shown {
				out = append(out, fmt.Sprintf("the figure %s: the %s it names (%q = %q) never reached the screen",
					f.Title, want[i], k.Name, k.Value))
				continue
			}
			if k.Name != want[i] {
				out = append(out, fmt.Sprintf("the figure %s names value %d %q, want %q in the order the marks are drawn", f.Title, i, k.Name, want[i]))
			}
			if strings.TrimSpace(k.Value) == "" {
				out = append(out, fmt.Sprintf("the figure %s shows no %s a reader can see", f.Title, want[i]))
				continue
			}
			if !strings.Contains(f.CardText, k.Value) {
				out = append(out, fmt.Sprintf("the figure %s: the %s value %q is not in the text of its own card", f.Title, want[i], k.Value))
			}
		}
	}
	if seen == 0 {
		out = append(out, "no spread figure named a minimum, a mean or a maximum")
	}
	return out
}

// countValue reads back the count a card shows beside a mark, so the proportion below is
// measured against the page's OWN published number rather than against a fixture.
func countValue(s string) (float64, bool) {
	t := strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	t = strings.ReplaceAll(t, " ", "")
	if t == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// gradeProportion is the measurement the drawing exists to make honest: every bar's
// RENDERED width is its own count as a share of the largest count in the same figure,
// within one CSS pixel. Anything else is a picture that disagrees with the number printed
// beside it, which on this page is the cardinal sin.
func gradeProportion(v dashVerdict) []string {
	var out []string
	graded := 0
	for _, f := range v.Figures {
		if len(f.Bars) == 0 {
			continue
		}
		if len(f.Bars) != len(f.Counts) {
			out = append(out, fmt.Sprintf("the figure %s draws %d bars for %d counts", f.Title, len(f.Bars), len(f.Counts)))
			continue
		}
		counts := make([]float64, len(f.Counts))
		maxCount, maxAt := 0.0, -1
		for i, c := range f.Counts {
			n, ok := countValue(c)
			if !ok {
				out = append(out, fmt.Sprintf("the figure %s shows %q beside a mark, which is not a count", f.Title, c))
				break
			}
			counts[i] = n
			if n > maxCount {
				maxCount, maxAt = n, i
			}
		}
		if maxAt < 0 || maxCount <= 0 {
			continue
		}
		track := f.Bars[maxAt].W
		if track <= 0 {
			out = append(out, fmt.Sprintf("the figure %s draws its largest count (%v) with no width at all", f.Title, maxCount))
			continue
		}
		graded++
		for i, b := range f.Bars {
			want := track * counts[i] / maxCount
			if diff := b.W - want; diff > 1 || diff < -1 {
				out = append(out, fmt.Sprintf("the figure %s: the mark for %s (count %v of a largest %v) is %.2f CSS px wide, want %.2f - off by %.2f",
					f.Title, f.Keys[i], counts[i], maxCount, b.W, want, diff))
			}
			// Every bar is measured against the same track, so two bars in one figure
			// can be compared by eye. A bar starting somewhere else would break that.
			if x := b.X - f.Bars[maxAt].X; x > 1 || x < -1 {
				out = append(out, fmt.Sprintf("the figure %s: the mark for %s starts %.2f CSS px away from the others",
					f.Title, f.Keys[i], x))
			}
		}
	}
	if graded == 0 {
		out = append(out, "no bucket figure drew a mark whose proportion could be measured")
	}
	return out
}

// gradeSpreadMarks: a spread puts three marks on one scale and they are told apart by
// POSITION, not by colour - the minimum on the left, the maximum on the right, and the
// mean somewhere between them.
func gradeSpreadMarks(v dashVerdict) []string {
	var out []string
	graded := 0
	for _, f := range v.Figures {
		if len(f.Ticks) == 0 {
			continue
		}
		if len(f.Ticks) != 3 {
			out = append(out, fmt.Sprintf("the figure %s draws %d ticks on its scale, want three", f.Title, len(f.Ticks)))
			continue
		}
		graded++
		lo, mid, hi := f.Ticks[0], f.Ticks[1], f.Ticks[2]
		if !(lo.X < hi.X) {
			out = append(out, fmt.Sprintf("the figure %s draws its minimum at x=%.1f and its maximum at x=%.1f", f.Title, lo.X, hi.X))
		}
		if mid.X < lo.X-1 || mid.X > hi.X+1 {
			out = append(out, fmt.Sprintf("the figure %s draws its mean at x=%.1f, outside its own scale (%.1f to %.1f)", f.Title, mid.X, lo.X, hi.X))
		}
		if mid.H <= lo.H && mid.H <= hi.H {
			out = append(out, fmt.Sprintf("the figure %s draws its mean tick %.1f CSS px tall, no taller than its ends (%.1f, %.1f): with one colour for every mark, height is what tells them apart",
				f.Title, mid.H, lo.H, hi.H))
		}
	}
	if graded == 0 {
		out = append(out, "no spread figure drew a scale whose marks could be measured")
	}
	return out
}

// gradeCarriers: nothing on this page says something with colour alone. Every carrier is
// paired with rendered text saying the same thing, and the three terminal outcomes are
// additionally given three different SHAPES.
func gradeCarriers(v dashVerdict) []string {
	var out []string
	if len(v.Carriers) == 0 {
		return []string{"the page reported no colour carriers at all"}
	}
	shapes := map[string]string{}
	for _, c := range v.Carriers {
		if !c.Shown {
			out = append(out, c.What+" carries its meaning in colour ("+c.Colour+") and what says the same thing without it never reached the screen")
			continue
		}
		if strings.TrimSpace(c.Text) == "" {
			out = append(out, c.What+" carries its meaning in colour ("+c.Colour+") with no rendered text beside it")
			continue
		}
		if c.State != "" && !strings.Contains(strings.ToLower(c.Text), strings.ToLower(c.State)) {
			out = append(out, fmt.Sprintf("%s shows %q, which does not name the state %q its colour encodes", c.What, c.Text, c.State))
		}
		if c.Kind == "dot" && c.State != "" {
			if prev, ok := shapes[c.State]; ok && prev != c.Shape {
				out = append(out, fmt.Sprintf("two %s dots are drawn in different shapes (%q and %q)", c.State, prev, c.Shape))
			}
			shapes[c.State] = c.Shape
		}
	}
	// The three TERMINAL outcomes must differ in form, not only in hue.
	terminal := []string{"done", "failed", "skipped"}
	for i := 0; i < len(terminal); i++ {
		for j := i + 1; j < len(terminal); j++ {
			a, aok := shapes[terminal[i]]
			b, bok := shapes[terminal[j]]
			if aok && bok && a == b {
				out = append(out, fmt.Sprintf("the %s and %s dots are the same shape (%q): with no colour vision they are the same mark",
					terminal[i], terminal[j], a))
			}
		}
	}
	return out
}

// wcagContrast is WCAG 2.2's contrast ratio, recomputed HERE from the two colours the
// browser reported, so the grader never takes the page's own arithmetic on trust. The
// formula and the relative-luminance definition are the ones in the committed copy of
// the specification (work/specs/S0045-holdfast-dash-9/sources/www.w3.org-TR-WCAG22).
func wcagContrast(fg, bg string) (float64, bool) {
	lum := func(s string) (float64, bool) {
		m := rgbRe.FindStringSubmatch(strings.TrimSpace(s))
		if m == nil {
			return 0, false
		}
		var c [3]float64
		for i := 0; i < 3; i++ {
			n, err := strconv.ParseFloat(m[i+1], 64)
			if err != nil {
				return 0, false
			}
			v := n / 255
			if v <= 0.03928 {
				c[i] = v / 12.92
			} else {
				c[i] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2], true
	}
	a, ok := lum(fg)
	if !ok {
		return 0, false
	}
	b, ok := lum(bg)
	if !ok {
		return 0, false
	}
	hi, lo := math.Max(a, b), math.Min(a, b)
	return (hi + 0.05) / (lo + 0.05), true
}

var rgbRe = regexp.MustCompile(`^rgba?\(\s*([0-9.]+)[,\s]+([0-9.]+)[,\s]+([0-9.]+)`)

// gradeContrast: every mark, scale, tick, state indicator and figure boundary clears 3:1
// against what is drawn behind it, measured from the browser's computed styles.
func gradeContrast(v dashVerdict) []string {
	var out []string
	if len(v.Contrast) == 0 {
		return []string{"the page reported no graphical element to measure"}
	}
	for _, c := range v.Contrast {
		if !c.Readable {
			out = append(out, fmt.Sprintf("%s: the browser reported no usable %s colour (%q)", c.What, c.Prop, c.Fg))
			continue
		}
		got, ok := wcagContrast(c.Fg, c.Bg)
		if !ok {
			out = append(out, fmt.Sprintf("%s: %q on %q is not a pair of colours a ratio can be taken from", c.What, c.Fg, c.Bg))
			continue
		}
		if diff := got - c.Ratio; diff > 0.01 || diff < -0.01 {
			out = append(out, fmt.Sprintf("%s: the page computed %.3f:1 and the specification's own formula gives %.3f:1", c.What, c.Ratio, got))
		}
		if got < 3.0 {
			out = append(out, fmt.Sprintf("%s: %s %s on %s is %.2f:1, under the 3:1 floor for a graphical element",
				c.What, c.Prop, c.Fg, c.Bg, got))
		}
	}
	return out
}

func gradeOffOrigin(v dashVerdict) []string {
	if len(v.OffOrigin) == 0 {
		return nil
	}
	out := make([]string, 0, len(v.OffOrigin))
	for _, r := range v.OffOrigin {
		out = append(out, "the rendered page reaches outside the server that served it: "+r)
	}
	return out
}

// gradeNoTooltip: no figure hides a value behind a pointer. A title attribute is the one
// carrier that needs hovering and cannot be reached by keyboard at all.
func gradeNoTooltip(v dashVerdict) []string {
	var out []string
	for _, f := range v.Figures {
		if f.Titled > 0 {
			out = append(out, fmt.Sprintf("the figure %s carries %d title attribute(s): a value behind a tooltip is readable only by pointing at it",
				f.Title, f.Titled))
		}
	}
	return out
}

// --- B22: the current run is answered before the history --------------------------

func TestRendered_TheCurrentRunIsPresentedBeforeTheHistory(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	if probs := gradeOrder(v); probs != nil {
		t.Errorf("the reorganised page does not answer the live question first: %v\nregions: %+v\nbrowser output:\n%s",
			probs, v.Regions, log)
	}
	// Both headings are text a reader can see, not merely markup.
	for _, h := range []string{v.Regions.Now.Heading, v.Regions.History.Heading} {
		if !strings.Contains(v.BodyText, h) {
			t.Errorf("the region heading %q is in the DOM but not in the text a reader can see", h)
		}
	}
	// And the queue's rows really are above the history's on the screen.
	if len(v.Queue) == 0 || len(v.History) == 0 {
		t.Fatalf("the fixture did not render: %d queue rows, %d history rows", len(v.Queue), len(v.History))
	}
	t.Logf("current run at y=%.0f (%q), history at y=%.0f (%q)",
		v.Regions.Now.Top, v.Regions.Now.Heading, v.Regions.History.Top, v.Regions.History.Heading)
}

// --- B5: a bucket figure's own values, as text, in the order its marks are drawn ----

func TestRendered_EveryBucketFigureShowsItsValuesAsTextInMarkOrder(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	if probs := gradeBucketText(v); probs != nil {
		t.Errorf("a distribution does not state what it draws: %v\nbrowser output:\n%s", probs, log)
	}
	// The fixture's own published buckets, in the order the figure ships them.
	want := map[string]struct{ keys, counts []string }{
		"Outcomes": {
			keys:   []string{"done", "skipped", "failed"},
			counts: []string{"9", "3", "2"},
		},
		"Skips by guard": {
			keys:   []string{"hardlinked (would break a seed)", "already efficient (low bitrate)"},
			counts: []string{"2", "1"},
		},
	}
	seen := map[string]bool{}
	for _, f := range v.Figures {
		w, ok := want[f.Title]
		if !ok {
			continue
		}
		seen[f.Title] = true
		if got := strings.Join(f.Keys, "|"); got != strings.Join(w.keys, "|") {
			t.Errorf("the figure %s labels its marks %q, want %q in the order the marks are drawn", f.Title, got, strings.Join(w.keys, "|"))
		}
		if got := strings.Join(f.Counts, "|"); got != strings.Join(w.counts, "|") {
			t.Errorf("the figure %s counts its marks %q, want %q", f.Title, got, strings.Join(w.counts, "|"))
		}
		if len(f.Bars) != len(w.keys) {
			t.Errorf("the figure %s draws %d marks for %d buckets", f.Title, len(f.Bars), len(w.keys))
		}
		// Read in sequence, the card's own visible text yields label then count, per mark.
		pos := 0
		for i := range w.keys {
			k := strings.Index(f.CardText[pos:], w.keys[i])
			if k < 0 {
				t.Fatalf("the figure %s does not show %q at all: %q", f.Title, w.keys[i], f.CardText)
			}
			pos += k + len(w.keys[i])
			c := strings.Index(f.CardText[pos:], w.counts[i])
			if c < 0 {
				t.Fatalf("the figure %s does not show the count %q after the label %q: %q",
					f.Title, w.counts[i], w.keys[i], f.CardText)
			}
			pos += c + len(w.counts[i])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("the page rendered %d of the %d bucket figures the fixture publishes", len(seen), len(want))
	}
}

// --- B6: a spread figure's minimum, mean and maximum, with its coverage -------------

func TestRendered_EverySpreadFigureShowsItsThreeValuesAndItsCoverage(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	if probs := gradeSpreadText(v); probs != nil {
		t.Errorf("a spread does not state what it draws: %v\nbrowser output:\n%s", probs, log)
	}
	if probs := gradeSpreadMarks(v); probs != nil {
		t.Errorf("a spread's marks are not told apart by position and height: %v", probs)
	}
	// Each figure's own published numbers, and the count it was computed over.
	want := map[string]struct {
		min, mean, max string
		counted        string
	}{
		"Replacement size": {"21%", "38%", "74%", "9"},
		"Encode time":      {"2m 0s", "15m 0s", "1h 30m", "9"},
		"VMAF pooled mean": {"95.1", "97.4", "99.2", "8"},
		"VMAF worst frame": {"81.2", "90.6", "96.3", "8"},
	}
	byTitle := map[string]dashFigure{}
	for _, f := range v.Figures {
		byTitle[f.Title] = f
	}
	for title, w := range want {
		f, ok := byTitle[title]
		if !ok {
			t.Errorf("the page rendered no figure titled %q", title)
			continue
		}
		got := map[string]string{}
		for _, k := range f.Spread {
			got[k.Name] = k.Value
		}
		for name, value := range map[string]string{"min": w.min, "mean": w.mean, "max": w.max} {
			if got[name] != value {
				t.Errorf("the figure %s states its %s as %q, want %q", title, name, got[name], value)
			}
		}
		// The count of files the spread was computed over, and the exclusions the card
		// already carried, are both in the figure's own region.
		if !strings.Contains(f.CardText, "across "+w.counted+" files") {
			t.Errorf("the figure %s does not say how many files it was computed over: %q", title, f.CardText)
		}
		if !strings.Contains(f.CardText, "excluded: no recorded value") {
			t.Errorf("the figure %s does not restate its exclusions beside its values: %q", title, f.CardText)
		}
	}
}

// --- B7: remove every drawing and the page still carries every number ---------------

func TestRendered_RemovingEveryDrawingLeavesEveryValueAsText(t *testing.T) {
	bin := chromium(t)
	// The SAME document, rendered the same way; the only difference is that every drawing
	// is removed from the rendered document before the reading is taken.
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot(), strip: true})

	for _, f := range v.Figures {
		if f.Drawings != 0 {
			t.Fatalf("the figure %s still holds %d drawing(s); the reading was not taken with them removed", f.Title, f.Drawings)
		}
	}
	// Every value every drawing encoded is still there, as text a reader can see.
	for _, want := range []string{
		"done", "9", "skipped", "3", "failed", "2",
		"hardlinked (would break a seed)", "already efficient (low bitrate)",
		"21%", "38%", "74%", "2m 0s", "15m 0s", "1h 30m",
		"95.1", "97.4", "99.2", "81.2", "90.6", "96.3",
	} {
		if !strings.Contains(v.BodyText, want) {
			t.Errorf("with the drawings removed, %q is no longer text a reader can see\nbrowser output:\n%s", want, log)
		}
	}
	// And the text equivalents themselves are still complete and in order.
	if probs := gradeBucketText(v); probs != nil {
		t.Errorf("with the drawings removed, a distribution no longer states its values: %v", probs)
	}
	if probs := gradeSpreadText(v); probs != nil {
		t.Errorf("with the drawings removed, a spread no longer states its values: %v", probs)
	}
}

// --- B8: every bar is in proportion, measured from the rendered geometry -------------

func TestRendered_EveryBarIsInProportionToItsOwnCount(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	if probs := gradeProportion(v); probs != nil {
		t.Errorf("a drawing disagrees with the number printed beside it: %v\nbrowser output:\n%s", probs, log)
	}
	// And the measurement is a real one: the fixture's own 9/3/2 must produce three
	// visibly different lengths, so the grader above is not comparing a row of equals.
	for _, f := range v.Figures {
		if f.Title != "Outcomes" || len(f.Bars) != 3 {
			continue
		}
		if !(f.Bars[0].W > f.Bars[1].W && f.Bars[1].W > f.Bars[2].W) {
			t.Errorf("the Outcomes figure draws 9, 3 and 2 as %.1f, %.1f and %.1f CSS px",
				f.Bars[0].W, f.Bars[1].W, f.Bars[2].W)
		}
		t.Logf("Outcomes bars: %.2f / %.2f / %.2f CSS px for counts 9 / 3 / 2", f.Bars[0].W, f.Bars[1].W, f.Bars[2].W)
	}
}

// --- B9 / B10: nothing says anything with colour alone -----------------------------

func TestRendered_NoMeaningIsCarriedByColourAlone(t *testing.T) {
	bin := chromium(t)
	// A snapshot carrying a done row, a skipped row and a failed row at once, so all
	// three terminal dots are on the page to be compared.
	v, log := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot()})

	if probs := gradeCarriers(v); probs != nil {
		t.Errorf("something on this page says what it means in colour alone: %v\nbrowser output:\n%s", probs, log)
	}
	// The connection state, the badges, the chips and the status dots are each here.
	kinds := map[string]int{}
	for _, c := range v.Carriers {
		kinds[c.Kind]++
	}
	for _, want := range []string{"conn", "badge", "chip", "dot"} {
		if kinds[want] == 0 {
			t.Errorf("the grader found no %s carrier to measure at all: %+v", want, kinds)
		}
	}
	// An unavailable figure keeps the literal word beside its warn colour.
	u, _ := mustRender(t, bin, dashOpts{snapshot: brokenAggregatesSnapshot()})
	outs := 0
	for _, c := range u.Carriers {
		if c.Kind != "out" {
			continue
		}
		outs++
		if !strings.Contains(strings.ToLower(c.Text), "unavailable") {
			t.Errorf("%s shows %q, want the literal word beside its colour", c.What, c.Text)
		}
	}
	if outs == 0 {
		t.Error("no unavailable figure was rendered to measure")
	}
	if probs := gradeCarriers(u); probs != nil {
		t.Errorf("with figures unavailable, something says what it means in colour alone: %v", probs)
	}
}

// collapseColours forces EVERY colour the page can paint with to one value. What is still
// readable afterwards is what was never carried by colour in the first place.
func collapseColours() func([]byte) []byte {
	const rule = `*, *::before, *::after {
	  color:#808080 !important; background-color:#808080 !important;
	  border-color:#808080 !important; outline-color:#808080 !important;
	  fill:#808080 !important; stroke:#808080 !important;
	  text-decoration-color:#808080 !important; caret-color:#808080 !important;
	}`
	return func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "</style>", rule+"\n</style>", 1))
	}
}

func TestRendered_EveryDistinctionSurvivesEveryColourForcedToOneValue(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot(), mutate: collapseColours()})

	// Every colour really is one value now, or the rest of this test proves nothing.
	for _, c := range v.Contrast {
		if got, ok := wcagContrast(c.Fg, c.Bg); ok && got > 1.05 {
			t.Fatalf("the colour-collapse counterexample did not collapse %s: %s on %s is still %.2f:1\nbrowser output:\n%s",
				c.What, c.Fg, c.Bg, got, log)
		}
	}
	// And every distinction the page makes is still readable, because none of them was
	// ever made by colour: the labels and counts of a distribution, the three values of a
	// spread, the state beside every dot and chip, and the shapes of the terminal dots.
	for _, g := range []dash9Grader{
		{"every bucket figure states its labels and counts as text", gradeBucketText},
		{"every spread figure states its minimum, mean and maximum as text", gradeSpreadText},
		{"every bar is in proportion to its own count", gradeProportion},
		{"the marks of a spread are told apart by position", gradeSpreadMarks},
		{"every colour carrier is paired with text or shape", gradeCarriers},
	} {
		if probs := g.probe(v); probs != nil {
			t.Errorf("with every colour forced to one value, %s no longer holds: %v", g.name, probs)
		}
	}
}

// --- B11: the 3:1 non-text floor, per graphical element -----------------------------

func TestRendered_EveryGraphicalElementClearsThreeToOne(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot()})

	if probs := gradeContrast(v); probs != nil {
		t.Errorf("a graphical element cannot be told apart from what is drawn behind it: %v\nbrowser output:\n%s", probs, log)
	}
	// The subjects are really there: marks, ticks, dots and boundaries all measured.
	kinds := map[string]int{}
	for _, c := range v.Contrast {
		kinds[strings.Fields(c.What)[0]]++
	}
	for _, want := range []string{"distribution", "spread", "status", "badge", "count", "figure"} {
		if kinds[want] == 0 {
			t.Errorf("no %s element was measured for contrast at all: %+v", want, kinds)
		}
	}
	for _, c := range v.Contrast {
		t.Logf("%-24s %s %s on %s = %.2f:1", c.What, c.Prop, c.Fg, c.Bg, c.Ratio)
	}
}

// --- B12: nothing is fetched from another origin, across every surface a drawing adds --

func TestRendered_TheDrawingsFetchNothingFromAnotherOrigin(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})
	if probs := gradeOffOrigin(v); probs != nil {
		t.Errorf("%v\nbrowser output:\n%s", probs, log)
	}
	// The widened scan must BITE on every surface a vector drawing adds, each of which
	// the old tag/attribute table did not cover.
	for name, mutate := range map[string]func([]byte) []byte{
		"an off-origin vector use reference": domReplace(
			`</main>`, `<svg width="8" height="8"><use href="https://example.invalid/sprite.svg#i"></use></svg></main>`),
		"an off-origin vector image element": domReplace(
			`</main>`, `<svg width="8" height="8"><image href="https://example.invalid/p.png" width="8" height="8"></image></svg></main>`),
		"an off-origin paint server in a rule the cascade applies": cssMutation(
			`.agg .fig .mark { fill:url(https://example.invalid/paint.svg#g); }`),
		"an off-origin filter reference": cssMutation(
			`body { filter:url(https://example.invalid/f.svg#blur); }`),
		"an off-origin mask": cssMutation(
			`.agg .fig { -webkit-mask-image:url(https://example.invalid/m.png); mask-image:url(https://example.invalid/m.png); }`),
		"an off-origin font": cssMutation(
			`@font-face { font-family:x; src:url(https://example.invalid/x.woff2); }`),
		"a data: URI used as an image": cssMutation(
			`.agg { background-image:url(data:image/gif;base64,R0lGODlhAQABAAAAACw=); }`),
		"a data: URI on an image element": domReplace(
			`</main>`, `<img alt="" src="data:image/gif;base64,R0lGODlhAQABAAAAACw="></main>`),
	} {
		v, log := renderDashboard(t, bin, dashOpts{snapshot: fixtureSnapshot(), mutate: mutate, wait: 15 * time.Second})
		if !v.Ready {
			t.Fatalf("%s: the page did not render at all\nbrowser output:\n%s", name, log)
		}
		if probs := gradeOffOrigin(v); probs == nil {
			t.Errorf("the off-origin scan passed a page that reaches off-origin (%s)", name)
		}
	}
}

// cssMutation appends one rule to the served document's own stylesheet.
func cssMutation(rule string) func([]byte) []byte {
	return func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "</style>", rule+"\n</style>", 1))
	}
}

// scriptMutation runs one statement inside the SERVED document, on a short interval, so
// it applies to nodes the page builds from its own snapshot after load. It is how a
// counterexample reaches something the renderer created rather than something the markup
// declared. Inline script is what this page's own policy allows and nothing more.
func scriptMutation(body string) func([]byte) []byte {
	return func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "</body>",
			"<script>setInterval(function(){"+body+"},20);</script></body>", 1))
	}
}

// inlineScriptMutation appends a statement to the page's OWN inline script instead of
// adding a second script element. It exists for the counterexamples aimed at graders that
// COUNT what the document carries: a mutation that added a script element would move the
// very count such a grader compares, and would then "fail" it for a reason the mutation
// itself introduced rather than for the property under test.
func inlineScriptMutation(body string) func([]byte) []byte {
	return func(b []byte) []byte {
		return []byte(strings.Replace(string(b), "\n</script>", "\n"+body+"\n</script>", 1))
	}
}

// --- B13: nothing on a figure needs to be pointed at --------------------------------

func TestRendered_NoFigureIsReadableOnlyByPointingAtIt(t *testing.T) {
	bin := chromium(t)
	// No pointer event is dispatched into the document by this probe, ever.
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})

	if probs := gradeNoTooltip(v); probs != nil {
		t.Errorf("%v\nbrowser output:\n%s", probs, log)
	}
	// And with nothing pointed at, every figure already shows everything it encodes.
	for _, g := range []func(dashVerdict) []string{gradeBucketText, gradeSpreadText} {
		if probs := g(v); probs != nil {
			t.Errorf("with no pointer event dispatched, a figure does not show its values: %v", probs)
		}
	}
}

// --- B14, B15, B16: the unhappy paths ------------------------------------------------

func TestRendered_AnUnavailableOrUnmeasuredFigureDrawsNoMark(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: brokenAggregatesSnapshot()})

	if len(v.Figures) != 6 {
		t.Fatalf("the page rendered %d figures, want all 6 even when figures are missing\nbrowser output:\n%s", len(v.Figures), log)
	}
	for _, f := range v.Figures {
		switch {
		case f.Out:
			// Unavailable: the card stays, says so in words, and draws nothing.
			if f.Drawings != 0 {
				t.Errorf("the unavailable figure %s drew %d mark(s); there is nothing measured to draw", f.Title, f.Drawings)
			}
			if !strings.Contains(strings.ToLower(f.CardText), "unavailable") {
				t.Errorf("the unavailable figure %s does not say so in words: %q", f.Title, f.CardText)
			}
			if probs := shownProblems("the unavailable card "+f.Title, f.Shown); probs != nil {
				t.Errorf("%v", probs)
			}
		case f.Title == "Skips by guard":
			// Available, but no row contributed a value: nothing recorded, and no mark
			// that could be read as a measured zero.
			if f.Drawings != 0 {
				t.Errorf("a figure no row contributed to drew %d mark(s), which would read as a measured zero", f.Drawings)
			}
			if f.ValueText != "not recorded" {
				t.Errorf("a figure no row contributed to shows %q, want %q", f.ValueText, "not recorded")
			}
		case f.Title == "Replacement size":
			// Available and counted, but every end of the scale came over as null: there
			// is no scale to draw, and the three values are stated as unrecorded.
			if f.Drawings != 0 {
				t.Errorf("a spread with no recorded ends drew %d mark(s)", f.Drawings)
			}
			for _, k := range f.Spread {
				if k.Value != "not recorded" {
					t.Errorf("the %s of a figure with no recorded ends reads %q", k.Name, k.Value)
				}
			}
			for _, lie := range []string{"0%", "0.0", "0 ms", "0 B"} {
				if strings.Contains(f.ValueText, lie) {
					t.Errorf("a figure with no recorded ends renders one as %q: %q", lie, f.ValueText)
				}
			}
		case f.Title == "VMAF pooled mean":
			// The one readable figure still draws and still states its values.
			if f.Drawings == 0 {
				t.Error("the one readable figure drew nothing")
			}
			if probs := gradeSpreadText(dashVerdict{Figures: []dashFigure{f}}); probs != nil {
				t.Errorf("the one readable figure does not state its values: %v", probs)
			}
		}
	}
}

// B16: a snapshot with no aggregates object at all, and one whose buckets are absent,
// empty or malformed. Each figure that cannot be read is shown AS unavailable and the
// rest of the page - counts, queue, history and the footer offer - still renders.
func TestRendered_AMalformedAggregatesObjectCostsOnlyItsOwnFigure(t *testing.T) {
	bin := chromium(t)

	for _, c := range []struct {
		name     string
		snapshot []byte
		wantOut  []string
	}{
		{"no aggregates object at all", snapshotWithAggregates("null"),
			[]string{"Outcomes", "Skips by guard", "Replacement size", "Encode time", "VMAF pooled mean", "VMAF worst frame"}},
		{"buckets absent, empty, malformed and hostile", snapshotWithAggregates(malformedAggregates),
			[]string{"Outcomes", "Skips by guard"}},
	} {
		v, log := mustRender(t, bin, dashOpts{snapshot: c.snapshot})
		if len(v.Figures) != 6 {
			t.Fatalf("%s: the page rendered %d figures, want all 6\nbrowser output:\n%s", c.name, len(v.Figures), log)
		}
		out := map[string]bool{}
		for _, f := range v.Figures {
			if f.Out {
				out[f.Title] = true
			}
			if f.Out && f.Drawings != 0 {
				t.Errorf("%s: the unavailable figure %s drew %d mark(s)", c.name, f.Title, f.Drawings)
			}
		}
		for _, title := range c.wantOut {
			if !out[title] {
				t.Errorf("%s: the figure %s was not shown as unavailable", c.name, title)
			}
		}
		// The render did NOT fail: the rest of the page is all still there.
		if len(v.Queue) != 3 || len(v.History) != 2 {
			t.Errorf("%s: a malformed figure cost the page its rows: %d queue, %d history", c.name, len(v.Queue), len(v.History))
		}
		if len(v.Chips) != 7 {
			t.Errorf("%s: a malformed figure cost the page its counts: %d chips", c.name, len(v.Chips))
		}
		if !strings.Contains(v.BodyText, "Corresponding Source") {
			t.Errorf("%s: a malformed figure cost the page the source offer in its footer", c.name)
		}
		if probs := gradeOrder(v); probs != nil {
			t.Errorf("%s: a malformed figure cost the page its shape: %v", c.name, probs)
		}
	}
}

// malformedAggregates is every way a bucket figure can arrive unreadable, plus a spread
// whose ends are text rather than numbers.
const malformedAggregates = `{
  "outcomes": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","window":"",
    "counted":14,"excluded":0,"buckets":null},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":9,"excluded":0,"buckets":[{"key":"hardlinked"},{"count":3}]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":9,"excluded":0,"min":"a lot","mean":"some","max":"most"},
  "encode_ms": {"available":true,"unavailable":"","covers":"every done row that recorded an encode duration","window":"",
    "counted":9,"excluded":0,"min":120000,"mean":900000,"max":5430000},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a pooled mean","window":"",
    "counted":8,"excluded":0,"min":95.1,"mean":97.4,"max":99.2},
  "vmaf_min": {"available":true,"unavailable":"","covers":"every done row that recorded a worst frame","window":"",
    "counted":8,"excluded":0,"min":81.2,"mean":90.6,"max":96.3}
}`

// hostileAggregates carries attacker-influencable text where a bucket key and a guard
// name go: markup, an attribute break-out, a javascript: URL, and a very long path.
var hostileAggregates = `{
  "outcomes": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","window":"",
    "counted":14,"excluded":0,"buckets":[
      {"key":"<img src=x onerror=alert(1)>","count":9},
      {"key":"\"><script>alert(2)</` + `script>","count":3},
      {"key":"javascript:alert(3)","count":2}]},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":3,"excluded":0,"buckets":[
      {"key":"` + strings.Repeat("/very-long-path-segment", 40) + `/f.mkv","count":2},
      {"key":"<svg onload=alert(4)><use href=https://example.invalid/x.svg#i></use></svg>","count":1}]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":9,"excluded":0,"min":0.21,"mean":0.38,"max":0.74},
  "encode_ms": {"available":true,"unavailable":"","covers":"every done row that recorded an encode duration","window":"",
    "counted":9,"excluded":0,"min":120000,"mean":900000,"max":5430000},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a pooled mean","window":"",
    "counted":8,"excluded":0,"min":95.1,"mean":97.4,"max":99.2},
  "vmaf_min": {"available":true,"unavailable":"","covers":"every done row that recorded a worst frame","window":"",
    "counted":8,"excluded":0,"min":81.2,"mean":90.6,"max":96.3}
}`

// snapshotWithAggregates is the healthy fixture with its aggregates object replaced.
func snapshotWithAggregates(aggs string) []byte {
	return []byte(strings.Replace(string(fixtureSnapshot()), healthyAggregates, aggs, 1))
}

// B17: a bucket key is attacker-influencable text. It must arrive on the page as INERT
// TEXT inside the figure's own region, with the browser refusing nothing and the value
// introducing no element that scripts or fetches.
func TestRendered_HostileBucketTextIsInertInsideItsFigure(t *testing.T) {
	bin := chromium(t)

	benign, _ := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot()})
	v, log := mustRender(t, bin, dashOpts{snapshot: []byte(hostileAggregates2())})

	// Nothing was parsed as markup: the same element count as a benign render, no image,
	// no script, no iframe, no event-handler attribute anywhere.
	if v.ElementCount != benign.ElementCount {
		t.Errorf("the hostile bucket keys introduced %d element(s) into the rendered document",
			v.ElementCount-benign.ElementCount)
	}
	// The values are SHOWN, verbatim, inside their own figure.
	for _, want := range []string{
		"<img src=x onerror=alert(1)>",
		`"><script>alert(2)</script>`,
		"javascript:alert(3)",
		"<svg onload=alert(4)><use href=https://example.invalid/x.svg#i></use></svg>",
	} {
		found := false
		for _, f := range v.Figures {
			if strings.Contains(f.CardText, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("the hostile key %q is not rendered as inert text inside a figure", want)
		}
	}
	// The long path is shown too, and its figure still reached the screen.
	long := strings.Repeat("/very-long-path-segment", 40) + "/f.mkv"
	if !strings.Contains(v.BodyText, long[:60]) {
		t.Error("the very long bucket key is not text a reader can see")
	}
	// Nothing it named is fetched, and the browser refused nothing while rendering it.
	if probs := gradeOffOrigin(v); probs != nil {
		t.Errorf("a hostile bucket key introduced a fetch: %v", probs)
	}
	if refusals := policyRefusals(log); len(refusals) != 0 {
		t.Errorf("the browser refused something while rendering hostile bucket text:\n%s", strings.Join(refusals, "\n"))
	}
	// And the figures around it are unharmed.
	for _, g := range []func(dashVerdict) []string{gradeBucketText, gradeSpreadText, gradeProportion, gradeOrder} {
		if probs := g(v); probs != nil {
			t.Errorf("a hostile bucket key cost the page a property: %v", probs)
		}
	}
}

func hostileAggregates2() string {
	return strings.Replace(string(fixtureSnapshot()), healthyAggregates, hostileAggregates, 1)
}

// B18: the stream fails after the first snapshot. The drawings and their text equivalents
// stay on screen, and the connection is reported as down in words.
func TestRendered_AFailedStreamKeepsTheDrawingsAndTheirText(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{
		snapshot: fixtureSnapshot(), streamFails: true, mode: "down", wait: 30 * time.Second})

	for _, g := range []dash9Grader{
		{"every drawing reached the screen", gradeDrawingsShown},
		{"every bucket figure states its labels and counts as text", gradeBucketText},
		{"every spread figure states its minimum, mean and maximum as text", gradeSpreadText},
		{"every bar is in proportion to its own count", gradeProportion},
	} {
		if probs := g.probe(v); probs != nil {
			t.Errorf("after the stream failed, %s no longer holds: %v\nbrowser output:\n%s", g.name, probs, log)
		}
	}
	// The connection is reported in WORDS, not by colour alone.
	if strings.TrimSpace(v.ConnText) == "" || v.ConnText == "live" {
		t.Errorf("the page reports its connection as %q after the stream failed", v.ConnText)
	}
	if !strings.Contains(v.BodyText, v.ConnText) {
		t.Errorf("the connection state %q is in the DOM but not in the text a reader can see", v.ConnText)
	}
	if probs := gradeCarriers(v); probs != nil {
		t.Errorf("after the stream failed, something says what it means in colour alone: %v", probs)
	}
}

// --- B23: every grader above FAILS against the mutation that would defeat it ---------

// A grader that cannot fail is not evidence, and this repository has already lost a whole
// spec to exactly that. Each row below serves the REAL document with one deliberate change
// that defeats one named property, and requires that property's grader to report it.
func TestRendered_EveryDASH9GraderFailsAgainstItsOwnMutation(t *testing.T) {
	bin := chromium(t)

	// First: every grader is clean on the shipped document, so a report below is a signal
	// rather than the baseline.
	base, log := mustRender(t, bin, dashOpts{snapshot: mixedSnapshot()})
	for _, g := range dash9Graders() {
		if probs := g.probe(base); probs != nil {
			t.Fatalf("the shipped document already fails %q: %v\nbrowser output:\n%s", g.name, probs, log)
		}
	}

	plain := servedDocument(t)
	for _, c := range []struct {
		name    string
		mutate  func([]byte) []byte
		defeats string
	}{
		{"the drawings hidden by a rule naming their own class",
			cssMutation(`.agg .fig { display:none; }`), "every drawing reached the screen"},
		{"the drawings collapsed to no box at all",
			cssMutation(`.agg .fig { position:absolute; width:0; height:0; overflow:hidden; }`),
			"every drawing reached the screen"},
		{"a distribution's labels and counts stripped",
			cssMutation(`.buckets .bk, .buckets .bc { display:none; }`),
			"every bucket figure states its labels and counts as text"},
		{"a spread's three values stripped",
			cssMutation(`.spreadkeys { display:none; }`),
			"every spread figure states its minimum, mean and maximum as text"},
		{"every bar forced to one length",
			cssMutation(`.agg .fig.bar .mark { width:100% !important; }`),
			"every bar is in proportion to its own count"},
		{"every bar given the length of the count above it",
			scriptMutation(`var b=document.querySelectorAll(".fig.bar .mark");` +
				`for (var i=0;i<b.length;i++) b[i].setAttribute("width","62%");`),
			"every bar is in proportion to its own count"},
		{"a spread's three ticks moved onto one position",
			scriptMutation(`var t=document.querySelectorAll(".fig.spread .tick");` +
				`for (var i=0;i<t.length;i++){t[i].setAttribute("x1","500");t[i].setAttribute("x2","500");}`),
			"the marks of a spread are told apart by position"},
		// Collapsed onto the SURFACE TOKEN rather than onto a hard-coded hex, so the
		// counterexample stays a counterexample in whichever theme the engine is in.
		{"a mark's colour collapsed onto the card face behind it",
			cssMutation(`.agg .fig .mark, .agg .fig .axis, .agg .fig .tick { fill:var(--panel) !important; stroke:var(--panel) !important; }`),
			"every graphical element clears 3:1 against what is behind it"},
		{"a status dot's colour collapsed onto the page behind it",
			cssMutation(`td.st .dot { background:var(--bg) !important; }`),
			"every graphical element clears 3:1 against what is behind it"},
		{"the status word hidden beside its dot",
			cssMutation(`td.st .stlabel { display:none; }`),
			"every colour carrier is paired with text or shape"},
		{"a count chip's key hidden beside its number",
			cssMutation(`#chips .chip .k { display:none; }`),
			"every colour carrier is paired with text or shape"},
		{"the terminal dots all given the same shape",
			cssMutation(`td.st .dot { border-radius:50% !important; transform:none !important; }`),
			"every colour carrier is paired with text or shape"},
		{"the history region painted above the current run",
			cssMutation(`main { display:flex; flex-direction:column-reverse; }`),
			"the current run is presented before the history"},
		{"the whole-ledger figures moved into the current-run region",
			domReplace(
				aggHostMarkup, ``,
				`<div class="chips" id="chips"></div>`,
				`<div class="chips" id="chips"></div>`+aggHostMarkup),
			"the current run is presented before the history"},
		{"an off-origin reference injected into a drawing",
			cssMutation(`.agg .fig .mark { fill:url(https://example.invalid/paint.svg#g); }`),
			"nothing is fetched from another origin"},
		{"a value put behind a tooltip",
			scriptMutation(`var a=document.querySelectorAll("#aggregates .agg *");` +
				`for (var i=0;i<a.length;i++) a[i].setAttribute("title","hover to read me");`),
			"no figure is readable only by pointing at it"},
	} {
		if string(c.mutate([]byte(plain))) == plain {
			t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", c.name)
		}
		v, log := renderDashboard(t, bin, dashOpts{snapshot: mixedSnapshot(), mutate: c.mutate, wait: 15 * time.Second})
		if !v.Ready {
			t.Fatalf("%s: the page did not render at all\nbrowser output:\n%s", c.name, log)
		}
		found := false
		for _, g := range dash9Graders() {
			if g.name != c.defeats {
				continue
			}
			found = true
			if probs := g.probe(v); probs == nil {
				t.Errorf("the grader %q PASSED a document mutated to defeat it (%s)\nverdict: %+v\nbrowser output:\n%s",
					g.name, c.name, v, log)
			} else {
				t.Logf("%-58s defeats %-58s -> %s", c.name, c.defeats, probs[0])
			}
		}
		if !found {
			t.Fatalf("the mutation %q names a grader that does not exist: %q", c.name, c.defeats)
		}
	}
}

// And the colour-collapse reading is proved to bite too: with every colour forced to one
// value AND the text equivalents stripped, the same graders that pass above must fail.
func TestRendered_TheColourCollapseReadingFailsWhenTheTextIsStripped(t *testing.T) {
	bin := chromium(t)
	both := func(b []byte) []byte {
		return cssMutation(`.buckets .bk, .buckets .bc, .spreadkeys, td.st .stlabel, #chips .chip .k { display:none; }`)(
			collapseColours()(b))
	}
	v, log := renderDashboard(t, bin, dashOpts{snapshot: mixedSnapshot(), mutate: both, wait: 15 * time.Second})
	if !v.Ready {
		t.Fatalf("the page did not render at all\nbrowser output:\n%s", log)
	}
	for _, g := range []dash9Grader{
		{"every bucket figure states its labels and counts as text", gradeBucketText},
		{"every spread figure states its minimum, mean and maximum as text", gradeSpreadText},
		{"every colour carrier is paired with text or shape", gradeCarriers},
	} {
		if probs := g.probe(v); probs == nil {
			t.Errorf("with every colour collapsed AND the text stripped, %q still passed, so it cannot fail", g.name)
		}
	}
}

// --- LEDGER-5: the total a capped table was capped AGAINST --------------------------
//
// Two criteria, both about what the page SHOWS, so both are decided by loading the served
// document in the browser and reading the rendered notice - never by matching the module
// source, which cannot tell a figure that was rendered from one that was merely computed.
//
//	16. a capped table whose response carries a total shows the REPORTED total, not one
//	    the page derived;
//	17. a response with no readable total tells the reader the total is unavailable and
//	    shows NO figure in its place.
//
// The first is only gradeable because the fixture below makes the two answers DIFFER: a
// reported total that no arithmetic over this payload produces. Held equal, every grader
// here would pass a page that still derived its own.

// divergentBothTotalsSnapshot reports 613 queue rows and 41,237 terminal ones while the
// summary in the same frame rolls up to 7 and 14 - the two numbers the page used to print.
// A page deriving its own total shows the latter, which is exactly the defect to catch.
func divergentBothTotalsSnapshot() []byte {
	s := strings.Replace(string(fixtureSnapshot()), `"cap":500,"count":7}`, `"cap":500,"count":613}`, 1)
	return []byte(strings.Replace(s, `"cap":200,"count":14}`, `"cap":200,"count":41237}`, 1))
}

// unreadableTotalsSnapshot is the server saying it could not read either total: available
// false, count an explicit null, and the same fixed statement the aggregates use. The rows
// are all still there, which is the point - one unreadable figure never costs an operator
// the rows they can otherwise see.
func unreadableTotalsSnapshot() []byte {
	s := string(fixtureSnapshot())
	s = strings.Replace(s,
		`"queue_total": {"available":true,"unavailable":"","covers":"every pending or active row in the ledger","cap":500,"count":7}`,
		`"queue_total": {"available":false,"unavailable":"this figure could not be read from the ledger","covers":"every pending or active row in the ledger","cap":500,"count":null}`, 1)
	s = strings.Replace(s,
		`"history_total": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","cap":200,"count":14}`,
		`"history_total": {"available":false,"unavailable":"this figure could not be read from the ledger","covers":"every terminal row in the ledger","cap":200,"count":null}`, 1)
	return []byte(s)
}

var capDigit = regexp.MustCompile(`[0-9]`)

// gradeReportedCapTotal is criterion 16. Each notice must be SHOWN, must carry the
// reported total, and must not carry the figure this payload's own summary rolls up to -
// which is the whole of what "rather than one the page derived" asserts.
func gradeReportedCapTotal(v dashVerdict) []string {
	if !v.Ready {
		return []string{"the page never rendered a snapshot"}
	}
	var out []string
	for _, c := range []struct {
		what     string
		cap      dashCap
		reported string // what the server reported, as a reader sees it
		derived  string // what this payload's summary rolls up to
	}{
		{"the queue cap notice", v.QueueCap, "613", "7"},
		{"the history cap notice", v.HistCap, "41,237", "14"},
	} {
		out = append(out, shownProblems(c.what, c.cap.Shown)...)
		if !strings.Contains(c.cap.Text, "of "+c.reported) {
			out = append(out, fmt.Sprintf("%s reads %q; it must carry the reported total %s", c.what, c.cap.Text, c.reported))
		}
		if strings.Contains(c.cap.Text, "of "+c.derived) {
			out = append(out, fmt.Sprintf("%s reads %q, which is the total the PAGE derives from this snapshot (%s), not the one the server reported",
				c.what, c.cap.Text, c.derived))
		}
		if c.cap.Text == "" || !strings.Contains(v.BodyText, c.cap.Text) {
			out = append(out, fmt.Sprintf("%s is not in the text a reader can see: %q", c.what, c.cap.Text))
		}
	}
	return out
}

// gradeUnavailableCapTotal is criterion 17: told in words, with NO figure standing in for
// the total. The digit test is the sharp end - a number beside the word "capped" reads AS
// the total whatever the sentence around it says, so an unreadable one puts no digit on
// the screen at all.
func gradeUnavailableCapTotal(v dashVerdict) []string {
	if !v.Ready {
		return []string{"the page never rendered a snapshot"}
	}
	var out []string
	for _, c := range []struct {
		what string
		cap  dashCap
	}{
		{"the queue cap notice", v.QueueCap},
		{"the history cap notice", v.HistCap},
	} {
		out = append(out, shownProblems(c.what, c.cap.Shown)...)
		if !strings.Contains(strings.ToLower(c.cap.Text), "unavailable") {
			out = append(out, fmt.Sprintf("%s does not tell the reader the total is unavailable: %q", c.what, c.cap.Text))
		}
		if capDigit.MatchString(c.cap.Text) {
			out = append(out, fmt.Sprintf("%s shows a figure where the unreadable total belongs: %q", c.what, c.cap.Text))
		}
		if c.cap.Text == "" || !strings.Contains(v.BodyText, c.cap.Text) {
			out = append(out, fmt.Sprintf("%s is not in the text a reader can see: %q", c.what, c.cap.Text))
		}
	}
	return out
}

func TestRendered_ACappedTableShowsTheTotalTheServerReportedNotOneThePageDerived(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: divergentBothTotalsSnapshot()})

	if probs := gradeReportedCapTotal(v); probs != nil {
		t.Errorf("%v\nqueue notice: %q\nhistory notice: %q\nbrowser output:\n%s",
			probs, v.QueueCap.Text, v.HistCap.Text, log)
	}
	// The notice is a statement about the ledger, never a filter on what was drawn.
	if len(v.Queue) != 3 || len(v.History) != 2 {
		t.Errorf("the reported totals changed what was drawn: %d queue rows and %d history rows",
			len(v.Queue), len(v.History))
	}
}

func TestRendered_AnUnreadableCapTotalIsShownAsUnavailableWithNoFigureInItsPlace(t *testing.T) {
	bin := chromium(t)
	v, log := mustRender(t, bin, dashOpts{snapshot: unreadableTotalsSnapshot()})

	if probs := gradeUnavailableCapTotal(v); probs != nil {
		t.Errorf("%v\nqueue notice: %q\nhistory notice: %q\nbrowser output:\n%s",
			probs, v.QueueCap.Text, v.HistCap.Text, log)
	}
	// One unreadable figure costs the operator nothing else: every row still drew, and so
	// did every aggregate card.
	if len(v.Queue) != 3 || len(v.History) != 2 {
		t.Errorf("an unreadable total cost the reader rows: %d queue rows and %d history rows",
			len(v.Queue), len(v.History))
	}
	if len(v.Aggregates) != 6 {
		t.Errorf("an unreadable total cost the reader %d of the 6 aggregate cards", 6-len(v.Aggregates))
	}
}

// A grader that cannot fail is not evidence. Each mutation below breaks exactly the
// property its grader asserts, inside the SERVED document, and the grader must report it.
func TestRendered_EveryCapTotalGraderFailsAgainstItsOwnMutation(t *testing.T) {
	bin := chromium(t)
	plain := servedDocument(t)

	const unavailableDecl = "const CAP_TOTAL_UNAVAILABLE =\n  \"The total behind this view is unavailable, so whether it is capped cannot be shown.\";"

	for _, c := range []struct {
		name     string
		snapshot []byte
		mutate   func([]byte) []byte
		grade    func(dashVerdict) []string
	}{
		{
			// The page derives the total from the summary counts again - the exact
			// behaviour LEDGER-5 replaced, and the one no source scan can see.
			name:     "the page derives the total from the summary instead of reading the reported one",
			snapshot: divergentBothTotalsSnapshot(),
			mutate: domReplace(
				`capNote("queue-cap", q.length, snap.queue_total);`,
				`capNote("queue-cap", q.length, {available:true,count:(snap.summary.pending||0)+(snap.summary.probing||0)+(snap.summary.encoding||0)+(snap.summary.verifying||0)});`,
				`capNote("hist-cap", h.length, snap.history_total);`,
				`capNote("hist-cap", h.length, {available:true,count:(snap.summary.done||0)+(snap.summary.skipped||0)+(snap.summary.failed||0)});`),
			grade: gradeReportedCapTotal,
		},
		{
			// Built with the reported total and then hidden, so no reader ever sees it.
			name:     "the notices carry the reported total and are hidden from the reader",
			snapshot: divergentBothTotalsSnapshot(),
			mutate:   cssMutation(`.note.cap { display:none; }`),
			grade:    gradeReportedCapTotal,
		},
		{
			// An unreadable total rendered as a figure: what criterion 17 forbids.
			name:     "an unreadable total is shown as a number anyway",
			snapshot: unreadableTotalsSnapshot(),
			mutate: domReplace(unavailableDecl,
				`const CAP_TOTAL_UNAVAILABLE = "Showing the most recent 0 of 0 - this view is capped.";`),
			grade: gradeUnavailableCapTotal,
		},
		{
			// An unreadable total rendered as nothing at all: indistinguishable from a
			// view that is not capped, which is the other half of criterion 17.
			name:     "an unreadable total says nothing at all",
			snapshot: unreadableTotalsSnapshot(),
			mutate:   domReplace(unavailableDecl, `const CAP_TOTAL_UNAVAILABLE = "";`),
			grade:    gradeUnavailableCapTotal,
		},
	} {
		if string(c.mutate([]byte(plain))) == plain {
			t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", c.name)
		}
		v, log := renderDashboard(t, bin, dashOpts{snapshot: c.snapshot, mutate: c.mutate, wait: 10 * time.Second})
		if probs := c.grade(v); probs == nil {
			t.Errorf("the grader passed a page that breaks the property it asserts (%s)\nqueue notice: %q\nhistory notice: %q\nbrowser output:\n%s",
				c.name, v.QueueCap.Text, v.HistCap.Text, log)
		}
	}
}

// --- S0053: hostile free text, and where the methodology went ------------------------

// Clause F11's second half: the three places the wire carries free text a reader sees are
// a media path, a failure reason and a bucket label. Each is rendered as INERT TEXT - it
// introduces no element, no attribute and no handler, and the browser refuses nothing
// while rendering it.
//
// It is driven over the DevTools protocol rather than through the iframe harness for one
// reason that matters to this criterion: the document is loaded at the TOP LEVEL, so the
// response's own Content-Security-Policy governs it exactly as it governs a reader's
// page, and the engine's report of every refusal is read straight off its log.
// domCounts is what the hostile value must not move: the elements, scripts, media
// elements and event-handler attributes the rendered document carries.
type domCounts struct {
	Elements int `json:"elements"`
	Scripts  int `json:"scripts"`
	Media    int `json:"media"`
	Handlers int `json:"handlers"`
}

const domCountsJS = `(function(){ return {
  elements: document.getElementsByTagName("*").length,
  scripts: document.getElementsByTagName("script").length,
  media: document.querySelectorAll("img, iframe, object, embed, svg image, use").length,
  handlers: document.querySelectorAll("[onerror],[onload],[onclick],[onmouseover],[onfocus],[onanimationend]").length
}; })()`

// gradeHostileTextIsInert decides clause F11's second half: the hostile values are SHOWN,
// as text a reader can see, and nothing came with them - no element, no attribute, no
// handler, no policy refusal - and the page showing them still meets every convention.
func gradeHostileTextIsInert(base, got domCounts, s convSnapshot, refusals []string) []string {
	var out []string
	// The values are on the screen, as TEXT a reader can see.
	for _, want := range []string{"onerror=alert(1)", "onmouseover=", "<script>alert(2)</script>"} {
		if !strings.Contains(s.BodyText, want) {
			out = append(out, fmt.Sprintf("the hostile value %q is not in the text a reader can see; it must be shown, inert, not swallowed", want))
		}
	}
	// And nothing came with them.
	if got.Scripts != base.Scripts {
		out = append(out, fmt.Sprintf("hostile text changed the script count from %d to %d", base.Scripts, got.Scripts))
	}
	if got.Media != 0 || base.Media != 0 {
		out = append(out, fmt.Sprintf("hostile text put %d media elements on the page (the clean page has %d, and both must be 0)", got.Media, base.Media))
	}
	if got.Handlers != 0 || base.Handlers != 0 {
		out = append(out, fmt.Sprintf("hostile text put %d event-handler attributes on the page (the clean page has %d, and both must be 0)",
			got.Handlers, base.Handlers))
	}
	out = append(out, gradeNoPolicyRefusal("rendering hostile text", refusals)...)
	// The whole convention set still holds on the page showing it.
	for _, g := range convGraders() {
		if g.name == "wide content scrolls inside its own container" {
			continue
		}
		for _, prob := range g.probe(s) {
			out = append(out, fmt.Sprintf("with hostile text on the page, %s: %s", g.name, prob))
		}
	}
	return out
}

// readHostileTextPair renders the shipped page twice - once with ordinary data, for the
// counts a hostile value must not move, and once with hostile text in all three places at
// once. mutate, when non-nil, is applied to the hostile document only, which is how this
// grader's counterexample is built.
func readHostileTextPair(t *testing.T, b *cdpBrowser, p *cdpPage, mutate func([]byte) []byte) (base, got domCounts, s convSnapshot, refusals []string) {
	t.Helper()
	clean := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})
	p.loadConventions(t, clean.url, convOpts{theme: "light"})
	p.mustEval(domCountsJS, &base)

	hostile := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: hostileSnapshot(), mutate: mutate})
	mark := b.logMark()
	p.loadConventions(t, hostile.url, convOpts{theme: "light"})
	p.mustEval(domCountsJS, &got)
	s = p.collect(t)
	return base, got, s, b.securityRefusals(mark)
}

func TestRendered_HostilePathReasonAndBucketAreRenderedAsInertText(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	base, got, s, refusals := readHostileTextPair(t, b, p, nil)
	for _, prob := range gradeHostileTextIsInert(base, got, s, refusals) {
		t.Error(prob)
	}
}

// Clause F8, both halves: each REGION renders exactly one link to this repository's own
// documentation for that region's methodology, and every claim taken off the surface is
// in the document that link names. The first half is decided in the browser, on the
// rendered document; the second is a read of the committed document, because "the claim
// is in the doc" is not a rendered property of the dashboard.
func TestRendered_EachRegionLinksItsMethodologyOnceAndEveryMovedClaimIsInThatDocument(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})
	s := p.collect(t)

	if probs := gradeDocLinks(s); probs != nil {
		for _, prob := range probs {
			t.Error(prob)
		}
	}
	if len(s.Doclinks) != 2 {
		t.Fatalf("the page rendered %d regions, want the two the dashboard has", len(s.Doclinks))
	}
	// Each region's link names the section of the document that carries ITS methodology,
	// not merely the document.
	wantFragment := map[string]string{
		"region-now":     "#right-now",
		"region-history": "#what-it-has-done-to-your-library",
	}
	for _, r := range s.Doclinks {
		if r.Count != 1 {
			continue
		}
		want, ok := wantFragment[r.Region]
		if !ok {
			t.Errorf("the page rendered an unexpected region %q", r.Region)
			continue
		}
		if !strings.HasSuffix(r.Hrefs[0], want) {
			t.Errorf("the region %s links %q, which does not name its own section (%s)", r.Region, r.Hrefs[0], want)
		}
		if !strings.Contains(r.Hrefs[0], DocPath) {
			t.Errorf("the region %s links %q, which does not name %s", r.Region, r.Hrefs[0], DocPath)
		}
	}

	// And the claims that came off the surface are in the document those links name.
	doc := flattenText(readRepoDoc(t, DocPath))
	surface := flattenText(s.BodyText)
	for _, claim := range movedClaims {
		flat := flattenText(claim)
		if !strings.Contains(doc, flat) {
			t.Errorf("the claim %q left the dashboard and is not in %s; F8 moves claims, it does not drop them", claim, DocPath)
		}
		if strings.Contains(surface, flat) {
			t.Errorf("the claim %q is still printed on the surface; F8 puts the paragraphs in the docs", claim)
		}
	}
}

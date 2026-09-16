// The measuring script for the frontend-convention graders.
//
// It runs against the SERVED document loaded at the top level - not inside an iframe - so
// the response headers, the Content-Security-Policy and the media the engine emulates all
// apply to the document under measurement exactly as they would to a reader's. Everything
// it reports is something the ENGINE computed: the resolved value of a custom property
// after the cascade and the media queries, a real layout box, the colour actually painted
// behind a run of text, the font the layout engine actually used, and the text innerText
// says a reader can see.
//
// Nothing here decides anything. Every predicate lives in graders.mjs, so each one can be
// run against a document deliberately mutated to defeat it - which is the only way to know
// a grader can fail at all.
//
// THIS FILE IS THE MEASUREMENT LAYER AND IT MOVED VERBATIM. It was a Go string literal
// driven over the DevTools protocol by hand; it is the same script, evaluated by a runner
// that already speaks to the engine. Carrying it across unchanged is deliberate: a rewrite
// of the thing that does the measuring would have put every grader's verdict in doubt at
// the same moment, with nothing left to check it against.

export const probeSource = String.raw`
window.__hf = (function () {

// --- colour arithmetic, WCAG 2.2's own definitions --------------------------------
function parseColor(s) {
  const m = /^rgba?\(([^)]*)\)$/.exec(String(s).trim());
  if (!m) return null;
  const p = m[1].split(/[,\s\/]+/).filter(function (x) { return x !== ""; }).map(Number);
  if (p.length < 3) return null;
  for (const v of p) { if (!isFinite(v)) return null; }
  return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
}
function rgbText(c) { return "rgb(" + c.r + ", " + c.g + ", " + c.b + ")"; }

// behind is the colour actually PAINTED behind an element: the nearest ancestor, itself
// included, that paints an opaque background. Text sits on its own element's background
// when it has one, which is why the walk starts at the element rather than at its parent.
function behind(el) {
  for (let n = el; n; n = n.parentElement) {
    const c = parseColor(getComputedStyle(n).backgroundColor);
    if (c && c.a >= 0.999) return c;
  }
  const root = parseColor(getComputedStyle(document.documentElement).backgroundColor);
  return root && root.a >= 0.999 ? root : { r: 255, g: 255, b: 255, a: 1 };
}

// --- what reached the screen -------------------------------------------------------
function isRendered(el) {
  if (!el) return false;
  const r = el.getBoundingClientRect();
  if (r.width <= 0 || r.height <= 0) return false;
  for (let n = el; n; n = n.parentElement) {
    const cs = getComputedStyle(n);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.visibility === "collapse") return false;
    if (cs.opacity === "0") return false;
    if (n.hasAttribute("hidden")) return false;
  }
  return true;
}
function visText(el) { return el ? String(el.innerText || "").trim() : ""; }

// where names an element well enough for a failure message to be actionable.
function where(el) {
  if (!el) return "(none)";
  let s = el.tagName.toLowerCase();
  if (el.id) s += "#" + el.id;
  const cls = (el.getAttribute("class") || "").trim();
  if (cls) s += "." + cls.split(/\s+/).join(".");
  return s;
}

// ownText is the text a element carries DIRECTLY, as opposed to through a descendant.
// A contrast reading belongs to the element that paints the glyphs, and an ancestor
// would otherwise be read against a colour none of its own text is drawn in.
function ownText(el) {
  let s = "";
  for (const n of el.childNodes) if (n.nodeType === 3) s += n.nodeValue;
  return s.trim();
}

// --- the collectors ----------------------------------------------------------------

const ROLE_TOKENS = ["--bg","--panel","--line","--fg","--muted","--accent","--ok","--warn",
  "--bad","--border","--mark","--focus","--disabled","--selected","--link"];

// tokens is every custom property the engine RESOLVED on the root after the whole
// cascade, including whichever theme's media query is in force. It is how the theme
// under test is read back: the values are the engine's, never the grader's.
function tokens() {
  const cs = getComputedStyle(document.documentElement);
  const out = {};
  for (const sheet of document.styleSheets) {
    let rules = null;
    try { rules = sheet.cssRules; } catch (e) { continue; }
    for (const rule of rules) collectNames(rule, out);
  }
  function collectNames(rule, acc) {
    if (rule.style) {
      for (let i = 0; i < rule.style.length; i++) {
        const p = rule.style[i];
        if (p.indexOf("--") === 0) acc[p] = true;
      }
    }
    if (rule.cssRules) for (const r of rule.cssRules) collectNames(r, acc);
  }
  const resolved = {};
  for (const name of Object.keys(out).concat(ROLE_TOKENS)) {
    resolved[name] = cs.getPropertyValue(name).trim();
  }
  return { resolved: resolved, colorScheme: cs.colorScheme,
    bodyBackground: getComputedStyle(document.body).backgroundColor,
    bodyColor: getComputedStyle(document.body).color };
}

// textRuns is every run of text a reader can SEE, with the colour it is painted in, the
// colour actually painted behind it, and the size and weight that decide which WCAG
// floor applies to it.
function textRuns() {
  const out = [];
  for (const el of document.body.querySelectorAll("*")) {
    const t = ownText(el);
    if (t === "") continue;
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    const fg = parseColor(cs.color);
    if (!fg) continue;
    const bg = behind(el);
    out.push({ what: where(el), text: t.slice(0, 80),
      fg: rgbText(fg), fgAlpha: fg.a, bg: rgbText(bg),
      size: parseFloat(cs.fontSize) || 0, weight: parseInt(cs.fontWeight, 10) || 400 });
  }
  return out;
}

// proseWords counts the words a link's own block carries BESIDES the link. A link inside
// a sentence of prose is outside the pointer-target rule; a standalone link is not.
function proseWords(a) {
  let block = a.parentElement;
  while (block && getComputedStyle(block).display.indexOf("inline") === 0) block = block.parentElement;
  if (!block) return 0;
  const all = (block.innerText || "");
  const mine = (a.innerText || "");
  const rest = all.replace(mine, " ");
  return rest.split(/\s+/).filter(function (w) { return /[A-Za-z0-9]/.test(w); }).length;
}

// pointerTargets is every control a reader points at, with the box the engine painted it
// in. A link inside a sentence of prose is reported and marked, not dropped, so the Go
// side decides the exemption rather than this script hiding it.
function pointerTargets() {
  const out = [];
  for (const el of document.querySelectorAll("button, input, select, textarea, a[href]")) {
    if (!isRendered(el)) continue;
    const r = el.getBoundingClientRect();
    out.push({ what: where(el), tag: el.tagName.toLowerCase(),
      w: r.width, h: r.height,
      inProse: el.tagName.toLowerCase() === "a" && proseWords(el) >= 3,
      disabled: !!el.disabled });
  }
  return out;
}

// focusables is every interactive control the page offers, in DOM order, each with the
// computed style it wears while NOT focused. The driver then tabs through them for real
// and reads each one again while it holds focus.
function focusables() {
  const out = [];
  const all = Array.prototype.slice.call(document.querySelectorAll("*"));
  for (const el of document.querySelectorAll('a[href], button, input, select, textarea, [tabindex]:not([tabindex="-1"])')) {
    if (!isRendered(el) || el.disabled) continue;
    const cs = getComputedStyle(el);
    out.push({ index: all.indexOf(el), what: where(el),
      outline: cs.outlineStyle + " " + cs.outlineWidth + " " + cs.outlineColor,
      boxShadow: cs.boxShadow, border: cs.borderTopColor + " " + cs.borderTopStyle,
      background: cs.backgroundColor });
  }
  return out;
}

// focused reports the element that currently holds focus, the indicator it draws, and the
// colour behind the indicator, so the Go side can measure the indicator's own contrast.
function focused() {
  const el = document.activeElement;
  if (!el || el === document.body || el === document.documentElement) return { none: true };
  const all = Array.prototype.slice.call(document.querySelectorAll("*"));
  const cs = getComputedStyle(el);
  const oc = parseColor(cs.outlineColor);
  return {
    none: false, index: all.indexOf(el), what: where(el),
    focusVisible: el.matches(":focus-visible"),
    outline: cs.outlineStyle + " " + cs.outlineWidth + " " + cs.outlineColor,
    outlineColor: oc ? rgbText(oc) : "",
    outlineWidth: parseFloat(cs.outlineWidth) || 0,
    outlineStyle: cs.outlineStyle,
    outlineOffset: parseFloat(cs.outlineOffset) || 0,
    boxShadow: cs.boxShadow, border: cs.borderTopColor + " " + cs.borderTopStyle,
    background: cs.backgroundColor,
    behindIndicator: rgbText(behind(el.parentElement || document.body)),
    ownBackground: rgbText(behind(el))
  };
}

// views is each view's own answer to "which of the three states am I in" (F7), read from
// the rendered document: the state element's data-state, the words it shows, and whether
// the view is showing content of its own.
function views() {
  const out = [];
  for (const host of document.querySelectorAll("[data-view]")) {
    const el = host.querySelector("[data-state]");
    let content = 0;
    for (const child of host.children) if (!(child.dataset && child.dataset.state)) content++;
    if (host.id === "counts") {
      const chips = document.getElementById("chips");
      content = chips ? chips.children.length : 0;
    }
    out.push({ view: host.dataset.view, state: el ? el.dataset.state : "",
      text: el ? visText(el) : "", shown: el ? isRendered(el) : false, content: content });
  }
  return out;
}

// SNAPSHOT_VIEWS are the views a SNAPSHOT fills. Every claim of the form "before its first
// snapshot every view is loading", "a snapshot carrying no rows leaves every view empty",
// "a payload that cannot be read leaves every view saying so" is a claim about THESE, and
// about nothing else: each of those triggers is a fact about the snapshot.
//
// The ledger search's results view is deliberately not one of them. It is filled by a
// SEARCH an operator asks for, so a snapshot carrying no rows leaves it exactly where it
// was, and requiring it to answer a question about the snapshot would be requiring the page
// to describe work it is not doing. Its own three states are driven by real searches.
const SNAPSHOT_VIEWS = ["counts", "queue", "aggs", "history"];

function snapshotViews() {
  return views().filter(function (v) { return SNAPSHOT_VIEWS.indexOf(v.view) >= 0; });
}

// LABEL_SELECTOR is the page's explanatory and scope labels: every element whose job is
// to say what a region, a figure or a control IS. It deliberately excludes the two kinds
// of long text that are not labels: a failure reason, which is the server's own error
// text and is data, and the footer, which carries the product identity and the AGPL
// section 13 offer - text that is required verbatim and cannot be shortened.
const LABEL_SELECTOR = ".scope, .state, .lbl, .note, .docs, .doclink, .reclaimed, " +
  ".agg-cov, .agg-ex, .cond, .empty, .range, .of, .agg-k, .buckets .bk, .spreadkeys .sn";

function labels() {
  const out = [];
  for (const el of document.querySelectorAll(LABEL_SELECTOR)) {
    if (!isRendered(el)) continue;
    const t = visText(el);
    const words = t.split(/\s+/).filter(function (w) { return /[A-Za-z0-9]/.test(w); });
    out.push({ what: where(el), text: t, words: words.length });
  }
  return out;
}

// mainParagraphs is every paragraph inside the page's own regions, with whether it is in
// the graded label set. It exists so a long explanation cannot be added to a region by
// giving it a class the word bound does not cover.
function mainParagraphs() {
  const out = [];
  const main = document.querySelector("main");
  if (!main) return out;
  for (const p of main.querySelectorAll("p")) {
    if (!isRendered(p)) continue;
    out.push({ what: where(p), graded: p.matches(LABEL_SELECTOR), text: visText(p).slice(0, 120) });
  }
  return out;
}

// doclinks is the documentation link each region carries (F8), reported per region so
// the "exactly one" is decided over the rendered document rather than over the markup.
function doclinks() {
  const out = [];
  for (const section of document.querySelectorAll("main section")) {
    const links = section.querySelectorAll("a.doclink");
    const rec = { region: section.id, count: links.length, hrefs: [], texts: [], names: [], shown: [] };
    for (const a of links) {
      rec.hrefs.push(a.getAttribute("href"));
      rec.texts.push(visText(a));
      // A link carries a NAME however it carries one: its own text, or a label on the
      // element when the link is a mark. A link with none announces itself as "link".
      rec.names.push(a.getAttribute("aria-label") || a.getAttribute("title") || visText(a));
      rec.shown.push(isRendered(a));
    }
    out.push(rec);
  }
  return out;
}

// layout is the page's own horizontal overflow, and every container that takes a scroll
// of its own (F9).
function layout() {
  const de = document.documentElement;
  const scrollers = [];
  for (const el of document.querySelectorAll("*")) {
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    if (cs.overflowX !== "auto" && cs.overflowX !== "scroll") continue;
    scrollers.push({ what: where(el), scrollWidth: el.scrollWidth, clientWidth: el.clientWidth });
  }
  // Every element whose painted box sticks out past the viewport, which is what a body
  // scroll is made of and what names the culprit when there is one.
  const overflowing = [];
  for (const el of document.querySelectorAll("body *")) {
    if (!isRendered(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.right > window.innerWidth + 1) {
      overflowing.push({ what: where(el), right: r.right, width: r.width });
    }
  }
  return { innerWidth: window.innerWidth,
    bodyScrollWidth: document.body.scrollWidth, bodyClientWidth: document.body.clientWidth,
    docScrollWidth: de.scrollWidth, docClientWidth: de.clientWidth,
    scrollers: scrollers, overflowing: overflowing.slice(0, 12) };
}

// --- S5: which family the engine actually USED -------------------------------------
//
// Not which family the stylesheet asked for. A font stack is a preference list and the
// engine picks from it; the only honest question is whether the glyphs it laid out are
// fixed-advance. So this measures two real runs of text in the element's own used font
// and compares their advance widths: equal means every glyph has the same advance, which
// is what monospace means.
function usedFontIsMono(el) {
  const cs = getComputedStyle(el);
  const probe = document.createElement("span");
  probe.style.position = "absolute";
  probe.style.left = "-9999px";
  probe.style.top = "0";
  probe.style.whiteSpace = "pre";
  probe.style.fontFamily = cs.fontFamily;
  probe.style.fontSize = cs.fontSize;
  probe.style.fontWeight = cs.fontWeight;
  probe.style.fontStyle = cs.fontStyle;
  probe.style.letterSpacing = cs.letterSpacing;
  document.body.appendChild(probe);
  probe.textContent = "iiiiiiiiiiiiiiii";
  const narrow = probe.getBoundingClientRect().width;
  probe.textContent = "MMMMMMMMMMMMMMMM";
  const wide = probe.getBoundingClientRect().width;
  probe.remove();
  return { mono: Math.abs(narrow - wide) < 0.5, narrow: narrow, wide: wide, family: cs.fontFamily };
}

function fonts(spec) {
  const out = [];
  for (const row of spec) {
    const els = document.querySelectorAll(row.selector);
    let seen = 0;
    for (const el of els) {
      if (!isRendered(el)) continue;
      seen++;
      const m = usedFontIsMono(el);
      out.push({ selector: row.selector, want: row.want, what: where(el),
        mono: m.mono, narrow: m.narrow, wide: m.wide, family: m.family });
      break; // one reading per selector is enough; they share a rule
    }
    if (seen === 0) out.push({ selector: row.selector, want: row.want, what: "", missing: true });
  }
  return out;
}

// --- S8: depth -----------------------------------------------------------------------
function shadows() {
  const out = [];
  for (const el of document.querySelectorAll("body, body *")) {
    if (!isRendered(el)) continue;
    const bs = getComputedStyle(el).boxShadow;
    if (bs && bs !== "none") out.push({ what: where(el), shadow: bs });
  }
  return out;
}

// --- S9: motion ----------------------------------------------------------------------
function durations(v) {
  return String(v || "").split(",").map(function (s) { return parseFloat(s) || 0; });
}
function motion() {
  const out = [];
  let maxTransition = 0, maxAnimation = 0;
  for (const el of document.querySelectorAll("body, body *")) {
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    const t = Math.max.apply(null, durations(cs.transitionDuration).concat([0]));
    const a = Math.max.apply(null, durations(cs.animationDuration).concat([0]));
    if (t > 0 || a > 0) out.push({ what: where(el), transition: t, animation: a });
    if (t > maxTransition) maxTransition = t;
    if (a > maxAnimation) maxAnimation = a;
  }
  return { moving: out.slice(0, 40), count: out.length,
    maxTransition: maxTransition, maxAnimation: maxAnimation };
}

// --- S1 / S3: every painted colour and every spacing length, as COMPUTED -------------
//
// The text check proves a value is not WRITTEN outside the token file. This proves the
// complementary thing, which no text check can: that what the cascade actually applied
// came from a token. A token declared and never applied, or a value arriving from
// somewhere the text check never looked, shows up here and nowhere else.
const SPACING_PROPS = ["margin-top","margin-right","margin-bottom","margin-left",
  "padding-top","padding-right","padding-bottom","padding-left","row-gap","column-gap"];

// computedLengthPx answers the COMPUTED value of a spacing property when that value is a
// length, and null when it is anything else.
//
// It goes through the Typed OM rather than through getComputedStyle deliberately, and the
// difference is load-bearing: getComputedStyle resolves a layout-dependent value, so a
// centred block's "margin: 0 auto" comes back as the used pixel offset - a number the
// layout computed from the viewport, not a value anybody wrote. The Typed OM keeps "auto"
// as the keyword it is. A keyword is not a spacing length and S1 says nothing about it;
// what S1 refuses is a LENGTH that did not come from the token file.
function computedLengthPx(el, prop) {
  if (!el.computedStyleMap) return null;
  let v;
  try { v = el.computedStyleMap().get(prop); } catch (e) { return null; }
  if (!v) return null;
  if (typeof v.value !== "number" || v.unit !== "px") return null;
  return v.value;
}

function painted() {
  const colours = [], lengths = [];
  for (const el of document.querySelectorAll("body, body *")) {
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    const w = where(el);
    const fg = parseColor(cs.color);
    if (fg && fg.a > 0 && ownText(el) !== "") colours.push({ what: w, prop: "color", value: rgbText(fg) });
    const bg = parseColor(cs.backgroundColor);
    if (bg && bg.a > 0.01) colours.push({ what: w, prop: "background-color", value: rgbText(bg) });
    for (const side of ["Top","Right","Bottom","Left"]) {
      if (cs["border" + side + "Style"] === "none") continue;
      if ((parseFloat(cs["border" + side + "Width"]) || 0) <= 0) continue;
      const bc = parseColor(cs["border" + side + "Color"]);
      if (bc && bc.a > 0.01) colours.push({ what: w, prop: "border-" + side.toLowerCase() + "-color", value: rgbText(bc) });
    }
    for (const p of SPACING_PROPS) {
      const v = computedLengthPx(el, p);
      if (v === null || v === 0) continue;
      lengths.push({ what: w, prop: p, value: v });
    }
  }
  // The figures' own paint. Only the marks: an HTML element's computed fill defaults to
  // black and paints nothing, so sweeping every element would report a colour no reader
  // ever sees.
  for (const el of document.querySelectorAll(".agg .fig .mark, .agg .fig .axis, .agg .fig .tick")) {
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    for (const p of ["fill", "stroke"]) {
      const c = parseColor(cs[p]);
      if (c && c.a > 0.01) colours.push({ what: where(el), prop: p, value: rgbText(c) });
    }
  }
  return { colours: colours, lengths: lengths };
}

// --- the page's own numbers, for the "absence is not zero" graders ------------------
function figures() {
  const cells = [];
  for (const body of [document.getElementById("queue"), document.getElementById("history")]) {
    if (!body) continue;
    for (const tr of body.children) {
      if (tr.dataset && tr.dataset.state) continue;
      for (const td of tr.children) {
        cells.push({ what: (body.id) + " " + (td.className || td.tagName.toLowerCase()),
          text: visText(td) });
      }
    }
  }
  const aggs = [];
  for (const card of document.querySelectorAll("#aggregates .agg")) {
    aggs.push({ title: visText(card.querySelector(".agg-k")), value: visText(card.querySelector(".agg-v")),
      out: card.classList.contains("out"), text: visText(card) });
  }
  const chips = [];
  const host = document.getElementById("chips");
  if (host) for (const c of host.children) {
    chips.push({ key: visText(c.querySelector(".k")), n: visText(c.querySelector(".n")) });
  }
  return { cells: cells, aggs: aggs, chips: chips,
    reclaimedSession: visText(document.getElementById("reclaimed-session")),
    reclaimedLifetime: visText(document.getElementById("reclaimed-lifetime")) };
}

function elapsedValues() {
  const out = [];
  for (const td of liveClockCells()) out.push(visText(td));
  return out;
}

// --- the live clock, named structurally so a grader can exclude exactly it -------------
//
// The queue's elapsed column is the ONE thing a reader sees whose text is a function of
// the WALL CLOCK rather than of the snapshot: 80-wire.js rewrites every cell each second
// from the row's own transition timestamp. Two renders of the same page can never be
// taken at the same instant, so any grader that compares the whole of innerText across a
// pair of renders is comparing how OLD each page happened to be as well as the property
// it is actually about - and reds on a correct page whenever the two readings straddle a
// second.
//
// bodyTextWithoutTheLiveClock is the text a reader sees with those cells, and only those
// cells, replaced by a constant. The substitution is structural (a selector), never a
// guess at which words look like a duration, and it is undone before the function
// returns. The whole of it runs in one task, so the page's own one-second ticker cannot
// interleave with it.
//
// The exclusion is not a hole: the cells are COUNTED and their values reported alongside,
// so a column that disappeared, was blanked, or gained a member is visible to the Go side
// rather than hidden inside the part that was excluded.
const LIVE_CLOCK_SELECTOR = "#queue td.elapsed";
const LIVE_CLOCK_MASK = "(live clock)";

function liveClockCells() {
  return Array.prototype.slice.call(document.querySelectorAll(LIVE_CLOCK_SELECTOR));
}

function bodyTextWithoutTheLiveClock() {
  const cells = liveClockCells();
  const values = cells.map(function (td) { return visText(td); });
  const saved = cells.map(function (td) { return Array.prototype.slice.call(td.childNodes); });
  for (const td of cells) td.replaceChildren(document.createTextNode(LIVE_CLOCK_MASK));
  let text;
  try { text = document.body.innerText; }
  finally {
    for (let i = 0; i < cells.length; i++) cells[i].replaceChildren.apply(cells[i], saved[i]);
  }
  return { text: text, cells: cells.length, values: values };
}

function controls() {
  const out = [];
  for (const id of ["token", "filter", "rescan", "pause", "resume"]) {
    const el = document.getElementById(id);
    out.push({ id: id, present: !!el, rendered: isRendered(el),
      disabled: !!(el && el.disabled) });
  }
  return out;
}

function connText() {
  const c = document.getElementById("conn");
  return c ? visText(c) : "";
}

// --- interface-craft C3 and C5: what the page PAINTED, per region and per container ------
//
// Everything below reports readings and nothing below decides: the two channel counts and
// the three-fact floor are in graders.mjs, so both can be run against a document built to
// defeat them. Each reading is the engine's own - the computed size, weight and colour
// AFTER the whole cascade, the box the layout actually produced, and the text innerText
// says a reader can see - which is why a later rule that changes only what is painted is
// caught here while every declaration in the source stays exactly as it was written.

// onScreen is isRendered plus the one thing it does not cover: a box the engine CLIPS out
// of sight. The screen-reader summary is a 1px box under an inset clip path - present
// in the document, announced by assistive technology, and never painted for a reader - so
// counting its text would put a size in the page's type scale nobody sees and a fact in a
// container nobody can read. The test is the engine's own geometry, not a class name.
function onScreen(el) {
  if (!isRendered(el)) return false;
  for (let n = el; n; n = n.parentElement) {
    if (getComputedStyle(n).clipPath === "none") continue;
    const r = n.getBoundingClientRect();
    if (r.width <= 2 || r.height <= 2) return false;
  }
  return true;
}

// FIGURE_TOKEN is one token of a value: a number, optionally carrying its unit against it
// ("17s", "25%", "99.3", "0"). FIGURE_UNIT is that unit written as a token of its OWN,
// which is the shape this page's LARGEST figures are painted in: fmtBytes renders
// "12.3 MB" and fmtDur renders "725 ms", both a number, a space and a unit.
//
// A run of text is read as a FIGURE when every token in it is a value, or when it is
// exactly a value and the unit naming it. The second form is bounded at two tokens on
// purpose: it admits "12.3 MB" and "725 ms" while a longer run carrying a word between
// numbers stays out - "1h 0m of 1h 0m" is the progress cell's own explanatory text, which
// LABELS a figure and is not one. The same bound is what keeps a path, a codec string and
// a sentence with a version number in it out of the set.
const FIGURE_TOKEN = /^[+-]?\d[\d,]*(?:\.\d+)?(?:%|[A-Za-z]{1,3})?$/;
const FIGURE_UNIT = /^(?:%|[A-Za-z]{1,3})$/;

function isFigureText(t) {
  const parts = String(t).split(/[\s→>]+/).filter(function (p) { return p !== ""; });
  if (parts.length === 0) return false;
  if (parts.every(function (p) { return FIGURE_TOKEN.test(p); })) return true;
  return parts.length === 2 && FIGURE_TOKEN.test(parts[0]) && FIGURE_UNIT.test(parts[1]);
}

// channelsOf is one subject's three channels, read off the engine after the cascade.
function channelsOf(cs) {
  return { size: parseFloat(cs.fontSize) || 0,
    weight: parseInt(cs.fontWeight, 10) || 400, color: cs.color };
}

function runsOnScreen(root) {
  const out = [];
  for (const el of root.querySelectorAll("*")) {
    const t = ownText(el);
    if (t === "") continue;
    if (!onScreen(el)) continue;
    const ch = channelsOf(getComputedStyle(el));
    out.push({ el: el, what: where(el), text: t, figure: isFigureText(t),
      size: ch.size, weight: ch.weight, color: ch.color,
      rect: el.getBoundingClientRect() });
  }
  return out;
}

// columnLabelOf answers a data cell's label the way a TABLE means it: the column header.
// At full width that header is a rendered <th>; below the width where the row stacks into
// a card the header row is gone and the engine draws the column's name as the cell's own
// generated content, so the reading follows it there rather than losing the label the
// moment the layout changes. Both answers come from the engine - one a laid-out element,
// the other a computed pseudo-element - and neither is a copy of the column's name kept
// here.
function columnLabelOf(el) {
  const cell = el.closest("td");
  if (!cell) return null;
  const table = cell.closest("table");
  const head = table && table.tHead && table.tHead.rows[0];
  const th = head ? head.cells[cell.cellIndex] : null;
  if (th && onScreen(th)) {
    const ch = channelsOf(getComputedStyle(th));
    return { what: where(th), text: visText(th), how: "column header",
      size: ch.size, weight: ch.weight, color: ch.color };
  }
  const before = getComputedStyle(cell, "::before");
  if (before && before.content && before.content !== "none" && before.content !== "normal") {
    const ch = channelsOf(before);
    return { what: where(cell) + "::before", how: "the column name drawn on the stacked cell",
      text: before.content.replace(/^"|"$/g, ""),
      size: ch.size, weight: ch.weight, color: ch.color };
  }
  return null;
}

function depthOf(el) {
  let d = 0;
  for (let n = el; n; n = n.parentElement) d++;
  return d;
}

function commonAncestor(a, b) {
  for (let n = a; n; n = n.parentElement) if (n.contains(b)) return n;
  return null;
}

// namingRunFor is the label of a figure that is not in a table: the run of text the page
// puts NEAREST it, where nearest is decided in the order a reader resolves it - the text
// sharing the figure's own smallest box first (deepest common ancestor), then the text on
// the figure's own rendered line, then the text closest to it in the document. Nothing
// here consults a class name, so a figure that moves keeps whatever names it.
function namingRunFor(fig, runs, region, order) {
  let best = null, bestKey = null;
  for (const c of runs) {
    if (c.figure || c.el === fig.el) continue;
    if (!region.contains(c.el)) continue;
    const a = commonAncestor(fig.el, c.el);
    if (!a) continue;
    const sameLine = !(c.rect.bottom <= fig.rect.top + 0.5 || c.rect.top >= fig.rect.bottom - 0.5);
    const key = [depthOf(a), sameLine ? 1 : 0, -Math.abs(order.get(c.el) - order.get(fig.el))];
    if (bestKey === null || key[0] > bestKey[0] ||
        (key[0] === bestKey[0] && (key[1] > bestKey[1] || (key[1] === bestKey[1] && key[2] > bestKey[2])))) {
      best = c; bestKey = key;
    }
  }
  if (!best) return null;
  return { what: best.what, text: best.text, how: "the nearest text naming it",
    size: best.size, weight: best.weight, color: best.color };
}

// hierarchy is clause C3's reading: every region the page renders, and within each one
// every figure it paints beside the text that names that figure, both sides carrying the
// three channels the clause counts, plus the whole page's painted type scale.
function hierarchy() {
  const runs = runsOnScreen(document.body);
  const all = Array.prototype.slice.call(document.body.querySelectorAll("*"));
  const order = new Map();
  for (let i = 0; i < all.length; i++) order.set(all[i], i);

  const sizes = [];
  for (const r of runs) if (sizes.indexOf(r.size) < 0) sizes.push(r.size);

  const regions = [];
  for (const region of document.querySelectorAll("main section")) {
    const heading = region.querySelector("h1, h2, h3, h4, h5, h6");
    const figures = [];
    for (const r of runs) {
      if (!r.figure || !region.contains(r.el)) continue;
      const label = columnLabelOf(r.el) || namingRunFor(r, runs, region, order);
      figures.push({ what: r.what, text: r.text.slice(0, 60),
        size: r.size, weight: r.weight, color: r.color, label: label });
    }
    regions.push({ what: where(region), shown: onScreen(region),
      heading: heading ? visText(heading) : "",
      runs: runs.filter(function (r) { return region.contains(r.el); }).length,
      figures: figures });
  }
  return { regions: regions, sizes: sizes.sort(function (a, b) { return a - b; }) };
}

// chromed is clause C5's reading: every element the engine paints CHROME on, with the
// facts it holds and with the four properties the grader decides a container by. Nothing
// is dropped here - a landmark, a control and a drawing are all reported and marked, so
// the exemption is made where it can be read and argued with rather than hidden inside
// the measurement.
const CRAFT_LANDMARK = "header, footer, main, nav, aside, section, [role=banner], " +
  "[role=contentinfo], [role=main], [role=navigation], [role=region], [role=complementary]";
const CRAFT_CONTROL = "a[href], button, input, select, textarea, [role=button], [role=link], [tabindex]";
const CRAFT_GRAPHIC = "svg, img, canvas, video, iframe, object, picture";

function paintedSides(cs) {
  const out = [];
  for (const side of ["Top", "Right", "Bottom", "Left"]) {
    const style = cs["border" + side + "Style"];
    if (style === "none" || style === "hidden") continue;
    if ((parseFloat(cs["border" + side + "Width"]) || 0) <= 0) continue;
    const c = parseColor(cs["border" + side + "Color"]);
    if (!c || c.a <= 0.01) continue;
    out.push(side.toLowerCase());
  }
  return out;
}

function chromed() {
  const out = [];
  for (const el of document.querySelectorAll("body, body *")) {
    if (!onScreen(el)) continue;
    const cs = getComputedStyle(el);
    const sides = paintedSides(cs);
    const shadow = cs.boxShadow && cs.boxShadow !== "none" ? cs.boxShadow : "";
    if (sides.length === 0 && shadow === "") continue;
    const facts = [];
    for (const r of runsOnScreen(el)) facts.push(r.text.slice(0, 40));
    const own = ownText(el);
    if (own !== "") facts.unshift(own.slice(0, 40));
    let children = 0;
    for (const kid of el.children) if (onScreen(kid)) children++;
    out.push({ what: where(el), sides: sides, shadow: shadow,
      landmark: el.matches(CRAFT_LANDMARK), control: el.matches(CRAFT_CONTROL),
      graphic: el.matches(CRAFT_GRAPHIC), elementChildren: children, facts: facts });
  }
  return out;
}

// --- interface-craft C4: depth, double separation and the coloured left strip -------------
//
// Everything below is a READING and decides nothing; the three predicates are in
// graders.mjs. Each reading is the engine's own - the computed shadow after the cascade,
// the border the engine painted on each of an element's four sides, the background it
// actually filled, and the boxes the layout produced - so a later rule that changes only
// what is PAINTED is caught here while every declaration in the source stays as written.

// AT REST is the condition clause C4's first sentence is about: a view draws at most one
// shadow depth, and a focus ring or a hover elevation is not a second depth because no
// reader ever sees it at the same time as the first. So the reading records what the engine
// says is hovered, focused or being pressed AT THE MOMENT IT WAS TAKEN, and the grader
// refuses a reading taken while any of them was true. html and body are excluded on
// purpose: they match :hover whenever the pointer is anywhere over the document at all,
// which is a fact about the pointer and not about an element's own treatment.
function atRest() {
  const skip = new Set([document.documentElement, document.body]);
  const names = (sel) => Array.prototype.slice.call(document.querySelectorAll(sel))
    .filter(function (el) { return !skip.has(el); }).map(where);
  const a = document.activeElement;
  return {
    hovered: names(":hover"),
    active: names(":active"),
    focused: a && a !== document.body && a !== document.documentElement ? where(a) : "",
  };
}

// paintedBorders is the border the engine actually painted on each side: a side with no
// style, no width or a fully transparent colour paints nothing and is absent from the
// answer. What is written in the stylesheet is not consulted.
function paintedBorders(cs) {
  const out = {};
  for (const side of ["top", "right", "bottom", "left"]) {
    const S = side[0].toUpperCase() + side.slice(1);
    const style = cs["border" + S + "Style"];
    if (style === "none" || style === "hidden") continue;
    const width = parseFloat(cs["border" + S + "Width"]) || 0;
    if (width <= 0) continue;
    const c = parseColor(cs["border" + S + "Color"]);
    if (!c || c.a <= 0.01) continue;
    out[side] = { style: style, width: width, colour: rgbText(c) };
  }
  return out;
}

// ownSurface is the background an element paints ITSELF, and null when it paints none. The
// distinction is the whole of C4's second clause: an element that fills no background of
// its own IS the canvas behind it at that point, and a bordered panel against the canvas is
// figure and ground rather than two surfaces told apart twice.
function ownSurface(cs) {
  const c = parseColor(cs.backgroundColor);
  if (!c || c.a <= 0.01) return null;
  return rgbText(c);
}

// SECTIONING is the roles the accessibility tree reports for a region of the page. An
// element is a REGION for this clause when it carries one of them AND a name - which is
// what the page's own <section aria-labelledby> pairs are - or when it paints chrome of its
// own: a visible border or a shadow. The second limb is what keeps the clause about what is
// DRAWN rather than about which elements happen to be landmarks.
const SECTIONING = "main, section, article, aside, nav, header, footer, form, " +
  "[role=region], [role=main], [role=complementary], [role=navigation], [role=banner], " +
  "[role=contentinfo], [role=form], [role=search]";

function namedSection(el) {
  if (!el.matches(SECTIONING)) return false;
  if ((el.getAttribute("aria-label") || "").trim() !== "") return true;
  const by = el.getAttribute("aria-labelledby");
  if (!by) return false;
  for (const id of by.split(/\s+/)) {
    const t = document.getElementById(id);
    if (t && visText(t) !== "") return true;
  }
  return false;
}

// hasNonColourSignal answers C4's third clause: whether the element says what it says by
// something other than the colour of a strip down its left edge - rendered text of its own
// or in a descendant, a drawing, or an accessible name the engine would announce.
function hasNonColourSignal(el) {
  const text = visText(el);
  const mark = el.querySelector("svg, img, canvas, picture, video") ||
    (el.matches("svg, img, canvas, picture, video") ? el : null);
  const name = (el.getAttribute("aria-label") || el.getAttribute("title") || "").trim();
  return { text: text.slice(0, 60), hasText: text !== "", hasMark: !!mark, name: name };
}

// ornament is the whole of clause C4's reading, taken in ONE pass over the rendered
// document. Its one argument is the set of element INDICES the accessibility tree reported
// with an interactive role, handed in by the driver: a control's own edge is not a region
// separation, and which elements are controls is the engine's answer rather than a
// selector's.
function ornament(interactive) {
  const all = Array.prototype.slice.call(document.querySelectorAll("*"));
  const order = new Map();
  for (let i = 0; i < all.length; i++) order.set(all[i], i);
  const isControl = new Set(interactive || []);

  const shadows = [], regions = [];
  const strips = [];
  for (const el of all) {
    if (!isRendered(el)) continue;
    const cs = getComputedStyle(el);
    const shadow = cs.boxShadow && cs.boxShadow !== "none" ? cs.boxShadow : "";
    const sides = paintedBorders(cs);
    const sideNames = Object.keys(sides);
    if (shadow) shadows.push({ what: where(el), shadow: shadow });

    // The left-strip subject: a left edge painted in a colour none of the other painted
    // edges carries. An element whose ONLY painted edge is the left one is the clearest
    // case of it and is included by the same test.
    if (sides.left) {
      const others = [];
      for (const side of ["top", "right", "bottom"]) if (sides[side]) others.push(sides[side].colour);
      if (others.indexOf(sides.left.colour) < 0) {
        const signal = hasNonColourSignal(el);
        strips.push({ what: where(el), left: sides.left.colour, others: others,
          lone: others.length === 0, text: signal.text, hasText: signal.hasText,
          hasMark: signal.hasMark, name: signal.name,
          control: isControl.has(order.get(el)) });
      }
    }

    if (sideNames.length === 0 && shadow === "" && !namedSection(el)) continue;
    regions.push({ index: order.get(el), what: where(el), sides: sides, shadow: shadow,
      surface: ownSurface(cs), section: namedSection(el),
      control: isControl.has(order.get(el)) });
  }

  // NEIGHBOURING is adjacent siblings inside one container, and which edges FACE each other
  // is decided from the boxes the layout produced rather than from the writing mode or from
  // the order of the markup. A pair the engine laid out overlapping, or wrapped onto
  // another line, has no single facing edge and is reported as unresolved rather than
  // silently dropped.
  const inRegion = new Set(regions.map(function (r) { return r.index; }));
  const byIndex = new Map();
  for (const r of regions) byIndex.set(r.index, r);
  const pairs = [], unresolved = [];
  for (const el of all) {
    const next = el.nextElementSibling;
    if (!next || !isRendered(el) || !isRendered(next)) continue;
    const ai = order.get(el), bi = order.get(next);
    if (!inRegion.has(ai) && !inRegion.has(bi)) continue;
    if (isControl.has(ai) || isControl.has(bi)) continue;
    const ra = el.getBoundingClientRect(), rb = next.getBoundingClientRect();
    const ca = getComputedStyle(el), cb = getComputedStyle(next);
    const A = byIndex.get(ai) || { sides: paintedBorders(ca), surface: ownSurface(ca), what: where(el) };
    const B = byIndex.get(bi) || { sides: paintedBorders(cb), surface: ownSurface(cb), what: where(next) };
    let axis = "", aEdge = "", bEdge = "";
    if (rb.top >= ra.bottom - 1) { axis = "vertical"; aEdge = "bottom"; bEdge = "top"; }
    else if (rb.left >= ra.right - 1) { axis = "horizontal"; aEdge = "right"; bEdge = "left"; }
    else if (ra.left >= rb.right - 1) { axis = "horizontal"; aEdge = "left"; bEdge = "right"; }
    if (axis === "") { unresolved.push({ a: where(el), b: where(next) }); continue; }
    pairs.push({ a: A.what, b: B.what, axis: axis,
      aEdge: aEdge, bEdge: bEdge,
      aBorder: A.sides[aEdge] || null, bBorder: B.sides[bEdge] || null,
      aSurface: A.surface, bSurface: B.surface,
      aIsRegion: inRegion.has(ai), bIsRegion: inRegion.has(bi) });
  }

  return { rest: atRest(), shadows: shadows, regions: regions, pairs: pairs,
    strips: strips, unresolved: unresolved, elements: all.length };
}

// --- interface-craft C7: one component in one state -----------------------------------
//
// STATE_PROPERTIES is what "renders differently" is decided over: the properties an
// engine changes when a control enters a state, each read AFTER the cascade. It is a
// closed list on purpose - a comparison over every computed property would report a
// difference for a layout the state happened to reflow - and every one of them is
// something a reader can see.
const STATE_PROPERTIES = ["color", "backgroundColor", "backgroundImage",
  "borderTopColor", "borderRightColor", "borderBottomColor", "borderLeftColor",
  "borderTopStyle", "borderRightStyle", "borderBottomStyle", "borderLeftStyle",
  "borderTopWidth", "borderRightWidth", "borderBottomWidth", "borderLeftWidth",
  "outlineStyle", "outlineWidth", "outlineColor", "outlineOffset",
  "boxShadow", "opacity", "textDecorationLine", "textDecorationColor",
  "textDecorationThickness", "textDecorationStyle",
  "fontWeight", "fontStyle", "transform", "filter", "visibility"];

// stateOf is one component instance's rendered appearance right now, plus the pseudo-class
// the engine says it is in. The pseudo-class matters: a cell that claims to have entered
// :focus-visible and did not is a cell that measured the default twice, and only the
// engine can say which it was.
function stateOf(index) {
  const all = Array.prototype.slice.call(document.querySelectorAll("*"));
  const el = all[index];
  if (!el) return { missing: true };
  const cs = getComputedStyle(el);
  const props = {};
  for (const p of STATE_PROPERTIES) props[p] = String(cs[p]);
  const r = el.getBoundingClientRect();
  return { missing: false, what: where(el), props: props,
    rendered: isRendered(el), width: r.width, height: r.height,
    matches: {
      hover: el.matches(":hover"), active: el.matches(":active"),
      focus: el.matches(":focus"), focusVisible: el.matches(":focus-visible"),
      disabled: el.matches(":disabled"),
    },
    disabledProperty: "disabled" in el ? !!el.disabled : null };
}

// componentsOf turns the element indices the driver derived into the readings the grouping
// runs over. Nothing here groups anything: the key and the checks are in graders.mjs.
function componentsOf(indices) {
  const all = Array.prototype.slice.call(document.querySelectorAll("*"));
  const out = [];
  for (const i of indices || []) {
    const el = all[i];
    if (!el) { out.push({ index: i, missing: true }); continue; }
    const cls = (el.getAttribute("class") || "").trim();
    out.push({ index: i, missing: false, what: where(el),
      tag: el.tagName.toLowerCase(), type: el.getAttribute("type") || "",
      classes: cls === "" ? [] : cls.split(/\s+/).slice().sort(),
      rendered: isRendered(el),
      canDisable: "disabled" in el,
      disabled: "disabled" in el ? !!el.disabled : false });
  }
  return out;
}

// tabIndexOf reports the element the engine currently focuses, by the same index
// componentsOf uses. It is read after a REAL key press and is the only honest answer to
// what Tab moved: a KeyboardEvent constructed in the page is untrusted and moves nothing.
function focusedIndex() {
  const a = document.activeElement;
  if (!a || a === document.body || a === document.documentElement) return -1;
  return Array.prototype.slice.call(document.querySelectorAll("*")).indexOf(a);
}

// tagNames is the document's elements in the order querySelectorAll walks them. The driver
// cross-checks it against the tree the DevTools protocol returned, so an index computed on
// one side and used on the other cannot quietly mean two different elements.
function tagNames() {
  return Array.prototype.slice.call(document.querySelectorAll("*"))
    .map(function (el) { return el.tagName.toLowerCase(); });
}

// densityMechanism applies the way the record says a density is entered, and reports what
// it did. "the document as served" changes nothing, which is the honest answer for a
// surface that builds one density and offers no switch.
function applyDensity(attr, value) {
  const de = document.documentElement;
  if (!attr) return { applied: "the document as served" };
  if (value === null) de.removeAttribute(attr);
  else de.setAttribute(attr, value);
  return { applied: attr + "=" + JSON.stringify(value) };
}

function rendered() {
  const sr = document.getElementById("sr-status");
  return !!sr && sr.textContent.trim() !== "";
}

return {
  tokens: tokens, textRuns: textRuns, pointerTargets: pointerTargets,
  focusables: focusables, focused: focused, views: views, snapshotViews: snapshotViews,
  labels: labels,
  mainParagraphs: mainParagraphs, doclinks: doclinks, layout: layout,
  fonts: fonts, shadows: shadows, motion: motion, painted: painted,
  figures: figures, elapsedValues: elapsedValues, controls: controls,
  hierarchy: hierarchy, chromed: chromed,
  ornament: ornament, stateOf: stateOf, componentsOf: componentsOf,
  focusedIndex: focusedIndex, tagNames: tagNames, applyDensity: applyDensity,
  connText: connText, rendered: rendered,
  bodyText: function () { return document.body.innerText; },
  bodyTextWithoutTheLiveClock: bodyTextWithoutTheLiveClock,
  sourceOffer: function () {
    const p = document.querySelector("p.source-offer");
    return { present: !!p, shown: isRendered(p), text: p ? visText(p) : "" };
  },
  msg: function () { const m = document.getElementById("msg"); return { text: m ? visText(m) : "", cls: m ? m.className : "" }; },
  badges: function () {
    return { paused: visText(document.getElementById("b-paused")),
             scan: visText(document.getElementById("b-scan")) };
  }
};
})();
true
`;

package webui

// The measuring script for the frontend-convention graders (S0053).
//
// It runs from the DEVTOOLS side against the SERVED document loaded at the top level -
// not inside an iframe - so the response headers, the Content-Security-Policy and the
// media the engine emulates all apply to the document under measurement exactly as they
// would to a reader's. Everything it reports is something the ENGINE computed: the
// resolved value of a custom property after the cascade and the media queries, a real
// layout box, the colour actually painted behind a run of text, the font the layout
// engine actually used, and the text innerText says a reader can see.
//
// Nothing here decides anything. Every predicate lives in Go, in
// conventions_rendered_test.go, so each one can be run against a document deliberately
// mutated to defeat it - which is the only way to know a grader can fail at all.

const conventionsProbeJS = `
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
    const rec = { region: section.id, count: links.length, hrefs: [], texts: [], shown: [] };
    for (const a of links) {
      rec.hrefs.push(a.getAttribute("href"));
      rec.texts.push(visText(a));
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

function rendered() {
  const sr = document.getElementById("sr-status");
  return !!sr && sr.textContent.trim() !== "";
}

return {
  tokens: tokens, textRuns: textRuns, pointerTargets: pointerTargets,
  focusables: focusables, focused: focused, views: views, labels: labels,
  mainParagraphs: mainParagraphs, doclinks: doclinks, layout: layout,
  fonts: fonts, shadows: shadows, motion: motion, painted: painted,
  figures: figures, elapsedValues: elapsedValues, controls: controls,
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
`

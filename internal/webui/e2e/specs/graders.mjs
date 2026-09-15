// The frontend-convention GRADERS: one pure function per clause, each taking the snapshot
// the probe measured and returning the problems it found.
//
// They are pure functions, and separate from both the measuring and the driving, for one
// reason: every one of them is run against a document deliberately built to defeat it
// (mutations.spec.mjs). A grader that cannot fail is not evidence, and the only way to
// know one can fail is to defeat it on purpose and watch it report.
//
// Each returns an ARRAY of problems - empty means the clause held. Nothing here throws
// and nothing here asserts; the specs decide what to do with what these report, which is
// what lets the same function be used both to grade the page and to prove itself.

// --- WCAG 2.2's own relative luminance, recomputed rather than trusted -----------------
const RGB = /rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)/;

export function wcagContrast(fg, bg) {
  const lum = (s) => {
    const m = RGB.exec(String(s).trim());
    if (!m) return null;
    let acc = [0, 0, 0];
    for (let i = 0; i < 3; i++) {
      const n = Number(m[i + 1]);
      if (!isFinite(n)) return null;
      const v = n / 255;
      acc[i] = v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    }
    return 0.2126 * acc[0] + 0.7152 * acc[1] + 0.0722 * acc[2];
  };
  const a = lum(fg), b = lum(bg);
  if (a === null || b === null) return null;
  const hi = Math.max(a, b), lo = Math.min(a, b);
  return (hi + 0.05) / (lo + 0.05);
}

// --- what the page declares about itself ----------------------------------------------

// The surfaces this page calls RAISED. Only a genuinely raised surface computes a shadow.
export const RAISED_SURFACES = new Set(["header"]);

// fontExpectations is clause S5 written down: monospace where COLUMN ALIGNMENT carries
// meaning, and the system UI face for headings, explanatory text and controls. Each row is
// measured against the font the engine actually used, never against the stack asked for.
export const FONT_EXPECTATIONS = [
  { selector: "td.path", want: "mono" },
  { selector: "td.elapsed", want: "mono" },
  { selector: "td.dur", want: "mono" },
  { selector: "td.upd", want: "mono" },
  { selector: "td.worker", want: "mono" },
  { selector: "td.size", want: "mono" },
  { selector: "td.vmaf .mean", want: "mono" },
  { selector: "#chips .chip .n", want: "mono" },
  { selector: ".reclaimed b", want: "mono" },
  { selector: ".buckets .bc", want: "mono" },
  { selector: ".spreadkeys .sv", want: "mono" },
  { selector: "h1", want: "ui" },
  { selector: "section > h2", want: "ui" },
  { selector: "section > h3", want: "ui" },
  { selector: ".scope", want: "ui" },
  { selector: ".lbl", want: "ui" },
  { selector: "button", want: "ui" },
  { selector: ".controls input", want: "ui" },
  { selector: "th", want: "ui" },
  { selector: ".agg .agg-k", want: "ui" },
  { selector: ".agg .agg-cov", want: "ui" },
  { selector: ".vmaf .cond", want: "ui" },
];

// fontsAbsentBelow declares the selectors this surface deliberately renders NO element for
// below a given viewport width, with the reason. A selector that silently stops matching
// is a rule that silently stops asserting, so the default stays an error and the exception
// has to be written down here - which is also the only place a reader can find out that
// the narrow layout is a different layout and not the same one squeezed.
export const FONTS_ABSENT_BELOW = {
  th: {
    px: 761,
    why: "below this width a table row STACKS into a card and carries each column's name on the cell itself, so there is no column header row to set a family on",
  },
};

// --- the graders ------------------------------------------------------------------------

// Clause F1's contrast floor over EVERY run of text the engine rendered, measured against
// the colour it actually painted behind that run. The floors are WCAG 2.2's own: 4.5:1,
// relaxed to 3:1 for large text (24px, or 18.66px and bold).
export function gradeTextContrast(s) {
  const out = [];
  if (!s.textRuns || s.textRuns.length < 10) {
    return [`only ${s.textRuns ? s.textRuns.length : 0} runs of text were measured; the page did not render`];
  }
  for (const r of s.textRuns) {
    let floor = 4.5, kind = "text";
    if (r.size >= 24 || (r.size >= 18.66 && r.weight >= 700)) { floor = 3.0; kind = "large text"; }
    const ratio = wcagContrast(r.fg, r.bg);
    if (ratio === null) {
      out.push(`${r.what}: the engine reported an unreadable colour pair (${r.fg} on ${r.bg})`);
      continue;
    }
    if (ratio < floor - 0.005) {
      out.push(`${r.what} ("${r.text}") is ${ratio.toFixed(2)}:1 (${r.fg} on ${r.bg}), under the ${floor.toFixed(1)}:1 ${kind} floor`);
    }
  }
  return out;
}

// Clause F1's target size: every pointer target is painted in a box at least 24 by 24 CSS
// pixels, measured from the engine's own layout geometry. A link inside a sentence of
// prose is WCAG 2.2's own exemption and is skipped by name.
export function gradePointerTargets(s) {
  const out = [];
  let measured = 0;
  for (const t of s.targets || []) {
    if (t.tag === "a" && t.inProse) continue;
    measured++;
    if (t.w < 23.5 || t.h < 23.5) {
      out.push(`${t.what} is painted ${t.w.toFixed(1)} by ${t.h.toFixed(1)} CSS px, under the 24 by 24 pointer-target floor`);
    }
  }
  if (measured === 0) out.push("no pointer target was measured at all, so this grader asserted nothing");
  return out;
}

// Clause F8's bound on what stays on the surface: no explanatory or scope label longer
// than 15 words, measured from the text the engine reports as visible. It also refuses a
// paragraph inside a region that is outside the graded set, so a long explanation cannot
// come back by wearing a class the bound does not cover.
export function gradeLabelLength(s) {
  const out = [];
  if (!s.labels || s.labels.length < 5) {
    out.push(`only ${s.labels ? s.labels.length : 0} labels were measured; the page did not render its labels`);
  }
  for (const l of s.labels || []) {
    if (l.words > 15) {
      out.push(`${l.what} is ${l.words} words long: "${l.text}". Clause F8 keeps a scope label to a few words and moves the paragraphs into the repo's docs`);
    }
  }
  for (const p of s.mainParagraphs || []) {
    if (!p.graded) out.push(`${p.what} is a paragraph inside a region that no label rule covers: "${p.text}"`);
  }
  return out;
}

// Clause F8's other half: exactly one link per region to the repository's own
// documentation for that region's methodology, and it has to have reached the screen.
export function gradeDocLinks(s) {
  const out = [];
  if (!s.doclinks || s.doclinks.length === 0) return ["the page rendered no regions at all"];
  for (const r of s.doclinks) {
    if (r.count !== 1) {
      out.push(`the region ${r.region} renders ${r.count} documentation links, want exactly one`);
      continue;
    }
    if (!r.shown[0]) out.push(`the region ${r.region} renders a documentation link a reader never sees`);
    if (!String(r.names[0] || "").trim()) {
      out.push(`the region ${r.region} renders a documentation link with no name at all - it announces itself as "link" and nothing more`);
    }
  }
  return out;
}

// Clause F9: at a phone viewport the document body's scroll width is no greater than the
// viewport's.
export function gradeBodyNeverScrollsSideways(s) {
  const out = [];
  const L = s.layout;
  if (!L || L.innerWidth <= 0) return ["the engine reported no viewport width"];
  for (const c of [
    { what: "the document body's", width: L.bodyScrollWidth },
    { what: "the document element's", width: L.docScrollWidth },
  ]) {
    if (c.width > L.innerWidth + 1) {
      const names = (L.overflowing || []).map((o) => `${o.what} (right edge ${Math.round(o.right)})`);
      out.push(`${c.what} scroll width is ${Math.round(c.width)} at a ${Math.round(L.innerWidth)} px viewport, so the page scrolls sideways. Sticking out: ${names.join(", ")}`);
    }
  }
  return out;
}

// The rest of F9: content WIDER THAN THE VIEWPORT takes a scroll of its own, proved by
// that container's scroll width exceeding its client width while the body's does not.
//
// The clause is conditional on there BEING wide content, and it is written that way rather
// than assumed. It used to demand a scrolling container unconditionally, which was true of
// a page whose answer to seven columns at 360px was a container that scrolled - and became
// false the moment the answer changed to a row that STACKS. Demanding a scroller then
// would be demanding the worse of the two designs by name.
//
// It is not vacuous when nothing is wide. What would make it vacuous is nothing being
// MEASURED, and the probe reports every element sticking out past the viewport, so the two
// limbs together still say the whole of F9.
export function gradeWideContentScrollsInItsOwnContainer(s) {
  const L = s.layout || {};
  for (const c of L.scrollers || []) {
    if (c.scrollWidth > c.clientWidth + 1) return [];
  }
  if (!L.overflowing || L.overflowing.length === 0) return [];
  const names = L.overflowing.map((o) => `${o.what} (right edge ${Math.round(o.right)})`);
  return [`content sticks out past a ${Math.round(L.innerWidth)} px viewport and no container takes a horizontal scroll of its own, so it is either crushed or pushing the body sideways. Sticking out: ${names.join(", ")}`];
}

// Clause S5, measured from the font the engine ACTUALLY USED: two runs of text of equal
// length in that font, one of narrow glyphs and one of wide, have the same advance width
// if and only if the face is fixed-advance.
export function gradeFontSplit(s) {
  const out = [];
  if (!s.fonts || s.fonts.length === 0) return ["no font reading was taken at all"];
  for (const f of s.fonts) {
    if (f.missing) {
      const a = FONTS_ABSENT_BELOW[f.selector];
      if (a && s.layout && s.layout.innerWidth < a.px) continue;
      out.push(`no element matched "${f.selector}", so the family rule for it asserted nothing`);
      continue;
    }
    if (f.want === "mono" && !f.mono) {
      out.push(`${f.selector} (${f.what}) is laid out in a PROPORTIONAL face ("${f.family}": 16 narrow glyphs measure ${f.narrow.toFixed(1)}, 16 wide ones ${f.wide.toFixed(1)}); its column alignment carries meaning, so S5 requires monospace`);
    } else if (f.want === "ui" && f.mono) {
      out.push(`${f.selector} (${f.what}) is laid out in a MONOSPACE face ("${f.family}"); S5 puts headings, explanatory text and controls in the system UI face`);
    }
  }
  return out;
}

// Clause S8: only a genuinely raised surface computes a shadow, it comes from the one
// shadow token, and every other surface separates by border and surface token with no
// shadow at all.
export function gradeDepthScale(s) {
  const out = [];
  const seen = new Set(), values = new Set();
  for (const sh of s.shadows || []) {
    const tag = String(sh.what).split("#")[0].split(".")[0];
    values.add(sh.shadow);
    if (!RAISED_SURFACES.has(tag)) {
      out.push(`${sh.what} computes a shadow (${sh.shadow}) but is not a raised surface; S8 separates every other surface by border and surface token`);
      continue;
    }
    seen.add(tag);
  }
  for (const want of RAISED_SURFACES) {
    if (!seen.has(want)) {
      out.push(`the raised surface "${want}" computes no shadow at all, so the depth scale is never applied and this grader would assert nothing`);
    }
  }
  if (values.size > 3) out.push(`the page paints ${values.size} distinct shadow values; S8 allows two or three shadow tokens`);
  return out;
}

// Clause S9 under an emulated `prefers-reduced-motion: reduce`: the engine computes no
// non-zero transition or animation duration anywhere on the page.
export function gradeReducedMotion(s) {
  const m = s.motion || {};
  if (!m.maxTransition && !m.maxAnimation) return [];
  const names = (m.moving || []).map((x) => `${x.what} (transition ${x.transition.toFixed(3)}s, animation ${x.animation.toFixed(3)}s)`);
  return [`${m.count} element(s) still compute a non-zero duration under prefers-reduced-motion: reduce: ${names.join(", ")}`];
}

// The anti-vacuity half of S9. A page with no motion at all honours the reduce query
// trivially, and a grader that only ever measured such a page could never fail. So the
// page must compute motion WITHOUT the preference in force.
export function gradeMotionExists(s) {
  const m = s.motion || {};
  if (m.maxTransition > 0 || m.maxAnimation > 0) return [];
  return ["the page computes no transition or animation at all without prefers-reduced-motion, so the reduce grader asserts nothing"];
}

// Clauses S1 and S3 as the ENGINE sees them: every colour the page actually painted and
// every spacing length it actually computed is a value the token file declares. This is
// the half no text check can do - a token the stylesheet defines but the cascade never
// applies, or a value arriving from a place the text sweep never looked, is caught here
// and only here.
export function gradePaintedValuesComeFromTokens(s) {
  const { colours, lengths } = tokenValueSets(s.tokens || {});
  if (colours.size === 0 || lengths.size === 0) {
    return ["the engine resolved no token values at all, so nothing could be checked against them"];
  }
  const out = [];
  let seenColours = 0, seenLengths = 0;
  for (const c of (s.painted && s.painted.colours) || []) {
    seenColours++;
    if (!colours.has(String(c.value).trim())) {
      out.push(`${c.what} paints ${c.prop}: ${c.value}, which is not any value the token file declares`);
    }
  }
  for (const l of (s.painted && s.painted.lengths) || []) {
    seenLengths++;
    if (!lengths.has(roundLen(Math.abs(l.value)))) {
      out.push(`${l.what} computes ${l.prop}: ${l.value}px, which is not any length the token file declares`);
    }
  }
  if (seenColours < 10 || seenLengths < 10) {
    out.push(`only ${seenColours} colours and ${seenLengths} lengths were swept; the page did not render`);
  }
  return out;
}

// tokenValueSets turns the token values the ENGINE resolved into the two sets every
// painted value must come from.
export function tokenValueSets(tk) {
  const colours = new Set(), lengths = new Set();
  // Transparent is not a token value; it is the absence of paint, and the sweep already
  // skips it. rgba(0,0,0,0) is admitted so a fully transparent computed value that slipped
  // through cannot be reported as an unknown colour.
  colours.add("rgb(0, 0, 0)/0");
  for (const raw of Object.values(tk.resolved || {})) {
    const v = String(raw).trim();
    if (!v) continue;
    const hex = /^#([0-9a-fA-F]{6})$/.exec(v);
    if (hex) {
      colours.add(`rgb(${parseInt(hex[1].slice(0, 2), 16)}, ${parseInt(hex[1].slice(2, 4), 16)}, ${parseInt(hex[1].slice(4, 6), 16)})`);
    }
    for (const m of v.matchAll(/(\d+(?:\.\d+)?)px/g)) lengths.add(roundLen(Number(m[1])));
  }
  return { colours, lengths };
}

export function roundLen(v) { return Math.round(v * 100); }

// Clause F7: every view says, in words, which of the three states it is in, and it is the
// state the case put it in.
export function gradeViewsAllIn(want) {
  return (s) => {
    const out = [];
    // The SNAPSHOT-driven views, which are the ones every trigger of this criterion is a
    // fact about (see SNAPSHOT_VIEWS in probe.mjs). A view filled by something else is
    // graded by whatever fills it.
    const views = s.snapshotViews || s.views;
    if (!views || views.length !== 4) {
      return [`the page renders ${views ? views.length : 0} snapshot-driven views, want the four the dashboard has (counts, queue, aggs, history)`];
    }
    for (const v of views) {
      if (v.state !== want) {
        out.push(`the ${v.view} view is in state "${v.state}", want "${want}" (it shows "${v.text}")`);
        continue;
      }
      if (!v.shown) out.push(`the ${v.view} view's ${want} state is in the document but never reached the screen`);
      if (String(v.text).trim().split(/\s+/).filter(Boolean).length < 2) {
        out.push(`the ${v.view} view's ${want} state is "${v.text}", which is not a state in words`);
      }
    }
    return out;
  };
}

// Clause F3's other half: a page that is connected and has been given nothing must not
// render a count, a total or an aggregate as 0.
export function gradeNoZeroBeforeASnapshot(s) {
  const out = [];
  const f = s.figures || {};
  for (const c of f.chips || []) out.push(`a count chip (${c.key} = "${c.n}") is on screen before any snapshot arrived`);
  for (const a of f.aggs || []) out.push(`a whole-ledger figure (${a.title} = "${a.value}") is on screen before any snapshot arrived`);
  for (const x of [
    { what: "reclaimed this run", v: f.reclaimedSession },
    { what: "reclaimed lifetime", v: f.reclaimedLifetime },
  ]) {
    if (zeroLike(x.v)) {
      out.push(`"${x.what}" reads "${x.v}" before any snapshot arrived; a total nobody has reported yet is not 0`);
    }
  }
  return out;
}

// zeroLike reports whether a rendered figure reads as zero to a reader: a bare 0, a 0 with
// a unit, a 0.0, or a dash standing in for one.
//
// Every dash a reader would take for a zero is decided by CODE POINT rather than by a dash
// character written into this file: clause F3 forbids "a dash a reader will read as zero",
// and that is the ASCII hyphen, the whole Unicode dash block (U+2010 hyphen through U+2015
// horizontal bar, which includes the en and em dashes), and the minus sign U+2212.
export function zeroLike(v) {
  const t = String(v == null ? "" : v).trim();
  if (t === "") return true;
  if ([...t].length === 1) {
    const c = t.codePointAt(0);
    if (c === 0x2d || (c >= 0x2010 && c <= 0x2015) || c === 0x2212) return true;
  }
  if (/(^|[^0-9.])0([^0-9.]|$)/.test(t)) return true;
  return t.startsWith("0.0") || t.startsWith("0%") || t.startsWith("0 ");
}

// --- interface-craft C3 and C5 ------------------------------------------------------------
//
// Both are decided from what the ENGINE painted and from nothing else. That is the whole
// point of them: a rule appended to the page's stylesheet can flatten a figure onto its
// label, or leave a bordered card showing one of its facts, without touching a single
// declaration the source carries - and a grader that matched HTML or CSS text would report
// the page as it was written rather than as it is drawn.
//
// Neither may pass by measuring nothing. A subject query that matches nothing is the
// cheapest wrong outcome a grader like this has, so finding no figure and finding no
// bordered container are both FAILURES here, named as such.

// CHANNELS is clause C3's three, in the order a failure names them.
const CHANNELS = [
  { name: "size", of: (x) => x.size, show: (v) => `${v}px` },
  { name: "weight", of: (x) => x.weight, show: (v) => String(v) },
  { name: "colour", of: (x) => x.color, show: (v) => String(v) },
];

// Clause C3's first half: within a region, a figure differs from the text that names it in
// at least two of rendered size, rendered weight and rendered colour. A region that renders
// no figure at all is UNMEASURED - it is reported by the case that ran this, and no problem
// is raised against it - because a region with nothing to show is not a region with a flat
// hierarchy.
export function gradeFigureStandsApartFromItsLabel(s) {
  const h = s.hierarchy;
  if (!h || !h.regions || h.regions.length === 0) {
    return ["interface-craft C3 (AC-6): the page rendered no region at all, so the hierarchy clause was decided over nothing"];
  }
  const out = [];
  let measured = 0;
  for (const region of h.regions) {
    for (const f of region.figures) {
      if (!f.label) {
        out.push(`interface-craft C3 (AC-1): ${region.what} paints the figure ${f.what} ("${f.text}") and nothing on the screen names it, so there is no label for it to stand apart from`);
        continue;
      }
      measured++;
      const differ = CHANNELS.filter((c) => c.of(f) !== c.of(f.label));
      if (differ.length >= 2) continue;
      const said = CHANNELS.map((c) => `${c.name} ${c.show(c.of(f))} against ${c.show(c.of(f.label))}`).join(", ");
      out.push(`interface-craft C3 (AC-1): in ${region.what} the figure ${f.what} ("${f.text}") differs from its label ${f.label.what} ("${f.label.text}", ${f.label.how}) in ${differ.length} of the three channels (${said}); the clause asks for two`);
    }
  }
  if (measured === 0 && out.length === 0) {
    out.push(`interface-craft C3 (AC-6): no region on this page carried a primary figure, so this grader measured nothing and could not have failed. Regions seen: ${h.regions.map((r) => `${r.what} (${r.runs} runs of text)`).join(", ")}`);
  }
  return out;
}

// Clause C3's second half: the text a reader can SEE is painted at three or more distinct
// sizes. Measured from what the engine computed for each run, so a scale declared in the
// token file and never applied does not count towards it.
export function gradeTypeScaleCarriesThreeSizes(s) {
  const sizes = (s.hierarchy && s.hierarchy.sizes) || [];
  if (sizes.length >= 3) return [];
  return [`interface-craft C3 (AC-2): the page paints its visible text at ${sizes.length} distinct size(s) (${sizes.join(", ")}px); the clause asks for at least three`];
}

// craftContainers is clause C5's subject, decided from the reading rather than chosen by
// name. A container is an element the engine paints ENCLOSING chrome on - a border on all
// four of its OWN sides, or a shadow - that holds something:
//
//   four sides, not one, because a single painted edge is a RULE between two things and
//   not a box drawn around one. The separator under a heading and the line under a table
//   row are exactly that, and C4 is the clause that governs them;
//   a LANDMARK is a region of the page, and how one region is separated from the next is
//   C4's subject too, not C5's;
//   a CONTROL is operated rather than read, and its edge is the affordance that says so,
//   which is how it earns it;
//   and a DRAWING carries no fact - this page's own rule is that every value a mark encodes
//   is also rendered as text in the same container.
//
// A LEAF is NOT excluded, and that is the case the clause is most about: a badge or a
// status pill that paints a box round one word IS "one card per fact", which C5 refuses by
// name. An exclusion for it would remove precisely the worst violations from the subject
// set and leave the grader unable to report the thing it exists to report.
//
// KNOWN BOUND, and it is a bound rather than a reading. The count is of the edges an
// element paints ITSELF, so a box a reader sees closed because its fourth edge belongs to
// its NEIGHBOUR - two bars sitting flush, the upper one's border-bottom closing the lower
// one - is three edges here and is not a subject. That case is filed as its own item with
// the finding that named it and a repro that fails against the served page; deciding it
// needs a reading of whether the painted edges CLOSE a box, which is a different question
// from how many this element drew, and it is deliberately not answered here. Until it
// lands, a contributor can escape this grader by leaving one edge to a neighbour, and
// docs/webui.md says so in the same words.
export function craftContainers(chromedElements) {
  return (chromedElements || []).filter((c) =>
    (c.sides.length === 4 || c.shadow !== "") &&
    !c.landmark && !c.control && !c.graphic);
}

// Clause C5: every bordered or raised container holds at least three facts - three things a
// reader can read off it, rendered as visible text inside it.
export function gradeContainerEarnsItsChrome(s) {
  const subjects = craftContainers(s.chromed);
  if (subjects.length === 0) {
    const seen = (s.chromed || []).length;
    return [`interface-craft C5 (AC-6): the page rendered no bordered or raised container at all, so this grader measured nothing and could not have failed. ${seen} element(s) painted a border or a shadow, and every one of them was a landmark, a control, a drawing, or an element that paints fewer than four of its own edges and no shadow`];
  }
  const out = [];
  for (const c of subjects) {
    if (c.facts.length >= 3) continue;
    const chrome = c.sides.length === 4 ? "a border on all four of its own sides" : `a shadow (${c.shadow})`;
    out.push(`interface-craft C5 (AC-3): ${c.what} draws ${chrome} and holds ${c.facts.length} fact(s) [${c.facts.join(" | ")}]; a container that does not carry three facts has not earned its chrome`);
  }
  return out;
}

// Every predicate this project decides on a LIVE page, by name. The mutation spec drives
// each one against a document built to defeat it.
export function convGraders() {
  return [
    { name: "every run of text clears its contrast floor", probe: gradeTextContrast },
    { name: "every pointer target is at least 24 by 24", probe: gradePointerTargets },
    { name: "no on-surface label runs past fifteen words", probe: gradeLabelLength },
    { name: "each region carries exactly one documentation link", probe: gradeDocLinks },
    { name: "the body never scrolls sideways", probe: gradeBodyNeverScrollsSideways },
    { name: "wide content scrolls inside its own container", probe: gradeWideContentScrollsInItsOwnContainer },
    { name: "data is monospace and prose is the system face", probe: gradeFontSplit },
    { name: "only a raised surface computes a shadow", probe: gradeDepthScale },
    { name: "every painted value came from the token file", probe: gradePaintedValuesComeFromTokens },
    { name: "the page computes motion at all", probe: gradeMotionExists },
    { name: "a figure stands apart from the text that names it", probe: gradeFigureStandsApartFromItsLabel },
    { name: "the painted type scale carries three sizes", probe: gradeTypeScaleCarriesThreeSizes },
    { name: "a bordered or raised container earns its chrome", probe: gradeContainerEarnsItsChrome },
  ];
}

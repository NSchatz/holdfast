// --- whole-ledger aggregates -------------------------------------------------
//
// The queue and history tables are capped by the API, so anything derived from them in
// the browser would be a statistic about the last few hundred files wearing the clothes
// of a statistic about the library. These figures are computed server-side over every
// matching row, and each arrives with the set it covers, the rows excluded for want of a
// recorded value, and whether it could be read at all.
//
// Each one is DRAWN as well as stated, and the drawing is held to three rules:
//
//   1. It is built the way the rows are - a shell cloned from a <template> in this
//      document, with one geometry attribute set per mark from a number the server
//      published. No library, no image, no font, no data: URI, no string assigned to an
//      HTML sink; the response Content-Security-Policy forbids the first four and
//      Trusted Types the last.
//   2. It encodes no value the text beside it does not also carry, in the order the
//      marks are drawn. Delete every drawing from the document and the card still reads.
//      That is what makes the figures answerable with a screen reader, and it is why the
//      drawings are aria-hidden: they would otherwise be read out twice.
//   3. Nothing inside a drawing is told apart from anything else in it by COLOUR. A bar
//      is told apart by its row, its length and the label beside it; a minimum, a mean
//      and a maximum by their position and the height of their tick. Every mark is one
//      token (--mark), chosen for 3:1 against the card face behind it.

// figBar is one bar of a distribution: the mark's length is this bucket's own count as a
// share of the largest count in the SAME figure, which is the only comparison a reader
// can safely make by eye and the only one this drawing offers.
function figBar(share) {
  const svg = tplNode("tpl-fig-bar");
  svg.querySelector(".mark").setAttribute("width", (share * 100).toFixed(4) + "%");
  return svg;
}

// The user-space ends of the spread shell's scale, inset so a 2px stroke centred on
// either end is drawn inside the box rather than half-clipped by it.
const SPREAD_LEFT = 2;
const SPREAD_RIGHT = 998;

// figSpread puts the mean on the scale its own minimum and maximum define. The two ends
// are where the shell already draws them; only the mean moves.
function figSpread(pos) {
  const svg = tplNode("tpl-fig-spread");
  const x = String(SPREAD_LEFT + pos.mean * (SPREAD_RIGHT - SPREAD_LEFT));
  const mean = svg.querySelector(".tick.mean");
  mean.setAttribute("x1", x);
  mean.setAttribute("x2", x);
  return svg;
}

// One card's value renderer per published figure. Each returns DOM nodes; none of them
// invents a number, and each is only ever called when the figure reports data.
function spreadNodes(a, fmt, lead) {
  const out = [mk("b", null, fmt(a.mean))];
  if (lead) out.push(document.createTextNode(" " + lead));
  const pos = spreadPositions(a.min, a.mean, a.max);
  if (pos) out.push(figSpread(pos));
  out.push(spreadKeys(a, fmt));
  out.push(mk("span", "range", "range " + fmt(a.min) + " to " + fmt(a.max) + " across " +
    fmtCount(a.counted) + " files"));
  return out;
}

// spreadKeys is the drawing's text equivalent: the three values the ticks above it stand
// for, named and in the left-to-right order those ticks are drawn.
function spreadKeys(a, fmt) {
  const box = mk("div", "spreadkeys");
  for (const pair of [["min", a.min], ["mean", a.mean], ["max", a.max]]) {
    const key = mk("span", "sk");
    key.appendChild(mk("span", "sn", pair[0]));
    key.appendChild(mk("span", "sv", fmt(pair[1])));
    box.appendChild(key);
  }
  return box;
}

// bucketNodes draws a distribution: one row per bucket, in the order the figure ships
// them, each row carrying its label, its bar and its count. Label and count are the
// drawing's text equivalent and sit in the same row as the mark they describe, so the
// card read straight through yields every value the bars encode.
//
// A figure whose buckets cannot be read at all throws out of readBuckets, and
// renderAggregates turns that into an unavailable card.
function bucketNodes(a, label) {
  const rows = readBuckets(a.buckets);
  const shares = bucketProportions(rows);
  const box = mk("div", "buckets");
  rows.forEach(function (b, i) {
    box.appendChild(mk("span", "bk", label(b.key)));
    // No usable proportion means no mark: an empty cell keeps the grid aligned without
    // drawing a length nobody measured.
    box.appendChild(shares ? figBar(shares[i]) : mk("span", "bn"));
    box.appendChild(mk("span", "bc", fmtCount(b.count)));
  });
  return [box];
}

const AGGREGATES = [
  ["outcomes", "Outcomes", (a) => bucketNodes(a, (k) => k)],
  ["skips_by_guard", "Skips by guard", (a) => bucketNodes(a, guardLabel)],
  ["size_ratio", "Replacement size", (a) => spreadNodes(a, pct, "of the original, mean")],
  ["encode_ms", "Encode time", (a) => spreadNodes(a, fmtDur, "mean")],
  ["vmaf_mean", "VMAF pooled mean", (a) => spreadNodes(a, fmtScore, "mean of per-file means")],
  ["vmaf_min", "VMAF worst frame", (a) => spreadNodes(a, fmtScore, "mean of per-file worst frames")],
];

// aggCard renders ONE figure. An unavailable figure is drawn as unavailable - the card
// stays on the page, saying it could not be read, because a card that disappeared would
// leave the page looking complete while a number was missing from it. It carries no
// drawing either: there is nothing measured to draw.
function aggCard(a, title, nodes) {
  const el = tplNode("tpl-agg");
  el.querySelector(".agg-k").textContent = title;
  const v = el.querySelector(".agg-v");
  const cov = el.querySelector(".agg-cov");
  const ex = el.querySelector(".agg-ex");

  if (!a || a.available !== true) {
    el.classList.add("out");
    v.appendChild(mk("span", "nr", "unavailable"));
    ex.textContent = (a && a.unavailable) ? a.unavailable : "this figure could not be read from the ledger";
    if (a && a.covers) cov.textContent = "over " + a.covers;
    return el;
  }

  const counted = isNum(a.counted) ? a.counted : 0;
  if (counted === 0) {
    // No row contributed a value. Say so - never 0, never an average of 0, never an
    // empty list that reads as a set of zero counts, and never a mark of any length: a
    // drawing of nothing is a drawing of a measurement nobody took.
    v.appendChild(nrNode());
  } else {
    for (const n of nodes(a)) v.appendChild(n);
  }
  cov.textContent = aggCoverageText(a);
  ex.textContent = aggExclusionText(a);
  return el;
}

// renderAggregates draws every card, each one INDEPENDENTLY: a figure that throws while
// rendering becomes an unavailable card and takes nothing else with it. It is also the
// last thing render() does, so the badges, chips, queue and history are already on the
// page before any of this runs.
function renderAggregates(aggs) {
  const host = $("aggregates");
  if (!host) return;
  host.replaceChildren();
  // The view's own three states (F7), decided before a single card is built, and the
  // three inputs that decide them are DIFFERENT FACTS about the wire:
  //
  //   aggregates: {}       the server published the figures and there are none - this
  //                        view has nothing to show, which is the EMPTY state.
  //   aggregates: null     the server could not publish them at all. That is six
  //                        figures that could not be read, and each keeps its card and
  //                        says "unavailable" (F5), because a view that went blank here
  //                        would leave the page looking complete with six numbers gone.
  //   aggregates: 7        not an object: the payload cannot be read (F7).
  //
  // None of the three is ever drawn as a grid of zeroes, and none leaves the loading
  // state on screen for ever.
  if (aggs !== null && aggs !== undefined) {
    if (typeof aggs !== "object" || Array.isArray(aggs)) { setViewState("aggs", "unreadable"); return; }
    if (Object.keys(aggs).length === 0) { setViewState("aggs", "empty"); return; }
  }
  const src = aggs || {};
  for (const [key, title, nodes] of AGGREGATES) {
    let card;
    try {
      card = aggCard(src[key], title, nodes);
    } catch (_) {
      card = aggCard(null, title, nodes);
    }
    host.appendChild(card);
  }
  setViewState("aggs", null);
}

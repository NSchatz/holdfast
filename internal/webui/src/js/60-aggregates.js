// --- whole-ledger aggregates -------------------------------------------------
//
// The queue and history tables are capped by the API, so anything derived from them in
// the browser would be a statistic about the last few hundred files wearing the clothes
// of a statistic about the library. These figures are computed server-side over every
// matching row, and each arrives with the set it covers, the rows excluded for want of a
// recorded value, and whether it could be read at all.

// One card's value renderer per published figure. Each returns DOM nodes; none of them
// invents a number, and each is only ever called when the figure reports data.
function spreadNodes(a, fmt, lead) {
  const out = [mk("b", null, fmt(a.mean))];
  if (lead) out.push(document.createTextNode(" " + lead));
  out.push(mk("span", "range", "range " + fmt(a.min) + " to " + fmt(a.max) + " across " +
    fmtCount(a.counted) + " files"));
  return out;
}
function bucketNodes(a, label) {
  return a.buckets.map((b) => {
    const row = mk("div", "bucket");
    row.appendChild(mk("span", null, label(b.key)));
    row.appendChild(mk("span", "c", fmtCount(b.count)));
    return row;
  });
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
// leave the page looking complete while a number was missing from it.
function aggCard(a, title, nodes) {
  const el = $("tpl-agg").content.cloneNode(true).firstElementChild;
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
    // No row contributed a value. Say so - never 0, never an average of 0, and never an
    // empty list that reads as a set of zero counts.
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
}

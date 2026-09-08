// The cells. Each takes the value its derivation produced and puts it on screen; an
// absent derivation is rendered as the page's honest absence, never as a zero.

// The size cell: before → after with the percent reclaimed. Only meaningful on a done
// row that recorded both sizes.
function sizeCell(td, j) {
  const f = sizeFigures(j);
  if (!f) { td.appendChild(nrNode()); return; }
  td.appendChild(mk("span", "before", f.before));
  td.appendChild(mk("span", "arrow", "→"));
  td.appendChild(document.createTextNode(f.after));
  td.appendChild(mk("span", "pct", f.reduction));
}

// The VMAF cell states exactly what the number licenses and no more: the two pooled
// statistics (harmonic mean AND worst frame), the model that produced them, its pooling,
// its luma-only blind spot, and that it was measured against the operator's own source.
// It never grades the result, never claims perfect fidelity, and never compares files.
function vmafCell(td, j) {
  const f = vmafFigures(j);
  if (!f) { td.appendChild(nrNode()); return; }
  const pair = mk("span", "pair");
  pair.appendChild(mk("span", "mean", f.mean));
  pair.appendChild(document.createTextNode(" mean · "));
  pair.appendChild(mk("span", "worst", f.worst));
  pair.appendChild(document.createTextNode(" worst frame"));
  td.appendChild(pair);
  td.appendChild(mk("span", "cond", f.condition));
}

// The result cell: the status, plus WHY for the two states that have a reason. A skipped
// row names the guard; a failed one shows the error text verbatim.
function resultCell(td, j) {
  const st = mk("span", "st st-" + j.status);
  st.appendChild(mk("span", "dot"));
  st.appendChild(document.createTextNode(j.status));
  td.appendChild(st);
  if (j.status === "skipped" && j.reason) {
    td.appendChild(mk("div", "reason", guardLabel(j.reason)));
  } else if (j.status === "failed") {
    td.appendChild(mk("div", "reason fail", j.reason ? j.reason : "reason not recorded"));
  }
}

// The progress cell for a queue row: the derivation's figure, or the page's ONE absence
// phrase (F3). A row that is not encoding has no measurement of a fraction, and a running
// encode whose encoder has reported nothing usable has none either - to a reader those
// are the same fact, nobody measured this, so both read as that one phrase rather than as
// a blank cell a reader could take for a zero or for a figure that failed to draw.
function progressCell(td, j) {
  const f = progressFigure(j);
  if (!f || f.unknown) {
    td.appendChild(nrNode());
    return;
  }
  td.appendChild(mk("span", "pctv", f.percent));
  if (f.of) td.appendChild(mk("span", "of", f.of));
}

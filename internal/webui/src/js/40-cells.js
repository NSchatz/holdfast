// The cells. Each takes the value its derivation produced and puts it on screen; an
// absent derivation is rendered as the page's honest absence, never as a zero.

// The path cell. The file's own NAME leads and the directory that led to it recedes
// above it, because both tables are read by scanning for a file and an unbroken column of
// absolute paths hides the one segment a reader is looking for. Nothing is elided and
// nothing is added: the two parts concatenate back to the path the server sent, so the
// cell still carries it whole for anyone copying it out.
function pathCell(td, path) {
  const parts = pathParts(path);
  if (parts.dir) td.appendChild(mk("span", "pdir", parts.dir));
  if (parts.name) td.appendChild(mk("span", "pname", parts.name));
  if (!parts.dir && !parts.name) td.appendChild(nrNode());
}

// The size cell: before → after with the percent reclaimed. Only meaningful on a done
// row that recorded both sizes.
function sizeCell(td, j) {
  // A CANDIDATE row has a source and no output: nothing has encoded that file. So the cell
  // is the SOURCE's own size, named as that, and never a before-and-after - and there is
  // no percentage, because a share of a saving nobody has measured is not a figure this
  // page is allowed to draw.
  if (j && j.status === CANDIDATE_STATUS) {
    const size = sourceSizeText(j);
    if (size === NOT_RECORDED) { td.appendChild(nrNode()); return; }
    td.appendChild(mk("span", "srcsize", size));
    td.appendChild(mk("span", "srck", "source"));
    return;
  }
  const f = sizeFigures(j);
  if (!f) { td.appendChild(nrNode()); return; }
  td.appendChild(mk("span", "before", f.before));
  td.appendChild(mk("span", "arrow", "→"));
  // The replacement's size is an element rather than a bare text node so the column can
  // give the two figures a shape: a before-and-after read down a column is read by its
  // ARROWS, and an arrow that moves from row to row is a column with no shape at all.
  td.appendChild(mk("span", "after", f.after));
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
  } else if (j.status === "indeterminate") {
    td.appendChild(mk("div", "reason fail", j.reason ? j.reason : "reason not recorded"));
    td.appendChild(mk("div", "cond",
      "both files are intact and held; run `holdfast resolve` to record what happened"));
  } else if (j.status === "applied-despite-error") {
    td.appendChild(mk("div", "reason fail", j.reason ? j.reason : "reason not recorded"));
    td.appendChild(mk("div", "cond",
      "the rename took effect despite the error - the file at that path is the replacement"));
  } else if (j.status === CANDIDATE_STATUS) {
    // The source's CODEC, on the row, as selectable text. It is the other half of what an
    // operator sizing a dry run needs - the size beside it says how much, this says what
    // of - and a candidate list that omitted it would send them off to probe the files
    // themselves. A codec nobody read renders as the page's one absence phrase, never as
    // a blank a reader could take for "no video".
    const codec = mk("div", "cond");
    codec.appendChild(document.createTextNode("source codec: "));
    const value = codecText(j.source_codec);
    codec.appendChild(value === NOT_RECORDED ? nrNode() : mk("span", "codec", value));
    td.appendChild(codec);
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
  // The same measurement, drawn. It is the figure the percentage above it was rounded
  // from - not a second arithmetic - and it carries no value that text does not, so a
  // reader who cannot see it has lost nothing. It is aria-hidden for that reason: the
  // percentage is already there to be read.
  td.appendChild(figBar(f.fraction));
  if (f.of) td.appendChild(mk("span", "of", f.of));
}

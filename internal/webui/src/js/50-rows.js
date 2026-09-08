// The rows. Each is cloned from a <template> in the document and filled cell by cell, so
// the row's shape is markup the author wrote and its content is text the browser can only
// ever treat as text.

// refreshElapsed recomputes every active row's in-state age from the transition
// timestamp the row carries. It runs on render AND on a timer, which is what makes the
// figure move with no page reload and no state transition.
function refreshElapsed() {
  // A severed feed must not keep a figure reading as live (F6). Elapsed is the one
  // figure on this page that would otherwise go on advancing with nothing behind it:
  // it is recomputed on a timer from each row's own transition timestamp, and that
  // timer knows nothing about the stream. So the ticker freezes the instant the stream
  // is no longer live, and the ages a reader sees stay at the last moment the page had
  // a server behind them. The connection state in the header says why.
  if (!streamIsLive) return;
  const now = serverNow();
  for (const td of $("queue").querySelectorAll("td.elapsed")) {
    const age = elapsedText(now, td.dataset.since);
    // No usable basis is an absence, not an empty cell (F3).
    if (age === null) td.replaceChildren(nrNode());
    else td.textContent = age;
  }
}

function queueRow(j) {
  const tr = $("tpl-queue-row").content.cloneNode(true).firstElementChild;
  tr.dataset.path = (j.path || "").toLowerCase();
  tr.querySelector(".path").textContent = j.path;
  tr.querySelector(".st").classList.add("st-" + j.status);
  tr.querySelector(".stlabel").textContent = j.status;
  // The elapsed basis is the transition timestamp already on the wire; the cell itself
  // is filled by refreshElapsed, so the row and the ticker can never disagree.
  tr.querySelector(".elapsed").dataset.since = String(j.updated_at || 0);
  progressCell(tr.querySelector(".prog"), j);
  const worker = tr.querySelector(".worker");
  if (j.worker) worker.textContent = j.worker;
  else worker.appendChild(nrNode());
  return tr;
}

function histRow(j) {
  const tr = $("tpl-hist-row").content.cloneNode(true).firstElementChild;
  tr.dataset.path = (j.path || "").toLowerCase();
  tr.querySelector(".path").textContent = j.path;
  resultCell(tr.querySelector(".st"), j);
  // EVERY cell of a history row is filled, in every status. A skipped or failed row
  // recorded no size, no score, no encoder and no encode time, and F3 says that reads
  // as the page's one absence phrase in the field itself - not as a blank a reader is
  // left to interpret. The cell renderers already answer an absent derivation that way,
  // so they are called unconditionally rather than only on a done row.
  sizeCell(tr.querySelector(".size"), j);
  vmafCell(tr.querySelector(".vmaf"), j);
  const enc = tr.querySelector(".enc");
  if (j.encoder) enc.textContent = j.encoder;
  else enc.appendChild(nrNode());
  const dur = tr.querySelector(".dur");
  if (isNum(j.encode_ms)) dur.textContent = fmtDur(j.encode_ms);
  else dur.appendChild(nrNode());
  const upd = tr.querySelector(".upd");
  if (isNum(j.updated_at) && j.updated_at > 0) upd.textContent = fmtTime(j.updated_at);
  else upd.appendChild(nrNode());
  return tr;
}

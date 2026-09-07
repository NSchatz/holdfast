// The rows. Each is cloned from a <template> in the document and filled cell by cell, so
// the row's shape is markup the author wrote and its content is text the browser can only
// ever treat as text.

// refreshElapsed recomputes every active row's in-state age from the transition
// timestamp the row carries. It runs on render AND on a timer, which is what makes the
// figure move with no page reload and no state transition.
function refreshElapsed() {
  const now = serverNow();
  for (const td of $("queue").querySelectorAll("td.elapsed")) {
    const age = elapsedText(now, td.dataset.since);
    td.textContent = age === null ? "" : age;
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
  tr.querySelector(".worker").textContent = j.worker || "";
  return tr;
}

function histRow(j) {
  const tr = $("tpl-hist-row").content.cloneNode(true).firstElementChild;
  tr.dataset.path = (j.path || "").toLowerCase();
  tr.querySelector(".path").textContent = j.path;
  resultCell(tr.querySelector(".st"), j);
  if (j.status === "done") sizeCell(tr.querySelector(".size"), j);
  if (j.status === "done") vmafCell(tr.querySelector(".vmaf"), j);
  const enc = tr.querySelector(".enc");
  if (j.encoder) enc.textContent = j.encoder;
  else if (j.status === "done") enc.appendChild(nrNode());
  const dur = tr.querySelector(".dur");
  if (isNum(j.encode_ms)) dur.textContent = fmtDur(j.encode_ms);
  else if (j.status === "done") dur.appendChild(nrNode());
  tr.querySelector(".upd").textContent = j.updated_at ? fmtTime(j.updated_at) : "";
  return tr;
}

function emptyRow(cols, text) {
  const tr = document.createElement("tr");
  tr.dataset.empty = "1";
  const td = mk("td", "empty", text);
  td.colSpan = cols;
  tr.appendChild(td);
  return tr;
}

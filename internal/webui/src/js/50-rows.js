// The rows. Each is cloned from a <template> in the document and filled cell by cell, so
// the row's shape is markup the author wrote and its content is text the browser can only
// ever treat as text.

// --- reconciling rows, rather than rebuilding them ----------------------------------
//
// The page renders a whole snapshot at a time, and it used to do that by emptying each
// tbody and building every row again. That is correct and it is invisible - until an
// operator tries to USE the page while holdfast is working. Every snapshot destroyed
// every row NODE, so a path being selected to copy vanished mid-gesture, and snapshots
// arrive on every job transition. Reading a failing file's path off this dashboard during
// a scan was not difficult, it was impossible.
//
// So a row is now KEYED by its path and carries a signature of everything it renders. A
// row whose signature is unchanged is left alone - the same node, with the same selection
// and the same scroll position in it - and only a row that actually changed is replaced.
// Rows are then ordered by moving nodes rather than by recreating them.
//
// The signature is taken from the JOB, not from the row built out of it - the whole DTO
// the server sent, serialised. Two properties matter and they pull in opposite directions:
//
//   it can never be under-sensitive. A row is a pure function of its job, so a signature
//   over the whole job changes whenever anything the row could render changes. A list of
//   "the fields this row uses" would be a second place to remember, and the first field
//   somebody adds without updating it is a row that silently stops refreshing - which is
//   a stale figure presented as a live one, the exact thing F6 refuses.
//
//   it may be over-sensitive, and that costs nothing. A field the row does not render
//   changing rebuilds that one row for no visible reason. Nobody can see it.
//
// It is deliberately NOT taken by serialising the built row back to markup. That would
// mean touching one of the HTML-string sinks this page's render idiom refuses, and the
// idiom refuses them by NAME rather than by intent: the sweep inside `make check` cannot
// tell a read from a write, and it should not have to - a rule that admitted "reading one
// is fine" would be a rule with a hole in it that the next person has to know about.
function stamp(tr, key, job) {
  tr.dataset.key = key;
  tr.dataset.sig = JSON.stringify(job);
  return tr;
}

// syncRows brings one tbody into agreement with the rows a snapshot describes, touching
// as little of the document as it can.
function syncRows(body, items, build) {
  const have = new Map();
  for (const tr of Array.from(body.children)) {
    // A state row (loading / empty / unreadable) speaks for the whole table and is not a
    // row of data; it is removed here and re-added by setViewState if it still applies.
    if (tr.dataset && tr.dataset.key && !tr.dataset.state) have.set(tr.dataset.key, tr);
    else tr.remove();
  }
  let i = 0;
  for (const j of items) {
    const built = build(j);
    const existing = have.get(built.dataset.key);
    let tr = built;
    if (existing) {
      have.delete(built.dataset.key);
      // Unchanged: keep the node a reader may be part-way through interacting with.
      if (existing.dataset.sig === built.dataset.sig) tr = existing;
    }
    const at = body.children[i];
    if (at !== tr) body.insertBefore(tr, at || null);
    i++;
  }
  // Everything the snapshot no longer describes has been pushed past the rows it does.
  while (body.children.length > items.length) body.removeChild(body.lastChild);
}

// labelCells copies each column's own header onto the cells beneath it, so that a
// viewport too narrow for seven columns can stack a row and still say which value is
// which. The label is read from the table's OWN <th>, never written a second time in a
// stylesheet or a constant: a column header restated somewhere else agrees with the table
// today and disagrees with it the first time either moves, which is the drift this
// repository has spent whole phases refusing.
//
// It sets an attribute and nothing else. The cell's text is untouched, so every reader of
// these rows - the filter, the graders, an operator copying a path - still sees exactly
// what the server sent.
function labelCells(tr, headers) {
  const tds = tr.children;
  for (let i = 0; i < tds.length && i < headers.length; i++) {
    tds[i].dataset.label = headers[i];
  }
}

// headersOf reads one table's column headers once per render, in document order.
function headersOf(tbodyId) {
  const body = $(tbodyId);
  const table = body ? body.closest("table") : null;
  if (!table) return [];
  return Array.prototype.map.call(table.querySelectorAll("thead th"), function (th) {
    return th.textContent.trim();
  });
}

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

function queueRow(j, headers) {
  const tr = $("tpl-queue-row").content.cloneNode(true).firstElementChild;
  tr.dataset.path = (j.path || "").toLowerCase();
  pathCell(tr.querySelector(".path"), j.path);
  tr.querySelector(".st").classList.add("st-" + j.status);
  tr.querySelector(".stlabel").textContent = j.status;
  // The elapsed basis is the transition timestamp already on the wire; the cell itself
  // is filled by refreshElapsed, so the row and the ticker can never disagree.
  tr.querySelector(".elapsed").dataset.since = String(j.updated_at || 0);
  progressCell(tr.querySelector(".prog"), j);
  const worker = tr.querySelector(".worker");
  if (j.worker) worker.textContent = j.worker;
  else worker.appendChild(nrNode());
  if (headers) labelCells(tr, headers);
  return stamp(tr, "q:" + (j.path || ""), j);
}

function histRow(j, headers) {
  const tr = $("tpl-hist-row").content.cloneNode(true).firstElementChild;
  tr.dataset.path = (j.path || "").toLowerCase();
  pathCell(tr.querySelector(".path"), j.path);
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
  if (headers) labelCells(tr, headers);
  return stamp(tr, "h:" + (j.path || ""), j);
}

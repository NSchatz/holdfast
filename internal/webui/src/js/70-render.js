// The whole-page render. One snapshot in, the page it describes out.

// bytesInto writes one byte total into its field: the figure when the server recorded
// one, and the page's ONE absence phrase when it did not. Never 0 B for a total nobody
// measured, which is exactly what a zeroed counter would look like (F3).
function bytesInto(id, v) {
  const el = $(id);
  if (!el) return;
  if (isNum(v) && v >= 0) el.textContent = fmtBytes(v);
  else el.replaceChildren(nrNode());
}

// Surface the API's silent row caps: it ships at most a fixed number of queue / history
// rows, so a truncated view could read as the whole ledger.
function capNote(id, shown, total) {
  const el = $(id);
  const text = capNoteText(shown, total);
  el.textContent = text;
  el.hidden = text === "";
}

// Client-side path filter over the rows already loaded (which are themselves capped -
// see capNote). Hides non-matching rows in both tables; empty term shows all.
function applyFilter() {
  const term = $("filter").value.trim().toLowerCase();
  for (const body of [$("queue"), $("history")]) {
    for (const tr of body.children) {
      if (tr.dataset.empty) continue;
      tr.hidden = term !== "" && !(tr.dataset.path || "").includes(term);
    }
  }
}

// The polite screen-reader summary, updated only when it changes so a snapshot that
// shifts nothing stays silent.
function announce(sum) {
  const msg = announceText(sum);
  const el = $("sr-status");
  if (el.textContent !== msg) el.textContent = msg;
}

function render(snap) {
  // A payload that is not a snapshot object at all cannot be read, and says so in every
  // view rather than being rendered as a page full of zeroes (F7).
  if (!snap || typeof snap !== "object" || Array.isArray(snap)) { markUnreadable(); return; }

  // Re-anchor the elapsed basis to the server's clock as of this frame.
  if (isNum(snap.now)) clockOffset = clockOffsetFrom(snap.now, Date.now());

  // Badges.
  const bp = $("b-paused");
  bp.textContent = snap.paused ? "paused" : "running";
  bp.classList.toggle("on", !!snap.paused);
  const bs = $("b-scan");
  bs.textContent = snap.scanning ? "scanning" : "idle";
  bs.classList.toggle("on", !!snap.scanning);

  // Reclaimed: the durable lifetime figure leads; the per-run figure is the subline.
  // A total nobody recorded reads as the page's one absence phrase, never as 0 B (F3).
  bytesInto("reclaimed-lifetime", snap.bytes_reclaimed_lifetime);
  bytesInto("reclaimed-session", snap.bytes_reclaimed_session);

  // Pause is meaningless when already paused; Resume is meaningless when running.
  $("pause").disabled = !!snap.paused;
  $("resume").disabled = !snap.paused;

  // Summary chips. A status the summary does not name has NO ROW in that state - the
  // server's own count is a `GROUP BY status`, which omits a status with nothing in it -
  // so 0 here is a measured zero and is rendered as one. A summary that is not an object
  // at all is a different fact: nothing was counted, and the view says it could not be
  // read rather than drawing seven zeroes nobody measured (F3).
  const chips = $("chips");
  chips.replaceChildren();
  const sum = snap.summary;
  const sumOK = !!sum && typeof sum === "object" && !Array.isArray(sum);
  if (sumOK) {
    let counted = 0;
    for (const s of STATUSES) {
      const n = isNum(sum[s]) ? sum[s] : 0;
      counted += n;
      const chip = mk("div", "chip " + s);
      chip.appendChild(mk("div", "n", String(n)));
      chip.appendChild(mk("div", "k", s));
      chips.appendChild(chip);
    }
    // The counts view has nothing to show when the ledger holds no row in ANY state,
    // which is the empty state F7 requires of it. The chips still read their measured
    // zeroes beside it.
    setViewState("counts", counted > 0 ? null : "empty");
  } else {
    setViewState("counts", "unreadable");
  }

  // Queue.
  const q = Array.isArray(snap.queue) ? snap.queue : [];
  const qbody = $("queue");
  qbody.replaceChildren();
  for (const j of q) qbody.appendChild(queueRow(j));
  setViewState("queue", q.length ? null : "empty");
  refreshElapsed();

  // History - the trust surface: per file, the evidence its swap was safe.
  const h = Array.isArray(snap.history) ? snap.history : [];
  const hbody = $("history");
  hbody.replaceChildren();
  for (const j of h) hbody.appendChild(histRow(j));
  setViewState("history", h.length ? null : "empty");

  // Honest row-cap notices. The total is the one the SERVER reported for each table -
  // counted over every matching row in the ledger - and never one this page derived from
  // the summary counts, which answer a different question and cannot see the cap.
  capNote("queue-cap", q.length, snap.queue_total);
  capNote("hist-cap", h.length, snap.history_total);

  announce(sumOK ? sum : {});
  applyFilter();

  // Last, and guarded: the tables above are already drawn, so nothing that happens
  // inside the aggregate cards can cost the operator the rest of the page.
  try { renderAggregates(snap.aggregates); } catch (_) { setViewState("aggs", "unreadable"); }

  // One whole snapshot has now been through the page. From here a stream that dies is
  // STALE, not unreadable: the rows stay and the connection state alone says so (F6).
  renderedOnce = true;
}

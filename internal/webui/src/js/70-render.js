// The whole-page render. One snapshot in, the page it describes out.

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
  $("reclaimed-lifetime").textContent = fmtBytes(snap.bytes_reclaimed_lifetime);
  $("reclaimed-session").textContent = fmtBytes(snap.bytes_reclaimed_session);

  // Pause is meaningless when already paused; Resume is meaningless when running.
  $("pause").disabled = !!snap.paused;
  $("resume").disabled = !snap.paused;

  // Summary chips.
  const sum = snap.summary || {};
  const chips = $("chips");
  chips.replaceChildren();
  for (const s of STATUSES) {
    const chip = mk("div", "chip " + s);
    chip.appendChild(mk("div", "n", String(isNum(sum[s]) ? sum[s] : 0)));
    chip.appendChild(mk("div", "k", s));
    chips.appendChild(chip);
  }

  // Queue.
  const q = snap.queue || [];
  const qbody = $("queue");
  qbody.replaceChildren();
  if (!q.length) qbody.appendChild(emptyRow(5, "Nothing queued."));
  else for (const j of q) qbody.appendChild(queueRow(j));
  refreshElapsed();

  // History - the trust surface: per file, the evidence its swap was safe.
  const h = snap.history || [];
  const hbody = $("history");
  hbody.replaceChildren();
  if (!h.length) hbody.appendChild(emptyRow(7, "No history yet."));
  else for (const j of h) hbody.appendChild(histRow(j));

  // Honest row-cap notices (the summary counts are the authoritative totals).
  capNote("queue-cap", q.length, sumStatuses(sum, QUEUE_STATUSES));
  capNote("hist-cap", h.length, sumStatuses(sum, TERMINAL_STATUSES));

  announce(sum);
  applyFilter();

  // Last, and guarded: the tables above are already drawn, so nothing that happens
  // inside the aggregate cards can cost the operator the rest of the page.
  try { renderAggregates(snap.aggregates); } catch (_) {}
}

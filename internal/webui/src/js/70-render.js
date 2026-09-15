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

// candidateNote is the total SOURCE BYTES the rendered candidates account for, the set it
// was taken over, and the rows it had to leave out.
//
// The whole block goes when there is no candidate row to total, which is the F3 answer
// rather than a tidiness: a "0 B under consideration" beside an empty candidate list reads
// as "these files are worth nothing", and nobody has looked at any files. The exclusion
// line goes when there is nothing to exclude, exactly as an aggregate card's does.
//
// There is deliberately no projected saving here and none in the rows. Nothing has encoded
// these files, so no honest figure exists for what they would give back; a projection
// would be read as a measurement, which is the overclaim this whole surface refuses.
function candidateNote(rows) {
  const host = $("cand");
  if (!host) return;
  const t = candidateTotal(rows);
  host.hidden = !t;
  if (!t) return;
  bytesInto("cand-bytes", t.bytes);
  $("cand-cov").textContent = candidateCoverageText(t);
  const ex = $("cand-ex");
  const exText = candidateExclusionText(t);
  ex.textContent = exText;
  ex.hidden = exText === "";
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

// --- the ledger search's own region -------------------------------------------------

// showFound reveals the search region and puts its results view into one of the three
// states, or clears the state when it has rows of its own to show.
//
// The region is ABSENT until a search has been made, which is why the shell ships this
// view with no state element in it: before anybody asks a question there is nothing to be
// loading, nothing to be empty of and nothing that could not be read, and a results table
// standing there in one of those states would be the page answering a question nobody put.
function showFound(state) {
  const host = $("found");
  if (!host) return;
  host.hidden = false;
  setViewState("search", state);
}

// renderSearchResults draws what one ledger search found: the rows in their own region,
// never merged into either capped table, and the count over the WHOLE ledger beside them.
function renderSearchResults(payload) {
  const body = $("search-results");
  if (!body) return;
  const rows = (payload && Array.isArray(payload.results)) ? payload.results : [];
  const headers = headersOf("search-results");
  syncRows(body, rows, (j) => searchRow(j, headers));
  const note = $("found-count");
  if (note) note.textContent = searchCountText(rows.length, payload ? payload.total : null);
  showFound(rows.length ? null : "empty");
}

// searchRefused is what the page shows when the search was REFUSED rather than answered.
// It is deliberately not the empty state: "nothing matched" and "nobody was allowed to
// ask" are different facts, and a page that rendered an empty result set for the second
// would tell an operator their file is not in the ledger when nothing looked.
function searchRefused(why) {
  const body = $("search-results");
  if (body) syncRows(body, [], (j) => searchRow(j, null));
  const note = $("found-count");
  if (note) note.textContent = why;
  showFound("unreadable");
}

// --- the paths this daemon is withholding ---------------------------------------------

// renderHeld draws every withholding in force, each with the control that removes it.
//
// The region is absent while there is nothing to say - no withheld path and no refusal to
// report - and present the moment either exists, which is what carries the one sentence
// about where these records live to a reader who is looking at one.
function renderHeld(list, message) {
  const host = $("held");
  const ul = $("held-list");
  const msg = $("held-msg");
  if (!host || !ul || !msg) return;
  const rows = Array.isArray(list) ? list : [];
  ul.replaceChildren();
  for (const e of rows) {
    const path = (e && typeof e.path === "string") ? e.path : "";
    if (!path) continue;
    const li = tplNode("tpl-held-row");
    li.querySelector(".heldpath").textContent = path;
    const b = li.querySelector(".heldgo");
    b.textContent = "Stop withholding";
    b.dataset.release = path;
    ul.appendChild(li);
  }
  msg.textContent = message || "";
  msg.hidden = !message;
  host.hidden = ul.children.length === 0 && !message;
}

// markChipGroupBreak decides whether the group boundary is drawn at all.
//
// The boundary between work IN HAND and work FINISHED is a rule to the left of the first
// terminal chip. A rule divides two things - so when the row wraps between the two groups
// there is nothing to its left, and what a reader sees is a line hanging in the margin
// beside a chip that is also indented for no reason. That happens at any width where the
// nine chips break at exactly that point.
//
// CSS cannot ask whether two flex items share a line, so the page asks AFTER layout and
// records the answer on the element; the stylesheet draws the rule only when they do. It
// is a comparison with a tolerance rather than an equality because sub-pixel layout can
// put two boxes on one line a fraction of a pixel apart.
function markChipGroupBreak() {
  const chips = $("chips");
  if (!chips) return;
  const first = chips.querySelector(".chip.terminal");
  if (!first) return;
  const prev = first.previousElementSibling;
  const together = !!prev &&
    Math.abs(prev.getBoundingClientRect().top - first.getBoundingClientRect().top) < 2;
  first.classList.toggle("linestart", !together);
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
  // Space a swap has not yet given back. It is published as a POINTER precisely so that
  // an unreadable figure and a figure of zero are two different answers, and bytesInto
  // carries that distinction to the screen: null renders the page's one absence phrase,
  // never a 0 a reader would take for an empty retention area.
  bytesInto("bytes-held", snap.bytes_held_by_undo_window);

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
      // The chip's GROUP is on the chip itself rather than in a wrapper element, so
      // #chips keeps one flat list of children: a reader is told apart what is in hand
      // from what is finished by a rule drawn before the first terminal chip, and nothing
      // that reads this view has to descend through a box to find a count.
      const chip = mk("div", "chip " + s + (IN_FLIGHT.indexOf(s) >= 0 ? " inflight" : " terminal"));
      chip.appendChild(mk("div", "n", String(n)));
      chip.appendChild(mk("div", "k", s));
      chips.appendChild(chip);
    }
    // Where the two groups fall is a question about LAYOUT, so it is asked once the chips
    // are in the document rather than while they are being built.
    markChipGroupBreak();
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
  const qheaders = headersOf("queue");
  // Reconciled, never rebuilt wholesale: a row the snapshot did not change keeps its own
  // node, and with it whatever a reader was part-way through doing to it.
  syncRows(qbody, q, (j) => queueRow(j, qheaders));
  setViewState("queue", q.length ? null : "empty");
  refreshElapsed();

  // History - the trust surface: per file, the evidence its swap was safe.
  const h = Array.isArray(snap.history) ? snap.history : [];
  const hbody = $("history");
  const hheaders = headersOf("history");
  syncRows(hbody, h, (j) => histRow(j, hheaders));
  setViewState("history", h.length ? null : "empty");
  // What the candidates add up to, over the rows just rendered.
  candidateNote(h);

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

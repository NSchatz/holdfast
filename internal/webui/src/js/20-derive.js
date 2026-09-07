// The dashboard's VALUE DERIVATIONS. Every function here maps wire data to the value a
// cell shows, and NONE of them touches the DOM - that is what makes them exercisable in
// a plain JavaScript runtime, one input at a time, without standing up the page.
//
// The absence rule, which is the store's own invariant carried to the screen: a pointer
// field arrives as JSON null when the fact was never recorded, and a reader must render
// that AS not recorded - never as 0, never as NaN, never as the string "undefined". So a
// derivation handed an absent, null, non-numeric, non-finite or negative value answers
// with the page's honest absent result for its own type: NOT_RECORDED for the string
// formatters, null for the roll-up and the composite cell derivations, false for the
// predicate, and the "unknown" progress figure for a running encode with no measurement.
// The cell renderers then map that absence to exactly what the page has always shown.
const NOT_RECORDED = "not recorded";

// A pointer field arrives as JSON null when the fact was never recorded. Render that
// honestly - never as 0, never as an invented value.
function isNum(v) { return typeof v === "number" && isFinite(v); }

function fmtBytes(n) {
  if (!isNum(n) || n < 0) return NOT_RECORDED;
  const u = ["B","KB","MB","GB","TB","PB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + " " + u[i];
}
function fmtTime(sec) {
  if (!isNum(sec) || sec <= 0) return NOT_RECORDED;
  const d = new Date(sec * 1000);
  return d.toLocaleString();
}
function fmtDur(ms) {
  if (!isNum(ms) || ms < 0) return NOT_RECORDED;
  if (ms < 1000) return ms + " ms";
  let s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60); s = s % 60;
  if (m < 60) return m + "m " + s + "s";
  const h = Math.floor(m / 60);
  return h + "h " + (m % 60) + "m";
}
// fmtSpan renders a duration in seconds as a compact H/M/S span. Used for how long a job
// has been in its state and for a position within the source.
function fmtSpan(sec) {
  if (!isNum(sec) || sec < 0) return NOT_RECORDED;
  sec = Math.floor(sec);
  const h = Math.floor(sec / 3600), m = Math.floor(sec / 60) % 60, s = sec % 60;
  if (h) return h + "h " + m + "m";
  if (m) return m + "m " + s + "s";
  return s + "s";
}
function pct(x) { return isNum(x) ? Math.round(x * 100) + "%" : NOT_RECORDED; }
function fmtScore(x) { return isNum(x) ? Number(x).toFixed(1) : NOT_RECORDED; }
// The count an aggregate states its spread across. Absent is stated, never printed as NaN.
function fmtCount(x) { return isNum(x) ? Number(x).toLocaleString() : NOT_RECORDED; }

// The status roll-up behind each table's cap notice: how many rows the ledger holds in
// the states this table shows. A summary that is not an object cannot be rolled up, and
// says so with null rather than reporting a total of 0 nobody counted.
function sumStatuses(sum, keys) {
  if (!sum || typeof sum !== "object" || !Array.isArray(keys)) return null;
  return keys.reduce((a, s) => a + (isNum(sum[s]) ? sum[s] : 0), 0);
}

// The offset between this page's clock and the server's, taken from the snapshot's own
// `now` field. Every elapsed figure is DERIVED from it on each tick - never accumulated
// in a counter here. Two reasons, both load-bearing: a background tab's timers are
// throttled by policies with no normative guarantee, so a counter ticking in the page
// would silently drift while a value recomputed from the transition timestamp cannot;
// and a client whose clock is skewed against the server's would otherwise read a
// nonsensical (or negative) age straight off updated_at.
let clockOffset = 0;
function clockOffsetFrom(serverSeconds, clientMillis) { return serverSeconds - clientMillis / 1000; }
function serverNow() { return Date.now() / 1000 + clockOffset; }

// elapsedText is the in-state age one queue row shows: the server's clock now, less the
// transition timestamp that row carries on the wire. A row with no usable basis has no
// age to show and says so with null, which the cell renders as an empty cell.
function elapsedText(now, since) {
  since = Number(since);
  if (!isNum(since) || since <= 0 || !isNum(now)) return null;
  return fmtSpan(Math.max(0, now - since));
}

// The size figures a done row shows: before, after, and the percent reclaimed. Null when
// either size was never recorded, so the cell reads "not recorded" and never 0%.
function sizeFigures(j) {
  if (!isNum(j.source_bytes) || !isNum(j.output_bytes)) return null;
  // Clamp at 0 for the same reason the server does (Event.BytesReclaimed / ReclaimedTotal):
  // the strictly-smaller gate precludes output > source, but a defensive clamp means a
  // future bug there can never render a nonsensical negative "% smaller".
  const saved = Math.max(0, j.source_bytes - j.output_bytes);
  const share = j.source_bytes > 0 ? Math.round((saved / j.source_bytes) * 100) : 0;
  return {
    before: fmtBytes(j.source_bytes),
    after: fmtBytes(j.output_bytes),
    reduction: share + "% smaller",
  };
}

// The VMAF figures a done row shows, and exactly what they license: the two pooled
// statistics (harmonic mean AND worst frame), the model that produced them, its pooling,
// its luma-only blind spot, and that it was measured against the operator's own source.
// It never grades the result, never claims perfect fidelity, and never compares files.
function vmafFigures(j) {
  if (!isNum(j.vmaf_mean) && !isNum(j.vmaf_min)) return null;
  const model = j.vmaf_model ? String(j.vmaf_model).replace(/^version=/, "") : "unspecified model";
  return {
    mean: isNum(j.vmaf_mean) ? fmtScore(j.vmaf_mean) : "?",
    worst: isNum(j.vmaf_min) ? fmtScore(j.vmaf_min) : "?",
    condition: "model " + model + " · harmonic-mean + worst-frame pooling · luma-only · measured vs your source",
  };
}

// The progress figure for a queue row. A figure exists ONLY when the row is a RUNNING
// encode AND that encoder reported a position AND the source duration is known; a running
// encode with no usable figure is "unknown", and every other state has no progress to
// have at all (null). There is no interpolation, no estimate from elapsed time, and no
// carry-over of the last figure - an absent measurement is absent, including the instant
// the encode ends and the verify begins.
function progressFigure(j) {
  if (j.status !== PROGRESS_STATUS) return null;
  if (!isNum(j.progress_fraction)) return { unknown: true };
  const frac = Math.min(1, Math.max(0, j.progress_fraction));
  const out = { unknown: false, percent: Math.round(frac * 100) + "%" };
  if (isNum(j.progress_seconds) && isNum(j.progress_duration_seconds)) {
    out.of = fmtSpan(Math.min(j.progress_seconds, j.progress_duration_seconds))
      + " of " + fmtSpan(j.progress_duration_seconds);
  }
  return out;
}

// Human label for one skip guard. An unknown token falls back to itself, so a new guard
// is never hidden behind a blank.
function guardLabel(k) { return GUARD_LABELS[k] || k; }

// Surface the API's silent row caps: it ships at most a fixed number of queue / history
// rows, so a truncated view could read as the whole ledger. When the store holds more
// than we were handed, say so; when the total could not be rolled up, claim nothing.
function capNoteText(shown, total) {
  if (!isNum(shown) || !isNum(total) || total <= shown) return "";
  return "Showing the most recent " + shown.toLocaleString() + " of "
    + total.toLocaleString() + " — this view is capped.";
}

// The polite screen-reader summary: short counts, so a snapshot that shifts nothing can
// be compared against the last one and stay silent.
function announceText(sum) {
  const s = (sum && typeof sum === "object") ? sum : {};
  const n = (k) => (isNum(s[k]) ? s[k] : 0);
  const active = n("probing") + n("encoding") + n("verifying");
  // A parked job is counted SEPARATELY and named for what it is. Folding it into
  // "failed" would tell a screen-reader user the one thing this phase exists to stop
  // holdfast saying: on this dashboard "failed" has always carried "and your source is
  // fine", and a parked job is precisely the case where that is not established.
  return n("done") + " done, " + n("skipped") + " skipped, "
    + n("failed") + " failed, " + n("indeterminate") + " parked awaiting a determination; "
    + active + " active, " + n("pending") + " pending.";
}

// The set an aggregate is over, always stated; plus the bound, when it is not over all
// of that set.
function aggCoverageText(a) {
  return "over " + ((a && a.covers) || "an unstated set") +
    (a && a.window ? " · window: " + a.window : "");
}
// The rows an aggregate had to leave out for want of a recorded value.
function aggExclusionText(a) {
  const excluded = (a && isNum(a.excluded)) ? a.excluded : 0;
  if (excluded <= 0) return "";
  return excluded.toLocaleString() + " row" + (excluded === 1 ? "" : "s") +
    " excluded: no recorded value";
}

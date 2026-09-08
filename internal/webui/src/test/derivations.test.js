// The dashboard's value derivations, one input at a time, with no page standing up.
//
// Runtime: node's BUILT-IN test runner (`node --test`) and node:assert. No registry
// package, no lockfile, no bundler - B18. The derivations under test hold no DOM
// reference, which is exactly why they can be exercised here at all; anything that builds
// nodes is graded in a real browser engine instead (rendered_test.go).
"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const { load, moduleSource, DERIVATION_MODULES } = require("./load.js");

const d = load();
const NR = d.NOT_RECORDED;

// Every way a value can be absent on this wire. `null` is what the store's nullable
// outcome columns serialize to; the rest are what a malformed or truncated payload can
// hand a derivation.
const ABSENT = [undefined, null, NaN, Infinity, -Infinity, "12", "", {}, [], true, false];

// A derivation must never answer with any of these. A zero is the specific lie this
// repo's store exists to prevent (a VMAF of 0.0 is a destroyed frame, not a missing
// measurement); NaN and "undefined" are the two ways a formatter leaks its own failure
// onto the screen.
// The objects a derivation builds live in the vm realm the modules were evaluated in, so
// their prototypes are not this realm's. Compare their SHAPE - a JSON round trip brings
// the value across - rather than prototype identity.
function shape(v) { return JSON.parse(JSON.stringify(v)); }

function refusesFabrication(name, out) {
  assert.notEqual(out, undefined, name + " returned undefined");
  const s = String(out);
  assert.ok(!s.includes("NaN"), name + " rendered NaN: " + s);
  assert.ok(!s.includes("undefined"), name + " rendered the string undefined: " + s);
  assert.ok(!s.includes("Invalid Date"), name + " rendered an Invalid Date: " + s);
}

test("the derivation modules hold no DOM reference", () => {
  // The property that makes this whole file possible. It is asserted, not assumed: a
  // module that reached for the DOM would stop being unit-testable and the suite would
  // quietly shrink to whatever still loaded.
  for (const name of DERIVATION_MODULES) {
    const src = moduleSource(name);
    for (const dom of ["document", "window.", "localStorage", "getElementById", "querySelector"]) {
      assert.ok(!src.includes(dom), name + " reaches for the DOM (" + dom + ")");
    }
  }
});

// --- B5: a value derivation, given an input, returns a value a test can assert --------

test("fmtBytes renders a byte size in the largest unit that fits", () => {
  assert.equal(d.fmtBytes(0), "0 B");
  assert.equal(d.fmtBytes(999), "999 B");
  assert.equal(d.fmtBytes(1023), "1023 B");
  assert.equal(d.fmtBytes(1024), "1.0 KB");
  assert.equal(d.fmtBytes(1536), "1.5 KB");
  assert.equal(d.fmtBytes(1024 * 1024), "1.0 MB");
  assert.equal(d.fmtBytes(3 * 1024 * 1024 * 1024), "3.0 GB");
  assert.equal(d.fmtBytes(1024 ** 5), "1.0 PB");
  // The largest unit is the last one, never an invented one beyond it.
  assert.equal(d.fmtBytes(4096 * 1024 ** 5), "4096.0 PB");
});

test("fmtDur renders an encode duration from milliseconds", () => {
  assert.equal(d.fmtDur(0), "0 ms");
  assert.equal(d.fmtDur(999), "999 ms");
  assert.equal(d.fmtDur(1000), "1s");
  assert.equal(d.fmtDur(59_400), "59s");
  assert.equal(d.fmtDur(60_000), "1m 0s");
  assert.equal(d.fmtDur(90_000), "1m 30s");
  assert.equal(d.fmtDur(3_600_000), "1h 0m");
  assert.equal(d.fmtDur(5_430_000), "1h 30m");
});

test("fmtSpan renders a position or an age in seconds", () => {
  assert.equal(d.fmtSpan(0), "0s");
  assert.equal(d.fmtSpan(59), "59s");
  assert.equal(d.fmtSpan(60), "1m 0s");
  assert.equal(d.fmtSpan(3599), "59m 59s");
  assert.equal(d.fmtSpan(3600), "1h 0m");
  assert.equal(d.fmtSpan(7_384), "2h 3m");
  assert.equal(d.fmtSpan(12.9), "12s", "a fractional second floors, never rounds up past the measurement");
});

test("elapsedText derives an in-state age from the wire timestamp and the server clock", () => {
  // updated_at is seconds, server-side; the page never counts, it subtracts.
  assert.equal(d.elapsedText(1_700_000_000, 1_700_000_000), "0s");
  assert.equal(d.elapsedText(1_700_000_045, 1_700_000_000), "45s");
  assert.equal(d.elapsedText(1_700_000_090, 1_700_000_000), "1m 30s");
  assert.equal(d.elapsedText(1_700_003_600, 1_700_000_000), "1h 0m");
  // The wire carries the timestamp as a string on a dataset attribute; it is coerced.
  assert.equal(d.elapsedText(1_700_000_045, "1700000000"), "45s");
  // A client clock ahead of the server's cannot produce a negative age.
  assert.equal(d.elapsedText(1_699_999_990, 1_700_000_000), "0s");
});

test("clockOffsetFrom re-anchors this page's clock to the server's", () => {
  assert.equal(d.clockOffsetFrom(1_700_000_000, 1_700_000_000_000), 0);
  assert.equal(d.clockOffsetFrom(1_700_000_030, 1_700_000_000_000), 30);
  assert.equal(d.clockOffsetFrom(1_699_999_970, 1_700_000_000_000), -30);
});

test("pct renders a ratio as a percentage", () => {
  assert.equal(d.pct(0), "0%");
  assert.equal(d.pct(0.5), "50%");
  assert.equal(d.pct(0.615), "62%");
  assert.equal(d.pct(1), "100%");
});

test("fmtScore renders a VMAF score to one decimal", () => {
  assert.equal(d.fmtScore(0), "0.0", "a real measured zero is a destroyed frame and must render as 0.0");
  assert.equal(d.fmtScore(43.21), "43.2");
  assert.equal(d.fmtScore(95), "95.0");
  assert.equal(d.fmtScore(100), "100.0");
});

test("the page carries no way to derive the total a view was capped against", () => {
  // LEDGER-5 removed the summary roll-up outright, which is a stronger statement than a
  // test that the renderer stopped calling it: there is nothing left to call.
  //
  // This reads the MODULE SOURCES, and it has to. Asking load()'s api object instead
  // (`assert.equal(d.sumStatuses, undefined)`) would measure load.js: that object is built
  // from a fixed NAMES list, so ANY identifier the list does not mention is undefined on it
  // whatever the modules declare - the assertion would pass unchanged against a page that
  // had the roll-up back. And the scan covers every SHIPPED module, not just the two that
  // load here, because a roll-up reintroduced in the renderer is the same defect.
  const shipped = moduleSource("modules.txt").split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "" && !l.startsWith("#"));
  assert.ok(shipped.length >= DERIVATION_MODULES.length,
    "modules.txt names " + shipped.length + " modules; this scan would be vacuous");

  const sources = shipped.map((name) => [name, moduleSource(name)]);
  // Anti-vacuity: the scan is reading real module text, so a name that IS there is found.
  // Without this, a mis-resolved path would read as "the identifier is gone" every time.
  assert.ok(sources.some(([, src]) => src.includes("capNoteText")),
    "the scan found no module declaring capNoteText, so it is not reading the shipped sources");

  for (const [name, src] of sources) {
    for (const gone of ["sumStatuses", "QUEUE_STATUSES", "TERMINAL_STATUSES"]) {
      assert.ok(!src.includes(gone), name + " still carries " + gone
        + ": the server reports the total, and a client-side roll-up beside it is a second answer"
        + " to the same question for a later reader to reach for");
    }
  }
});

test("sizeFigures derives before, after and the percent reclaimed", () => {
  const f = d.sizeFigures({ source_bytes: 4 * 1024 ** 3, output_bytes: 1024 ** 3 });
  assert.deepEqual(shape(f), { before: "4.0 GB", after: "1.0 GB", reduction: "75% smaller" });
  // The strictly-smaller gate precludes it, but a defensive clamp must never render a
  // negative "% smaller" if that gate ever regresses.
  assert.equal(d.sizeFigures({ source_bytes: 100, output_bytes: 300 }).reduction, "0% smaller");
  assert.equal(d.sizeFigures({ source_bytes: 0, output_bytes: 0 }).reduction, "0% smaller");
});

test("vmafFigures carries both pooled statistics and the viewing condition", () => {
  const f = d.vmafFigures({ vmaf_mean: 98.24, vmaf_min: 91.5, vmaf_model: "version=vmaf_v0.6.1" });
  assert.equal(f.mean, "98.2");
  assert.equal(f.worst, "91.5");
  assert.ok(f.condition.includes("model vmaf_v0.6.1"), f.condition);
  assert.ok(f.condition.includes("worst-frame pooling"), f.condition);
  assert.ok(f.condition.includes("luma-only"), f.condition);
  assert.ok(f.condition.includes("measured vs your source"), f.condition);
  // The score is never graded and never compared between files.
  for (const banned of ["lossless", "identical", "perfect", "better than", "worse than"]) {
    assert.ok(!f.condition.toLowerCase().includes(banned), f.condition);
  }
  // One statistic recorded and the other not: the missing one is a "?", not a copy.
  assert.equal(d.vmafFigures({ vmaf_mean: 97, vmaf_min: null }).worst, "?");
  assert.equal(d.vmafFigures({ vmaf_mean: null, vmaf_min: 40 }).mean, "?");
  assert.equal(d.vmafFigures({ vmaf_mean: 97, vmaf_min: 90 }).condition.includes("unspecified model"), true,
    "a score with no model says the model is unspecified rather than naming one");
});

test("progressFigure exists for a running encode and for no other state", () => {
  const running = { status: "encoding", progress_fraction: 0.421, progress_seconds: 1200, progress_duration_seconds: 3600 };
  // `fraction` is the clamped measurement the percentage is rounded from. It is carried
  // on the figure so the bar the cell draws and the percentage beside it are two
  // readings of ONE number: a drawing computing its own would be a second answer.
  assert.deepEqual(shape(d.progressFigure(running)),
    { unknown: false, fraction: 0.421, percent: "42%", of: "20m 0s of 1h 0m" });
  // Clamping is what the DRAWING depends on: a fraction outside 0..1 would put a mark
  // outside its own track.
  assert.equal(d.progressFigure({ ...running, progress_fraction: 3 }).fraction, 1);
  assert.equal(d.progressFigure({ ...running, progress_fraction: -1 }).fraction, 0);
  // A position past the end is clamped to the duration, never rendered beyond it.
  assert.equal(d.progressFigure({ ...running, progress_seconds: 99_999 }).of, "1h 0m of 1h 0m");
  assert.equal(d.progressFigure({ ...running, progress_fraction: 3 }).percent, "100%");
  assert.equal(d.progressFigure({ ...running, progress_fraction: -1 }).percent, "0%");
  // No duration means no "x of y" line, but the fraction the encoder reported still shows.
  assert.deepEqual(shape(d.progressFigure({ status: "encoding", progress_fraction: 0.5 })),
    { unknown: false, fraction: 0.5, percent: "50%" });
  // Every other state has no progress to have.
  for (const status of ["pending", "probing", "verifying", "done", "skipped", "failed"]) {
    assert.equal(d.progressFigure({ ...running, status }), null, status + " must carry no progress figure");
  }
});

test("guardLabel names each skip guard and never hides an unknown one", () => {
  assert.equal(d.guardLabel("hardlinked"), "hardlinked (would break a seed)");
  assert.equal(d.guardLabel("low-bitrate"), "already efficient (low bitrate)");
  assert.equal(d.guardLabel("already-at-target-codec"), "already at target codec");
  assert.equal(d.guardLabel("a-guard-added-next-week"), "a-guard-added-next-week");
  // A bucket key is attacker-influencable text off the wire, and every one of these
  // names something on Object.prototype. An inherited lookup would put a function body
  // (or "[object Object]") on screen where a guard name belongs; the label is an OWN
  // property or it is the key itself.
  for (const k of ["constructor", "toString", "hasOwnProperty", "__proto__", "valueOf"]) {
    assert.equal(d.guardLabel(k), k, "guardLabel(" + k + ") must fall back to the key itself");
  }
});

// The wire shape of a reported row total, available and carrying a count.
function total(count) {
  return { available: true, unavailable: "", covers: "every terminal row in the ledger", cap: 200, count: count };
}

test("capNoteText claims a cap only when the REPORTED total exceeds the rows we were handed", () => {
  assert.equal(d.capNoteText(200, total(200)), "");
  assert.equal(d.capNoteText(200, total(12)), "");
  assert.ok(d.capNoteText(200, total(1500)).startsWith("Showing the most recent 200 of 1,500"));
  assert.ok(d.capNoteText(200, total(1500)).includes("this view is capped"));
});

test("capNoteText reports the total the server sent, never one derived from the rows", () => {
  // The rows on screen and the summary a page could add up say one thing; the ledger says
  // another, and only the server can see it. The reported figure is the one that shows.
  assert.ok(d.capNoteText(3, total(41_237)).includes("of 41,237"),
    "the reported total must be the figure the notice carries");
  assert.ok(!d.capNoteText(3, total(41_237)).includes("of 3"), "the rows returned are not the total");
});

test("capNoteText states an unreadable total as unavailable and shows no figure in its place", () => {
  const unread = { available: false, unavailable: "this figure could not be read from the ledger",
                   covers: "every terminal row in the ledger", cap: 200, count: null };
  for (const t of [unread, {}, { available: true, count: null }, { available: true, count: "many" },
                   { available: true, count: NaN }, { available: true, count: -1 }, [], "200", 200, true]) {
    const text = d.capNoteText(200, t);
    assert.equal(text, d.CAP_TOTAL_UNAVAILABLE, "capNoteText(200, " + JSON.stringify(t) + ")");
    assert.ok(/unavailable/i.test(text), "the reader must be told the total is unavailable");
    assert.ok(!/\d/.test(text),
      "an unreadable total must put NO figure on screen - a number beside 'capped' reads AS the total: " + text);
  }
  // A total absent from the frame entirely is no readable total either, and answers the
  // same way. The alternative - "" - is indistinguishable from an uncapped view, which is
  // the one thing an unknown cap must not look like.
  assert.equal(d.capNoteText(200, undefined), d.CAP_TOTAL_UNAVAILABLE);
  assert.equal(d.capNoteText(200, null), d.CAP_TOTAL_UNAVAILABLE);
  // And a page that does not know how many rows it drew claims nothing either.
  assert.equal(d.capNoteText(undefined, total(1500)), "");
});

test("an unreadable total does not claim the view is capped, which it cannot know", () => {
  // Cappedness is the comparison total > shown. With the total unreadable that comparison
  // cannot be made, so the notice states the unavailability and stops there - it must not
  // assert a cap the page has just said it cannot see. 3 rows out of a 3-row ledger is not
  // a capped view, and the old wording called it one.
  const unread = { available: false, unavailable: "this figure could not be read from the ledger",
                   covers: "every terminal row in the ledger", cap: 200, count: null };
  const text = d.capNoteText(3, unread);
  assert.ok(/unavailable/i.test(text), "the reader must still be told the total is unavailable");
  assert.ok(!/this view is capped/i.test(text),
    "an unreadable total must not assert that the view IS capped - cappedness is the comparison "
    + "total > shown, and the total is the half that could not be read: " + text);
  assert.ok(/whether/i.test(text), "the notice must qualify the fact it cannot establish: " + text);
  assert.ok(!/\d/.test(text), "and still no figure: " + text);
});

test("announceText is a short count summary a screen reader can hear on every snapshot", () => {
  assert.equal(
    d.announceText({ pending: 4, probing: 1, encoding: 2, verifying: 1, done: 9, skipped: 3, failed: 2 }),
    "9 done, 3 skipped, 2 failed, 0 parked awaiting a determination, 0 applied despite an error; 4 active, 4 pending.");
  assert.equal(d.announceText({}),
    "0 done, 0 skipped, 0 failed, 0 parked awaiting a determination, 0 applied despite an error; 0 active, 0 pending.");
});

// A parked job is counted SEPARATELY and named for what it is (FILESYSTEM-1, AC15j). It is
// NOT folded into "failed", because on this dashboard "failed" has always carried "and your
// source is fine" - and a parked job is exactly the case where that is not established. A
// screen-reader user hearing the count as a failure would be told the one thing this phase
// exists to stop holdfast saying.
test("announceText counts a parked job as parked, never as a failure", () => {
  assert.equal(
    d.announceText({ done: 1, skipped: 0, failed: 2, indeterminate: 3, pending: 0 }),
    "1 done, 0 skipped, 2 failed, 3 parked awaiting a determination, 0 applied despite an error; 0 active, 0 pending.");
  // The two counts move independently: parked jobs do not inflate the failure count.
  assert.ok(d.announceText({ failed: 0, indeterminate: 5 }).includes("0 failed, 5 parked"));
  assert.ok(d.announceText({ failed: 5, indeterminate: 0 }).includes("5 failed, 0 parked"));
});

// The OTHER FILESYSTEM-1 outcome, and the same fault caught by omission rather than by
// mislabelling: applied-despite-error has its own chip, its own history rows and its own
// result-cell text, so a sighted user sees the count while a screen-reader user hearing
// the summary was told nothing about them at all. It is spoken, it is spoken as itself,
// and it is independent of both "done" and "failed".
test("announceText speaks applied-despite-error too, as itself", () => {
  assert.equal(
    d.announceText({ done: 1, failed: 0, indeterminate: 0, "applied-despite-error": 4 }),
    "1 done, 0 skipped, 0 failed, 0 parked awaiting a determination, 4 applied despite an error; 0 active, 0 pending.");
  // Never folded into a success, and never into a failure.
  assert.ok(d.announceText({ done: 0, "applied-despite-error": 7 }).includes("0 done"));
  assert.ok(d.announceText({ failed: 0, "applied-despite-error": 7 }).includes("0 failed"));
  assert.ok(d.announceText({ "applied-despite-error": 7 }).includes("7 applied despite an error"));
  // And every status the served document declares is either spoken or deliberately
  // rolled into "active": a new outcome must not be able to go silent again.
  const spoken = d.announceText({ done: 1, skipped: 1, failed: 1, indeterminate: 1, "applied-despite-error": 1 });
  for (const s of ["done", "skipped", "failed", "indeterminate", "applied-despite-error"]) {
    assert.ok(/1 /.test(spoken.split(",").find((p) => p.includes(s === "indeterminate" ? "parked" :
      s === "applied-despite-error" ? "applied despite" : s)) || ""),
      "the summary does not count " + s + ": " + spoken);
  }
});

test("an aggregate states the set it covers and the rows it excluded", () => {
  assert.equal(d.aggCoverageText({ covers: "every done row" }), "over every done row");
  assert.equal(d.aggCoverageText({ covers: "every done row", window: "the last 30 days" }),
    "over every done row · window: the last 30 days");
  assert.equal(d.aggExclusionText({ excluded: 0 }), "");
  assert.equal(d.aggExclusionText({ excluded: 1 }), "1 row excluded: no recorded value");
  assert.equal(d.aggExclusionText({ excluded: 2400 }), "2,400 rows excluded: no recorded value");
});

test("fmtTime renders a wire timestamp", () => {
  const out = d.fmtTime(1_700_000_000);
  assert.equal(typeof out, "string");
  assert.notEqual(out, NR);
  refusesFabrication("fmtTime", out);
});

test("isNum accepts only a finite number", () => {
  for (const v of [0, -1, 1.5, 1e300]) assert.equal(d.isNum(v), true, String(v));
  for (const v of ABSENT) assert.equal(d.isNum(v), false, String(v));
});

// --- B6: absent, null, non-numeric, non-finite or negative is answered honestly --------

test("fmtBytes answers an absent, non-finite or negative size as not recorded", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtBytes(v), NR, "fmtBytes(" + String(v) + ")");
    refusesFabrication("fmtBytes", d.fmtBytes(v));
  }
  assert.equal(d.fmtBytes(-1), NR, "a negative byte count is not a measurement");
});

test("fmtDur answers an absent, non-finite or negative duration as not recorded", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtDur(v), NR, "fmtDur(" + String(v) + ")");
    refusesFabrication("fmtDur", d.fmtDur(v));
  }
  assert.equal(d.fmtDur(-1), NR);
});

test("fmtSpan answers an absent, non-finite or negative span as not recorded", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtSpan(v), NR, "fmtSpan(" + String(v) + ")");
    refusesFabrication("fmtSpan", d.fmtSpan(v));
  }
  assert.equal(d.fmtSpan(-1), NR);
});

test("fmtTime answers an absent or non-positive timestamp as not recorded", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtTime(v), NR, "fmtTime(" + String(v) + ")");
    refusesFabrication("fmtTime", d.fmtTime(v));
  }
  assert.equal(d.fmtTime(0), NR, "epoch zero is 'never recorded', not a date");
  assert.equal(d.fmtTime(-1), NR);
});

test("pct answers an absent or non-finite ratio as not recorded, never 0%", () => {
  for (const v of ABSENT) {
    assert.equal(d.pct(v), NR, "pct(" + String(v) + ")");
    refusesFabrication("pct", d.pct(v));
  }
});

test("fmtScore answers an absent or non-finite score as not recorded, never 0.0", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtScore(v), NR, "fmtScore(" + String(v) + ")");
    refusesFabrication("fmtScore", d.fmtScore(v));
  }
});

test("fmtCount answers an absent or non-finite count as not recorded", () => {
  for (const v of ABSENT) {
    assert.equal(d.fmtCount(v), NR, "fmtCount(" + String(v) + ")");
    refusesFabrication("fmtCount", d.fmtCount(v));
  }
});

test("a cap notice never renders a zero total as a real one", () => {
  // The failure this whole field replaces: a figure nobody could read, shown as 0.
  assert.equal(d.capNoteText(200, { available: false, count: 0 }), d.CAP_TOTAL_UNAVAILABLE);
  // A genuine zero is a genuine answer, and it caps nothing.
  assert.equal(d.capNoteText(0, total(0)), "");
});

test("elapsedText answers a row with no usable basis with no age at all", () => {
  for (const v of [undefined, null, 0, -1, NaN, Infinity, "", "not-a-number", {}]) {
    assert.equal(d.elapsedText(1_700_000_000, v), null, "elapsedText(now, " + String(v) + ")");
  }
  assert.equal(d.elapsedText(NaN, 1_700_000_000), null, "no server clock is no age");
  assert.equal(d.elapsedText(undefined, 1_700_000_000), null);
});

test("sizeFigures answers an unrecorded size with no figures at all, never 0 bytes", () => {
  for (const v of ABSENT) {
    assert.equal(d.sizeFigures({ source_bytes: v, output_bytes: 1024 }), null, "source " + String(v));
    assert.equal(d.sizeFigures({ source_bytes: 1024, output_bytes: v }), null, "output " + String(v));
  }
  assert.equal(d.sizeFigures({}), null);
});

test("vmafFigures answers an unmeasured row with no figures at all, never 0.0", () => {
  for (const v of ABSENT) {
    assert.equal(d.vmafFigures({ vmaf_mean: v, vmaf_min: v }), null, String(v));
  }
  assert.equal(d.vmafFigures({}), null);
  assert.equal(d.vmafFigures({ vmaf_mean: null, vmaf_min: null }), null);
});

test("progressFigure answers a running encode with no measurement as unknown", () => {
  for (const v of ABSENT) {
    assert.deepEqual(shape(d.progressFigure({ status: "encoding", progress_fraction: v })), { unknown: true },
      "progress_fraction " + String(v));
  }
  // A fraction with an unusable duration keeps the fraction and drops the "x of y" line,
  // rather than inventing a position.
  const partial = d.progressFigure({ status: "encoding", progress_fraction: 0.5, progress_seconds: 10, progress_duration_seconds: null });
  assert.equal(partial.percent, "50%");
  assert.equal(partial.of, undefined);
});

test("announceText and aggregate copy survive an unusable summary", () => {
  for (const v of [undefined, null, 7, "x"]) {
    refusesFabrication("announceText", d.announceText(v));
  }
  refusesFabrication("aggCoverageText", d.aggCoverageText(undefined));
  assert.equal(d.aggCoverageText(undefined), "over an unstated set");
  assert.equal(d.aggCoverageText({}), "over an unstated set");
  for (const v of ABSENT) {
    assert.equal(d.aggExclusionText({ excluded: v }), "", "excluded " + String(v));
  }
});

// --- DASH-9: the value-to-geometry derivations behind the drawings -------------------
//
// These are the only arithmetic the figures do, so they are exercised here one input at
// a time - including every degenerate input a real ledger produces. What the marks then
// MEASURE on screen is a different question and is decided in a real browser engine
// (dashboard_rendered_test.go), because no assertion here can say what was drawn.

test("readBuckets accepts a published figure and refuses one that cannot be read", () => {
  assert.deepEqual(
    shape(d.readBuckets([{ key: "done", count: 9 }, { key: "failed", count: 2 }])),
    [{ key: "done", count: 9 }, { key: "failed", count: 2 }]);
  // A numeric key is a key; it arrives as text on the page either way.
  assert.deepEqual(shape(d.readBuckets([{ key: 7, count: 1 }])), [{ key: "7", count: 1 }]);
  // A count of zero is a MEASUREMENT and is kept; only an unusable one is refused.
  assert.deepEqual(shape(d.readBuckets([{ key: "done", count: 0 }])), [{ key: "done", count: 0 }]);

  // Absent, empty or malformed: each throws, which is what draws the card as unavailable
  // rather than as a figure with a shape over a number nobody published.
  for (const bad of [undefined, null, {}, "done=9", 3, true, []]) {
    assert.throws(() => d.readBuckets(bad), "readBuckets(" + JSON.stringify(bad) + ")");
  }
  for (const bad of [
    [null], [undefined], ["done"], [[]], [{ count: 9 }], [{ key: "done" }],
    [{ key: "done", count: null }], [{ key: "done", count: "9" }], [{ key: "done", count: NaN }],
    [{ key: "done", count: Infinity }], [{ key: "done", count: -1 }], [{ key: {}, count: 1 }],
    [{ key: "done", count: 9 }, { key: "failed", count: null }],
  ]) {
    assert.throws(() => d.readBuckets(bad), "readBuckets(" + JSON.stringify(bad) + ")");
  }
});

test("bucketProportions sizes every bar against the largest count in its own figure", () => {
  assert.deepEqual(shape(d.bucketProportions([{ count: 9 }, { count: 3 }, { count: 2 }])),
    [1, 1 / 3, 2 / 9]);
  // One bucket is its own maximum.
  assert.deepEqual(shape(d.bucketProportions([{ count: 4 }])), [1]);
  // All-equal counts are all full length - the figure says "these are the same", which
  // is what it measured.
  assert.deepEqual(shape(d.bucketProportions([{ count: 5 }, { count: 5 }])), [1, 1]);
  // A zero beside a real count is a real zero, and draws as no length at all.
  assert.deepEqual(shape(d.bucketProportions([{ count: 10 }, { count: 0 }])), [1, 0]);
  // Nothing to take a proportion against: no bar is drawn at all, rather than a row of
  // full-length marks or a division by zero.
  assert.equal(d.bucketProportions([{ count: 0 }, { count: 0 }]), null);
  for (const bad of [undefined, null, [], {}, "x", 4, [{ count: null }], [{ count: "3" }],
    [{ count: NaN }], [{ count: -2 }], [null], [{ count: 3 }, { count: undefined }]]) {
    assert.equal(d.bucketProportions(bad), null, "bucketProportions(" + JSON.stringify(bad) + ")");
  }
});

test("spreadPositions places the mean on the scale its own ends define", () => {
  assert.deepEqual(shape(d.spreadPositions(0, 5, 10)), { mean: 0.5 });
  assert.deepEqual(shape(d.spreadPositions(0.21, 0.38, 0.74)), { mean: (0.38 - 0.21) / (0.74 - 0.21) });
  assert.deepEqual(shape(d.spreadPositions(10, 10, 20)), { mean: 0 });
  assert.deepEqual(shape(d.spreadPositions(10, 20, 20)), { mean: 1 });
  // A mean outside its own ends cannot be plotted honestly beyond them; it is clamped to
  // the scale rather than drawn off the end of the card.
  assert.deepEqual(shape(d.spreadPositions(10, 4, 20)), { mean: 0 });
  assert.deepEqual(shape(d.spreadPositions(10, 40, 20)), { mean: 1 });
  // Ends that coincide are a point, not a spread: nothing is drawn, and the three values
  // are still stated as text by the card.
  assert.equal(d.spreadPositions(7, 7, 7), null);
  // A maximum below its minimum is a figure that cannot be true.
  assert.equal(d.spreadPositions(9, 5, 1), null);
  for (const v of ABSENT) {
    assert.equal(d.spreadPositions(v, 5, 10), null, "min " + String(v));
    assert.equal(d.spreadPositions(0, v, 10), null, "mean " + String(v));
    assert.equal(d.spreadPositions(0, 5, v), null, "max " + String(v));
  }
});

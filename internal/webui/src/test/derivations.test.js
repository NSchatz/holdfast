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

test("sumStatuses rolls the summary up over one table's states", () => {
  const sum = { pending: 4, probing: 1, encoding: 2, verifying: 1, done: 9, skipped: 3, failed: 2 };
  assert.equal(d.sumStatuses(sum, d.QUEUE_STATUSES), 8);
  assert.equal(d.sumStatuses(sum, d.TERMINAL_STATUSES), 14);
  assert.equal(d.sumStatuses({}, d.QUEUE_STATUSES), 0, "an empty ledger rolls up to a counted zero");
  assert.equal(d.sumStatuses({ done: 2, encoding: "many" }, d.TERMINAL_STATUSES), 2,
    "a non-numeric member contributes nothing rather than NaN");
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
  assert.deepEqual(shape(d.progressFigure(running)), { unknown: false, percent: "42%", of: "20m 0s of 1h 0m" });
  // A position past the end is clamped to the duration, never rendered beyond it.
  assert.equal(d.progressFigure({ ...running, progress_seconds: 99_999 }).of, "1h 0m of 1h 0m");
  assert.equal(d.progressFigure({ ...running, progress_fraction: 3 }).percent, "100%");
  assert.equal(d.progressFigure({ ...running, progress_fraction: -1 }).percent, "0%");
  // No duration means no "x of y" line, but the fraction the encoder reported still shows.
  assert.deepEqual(shape(d.progressFigure({ status: "encoding", progress_fraction: 0.5 })),
    { unknown: false, percent: "50%" });
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
});

test("capNoteText claims a cap only when the ledger holds more than we were handed", () => {
  assert.equal(d.capNoteText(200, 200), "");
  assert.equal(d.capNoteText(200, 12), "");
  assert.ok(d.capNoteText(200, 1500).startsWith("Showing the most recent 200 of 1,500"));
  assert.ok(d.capNoteText(200, 1500).includes("this view is capped"));
});

test("announceText is a short count summary a screen reader can hear on every snapshot", () => {
  assert.equal(
    d.announceText({ pending: 4, probing: 1, encoding: 2, verifying: 1, done: 9, skipped: 3, failed: 2 }),
    "9 done, 3 skipped, 2 failed, 0 parked awaiting a determination; 4 active, 4 pending.");
  assert.equal(d.announceText({}),
    "0 done, 0 skipped, 0 failed, 0 parked awaiting a determination; 0 active, 0 pending.");
});

// A parked job is counted SEPARATELY and named for what it is (FILESYSTEM-1, AC15j). It is
// NOT folded into "failed", because on this dashboard "failed" has always carried "and your
// source is fine" - and a parked job is exactly the case where that is not established. A
// screen-reader user hearing the count as a failure would be told the one thing this phase
// exists to stop holdfast saying.
test("announceText counts a parked job as parked, never as a failure", () => {
  assert.equal(
    d.announceText({ done: 1, skipped: 0, failed: 2, indeterminate: 3, pending: 0 }),
    "1 done, 0 skipped, 2 failed, 3 parked awaiting a determination; 0 active, 0 pending.");
  // The two counts move independently: parked jobs do not inflate the failure count.
  assert.ok(d.announceText({ failed: 0, indeterminate: 5 }).includes("0 failed, 5 parked"));
  assert.ok(d.announceText({ failed: 5, indeterminate: 0 }).includes("5 failed, 0 parked"));
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

test("sumStatuses answers an unusable summary as not rolled up, never as a total of 0", () => {
  for (const v of [undefined, null, NaN, "9", 7, true]) {
    assert.equal(d.sumStatuses(v, d.QUEUE_STATUSES), null, "sumStatuses(" + String(v) + ")");
  }
  assert.equal(d.sumStatuses({ done: 1 }, null), null, "no state list is nothing to roll up");
  // And the cap notice claims nothing when the total could not be rolled up.
  assert.equal(d.capNoteText(200, d.sumStatuses(null, d.TERMINAL_STATUSES)), "");
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

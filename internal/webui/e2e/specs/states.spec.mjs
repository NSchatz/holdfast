// Clauses F3, F6 and F7: what the page says when it has nothing, when it cannot read what
// it was given, and when the feed that was filling it dies.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered } from "./conventions.mjs";
import { gradeViewsAllIn, gradeNoZeroBeforeASnapshot } from "./graders.mjs";
import { pageURL } from "./theme.mjs";
import { gradeEveryUnmeasuredFieldReadsTheAbsencePhrase, gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure, severedStreamReading } from "./states.mjs";


const waitAllViews = (state) => (p) => p.waitForFunction((want) => {
  const v = window.__hf.views();
  return v.length === 4 && v.every((x) => x.state === want);
}, state, { timeout: 15000 });

// --- F7: the three states, in every view, all distinct ------------------------------------

test("every view shows its loading, empty and unreadable states in words", async ({ browser, baseURL }) => {
  const problems = [];
  // The states each view showed, per state, so they can be compared with each other.
  const texts = {};
  for (const state of ["loading", "empty", "unreadable"]) {
    const { ctx, page } = await open(browser, {
      url: pageURL(baseURL, state), theme: "light", waitFor: waitAllViews(state),
    });
    const s = await collect(page);
    for (const prob of gradeViewsAllIn(state)(s)) problems.push(`[${state}] ${prob}`);
    texts[state] = Object.fromEntries(s.views.map((v) => [v.view, v.text]));
    // The controls stay operable in every one of the three states.
    for (const ctl of s.controls) {
      if (!ctl.present || !ctl.rendered) problems.push(`[${state}] the control "${ctl.id}" is not on the page`);
    }
    // And in the empty state, no view may still be claiming to be loading.
    if (state === "empty") {
      for (const v of s.views) {
        if (v.state === "loading") problems.push(`the ${v.view} view still shows a loading state after a snapshot arrived`);
      }
    }
    await ctx.close();
  }
  // DISTINCT: within one view, the three states say three different things. A page that
  // said "no data" for both "nothing to show" and "could not be read" would be lying about
  // one of them.
  for (const view of ["counts", "queue", "aggs", "history"]) {
    const seen = new Map();
    for (const state of ["loading", "empty", "unreadable"]) {
      const txt = texts[state][view];
      if (!txt) { problems.push(`the ${view} view says nothing at all in its ${state} state`); continue; }
      if (seen.has(txt)) problems.push(`the ${view} view says the same thing in its ${seen.get(txt)} state and its ${state} state: "${txt}"`);
      seen.set(txt, state);
    }
  }
  expect(problems, problems.join("\n")).toEqual([]);
});

// --- F3: absence is not zero ---------------------------------------------------------------

test("no count, total or aggregate reads as zero before a snapshot arrives", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "loading"), theme: "light",
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.connText() === "live" && window.__hf.views().every((v) => v.state === "loading"),
      null, { timeout: 15000 }),
  });
  const s = await collect(page);
  const probs = gradeNoZeroBeforeASnapshot(s);
  expect(probs, probs.join("\n")).toEqual([]);
  expect(s.bodyText.includes("0 B"),
    'the page shows "0 B" before any snapshot arrived; a total nobody has reported yet is not 0 bytes').toBe(false);
  await ctx.close();
});



test("every unmeasured field renders the one absence phrase", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "null"), theme: "light", waitFor: waitRendered });
  const probs = gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(await collect(page));
  expect(probs, probs.join("\n")).toEqual([]);
  await ctx.close();
});

// --- F6: a severed stream stops every figure reading as live --------------------------------





test("a severed stream keeps the rows and stops every figure advancing", async ({ browser, baseURL }) => {
  const { first, before, after } = await severedStreamReading(browser, pageURL(baseURL, "severed"));
  const probs = gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first, before, after);
  expect(probs, probs.join("\n")).toEqual([]);
});

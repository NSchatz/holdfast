// The frontend-convention cases, decided against the SERVED document in a real engine.
//
// These moved off a hand-written DevTools-protocol driver. What they decide has not
// changed: the measuring script is the same script, carried across verbatim, and each
// grader is the same predicate. What changed is who drives the engine - the theme, the
// reduced-motion preference, the viewport, the accessibility tree and real key input are
// the runner's own vocabulary now, so a new criterion costs a few lines instead of a probe.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered } from "./conventions.mjs";
import { convGraders, gradeBodyNeverScrollsSideways } from "./graders.mjs";
import { pageURL, gradeThemeFollowsTheEnginesPreference, readThemeTriple, readContrastRecords, gradeRecordedRatiosAgreeWithTheEngine } from "./theme.mjs";





// --- the whole convention set, in both themes and at both widths ------------------------

test("the shipped page meets every convention in both themes at both widths", async ({ browser, baseURL }) => {
  // Which combinations actually produced a MEASUREMENT. Clause F10 says a contrast
  // assertion is run once per theme and that the gate fails "if either run is missing", so
  // the runs are counted rather than assumed: a loop that quietly stopped visiting a theme
  // would otherwise pass by measuring nothing.
  const measured = { light: 0, dark: 0 };
  const problems = [];

  for (const theme of ["light", "dark"]) {
    for (const width of [360, 1280]) {
      const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme, width, height: 900, waitFor: waitRendered });
      const s = await collect(page);
      measured[theme] += s.textRuns.length;
      for (const g of convGraders()) {
        // Wide content taking a scroll of its own is a claim about a NARROW viewport; at
        // 1280 the tables fit and there is nothing to scroll.
        if (g.name === "wide content scrolls inside its own container" && width !== 360) continue;
        for (const prob of g.probe(s)) problems.push(`[${theme} theme, ${width}px] ${g.name}: ${prob}`);
      }
      await ctx.close();
    }
  }
  for (const theme of ["light", "dark"]) {
    expect(measured[theme],
      `the ${theme} theme contributed only ${measured[theme]} text measurements; clause F10 requires every contrast assertion to be RUN in both themes, and a run that measured nothing is a missing run`
    ).toBeGreaterThanOrEqual(20);
  }
  expect(problems, problems.join("\n")).toEqual([]);
});

// Clause F9 in every STATE, not only the live one: a page that is loading, empty or
// unreadable is still a page an operator is looking at on a phone, and a state row or a
// state line that pushes the body sideways is the same defect as a table that does.
test("the body never scrolls sideways at 360 in every theme and state", async ({ browser, baseURL }) => {
  const problems = [];
  for (const theme of ["light", "dark"]) {
    for (const [state, scenario] of [["live", "full"], ["loading", "loading"], ["empty", "empty"], ["unreadable", "unreadable"]]) {
      const waitFor = state === "live"
        ? waitRendered
        : (p) => p.waitForFunction((want) => {
            const v = window.__hf.views();
            return v.length === 4 && v.every((x) => x.state === want);
          }, state, { timeout: 15000 });
      const { ctx, page } = await open(browser, { url: pageURL(baseURL, scenario), theme, width: 360, height: 900, waitFor });
      const s = await collect(page);
      for (const prob of gradeBodyNeverScrollsSideways(s)) problems.push(`[${theme} theme, ${state} state] ${prob}`);
      expect(s.layout.innerWidth,
        `[${theme} theme, ${state} state] the engine laid the page out against a ${s.layout.innerWidth} px viewport, not 360`).toBe(360);
      await ctx.close();
    }
  }
  expect(problems, problems.join("\n")).toEqual([]);
});

// --- F10: the theme is the operating system's preference, read at the engine ------------





test("the engine's colour-scheme preference chooses the theme", async ({ browser, baseURL }) => {
  const [light, dark, none] = await readThemeTriple(browser, pageURL(baseURL, "full"));
  const probs = gradeThemeFollowsTheEnginesPreference(light, dark, none);
  expect(probs, probs.join("\n")).toEqual([]);
});

// --- S6 / S7: the recorded ratios are the ratios the engine measures ---------------------







test("every recorded contrast ratio agrees with the engine in both themes", async ({ browser, baseURL }) => {
  const records = readContrastRecords();
  const problems = [];
  for (const theme of ["light", "dark"]) {
    const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme, waitFor: waitRendered });
    const s = await collect(page);
    problems.push(...gradeRecordedRatiosAgreeWithTheEngine(theme, s.tokens, records));
    await ctx.close();
  }
  expect(records.length, "no contrast record was checked, so this asserted nothing").toBeGreaterThan(4);
  expect(problems, problems.join("\n")).toEqual([]);
});

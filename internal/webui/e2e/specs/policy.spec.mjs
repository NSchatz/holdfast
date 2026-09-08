// Clauses F11, F1 and F5: the browser's OWN verdict on the page, and what a refused
// control action costs.
//
// The response policy is asserted byte for byte elsewhere. What these add is that the
// ENGINE, rendering the shipped page in both themes, at both widths, through every state
// it has, refused nothing - and that a control action the server rejects says so in words
// and takes nothing else with it.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered, waitDown } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import { gradeNoPolicyRefusal, gradeRefusedControlActionCostsNothingElse, refuseAControlAction, gradeServerErrorStillRendersTheOfferAndTheControls, armControlStatus } from "./interaction.mjs";




// The five worlds the engine has to render without refusing anything, graded once per
// theme and width. They are FOUR cases rather than one loop of twenty page loads: each
// load takes a real render and a real settle, so the single case ran for longer than the
// runner's own per-case deadline and reported a timeout instead of a verdict. Split, each
// combination is well inside it, the four run in parallel, and a failure names the theme
// and the width in the case title rather than only in the message.
const INTERACTIONS = [
  { name: "a live snapshot", scenario: "full" },
  { name: "a severed stream", scenario: "severed", waitFor: waitDown },
  { name: "a snapshot carrying hostile text", scenario: "hostile" },
  { name: "a filter entry", scenario: "full", drive: async (p) => { await p.fill("#filter", "alpha"); } },
  { name: "a control action", scenario: "full", refuse: 401, drive: async (p) => { await p.click("#rescan"); } },
];

for (const theme of ["light", "dark"]) {
  for (const width of [360, 1280]) {
    test(`no policy violation in the ${theme} theme at ${width}px through every interaction`, async ({ browser, baseURL }) => {
      const problems = [];
      for (const c of INTERACTIONS) {
        const { ctx, page, violations } = await open(browser, {
          url: pageURL(baseURL, c.scenario), theme, width, height: 900,
          waitFor: c.waitFor || waitRendered,
        });
        if (c.refuse) await armControlStatus(page, baseURL, c.refuse);
        if (c.drive) { await c.drive(page); await page.waitForTimeout(400); }
        problems.push(...gradeNoPolicyRefusal(`[${theme} theme, ${width}px, ${c.name}]`, violations));
        if (c.refuse) await armControlStatus(page, baseURL, 0);
        await ctx.close();
      }
      expect(problems.length + INTERACTIONS.length,
        "no interaction was driven at all, so this case asserted nothing").toBeGreaterThan(4);
      expect(problems, problems.join("\n")).toEqual([]);
    });
  }
}

// --- F1 / F5: a refused control action ------------------------------------------------------





test("a refused control action says so and costs nothing else", async ({ browser, baseURL }) => {
  const { before, after, refusals } = await refuseAControlAction(browser, baseURL);
  const probs = gradeRefusedControlActionCostsNothingElse(before, after, refusals);
  expect(probs, probs.join("\n")).toEqual([]);
});

// --- F7 / F11: the snapshot endpoint answering with a server error ----------------------------



test("a server error on the snapshot endpoint still renders the offer and the controls", async ({ browser, baseURL }) => {
  const { ctx, page, violations } = await open(browser, {
    url: pageURL(baseURL, "no-stream"), theme: "light",
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.views().length === 4 && window.__hf.views().every((v) => v.state === "unreadable"),
      null, { timeout: 15000 }),
  });
  const probs = gradeServerErrorStillRendersTheOfferAndTheControls(await collect(page), violations);
  expect(probs, probs.join("\n")).toEqual([]);
  await ctx.close();
});

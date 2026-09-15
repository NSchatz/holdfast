// interface-craft C3 (hierarchy) and C5 (density), decided in a real engine.
//
// Both clauses used to be graded by a human looking at a screenshot, which is the weakest
// evidence this project accepts: it cannot fail a regression and it passes by default. What
// replaces it is the reading in probe.mjs - the computed size, weight and colour of each
// figure and of the text that names it, and the chrome and the facts of every container -
// and the predicates in graders.mjs, which decide from that reading and from nothing else.
//
// These cases DRIVE the worlds the clauses have to hold in. The runner's per-theme projects
// cover three of the four combinations F9 and F10 together ask for (dark-wide, light-wide,
// dark-narrow); a clause decided in three of four is a clause with an untested corner, so
// the loop below visits all four itself and then REFUSES a combination that contributed no
// measurement. A world that measured nothing is a missing run, not a passing one.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import {
  gradeFigureStandsApartFromItsLabel,
  gradeTypeScaleCarriesThreeSizes,
  gradeContainerEarnsItsChrome,
  craftContainers,
} from "./graders.mjs";

// REPORT marks a line printed for a READER OF THE GATE rather than for the runner: a
// reading a criterion requires to be NAMED even when nothing failed, which is what an
// unmeasured region is. The Go wrapper swallows the runner's output on success, so it
// lifts every line carrying this marker back out where `go test -v` - and so
// `make webui-check` - shows it. Its other reader is `internal/webui/e2e_test.go`.
const REPORT = "e2e-report: ";

// The four worlds: each colour-scheme preference the engine can be put in, at the phone
// width F9 names and at a desktop width.
const WORLDS = [];
for (const theme of ["light", "dark"]) {
  for (const width of [360, 1280]) WORLDS.push({ theme, width, name: `${theme} theme at ${width}px` });
}

function craftProblems(s) {
  return [
    ...gradeFigureStandsApartFromItsLabel(s),
    ...gradeTypeScaleCarriesThreeSizes(s),
    ...gradeContainerEarnsItsChrome(s),
  ];
}

// The two regions an OPERATOR's own actions put on the page, driven for real on the page
// already open rather than in a browser of their own. Neither is in the document a reader
// meets: the ledger search's results appear when a search is run, and the withheld-paths
// card appears when a path is withheld. A clause decided only over the document as it
// loads is a clause with a hole exactly the size of everything the operator can summon,
// and both of these are bordered containers, which is what C5 is about.
//
// The withheld list is a token-gated read, so the page is given a token first; without one
// it has nothing to draw and this would measure an empty region and call it a pass.
async function summonTheOperatorsOwnRegions(page) {
  await page.evaluate(() => {
    const e = document.getElementById("token");
    e.value = "a-token";
    e.dispatchEvent(new Event("change", { bubbles: true }));
  });
  await page.fill("#ledger-search", "some-film");
  await page.click("#search-go");
  await page.locator("#search-results tr").first().waitFor({ state: "visible", timeout: 15000 });
  await page.click("#history td.remedy button");
  await page.locator("#held-list li").first().waitFor({ state: "visible", timeout: 15000 });
}

// Criteria: AC-1 (two channels of three), AC-2 (three painted sizes), AC-3 (three facts per
// bordered container, over the page as it loads AND over the two regions an operator's own
// actions put on it), AC-5 (every theme at every width, and a world that measured nothing
// is a failure), AC-6 (no figure or no container found is a failure), AC-7 (a region with
// no figure is named as unmeasured).
test("hierarchy and density hold in every theme at every width, and every world measured something", async ({ browser, baseURL }, testInfo) => {
  // A DECLARED budget, larger than the project's 30s default, because this case is
  // deliberately larger than a case: it opens four browser contexts, takes eight whole
  // readings and drives three real interactions in each world. AC-5 is why they are one
  // case rather than four - "all four combinations were visited" is only assertable where
  // all four ran - and the alternative to a bigger budget is four cases opening the same
  // browsers again, which `## Risks` refuses by name.
  //
  // The number is measured, not guessed: ~15s on an idle machine, 30.9s under `make check`,
  // where this runs inside `go test -race` beside every other package. 120s is the same
  // budget the config gives its own webServer and leaves a wedge failing with its output
  // rather than hanging, which is the rule the 30s default exists for.
  test.setTimeout(120_000);
  const problems = [];
  const measured = [];
  const unmeasured = [];
  const summoned = [];

  for (const world of WORLDS) {
    const { ctx, page } = await open(browser, {
      url: pageURL(baseURL, "full"), theme: world.theme, width: world.width, height: 900,
      waitFor: waitRendered,
    });
    const s = await collect(page);
    // The same page, after the operator has run a ledger search and withheld a path: two
    // more bordered regions, measured in the browser already open rather than a new one.
    await summonTheOperatorsOwnRegions(page);
    const acted = await collect(page);
    await ctx.close();

    for (const [what, reading] of [["as it loads", s], ["after a search and a withholding", acted]]) {
      for (const prob of craftProblems(reading)) problems.push(`[${world.name}, ${what}] ${prob}`);

      const figures = reading.hierarchy.regions.reduce((n, r) => n + r.figures.length, 0);
      measured.push({ world: `${world.name}, ${what}`, figures,
        containers: craftContainers(reading.chromed).length,
        sizes: reading.hierarchy.sizes.length });
      // Clause AC-7's half: a region that rendered no figure is UNMEASURED for the
      // hierarchy clause. It is not a pass and it is not a problem - it is named, here, so
      // a region that quietly stopped rendering its figures is visible in the run rather
      // than absorbed by a grader that found nothing to say about it.
      for (const r of reading.hierarchy.regions) {
        if (r.figures.length === 0) unmeasured.push(`[${world.name}, ${what}] ${r.what} ("${r.heading}") rendered no figure: UNMEASURED for interface-craft C3`);
      }
    }
    // And the two regions the actions summoned really were on the screen when the second
    // reading was taken. Without this, a control that silently stopped working would turn
    // the extra reading into a copy of the first and nothing would say so.
    const actedContainers = craftContainers(acted.chromed).map((c) => c.what);
    const actedFigures = acted.hierarchy.regions.reduce((n, r) => n + r.figures.length, 0);
    const loadedFigures = s.hierarchy.regions.reduce((n, r) => n + r.figures.length, 0);
    summoned.push({ world: world.name, containers: actedContainers.length,
      figuresGained: actedFigures - loadedFigures });
    expect(actedContainers,
      `[${world.name}] the withheld-paths card is not among the containers the second reading measured, so the operator's action put a bordered card on the page without the density clause ever being decided over it`
    ).toContain("div#held.held");
    expect(actedFigures,
      `[${world.name}] the ledger search's results added no figure to the hierarchy reading, so either the search did not run or the clause was not decided over what it drew`
    ).toBeGreaterThan(loadedFigures);
  }

  for (const m of measured) {
    console.log(`${REPORT}craft: ${m.world} measured ${m.figures} figure(s), ${m.containers} bordered or raised container(s), ${m.sizes} painted text size(s)`);
  }
  for (const line of unmeasured) console.log(`${REPORT}craft: ${line}`);
  for (const t of summoned) {
    console.log(`${REPORT}craft: ${t.world}, after a search and a withholding: ${t.containers} container(s), ${t.figuresGained} figure(s) more than the page as it loads`);
  }
  testInfo.annotations.push({ type: "craft", description: JSON.stringify({ measured, unmeasured, summoned }) });

  expect(problems, problems.join("\n")).toEqual([]);
  for (const m of measured) {
    expect(m.figures,
      `the ${m.world} world contributed no figure measurement at all; interface-craft C3 must be DECIDED in every theme at every width, and a world that measured nothing is a missing run`
    ).toBeGreaterThan(0);
    expect(m.containers,
      `the ${m.world} world contributed no container measurement at all; interface-craft C5 must be DECIDED in every theme at every width, and a world that measured nothing is a missing run`
    ).toBeGreaterThan(0);
  }
  // Every theme-and-width combination was visited, and each contributed BOTH of its
  // readings: the page as it loads and the page after the operator has acted on it.
  expect(summoned.map((t) => t.world), "not every theme and width combination was visited")
    .toEqual(WORLDS.map((w) => w.name));
  expect(measured.length, "a world contributed fewer than its two readings")
    .toBe(WORLDS.length * 2);
});

// The empty snapshot. The page has nothing to list and still renders its counts, its
// controls and its states, so both clauses are still decided over what it DOES render -
// and nothing it reports may be caused by the rows that are not there.
//
// Criteria: AC-8 (both clauses decided under a snapshot with nothing to list, and no
// problem caused solely by the absence of rows), AC-7 (the region that renders no figure
// under this snapshot is named rather than counted as a pass).
test("hierarchy and density are decided under a snapshot with nothing to list", async ({ browser, baseURL }, testInfo) => {
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "empty"), theme: "dark", width: 1280, height: 900,
    // The SNAPSHOT-driven views, which are the four a snapshot with nothing to list is a
    // fact about. The ledger search's results view is filled by a search an operator asks
    // for, so an empty snapshot leaves it exactly where it was and waiting for it to say
    // "empty" would be waiting for the page to answer a question nobody put.
    waitFor: (p) => p.waitForFunction(() => {
      const v = window.__hf.snapshotViews();
      return v.length === 4 && v.every((x) => x.state === "empty");
    }, null, { timeout: 15000 }),
  });
  const s = await collect(page);
  await ctx.close();

  const withFigures = s.hierarchy.regions.filter((r) => r.figures.length > 0);
  const without = s.hierarchy.regions.filter((r) => r.figures.length === 0);
  for (const r of without) {
    console.log(`${REPORT}craft: [empty snapshot] ${r.what} ("${r.heading}") rendered no figure: UNMEASURED for interface-craft C3`);
  }
  testInfo.annotations.push({ type: "craft", description: JSON.stringify({
    regions: s.hierarchy.regions.map((r) => ({ what: r.what, figures: r.figures.length })),
    containers: craftContainers(s.chromed).length,
  }) });

  const problems = craftProblems(s);
  expect(problems, problems.join("\n")).toEqual([]);
  // And it decided something: the empty page still paints figures somewhere and still
  // draws chrome somewhere, so "no problem" here is a verdict rather than a shrug.
  expect(withFigures.length,
    "the empty page rendered no figure in any region, so the hierarchy clause was not decided over it at all"
  ).toBeGreaterThan(0);
  expect(craftContainers(s.chromed).length,
    "the empty page rendered no bordered or raised container, so the density clause was not decided over it at all"
  ).toBeGreaterThan(0);
});

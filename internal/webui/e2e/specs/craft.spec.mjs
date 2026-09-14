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

test("hierarchy and density hold in every theme at every width, and every world measured something", async ({ browser, baseURL }, testInfo) => {
  const problems = [];
  const measured = [];
  const unmeasured = [];

  for (const world of WORLDS) {
    const { ctx, page } = await open(browser, {
      url: pageURL(baseURL, "full"), theme: world.theme, width: world.width, height: 900,
      waitFor: waitRendered,
    });
    const s = await collect(page);
    await ctx.close();

    for (const prob of craftProblems(s)) problems.push(`[${world.name}] ${prob}`);

    const figures = s.hierarchy.regions.reduce((n, r) => n + r.figures.length, 0);
    measured.push({ world: world.name, figures, containers: craftContainers(s.chromed).length,
      sizes: s.hierarchy.sizes.length });
    // Clause AC-7's half: a region that rendered no figure is UNMEASURED for the hierarchy
    // clause. It is not a pass and it is not a problem - it is named, here, so a region
    // that quietly stopped rendering its figures is visible in the run rather than
    // absorbed by a grader that found nothing to say about it.
    for (const r of s.hierarchy.regions) {
      if (r.figures.length === 0) unmeasured.push(`[${world.name}] ${r.what} ("${r.heading}") rendered no figure: UNMEASURED for interface-craft C3`);
    }
  }

  for (const m of measured) {
    console.log(`craft: ${m.world} measured ${m.figures} figure(s), ${m.containers} bordered or raised container(s), ${m.sizes} painted text size(s)`);
  }
  for (const line of unmeasured) console.log(`craft: ${line}`);
  testInfo.annotations.push({ type: "craft", description: JSON.stringify({ measured, unmeasured }) });

  expect(problems, problems.join("\n")).toEqual([]);
  for (const m of measured) {
    expect(m.figures,
      `the ${m.world} world contributed no figure measurement at all; interface-craft C3 must be DECIDED in every theme at every width, and a world that measured nothing is a missing run`
    ).toBeGreaterThan(0);
    expect(m.containers,
      `the ${m.world} world contributed no container measurement at all; interface-craft C5 must be DECIDED in every theme at every width, and a world that measured nothing is a missing run`
    ).toBeGreaterThan(0);
  }
  expect(measured.length, "not every theme and width combination was visited").toBe(WORLDS.length);
});

// The empty snapshot. The page has nothing to list and still renders its counts, its
// controls and its states, so both clauses are still decided over what it DOES render -
// and nothing it reports may be caused by the rows that are not there.
test("hierarchy and density are decided under a snapshot with nothing to list", async ({ browser, baseURL }, testInfo) => {
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "empty"), theme: "dark", width: 1280, height: 900,
    waitFor: (p) => p.waitForFunction(() => {
      const v = window.__hf.views();
      return v.length === 4 && v.every((x) => x.state === "empty");
    }, null, { timeout: 15000 }),
  });
  const s = await collect(page);
  await ctx.close();

  const withFigures = s.hierarchy.regions.filter((r) => r.figures.length > 0);
  const without = s.hierarchy.regions.filter((r) => r.figures.length === 0);
  for (const r of without) {
    console.log(`craft: [empty snapshot] ${r.what} ("${r.heading}") rendered no figure: UNMEASURED for interface-craft C3`);
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

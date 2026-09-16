// interface-craft C7 (components ship their whole state matrix), decided in a real engine.
//
// Every interactive component renders and proves default, hover, focus-visible, active,
// disabled, loading and error, in every density the surface builds. Until now this
// repository proved none of them: a control with no disabled treatment and no hover
// affordance passed every suite it had.
//
// THE INVENTORY IS DERIVED, never listed. It is every element the ACCESSIBILITY TREE reports
// with an interactive role AND the engine places in the tab order, joined at the engine and
// grouped by what the document actually says each one is. A list would agree with the page
// the day it was written; a control added later would enter the surface without entering the
// matrix, which is the exact failure AC-10 refuses.
//
// THE RECORD IS COMMITTED. docs/state-matrix.md carries one entry per component per state -
// proved, or not applicable with a sentence saying why - and the density set the surface
// builds. Four states may never be declared away: default, hover, focus-visible and active
// are reachable at the engine for anything focusable, so only disabled, loading and error
// can be, and only with a reason.
//
// EVERY PROVED CELL IS ENTERED FOR REAL. Focus is moved by a dispatched Tab and read while
// the engine holds it; hover and active are a pointer the engine moved and a button it is
// holding down; disabled is the element's own property. Nothing is entered by injecting a
// class, an attribute or an inline style that names a treatment - a constructed KeyboardEvent
// is untrusted and moves focus nowhere, so a grader that injected would be reporting a page
// it drew itself.
import { test, expect } from "@playwright/test";
import { open, waitRendered } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import {
  readStateMatrixRecord,
  runMatrixForDensity,
  executeCells,
} from "./matrix.mjs";
import {
  parseStateMatrixRecord,
  densityEntry,
  gradeDensityMechanismsAreReadable,
  groupComponents,
  gradeInventoryIsWholeAndGrouped,
  gradeRecordCoversEveryComponent,
  gradeProvedStatesRenderApart,
  gradeDensitiesAreReallyDifferent,
  densityShortfall,
  C7_STATES,
} from "./graders.mjs";

const REPORT = "e2e-report: ";

// The matrix runs in the DEFAULT theme only, and the scope says why: both themes are already
// graded for contrast by the suites beside this one, and doubling every cell for a property
// another grader owns is cost with no new verdict behind it. "" is the absence of a
// colour-scheme override at the engine, which is what the token file calls the default.
const DEFAULT_THEME = "";
const SCENARIO = "full";

test("every interactive component proves its whole state matrix, in every density the surface builds", async ({ browser, baseURL }, testInfo) => {
  // A DECLARED budget. This case derives the inventory at the engine, walks the tab order for
  // real and then enters one state at a time on each component - a pointer move, a press, a
  // property - once per density. It is measured rather than guessed: about 25s for seven
  // components at one density on an idle machine, and the multiplier when S0053 builds the
  // second density is exactly two.
  test.setTimeout(300_000);

  const record = parseStateMatrixRecord(readStateMatrixRecord());
  const densities = record.densities.map(densityEntry);
  const problems = [...gradeDensityMechanismsAreReadable(densities)];

  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, SCENARIO), theme: DEFAULT_THEME, width: 1440, height: 1000,
    waitFor: waitRendered,
  });

  const perDensity = [];
  let executed = 0;
  for (const density of densities) {
    const run = await runMatrixForDensity(ctx, page, density);
    const groups = groupComponents(run.inventory);
    problems.push(...gradeInventoryIsWholeAndGrouped(run.inventory, run.strays, groups));
    problems.push(...gradeRecordCoversEveryComponent(record, groups));

    const cells = await executeCells(page, density, groups, record, run.defaults, run.stops);
    problems.push(...gradeProvedStatesRenderApart(cells));
    executed += cells.length;

    // The defaults, keyed by COMPONENT rather than by element, so one density can be
    // compared with the next over the same subjects.
    const defaults = {};
    for (const g of groups) {
      if (!g.graded) continue;
      defaults[g.key] = run.defaults.get(g.graded.index).props;
    }
    perDensity.push({ density: density.name, groups, cells, defaults, strays: run.strays });
  }
  await ctx.close();

  problems.push(...gradeDensitiesAreReallyDifferent(perDensity));

  // AC-7's and AC-15's own half: the run SAYS what it derived and what it executed. The
  // mutation suite asserts these lines are here and name something, because three criteria
  // require a declaration that a zero exit cannot carry.
  const declared = densities.map((d) => d.name);
  const first = perDensity[0];
  if (first) {
    console.log(`${REPORT}c7: derived ${first.groups.length} component group(s) from the accessibility tree and the tab order, over ${
      first.groups.reduce((n, g) => n + g.count, 0)} instance(s)`);
    for (const g of first.groups) {
      console.log(`${REPORT}c7: component "${g.key}" (${g.roles.join(", ")}): ${g.count} instance(s), graded ${
        g.graded ? g.graded.what : "none"}`);
    }
  }
  console.log(`${REPORT}c7: densities declared built: ${declared.join(", ") || "none"}; ran ${
    executed} cell(s) over ${C7_STATES.length} state(s) per component per density`);
  const shortfall = densityShortfall(declared);
  if (shortfall) console.log(`${REPORT}c7: ${shortfall}`);
  testInfo.annotations.push({ type: "c7", description: JSON.stringify({
    declared, executed,
    groups: (first ? first.groups : []).map((g) => ({ key: g.key, count: g.count, roles: g.roles })),
  }) });

  expect(problems, problems.join("\n")).toEqual([]);
  // And it decided something. A run that derived no component, or executed no cell, has a
  // green exit and no verdict behind it.
  expect(executed, "the matrix executed no cell at all, so nothing was proved").toBeGreaterThan(0);
});

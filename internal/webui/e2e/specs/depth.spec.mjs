// interface-craft C4 (ornament has a ceiling), decided in a real engine.
//
// Three sentences, three predicates, one sweep. A view draws at most one shadow depth; a
// region is separated from its neighbour by a border OR a surface change and never both; no
// element carries a coloured left-border strip as its only state signal. Until now nothing
// in this repository graded any of them, so a second shadow depth or a colour-only flag
// shipped unnoticed.
//
// WHAT IS MEASURED, and why it is not a list. A VIEW is one fixture scenario the harness
// serves, rendered at one project the runner presents - so the set is the fixture server's
// own answer (`/e2e/scenarios`) crossed with the runner's own configuration, and a fixture
// or a project added later is swept the day it is added rather than the day somebody
// remembers to extend a list here. The run REPORTS what it measured, per view, because a
// reader of the gate cannot tell a sweep from a shrug by a zero exit.
//
// AT REST is the condition the first clause is only true under, and the reading refuses to
// be taken any other way: a focus ring or a hover elevation is not a second depth, so a
// reading taken while anything was hovered, focused or being pressed is a failure rather
// than a measurement.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import { axInteractiveElements, scenariosServed, worldsPresented } from "./matrix.mjs";
import {
  gradeOneShadowDepth,
  gradeNeighboursAreSeparatedOnce,
  gradeNoColourOnlyLeftStrip,
  gradeC4MeasuredSomething,
} from "./graders.mjs";

// REPORT marks a line printed for a READER OF THE GATE rather than for the runner. Its
// other writers are craft.spec.mjs and statematrix.spec.mjs, and its reader is the Go
// wrapper in internal/webui/e2e_test.go, which lifts these back out of the output it
// otherwise swallows on success.
const REPORT = "e2e-report: ";

// ornamentOf takes one whole C4 reading of one view. The set of elements the ACCESSIBILITY
// TREE calls interactive is derived first and handed to the reading, because a control's own
// edge is not a region separation and which elements are controls is the engine's answer
// rather than a selector's.
async function ornamentOf(browser, url, world) {
  const { ctx, page } = await open(browser, {
    url, theme: world.theme, width: world.width, height: world.height, waitFor: waitRendered,
  });
  const cdp = await ctx.newCDPSession(page);
  const { interactive } = await axInteractiveElements(cdp, page);
  const reading = await page.evaluate(
    (idx) => window.__hf.ornament(idx), interactive.map((n) => n.index));
  await ctx.close();
  return reading;
}

// Criteria: AC-1 (one shadow depth per view), AC-2 (measured at rest), AC-3 (a border or a
// surface change, never both), AC-4 (no colour-only left strip), AC-5 (every view the
// harness serves at every project it presents, and the run says which), AC-6 (no view and no
// region are failures, never a pass).
test("ornament has a ceiling in every view the harness serves", async ({ browser, request, baseURL }, testInfo) => {
  // A DECLARED budget, larger than the project's 30s default, because this case is
  // deliberately larger than a case: it is the whole cross product of the fixtures the
  // harness serves and the projects the runner presents, and every one of them is a real
  // page load, a real accessibility tree and a whole reading. AC-5 is why they are one case
  // rather than one per view - "every view was measured" is only assertable where every view
  // ran - and a wedge has to fail with its output rather than hang, which is the rule the
  // 30s default exists for.
  test.setTimeout(300_000);

  const scenarios = await scenariosServed(request, baseURL);
  const worlds = worldsPresented();
  const problems = [];
  const measured = [];

  // One scenario's worlds are read together and the scenarios in order. Each world is its
  // own browser context with its own preference and viewport, so nothing is shared between
  // them and a reading cannot be disturbed by the run beside it; four at once is the same
  // cap the runner puts on its own workers, and for the same reason - every one of these
  // waits on a real render rather than on a processor.
  for (const scenario of scenarios) {
    const readings = await Promise.all(worlds.map((world) =>
      ornamentOf(browser, pageURL(baseURL, scenario), world)));
    for (let i = 0; i < worlds.length; i++) {
      const world = worlds[i], o = readings[i];
      const view = `the "${scenario}" fixture at the ${world.project} project (${
        world.theme === "" ? "no colour-scheme preference" : world.theme} theme, ${world.width}px)`;
      problems.push(
        ...gradeOneShadowDepth(view, o),
        ...gradeNeighboursAreSeparatedOnce(view, o),
        ...gradeNoColourOnlyLeftStrip(view, o),
      );
      const depths = [...new Set(o.shadows.map((s) => s.shadow))];
      measured.push({ view, scenario, project: world.project,
        elements: o.elements, regions: o.regions.length, pairs: o.pairs.length,
        unresolved: o.unresolved.length, depths, strips: o.strips.length });
    }
  }
  problems.push(...gradeC4MeasuredSomething(measured));

  // AC-5's own half: the run SAYS what it measured. A reader of the gate cannot tell a
  // sweep from a grader that found nothing to look at by reading a zero exit, and the
  // mutation suite asserts these lines are here and name something.
  console.log(`${REPORT}c4: measured ${measured.length} view(s) - ${scenarios.length} fixture scenario(s) (${
    scenarios.join(", ") || "none"}) at ${worlds.length} project(s) (${worlds.map((w) => w.project).join(", ") || "none"})`);
  for (const m of measured) {
    console.log(`${REPORT}c4: ${m.view}: ${m.regions} region(s), ${m.pairs} neighbouring pair(s), ${
      m.depths.length} shadow depth(s) [${m.depths.join(" | ") || "none"}], ${m.strips} left-strip subject(s), over ${m.elements} element(s)`);
  }
  testInfo.annotations.push({ type: "c4", description: JSON.stringify(measured) });

  expect(problems, problems.join("\n")).toEqual([]);
});

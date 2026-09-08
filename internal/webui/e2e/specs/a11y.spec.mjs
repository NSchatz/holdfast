// The two criteria that exist ONLY at the engine: what Tab actually focuses, and what the
// accessibility tree says each control is called.
//
// Neither is observable from the document. A KeyboardEvent constructed inside the page is
// untrusted and moves focus nowhere, so tab order cannot be read by any expression the
// page evaluates; and an accessible name is the engine's own answer over labels, ARIA,
// native semantics and content, not a property of markup anything can reconstruct by
// inspection. This is why these were driven over the DevTools protocol by hand, and it is
// exactly the vocabulary the runner already speaks.
//
// These are the cases that grade the SHIPPED page. mutations.spec.mjs defeats the same two
// graders on purpose; that proves each one bites, and proves nothing about this page. Both
// halves are needed, and it is the half below that would go missing without a sound: a
// suite holding only the counterexamples reports "ok" over a dashboard whose controls have
// lost their names.
import { test, expect } from "@playwright/test";
import { open, waitRendered, axTree } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";
import { tabThroughEveryControl, gradeTabOrderAndFocusRing, gradeAccessibleNames } from "./a11y.mjs";

// Clause F1's keyboard half, in both themes: a REAL Tab reaches every control the page
// offers, in the order the page is read, and each one shows where the keyboard is.
test("a real Tab reaches every control in reading order and shows where it is", async ({ browser, baseURL }) => {
  const problems = [];
  let focused = 0;
  for (const theme of ["light", "dark"]) {
    const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme, waitFor: waitRendered });
    const { expected, reached } = await tabThroughEveryControl(page);
    await ctx.close();
    focused += reached.length;
    expect(expected.length,
      `[${theme}] the page offered ${expected.length} focusable controls; it has a token field, a filter and three buttons`
    ).toBeGreaterThanOrEqual(5);
    problems.push(...gradeTabOrderAndFocusRing(theme, expected, reached));
  }
  // A run in which the key presses moved nothing would report no problems and prove
  // nothing, which is the exact failure this whole criterion exists to catch.
  expect(focused, "no control was ever focused, so this case measured nothing").toBeGreaterThanOrEqual(10);
  expect(problems, problems.join("\n")).toEqual([]);
});

// Clause F1 read off the tree the ENGINE computed: every interactive control and every
// region heading exposes a name, and none is named only by a placeholder that disappears
// the moment a value is typed.
test("the accessibility tree the engine computed names every control and every heading", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const nodes = await axTree(ctx, page);
  await ctx.close();
  expect(nodes.length,
    "the engine computed no accessibility tree at all, so this case asserted nothing").toBeGreaterThan(20);
  const probs = gradeAccessibleNames(nodes);
  expect(probs, probs.join("\n")).toEqual([]);
});

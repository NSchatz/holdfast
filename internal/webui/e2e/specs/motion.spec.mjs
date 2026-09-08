// Clause S9: motion is decoration, so removing it removes nothing.
import { test, expect } from "@playwright/test";
import { gradeReducedMotion, gradeMotionExists } from "./graders.mjs";
import { pageURL } from "./theme.mjs";
import { motionPair, gradeReducedMotionCostsNoValue } from "./motionpair.mjs";





test("reduced motion removes every duration and costs no value", async ({ browser, baseURL }) => {
  const { moving, still } = await motionPair(browser, pageURL(baseURL, "full"));
  const probs = [
    // The page computes motion at all WITHOUT the preference, or the reduce grader below
    // would be honouring the query trivially and could never fail.
    ...gradeMotionExists(moving),
    ...gradeReducedMotion(still),
    ...gradeReducedMotionCostsNoValue(moving, still),
  ];
  expect(probs, probs.join("\n")).toEqual([]);
});

// The grader must be insensitive to the ONE thing that legitimately differs between two
// renders taken at different instants: the wall clock. This ages the first render well past
// a tick, so the two land on different seconds, and requires the grader to stay silent.
test("the reduced-motion grader is insensitive to the live clock", async ({ browser, baseURL }) => {
  const { moving, still } = await motionPair(browser, pageURL(baseURL, "full"), { ageMs: 2500 });
  expect(moving.stable.values.join("|"),
    `the two renders landed on the same second (${JSON.stringify(moving.stable.values)}), so this case measured nothing; the elapsed ticker or the fixture changed`
  ).not.toBe(still.stable.values.join("|"));
  const probs = gradeReducedMotionCostsNoValue(moving, still);
  expect(probs,
    `the reduced-motion grader reds on a correct page whose two renders differ only in the wall-clock column (${JSON.stringify(moving.stable.values)} vs ${JSON.stringify(still.stable.values)}): ${probs.join("\n")}`
  ).toEqual([]);
});

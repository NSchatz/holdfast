// Clause S9's second half: motion is decoration, so removing it removes nothing.
import { open, collect, waitRendered } from "./conventions.mjs";

const flatten = (s) => String(s).split(/\s+/).filter(Boolean).join(" ");

// The second half of clause S9: under an emulated reduce preference the page still renders
// every value, state change and status it carried with motion in place.
//
// It compares the two renders WITH THE LIVE CLOCK EXCLUDED, and that exclusion is the whole
// of the grader's honesty. The queue's elapsed column is rewritten every second from the
// wall clock, and the two renders cannot be taken at the same instant: the motion render
// waits for the engine's animations to drain, which costs real time and costs the reduce
// render none. A comparison of the whole of innerText therefore compares how OLD each page
// happened to be as well as what reduced motion did, and reds on a correct page whenever
// the difference straddles a second - which is what it did.
//
// The exclusion is structural (a selector in the probe), not a guess at which words look
// like a duration, and it is not a hole: the excluded cells are counted on both sides and
// each is required to still carry a value, so reduced motion cannot take that column away,
// blank it, or add a member to it without this grader saying so.
export function gradeReducedMotionCostsNoValue(moving, still) {
  const out = [];
  if (moving.stable.cells === 0) {
    return ["the page rendered no live-clock figure at all, so the exclusion below covered nothing and this grader asserts less than it claims"];
  }
  if (still.stable.cells !== moving.stable.cells) {
    out.push(`the page renders ${moving.stable.cells} live-clock figures with motion and ${still.stable.cells} with it reduced; reduced motion took a value off the page`);
  }
  for (const side of [
    { what: "with motion", values: moving.stable.values },
    { what: "with it reduced", values: still.stable.values },
  ]) {
    side.values.forEach((v, i) => {
      if (String(v).trim() === "") {
        out.push(`live-clock figure ${i + 1} is EMPTY ${side.what}; it is a value the page carries and the comparison below excludes it`);
      }
    });
  }
  if (flatten(still.stable.text) !== flatten(moving.stable.text)) {
    out.push(`the page shows different text with reduced motion (the live clock excluded from both readings).\nwith motion:    "${flatten(moving.stable.text)}"\nreduced motion: "${flatten(still.stable.text)}"`);
  }
  if (JSON.stringify(still.badges) !== JSON.stringify(moving.badges)) {
    out.push(`the badges read ${JSON.stringify(moving.badges)} with motion and ${JSON.stringify(still.badges)} with it reduced`);
  }
  return out;
}

// Renders the same served document twice: once as it ships, once under an emulated
// prefers-reduced-motion: reduce.
export async function motionPair(browser, url, opts = {}) {
  const { mutation = null, ageMs = 0 } = opts;
  const a = await open(browser, { url, theme: "light", mutation, waitFor: waitRendered });
  if (ageMs) await a.page.waitForTimeout(ageMs);
  const moving = await collect(a.page);
  await a.ctx.close();

  const b = await open(browser, { url, theme: "light", reduce: true, mutation, waitFor: waitRendered });
  const still = await collect(b.page);
  await b.ctx.close();
  return { moving, still };
}

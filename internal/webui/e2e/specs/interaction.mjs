// Clauses F11, F1 and F5: the browser's OWN verdict on the page, and what a refused control
// action costs.
import { open, collect, waitRendered } from "./conventions.mjs";
import { convGraders, gradeViewsAllIn } from "./graders.mjs";
import { pageURL } from "./theme.mjs";

// Arms the fixture server to answer every control action with one status. It is a TEST
// endpoint, not the page's: a refusal has to be produced by a REAL click on the shipped
// page rather than by a stub reaching inside it.
export async function armControlStatus(page, baseURL, code) {
  await page.request.post(`${baseURL}/e2e/control-status?code=${code}`);
}

// Clause F11 as the ENGINE reports it. Not that a policy header was set, but that the
// browser made no Content-Security-Policy or Trusted Types refusal while rendering.
//
// It asserts a list is EMPTY, which is the shape of grader that most needs a
// counterexample: a dead instrument and a clean page report the same nothing. Its
// counterexample is in mutations.spec.mjs.
export function gradeNoPolicyRefusal(what, refusals) {
  return refusals.map((r) => `${what}: the browser refused something against the page: ${r}`);
}

// Clauses F1 and F5 on the control path: a refusal says IN WORDS that the action did not
// happen, renders nothing a reader could take for success, and costs the page nothing else
// - not a row, not a figure, not a badge, not a control, and not a policy refusal.
export function gradeRefusedControlActionCostsNothingElse(before, after, refusals) {
  const out = [];
  const msg = String(after.msg.text).toLowerCase();
  if (!msg.includes("nothing changed")) {
    out.push(`a refused control action reports "${after.msg.text}"; it must say, in words, that the action did not happen`);
  }
  for (const success of ["done", " ok", "started", "rescanning"]) {
    if (msg.includes(success)) {
      out.push(`a refused control action reports "${after.msg.text}", which reads as the action having succeeded`);
    }
  }
  // Every other region is still rendered and every control is still operable.
  if (after.figures.cells.length !== before.figures.cells.length || after.figures.aggs.length !== before.figures.aggs.length) {
    out.push(`a refused control action cost the page its content: ${before.figures.cells.length} cells and ${before.figures.aggs.length} figures became ${after.figures.cells.length} and ${after.figures.aggs.length}`);
  }
  if (JSON.stringify(after.badges) !== JSON.stringify(before.badges)) {
    out.push(`a refused control action moved the badges from ${JSON.stringify(before.badges)} to ${JSON.stringify(after.badges)}; nothing on the server changed`);
  }
  for (const ctl of after.controls) {
    if (!ctl.rendered) out.push(`a refused control action left "${ctl.id}" off the screen`);
  }
  out.push(...gradeNoPolicyRefusal("driving a refused control action", refusals));
  // And the whole convention set still holds on the page that just refused an action.
  for (const g of convGraders()) {
    if (g.name === "wide content scrolls inside its own container") continue;
    for (const prob of g.probe(after)) out.push(`after a refused control action, ${g.name}: ${prob}`);
  }
  return out;
}

// Drives a REAL click on a control the server answers 401 to, and reads the page either
// side of it.
export async function refuseAControlAction(browser, baseURL, mutation = null) {
  const { ctx, page, violations } = await open(browser, {
    url: pageURL(baseURL, "full"), theme: "light", mutation, waitFor: waitRendered,
  });
  const before = await collect(page);
  await armControlStatus(page, baseURL, 401);
  await page.click("#rescan");
  await page.waitForFunction(() => {
    const m = window.__hf.msg().text;
    return m !== "" && m !== "working";
  }, null, { timeout: 10000 });
  const after = await collect(page);
  await armControlStatus(page, baseURL, 0);
  await ctx.close();
  return { before, after, refusals: violations };
}

// Clauses F7 and F11 on the path where the page HAS scripting, asked the server for a
// snapshot, and was refused: every view shows its unreadable state in words, the AGPL
// source offer is still on the screen, every control is still there, and the browser
// refused nothing.
export function gradeServerErrorStillRendersTheOfferAndTheControls(s, refusals) {
  const out = gradeViewsAllIn("unreadable")(s);
  if (!s.offer.present || !s.offer.shown) {
    out.push(`the AGPL source offer is not on the screen when the snapshot endpoint errors: ${JSON.stringify(s.offer)}`);
  }
  if (!String(s.offer.text).includes("Corresponding Source")) out.push(`the source offer reads "${s.offer.text}"`);
  for (const ctl of s.controls) {
    if (!ctl.present || !ctl.rendered) out.push(`the control "${ctl.id}" is not on the page when the snapshot endpoint errors`);
  }
  return out.concat(gradeNoPolicyRefusal("a server error on the snapshot endpoint", refusals));
}

// Clause F11's second half: a hostile media path, failure reason and bucket label are SHOWN
// to a reader, as text, and nothing comes with them.
//
// The document is loaded at the TOP LEVEL, so the response's own Content-Security-Policy
// governs it exactly as it governs a reader's page, and the engine's report of every
// refusal is read straight off its own log.
import { open, collect, waitRendered } from "./conventions.mjs";
import { convGraders } from "./graders.mjs";
import { pageURL } from "./theme.mjs";
import { gradeNoPolicyRefusal } from "./interaction.mjs";

// What the hostile value must not move: the elements, scripts, media elements and
// event-handler attributes the rendered document carries.
const DOM_COUNTS = () => ({
  elements: document.getElementsByTagName("*").length,
  scripts: document.getElementsByTagName("script").length,
  media: document.querySelectorAll("img, iframe, object, embed, svg image, use").length,
  handlers: document.querySelectorAll("[onerror],[onload],[onclick],[onmouseover],[onfocus],[onanimationend]").length,
});

// The hostile values are SHOWN, as text a reader can see, and nothing came with them - no
// element, no attribute, no handler, no policy refusal - and the page showing them still
// meets every convention.
export function gradeHostileTextIsInert(base, got, s, refusals) {
  const out = [];
  for (const want of ["onerror=alert(1)", "onmouseover=", "<script>alert(2)</script>"]) {
    if (!String(s.bodyText).includes(want)) {
      out.push(`the hostile value "${want}" is not in the text a reader can see; it must be shown, inert, not swallowed`);
    }
  }
  if (got.scripts !== base.scripts) out.push(`hostile text changed the script count from ${base.scripts} to ${got.scripts}`);
  if (got.media !== 0 || base.media !== 0) {
    out.push(`hostile text put ${got.media} media elements on the page (the clean page has ${base.media}, and both must be 0)`);
  }
  if (got.handlers !== 0 || base.handlers !== 0) {
    out.push(`hostile text put ${got.handlers} event-handler attributes on the page (the clean page has ${base.handlers}, and both must be 0)`);
  }
  out.push(...gradeNoPolicyRefusal("rendering hostile text", refusals));
  for (const g of convGraders()) {
    if (g.name === "wide content scrolls inside its own container") continue;
    for (const prob of g.probe(s)) out.push(`with hostile text on the page, ${g.name}: ${prob}`);
  }
  return out;
}

// Reads the clean page and the hostile one, so the counts are a COMPARISON and not a
// number somebody decided was right.
export async function readHostileTextPair(browser, baseURL, mutation = null) {
  const clean = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const base = await clean.page.evaluate(DOM_COUNTS);
  await clean.ctx.close();

  const h = await open(browser, { url: pageURL(baseURL, "hostile"), theme: "light", mutation, waitFor: waitRendered });
  await h.page.waitForTimeout(300);
  const got = await h.page.evaluate(DOM_COUNTS);
  const s = await collect(h.page);
  const refusals = h.violations.slice();
  await h.ctx.close();
  return { base, got, s, refusals };
}

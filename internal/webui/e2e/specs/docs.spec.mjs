// Clause F8's RENDERED half: each region carries exactly one link to this repository's own
// documentation for that region's methodology, that link names its OWN section of that
// document, and no claim taken off the surface is still printed on it.
//
// The other half - that every moved claim IS in the document those links name - is a read
// of two committed files and needs no browser; it stays in Go, where that is plain.
import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { open, collect, waitRendered } from "./conventions.mjs";
import { gradeDocLinks } from "./graders.mjs";
import { pageURL } from "./theme.mjs";

// The repository-relative path of the document the methodology lives in. It is a constant
// on the Go side too (webui.DocPath), and the links the page renders are built from it, so
// a link that stopped naming it is what this notices.
const DOC_PATH = "docs/dashboard-methodology.md";

// One list, two readers, one source. See internal/webui/prose/moved-claims.txt.
const MOVED_CLAIMS = readFileSync(fileURLToPath(new URL("../../prose/moved-claims.txt", import.meta.url)), "utf8")
  .split("\n").map((l) => l.trim()).filter((l) => l && !l.startsWith("#"));

const flatten = (s) => String(s).split(/\s+/).filter(Boolean).join(" ").toLowerCase();

test("each region links its methodology once, at its own section, and prints no moved claim", async ({ browser, baseURL }) => {
  expect(MOVED_CLAIMS.length, "no moved claim was read, so this asserted nothing").toBeGreaterThan(5);

  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const s = await collect(page);
  await ctx.close();

  const probs = gradeDocLinks(s);
  expect(probs, probs.join("\n")).toEqual([]);
  expect(s.doclinks.length, `the page rendered ${s.doclinks.length} regions, want the two the dashboard has`).toBe(2);

  // Each region's link names the section of the document that carries ITS methodology, not
  // merely the document.
  const wantFragment = {
    "region-now": "#right-now",
    "region-history": "#what-it-has-done-to-your-library",
  };
  for (const r of s.doclinks) {
    if (r.count !== 1) continue;
    const want = wantFragment[r.region];
    expect(want, `the page rendered an unexpected region "${r.region}"`).toBeTruthy();
    expect(r.hrefs[0].endsWith(want),
      `the region ${r.region} links "${r.hrefs[0]}", which does not name its own section (${want})`).toBe(true);
    expect(r.hrefs[0].includes(DOC_PATH),
      `the region ${r.region} links "${r.hrefs[0]}", which does not name ${DOC_PATH}`).toBe(true);
  }

  // And nothing that came off the surface is still on it.
  const surface = flatten(s.bodyText);
  for (const claim of MOVED_CLAIMS) {
    expect(surface.includes(flatten(claim)),
      `the claim "${claim}" is still printed on the surface; F8 puts the paragraphs in the docs`).toBe(false);
  }
});

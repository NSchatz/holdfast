// Rendered evidence for `frontend` F12: every view this item changed, in both themes at
// 360 and at desktop width, from the SAME engine and the SAME served document the graders
// read - the fixture server mounts the real webui.HandlerFor, so these are pictures of the
// page `holdfast serve` produces rather than of a copy assembled here.
//
// It is a script rather than a spec because it decides nothing. The graders decide; this
// leaves the images a refuter grades the craft clauses against.
//
// Usage, from internal/webui/e2e, with the fixture server already listening:
//   node shots.mjs <baseURL> <outDir> [browser]
//
// The engine is NAMED by the caller - HOLDFAST_BROWSER, or the third argument - and this
// process never searches for one, exactly as the driver does not: evidence produced by an
// engine nobody chose is evidence nobody can reason about.
import { chromium } from "@playwright/test";
import { mkdir } from "node:fs/promises";
import { join } from "node:path";

const baseURL = process.argv[2];
const outDir = process.argv[3];
const executablePath = process.argv[4] || process.env.HOLDFAST_BROWSER;
if (!baseURL || !outDir) {
  process.stderr.write("usage: node shots.mjs <baseURL> <outDir> [browser]\n");
  process.exit(2);
}
if (!executablePath) {
  process.stderr.write("shots: no engine named; pass one or set HOLDFAST_BROWSER\n");
  process.exit(2);
}

await mkdir(outDir, { recursive: true });
const browser = await chromium.launch({ executablePath });

// The two widths F12 names. 360 is clause F9's own; "desktop" is the width the wide
// projects grade at.
const WIDTHS = [
  ["360", { width: 360, height: 900 }],
  ["desktop", { width: 1440, height: 1000 }],
];

async function settle(page) {
  await page.waitForFunction(() => {
    const sr = document.getElementById("sr-status");
    return !!sr && sr.textContent.trim() !== "";
  }, null, { timeout: 20000 });
}

async function withToken(page) {
  await page.evaluate(() => {
    const e = document.getElementById("token");
    e.value = "a-token";
    e.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

// clipTo answers the box the engine laid an element out in, so a view that is a REGION of
// the dashboard is photographed as that region rather than as the whole page with the
// region somewhere in it. A craft clause is read off what a region shows; an image where
// it is one band among twelve is an image nobody can read it from.
// The timeout is DECLARED rather than left to the runner's default, because this script is
// not a test and nothing here reports a wedge: a selector that matches nothing waits, and a
// run that hangs is indistinguishable from a slow one until somebody kills it. Fifteen
// seconds and then an error naming the selector is the failure a reader can act on.
async function clipTo(page, selector) {
  await page.locator(selector).scrollIntoViewIfNeeded({ timeout: 15000 });
  const box = await page.locator(selector).boundingBox({ timeout: 15000 });
  if (!box) throw new Error(`shots: ${selector} laid out no box, so there is nothing to photograph`);
  const pad = 12;
  return {
    x: Math.max(0, box.x - pad), y: Math.max(0, box.y - pad),
    width: box.width + pad * 2, height: box.height + pad * 2,
  };
}

// Each view this repository's surface work has changed, driven into the state the change is
// about. A view is either the whole page or one region of it: the regions are photographed
// clipped, because interface-craft C3 and C5 are read off a region and an image that shows
// it as one band among twelve is not evidence about it.
const VIEWS = {
  // The page as it arrives: the filter and the ledger search side by side, and the
  // per-row control on every terminal row.
  dashboard: async (page) => {
    await withToken(page);
  },
  // The live counts: the two state badges, the run's reclaimed figure and the band the
  // nine count chips are rows in - the containers clause C5 moved.
  counts: async (page) => {
    await withToken(page);
    return clipTo(page, "#counts");
  },
  // The two byte figures, entries in one bordered band rather than a card each. The band
  // is the container clause C5 recognises and neither figure draws a box of its own.
  "byte-figures": async (page) => {
    await withToken(page);
    return clipTo(page, ".stats");
  },
  // The aggregate cards, each a bordered container with its figure, its coverage and its
  // exclusions - and the buckets whose keys clause C3 moved.
  aggs: async (page) => {
    await withToken(page);
    return clipTo(page, "#aggregates");
  },
  // The control bar with the pointer ON the primary button. It is here because the hover
  // affordance is the one thing about this surface that a picture of the page AT REST cannot
  // show: interface-craft C7 asks every component to render the state, and a reader grading
  // that clause off a resting screenshot would be grading the absence of it.
  "controls-hover": async (page) => {
    await withToken(page);
    await page.hover("#rescan");
    return clipTo(page, "div.controls:has(#rescan)");
  },
  // A ledger search that found something, in its own region.
  "ledger-search": async (page) => {
    await withToken(page);
    await page.fill("#ledger-search", "some-film");
    await page.click("#search-go");
    await page.waitForSelector("#search-results tr", { timeout: 15000 });
    await page.locator("#found").scrollIntoViewIfNeeded();
  },
  // A withheld path, rendered back with the control that removes it.
  exclusions: async (page) => {
    await withToken(page);
    await page.click("#history td.remedy button");
    await page.waitForSelector("#held-list li", { timeout: 15000 });
    await page.locator("#held").scrollIntoViewIfNeeded();
  },
};

for (const [view, drive] of Object.entries(VIEWS)) {
  for (const theme of ["light", "dark"]) {
    for (const [label, viewport] of WIDTHS) {
      const ctx = await browser.newContext({ colorScheme: theme, viewport });
      const page = await ctx.newPage();
      await page.goto(`${baseURL}/?scenario=full`, { waitUntil: "load" });
      await settle(page);
      const clip = await drive(page);
      const file = join(outDir, `${view}.${theme}.${label}.png`);
      await page.screenshot(clip ? { path: file, clip } : { path: file, fullPage: true });
      process.stdout.write(`${file}\n`);
      await ctx.close();
    }
  }
}

await browser.close();

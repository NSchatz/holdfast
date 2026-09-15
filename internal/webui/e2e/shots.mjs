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

// The three views this item changed, each driven into the state the change is about.
const VIEWS = {
  // The page as it arrives: the filter and the ledger search side by side, and the
  // per-row control on every terminal row.
  dashboard: async (page) => {
    await withToken(page);
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
      await drive(page);
      const file = join(outDir, `${view}.${theme}.${label}.png`);
      await page.screenshot({ path: file, fullPage: true });
      process.stdout.write(`${file}\n`);
      await ctx.close();
    }
  }
}

await browser.close();

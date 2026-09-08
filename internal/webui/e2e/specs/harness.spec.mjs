// The harness's own proof. These are the graders that decide whether the rest of this
// suite is measuring anything at all: that the fixture server mounts the REAL handler
// (same document, same policy), that a scenario reaches the page, and that `shown` - which
// every other spec's verdict rests on - actually fails against something hidden.
import { test, expect } from "@playwright/test";
import { open, shown, expectShown, watchOrigins, readableText } from "./dashboard.mjs";

test("the graders load the served document, with the policy holdfast serve puts on the wire", async ({ page, baseURL }) => {
  const res = await page.goto("/?scenario=full");
  expect(res.status()).toBe(200);
  // Byte for byte the handler's own policy. If this moves, it moved in one place.
  expect(res.headers()["content-security-policy"]).toBe(
    "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; require-trusted-types-for 'script'"
  );
  expect(res.headers()["content-type"]).toContain("text/html");
});

test("a whole snapshot reaches the page and fills both tables", async ({ page }) => {
  await open(page, "full");
  await expect(page.locator("#history tr")).toHaveCount(10);
  await expect(page.locator("#queue tr")).toHaveCount(4);
  await expect(page.locator("#conn")).toHaveText("live");
});

test("the page fetches nothing from any origin but the one that served it", async ({ page, baseURL }) => {
  const foreign = watchOrigins(page, baseURL);
  await open(page, "full");
  expect(foreign, `the page requested off-origin resources: ${foreign.join(", ")}`).toEqual([]);
});

test("no console error, page error or failed request while rendering real data", async ({ page }) => {
  const problems = await open(page, "full");
  expect(problems, problems.join("\n")).toEqual([]);
});

// The harness's anti-vacuity proof. `shown` is the predicate every other verdict in this
// suite rests on, so it is driven against a subject hidden each of the four ways a page
// can hide one. A predicate nobody tries to defeat is a predicate nobody knows works.
for (const [how, css] of [
  ["display:none", "display:none"],
  ["visibility:hidden", "visibility:hidden"],
  ["opacity:0", "opacity:0"],
  ["a zero box", "display:block;width:0;height:0;overflow:hidden"],
]) {
  test(`the visibility predicate fails against a subject hidden by ${how}`, async ({ page }) => {
    await open(page, "full");
    const first = page.locator("#history tr").first();
    await expectShown(first, "the first history row"); // shown before
    await first.evaluate((el, c) => el.setAttribute("style", c), css);
    const after = await shown(first);
    expect(after.visible && after.hit, `${how} did not defeat the predicate, so it proves nothing`).toBe(false);
  });
}

test("the visibility predicate fails against a subject painted over", async ({ page }) => {
  await open(page, "full");
  const first = page.locator("#history tr").first();
  await expectShown(first, "the first history row");
  await first.evaluate((el) => {
    const r = el.getBoundingClientRect();
    const over = document.createElement("div");
    over.setAttribute("style", `position:fixed;left:${r.x}px;top:${r.y}px;width:${r.width}px;height:${r.height}px;background:red;z-index:9999`);
    document.body.appendChild(over);
  });
  const after = await shown(first);
  expect(after.hit, "something painted over the row did not defeat the hit test").toBe(false);
});

test("innerText reads what a reader sees and not a hidden subtree", async ({ page }) => {
  await open(page, "full");
  const t = await readableText(page.locator("#history tr").first());
  expect(t).toContain("failed");
  await page.locator("#history tr").first().evaluate((el) => {
    el.querySelector("td.path").style.display = "none";
  });
  const after = await readableText(page.locator("#history tr").first());
  expect(after, "innerText returned text from a display:none subtree").not.toContain("Corrupt Rip");
});

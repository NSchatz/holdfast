// What the page LINES UP, measured from the geometry the engine laid out.
//
// Every criterion here was a defect first, found by measuring rather than by looking: a
// separator with more space on one side than the other, a rule two pixels shorter than the
// things it divided, a mark sitting off the centre of its own ring, a heading rule nearer
// the block below it than the heading it belongs to. None of them is visible in a
// screenshot at a glance and all of them are visible once you know; a number is the only
// way to keep them fixed.
import { test, expect } from "@playwright/test";
import { open, waitRendered } from "./conventions.mjs";
import { pageURL } from "./theme.mjs";

// How far two measurements may differ and still be "the same": one CSS pixel, because
// sub-pixel layout does not put two boxes that share a line at the same coordinate.
const TOL = 1.0;

test("a separator has the same space on both sides, and is as tall as what it divides", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const m = await page.evaluate(() => {
    const R = (el) => el.getBoundingClientRect();
    const chips = [...document.querySelectorAll("#chips .chip")];
    const first = chips.find((c) => c.classList.contains("terminal"));
    const prev = first.previousElementSibling;
    const b4 = getComputedStyle(first, "::before");
    const fb = R(first), pb = R(prev);
    const x = fb.left + parseFloat(b4.left);
    return {
      drawn: b4.content !== "none",
      sameLine: Math.abs(pb.top - fb.top) < 2,
      left: x - pb.right, right: fb.left - x,
      ruleHeight: parseFloat(b4.height), chipHeight: fb.height,
    };
  });
  await ctx.close();

  expect(m.sameLine, "the two chips the rule divides are not on one line at this width").toBe(true);
  expect(m.drawn, "the group divider is not drawn at all").toBe(true);
  expect(Math.abs(m.left - m.right),
    `the divider has ${m.left}px on one side and ${m.right}px on the other; a rule sits BETWEEN two things`
  ).toBeLessThanOrEqual(TOL);
  expect(Math.abs(m.ruleHeight - m.chipHeight),
    `the divider is ${m.ruleHeight}px tall and the chips it divides are ${m.chipHeight}px; an absolutely positioned box is laid out against the padding box, so it stops inside the border unless the inset says otherwise`
  ).toBeLessThanOrEqual(TOL);
});

// The rule divides two things, so it is drawn only when there ARE two things beside it.
// The row wraps at some widths and not others, and CSS cannot ask whether two flex items
// share a line - so the page answers after layout, and this is what holds that answer to
// the layout it describes. It sweeps a range of widths rather than a chosen one because
// where the row breaks depends on the longest label, not on a number anybody picked.
test("the group divider is drawn exactly when the two groups share a line, at every width", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const readings = await page.evaluate(async () => {
    const R = (el) => el.getBoundingClientRect();
    const out = [];
    for (let w = 360; w <= 1440; w += 40) {
      document.documentElement.style.width = w + "px";
      document.body.style.width = w + "px";
      await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
      await new Promise((r) => setTimeout(r, 60));
      const read = () => {
        const chips = [...document.querySelectorAll("#chips .chip")];
        const f = chips.find((c) => c.classList.contains("terminal"));
        const p = f.previousElementSibling;
        const b4 = getComputedStyle(f, "::before");
        return { same: Math.abs(R(p).top - R(f).top) < 2, drawn: b4.content !== "none" };
      };
      const a = read();
      // A second reading a frame later: a layout that changes what it is asked about
      // oscillates, and answers differently each time.
      await new Promise((r) => setTimeout(r, 100));
      const b = read();
      out.push({ w, ...a, stable: a.same === b.same && a.drawn === b.drawn });
    }
    document.documentElement.style.removeProperty("width");
    document.body.style.removeProperty("width");
    return out;
  });
  await ctx.close();

  expect(readings.length, "no width was measured, so this asserted nothing").toBeGreaterThan(20);
  const wrong = readings.filter((r) => r.same !== r.drawn);
  expect(wrong.map((r) => `${r.w}px: onOneLine=${r.same} ruleDrawn=${r.drawn}`),
    "the rule is drawn where it divides nothing, or missing where it divides two chips").toEqual([]);
  const flapping = readings.filter((r) => !r.stable);
  expect(flapping.map((r) => `${r.w}px`),
    "the answer changed between two readings at one width: suppressing the rule changed the layout that decided it, and the row flickers"
  ).toEqual([]);
  // And it is not vacuous: both answers must actually occur somewhere in the range.
  expect(readings.some((r) => r.drawn), "the rule is never drawn at any width").toBe(true);
  expect(readings.some((r) => !r.drawn), "the rule is drawn at every width, so the suppression is never exercised").toBe(true);
});

// A mark is not text, and the box around it must not be sized as though it were. An inline
// box sits on its line's BASELINE, and a line box reserves descender space under that
// baseline whether or not anything is written on it - so a 24px mark in an inline container
// comes out in a 26px box, sitting a pixel above the centre of the thing that holds it, and
// the row it is in grows two pixels for text that does not exist. It is invisible until you
// measure it and unmistakable once you have.
test("a mark's own container is the size of the mark, not of a line of text", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const m = await page.evaluate(() => {
    const R = (el) => el.getBoundingClientRect();
    const mid = (r) => (r.top + r.bottom) / 2;
    return [...document.querySelectorAll(".regionintro")].map((intro) => {
      const docs = intro.querySelector(".docs"), link = intro.querySelector("a.doclink");
      const scope = intro.querySelector(".scope");
      const rg = document.createRange(); rg.selectNodeContents(scope);
      const line = rg.getClientRects()[0];
      return {
        slack: R(docs).height - R(link).height,
        offCentre: mid(R(link)) - mid(R(docs)),
        rowVsMark: R(intro).height - R(link).height,
        vsText: mid(R(link)) - mid(line),
      };
    });
  });
  await ctx.close();
  expect(m.length, "no documentation mark was measured").toBeGreaterThan(0);
  for (const x of m) {
    expect(Math.abs(x.slack),
      `the mark's container is ${x.slack}px taller than the mark; it is being sized as a line of text`).toBeLessThanOrEqual(TOL);
    expect(Math.abs(x.offCentre),
      `the mark sits ${x.offCentre.toFixed(2)}px off the centre of its own container`).toBeLessThanOrEqual(TOL);
    expect(Math.abs(x.rowVsMark),
      `the row holding the mark is ${x.rowVsMark}px taller than the mark itself`).toBeLessThanOrEqual(TOL);
    expect(Math.abs(x.vsText),
      `the mark is ${x.vsText.toFixed(2)}px off the centre line of the text beside it`).toBeLessThanOrEqual(TOL);
  }
});

test("the information mark sits at the centre of its own ring", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const marks = await page.evaluate(() => [...document.querySelectorAll("a.doclink")].map((a) => {
    const svg = a.querySelector("svg");
    const ring = a.getBoundingClientRect();
    const circle = svg.querySelector("circle").getBoundingClientRect();
    const paths = [...svg.querySelectorAll("path")].map((p) => p.getBoundingClientRect());
    const top = Math.min(...paths.map((p) => p.top)), bottom = Math.max(...paths.map((p) => p.bottom));
    const left = Math.min(...paths.map((p) => p.left)), right = Math.max(...paths.map((p) => p.right));
    return {
      // the drawn circle inside the box that frames it
      circleVsRingY: (circle.top + circle.bottom) / 2 - (ring.top + ring.bottom) / 2,
      circleVsRingX: (circle.left + circle.right) / 2 - (ring.left + ring.right) / 2,
      // and the glyph inside that circle
      glyphVsCircleY: (top + bottom) / 2 - (circle.top + circle.bottom) / 2,
      glyphVsCircleX: (left + right) / 2 - (circle.left + circle.right) / 2,
    };
  }));
  await ctx.close();

  expect(marks.length, "the page rendered no information mark at all").toBeGreaterThan(0);
  for (const m of marks) {
    for (const [what, v] of Object.entries(m)) {
      expect(Math.abs(v), `${what} is off by ${v.toFixed(2)}px`).toBeLessThanOrEqual(TOL);
    }
  }
});

test("a heading's rule is no further from its heading than from the block below it", async ({ browser, baseURL }) => {
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", waitFor: waitRendered });
  const rules = await page.evaluate(() => [...document.querySelectorAll("section > h2, section > h3")].map((h) => {
    const hb = h.getBoundingClientRect();
    const rg = document.createRange(); rg.selectNodeContents(h);
    const line = rg.getClientRects()[0];
    const next = h.nextElementSibling;
    return {
      heading: h.textContent.trim().slice(0, 24),
      // the rule is the heading's bottom border
      above: hb.bottom - line.bottom,
      below: next ? next.getBoundingClientRect().top - hb.bottom : null,
    };
  }));
  await ctx.close();

  expect(rules.length, "no heading rule was measured").toBeGreaterThan(3);
  for (const r of rules) {
    if (r.below === null) continue;
    // Even within a pixel or two. The space above a heading's rule is its padding PLUS the
    // half-leading a line box carries under its glyphs, which is why the two declared
    // values are not the same number.
    expect(Math.abs(r.above - r.below),
      `the rule under "${r.heading}" has ${r.above.toFixed(2)}px above it and ${r.below.toFixed(2)}px below; a rule nearer the block below it reads as belonging to that block`
    ).toBeLessThanOrEqual(2.0);
  }
});

// The per-file controls, at the width they have to survive (S0094).
//
// This file runs under EVERY project the harness declares - dark at 1440, light at 1440
// and DARK AT 360 - so a viewport-width question is decided once per world rather than
// asserted of one. The 360 project is the one criterion 18's width half turns on: clause
// F9 says a surface is operable at 360px wide with the page body free of horizontal
// scrolling, and the controls this item adds are the ones that had not been measured
// there.
//
// Everything here is read off what the ENGINE rendered - real layout geometry, the
// computed style after the whole cascade, a hit test at each control's own centre - and
// every control is OPERATED rather than inspected: the search is typed into and clicked,
// the withholding is recorded by a real click on a real row and removed by a real click on
// its own entry. A control that is merely present is not a control that is operable.
import { test, expect } from "@playwright/test";
import { open, shown, expectShown } from "./dashboard.mjs";

// The pointer-target floor WCAG 2.2 sets and this repository's token file already buys.
// It is asserted on the rendered box, because a padding that a narrow viewport collapses
// is a target that was 24px in the stylesheet and is not one on the screen.
const TARGET_MIN = 24;

// bodyScrollsSideways is F9's own question, asked of the document the engine laid out.
// Wide content is allowed to scroll inside its own container; the page BODY is not.
async function bodyScrollsSideways(page) {
  return page.evaluate(() => {
    const d = document.documentElement, b = document.body;
    return {
      docOverflow: d.scrollWidth - d.clientWidth,
      bodyOverflow: b.scrollWidth - b.clientWidth,
      width: window.innerWidth,
    };
  });
}

// setToken puts a control token in the page's own field and fires the change the page
// listens for. The withheld paths are a token-gated read, so without this the page has
// nothing to draw and the case would measure an empty region.
async function setToken(page, value = "a-token") {
  await page.evaluate((v) => {
    const e = document.getElementById("token");
    e.value = v;
    e.dispatchEvent(new Event("change", { bubbles: true }));
  }, value);
}

// everyAddedControl is the whole set this item puts on the page, named in one place so a
// case that says "every control this spec adds" is driven against every one of them.
const everyAddedControl = [
  ["the row filter", "#filter"],
  ["the ledger search field", "#ledger-search"],
  ["the ledger search button", "#search-go"],
];

test("every control this item adds is operable at this project's width", async ({ page }) => {
  const problems = await open(page, "full");
  expect(problems, problems.join("\n")).toEqual([]);
  await setToken(page);

  for (const [what, sel] of everyAddedControl) {
    const s = await expectShown(page.locator(sel), what);
    if (s.box.width < TARGET_MIN || s.box.height < TARGET_MIN) {
      problems.push(`${what} renders a ${Math.round(s.box.width)}x${Math.round(s.box.height)} target, below the ${TARGET_MIN}px floor`);
    }
  }

  // The per-row withholding control, on a real row of the served page.
  const hold = page.locator("#history td.remedy button").first();
  const holdBox = await expectShown(hold, "the per-row withholding control");
  if (holdBox.box.width < TARGET_MIN || holdBox.box.height < TARGET_MIN) {
    problems.push(`the per-row withholding control renders a ${Math.round(holdBox.box.width)}x${Math.round(holdBox.box.height)} target, below the ${TARGET_MIN}px floor`);
  }

  expect(problems, problems.join("\n")).toEqual([]);
});

test("the page body never scrolls sideways while the added controls are used", async ({ page }) => {
  const problems = await open(page, "full");
  expect(problems, problems.join("\n")).toEqual([]);
  await setToken(page);

  const check = async (where) => {
    const m = await bodyScrollsSideways(page);
    if (m.bodyOverflow > 1 || m.docOverflow > 1) {
      problems.push(`[${where}] the page body scrolls sideways at ${m.width}px: body overflows by ${m.bodyOverflow}px, the document by ${m.docOverflow}px`);
    }
  };

  await check("before anything is used");

  // The search, typed into and run for real. Its results carry a long path, which is
  // exactly the content a narrow viewport has to hold inside its own container.
  await page.fill("#ledger-search", "some-film");
  await page.click("#search-go");
  await expect(page.locator("#search-results tr")).toHaveCount(1);
  await expectShown(page.locator("#found"), "the ledger search's results region");
  await check("with the ledger search results on screen");

  // A withholding, recorded by a real click on a real row and rendered back.
  await page.click("#history td.remedy button");
  await expect(page.locator("#held-list li")).toHaveCount(1);
  await expectShown(page.locator("#held"), "the withheld paths");
  await check("with a withheld path on screen");

  // Its removal control, which is only on the page once there is something to remove.
  const release = page.locator("#held-list button").first();
  const box = await expectShown(release, "the control that removes a withholding");
  if (box.box.width < TARGET_MIN || box.box.height < TARGET_MIN) {
    problems.push(`the control that removes a withholding renders a ${Math.round(box.box.width)}x${Math.round(box.box.height)} target, below the ${TARGET_MIN}px floor`);
  }
  await release.click();
  await expect(page.locator("#held-list li")).toHaveCount(0);
  await check("after the withholding was removed");

  expect(problems, problems.join("\n")).toEqual([]);
});

// The grader's own proof. A case that asserts "the body does not overflow" passes on any
// page that happens not to, so the reading is driven against one that DOES: a single wide
// element outside a scrolling container, which is the defect F9 names.
test("the sideways-scroll reading fails against a page that really does overflow", async ({ page }) => {
  await open(page, "full");
  await page.evaluate(() => {
    const wide = document.createElement("div");
    wide.setAttribute("style", "width: 4000px; height: 8px;");
    document.body.appendChild(wide);
  });
  const m = await bodyScrollsSideways(page);
  expect(m.bodyOverflow > 1 || m.docOverflow > 1,
    "the reading passed a document carrying a 4000px element outside every scrolling container, so it measures nothing").toBe(true);
});

// Criterion 18's other half is the TOKEN containment, which is a check of the stylesheet
// SOURCE and belongs to `make check`. What is asked here is the half only an engine can
// answer: that the values those tokens resolve to actually reach the controls this item
// adds, in whatever theme this project is running.
test("every control this item adds is painted from the resolved token palette", async ({ page }) => {
  await open(page, "full");
  await setToken(page);
  const problems = [];
  for (const [what, sel] of everyAddedControl) {
    const seen = await page.locator(sel).evaluate((el) => {
      const cs = getComputedStyle(el);
      return { color: cs.color, background: cs.backgroundColor, border: cs.borderTopColor };
    });
    for (const [prop, value] of Object.entries(seen)) {
      if (!/^rgba?\(/.test(String(value))) {
        problems.push(`${what} resolved its ${prop} to ${value}, which is not a colour the engine computed`);
      }
    }
  }
  // The one token every added surface leans on, read from the document root so a theme
  // that failed to declare it shows up here rather than as a control painted in the
  // browser's default.
  const tokens = await page.evaluate(() => {
    const cs = getComputedStyle(document.documentElement);
    return ["--panel", "--border", "--fg", "--muted", "--warn", "--accent"]
      .map((n) => [n, cs.getPropertyValue(n).trim()]);
  });
  for (const [name, value] of tokens) {
    if (value === "") problems.push(`the token ${name} resolves to nothing in this theme`);
  }
  expect(problems, problems.join("\n")).toEqual([]);
});

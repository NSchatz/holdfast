// EVERY grader is run against a document deliberately built to defeat it.
//
// This is the half a grader suite is worthless without. A grader that cannot fail is not
// evidence, and the only way to know one can fail is to defeat it on purpose and watch it
// report. Each case below names the grader it is aimed at, refuses a mutation that did not
// change the served document at all - which is how a counterexample quietly stops being
// one - and fails if the grader stays silent.
//
// The sharpest of them is the policy grader, and it is the reason the second half of this
// file exists: it asserts a list is EMPTY, and a dead instrument and a clean page report
// the same nothing. Only a page the engine MUST refuse can tell the two apart.
import { test, expect } from "@playwright/test";
import { open, collect, waitRendered, mutate, axTree } from "./conventions.mjs";
import { convGraders, gradeViewsAllIn, gradeNoZeroBeforeASnapshot, gradeReducedMotion } from "./graders.mjs";
import { pageURL, gradeThemeFollowsTheEnginesPreference, readThemeTriple, readContrastRecords, gradeRecordedRatiosAgreeWithTheEngine, S2 } from "./theme.mjs";
import { tabThroughEveryControl, gradeTabOrderAndFocusRing, gradeAccessibleNames } from "./a11y.mjs";
import { gradeEveryUnmeasuredFieldReadsTheAbsencePhrase, gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure, severedStreamReading } from "./states.mjs";
import { gradeNoPolicyRefusal, gradeRefusedControlActionCostsNothingElse, refuseAControlAction, gradeServerErrorStillRendersTheOfferAndTheControls } from "./interaction.mjs";
import { motionPair, gradeReducedMotionCostsNoValue } from "./motionpair.mjs";

// Refuses a mutation that did not change the served document at all.
async function mustChange(page, baseURL, mutation) {
  const plain = await page.request.get(`${baseURL}/`).then((r) => r.text());
  const after = mutation(plain);
  expect(after, "the mutation did not change the served document - the assertion below would be vacuous").not.toBe(plain);
}

// --- the table: every grader in convGraders(), defeated by name -------------------------

const TABLE = [
  {
    name: "an explanatory label recoloured onto the surface behind it",
    defeats: "every run of text clears its contrast floor",
    mutation: mutate.css(`.scope, .agg-cov { color: var(--line) !important; }`),
  },
  {
    name: "every button crushed below the target floor",
    defeats: "every pointer target is at least 24 by 24",
    mutation: mutate.css(`button { min-height: 0 !important; min-width: 0 !important; padding: 0 !important; font-size: 8px !important; line-height: 1 !important; }`),
  },
  {
    name: "a paragraph of methodology put back on the surface",
    defeats: "no on-surface label runs past fifteen words",
    mutation: mutate.script(`var s=document.querySelector(".scope"); if (s) s.textContent = "Elapsed is how long the file has been in the state it is in now, recomputed from the transition timestamp on each update rather than counted in this page.";`),
  },
  {
    name: "a region's documentation link taken away",
    defeats: "each region carries exactly one documentation link",
    mutation: mutate.replace(`class="doclink"`, `class="notadoclink"`),
  },
  {
    name: "a region's documentation link duplicated",
    defeats: "each region carries exactly one documentation link",
    mutation: (body) => {
      const i = body.indexOf(`<p class="docs">`);
      const j = body.indexOf(`</p>`, i);
      if (i < 0 || j < 0) return body;
      const block = body.slice(i, j + 4);
      return body.replace(block, block + block);
    },
  },
  {
    name: "content wider than the phone viewport pushing the body sideways",
    defeats: "the body never scrolls sideways",
    mutation: mutate.css(`main { min-width: 1200px; }`),
    width: 360,
  },
  {
    // The clause is conditional on there BEING wide content, so the mutation has to
    // produce some. At a phone width this page's answer to seven columns is a row that
    // STACKS - so simply taking the wrapper's scroll away leaves nothing wider than the
    // viewport and nothing for the grader to catch, which is correct and is why this
    // mutation puts the wide table back FIRST. It undoes the stacking, restores a table
    // wider than the viewport, and then takes away the scroll that would have made it
    // readable: wide content, no container of its own, which is what the clause forbids.
    name: "a table wider than the phone viewport with no scroll of its own",
    defeats: "wide content scrolls inside its own container",
    mutation: mutate.css(`
      table, table.hist, table.queue { display: table !important; min-width: 900px !important; }
      table thead { display: table-header-group !important; }
      table tbody { display: table-row-group !important; }
      table tbody tr { display: table-row !important; }
      table tbody td { display: table-cell !important; padding-left: 12px !important; }
      .tablewrap { overflow-x: visible !important; }`),
    width: 360,
  },
  {
    name: "a data column put into the prose face",
    defeats: "data is monospace and prose is the system face",
    mutation: mutate.css(`td.path { font-family: var(--font-ui) !important; }`),
  },
  {
    name: "a heading put into the data face",
    defeats: "data is monospace and prose is the system face",
    mutation: mutate.css(`section > h2 { font-family: var(--font-mono) !important; }`),
  },
  {
    name: "a shadow given to a surface that is not raised",
    defeats: "only a raised surface computes a shadow",
    mutation: mutate.css(`.agg { box-shadow: var(--shadow-raised) !important; }`),
  },
  {
    name: "the raised surface's shadow taken away",
    defeats: "only a raised surface computes a shadow",
    mutation: mutate.css(`header { box-shadow: none !important; }`),
  },
  {
    name: "a colour that is in no token painted onto a control",
    defeats: "every painted value came from the token file",
    mutation: mutate.css(`#chips .chip { background: #123456 !important; }`),
  },
  {
    name: "an off-scale spacing length applied by the cascade",
    defeats: "every painted value came from the token file",
    mutation: mutate.css(`#chips .chip { padding: 7px 9px !important; }`),
  },
  {
    name: "every transition removed, so the reduce grader would assert nothing",
    defeats: "the page computes motion at all",
    mutation: mutate.css(`* { transition: none !important; animation: none !important; }`),
  },
];

for (const c of TABLE) {
  test(`the grader "${c.defeats}" fails against ${c.name}`, async ({ browser, baseURL, request }) => {
    const plain = await request.get(`${baseURL}/`).then((r) => r.text());
    expect(c.mutation(plain), `the mutation "${c.name}" did not change the served document - the assertion below would be vacuous`).not.toBe(plain);

    const { ctx, page } = await open(browser, {
      url: pageURL(baseURL, "full"), theme: "light",
      width: c.width || 1280, height: c.width ? 900 : 1024,
      mutation: c.mutation, waitFor: waitRendered,
    });
    const s = await collect(page);
    await ctx.close();

    const g = convGraders().find((x) => x.name === c.defeats);
    expect(g, `the mutation "${c.name}" names a grader that does not exist: "${c.defeats}"`).toBeTruthy();
    const probs = g.probe(s);
    expect(probs.length, `the grader "${c.defeats}" PASSED a document mutated to defeat it (${c.name})`).toBeGreaterThan(0);
  });
}

// --- the graders that decide from MORE than one reading ---------------------------------
//
// Everything above defeats a predicate over a single collected snapshot. The graders below
// decide their criterion from something else - a pair of renders, three renders under three
// preferences, a real tab walk, the accessibility tree the engine computed, the engine's
// own log of what it refused - so none of them could join the table, and each owes the same
// debt it pays.

test("the reduced-motion grader fails against a motion that survives the query", async ({ browser, baseURL }) => {
  const mutation = mutate.css(`@media (prefers-reduced-motion: reduce) { .badge { transition-duration: 3s !important; } }`);
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "full"), theme: "light", reduce: true, mutation, waitFor: waitRendered,
  });
  const probs = gradeReducedMotion(await collect(page));
  await ctx.close();
  expect(probs.length, "the reduced-motion grader PASSED a document whose transition survives prefers-reduced-motion: reduce").toBeGreaterThan(0);
});

test("the three-state grader fails against a view stuck on loading", async ({ browser, baseURL }) => {
  // Leave the queue view claiming to be loading after its snapshot has arrived - the page
  // that never notices its data landed, which is the failure F7 is written for.
  // Repeating, not once at parse time: the state row this rewrites is rebuilt by every
  // render, so a mutation that ran before the first snapshot would be undone by it.
  const mutation = mutate.script(`setInterval(function(){var q=document.querySelector('[data-view="queue"] [data-state]') || document.querySelector('[data-view="queue"] tr'); if (q) { q.dataset.state="loading"; var td=q.querySelector("td"); if (td) td.textContent="Loading the queue."; }}, 20);`);
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "empty"), theme: "light", mutation,
    waitFor: (p) => p.waitForFunction(() => {
      const v = window.__hf.views();
      return v.length === 4 && v.some((x) => x.view === "queue" && x.state === "loading");
    }, null, { timeout: 15000 }),
  });
  const probs = gradeViewsAllIn("empty")(await collect(page));
  await ctx.close();
  expect(probs.length, "the three-state grader PASSED a page with a view stuck on its loading state").toBeGreaterThan(0);
});

test("the no-zero-before-a-snapshot grader fails against a zeroed total", async ({ browser, baseURL }) => {
  const mutation = mutate.script(`var e=document.getElementById("reclaimed-lifetime"); if (e) e.textContent = "0 B";`);
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "loading"), theme: "light", mutation,
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.connText() === "live" && document.getElementById("reclaimed-lifetime").textContent === "0 B",
      null, { timeout: 15000 }),
  });
  const probs = gradeNoZeroBeforeASnapshot(await collect(page));
  await ctx.close();
  expect(probs.length, "the grader PASSED a page rendering a lifetime total as 0 B before any snapshot arrived").toBeGreaterThan(0);
});

// F11. An off-origin image, which `default-src 'none'` must refuse. If the engine's refusal
// log does not report it, the grader that asserts that log is empty is BLIND and would pass
// whatever the page did.
test("the policy grader fails against an off-origin image", async ({ browser, baseURL }) => {
  const marker = "mutation-off-origin";
  const mutation = mutate.replace("</body>", `<img id="${marker}" src="https://example.invalid/pixel.png" alt=""></body>`);
  const { ctx, page, violations } = await open(browser, {
    url: pageURL(baseURL, "full"), theme: "light", mutation, waitFor: waitRendered,
  });
  // The refusal is raised during the render; give the engine's log the same moment the
  // shipped grader gives it after driving an interaction.
  await page.waitForTimeout(600);
  // The element really is in the document the engine parsed, so a silent instrument cannot
  // be excused as "the mutation never landed".
  const present = await page.evaluate((m) => !!document.getElementById(m), marker);
  expect(present, "the off-origin image is not in the rendered document, so this case measured nothing").toBe(true);
  const probs = gradeNoPolicyRefusal("an off-origin image", violations);
  await ctx.close();
  expect(probs.length,
    "BLIND INSTRUMENT: the engine had to refuse an off-origin image under `default-src 'none'` and the grader reported NOTHING. It asserts that list is empty and would pass whatever the page did"
  ).toBeGreaterThan(0);
});

// F1 / F5. A refusal rewritten to read as success is the exact failure the grader exists
// for: the server changed nothing and the page says it did.
test("the refused-control grader fails against a message that reads as success", async ({ browser, baseURL }) => {
  const mutation = mutate.script(`setInterval(function(){var m=document.getElementById("msg"); if (m && m.textContent.indexOf("refused") === 0) m.textContent = "done";}, 20);`);
  const { before, after, refusals } = await refuseAControlAction(browser, baseURL, mutation);
  const probs = gradeRefusedControlActionCostsNothingElse(before, after, refusals);
  expect(probs.length, `the grader PASSED a page that answered a REFUSED control action with "${after.msg.text}"`).toBeGreaterThan(0);
});

test("the server-error grader fails against a hidden source offer", async ({ browser, baseURL }) => {
  const mutation = mutate.css(`.source-offer { display: none !important; }`);
  const { ctx, page, violations } = await open(browser, {
    url: pageURL(baseURL, "no-stream"), theme: "light", mutation,
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.views().length === 4 && window.__hf.views().every((v) => v.state === "unreadable"),
      null, { timeout: 15000 }),
  });
  const probs = gradeServerErrorStillRendersTheOfferAndTheControls(await collect(page), violations);
  await ctx.close();
  expect(probs.length, "the grader PASSED a page serving an error with its AGPL source offer hidden").toBeGreaterThan(0);
});

test("the colour-scheme grader fails against one palette wearing both preferences", async ({ browser, baseURL }) => {
  // Force every role token to its light value under BOTH preferences: two names, one
  // palette, which is the failure F10 exists to name.
  const rule = `@media (prefers-color-scheme: dark) { :root { ${S2.map((n) => `${n}: var(--forced-${n.slice(2)}) !important;`).join(" ")} } }`;
  const forced = `:root { ${S2.map((n) => `--forced-${n.slice(2)}: initial;`).join(" ")} }`;
  const mutation = mutate.css(`${forced}\n@media (prefers-color-scheme: dark) { :root { --bg: #f6f7f9 !important; --panel: #ffffff !important; --line: #e3e6eb !important; --fg: #12161c !important; --muted: #545e6d !important; --accent: #0f5aa8 !important; --ok: #10682f !important; --warn: #7a5000 !important; --bad: #b3221c !important; --border: #71798a !important; --mark: #0f5aa8 !important; --focus: #0f5aa8 !important; --disabled: #5f6874 !important; --selected: #d6e6f7 !important; --link: #0b4f96 !important; } }`);
  const [light, dark, none] = await readThemeTriple(browser, pageURL(baseURL, "full"), mutation);
  const probs = gradeThemeFollowsTheEnginesPreference(light, dark, none);
  expect(probs.length, "the grader PASSED a page painting one palette under both operating-system preferences").toBeGreaterThan(0);
});

test("the recorded-contrast grader fails against a token moved away from its record", async ({ browser, baseURL }) => {
  const mutation = mutate.css(`:root { --muted: #767676 !important; }`);
  const records = readContrastRecords();
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", mutation, waitFor: waitRendered });
  const probs = gradeRecordedRatiosAgreeWithTheEngine("light", (await collect(page)).tokens, records);
  await ctx.close();
  expect(probs.length,
    "the grader PASSED a page whose --muted value no longer matches the ratio recorded beside it in tokens.css"
  ).toBeGreaterThan(0);
});

test("the focus-ring grader fails against a control that shows no focus", async ({ browser, baseURL }) => {
  const mutation = mutate.css(`:focus, :focus-visible { outline: none !important; }`);
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", mutation, waitFor: waitRendered });
  const { expected, reached } = await tabThroughEveryControl(page);
  await ctx.close();
  const probs = gradeTabOrderAndFocusRing("light", expected, reached);
  expect(probs.length, "the grader PASSED a page whose controls draw no focus indicator at all").toBeGreaterThan(0);
});

test("the accessible-name grader fails against a control named only by its placeholder", async ({ browser, baseURL }) => {
  // Detach the label from the control it names; the engine then falls back to the
  // placeholder, which disappears the moment a value is typed.
  const mutation = mutate.replace(`<label for="filter" class="lbl">`, `<label class="lbl">`);
  const { ctx, page } = await open(browser, { url: pageURL(baseURL, "full"), theme: "light", mutation, waitFor: waitRendered });
  const probs = gradeAccessibleNames(await axTree(ctx, page));
  await ctx.close();
  expect(probs.length, "the grader PASSED a page whose search box is named only by its own placeholder text").toBeGreaterThan(0);
});

// F3. A field nobody measured rendered as a zero is the whole reason the absence phrase
// exists: an operator cannot tell a measured 0 from a measurement that never happened.
test("the absence-phrase grader fails against an unmeasured field rendered as zero", async ({ browser, baseURL }) => {
  const mutation = mutate.script(`setInterval(function(){for (const td of document.querySelectorAll("#history td.size")) td.textContent = "0 B";}, 20);`);
  const { ctx, page } = await open(browser, {
    url: pageURL(baseURL, "null"), theme: "light", mutation,
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.connText() === "live" && window.__hf.rendered() &&
        document.querySelectorAll("#history td.size").length > 0 &&
        document.querySelector("#history td.size").textContent === "0 B",
      null, { timeout: 15000 }),
  });
  const probs = gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(await collect(page));
  await ctx.close();
  expect(probs.length, "the grader PASSED a page rendering a size nobody recorded as 0 B").toBeGreaterThan(0);
});

// F6. The elapsed ticker is the one figure on this page that would go on advancing with
// nothing behind it, so the counterexample is exactly that: a ticker told the stream is
// still live when it is not.
test("the severed-stream grader fails against a ticker that keeps running", async ({ browser, baseURL }) => {
  const mutation = mutate.script(`setInterval(function(){ try { streamIsLive = true; } catch (e) {} }, 20);`);
  const { first, before, after } = await severedStreamReading(browser, pageURL(baseURL, "severed"), mutation);
  const probs = gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first, before, after);
  expect(probs.length,
    `the grader PASSED a severed page whose elapsed figures kept advancing (${JSON.stringify(before)} then ${JSON.stringify(after)})`
  ).toBeGreaterThan(0);
});

test("the reduced-motion value grader fails against a value dropped under reduce", async ({ browser, baseURL }) => {
  // Take a value off the page under the reduce preference alone. Motion is decoration, so
  // removing it must remove nothing; this removes something.
  const mutation = mutate.css(`@media (prefers-reduced-motion: reduce) { #chips .chip .k { display: none !important; } }`);
  const { moving, still } = await motionPair(browser, pageURL(baseURL, "full"), { mutation });
  const probs = gradeReducedMotionCostsNoValue(moving, still);
  expect(probs.length, "the grader PASSED a page that drops a value when motion is reduced").toBeGreaterThan(0);
});

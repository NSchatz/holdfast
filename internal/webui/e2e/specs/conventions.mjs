// The DRIVER for the convention graders: it puts the served document in front of a real
// engine under the world a case asks for, injects the probe, and hands back one snapshot.
//
// The three things this layer exists for are the three things no expression evaluated
// inside the page can do for itself:
//
//   the THEME is emulated at the ENGINE (`colorScheme`), never by a class, an attribute
//   or a stylesheet injected into the page. A grader that injects the theme is grading
//   its own fixture;
//   the ACCESSIBILITY TREE is read from the engine over the DevTools protocol. An
//   accessible name is the engine's own answer over labels, ARIA, native semantics and
//   content - not a property of markup anything can reconstruct by inspection;
//   KEY PRESSES are dispatched as real input. A KeyboardEvent constructed inside the page
//   is untrusted and moves focus nowhere, so tab order is not observable from the document.
//
// The runner speaks all three natively, which is the whole reason this moved onto it.
import { chromium } from "@playwright/test";
import { probeSource } from "./probe.mjs";
import { FONT_EXPECTATIONS } from "./graders.mjs";

// A MUTATION rewrites the served document on its way to the engine. The response still
// comes from the real handler - same bytes, same Content-Security-Policy - and only the
// body is rewritten, so a mutated case is still measuring the page this repository ships
// with exactly one thing changed. That is what makes a counterexample a counterexample.
export const mutate = {
  css: (rules) => (body) => body.replace("</style>", rules + "\n</style>"),
  script: (js) => (body) => body.replace("</body>", "<script>" + js + "</script></body>"),
  replace: (from, to) => (body) => body.replace(from, to),
};

// open loads the document under one world and returns the page plus everything a case may
// need to read back. `mutation` is applied to the document body; `route` interception
// keeps the handler's own headers, so the policy under measurement is the real one.
export async function open(browser, { url, theme = "light", reduce = false, width = 1280, height = 1024, mutation = null, waitFor = null } = {}) {
  // "" means NO PREFERENCE at the engine, which is a THIRD state and not a synonym for
  // light: it is what a reader whose system has nominated neither gets, and clause F10
  // requires the page to render one named default under it rather than a mixture. `null`
  // is how the runner says "emulate nothing"; passing a value named "no-preference" would
  // leave whatever the engine last had in force, which is how this read the dark palette
  // under a preference nobody set.
  const ctx = await browser.newContext({
    colorScheme: theme === "" ? null : theme,
    reducedMotion: reduce ? "reduce" : null,
    viewport: { width, height },
  });
  const page = await ctx.newPage();

  const policy = [];
  const violations = [];
  page.on("console", (m) => {
    const t = m.text();
    if (/Content Security Policy|Trusted Type|Refused to/i.test(t)) violations.push(t);
  });
  page.on("response", (r) => { if (r.url() === url) policy.push(r.headers()["content-security-policy"]); });

  if (mutation) {
    await page.route(url, async (route) => {
      const res = await route.fetch();
      route.fulfill({ response: res, body: mutation(await res.text()) });
    });
  }
  // The probe is installed BEFORE the document runs, so nothing it measures can have been
  // disturbed by the act of measuring it later.
  await page.addInitScript(probeSource);
  await page.goto(url);
  if (waitFor) await waitFor(page);
  await waitStill(page);
  return { ctx, page, policy, violations };
}

// waitStill waits for the page to STOP MOVING. This page has transitions on its
// affordances - clause S9 requires that they exist at all, or the reduce grader would
// assert nothing - and a colour read while one is running is an INTERPOLATED value that
// belongs to no token and sits at no recorded contrast ratio. document.getAnimations() is
// the engine's own list of what is still in flight.
export async function waitStill(page) {
  await page.waitForFunction(
    () => document.getAnimations().every((a) => a.playState !== "running"),
    null, { timeout: 5000 },
  ).catch(() => page.waitForTimeout(400));
}

// waitRendered is the page's OWN record that a whole snapshot went through render(): the
// screen-reader summary is written at the end of every render and nowhere else. Waiting on
// that rather than on a timeout is what keeps these cases free of a race.
export const waitRendered = (page) => page.waitForFunction(() => {
  const sr = document.getElementById("sr-status");
  return !!sr && sr.textContent.trim() !== "";
}, null, { timeout: 15000 });

export const waitConnected = (page) => page.waitForFunction(() => {
  const c = document.getElementById("conn");
  return !!c && c.textContent.trim() === "live";
}, null, { timeout: 15000 });

export const waitDown = (page) => page.waitForFunction(() => {
  const c = document.getElementById("conn");
  return !!c && c.textContent.trim().startsWith("reconnecting");
}, null, { timeout: 15000 });

// collect takes one whole reading. Every field is something the ENGINE computed.
export async function collect(page) {
  return page.evaluate((spec) => ({
    tokens: __hf.tokens(),
    textRuns: __hf.textRuns(),
    targets: __hf.pointerTargets(),
    views: __hf.views(),
    labels: __hf.labels(),
    mainParagraphs: __hf.mainParagraphs(),
    doclinks: __hf.doclinks(),
    layout: __hf.layout(),
    fonts: __hf.fonts(spec),
    shadows: __hf.shadows(),
    motion: __hf.motion(),
    painted: __hf.painted(),
    figures: __hf.figures(),
    controls: __hf.controls(),
    stable: __hf.bodyTextWithoutTheLiveClock(),
    offer: __hf.sourceOffer(),
    msg: __hf.msg(),
    badges: __hf.badges(),
    bodyText: __hf.bodyText(),
    connText: __hf.connText(),
    elapsedValues: __hf.elapsedValues(),
    focusables: __hf.focusables(),
  }), FONT_EXPECTATIONS);
}

// axTree reads the accessibility tree the ENGINE computed, over the DevTools protocol.
// There is no other place an accessible name exists at all.
export async function axTree(ctx, page) {
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("Accessibility.enable");
  const { nodes } = await cdp.send("Accessibility.getFullAXTree");
  return nodes.map((n) => ({
    role: n.role && n.role.value,
    name: n.name && n.name.value,
    ignored: !!n.ignored,
    // Whether every source that CONTRIBUTED to this name was a placeholder. The engine
    // reports what it considered and what it discarded, and the distinction is the whole
    // point: a control named only by its placeholder loses its name the moment a value is
    // typed into it, which no reading of the name alone can tell you.
    onlyPlaceholder: onlyNamedByPlaceholder(n),
  }));
}

function onlyNamedByPlaceholder(n) {
  if (!n.name || !Array.isArray(n.name.sources)) return false;
  let contributing = 0, placeholder = 0;
  for (const src of n.name.sources) {
    if (src.superseded || src.invalid || !src.value || !String(src.value.value || "").trim()) continue;
    contributing++;
    if (src.attribute === "placeholder" || src.type === "placeholder") placeholder++;
  }
  return contributing > 0 && contributing === placeholder;
}

// tabOrder dispatches REAL key presses and reports what the engine focused, in order. A
// KeyboardEvent constructed inside the page is untrusted and moves focus nowhere.
export async function tabOrder(page, steps = 12) {
  const seen = [];
  for (let i = 0; i < steps; i++) {
    await page.keyboard.press("Tab");
    const at = await page.evaluate(() => {
      const a = document.activeElement;
      if (!a || a === document.body) return null;
      const cs = getComputedStyle(a);
      return {
        what: a.tagName.toLowerCase() + (a.id ? "#" + a.id : "") +
          (typeof a.className === "string" && a.className.trim() ? "." + a.className.trim().split(/\s+/).join(".") : ""),
        id: a.id || "",
        outlineWidth: parseFloat(cs.outlineWidth) || 0,
        outlineStyle: cs.outlineStyle,
        outlineColor: cs.outlineColor,
      };
    });
    if (!at) break;
    if (seen.length && seen[0].what === at.what) break; // wrapped round
    seen.push(at);
  }
  return seen;
}

export { chromium };

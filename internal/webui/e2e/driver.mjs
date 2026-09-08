// The engine driver the PROSE graders operate the browser through.
//
// Those graders are Go: the page-copy budget, the block ceiling, the redundancy rules, the
// documentation-link rule and the preservation checklist are several hundred lines of
// decision that read a snapshot and a committed word list, and they are not moving. What
// they cannot do from Go is OPERATE an engine, and the three things they need are the same
// three the spec files above need:
//
//   the operating system's colour-scheme preference, emulated at the ENGINE and never by a
//   class, an attribute or a stylesheet injected into the page;
//   the ACCESSIBILITY TREE the engine computed, WITH the sources of each name - a name
//   built from an element's own text is the copy the budget already counts, while one from
//   an attribute or a related element is copy a reader meets only through a screen reader;
//   a page EVALUATION that is not subject to the served document's own policy, because the
//   response is served under `script-src` with no `unsafe-eval` and the measuring script
//   has to run against it unchanged.
//
// This process is the ONE engine driver in the repository, and that is the point of it. A
// second transport of this repository's own, beside the runner already adopted for the
// same three questions, would be a second answer to "what did the engine say" every time
// the two disagreed - and the wire format, the read loop and the pending-call table needed
// to have that argument are all cost with no grader behind it.
//
// The protocol here is deliberately small: one JSON object per line in, one per line out,
// each carrying the id it answers. The runner does the launching, the context, the
// preference and the navigation; a DevTools session opened THROUGH it does the two calls
// that have no runner-level equivalent (`Runtime.evaluate` of a multi-statement script, and
// `Accessibility.getFullAXTree` with each name's sources).
import { chromium } from "@playwright/test";
import { createInterface } from "node:readline";

const executablePath = process.env.HOLDFAST_BROWSER;
if (!executablePath) {
  process.stderr.write("driver: HOLDFAST_BROWSER is unset; the caller names the engine, this process never searches for one\n");
  process.exit(2);
}

const browser = await chromium.launch({ executablePath });

// Everything the browser said about itself while a reading was taken. It is not asserted
// on; it is what a failing grader prints, so a wedged page arrives with the engine's own
// account of why beside it rather than as a bare timeout.
const said = [];
const note = (line) => { if (said.length < 4000) said.push(line); };

const pages = new Map();
let nextPage = 0;

async function newPage() {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.on("console", (m) => note(`[console:${m.type()}] ${m.text()}`));
  page.on("pageerror", (e) => note(`[pageerror] ${e.message}`));
  page.on("requestfailed", (r) => note(`[requestfailed] ${r.url()}: ${(r.failure() || {}).errorText}`));
  const cdp = await ctx.newCDPSession(page);
  await cdp.send("Accessibility.enable");
  const id = ++nextPage;
  pages.set(id, { ctx, page, cdp });
  return id;
}

function held(id) {
  const p = pages.get(id);
  if (!p) throw new Error(`no page ${id}`);
  return p;
}

const commands = {
  newPage: () => newPage(),

  viewport: async ({ page, width, height }) => {
    await held(page).page.setViewportSize({ width, height });
    return true;
  },

  // "" is the ABSENCE of an override, which is a third state and not a synonym for light:
  // it is what an engine reports when the platform has nominated neither. `null` is how the
  // runner says "emulate nothing"; a value named "no-preference" would leave whatever the
  // engine last had in force.
  emulate: async ({ page, scheme, reduce }) => {
    await held(page).page.emulateMedia({
      colorScheme: scheme ? scheme : null,
      reducedMotion: reduce ? "reduce" : null,
    });
    return true;
  },

  navigate: async ({ page, url }) => {
    await held(page).page.goto(url, { waitUntil: "load", timeout: 30000 });
    return true;
  },

  // Runtime.evaluate rather than the runner's own evaluate, for two reasons that are both
  // about the page rather than about taste: the measuring script is a SCRIPT, not an
  // expression, and it has to run outside the served document's Content-Security-Policy,
  // which forbids eval. This call is made from the DevTools side and is subject to neither.
  eval: async ({ page, expr }) => {
    const { result, exceptionDetails } = await held(page).cdp.send("Runtime.evaluate", {
      expression: expr, returnByValue: true, awaitPromise: true,
    });
    if (exceptionDetails) {
      throw new Error(`the expression threw inside the page: ${exceptionDetails.text}${
        exceptionDetails.exception && exceptionDetails.exception.description
          ? ": " + exceptionDetails.exception.description : ""}`);
    }
    return result.value === undefined ? null : result.value;
  },

  // The tree the ENGINE computed, unreduced: every node with its role, its name, its
  // description AND the sources each of those was built from. The runner's own
  // accessibility snapshot drops the sources, and the sources are the whole question.
  ax: async ({ page }) => held(page).cdp.send("Accessibility.getFullAXTree"),

  log: () => said.join("\n"),

  closePage: async ({ page }) => {
    const p = pages.get(page);
    if (p) { await p.ctx.close(); pages.delete(page); }
    return true;
  },
};

const out = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");

process.stdout.write(JSON.stringify({ ready: true }) + "\n");

const lines = createInterface({ input: process.stdin });
for await (const line of lines) {
  if (!line.trim()) continue;
  let req;
  try {
    req = JSON.parse(line);
  } catch (e) {
    out({ id: 0, ok: false, error: `driver: unreadable command: ${e.message}` });
    continue;
  }
  if (req.cmd === "quit") break;
  const fn = commands[req.cmd];
  if (!fn) {
    out({ id: req.id, ok: false, error: `driver: no command named ${req.cmd}` });
    continue;
  }
  try {
    out({ id: req.id, ok: true, value: await fn(req) });
  } catch (e) {
    out({ id: req.id, ok: false, error: String((e && e.message) || e) });
  }
}

await browser.close();

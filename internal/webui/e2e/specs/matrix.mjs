// The DRIVING half of interface-craft C7, and of C4's exclusion of controls.
//
// It decides nothing - every predicate is in graders.mjs - and it measures nothing of its
// own: every reading it hands back came out of probe.mjs. What lives here is the part that
// can only be done by OPERATING the engine, which is the same three things the rest of this
// project reaches the runner for and one more:
//
//   the ACCESSIBILITY TREE the engine computed, which is the only place a role exists. The
//   inventory is derived from it rather than from markup, so a control that looks like a
//   button to a selector and is announced as something else is caught rather than counted;
//   the FOCUS a real Tab moves. A KeyboardEvent constructed inside the page is untrusted and
//   moves focus nowhere, so neither the tab order nor :focus-visible is observable from the
//   document - which is precisely why a grader that entered the focus state by injecting a
//   class, an attribute or an inline style would be grading its own fixture;
//   real POINTER input, for :hover and :active. Both are engine state and neither has a DOM
//   property to set;
//   and the HARNESS'S OWN account of what it serves, so "every view" is the set the fixture
//   server actually has rather than a list that agreed with it the day it was written.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { waitStill } from "./conventions.mjs";
import config from "../playwright.config.mjs";

const RECORD = fileURLToPath(new URL("../../../../docs/state-matrix.md", import.meta.url));

// The roles the accessibility tree gives a control. It is the same set a11y.mjs holds for
// clause F1, kept here rather than imported so the two clauses can diverge without one
// silently changing the other's subject.
export const INTERACTIVE_AX_ROLES = new Set([
  "button", "textbox", "searchbox", "link", "combobox", "checkbox", "radio",
  "slider", "spinbutton", "switch", "menuitem", "tab", "option", "treeitem",
]);

// The committed record, read from the one file that holds it.
export function readStateMatrixRecord() {
  return readFileSync(RECORD, "utf8");
}

// --- the views the harness serves ------------------------------------------------------

// scenariosServed asks the FIXTURE SERVER which snapshot scenarios it loaded. A grader that
// has to measure every view cannot carry the list: a list agrees with the fixtures directory
// the day it is written and disagrees the day one is added, and the run would report a sweep
// it never took. An empty answer is handed back as an empty array, because refusing it is
// the grader's job and not this function's.
export async function scenariosServed(request, baseURL) {
  const res = await request.get(`${baseURL}/e2e/scenarios`);
  if (!res.ok()) throw new Error(`the fixture harness would not say which scenarios it serves (HTTP ${res.status()})`);
  const body = await res.json();
  return Array.isArray(body.scenarios) ? body.scenarios : [];
}

// worldsPresented is every project the runner presents, read out of the runner's own
// configuration rather than restated here. Each one is a colour-scheme preference and a
// viewport, and "" is the ABSENCE of a preference - a third state, and what a reader whose
// platform has nominated neither gets.
export function worldsPresented() {
  const out = [];
  for (const p of config.projects || []) {
    const use = p.use || {};
    const vp = use.viewport || { width: 1280, height: 720 };
    out.push({ project: p.name, theme: use.colorScheme === undefined ? "" : use.colorScheme,
      width: vp.width, height: vp.height });
  }
  return out;
}

// --- the tree, joined to the document --------------------------------------------------

// elementIndexByBackendId walks the node tree the DevTools protocol returns and numbers the
// ELEMENTS in it exactly as document.querySelectorAll("*") numbers them: tree order, element
// nodes only, and never into a <template>'s content, which is a separate fragment the page's
// own query does not reach either.
//
// The numbering is CROSS-CHECKED against the page's own list of tag names before it is used
// for anything. Two independent walks that disagree would mean an index computed on one side
// naming a different element on the other, and a state matrix keyed on that would be
// measuring whatever it happened to hit.
function elementIndexByBackendId(root) {
  const byBackend = new Map();
  const tags = [];
  const walk = (node) => {
    if (node.nodeType === 1) {
      byBackend.set(node.backendNodeId, tags.length);
      tags.push(String(node.nodeName || "").toLowerCase());
    }
    for (const child of node.children || []) walk(child);
  };
  for (const child of root.children || []) walk(child);
  return { byBackend, tags };
}

// axInteractiveElements is every element the ENGINE reports with an interactive role, each
// carried back with the index the page's own query gives it.
export async function axInteractiveElements(cdp, page) {
  await cdp.send("Accessibility.enable");
  const [{ nodes }, { root }, pageTags] = await Promise.all([
    cdp.send("Accessibility.getFullAXTree"),
    cdp.send("DOM.getDocument", { depth: -1, pierce: false }),
    page.evaluate(() => window.__hf.tagNames()),
  ]);
  const { byBackend, tags } = elementIndexByBackendId(root);
  if (tags.length !== pageTags.length || tags.some((t, i) => t !== pageTags[i])) {
    throw new Error(`the DevTools node tree and the page's own element list disagree (${tags.length} against ${pageTags.length} elements); an index taken from one and used on the other would name a different element`);
  }
  const out = [];
  for (const n of nodes) {
    if (n.ignored) continue;
    const role = n.role && n.role.value;
    if (!INTERACTIVE_AX_ROLES.has(role)) continue;
    const index = byBackend.get(n.backendDOMNodeId);
    if (index === undefined) continue;
    out.push({ index, role, name: (n.name && n.name.value) || "" });
  }
  // The role of everything else, so a stray in the tab order can be NAMED with what the
  // tree actually calls it rather than reported as an absence.
  const roleByIndex = new Map();
  for (const n of nodes) {
    const index = byBackend.get(n.backendDOMNodeId);
    if (index === undefined) continue;
    if (!roleByIndex.has(index)) roleByIndex.set(index, n.role && n.role.value);
  }
  return { interactive: out, roleByIndex };
}

// --- the tab order, walked for real ------------------------------------------------------

// tabWalk presses Tab until focus comes back round, and reads the component's appearance at
// every stop. The focus-visible cell of the matrix is taken HERE and nowhere else: this is
// the only moment in the run when the engine has moved focus itself.
export async function tabWalk(page, cap) {
  const stops = [];
  const seen = new Set();
  for (let i = 0; i < cap; i++) {
    await page.keyboard.press("Tab");
    const index = await page.evaluate(() => window.__hf.focusedIndex());
    if (index < 0) continue;            // focus left the document for the browser's own UI
    if (seen.has(index)) break;         // round again
    seen.add(index);
    stops.push({ index, state: await page.evaluate((i2) => window.__hf.stateOf(i2), index) });
  }
  await page.evaluate(() => { const a = document.activeElement; if (a && a.blur) a.blur(); });
  return stops;
}

// --- one page, one density, one whole matrix ---------------------------------------------

// atRest parks the pointer where nothing on this page has a hover treatment and takes focus
// off whatever last held it, so a DEFAULT reading is the component as a reader first meets
// it. The page's own top-left corner is inside the header, which draws no hover state.
async function atRest(page) {
  await page.evaluate(() => { const a = document.activeElement; if (a && a.blur) a.blur(); });
  await page.mouse.move(0, 0);
  await waitStill(page);
}

// centreOf scrolls a component into the middle of the viewport and answers the point a
// pointer would land on it. The middle, not the edge: the page's header is sticky, so an
// element scrolled flush to the top would be under it and the pointer would hover the header
// instead - which the reading itself then catches, because the engine would not report the
// component as hovered.
async function centreOf(page, index) {
  return page.evaluate((i) => {
    const el = document.querySelectorAll("*")[i];
    if (!el) return null;
    el.scrollIntoView({ block: "center", inline: "center", behavior: "instant" });
    const r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return null;
    return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2) };
  }, index);
}

const read = (page, index) => page.evaluate((i) => window.__hf.stateOf(i), index);

// enterState puts ONE component into ONE state at the engine and reads what it rendered.
// Every entry here is real input or a real property: a pointer the engine moved, a button it
// is holding down, the element's own `disabled`. Nothing is entered by adding a class, an
// attribute or an inline style that names a treatment - a state entered that way is a state
// the grader drew for itself, and the report would say the page has something it has not.
async function enterState(page, index, state, focusStops) {
  if (state === "default") {
    await atRest(page);
    const s = await read(page, index);
    return { entered: true, reading: s };
  }
  if (state === "focus-visible") {
    const stop = focusStops.find((f) => f.index === index);
    if (!stop) return { entered: false, note: "a real Tab never reached it, so the engine never focused it" };
    if (!stop.state.matches.focusVisible) {
      return { entered: false, note: "a real Tab focused it and the engine did not report :focus-visible" };
    }
    return { entered: true, reading: stop.state };
  }
  if (state === "hover" || state === "active") {
    const at = await centreOf(page, index);
    if (!at) return { entered: false, note: "the engine laid it out no box to point at" };
    await page.mouse.move(at.x, at.y);
    await waitStill(page);
    if (state === "hover") {
      const s = await read(page, index);
      const ok = s.matches && s.matches.hover;
      await atRest(page);
      return ok ? { entered: true, reading: s }
        : { entered: false, note: "the pointer was moved onto its centre and the engine did not report :hover; something is drawn over it" };
    }
    await page.mouse.down();
    const s = await read(page, index);
    // Release AWAY from the component: a mouse-up elsewhere is not an activation, so
    // entering :active never presses a button or follows a link for real.
    await page.mouse.move(0, 0);
    await page.mouse.up();
    await atRest(page);
    const ok = s.matches && s.matches.active;
    return ok ? { entered: true, reading: s }
      : { entered: false, note: "the pointer was pressed on its centre and the engine did not report :active" };
  }
  if (state === "disabled") {
    const s = await page.evaluate((i) => {
      const el = document.querySelectorAll("*")[i];
      if (!el || !("disabled" in el)) return null;
      const was = el.disabled;
      el.disabled = true;
      const reading = window.__hf.stateOf(i);
      el.disabled = was;
      return reading;
    }, index);
    if (!s) return { entered: false, note: "the element has no disabled property to set, so the engine has no such state for it" };
    if (!s.matches.disabled) return { entered: false, note: "its disabled property was set and the engine did not report :disabled" };
    return { entered: true, reading: s };
  }
  // loading and error are page states rather than engine states: this surface expresses them
  // on the status line beside a control and never on the control itself, so there is nothing
  // to enter. Saying so is the honest answer; the record declares them not applicable with a
  // reason, and a record that declares one PROVED is refused here rather than waved through.
  return { entered: false, note: `this surface offers no way to put a control into "${state}" at the engine` };
}

// differingProperties is AC-13's comparison: which of the properties a reader can see came
// out different from the component's default. The list compared is reported beside it, so a
// cell that found no difference says what it looked at rather than only that it looked.
function differingProperties(base, other) {
  const differing = [];
  for (const p of Object.keys(base)) if (base[p] !== other[p]) differing.push(`${p}: ${base[p]} -> ${other[p]}`);
  return differing;
}

// runMatrixForDensity is the whole of C7 for one density: derive the inventory at the engine,
// group it, and execute every cell the record declares PROVED.
export async function runMatrixForDensity(ctx, page, density) {
  await page.evaluate(([attr, value]) => window.__hf.applyDensity(attr, value),
    [density.attribute, density.value]);
  await waitStill(page);

  const cdp = await ctx.newCDPSession(page);
  const { interactive, roleByIndex } = await axInteractiveElements(cdp, page);
  await atRest(page);

  const axByIndex = new Map(interactive.map((n) => [n.index, n]));
  // The DEFAULT reading of every candidate, taken before anything has been focused, hovered
  // or pressed. It is the baseline every other cell is compared against.
  const defaults = new Map();
  for (const n of interactive) defaults.set(n.index, await read(page, n.index));

  const stops = await tabWalk(page, interactive.length + 12);
  const tabReached = stops.map((s) => s.index);

  const inventoryIndices = tabReached.filter((i) => axByIndex.has(i));
  const strays = tabReached.filter((i) => !axByIndex.has(i)).map((i) => {
    const stop = stops.find((s) => s.index === i);
    return { index: i, role: roleByIndex.get(i) || "",
      what: (stop && stop.state && stop.state.what) || `element ${i}` };
  });
  const readings = await page.evaluate((idx) => window.__hf.componentsOf(idx), inventoryIndices);
  const inventory = readings.map((c) => ({ ...c, role: axByIndex.get(c.index).role,
    name: axByIndex.get(c.index).name }));

  return { density, inventory, strays, defaults, stops };
}

// executeCells runs every cell the record declares proved for one density and hands back the
// readings the predicate decides from. It is separate from the derivation above so a case can
// assert the inventory without paying for the whole matrix.
export async function executeCells(page, density, groups, record, defaults, stops) {
  const cells = [];
  for (const g of groups) {
    if (!g.graded) continue;
    for (const e of record.entries) {
      if (e.component !== g.key) continue;
      if (e.verdict !== "proved") continue;
      const base = defaults.get(g.graded.index);
      const got = await enterState(page, g.graded.index, e.state, stops);
      const cell = { density: density.name, component: g.key, state: e.state,
        instance: g.graded.what, entered: got.entered, note: got.note || "" };
      if (got.entered) {
        cell.rendered = got.reading.rendered;
        cell.compared = Object.keys(base.props);
        cell.differing = differingProperties(base.props, got.reading.props);
      }
      cells.push(cell);
    }
  }
  return cells;
}

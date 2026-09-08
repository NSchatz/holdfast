// The two criteria that exist ONLY at the engine: what Tab actually focuses, and what the
// accessibility tree says each control is called.
//
// Neither is observable from the document. A KeyboardEvent constructed inside the page is
// untrusted and moves focus nowhere, so tab order cannot be read by any expression the page
// evaluates; and an accessible name is the engine's own answer over labels, ARIA, native
// semantics and content, not a property of markup anything can reconstruct by inspection.
import { wcagContrast } from "./graders.mjs";

function ordinalSuffix(n) {
  if (n % 100 >= 11 && n % 100 <= 13) return "th";
  if (n % 10 === 1) return "st";
  if (n % 10 === 2) return "nd";
  if (n % 10 === 3) return "rd";
  return "th";
}

// The roles a control takes in the accessibility tree. A node in this set with no
// accessible name is a control a screen-reader user meets as "button" and nothing else.
const INTERACTIVE_AX_ROLES = new Set([
  "button", "textbox", "searchbox", "link", "combobox", "checkbox", "radio",
  "slider", "spinbutton", "switch", "menuitem", "tab",
]);

// tabThroughEveryControl presses Tab for real and records what the engine focused, in the
// order it focused it.
export async function tabThroughEveryControl(page) {
  const expected = await page.evaluate(() => window.__hf.focusables());
  const reached = [];
  for (let i = 0; i < expected.length + 2 && reached.length < expected.length; i++) {
    await page.keyboard.press("Tab");
    const f = await page.evaluate(() => window.__hf.focused());
    if (f.none) continue;
    if (reached.length && reached[reached.length - 1].index === f.index) continue;
    reached.push(f);
  }
  return { expected, reached };
}

// Clause F1's keyboard half: Tab reaches every interactive control in the order the page
// is read, and each focused control draws an indicator that differs from its unfocused
// state and clears 3:1 against the colours next to it.
export function gradeTabOrderAndFocusRing(theme, expected, reached) {
  const out = [];
  const unfocused = new Map(expected.map((f) => [f.index, f]));

  if (reached.length !== expected.length) {
    return [`[${theme}] tabbing reached ${reached.length} controls, and the page offers ${expected.length}: ${
      JSON.stringify(reached.map((f) => f.what))} vs ${JSON.stringify(expected.map((f) => f.what))}`];
  }
  // IN THE ORDER THEY ARE READ: the index of each element among every element in the
  // document is its document order, and focus must arrive in that same order.
  reached.forEach((f, i) => {
    if (f.index !== expected[i].index) {
      out.push(`[${theme}] the ${i + 1}${ordinalSuffix(i + 1)} Tab reaches ${f.what}; reading order puts ${expected[i].what} there`);
    }
  });
  // A VISIBLE indicator: the focused control's computed style differs from its unfocused
  // one, and the indicator clears 3:1 against what is drawn next to it.
  for (const f of reached) {
    const before = unfocused.get(f.index);
    if (!before) continue;
    if (f.outline === before.outline && f.boxShadow === before.boxShadow && f.border === before.border) {
      out.push(`[${theme}] ${f.what} looks exactly the same focused as unfocused (outline "${f.outline}", shadow "${f.boxShadow}", border "${f.border}"): a keyboard user cannot see where they are`);
      continue;
    }
    if (f.outlineStyle === "none" || f.outlineWidth <= 0) {
      out.push(`[${theme}] ${f.what} draws no focus outline while focused ("${f.outline}")`);
      continue;
    }
    // The colours ADJACENT to the indicator, which is what the 3:1 floor is about. A ring
    // drawn with a positive outline-offset does not touch the control at all: the gap
    // shows the surface behind it, so that surface is what the ring is adjacent to on BOTH
    // sides. A ring at zero or negative offset overlaps the control, and then the
    // control's own face is adjacent to it too.
    const adjacent = [{ what: "the surface it is drawn on", colour: f.behindIndicator }];
    if (f.outlineOffset <= 0) adjacent.push({ what: "the control's own face", colour: f.ownBackground });
    for (const against of adjacent) {
      const ratio = wcagContrast(f.outlineColor, against.colour);
      if (ratio === null) {
        out.push(`[${theme}] ${f.what}: cannot measure the focus indicator "${f.outlineColor}" against ${against.colour}`);
        continue;
      }
      if (ratio < 2.995) {
        out.push(`[${theme}] ${f.what}: the focus indicator ${f.outlineColor} is ${ratio.toFixed(2)}:1 against ${against.what} (${against.colour}), under the 3:1 floor`);
      }
    }
  }
  return out;
}

// Clause F1 read off the tree the ENGINE computed: every interactive control and every
// region heading exposes a non-empty accessible name, and no control is named only by its
// placeholder text.
export function gradeAccessibleNames(nodes) {
  if (nodes.length < 20) return [`the engine computed ${nodes.length} accessibility nodes; the dashboard is bigger than that`];
  const out = [];
  let controls = 0, headings = 0;
  for (const n of nodes) {
    if (n.ignored) continue;
    const name = String(n.name || "").trim();
    if (INTERACTIVE_AX_ROLES.has(n.role)) {
      controls++;
      if (name === "") out.push(`an interactive control with role ${n.role} exposes no accessible name`);
      else if (n.onlyPlaceholder) out.push(`the control with role ${n.role} is named ONLY by its placeholder ("${name}"): a placeholder disappears the moment a value is typed`);
    } else if (n.role === "heading") {
      headings++;
      if (name === "") out.push("a region heading exposes no accessible name");
    }
  }
  if (controls < 5) out.push(`the accessibility tree carries ${controls} interactive controls; the dashboard has a token field, a filter and three buttons`);
  if (headings < 4) out.push(`the accessibility tree carries ${headings} headings; the dashboard has two regions and their blocks`);
  return out;
}

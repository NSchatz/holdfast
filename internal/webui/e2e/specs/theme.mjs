// Clause F10 and clauses S6/S7: the theme the ENGINE chose, and the contrast ratios the
// token file records beside each pair.
//
// These are graders and helpers, not cases. They live outside the spec files because two
// spec files decide from them - the case that grades the shipped page, and the case that
// defeats each grader on purpose - and a predicate that cannot be shared cannot be proved.
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { open, collect, waitRendered } from "./conventions.mjs";
import { wcagContrast } from "./graders.mjs";

const TOKENS = fileURLToPath(new URL("../../src/tokens.css", import.meta.url));

// The fifteen role names clause S2 fixes, verbatim.
export const S2 = ["--bg", "--panel", "--line", "--fg", "--muted", "--accent", "--ok", "--warn",
  "--bad", "--border", "--mark", "--focus", "--disabled", "--selected", "--link"];


export function pageURL(base, scenario) { return `${base}/?scenario=${scenario}`; }

// Clause F10 over three readings of the same document, each taken under a preference the
// ENGINE emulates: a light one, a dark one, and the ABSENCE of the override, which is what
// an engine reports when the platform expresses none. It is a pure function of the three,
// so the whole of it can be run against a document built to defeat it.
export function gradeThemeFollowsTheEnginesPreference(light, dark, none) {
  const out = [];
  let differing = 0;
  for (const name of S2) {
    const l = light.resolved[name], d = dark.resolved[name];
    if (!l || !d) { out.push(`the engine resolved no value for ${name} (light "${l}", dark "${d}")`); continue; }
    if (l !== d) differing++;
    else out.push(`${name} resolves to the same value "${l}" under a light and a dark preference; the two palettes are not two palettes`);
  }
  if (differing !== S2.length) out.push(`only ${differing} of the ${S2.length} role tokens change with the preference`);
  // The page is actually PAINTED in the palette in force, not merely told about it.
  if (light.bodyBackground === dark.bodyBackground) out.push(`the body is painted ${light.bodyBackground} under both preferences`);
  // With NO override in force, the page renders ONE NAMED PALETTE and never a mixture of
  // the two. That is the whole of what F10 asks, and it is asked as CONSISTENCY rather
  // than as identity with the light run.
  //
  // The distinction is not pedantry, and this port is what surfaced it. Which palette an
  // engine chooses when the test imposes nothing is a property of the HOST: a runner whose
  // desktop is dark reports dark, one whose desktop is light reports light, and the CSS
  // media feature has had no third value to emulate since `no-preference` was dropped from
  // the specification. An assertion that the third reading equals the LIGHT one therefore
  // passes or fails on whose machine it runs, which is not a property of this page at all.
  //
  // What IS a property of this page is that the two palettes never blend: every token the
  // engine resolved under no override must come from the same one, and the body must be
  // painted by that same one. A mixture - the failure F10 exists to name - differs from
  // both palettes on at least one token, and this admits none.
  const names = Object.keys(none.resolved || {});
  if (names.length === 0) { out.push("the engine resolved no tokens at all under no preference"); return out; }
  const matches = (palette) => names.every((n) => none.resolved[n] === palette.resolved[n]);
  const isLight = matches(light), isDark = matches(dark);
  if (!isLight && !isDark) {
    const odd = names.filter((n) => none.resolved[n] !== light.resolved[n] && none.resolved[n] !== dark.resolved[n]);
    const mixed = names.filter((n) => none.resolved[n] === dark.resolved[n]);
    out.push(`with no operating-system preference in force the page resolves a MIXTURE of the two palettes, not one named default: ${
      odd.length ? `${odd.length} token(s) match neither palette (${odd.slice(0, 3).join(", ")})` :
      `${mixed.length} token(s) come from the dark palette and the rest from the light one (${mixed.slice(0, 3).join(", ")})`}`);
    return out;
  }
  const chosen = isLight ? light : dark;
  if (none.bodyBackground !== chosen.bodyBackground) {
    out.push(`with no preference in force the page resolves the ${isLight ? "light" : "dark"} palette but paints the body ${none.bodyBackground}, which that palette paints ${chosen.bodyBackground}`);
  }
  return out;
}

// readThemeTriple takes the three readings the grader decides from. Shared with the
// counterexample, so the mutation is measured by exactly the same route as the shipped page.
export async function readThemeTriple(browser, url, mutation = null) {
  const read = async (theme) => {
    const { ctx, page } = await open(browser, { url, theme, mutation, waitFor: waitRendered });
    const s = await collect(page);
    await ctx.close();
    return s.tokens;
  };
  return [await read("light"), await read("dark"), await read("")];
}

// The contrast ratios the token file RECORDS beside each pair, read from the one committed
// token file. A record nobody checks is worse than no record.
export function readContrastRecords() {
  const body = readFileSync(TOKENS, "utf8");
  const re = /contrast:\s*(--[-\w]+)\s+on\s+(--[-\w]+)\s*\|\s*floor\s*([0-9.]+)\s*\|\s*dark\s*([0-9.]+)\s*\|\s*light\s*([0-9.]+)/g;
  const out = [];
  for (const m of body.matchAll(re)) {
    out.push({ fg: m[1], bg: m[2], floor: Number(m[3]), dark: Number(m[4]), light: Number(m[5]) });
  }
  if (out.length === 0) throw new Error("tokens.css records no measured contrast ratio at all; clause S7 requires one beside every pair that must clear a floor");
  return out;
}

export function hexToRGBText(v) {
  const m = /^#([0-9a-fA-F]{6})$/.exec(String(v).trim());
  if (!m) return null;
  return `rgb(${parseInt(m[1].slice(0, 2), 16)}, ${parseInt(m[1].slice(2, 4), 16)}, ${parseInt(m[1].slice(4, 6), 16)})`;
}

// Clauses S6 and S7 in one theme: every ratio the token file RECORDS beside a pair is
// recomputed from the two colours the engine resolved for that pair in that theme, and
// must agree with the record and clear the floor. A recorded ratio that disagrees with the
// measurement is worse than none.
export function gradeRecordedRatiosAgreeWithTheEngine(theme, tk, records) {
  const out = [];
  for (const rec of records) {
    const fg = hexToRGBText(tk.resolved[rec.fg]), bg = hexToRGBText(tk.resolved[rec.bg]);
    if (!fg || !bg) {
      out.push(`[${theme}] the engine resolved ${rec.fg} as "${tk.resolved[rec.fg]}" and ${rec.bg} as "${tk.resolved[rec.bg]}"; a recorded pair must name two token colours`);
      continue;
    }
    const measured = wcagContrast(fg, bg);
    if (measured === null) { out.push(`[${theme}] cannot measure ${rec.fg} on ${rec.bg} from ${fg} and ${bg}`); continue; }
    const recorded = theme === "dark" ? rec.dark : rec.light;
    if (Math.abs(measured - recorded) > 0.05) {
      out.push(`[${theme}] tokens.css records ${rec.fg} on ${rec.bg} as ${recorded.toFixed(2)}:1, and the engine measures ${measured.toFixed(2)}:1 (${fg} on ${bg}). A recorded ratio that disagrees with the measurement is worse than none`);
    }
    if (measured < rec.floor - 0.005) {
      out.push(`[${theme}] ${rec.fg} on ${rec.bg} measures ${measured.toFixed(2)}:1, under its recorded floor of ${rec.floor.toFixed(1)}:1`);
    }
  }
  return out;
}

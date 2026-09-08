// Clauses F3 and F6: what the page says about a fact nobody recorded, and what it does when
// the feed that was filling it dies.
import { open, collect } from "./conventions.mjs";

// The page's ONE phrase for a fact nobody recorded.
export const ABSENCE_PHRASE = "not recorded";


// Clause F3 over a snapshot in which every nullable field arrived null: each such field
// reads the page's ONE absence phrase, in words, and never a zero, a blank, a bare dash or
// a second word for the same fact.
export function gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(s) {
  const out = [];
  const cells = (s.figures && s.figures.cells) || [];
  if (cells.length === 0) return ["the page rendered no row cells at all, so this grader asserted nothing"];
  for (const c of cells) {
    const txt = String(c.text || "").trim();
    if (txt === "") { out.push(`${c.what} renders an EMPTY cell; a fact nobody recorded reads "${ABSENCE_PHRASE}", in words`); continue; }
    if (["0", "-", "0 B", "0.0", "unknown", "n/a"].includes(txt)) {
      out.push(`${c.what} renders "${txt}" for a value nobody recorded; the page's one absence phrase is "${ABSENCE_PHRASE}"`);
    }
  }
  // The fields the criterion names by hand, on a row where every one is null.
  const wantAbsent = new Set(["history td.size", "history td.vmaf", "history td.enc",
    "history td.dur", "history td.upd", "queue td.prog", "queue td.worker"]);
  for (const c of cells) {
    if (wantAbsent.has(c.what) && String(c.text || "").trim() !== ABSENCE_PHRASE) {
      out.push(`${c.what} reads "${c.text}" for an unmeasured value, want the page's one absence phrase "${ABSENCE_PHRASE}"`);
    }
  }
  for (const f of [
    { what: "reclaimed this run", v: s.figures.reclaimedSession },
    { what: "reclaimed lifetime", v: s.figures.reclaimedLifetime },
  ]) {
    if (String(f.v || "").trim() !== ABSENCE_PHRASE) {
      out.push(`"${f.what}" reads "${f.v}" for a total the server sent as null, want "${ABSENCE_PHRASE}"`);
    }
  }
  // A figure no row contributed to reads the same phrase, never a zero or an average of
  // nothing.
  for (const a of s.figures.aggs || []) {
    if (a.out) continue;
    if (String(a.value || "").trim() !== ABSENCE_PHRASE) {
      out.push(`the figure "${a.title}" reads "${a.value}" with no row contributing a value, want "${ABSENCE_PHRASE}"`);
    }
  }
  return out;
}

// Clause F6: the connection state says so in words, the rows and figures stay on the
// screen, and nothing on the page goes on presenting itself as live. The elapsed figure is
// the one that would: it is recomputed on a one-second timer from each row's own transition
// timestamp, and that timer knows nothing about the stream.
export function gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first, before, after) {
  const out = [];
  if (!String(first.connText).toLowerCase().includes("reconnect")) {
    out.push(`with the stream severed the page still reports its connection as "${first.connText}"`);
  }
  if (!first.figures.cells || first.figures.cells.length === 0) {
    out.push("a severed stream took the rows off the page; F6 keeps them and says they are stale");
  }
  if ((first.figures.aggs || []).length !== 6) {
    out.push(`a severed stream left ${(first.figures.aggs || []).length} whole-ledger figures on the page, want all 6`);
  }
  if (before.length === 0) { out.push("no elapsed figure was on the page at all, so this grader asserted nothing"); return out; }
  if (before.join("|") !== after.join("|")) {
    out.push(`with the stream severed the elapsed figures went on advancing: ${JSON.stringify(before)} became ${JSON.stringify(after)}. F6 refuses a figure that keeps reading as live`);
  }
  return out;
}

// severedStreamReading loads a page whose stream is severed and reads it twice, far enough
// apart that a figure still driven by the wall clock cannot help but move.
export async function severedStreamReading(browser, url, mutation = null) {
  const { ctx, page } = await open(browser, {
    url, theme: "light", mutation,
    waitFor: (p) => p.waitForFunction(
      () => window.__hf.connText().indexOf("reconnecting") === 0 && window.__hf.rendered(),
      null, { timeout: 15000 }),
  });
  const first = await collect(page);
  const before = await page.evaluate(() => window.__hf.elapsedValues());
  await page.waitForTimeout(3500);
  const after = await page.evaluate(() => window.__hf.elapsedValues());
  await ctx.close();
  return { first, before, after };
}

// The shared harness for the dashboard's rendered graders.
//
// Every criterion these specs decide is about what the page SHOWS, so every one is
// decided by reading what a real engine rendered: computed style after the whole cascade,
// real layout geometry, the text `innerText` says a reader can see, and a hit test at the
// subject's own centre. Nothing here matches HTML or CSS source text. A text grader
// cannot decide what a rule applies to, what wins the cascade, or what is SHOWN rather
// than merely built - and hardening one only ever closes the hole it was shown.
import { expect } from "@playwright/test";

// open loads the served document under one scenario and waits until a WHOLE snapshot has
// been through the page. That is a different moment from the stream opening: the page
// reports "live" on connect and the snapshot that fills the tables lands after it. The
// screen-reader summary is written at the end of every render and nowhere else, so a
// non-empty one is the page's OWN record that a render completed - which is why it, and
// not a timeout, is what these specs wait on.
export async function open(page, scenario = "full", { expectRender = true } = {}) {
  const problems = [];
  page.on("console", (m) => {
    if (["error", "warning"].includes(m.type())) problems.push(`console.${m.type()}: ${m.text()}`);
  });
  page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`));
  page.on("requestfailed", (r) => problems.push(`requestfailed: ${r.url()} ${r.failure()?.errorText}`));
  page.__problems = problems;

  await page.goto(`/?scenario=${encodeURIComponent(scenario)}`);
  if (expectRender) {
    await expect(page.locator("#sr-status")).not.toBeEmpty({ timeout: 15_000 });
  }
  return problems;
}

// requestsOffOrigin records every request the page issued to any origin but the one that
// served it. The served policy is `default-src 'none'`, so an off-origin fetch would not
// merely be a heavier page, it would be a refused one - and this is how a spec proves the
// page asked for nothing in the first place.
export function watchOrigins(page, baseURL) {
  const foreign = [];
  page.on("request", (r) => {
    const u = r.url();
    if (u.startsWith("data:") || u.startsWith("blob:") || u.startsWith("about:")) return;
    if (!u.startsWith(baseURL)) foreign.push(u);
  });
  return foreign;
}

// shown is the whole of "did a reader see this": present in the document, laid out with a
// real box, not display:none / visibility:hidden / opacity:0 anywhere up its ancestor
// chain, having an offsetParent, and winning a hit test at its own centre - which is the
// only way to catch something painted on top of it.
export async function shown(locator) {
  return locator.evaluate((el) => {
    if (!el) return { present: false };
    for (let n = el; n; n = n.parentElement) {
      const cs = getComputedStyle(n);
      if (cs.display === "none" || cs.visibility === "hidden" || Number(cs.opacity) === 0 || n.hasAttribute("hidden")) {
        return { present: true, visible: false, reason: `${n.tagName.toLowerCase()} is ${cs.display}/${cs.visibility}/${cs.opacity}${n.hasAttribute("hidden") ? "/[hidden]" : ""}` };
      }
    }
    el.scrollIntoView({ block: "center" });
    const r = el.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return {
      present: true,
      visible: r.width > 0 && r.height > 0 && el.offsetParent !== null,
      hit: !!hit && (hit === el || el.contains(hit) || hit.contains(el)),
      box: { x: r.x, y: r.y, width: r.width, height: r.height },
    };
  });
}

export async function expectShown(locator, what) {
  const s = await shown(locator);
  expect(s.present, `${what} is not in the document`).toBe(true);
  expect(s.visible, `${what} is in the document but not rendered to a reader: ${s.reason || "no box"}`).toBe(true);
  expect(s.hit, `${what} is laid out but something is painted over it`).toBe(true);
  return s;
}

// --- WCAG 2.2 relative luminance, recomputed here rather than trusted from the page ----
function channel(c) {
  const s = c / 255;
  return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
}
function luminance([r, g, b]) {
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}
export function contrastRatio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}
export function parseColor(css) {
  const m = css.match(/rgba?\(([^)]+)\)/);
  if (!m) throw new Error(`not a resolved colour: ${css}`);
  const parts = m[1].split(/[,\s/]+/).filter(Boolean).map(Number);
  return [parts[0], parts[1], parts[2]];
}

// effectiveBackground walks up until it finds an ancestor that actually paints one. A
// transparent background is not a colour a reader sees; the colour behind it is.
export async function effectiveBackground(locator) {
  return locator.evaluate((el) => {
    for (let n = el; n; n = n.parentElement) {
      const bg = getComputedStyle(n).backgroundColor;
      const m = bg.match(/rgba?\(([^)]+)\)/);
      if (!m) continue;
      const p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number);
      if (p.length < 4 || p[3] > 0) return bg;
    }
    return getComputedStyle(document.body).backgroundColor;
  });
}

// readableText is what a reader can actually READ, which is innerText and never
// textContent: textContent returns the contents of a display:none subtree just as
// happily as a visible one.
export async function readableText(locator) {
  return (await locator.evaluate((el) => el.innerText)).replace(/\s+/g, " ").trim();
}

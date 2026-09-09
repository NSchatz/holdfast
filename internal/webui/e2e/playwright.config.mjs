// The dashboard's Playwright project.
//
// It grades the SERVED document. `webServer` below starts the Go fixture server, which
// mounts the real `webui.HandlerFor` - so what these specs load is the same bytes and the
// same Content-Security-Policy `holdfast serve` puts on the wire, not a copy of them
// assembled here. There is one reader of that document in this repository, deliberately.
//
// The browser is the one the machine already has. `channel: "chromium"` would make
// Playwright fetch its own build, which would put a browser download in the build path of
// a repository whose whole packaging story is a pinned, digest-verified artifact; the
// projects below take `HOLDFAST_BROWSER` or the chromium on PATH instead, exactly as the
// repository's own graders have always located one.
import { defineConfig, devices } from "@playwright/test";
import { accessSync, constants } from "node:fs";
import { cpus } from "node:os";
import { join } from "node:path";

// browserPath resolves the engine the same way the rest of this repository does: an
// explicit HOLDFAST_BROWSER wins, then whatever `chromium`/`chrome` is on PATH. A run
// with none is a FAILURE here rather than a silent fall back to a downloaded build - a
// grader that quietly measures a different engine than the gate believes is a grader
// nobody can reason about.
function browserPath() {
  if (process.env.HOLDFAST_BROWSER) {
    const pinned = process.env.HOLDFAST_BROWSER;
    try {
      accessSync(pinned, constants.X_OK);
    } catch {
      throw new Error(
        `HOLDFAST_BROWSER names ${pinned}, which is not an executable this process can run. ` +
        "An explicit engine pin is never replaced by a browser found on PATH: a grader that " +
        "silently measures a different engine than the gate believes is a grader nobody can reason about."
      );
    }
    return pinned;
  }
  // The candidates in the order scripts/find-browser.sh uses: a real vendor binary before
  // `chromium`, because on several distributions /usr/bin/chromium is a snap shim that
  // hangs instead of failing. CI never reaches this loop - it exports HOLDFAST_BROWSER,
  // pinned to an engine it watched render - so this is the answer for a machine that
  // named none.
  const dirs = (process.env.PATH || "").split(":").filter(Boolean);
  for (const name of ["google-chrome", "google-chrome-stable", "chrome", "chromium", "chromium-browser"]) {
    for (const dir of dirs) {
      const candidate = join(dir, name);
      try {
        accessSync(candidate, constants.X_OK);
        return candidate;
      } catch { /* not here; try the next directory */ }
    }
  }
  throw new Error(
    "no browser engine found: set HOLDFAST_BROWSER or put chromium on PATH. " +
    "These graders read what a real engine rendered, so there is nothing to fall back to."
  );
}

const PORT = Number(process.env.HOLDFAST_E2E_PORT || 8931);

// How many engines run at once, and why it is CAPPED rather than left to the runner.
//
// Each worker holds a browser of its own, and every case here waits on a real render, a
// real layout and a real animation settling - so the work is not CPU-bound and more
// engines buy nothing past a handful. Playwright's default is half the machine's cores,
// which on the 56-core host these graders run on is 28 browsers: every case then missed
// its 30-second deadline, several chromiums died leaving core files beside the project,
// and the whole run was still unfinished after ten minutes. The same suite at four
// workers passes in under two.
const WORKERS = Number(process.env.HOLDFAST_E2E_WORKERS || 0) ||
  Math.min(4, Math.max(1, Math.ceil(cpus().length / 2)));

// The specs that set up their own theme, viewport and preference. They run once, under the
// `engine` project, and are ignored by the per-theme ones.
const PAGE_DRIVEN = /(conventions|a11y|states|motion|policy|mutations|inert|docs|alignment)\.spec\.mjs$/;

export default defineConfig({
  testDir: "./specs",
  // A wedged spec must fail with its output rather than hang until the runner is killed,
  // which is the same rule the Go graders' own deadlines exist for.
  timeout: 30_000,
  expect: { timeout: 8_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: process.env.CI ? 2 : WORKERS,
  reporter: process.env.CI ? [["list"], ["json", { outputFile: "results.json" }]] : [["list"]],
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    launchOptions: { executablePath: browserPath() },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  // Two kinds of spec, so two kinds of project.
  //
  // The specs that read the page as it comes get a project per THEME and WIDTH, so each
  // runs in every combination without having to remember to ask. The theme is set at the
  // ENGINE (colorScheme), never by a class, an attribute or a stylesheet injected into the
  // page: a grader that injects the theme is grading its own fixture.
  //
  // The convention graders drive their own world - they compare a light render against a
  // dark one, a moving page against a still one, three renders under three preferences -
  // so a project's theme and viewport mean nothing to them. They run ONCE, under `engine`.
  // Running them in three projects would not be three measurements; it would be the same
  // measurement three times, which is how a suite gets slower without getting stricter.
  projects: [
    {
      name: "engine",
      testMatch: /(conventions|a11y|states|motion|policy|mutations|inert|docs|alignment)\.spec\.mjs$/,
      use: { ...devices["Desktop Chrome"] },
    },
    { name: "dark-wide",   testIgnore: PAGE_DRIVEN, use: { ...devices["Desktop Chrome"], colorScheme: "dark",  viewport: { width: 1440, height: 1000 } } },
    { name: "light-wide",  testIgnore: PAGE_DRIVEN, use: { ...devices["Desktop Chrome"], colorScheme: "light", viewport: { width: 1440, height: 1000 } } },
    { name: "dark-narrow", testIgnore: PAGE_DRIVEN, use: { ...devices["Desktop Chrome"], colorScheme: "dark",  viewport: { width: 360,  height: 900  } } },
  ],
  // The fixture server is COMPILED FROM THIS TREE, which is the whole reason these specs
  // grade the document `holdfast serve` produces. So a run starts its own and never adopts
  // one that is already listening: an adopted server is a binary built from a tree nobody
  // can name, and the graders would report on a page this checkout did not produce. That
  // was not hypothetical - a killed run left one on this port and every later run silently
  // graded against it, including one that came back green over a defect since fixed.
  //
  // A port already in use is therefore a LOUD failure rather than a quiet adoption. Hand
  // iteration against a server you started yourself is the one case where adopting is what
  // you meant, and it says so.
  webServer: {
    command: `go run ./fixtureserver -addr 127.0.0.1:${PORT} -fixtures ./fixtures`,
    url: `http://127.0.0.1:${PORT}/`,
    reuseExistingServer: process.env.HOLDFAST_E2E_REUSE_SERVER === "1",
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});

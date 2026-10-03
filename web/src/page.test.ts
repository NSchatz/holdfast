// @vitest-environment node
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

// The page is a contract with the server, which this project cannot see from here:
// internal/ui replaces the empty footer below, byte for byte, with the AGPL section 13
// Corresponding Source offer, and refuses to serve a page that does not carry it.
const page = readFileSync(fileURLToPath(new URL("../index.html", import.meta.url)), "utf8");

describe("index.html", () => {
  it("carries the slot the server writes the Corresponding Source offer into, exactly once", () => {
    expect(page.split('<footer id="source-offer"></footer>').length - 1).toBe(1);
  });

  it("keeps the offer outside the element the UI mounts in", () => {
    const app = page.indexOf('<div id="app"></div>');
    expect(app).toBeGreaterThan(-1);
    expect(page.indexOf('<footer id="source-offer">')).toBeGreaterThan(app);
  });

  it("has no inline script and no inline style, so the page runs under script-src 'self'", () => {
    for (const tag of page.match(/<script\b[^>]*>/g) ?? []) {
      expect(tag).toContain(" src=");
    }
    expect(page).not.toMatch(/<style\b/);
    expect(page).not.toMatch(/\sstyle=/);
    expect(page).not.toMatch(/\son[a-z]+=/);
  });
});

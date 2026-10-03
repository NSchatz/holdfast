// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

// Rules about the source of the UI itself, held by reading it. They are about what a
// page built from these files can do, so they are checked on the files and not on one
// rendering of them.

const web = fileURLToPath(new URL("..", import.meta.url));
const SKIP = new Set(["node_modules", "dist", ".vite"]);

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    if (SKIP.has(name)) {
      return [];
    }
    const full = join(dir, name);
    return statSync(full).isDirectory() ? walk(full) : [full];
  });
}

const files = walk(web).map((full) => ({ name: relative(web, full), body: readFileSync(full, "utf8") }));
const isTest = (name: string) => /(\.test\.ts|test-helpers\.ts|test-setup\.ts)$/.test(name);
const shipped = files.filter((f) => f.name.startsWith("src/") && !isTest(f.name));
const components = shipped.filter((f) => f.name.endsWith(".svelte"));

describe("the UI's own source", () => {
  it("is being read: the walk sees the shell, the views and the API module", () => {
    const names = files.map((f) => f.name);
    for (const expected of ["index.html", "package.json", "src/App.svelte", "src/lib/api.ts", "src/views/Controls.svelte"]) {
      expect(names).toContain(expected);
    }
    expect(components.length).toBeGreaterThanOrEqual(8);
  });

  // The two local commands that put older bytes back over a library file and that re-open
  // an answered row. They are not on the HTTP surface and must not appear here in any
  // form: no route, no button, no text, no identifier. The words are assembled so that
  // this file does not contain them either.
  const banned = [["re", "store"].join(""), ["re", "queue"].join("")];

  it.each(banned.map((word) => [word.length, word] as const))(
    "controls: local command %#, of %i letters, appears nowhere under web/ - not in a file and not in a file name",
    (_length, word) => {
      const hits = files.filter((f) => f.body.toLowerCase().includes(word) || f.name.toLowerCase().includes(word));
      expect(hits.map((f) => f.name)).toEqual([]);
    },
  );

  it("the check for those two words bites: it finds a word that is there", () => {
    const present = ["hold", "fast"].join("");
    expect(files.some((f) => f.body.toLowerCase().includes(present))).toBe(true);
  });

  it("token: nothing shipped touches storage, cookies, IndexedDB, the address or the console", () => {
    const forbidden =
      /localStorage|sessionStorage|indexedDB|\.cookie|pushState|replaceState|location\.(hash|search|href|assign|replace)|console\.|caches\.|BroadcastChannel|postMessage/;
    for (const f of shipped) {
      expect(f.body.match(forbidden)?.[0], f.name).toBeUndefined();
    }
  });

  it("does not use the event stream, which cannot carry an Authorization header", () => {
    for (const f of shipped) {
      expect(/new\s+EventSource|\/api\/events/.test(f.body), f.name).toBe(false);
    }
  });

  it("has no inline style, no style directive, no raw HTML and no handler written as a string", () => {
    expect(components.length).toBeGreaterThan(0);
    for (const f of components) {
      const markup = f.body.replace(/<script[\s\S]*?<\/script>/g, "").replace(/<style[\s\S]*?<\/style>/g, "");
      expect(markup.match(/\sstyle\s*=|\sstyle:|\{@html|\son[a-z]+="|javascript:|data:/)?.[0], f.name).toBeUndefined();
    }
  });

  it("names no other origin: no absolute URL in anything shipped", () => {
    for (const f of shipped) {
      expect(f.body.match(/https?:\/\/|\/\/[a-z0-9.-]+\.[a-z]{2,}\//i)?.[0], f.name).toBeUndefined();
    }
  });

  it("uses plain hyphens only", () => {
    // The en dash and the em dash, by code point, so this file carries neither.
    const dashes = [String.fromCharCode(0x2013), String.fromCharCode(0x2014)];
    for (const f of files.filter((f) => f.name.startsWith("src/") || f.name === "index.html")) {
      expect(dashes.some((dash) => f.body.includes(dash)), f.name).toBe(false);
    }
  });
});

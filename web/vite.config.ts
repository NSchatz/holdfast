import { svelte } from "@sveltejs/vite-plugin-svelte";
// From vitest/config and not from vite: the `test` key below is vitest's, and
// svelte-check refuses it on vite's own type.
import { defineConfig } from "vitest/config";

// The build writes to web/dist and nowhere else. The Go binary embeds
// internal/ui/dist, which holds a committed placeholder so `go build` and `go vet`
// pass with no UI built; `make ui-build` copies this output beside that placeholder.
// Building straight into the embed directory would delete it: emptyOutDir clears
// the directory it is pointed at.
export default defineConfig({
  plugins: [svelte()],
  // Every asset lands under /assets/ with a content hash in its name. The server
  // serves that one directory and the page, and nothing else of the build.
  base: "/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    assetsDir: "assets",
    // No asset is inlined into the page as a data: URI, so the Content-Security-Policy
    // the server sends can name 'self' alone.
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  // The browser build of svelte in tests: without it vitest resolves the server
  // build, where mount() does not exist.
  resolve: process.env.VITEST ? { conditions: ["browser"] } : undefined,
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.ts"],
    setupFiles: ["src/test-setup.ts"],
    restoreMocks: true,
  },
});

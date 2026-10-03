import { cleanup } from "@testing-library/svelte";
import { afterEach } from "vitest";

// Each test renders into the one document jsdom gives the file. Without this a later
// test finds an earlier test's alert and passes or fails on it.
afterEach(() => {
  cleanup();
});

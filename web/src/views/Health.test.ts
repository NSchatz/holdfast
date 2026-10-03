import { render, screen, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import { cellsOf, fakeFetch, json, plain, textOf } from "../test-helpers";
import Health from "./Health.svelte";

// The API reference's own sample, with synthetic paths.
const idle = {
  enabled: true,
  interval_hours: 168,
  state: "idle",
  next_due_at: 1791469938,
  current: null,
  last_completed: {
    id: 1,
    started_at: 1790865138,
    finished_at: 1790865738,
    checked: 4,
    ok: 2,
    corrupt: 1,
    unreadable: 1,
    problems: [
      {
        path: "/library/shows/example-03.mkv",
        result: "corrupt",
        reason: "[in#0/matroska,webm @ 0x55d9dcd94480] File ended prematurely",
        checked_at: 1790865138,
        size: 19562,
      },
      {
        path: "/library/shows/example-04.mkv",
        result: "unreadable",
        reason: "permission denied",
        checked_at: 1790865140,
        size: 12,
      },
    ],
    problems_truncated: false,
  },
};

const running = {
  enabled: true,
  interval_hours: 24,
  state: "waiting",
  waiting: "paused",
  next_due_at: null,
  current: {
    id: 2,
    started_at: 1791000000,
    finished_at: null,
    checked: 10,
    ok: 10,
    corrupt: 0,
    unreadable: 0,
    problems: [],
    problems_truncated: false,
  },
  last_completed: idle.last_completed,
};

async function shown(body: unknown) {
  const server = fakeFetch((call) => (call.path === "/api/health" ? json(body) : plain("not found", 404)));
  const view = render(Health, { fetch: server.fetch, intervalMs: 0 });
  await screen.findByText(/^Read at /);
  return { ...view, calls: server.calls };
}

const fact = (root: HTMLElement, name: string) =>
  textOf(within(root).getByText(name, { selector: "dt" }).nextElementSibling);

describe("health view", () => {
  it("health view: shows the sweep's state, its schedule and when the next is due", async () => {
    const { container } = await shown(idle);
    expect(fact(container, "State")).toBe("idle: no sweep is due");
    expect(fact(container, "Scheduled")).toBe("every 168 hours");
    expect(fact(container, "Next due")).toMatch(/^2026-10-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{2}:\d{2}$/);
    expect(screen.getByText("No sweep is under way.")).toBeTruthy();
  });

  it("health view: shows the last finished sweep with its counts", async () => {
    await shown(idle);
    const last = screen.getByTestId("health-last");
    expect(within(last).getByRole("heading").textContent).toBe("The last finished sweep");
    expect(fact(last, "Files checked")).toBe("4");
    expect(fact(last, "Sound")).toBe("2");
    expect(fact(last, "Corrupt")).toBe("1");
    expect(fact(last, "Unreadable")).toBe("1");
    expect(fact(last, "Finished")).not.toBe("not yet");
  });

  it("health view: lists the files found corrupt or unreadable, with the reason, size and time", async () => {
    const { container } = await shown(idle);
    expect(cellsOf(container, "/library/shows/example-03.mkv").slice(0, 3)).toEqual([
      "corrupt",
      "[in#0/matroska,webm @ 0x55d9dcd94480] File ended prematurely",
      "19.10 KiB",
    ]);
    expect(cellsOf(container, "/library/shows/example-04.mkv").slice(0, 3)).toEqual([
      "unreadable",
      "permission denied",
      "12 B",
    ]);
  });

  it("health view: shows the sweep under way, what it waits on, and that it has not finished", async () => {
    const { container } = await shown(running);
    expect(fact(container, "State")).toBe("waiting: a sweep is due or under way, and no new decode may start now");
    expect(fact(container, "Waiting on")).toBe("paused");
    expect(fact(container, "Next due")).toBe("no time set");
    const current = screen.getByTestId("health-current");
    expect(fact(current, "Files checked so far")).toBe("10");
    expect(fact(current, "Finished")).toBe("not yet");
    expect(within(current).getByText("This sweep lists no corrupt or unreadable file.")).toBeTruthy();
    expect(screen.getByTestId("health-last")).toBeTruthy();
  });

  it("health view: says the list was cut short when the server says so", async () => {
    await shown({ ...idle, last_completed: { ...idle.last_completed, problems_truncated: true } });
    expect(screen.getByText(/The server cut this list short/)).toBeTruthy();
  });

  it("health view: with no sweep configured it says so, and shows no sweep", async () => {
    const { container } = await shown({
      enabled: false,
      interval_hours: 0,
      state: "off",
      next_due_at: null,
      current: null,
      last_completed: null,
    });
    expect(fact(container, "State")).toBe("off: no sweep is configured");
    expect(fact(container, "Scheduled")).toBe("no (health_sweep_interval_hours is off)");
    expect(screen.getByText("No sweep has run to the end yet.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("health view: counts missing from the answer are unavailable, never 0", async () => {
    await shown({ state: "running", current: { id: 3, problems: [{ path: "/library/films/example-a.mkv" }] } });
    const current = screen.getByTestId("health-current");
    for (const name of ["Files checked so far", "Sound", "Corrupt", "Unreadable"]) {
      expect(fact(current, name)).toBe("unavailable");
    }
    expect(cellsOf(current, "/library/films/example-a.mkv")).toEqual(["not recorded", "", "not recorded", "not recorded"]);
    expect(screen.getByText("The server did not say whether this list is complete.")).toBeTruthy();
  });

  it("health view: is a report only - it offers no action on a finding", async () => {
    const { calls } = await shown(idle);
    // The one button is the read's own Refresh; nothing else can be pressed, typed or followed.
    expect(screen.getAllByRole("button").map(textOf)).toEqual(["Refresh"]);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(calls.every((call) => call.method === "GET" && call.path === "/api/health")).toBe(true);
  });

  it("health view: a refusal is shown in the server's words", async () => {
    render(Health, { fetch: fakeFetch(() => plain("unauthorized", 401)).fetch, token: "nope", intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("401 unauthorized");
    expect(alert.textContent).toContain("The token entered is not one this server accepts");
  });
});

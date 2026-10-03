import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import { cellsOf, columnsOf, fakeFetch, json, plain, textOf } from "../test-helpers";
import Summary from "./Summary.svelte";

const GiB = 1024 ** 3;

const spreadOf = (min: number | null, mean: number | null, max: number | null, counted: number) => ({
  available: true,
  unavailable: "",
  covers: "every done row in the ledger",
  window: "",
  age_seconds: 0,
  counted,
  excluded: 1,
  min,
  mean,
  max,
});

const body = {
  summary: { pending: 4, encoding: 1, done: 12, skipped: 7, failed: 2 },
  bytes_reclaimed_session: 3 * GiB,
  bytes_reclaimed_lifetime: 40 * GiB,
  bytes_held_by_undo_window: 5 * GiB,
  paused: true,
  scanning: false,
  roots: [
    {
      root: "/library/films",
      candidate_files: 120,
      candidate_excluded: 3,
      candidate_bytes: 900 * GiB,
      projection_basis_files: 12,
      projected_savings_bytes: 450 * GiB,
      bytes_held_by_undo_window: 5 * GiB,
      free_bytes: 200 * GiB,
    },
    {
      root: "/library/shows",
      candidate_files: 0,
      candidate_excluded: 0,
      candidate_bytes: 0,
      projection_basis_files: 0,
      projected_savings_bytes: null,
      bytes_held_by_undo_window: null,
      free_bytes: null,
    },
  ],
  roots_unattributed: {
    candidate_files: 2,
    candidate_excluded: 0,
    candidate_bytes: 1536,
    bytes_held_by_undo_window: null,
  },
  aggregates: {
    outcomes: {
      available: true,
      unavailable: "",
      covers: "every terminal row in the ledger",
      window: "",
      age_seconds: 0,
      counted: 21,
      excluded: 0,
      buckets: [
        { key: "done", count: 12 },
        { key: "skipped", count: 7 },
        { key: "failed", count: 2 },
      ],
    },
    skips_by_guard: {
      available: true,
      unavailable: "",
      covers: "every skipped row in the ledger",
      window: "",
      age_seconds: 0,
      counted: 7,
      excluded: 0,
      buckets: [{ key: "already-at-target-codec", count: 7 }],
    },
    size_ratio: spreadOf(0.31, 0.42, 0.6, 11),
    encode_ms: spreadOf(61000, 185000, 7620000, 11),
    vmaf_mean: spreadOf(null, null, null, 0),
    vmaf_min: { ...spreadOf(null, null, null, 0), available: false, unavailable: "the figure could not be read" },
  },
};

function serve(answer: unknown = body) {
  return fakeFetch((call) => (call.path === "/api/summary" ? json(answer) : plain("not found", 404)));
}

async function shown(fetch: ReturnType<typeof serve>["fetch"], token = "") {
  const view = render(Summary, { fetch, token, intervalMs: 0 });
  await screen.findByText(/^Read at /);
  return view;
}

const fact = (name: string) => textOf(screen.getByText(name, { selector: "dt" }).nextElementSibling);

describe("summary view", () => {
  it("summary view: shows the counts per status, in the order a job passes through them", async () => {
    const { fetch } = serve();
    await shown(fetch);
    const table = screen.getByRole("table", { name: "Jobs by status" });
    const rows = Array.from(table.querySelectorAll("tbody tr")).map((tr) =>
      Array.from(tr.children).map(textOf).join(" "),
    );
    expect(rows).toEqual(["pending 4", "encoding 1", "done 12", "skipped 7", "failed 2"]);
  });

  it("summary view: shows paused and scanning", async () => {
    const { fetch } = serve();
    await shown(fetch);
    expect(fact("Paused")).toBe("yes");
    expect(fact("Scanning")).toBe("no");
  });

  it("summary view: shows bytes reclaimed (lifetime and this run) and bytes held by the undo window, with the exact count", async () => {
    const { fetch } = serve();
    await shown(fetch);
    expect(fact("Reclaimed, lifetime")).toBe("40.00 GiB");
    expect(fact("Reclaimed, this run")).toBe("3.00 GiB");
    expect(fact("Held by the undo window")).toBe("5.00 GiB");
    expect(screen.getByText("40.00 GiB").getAttribute("title")).toBe(`${40 * GiB} bytes`);
  });

  it("summary view: shows the per-root table, with the unattributed row", async () => {
    const { fetch } = serve();
    const { container } = await shown(fetch);
    const table = screen.getByRole("table", { name: "Library roots" });
    expect(columnsOf(table)).toEqual([
      "Root",
      "Candidate files",
      "Candidate bytes",
      "Excluded",
      "Projection basis (files)",
      "Projected savings",
      "Held by the undo window",
      "Free space",
    ]);
    expect(cellsOf(container, "/library/films")).toEqual([
      "120",
      "900.00 GiB",
      "3",
      "12",
      "450.00 GiB",
      "5.00 GiB",
      "200.00 GiB",
    ]);
    expect(cellsOf(container, "Under no configured root")).toEqual([
      "2",
      "1.50 KiB",
      "0",
      "not applicable",
      "not applicable",
      "unavailable",
      "not applicable",
    ]);
  });

  it("summary view: a null figure is unavailable and a zero stays a zero", async () => {
    const { fetch } = serve();
    const { container } = await shown(fetch);
    expect(cellsOf(container, "/library/shows")).toEqual([
      "0",
      "0 B",
      "0",
      "0",
      "unavailable",
      "unavailable",
      "unavailable",
    ]);
  });

  it("summary view: figures missing from the answer are unavailable, never 0", async () => {
    const { fetch } = serve({ summary: { done: null } });
    const { container } = await shown(fetch);
    expect(fact("Paused")).toBe("unavailable");
    expect(fact("Reclaimed, lifetime")).toBe("unavailable");
    expect(fact("Reclaimed, this run")).toBe("unavailable");
    expect(fact("Held by the undo window")).toBe("unavailable");
    expect(cellsOf(container, "done")).toEqual(["unavailable"]);
    expect(screen.getByText("This server's summary carries no per-root figures.")).toBeTruthy();
    expect(screen.getByText("This server's summary carries no whole-ledger figures.")).toBeTruthy();
    expect(container.textContent).not.toMatch(/\b0 B\b/);
  });

  it("summary view: shows the whole-ledger aggregates with the set each covers", async () => {
    const { fetch } = serve();
    const { container } = await shown(fetch);
    expect(cellsOf(container, "already-at-target-codec")).toEqual(["7"]);
    expect(container.textContent).toContain("every skipped row in the ledger");
    expect(cellsOf(container, "Output size as a share of the source")).toEqual([
      "0.310",
      "0.420",
      "0.600",
      "11",
      "1",
      "every done row in the ledger",
    ]);
    expect(cellsOf(container, "Encode time").slice(0, 3)).toEqual(["1 min 01 s", "3 min 05 s", "2 h 07 min"]);
  });

  it("summary view: an aggregate nothing contributed to is no data, and an unreadable one says why", async () => {
    const { fetch } = serve();
    const { container } = await shown(fetch);
    expect(cellsOf(container, "VMAF mean").slice(0, 4)).toEqual(["no data", "no data", "no data", "0"]);
    expect(cellsOf(container, "VMAF worst frame")).toEqual(["Unavailable: the figure could not be read"]);
  });

  it("summary view: states when the data was read, and reads again on Refresh", async () => {
    const { fetch, calls } = serve();
    await shown(fetch);
    expect(textOf(screen.getByRole("status"))).toMatch(/^Read at \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{2}:\d{2}\.$/);
    expect(calls).toHaveLength(1);
    await fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(calls).toHaveLength(2));
  });

  it("summary view: sends the token as a bearer header and in no URL", async () => {
    const { fetch, calls } = serve();
    await shown(fetch, "s3cret");
    expect(calls[0]?.headers["Authorization"]).toBe("Bearer s3cret");
    expect(calls[0]?.url).toBe("/api/summary");
  });

  it("summary view: a 401 is shown in the server's words and says a token is needed", async () => {
    const { fetch } = fakeFetch(() => plain("unauthorized", 401));
    render(Summary, { fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("401 unauthorized");
    expect(alert.textContent).toContain("server_read_token");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("summary view: a body of another shape is an alert, not a table of zeros", async () => {
    const { fetch } = serve({ status: "ok" });
    render(Summary, { fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("/api/summary did not answer with a summary");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("summary view: leaving the view aborts the read in flight", () => {
    const { fetch, calls } = fakeFetch(() => new Promise<Response>(() => {}));
    const view = render(Summary, { fetch, intervalMs: 0 });
    expect(calls[0]?.signal?.aborted).toBe(false);
    view.unmount();
    expect(calls[0]?.signal?.aborted).toBe(true);
  });
});

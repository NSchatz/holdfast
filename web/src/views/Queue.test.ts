import { render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import { cellsOf, columnsOf, fakeFetch, jobRow, json, plain, textOf, total } from "../test-helpers";
import Queue from "./Queue.svelte";

const MiB = 1024 ** 2;

const rows = [
  jobRow({
    path: "/library/films/example-a.mkv",
    status: "encoding",
    worker: "w1",
    updated_at: 1790000000,
    priority: 10,
    source_bytes: 2048 * MiB,
    source_codec: "h264",
    source_width: 1920,
    source_height: 1080,
    library_root: "/library/films",
    progress_seconds: 600,
    progress_duration_seconds: 2400,
    progress_fraction: 0.25,
  }),
  jobRow({ path: "/library/films/example-b.mkv", status: "pending", priority: 0, fail_count: 1 }),
  jobRow({ path: "/library/other/example-c.mkv", status: "encoding", priority: null }),
];

function serve(body: unknown) {
  return fakeFetch((call) => (call.path === "/api/queue" ? json(body) : plain("not found", 404)));
}

async function shown(body: unknown) {
  const { fetch, calls } = serve(body);
  const view = render(Queue, { fetch, intervalMs: 0 });
  await screen.findByText(/^Read at /);
  return { ...view, calls };
}

describe("queue view", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("queue view: shows status, path, priority, source size, codec, dimensions and library root", async () => {
    const { container } = await shown({ now: 1790000125, queue: rows, queue_total: total(3, 500) });
    expect(columnsOf(screen.getByRole("table"))).toEqual([
      "Status",
      "Path",
      "Priority",
      "Source size",
      "Codec",
      "Dimensions",
      "Library root",
      "Progress",
      "In this state",
      "Worker",
      "Failures",
    ]);
    const cells = cellsOf(container, "/library/films/example-a.mkv");
    expect(cells[0]).toBe("encoding");
    expect(cells.slice(1, 6)).toEqual(["10", "2.00 GiB", "h264", "1920x1080", "/library/films"]);
    expect(cells.slice(7)).toEqual(["2 min 05 s", "w1", "0"]);
    expect(screen.getByText("2.00 GiB").getAttribute("title")).toBe(`${2048 * MiB} bytes`);
  });

  it("queue view: shows priority 0 as 0 and a null priority as unavailable", async () => {
    const { container } = await shown({ now: 1790000125, queue: rows, queue_total: total(3, 500) });
    expect(cellsOf(container, "/library/films/example-b.mkv")[1]).toBe("0");
    expect(cellsOf(container, "/library/other/example-c.mkv")[1]).toBe("unavailable");
  });

  it("queue view: shows the encoder's progress as a bar and a figure where the row carries one", async () => {
    const { container } = await shown({ now: 1790000125, queue: rows, queue_total: total(3, 500) });
    const bars = container.querySelectorAll("progress");
    expect(bars).toHaveLength(1);
    expect(bars[0]?.getAttribute("value")).toBe("0.25");
    expect(cellsOf(container, "/library/films/example-a.mkv")[6]).toBe("25.0% (10 min 00 s of 40 min 00 s)");
  });

  it("queue view: a row with no progress reported shows none, never 0%", async () => {
    const { container } = await shown({ now: 1790000125, queue: rows, queue_total: total(3, 500) });
    expect(cellsOf(container, "/library/other/example-c.mkv")[6]).toBe("not reported yet");
    expect(cellsOf(container, "/library/films/example-b.mkv")[6]).toBe("none reported in this state");
    expect(container.textContent).not.toContain("0.0%");
  });

  it("queue view: facts a row does not record are not recorded, never 0", async () => {
    const { container } = await shown({ now: null, queue: rows, queue_total: total(3, 500) });
    expect(cellsOf(container, "/library/films/example-b.mkv").slice(2, 6)).toEqual([
      "not recorded",
      "not recorded",
      "not recorded",
      "not recorded",
    ]);
    expect(cellsOf(container, "/library/films/example-b.mkv")[7]).toBe("unavailable");
  });

  it("queue view: says 'showing N of M' from queue_total", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(1234, 500) });
    expect(textOf(screen.getByTestId("queue-total"))).toBe(
      "Showing 3 of 1234 pending and active jobs (the server sends at most 500).",
    );
  });

  it("queue view: says the total is unavailable rather than putting a figure in its place", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(null, 500, "the figure could not be read") });
    const said = textOf(screen.getByTestId("queue-total"));
    expect(said).toBe(
      "Showing 3 pending and active jobs. The total behind them is unavailable: the figure could not be read.",
    );
    expect(said).not.toMatch(/of \d/);
  });

  it("queue view: says the total is unavailable when the answer carries none", async () => {
    await shown({ now: 1790000125, queue: [] });
    expect(textOf(screen.getByTestId("queue-total"))).toBe(
      "Showing 0 pending and active jobs. The total behind them is unavailable.",
    );
    expect(screen.getByText("The queue is empty.")).toBeTruthy();
  });

  it("queue view: a refusal is shown in the server's words", async () => {
    const { fetch } = fakeFetch(() => plain("internal error reading queue", 500));
    render(Queue, { fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("500 internal error reading queue");
  });

  it("queue view: reads again on its interval while the page is visible, and stops when left", async () => {
    vi.useFakeTimers();
    const { fetch, calls } = serve({ now: 1, queue: [], queue_total: total(0, 500) });
    const view = render(Queue, { fetch, intervalMs: 1000 });
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(2000);
    expect(calls).toHaveLength(3);
    view.unmount();
    await vi.advanceTimersByTimeAsync(5000);
    expect(calls).toHaveLength(3);
  });

  it("queue view: states how old queue_total is beside it, where the server says it is not current", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(1234, 500, "", 12) });
    expect(textOf(screen.getByTestId("queue-total"))).toBe(
      "Showing 3 of 1234 pending and active jobs (the server sends at most 500), total as of 12 s before this read.",
    );
  });

  it.each([
    ["an age of 0", 0],
    ["no age", null],
    ["an age that is not a number", "12"],
  ])("queue view: claims no age for a total that carries %s", async (_name, age) => {
    await shown({ now: 1790000125, queue: rows, queue_total: { ...total(1234, 500), age_seconds: age } });
    expect(textOf(screen.getByTestId("queue-total"))).toBe(
      "Showing 3 of 1234 pending and active jobs (the server sends at most 500).",
    );
  });

  it("queue view: more rows than a stale total is said as that, never as 'N of fewer'", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(2, 500, "", 12) });
    const said = textOf(screen.getByTestId("queue-total"));
    expect(said).toBe(
      "Showing 3 pending and active jobs. The server's total of 2 was counted 12 s before this read, so it is older than the rows shown.",
    );
    expect(said).not.toMatch(/\d of \d/);
  });

  it("queue view: more rows than a total that states no age is not written as a share either", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(2, 500) });
    const said = textOf(screen.getByTestId("queue-total"));
    expect(said).toBe(
      "Showing 3 pending and active jobs. The server's total of 2 is lower than the rows shown: it was counted apart from them, and the rows are the newer of the two.",
    );
    expect(said).not.toMatch(/\d of \d/);
  });

  it("queue view: as many rows as the total is still 'N of N'", async () => {
    await shown({ now: 1790000125, queue: rows, queue_total: total(3, 500, "", 5) });
    expect(textOf(screen.getByTestId("queue-total"))).toBe(
      "Showing 3 of 3 pending and active jobs (the server sends at most 500), total as of 5 s before this read.",
    );
  });

  it("queue view: entering a token reads again at once with it, and forgetting it reads again without", async () => {
    // No interval: only the token changing can cause the second and third reads.
    const { fetch, calls } = serve({ now: 1, queue: [], queue_total: total(0, 500) });
    const view = render(Queue, { fetch, intervalMs: 0 });
    await screen.findByText(/^Read at /);
    expect(calls).toHaveLength(1);
    expect(calls[0]?.headers).not.toHaveProperty("Authorization");

    await view.rerender({ token: "s3cret" });
    expect(calls).toHaveLength(2);
    expect(calls[1]?.headers["Authorization"]).toBe("Bearer s3cret");

    await view.rerender({ token: "" });
    expect(calls).toHaveLength(3);
    expect(calls[2]?.headers).not.toHaveProperty("Authorization");
  });
});

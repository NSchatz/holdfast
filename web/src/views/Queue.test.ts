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
});

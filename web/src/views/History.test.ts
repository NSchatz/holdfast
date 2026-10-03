import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import { cellsOf, deferred, fakeFetch, jobRow, json, plain, textOf, total, type Call } from "../test-helpers";
import History from "./History.svelte";

const MiB = 1024 ** 2;

const done = jobRow({
  path: "/library/films/example-a.mkv",
  status: "done",
  encoder: "cpu",
  updated_at: 1700000000,
  priority: 5,
  vmaf_mean: 96.128,
  vmaf_min: 91.5,
  source_codec: "h264",
  source_bytes: 1000 * MiB,
  output_bytes: 400 * MiB,
  encode_ms: 185000,
  source_width: 1920,
  source_height: 1080,
  library_root: "/library/films",
});
const skipped = jobRow({
  path: "/library/films/example-b.mkv",
  status: "skipped",
  reason: "already-at-target-codec",
  updated_at: 1700000100,
});
const failed = jobRow({
  path: "/library/films/example-c.mkv",
  status: "failed",
  reason: "vmaf gate: worst frame 71.2 below floor 80",
  source_bytes: 500 * MiB,
});

// A server that pages: page one, then "c2", then "c3", then the end.
function pagingServer() {
  return fakeFetch((call: Call) => {
    const cursor = call.query.get("cursor");
    const filtered = call.query.getAll("status").length > 0;
    const page = (rows: unknown[], next: string | null) =>
      json({ history: rows, history_total: total(filtered ? 1 : 3, Number(call.query.get("limit"))), next_cursor: next });
    if (filtered) {
      return page([failed], null);
    }
    if (cursor === null) {
      return page([done], "c2");
    }
    if (cursor === "c2") {
      return page([skipped], "c3");
    }
    if (cursor === "c3") {
      return page([failed], null);
    }
    return json(
      {
        rule: "invalid-query",
        error: "the request was refused: 1 parameter is not valid",
        retryable: false,
        parameters: [{ parameter: "cursor", rule: "cursor-undecodable", error: "the cursor is not one this server issued" }],
      },
      400,
    );
  });
}

async function shown(server = pagingServer()) {
  const view = render(History, { fetch: server.fetch, intervalMs: 0 });
  await screen.findByText(/^Read at /);
  return { ...view, calls: server.calls };
}

const next = () => screen.getByRole("button", { name: "Next page" });
const first = () => screen.getByRole("button", { name: "First page" });

describe("history view", () => {
  it("history view: shows each row's recorded outcome - status, reason, sizes, saving, source facts and when", async () => {
    const { container } = await shown();
    const cells = cellsOf(container, "/library/films/example-a.mkv");
    expect(cells.slice(0, 5)).toEqual(["done", "", "1000.00 MiB", "400.00 MiB", "600.00 MiB (60.0%)"]);
    expect(cells.slice(5, 13)).toEqual(["h264", "1920x1080", "cpu", "96.13", "91.50", "3 min 05 s", "5", "/library/films"]);
    expect(cells[13]).toMatch(/^2023-11-1[45] \d{2}:\d{2}:20 [+-]\d{2}:\d{2}$/);
  });

  it("history view: a fact a row never recorded is not recorded, never 0 and never a 100% saving", async () => {
    const { container } = await shown(
      fakeFetch(() => json({ history: [skipped, failed], history_total: total(2, 50), next_cursor: null })),
    );
    const cells = cellsOf(container, "/library/films/example-b.mkv");
    expect(cells.slice(0, 2)).toEqual(["skipped", "already-at-target-codec"]);
    expect(cells.slice(2, 11)).toEqual(Array(9).fill("not recorded"));
    expect(cells[11]).toBe("unavailable");
    // A source size with no output size is no saving at all.
    expect(cellsOf(container, "/library/films/example-c.mkv").slice(2, 5)).toEqual([
      "500.00 MiB",
      "not recorded",
      "not recorded",
    ]);
    expect(container.textContent).not.toContain("100.0%");
  });

  it("history view: asks for the first page with the page size and no filter", async () => {
    const { calls } = await shown();
    expect(calls[0]?.url).toBe("/api/history?limit=50");
  });

  it("history view: offers a filter over the six terminal statuses, and no other", async () => {
    await shown();
    expect(screen.getAllByRole("checkbox").map((box) => textOf(box.closest("label")))).toEqual([
      "done",
      "skipped",
      "failed",
      "would-transcode",
      "indeterminate",
      "applied-despite-error",
    ]);
  });

  it("history view: sends the ticked statuses as the status filter and starts again from the first page", async () => {
    const { calls } = await shown();
    await fireEvent.click(next());
    await screen.findByText("Page 2: 1 rows.", { exact: false });
    await fireEvent.click(screen.getByRole("checkbox", { name: "failed" }));
    await waitFor(() => expect(calls.at(-1)?.query.getAll("status")).toEqual(["failed"]));
    expect(calls.at(-1)?.query.get("cursor")).toBeNull();
    await screen.findByText("Page 1: 1 rows.", { exact: false });
    await fireEvent.click(screen.getByRole("checkbox", { name: "would-transcode" }));
    await waitFor(() => expect(calls.at(-1)?.query.getAll("status")).toEqual(["failed", "would-transcode"]));
    await fireEvent.click(screen.getByRole("checkbox", { name: "failed" }));
    await waitFor(() => expect(calls.at(-1)?.query.getAll("status")).toEqual(["would-transcode"]));
  });

  it("history view: sends the chosen page size as limit and starts again from the first page", async () => {
    const { calls } = await shown();
    expect(Array.from(screen.getByRole("combobox").querySelectorAll("option")).map(textOf)).toEqual([
      "25",
      "50",
      "100",
      "200",
    ]);
    await fireEvent.click(next());
    await screen.findByText("Page 2: 1 rows.", { exact: false });
    await fireEvent.change(screen.getByRole("combobox"), { target: { value: "200" } });
    await waitFor(() => expect(calls.at(-1)?.url).toBe("/api/history?limit=200"));
  });

  it("history view: follows next_cursor page by page until it is null", async () => {
    const { calls, container } = await shown();
    expect(next()).toHaveProperty("disabled", false);
    await fireEvent.click(next());
    await screen.findByText("Page 2: 1 rows.", { exact: false });
    expect(calls.at(-1)?.query.get("cursor")).toBe("c2");
    expect(cellsOf(container, "/library/films/example-b.mkv")[0]).toBe("skipped");

    await fireEvent.click(next());
    await screen.findByText("Page 3: 1 rows.", { exact: false });
    expect(calls.at(-1)?.query.get("cursor")).toBe("c3");
    expect(cellsOf(container, "/library/films/example-c.mkv")[0]).toBe("failed");

    // The last page: nothing follows a null cursor.
    expect(next()).toHaveProperty("disabled", true);
    expect(screen.getByText("This is the last page.")).toBeTruthy();
    const asked = calls.length;
    await fireEvent.click(next());
    expect(calls).toHaveLength(asked);
  });

  it("history view: goes back to the first page", async () => {
    const { calls, container } = await shown();
    expect(first()).toHaveProperty("disabled", true);
    await fireEvent.click(next());
    await screen.findByText("Page 2: 1 rows.", { exact: false });
    await fireEvent.click(first());
    await screen.findByText("Page 1: 1 rows.", { exact: false });
    expect(calls.at(-1)?.url).toBe("/api/history?limit=50");
    expect(cellsOf(container, "/library/films/example-a.mkv")[0]).toBe("done");
  });

  it("history view: states history_total as the ledger's count, not the rows shown", async () => {
    await shown();
    expect(textOf(screen.getByTestId("history-total"))).toBe(
      "Page 1: 1 rows. The ledger holds 3 rows (every matching row in the ledger).",
    );
    await fireEvent.click(screen.getByRole("checkbox", { name: "failed" }));
    await waitFor(() =>
      expect(textOf(screen.getByTestId("history-total"))).toBe(
        "Page 1: 1 rows. The ledger holds 1 rows matching this filter (every matching row in the ledger).",
      ),
    );
  });

  it("history view: says the total is unavailable rather than putting a figure in its place", async () => {
    await shown(
      fakeFetch(() =>
        json({ history: [done], history_total: total(null, 50, "the figure could not be read"), next_cursor: null }),
      ),
    );
    expect(textOf(screen.getByTestId("history-total"))).toBe(
      "Page 1: 1 rows. The total behind them is unavailable: the figure could not be read.",
    );
  });

  it("history view: shows a 400 refusal in the server's own words, with each parameter's", async () => {
    const refusing = fakeFetch(() =>
      json(
        {
          rule: "invalid-query",
          error: "the request was refused: 1 parameter is not valid",
          retryable: false,
          parameters: [{ parameter: "cursor", rule: "cursor-undecodable", error: "the cursor is not one this server issued" }],
        },
        400,
      ),
    );
    render(History, { fetch: refusing.fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("400 the request was refused: 1 parameter is not valid");
    expect(alert.textContent).toContain("cursor: the cursor is not one this server issued");
    // Never the stable tokens in place of the words.
    expect(alert.textContent).not.toContain("cursor-undecodable");
    expect(alert.textContent).not.toContain("invalid-query");
  });

  it("history view: after a refused page, offers the way back to the first page", async () => {
    let refuse = false;
    const server = fakeFetch((call) => {
      if (refuse && call.query.get("cursor") !== null) {
        return json({ rule: "invalid-query", error: "refused", retryable: false, parameters: [] }, 400);
      }
      return json({ history: [done], history_total: total(3, 50), next_cursor: "c2" });
    });
    await shown(server);
    refuse = true;
    await fireEvent.click(next());
    await screen.findByRole("alert");
    expect(screen.queryByRole("table")).toBeNull();
    await fireEvent.click(first());
    await screen.findByText("Page 1: 1 rows.", { exact: false });
    expect(server.calls.at(-1)?.query.get("cursor")).toBeNull();
  });

  it("history view: says so when the server predates paging and filtering", async () => {
    await shown(fakeFetch(() => json({ history: [done], history_total: total(1, 50) })));
    expect(screen.getByTestId("history-unpaged").textContent).toContain("predates filtering and paging");
    expect(next()).toHaveProperty("disabled", true);
  });

  it("history view: sends the token as a bearer header and in no URL", async () => {
    const server = pagingServer();
    render(History, { fetch: server.fetch, token: "s3cret", intervalMs: 0 });
    await screen.findByText(/^Read at /);
    await fireEvent.click(next());
    await screen.findByText("Page 2: 1 rows.", { exact: false });
    for (const call of server.calls) {
      expect(call.headers["Authorization"]).toBe("Bearer s3cret");
      expect(call.url).not.toContain("s3cret");
    }
  });

  it("history view: a 401 is shown in the server's words", async () => {
    render(History, { fetch: fakeFetch(() => plain("unauthorized", 401)).fetch, intervalMs: 0 });
    expect((await screen.findByRole("alert")).textContent).toContain("401 unauthorized");
  });

  it("history view: shows each parameter's words for every refusal the server names, and never the token", async () => {
    const refusing = fakeFetch(() =>
      json(
        {
          rule: "invalid-query",
          error: "nothing was read: the request's status and cursor could not be accepted",
          retryable: false,
          parameters: [
            { parameter: "status", rule: "status-not-terminal", error: "status names a state that is not terminal" },
            { parameter: "cursor", rule: "cursor-filter-mismatch", error: "cursor was issued under another status set" },
          ],
        },
        400,
      ),
    );
    render(History, { fetch: refusing.fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("status: status names a state that is not terminal");
    expect(alert.textContent).toContain("cursor: cursor was issued under another status set");
    for (const token of ["invalid-query", "status-not-terminal", "cursor-filter-mismatch"]) {
      expect(alert.textContent).not.toContain(token);
    }
  });

  it("history view: states how old history_total is beside it, where the server says it is not current", async () => {
    await shown(fakeFetch(() => json({ history: [done], history_total: total(40, 50, "", 12), next_cursor: null })));
    expect(textOf(screen.getByTestId("history-total"))).toBe(
      "Page 1: 1 rows. The ledger holds 40 rows (every matching row in the ledger), total as of 12 s before this read.",
    );
  });

  it("history view: more rows on the page than a stale total is said as that, not as a contradiction", async () => {
    await shown(
      fakeFetch(() => json({ history: [done, skipped, failed], history_total: total(2, 50, "", 12), next_cursor: null })),
    );
    const said = textOf(screen.getByTestId("history-total"));
    expect(said).toBe(
      "Page 1: 3 rows. The server's total of 2 was counted 12 s before this read, so it is older than the rows shown.",
    );
    expect(said).not.toContain("The ledger holds 2 rows");
  });

  it("history view: a page that arrives after the filter changed does not land over the newer one", async () => {
    // The answer to the filtered read is held back, and delivered only after the filter
    // was cleared and the unfiltered page has been shown. This fetch does not honour the
    // abort: a real one may already have the answer on its way.
    const late = deferred<Response>();
    const server = fakeFetch((call) => {
      if (call.query.getAll("status").length > 0) {
        return late.promise;
      }
      return json({ history: [done], history_total: total(3, 50), next_cursor: null });
    });
    const { container, calls } = await shown(server);
    await fireEvent.click(screen.getByRole("checkbox", { name: "failed" }));
    await waitFor(() => expect(calls.at(-1)?.query.getAll("status")).toEqual(["failed"]));
    const superseded = calls.at(-1);

    await fireEvent.click(screen.getByRole("checkbox", { name: "failed" }));
    await waitFor(() => expect(calls).toHaveLength(3));
    await screen.findByText(/^Read at /);
    expect(superseded?.signal?.aborted).toBe(true);
    expect(cellsOf(container, "/library/films/example-a.mkv")[0]).toBe("done");

    late.resolve(json({ history: [failed], history_total: total(1, 50), next_cursor: null }));
    await late.promise;
    await new Promise((r) => setTimeout(r, 20));
    expect(cellsOf(container, "/library/films/example-a.mkv")[0]).toBe("done");
    expect(container.textContent).not.toContain("/library/films/example-c.mkv");
    expect(textOf(screen.getByTestId("history-total"))).toContain("The ledger holds 3 rows");
  });
});

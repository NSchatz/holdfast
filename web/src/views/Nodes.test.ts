import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import { cellsOf, columnsOf, fakeFetch, json, plain, textOf, total } from "../test-helpers";
import Nodes from "./Nodes.svelte";

const MiB = 1024 ** 2;

// The shape the goal-14 wire contract fixes for GET /api/nodes.
const body = {
  enabled: true,
  now: 1700000020,
  nodes: [
    { node: "n1", mode: "http", encoders: ["libx265", "nvenc"], waiting: false, cooling_until: null, leases_active: 1 },
    { node: "n2", mode: null, encoders: null, waiting: true, cooling_until: 1700000300, leases_active: 0 },
  ],
  leases: [
    {
      node: "n1",
      path: "/library/films/example-a.mkv",
      state: "granted",
      epoch: 3,
      granted_at: 1700000000,
      updated_at: 1700000010,
      expires_at: 1700000070,
      ended_at: null,
      reason: null,
      source_bytes: 2048 * MiB,
      output_bytes: null,
    },
    {
      node: "n2",
      path: "/library/films/example-b.mkv",
      state: "expired",
      epoch: 1,
      granted_at: 1699990000,
      updated_at: 1699990100,
      expires_at: 1699990160,
      ended_at: 1699990160,
      reason: "server_restart",
      source_bytes: 1024 * MiB,
      output_bytes: 300 * MiB,
    },
  ],
  leases_total: total(12, 200),
};

async function shown(answer: unknown) {
  const server = fakeFetch((call) => (call.path === "/api/nodes" ? json(answer) : plain("not found", 404)));
  const view = render(Nodes, { fetch: server.fetch, intervalMs: 0 });
  await screen.findByText(/^Read at /);
  return { ...view, calls: server.calls };
}

describe("nodes view", () => {
  it("nodes view: lists each worker node with its mode, encoders, poll, cooling-off and active leases", async () => {
    const { container } = await shown(body);
    expect(columnsOf(screen.getByRole("table", { name: "Worker nodes" }))).toEqual([
      "Node",
      "Mode",
      "Encoders offered",
      "Waiting for work",
      "Cooling off until",
      "Active leases",
    ]);
    expect(cellsOf(container, "n1")).toEqual(["http", "libx265, nvenc", "no", "not cooling off", "1"]);
    const n2 = cellsOf(container, "n2");
    expect(n2.slice(0, 3)).toEqual(["not known", "not known", "yes"]);
    expect(n2[3]).toMatch(/^2023-11-1[45] \d{2}:\d{2}:20 [+-]\d{2}:\d{2}$/);
    expect(n2[4]).toBe("0");
    expect(textOf(screen.getByTestId("nodes-enabled"))).toBe("This server takes worker nodes.");
  });

  it("nodes view: lists each lease with its node, state, reason, epoch, times and sizes", async () => {
    const { container } = await shown(body);
    const active = cellsOf(container, "/library/films/example-a.mkv");
    expect(active.slice(0, 4)).toEqual(["n1", "granted", "", "3"]);
    expect(active[7]).toBe("not ended");
    expect(active.slice(8)).toEqual(["2.00 GiB", "none uploaded"]);
    const ended = cellsOf(container, "/library/films/example-b.mkv");
    expect(ended.slice(0, 4)).toEqual(["n2", "expired", "server_restart", "1"]);
    expect(ended[7]).not.toBe("not ended");
    expect(ended.slice(8)).toEqual(["1.00 GiB", "300.00 MiB"]);
  });

  it("nodes view: says 'showing N of M' leases from leases_total", async () => {
    await shown(body);
    expect(textOf(screen.getByTestId("leases-total"))).toBe(
      "Showing 2 of 12 leases, newest first (the server sends at most 200).",
    );
  });

  it("nodes view: says the lease total is unavailable rather than putting a figure in its place", async () => {
    await shown({ ...body, leases_total: total(null, 200, "the figure could not be read") });
    expect(textOf(screen.getByTestId("leases-total"))).toBe(
      "Showing 2 leases, newest first. The total behind them is unavailable: the figure could not be read.",
    );
  });

  it("nodes view: a server that takes no worker nodes says so", async () => {
    await shown({ enabled: false, now: 1700000020, nodes: [], leases: [], leases_total: total(0, 200) });
    expect(textOf(screen.getByTestId("nodes-enabled"))).toBe("This server takes no worker nodes: none is configured.");
    expect(screen.getByText("No worker node is known to this server.")).toBeTruthy();
    expect(screen.getByText("No lease is recorded.")).toBeTruthy();
  });

  it("nodes view: fields missing from the answer are unavailable or not recorded, never 0", async () => {
    const { container } = await shown({ nodes: [{ node: "n9" }], leases: [{ node: "n9", path: "/library/films/example-c.mkv" }] });
    expect(cellsOf(container, "n9")).toEqual(["not known", "not known", "unavailable", "not cooling off", "unavailable"]);
    const lease = cellsOf(container, "/library/films/example-c.mkv");
    expect(lease.slice(1, 4)).toEqual(["not recorded", "", "not recorded"]);
    expect(lease.slice(8)).toEqual(["not recorded", "none uploaded"]);
    expect(textOf(screen.getByTestId("nodes-enabled"))).toBe("The server did not say whether it takes worker nodes.");
  });

  it("nodes view: on a server that predates the endpoint (404) it says so plainly", async () => {
    render(Nodes, { fetch: fakeFetch(() => plain("404 page not found", 404)).fetch, intervalMs: 0 });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("This server does not serve /api/nodes (404): it predates this view");
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("nodes view: only reads - it offers no action on a node or a lease", async () => {
    const { calls } = await shown(body);
    expect(screen.getAllByRole("button").map(textOf)).toEqual(["Refresh"]);
    expect(calls.every((call) => call.method === "GET" && call.path === "/api/nodes")).toBe(true);
  });

  it("nodes view: sends the token as a bearer header and in no URL", async () => {
    const server = fakeFetch(() => json(body));
    render(Nodes, { fetch: server.fetch, token: "s3cret", intervalMs: 0 });
    await screen.findByText(/^Read at /);
    expect(server.calls[0]?.headers["Authorization"]).toBe("Bearer s3cret");
    expect(server.calls[0]?.url).toBe("/api/nodes");
  });

  it("nodes view: shows the server's own words for a mode and a lease state", async () => {
    const lease = body.leases[0];
    const states = ["granted", "uploaded", "completed", "failed", "expired"];
    const { container } = await shown({
      ...body,
      nodes: [
        { node: "n-mapped", mode: "mapped", encoders: [], waiting: true, cooling_until: null, leases_active: 0 },
        { node: "n-http", mode: "http", encoders: [], waiting: true, cooling_until: null, leases_active: 0 },
      ],
      leases: states.map((state) => ({ ...lease, state, path: `/library/films/${state}.mkv` })),
    });
    expect(cellsOf(container, "n-mapped")[0]).toBe("mapped");
    expect(cellsOf(container, "n-http")[0]).toBe("http");
    expect(states.map((state) => cellsOf(container, `/library/films/${state}.mkv`)[1])).toEqual(states);
  });

  it("nodes view: states how old leases_total is beside it, where the server says it is not current", async () => {
    await shown({ ...body, leases_total: total(12, 200, "", 75) });
    expect(textOf(screen.getByTestId("leases-total"))).toBe(
      "Showing 2 of 12 leases, newest first (the server sends at most 200), total as of 1 min 15 s before this read.",
    );
  });

  it("nodes view: more leases than a stale total is said as that, never as 'N of fewer'", async () => {
    await shown({ ...body, leases_total: total(1, 200, "", 30) });
    const said = textOf(screen.getByTestId("leases-total"));
    expect(said).toBe(
      "Showing 2 leases, newest first. The server's total of 1 was counted 30 s before this read, so it is older than the rows shown.",
    );
    expect(said).not.toMatch(/\d of \d/);
  });
});

import { describe, expect, it } from "vitest";

import { fakeFetch, jobRow, json, plain, total } from "../test-helpers";
import {
  addExclusion,
  loadExclusions,
  loadHealth,
  loadHistory,
  loadNodes,
  loadQueue,
  loadSummary,
  parseHealth,
  parseHistory,
  parseNodes,
  parseQueue,
  parseRefusal,
  parseScanReport,
  parseSummary,
  pause,
  removeExclusion,
  resume,
  scanPaths,
  startLibraryScan,
} from "./endpoints";

describe("endpoints: a body of another shape is unreachable, never a partly filled view", () => {
  it.each([
    ["summary", loadSummary, [null, [], "x", {}, { summary: [] }, { summary: null }]],
    ["queue", loadQueue, [null, {}, { queue: null }, { queue: {} }, { queue: [7] }, { queue: [{ status: "pending" }] }]],
    ["health", loadHealth, [null, {}, { state: 7 }, []]],
    ["nodes", loadNodes, [null, {}, { nodes: [] }, { leases: [] }, { nodes: {}, leases: [] }]],
    ["exclusions", loadExclusions, [null, {}, { exclusions: null }, { exclusions: [{ created_at: 1 }] }]],
  ] as const)("%s", async (_name, load, bodies) => {
    for (const body of bodies) {
      const { fetch } = fakeFetch(() => json(body));
      const result = await load({ fetch });
      expect(result.kind, JSON.stringify(body)).toBe("unreachable");
    }
  });

  it("history", async () => {
    for (const body of [null, {}, { history: null }, { history: [{ path: "/library/films/example-a.mkv" }] }]) {
      const { fetch } = fakeFetch(() => json(body));
      expect((await loadHistory({ limit: 50, statuses: [], cursor: null }, { fetch })).kind).toBe("unreachable");
    }
  });
});

describe("endpoints: a field that is missing or null is null, never 0", () => {
  it("summary", () => {
    const s = parseSummary({ summary: { done: 2, pending: null, odd: 1 } });
    expect(s).toEqual({
      counts: [
        { status: "pending", count: null },
        { status: "done", count: 2 },
        { status: "odd", count: 1 },
      ],
      paused: null,
      scanning: null,
      reclaimedLifetime: null,
      reclaimedSession: null,
      heldByUndoWindow: null,
      roots: null,
      unattributed: null,
      aggregates: null,
    });
  });

  it("summary roots and aggregates", () => {
    const s = parseSummary({
      summary: {},
      roots: [{ root: "/library/films", candidate_files: 3, free_bytes: null }],
      roots_unattributed: { candidate_files: null },
      aggregates: {
        outcomes: { available: true, counted: 2, excluded: 0, buckets: [{ key: "done", count: 2 }, { count: 9 }] },
        size_ratio: { available: false, unavailable: "could not be read", min: 0, mean: 0, max: 0 },
      },
    });
    expect(s?.roots).toEqual([
      {
        root: "/library/films",
        candidateFiles: 3,
        candidateExcluded: null,
        candidateBytes: null,
        projectionBasisFiles: null,
        projectedSavingsBytes: null,
        heldByUndoWindow: null,
        freeBytes: null,
      },
    ]);
    expect(s?.unattributed?.candidateFiles).toBeNull();
    expect(s?.aggregates?.outcomes?.buckets).toEqual([{ key: "done", count: 2 }]);
    // A figure the server marked unavailable is not read, whatever number sits beside it.
    expect(s?.aggregates?.sizeRatio).toMatchObject({ available: false, min: null, mean: null, max: null });
    expect(s?.aggregates?.vmafMin).toBeNull();
  });

  it("a job row", () => {
    const q = parseQueue({ queue: [{ path: "/library/films/example-a.mkv", status: "encoding" }] });
    const row = q?.rows[0];
    expect(row?.priority).toBeNull();
    expect(row?.sourceBytes).toBeNull();
    expect(row?.progressFraction).toBeNull();
    expect(row?.sourceCodec).toBeNull();
    expect(q?.now).toBeNull();
    expect(q?.total).toBeNull();
  });

  it("a total marked unavailable has no count, whatever number sits beside it", () => {
    const q = parseQueue({ queue: [], queue_total: { available: false, unavailable: "not read", cap: 500, count: 0 } });
    expect(q?.total).toEqual({
      available: false,
      count: null,
      cap: 500,
      covers: null,
      unavailable: "not read",
      ageSeconds: null,
    });
  });

  it("the age of a total and of each whole-ledger figure is a number, else null", () => {
    const aged = (age: unknown) => ({ available: true, cap: 500, count: 3, age_seconds: age });
    expect(parseQueue({ queue: [], queue_total: aged(12) })?.total?.ageSeconds).toBe(12);
    expect(parseQueue({ queue: [], queue_total: aged(0) })?.total?.ageSeconds).toBe(0);
    expect(parseQueue({ queue: [], queue_total: aged(null) })?.total?.ageSeconds).toBeNull();
    expect(parseQueue({ queue: [], queue_total: aged("12") })?.total?.ageSeconds).toBeNull();
    expect(parseHistory({ history: [], history_total: aged(7) })?.total?.ageSeconds).toBe(7);
    expect(parseNodes({ nodes: [], leases: [], leases_total: aged(9) })?.total?.ageSeconds).toBe(9);
    const figure = (age: unknown) => ({ available: true, age_seconds: age, counted: 1, excluded: 0, buckets: [] });
    const s = parseSummary({
      summary: {},
      aggregates: { outcomes: figure(30), skips_by_guard: figure("30"), size_ratio: figure(4), encode_ms: figure(null) },
    });
    expect(s?.aggregates?.outcomes?.ageSeconds).toBe(30);
    expect(s?.aggregates?.skipsByGuard?.ageSeconds).toBeNull();
    expect(s?.aggregates?.sizeRatio?.ageSeconds).toBe(4);
    expect(s?.aggregates?.encodeMs?.ageSeconds).toBeNull();
  });

  it("history tells an absent cursor from a null one", () => {
    expect(parseHistory({ history: [] })).toMatchObject({ nextCursor: null, pages: false });
    expect(parseHistory({ history: [], next_cursor: null })).toMatchObject({ nextCursor: null, pages: true });
    expect(parseHistory({ history: [], next_cursor: "abc" })).toMatchObject({ nextCursor: "abc", pages: true });
  });

  it("health", () => {
    const h = parseHealth({ state: "running", current: { id: 2, problems: [{ path: "/library/films/example-a.mkv" }] } });
    expect(h).toMatchObject({ enabled: null, intervalHours: null, nextDueAt: null, lastCompleted: null });
    expect(h?.current).toMatchObject({ checked: null, corrupt: null, finishedAt: null, problemsTruncated: null });
    expect(h?.current?.problems[0]).toEqual({
      path: "/library/films/example-a.mkv",
      result: null,
      reason: null,
      checkedAt: null,
      size: null,
    });
  });

  it("nodes", () => {
    const n = parseNodes({ nodes: [{ node: "n1" }], leases: [{ node: "n1" }] });
    expect(n?.enabled).toBeNull();
    expect(n?.nodes[0]).toEqual({
      node: "n1",
      mode: null,
      encoders: null,
      waiting: null,
      coolingUntil: null,
      leasesActive: null,
    });
    expect(n?.leases[0]).toMatchObject({ path: null, state: null, outputBytes: null, endedAt: null });
    expect(n?.total).toBeNull();
  });

  it("the refusal envelope and the scan report", () => {
    expect(parseRefusal({ error: "bad", parameters: [{ parameter: "status", error: "no such status" }, 7] })).toEqual({
      error: "bad",
      parameters: [{ parameter: "status", error: "no such status" }],
    });
    expect(parseRefusal("unauthorized")).toBeNull();
    expect(parseScanReport({ results: [{ path: "/x.mkv" }] })).toMatchObject({
      accepted: null,
      rejected: null,
      results: [{ path: "/x.mkv", accepted: null, rule: null }],
    });
    expect(parseScanReport({ started: false })).toBeNull();
  });
});

describe("endpoints: what each call sends", () => {
  it("the reads are GETs of their own path, with the token as a bearer header", async () => {
    const { fetch, calls } = fakeFetch((call) => {
      switch (call.path) {
        case "/api/summary":
          return json({ summary: {} });
        case "/api/queue":
          return json({ queue: [], queue_total: total(0, 500) });
        case "/api/history":
          return json({ history: [jobRow({ status: "done" })], next_cursor: null });
        case "/api/health":
          return json({ state: "off" });
        case "/api/nodes":
          return json({ nodes: [], leases: [] });
        case "/api/exclusions":
          return json({ exclusions: [] });
        default:
          return plain("not found", 404);
      }
    });
    const opts = { fetch, token: "s3cret" };
    expect((await loadSummary(opts)).kind).toBe("ok");
    expect((await loadQueue(opts)).kind).toBe("ok");
    expect((await loadHistory({ limit: 25, statuses: ["done", "failed"], cursor: "c1" }, opts)).kind).toBe("ok");
    expect((await loadHealth(opts)).kind).toBe("ok");
    expect((await loadNodes(opts)).kind).toBe("ok");
    expect((await loadExclusions(opts)).kind).toBe("ok");
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      "GET /api/summary",
      "GET /api/queue",
      "GET /api/history?limit=25&status=done&status=failed&cursor=c1",
      "GET /api/health",
      "GET /api/nodes",
      "GET /api/exclusions",
    ]);
    for (const call of calls) {
      expect(call.headers["Authorization"]).toBe("Bearer s3cret");
      expect(call.url).not.toContain("s3cret");
    }
  });

  it("the controls are the API's own methods, paths and bodies", async () => {
    const { fetch, calls } = fakeFetch((call) => {
      if (call.path === "/api/scan") {
        return json({ accepted: 1, rejected: 0, retryable: false, results: [] }, 202);
      }
      if (call.path === "/api/rescan") {
        return json({ paused: false, reason: "", scanning: true, started: true }, 202);
      }
      if (call.path === "/api/exclusions") {
        return json({ path: "/library/films/example-a.mkv", changed: true, runtime_state: "runtime" });
      }
      return json({ paused: call.path === "/api/pause", scanning: false });
    });
    const opts = { fetch, token: "ctl" };
    expect(await pause(opts)).toEqual({ kind: "ok", value: { paused: true, scanning: false } });
    expect(await resume(opts)).toEqual({ kind: "ok", value: { paused: false, scanning: false } });
    expect(await startLibraryScan(opts)).toMatchObject({ kind: "ok", value: { started: true } });
    expect(await scanPaths(["/library/films/example-a.mkv"], opts)).toMatchObject({ kind: "ok", value: { accepted: 1 } });
    expect(await addExclusion("/library/films/example-a.mkv", opts)).toMatchObject({ kind: "ok", value: { changed: true } });
    expect(await removeExclusion("/library/films/example-a.mkv", opts)).toMatchObject({ kind: "ok" });
    expect(calls.map((c) => [c.method, c.url, c.body])).toEqual([
      ["POST", "/api/pause", null],
      ["POST", "/api/resume", null],
      ["POST", "/api/rescan", null],
      ["POST", "/api/scan", '{"paths":["/library/films/example-a.mkv"]}'],
      ["POST", "/api/exclusions", '{"path":"/library/films/example-a.mkv"}'],
      ["DELETE", "/api/exclusions", '{"path":"/library/films/example-a.mkv"}'],
    ]);
    for (const call of calls) {
      expect(call.headers["Authorization"]).toBe("Bearer ctl");
    }
  });

  it("a refusal passes through with its status and the server's words", async () => {
    const { fetch } = fakeFetch(() => plain("unauthorized", 401));
    expect(await pause({ fetch, token: "wrong" })).toEqual({ kind: "refused", status: 401, message: "unauthorized" });
    expect(await loadNodes({ fetch })).toMatchObject({ kind: "refused", status: 401 });
  });
});

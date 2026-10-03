import { describe, expect, it, vi } from "vitest";

import { getJSON, isApiPath, loadSurface, request, SCHEMA_PATH } from "./api";

function answer(body: string, init: ResponseInit = {}): typeof fetch {
  return vi.fn(() => Promise.resolve(new Response(body, init)));
}

const surface = {
  schema: "holdfast.http-surface",
  schema_version: "1",
  holdfast_version: "v9.9.9",
  endpoints: [
    { method: "GET", path: "/" },
    { method: "GET", path: "/api/summary" },
  ],
};

describe("getJSON", () => {
  it("returns the decoded document on a 200", async () => {
    const result = await getJSON<{ a: number }>("/api/x", { fetch: answer('{"a":1}') });
    expect(result).toEqual({ kind: "ok", value: { a: 1 } });
  });

  it("asks for JSON and sends no credential when it was given none", async () => {
    const fetch = answer("{}");
    await getJSON("/api/x", { fetch });
    await getJSON("/api/x", { fetch, token: "" });
    for (const call of vi.mocked(fetch).mock.calls) {
      const headers = call[1]?.headers as Record<string, string>;
      expect(headers["Accept"]).toBe("application/json");
      expect(headers).not.toHaveProperty("Authorization");
    }
  });

  it("sends a token as a bearer credential, in a header and never in the URL", async () => {
    const fetch = answer("{}");
    await getJSON("/api/x", { fetch, token: "s3cret" });
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? [];
    expect((init?.headers as Record<string, string>)["Authorization"]).toBe("Bearer s3cret");
    expect(String(url)).toBe("/api/x");
  });

  it("reports a refusal with its status and the server's own first line", async () => {
    const fetch = answer("read token required\nsecond line\n", { status: 401, statusText: "Unauthorized" });
    expect(await getJSON("/api/x", { fetch })).toEqual({
      kind: "refused",
      status: 401,
      message: "read token required",
    });
  });

  it("falls back to the status text when a refusal carries no sentence", async () => {
    const empty = answer("", { status: 503, statusText: "Service Unavailable" });
    expect(await getJSON("/api/x", { fetch: empty })).toMatchObject({ message: "Service Unavailable" });
    const page = answer("x".repeat(301), { status: 500, statusText: "Internal Server Error" });
    expect(await getJSON("/api/x", { fetch: page })).toMatchObject({ message: "Internal Server Error" });
  });

  it("reports a request that never got an answer as unreachable, and does not throw", async () => {
    const fetch = vi.fn(() => Promise.reject(new TypeError("network down")));
    expect(await getJSON("/api/x", { fetch })).toEqual({ kind: "unreachable", message: "network down" });
  });

  it("reports a 200 that is not JSON as unreachable, never as a value", async () => {
    const result = await getJSON("/api/x", { fetch: answer("<html>a proxy's page</html>") });
    expect(result.kind).toBe("unreachable");
  });
});

describe("loadSurface", () => {
  it("reads the surface document from its path", async () => {
    const fetch = answer(JSON.stringify(surface));
    expect(await loadSurface({ fetch })).toEqual({ kind: "ok", value: surface });
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toBe(SCHEMA_PATH);
  });

  it.each([
    ["some other JSON document", { status: "ok" }],
    ["a surface document of another schema", { ...surface, schema: "other" }],
    ["endpoints that are not a list", { ...surface, endpoints: 7 }],
    ["an endpoint with no path", { ...surface, endpoints: [{ method: "GET" }] }],
    ["null", null],
  ])("refuses %s rather than counting its endpoints", async (_name, body) => {
    const result = await loadSurface({ fetch: answer(JSON.stringify(body)) });
    expect(result.kind).toBe("unreachable");
  });

  it("passes a refusal through unchanged", async () => {
    const fetch = answer("nope", { status: 500, statusText: "Internal Server Error" });
    expect(await loadSurface({ fetch })).toEqual({ kind: "refused", status: 500, message: "nope" });
  });
});

describe("request", () => {
  it.each([
    "https://elsewhere.example/api/summary",
    "//elsewhere.example/api/summary",
    "/metrics",
    "/api/../metrics",
    "/api/history?cursor=x",
    "/api//summary",
    "api/summary",
    "/api/summary#frag",
  ])("token: is not sent to %s, and neither is the request", async (path) => {
    expect(isApiPath(path)).toBe(false);
    const fetch = answer("{}");
    const result = await request("GET", path, { fetch, token: "s3cret" });
    expect(result.kind).toBe("unreachable");
    expect(fetch).not.toHaveBeenCalled();
  });

  it("builds the query itself, repeating a key, and keeps the token out of the URL", async () => {
    const fetch = answer("{}");
    await request("GET", "/api/history", {
      fetch,
      token: "s3cret",
      query: [
        ["limit", "50"],
        ["status", "done"],
        ["status", "failed"],
        ["cursor", "a b&c"],
      ],
    });
    const [url, init] = vi.mocked(fetch).mock.calls[0] ?? [];
    expect(String(url)).toBe("/api/history?limit=50&status=done&status=failed&cursor=a+b%26c");
    expect(String(url)).not.toContain("s3cret");
    expect((init?.headers as Record<string, string>)["Authorization"]).toBe("Bearer s3cret");
  });

  it("sends a body as JSON with its method", async () => {
    const fetch = answer("{}");
    await request("DELETE", "/api/exclusions", { fetch, token: "t", body: { path: "/library/films/example-a.mkv" } });
    const [, init] = vi.mocked(fetch).mock.calls[0] ?? [];
    expect(init?.method).toBe("DELETE");
    expect(init?.body).toBe('{"path":"/library/films/example-a.mkv"}');
    expect((init?.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
  });

  it("keeps a JSON refusal whole, and takes its `error` as the sentence", async () => {
    const body = { rule: "paused", error: "holdfast is paused; nothing was enqueued", retryable: true, results: [] };
    const fetch = answer(JSON.stringify(body), { status: 409, statusText: "Conflict" });
    expect(await request("POST", "/api/scan", { fetch })).toEqual({
      kind: "refused",
      status: 409,
      message: "holdfast is paused; nothing was enqueued",
      body,
    });
  });

  it("falls back to the status text for a JSON refusal with no `error`", async () => {
    const fetch = answer('{"started":false,"reason":"paused"}', { status: 409, statusText: "Conflict" });
    expect(await request("POST", "/api/rescan", { fetch })).toMatchObject({
      kind: "refused",
      status: 409,
      message: "Conflict",
      body: { started: false, reason: "paused" },
    });
  });
});

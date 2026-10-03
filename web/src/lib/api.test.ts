import { describe, expect, it, vi } from "vitest";

import {
  getJSON,
  isApiPath,
  loadSurface,
  MESSAGE_WITHHELD,
  NO_ANSWER,
  REDIRECT_NOT_FOLLOWED,
  request,
  SCHEMA_PATH,
  TOKEN_NOT_SENDABLE,
} from "./api";

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
    const blank = answer(" \n", { status: 500, statusText: "Internal Server Error" });
    expect(await getJSON("/api/x", { fetch: blank })).toMatchObject({ message: "Internal Server Error" });
  });

  it("cuts a refusal sentence over 300 characters and keeps its start, rather than dropping it", async () => {
    // What the exclusions 400 can be: the path and every configured root, on one line.
    const long = `path is outside every library root: ${Array.from({ length: 30 }, (_, i) => `/library/root-${i}`).join(", ")}`;
    expect(long.length).toBeGreaterThan(300);
    // Under HTTP/2 there is no status text to fall back to.
    const result = await getJSON("/api/x", { fetch: answer(`${long}\n`, { status: 400, statusText: "" }) });
    expect(result).toEqual({ kind: "refused", status: 400, message: `${long.slice(0, 300)}...` });
    const exact = "y".repeat(300);
    expect(await getJSON("/api/x", { fetch: answer(exact, { status: 400 }) })).toMatchObject({ message: exact });
    const said = await getJSON("/api/x", { fetch: answer(JSON.stringify({ error: long }), { status: 400 }) });
    expect(said).toMatchObject({ message: `${long.slice(0, 300)}...`, body: { error: long } });
  });

  it("reports a request that never got an answer as unreachable, and does not throw", async () => {
    const fetch = vi.fn(() => Promise.reject(new TypeError("network down")));
    expect(await getJSON("/api/x", { fetch })).toEqual({
      kind: "unreachable",
      message: `network down (${NO_ANSWER})`,
    });
    expect(NO_ANSWER).toContain("a redirect, which this page does not follow");
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

  it.each(["GET", "POST", "DELETE"] as const)(
    "token: a %s follows no redirect, asks this origin only, sends no cookie and reads no cache",
    async (method) => {
      const fetch = answer("{}");
      await request(method, "/api/exclusions", { fetch, token: "s3cret" });
      const [, init] = vi.mocked(fetch).mock.calls[0] ?? [];
      expect(init?.redirect).toBe("error");
      expect(init?.mode).toBe("same-origin");
      expect(init?.credentials).toBe("omit");
      expect(init?.cache).toBe("no-store");
    },
  );

  it("token: a redirect an engine hands back anyway is no answer, and its body is not read", async () => {
    const moved = vi.fn(() => Promise.resolve(new Response(null, { status: 307, headers: { Location: "/metrics" } })));
    expect(await request("GET", "/api/summary", { fetch: moved, token: "s3cret" })).toEqual({
      kind: "unreachable",
      message: REDIRECT_NOT_FOLLOWED,
    });
    const followed = new Response('{"a":1}');
    Object.defineProperty(followed, "redirected", { value: true });
    const fetch = vi.fn(() => Promise.resolve(followed));
    expect(await request("GET", "/api/summary", { fetch, token: "s3cret" })).toEqual({
      kind: "unreachable",
      message: REDIRECT_NOT_FOLLOWED,
    });
  });

  it("token: is in exactly one request header, Authorization, with or without a body", async () => {
    const fetch = answer("{}");
    await request("GET", "/api/summary", { fetch, token: "s3cret" });
    await request("POST", "/api/exclusions", { fetch, token: "s3cret", body: { path: "/library/films/example-a.mkv" } });
    expect(vi.mocked(fetch).mock.calls).toHaveLength(2);
    for (const [, init] of vi.mocked(fetch).mock.calls) {
      const carrying = Object.entries(init?.headers as Record<string, string>).filter(([, value]) => value.includes("s3cret"));
      expect(carrying).toEqual([["Authorization", "Bearer s3cret"]]);
    }
  });

  it.each([
    ["a space inside", "two words"],
    ["a line break", "abc\ndef"],
    ["a tab", "abc\tdef"],
    ["a control character", "abc\u0000def"],
    ["a character outside ASCII", "caf\u00e9-token"],
    ["a delete character", "abc\u007f"],
  ])("token: one with %s is refused before fetch is called, in a sentence that does not repeat it", async (_name, token) => {
    const fetch = answer("{}");
    const result = await request("GET", "/api/summary", { fetch, token });
    expect(result).toEqual({ kind: "unreachable", message: TOKEN_NOT_SENDABLE });
    expect(fetch).not.toHaveBeenCalled();
    expect(TOKEN_NOT_SENDABLE).not.toContain(token);
  });

  it("token: every printable ASCII character without a space is one a request can carry", async () => {
    const token = Array.from({ length: 0x7e - 0x21 + 1 }, (_, i) => String.fromCharCode(0x21 + i)).join("");
    const fetch = answer("{}");
    expect((await request("GET", "/api/summary", { fetch, token })).kind).toBe("ok");
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("token: a thrown message that names the token is replaced by a fixed sentence", async () => {
    const naming = vi.fn(() => Promise.reject(new TypeError("Invalid header value: Bearer s3cret-synthetic")));
    const result = await request("GET", "/api/summary", { fetch: naming, token: "s3cret-synthetic" });
    expect(result).toEqual({ kind: "unreachable", message: MESSAGE_WITHHELD });
    expect(JSON.stringify(result)).not.toContain("s3cret-synthetic");
    // The same failure with no token in it keeps the engine's words.
    const plainly = vi.fn(() => Promise.reject(new TypeError("Failed to fetch")));
    expect(await request("GET", "/api/summary", { fetch: plainly, token: "s3cret-synthetic" })).toMatchObject({
      message: `Failed to fetch (${NO_ANSWER})`,
    });
  });

  it("token: a refusal or a non-JSON answer that repeats the token is not passed on", async () => {
    const token = "s3cret-synthetic";
    const echoing = answer(`no such token: ${token}`, { status: 401 });
    const text = await request("GET", "/api/summary", { fetch: echoing, token });
    expect(text).toEqual({ kind: "refused", status: 401, message: MESSAGE_WITHHELD });
    const body = { error: "refused", parameters: [{ parameter: "authorization", error: `Bearer ${token}` }] };
    const json = await request("GET", "/api/history", { fetch: answer(JSON.stringify(body), { status: 400 }), token });
    expect(json).toEqual({ kind: "refused", status: 400, message: "refused" });
    const thrown = vi.fn(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        redirected: false,
        type: "basic",
        json: () => Promise.reject(new SyntaxError(`unexpected ${token}`)),
      } as unknown as Response),
    );
    const parsed = await request("GET", "/api/summary", { fetch: thrown, token });
    expect(JSON.stringify([text, json, parsed])).not.toContain(token);
    expect(parsed).toEqual({ kind: "unreachable", message: MESSAGE_WITHHELD });
  });
});

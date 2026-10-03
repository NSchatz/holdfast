import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import App from "./App.svelte";
import type { ApiResult, SurfaceDocument } from "./lib/api";
import { fakeFetch, json, plain, total, type Call } from "./test-helpers";

const TOKEN = "tok-9f2c-synthetic";

const surface: ApiResult<SurfaceDocument> = {
  kind: "ok",
  value: { schema: "holdfast.http-surface", schema_version: "1", holdfast_version: "v9.9.9", endpoints: [] },
};

// A server with both tokens set to the one value: every /api/ path wants the header.
function server() {
  return fakeFetch((call: Call) => {
    if (call.headers["Authorization"] !== `Bearer ${TOKEN}`) {
      return plain("unauthorized", 401);
    }
    switch (`${call.method} ${call.path}`) {
      case "GET /api/summary":
        return json({ summary: { done: 1 }, paused: false, scanning: false });
      case "GET /api/queue":
        return json({ now: 1, queue: [], queue_total: total(0, 500) });
      case "GET /api/history":
        return json({ history: [], history_total: total(0, 50), next_cursor: null });
      case "GET /api/health":
        return json({ state: "off", enabled: false });
      case "GET /api/nodes":
        return json({ enabled: false, nodes: [], leases: [], leases_total: total(0, 200) });
      case "GET /api/exclusions":
        return json({ exclusions: [] });
      case "POST /api/pause":
        return json({ paused: true, scanning: false });
      default:
        return plain("not found", 404);
    }
  });
}

function watch() {
  const idb = { open: vi.fn(), deleteDatabase: vi.fn() };
  vi.stubGlobal("indexedDB", idb);
  return {
    setItem: vi.spyOn(Storage.prototype, "setItem"),
    cookie: vi.spyOn(Document.prototype, "cookie", "set"),
    pushState: vi.spyOn(History.prototype, "pushState"),
    replaceState: vi.spyOn(History.prototype, "replaceState"),
    idb,
    logs: (["log", "info", "warn", "error", "debug"] as const).map((level) =>
      vi.spyOn(console, level).mockImplementation(() => {}),
    ),
    href: location.href,
  };
}

async function start(s = server()) {
  const view = render(App, { load: () => Promise.resolve(surface), fetch: s.fetch, intervalMs: 0 });
  await screen.findByText(/Reachable/);
  return { ...view, calls: s.calls };
}

const field = () => screen.getByLabelText("Token") as HTMLInputElement;

async function enter(token: string) {
  await fireEvent.input(field(), { target: { value: token } });
  await fireEvent.click(screen.getByRole("button", { name: "Use token" }));
}

async function visitEveryView() {
  for (const name of ["Queue", "History", "Health", "Nodes", "Controls", "Summary"]) {
    await fireEvent.click(screen.getByRole("button", { name }));
    await screen.findByRole("heading", { level: 2, name: name === "Summary" ? "Summary and savings" : name });
    await waitFor(() => expect(screen.queryByText(/^Asking the server/)).toBeNull());
  }
}

describe("token", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
    sessionStorage.clear();
  });

  it("token: the field is a password input with autocomplete off, and is not inside a form", async () => {
    await start();
    expect(field().type).toBe("password");
    expect(field().getAttribute("autocomplete")).toBe("off");
    expect(field().closest("form")).toBeNull();
    expect(document.querySelector("form")).toBeNull();
  });

  it("token: without one, reads carry no credential and the page says none is held", async () => {
    const { calls } = await start();
    await screen.findByRole("alert");
    expect(calls.length).toBeGreaterThan(0);
    expect(calls.every((call) => !("Authorization" in call.headers))).toBe(true);
    expect(screen.getByTestId("token-note").textContent).toContain("No token is held.");
    expect(screen.getByRole("button", { name: "Forget token" })).toHaveProperty("disabled", true);
  });

  it("token: once entered it is sent as Authorization: Bearer, and the field is emptied", async () => {
    const { calls } = await start();
    await screen.findByRole("alert");
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    expect(calls.at(-1)?.headers["Authorization"]).toBe(`Bearer ${TOKEN}`);
    expect(field().value).toBe("");
    expect(screen.getByTestId("token-note").textContent).toContain("A token is held in this page's memory");
  });

  it("token: is never written to storage, a cookie, IndexedDB, the address, the history state or a log", async () => {
    const seen = watch();
    const { calls } = await start();
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    await visitEveryView();
    await fireEvent.click(screen.getByRole("button", { name: "Controls" }));
    await screen.findByText("The server accepted the token for its controls.");
    await fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await screen.findByText(/^The server answered: paused yes/);

    expect(seen.setItem).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(seen.cookie).not.toHaveBeenCalled();
    expect(document.cookie).toBe("");
    expect(seen.idb.open).not.toHaveBeenCalled();
    expect(seen.pushState).not.toHaveBeenCalled();
    expect(seen.replaceState).not.toHaveBeenCalled();
    expect(history.state).toBeNull();
    expect(location.href).toBe(seen.href);
    expect(location.href).not.toContain(TOKEN);
    for (const log of seen.logs) {
      expect(JSON.stringify(log.mock.calls)).not.toContain(TOKEN);
    }
    // And it was really in use throughout: every view and the control sent it.
    expect(new Set(calls.filter((c) => "Authorization" in c.headers).map((c) => c.path))).toEqual(
      new Set([
        "/api/summary",
        "/api/queue",
        "/api/history",
        "/api/health",
        "/api/nodes",
        "/api/exclusions",
        "/api/pause",
      ]),
    );
  });

  it("token: appears in no request URL and no request body, and goes only to paths under /api/", async () => {
    const { calls } = await start();
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    await visitEveryView();
    expect(calls.length).toBeGreaterThan(6);
    for (const call of calls) {
      expect(call.url).not.toContain(TOKEN);
      expect(call.body ?? "").not.toContain(TOKEN);
      expect(call.url.startsWith("/api/")).toBe(true);
    }
  });

  it("token: is not rendered anywhere on the page", async () => {
    await start();
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    await visitEveryView();
    expect(document.documentElement.outerHTML).not.toContain(TOKEN);
    expect(field().value).toBe("");
  });

  it("token: Forget drops it - later requests carry no credential and the controls are unavailable again", async () => {
    const { calls } = await start();
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    await fireEvent.click(screen.getByRole("button", { name: "Forget token" }));
    await screen.findByRole("alert");
    expect(calls.at(-1)?.headers).not.toHaveProperty("Authorization");
    expect(screen.getByTestId("token-note").textContent).toContain("No token is held.");

    await fireEvent.click(screen.getByRole("button", { name: "Controls" }));
    await waitFor(() =>
      expect(screen.getByTestId("control-gate").textContent).toContain("The controls are unavailable: no token is held."),
    );
    expect(screen.getByRole("button", { name: "Pause" })).toHaveProperty("disabled", true);
    expect(calls.at(-1)?.headers).not.toHaveProperty("Authorization");
  });

  it("token: Forget also empties what was typed and not yet used", async () => {
    await start();
    await fireEvent.input(field(), { target: { value: TOKEN } });
    await fireEvent.click(screen.getByRole("button", { name: "Forget token" }));
    expect(field().value).toBe("");
  });

  it("token: a reload asks again - a fresh page holds none", async () => {
    const first = await start();
    await enter(TOKEN);
    await screen.findByText(/^Read at /);
    first.unmount();

    const second = await start();
    await screen.findByRole("alert");
    expect(second.calls.length).toBeGreaterThan(0);
    expect(second.calls.every((call) => !("Authorization" in call.headers))).toBe(true);
    expect(field().value).toBe("");
    expect(screen.getByTestId("token-note").textContent).toContain("No token is held.");
  });

  it("token: the Enter key uses what was typed, as the button does", async () => {
    const { calls } = await start();
    await fireEvent.input(field(), { target: { value: TOKEN } });
    await fireEvent.keyDown(field(), { key: "Enter" });
    await screen.findByText(/^Read at /);
    expect(calls.at(-1)?.headers["Authorization"]).toBe(`Bearer ${TOKEN}`);
  });
});

describe("shell", () => {
  it("shows the views only once the API has answered for its surface", async () => {
    const s = server();
    render(App, { load: () => Promise.resolve({ kind: "unreachable", message: "network down" }), fetch: s.fetch });
    await screen.findByRole("alert");
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(s.calls).toHaveLength(0);
  });

  it("offers the five views and the controls, and shows one at a time", async () => {
    await start();
    const nav = screen.getByRole("navigation", { name: "Views" });
    expect(Array.from(nav.querySelectorAll("button")).map((b) => b.textContent?.trim())).toEqual([
      "Summary",
      "Queue",
      "History",
      "Health",
      "Nodes",
      "Controls",
    ]);
    expect(screen.getByRole("button", { name: "Summary" }).getAttribute("aria-current")).toBe("page");
    await fireEvent.click(screen.getByRole("button", { name: "Queue" }));
    expect(screen.getByRole("heading", { level: 2, name: "Queue" })).toBeTruthy();
    expect(screen.queryByRole("heading", { level: 2, name: "Summary and savings" })).toBeNull();
    expect(screen.getByRole("button", { name: "Queue" }).getAttribute("aria-current")).toBe("page");
  });

  it("leaving a view aborts its read in flight, and the address does not change", async () => {
    const hanging = fakeFetch(() => new Promise<Response>(() => {}));
    const href = location.href;
    render(App, { load: () => Promise.resolve(surface), fetch: hanging.fetch, intervalMs: 0 });
    await screen.findByText(/Reachable/);
    await waitFor(() => expect(hanging.calls).toHaveLength(1));
    await fireEvent.click(screen.getByRole("button", { name: "Health" }));
    expect(hanging.calls[0]?.signal?.aborted).toBe(true);
    expect(location.href).toBe(href);
  });
});

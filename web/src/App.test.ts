import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import App from "./App.svelte";
import type { ApiResult, SurfaceDocument } from "./lib/api";

const surface: SurfaceDocument = {
  schema: "holdfast.http-surface",
  schema_version: "1",
  holdfast_version: "v9.9.9",
  endpoints: [
    { method: "GET", path: "/" },
    { method: "GET", path: "/api/summary" },
    { method: "POST", path: "/api/pause" },
  ],
};

const answering = (result: ApiResult<SurfaceDocument>) => () => Promise.resolve(result);

describe("App", () => {
  it("says it is asking before the server has answered", () => {
    render(App, { load: () => new Promise<ApiResult<SurfaceDocument>>(() => {}) });
    expect(screen.getByRole("status").textContent).toContain("Asking the server");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("names the build that answered and counts its endpoints", async () => {
    render(App, { load: answering({ kind: "ok", value: surface }) });
    const status = await screen.findByText(/Reachable/);
    expect(status.textContent).toContain("3 endpoints");
    expect(status.textContent).toContain("surface version 1");
    expect(screen.getByText("v9.9.9")).toBeTruthy();
  });

  it("states a refusal as an alert, with the status and the server's sentence", async () => {
    render(App, { load: answering({ kind: "refused", status: 401, message: "read token required" }) });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("401");
    expect(alert.textContent).toContain("read token required");
    expect(screen.queryByText(/Reachable/)).toBeNull();
  });

  it("states an unreachable API as an alert and shows no version", async () => {
    render(App, { load: answering({ kind: "unreachable", message: "network down" }) });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("not reachable");
    expect(alert.textContent).toContain("network down");
    expect(screen.queryByText("v9.9.9")).toBeNull();
  });
});

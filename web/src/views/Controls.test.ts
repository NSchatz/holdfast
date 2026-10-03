import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import { cellsOf, fakeFetch, json, plain, textOf, type Call, type Route } from "../test-helpers";
import Controls from "./Controls.svelte";

const CONTROL = "ctl-token";
const NOTICE = "Withheld paths are runtime state this daemon holds.";
const DISABLED =
  "control disabled: point server_auth_token at a secret (file:/run/secrets/... or cmd:...) to enable rescan/pause/resume, the ledger search and the withheld paths - see docs/secrets.md";

// A server with a control token configured, answering as internal/server does. `over`
// answers first for a test that needs one route to say something else.
function controlServer(over: Route = () => plain("not found", 404), configured = true) {
  const withheld = [{ path: "/library/films/example-x.mkv", created_at: 1700000000 }];
  let paused = false;
  return fakeFetch((call: Call) => {
    if (!configured) {
      return plain(DISABLED, 403);
    }
    if (call.headers["Authorization"] !== `Bearer ${CONTROL}`) {
      return plain("unauthorized", 401);
    }
    const special = over(call) as Response;
    if (special.status !== 404) {
      return special;
    }
    const key = `${call.method} ${call.path}`;
    if (key === "GET /api/exclusions") {
      return json({ exclusions: withheld, runtime_state: NOTICE });
    }
    if (key === "POST /api/pause" || key === "POST /api/resume") {
      paused = key === "POST /api/pause";
      return json({ paused, scanning: false });
    }
    if (key === "POST /api/rescan") {
      return json({ paused, reason: "", scanning: true, started: true }, 202);
    }
    if (key === "POST /api/exclusions" || key === "DELETE /api/exclusions") {
      const { path } = JSON.parse(call.body ?? "{}") as { path: string };
      const at = withheld.findIndex((e) => e.path === path);
      const adding = call.method === "POST";
      const changed = adding ? at === -1 : at !== -1;
      if (adding && changed) {
        withheld.push({ path, created_at: 1700000500 });
      } else if (!adding && changed) {
        withheld.splice(at, 1);
      }
      return json({ path, changed, runtime_state: NOTICE });
    }
    return plain("not found", 404);
  });
}

async function shown(server = controlServer(), token = CONTROL) {
  const view = render(Controls, { fetch: server.fetch, token });
  if (token === CONTROL) {
    await screen.findByText("The server accepted the token for its controls.");
  }
  return { ...view, calls: server.calls };
}

const button = (name: string) => screen.getByRole("button", { name });
const actions = () => ["Pause", "Resume", "Start a library scan", "Scan these paths", "Withhold this path"].map(button);
const sent = (calls: Call[]) => calls.filter((c) => c.method !== "GET").map((c) => `${c.method} ${c.url}`);

describe("controls", () => {
  it("controls: offers exactly the controls the API has - pause, resume, library scan, named paths, exclusions", async () => {
    await shown();
    expect(screen.getAllByRole("button").map(textOf)).toEqual([
      "Pause",
      "Resume",
      "Start a library scan",
      "Scan these paths",
      "Withhold this path",
      "Refresh",
      "Stop withholding",
    ]);
  });

  it("controls: without a token every control is disabled and the page says they are unavailable", async () => {
    const { calls } = await shown(controlServer(), "");
    const gate = screen.getByTestId("control-gate");
    await waitFor(() => expect(gate.textContent).toContain("answered 401 to a request without it"));
    expect(gate.textContent).toContain("The controls are unavailable: no token is held.");
    for (const control of actions()) {
      expect(control).toHaveProperty("disabled", true);
      await fireEvent.click(control);
    }
    expect(screen.getByLabelText("Paths to scan")).toHaveProperty("disabled", true);
    expect(screen.getByLabelText("Path to withhold")).toHaveProperty("disabled", true);
    // Pressing a disabled control sends nothing.
    expect(sent(calls)).toEqual([]);
    expect(calls.every((call) => !("Authorization" in call.headers))).toBe(true);
  });

  it("controls: a 403 says the controls are off on this server, in its own words, token or no token", async () => {
    for (const token of ["", "anything"]) {
      const server = controlServer(undefined, false);
      const view = render(Controls, { fetch: server.fetch, token });
      const alert = await screen.findByRole("alert");
      expect(alert.textContent).toContain("The controls are off on this server (403)");
      expect(alert.textContent).toContain(DISABLED);
      expect(alert.textContent).toContain("server_auth_token");
      for (const control of actions()) {
        expect(control).toHaveProperty("disabled", true);
      }
      view.unmount();
    }
  });

  it("controls: a 401 says the token entered is not the control token, and the controls stay disabled", async () => {
    const server = controlServer();
    render(Controls, { fetch: server.fetch, token: "a-read-token" });
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The server refused the token entered (401): unauthorized");
    expect(alert.textContent).toContain("A read token opens the reads and never a control.");
    for (const control of actions()) {
      expect(control).toHaveProperty("disabled", true);
    }
    expect(screen.queryByText("The server accepted the token for its controls.")).toBeNull();
  });

  it("controls: pause sends POST /api/pause with the token and shows the server's answer", async () => {
    const { calls } = await shown();
    await fireEvent.click(button("Pause"));
    expect((await screen.findByText(/^The server answered: paused/)).textContent).toBe(
      "The server answered: paused yes, scanning no.",
    );
    expect(sent(calls)).toEqual(["POST /api/pause"]);
    expect(calls.at(-1)?.headers["Authorization"]).toBe(`Bearer ${CONTROL}`);
  });

  it("controls: resume sends POST /api/resume and shows the server's answer", async () => {
    const { calls } = await shown();
    await fireEvent.click(button("Resume"));
    expect((await screen.findByText(/^The server answered: paused/)).textContent).toBe(
      "The server answered: paused no, scanning no.",
    );
    expect(sent(calls)).toEqual(["POST /api/resume"]);
  });

  it("controls: pause reports what the server said, not what was asked for", async () => {
    // A server that answers 200 and still reports itself not paused.
    await shown(controlServer((call) => (call.path === "/api/pause" ? json({ scanning: false }) : plain("", 404))));
    await fireEvent.click(button("Pause"));
    expect((await screen.findByText(/^The server answered: paused/)).textContent).toBe(
      "The server answered: paused unavailable, scanning no.",
    );
  });

  it("controls: a refused pause is an alert in the server's words and claims no success", async () => {
    await shown(controlServer((call) => (call.path === "/api/pause" ? plain("unauthorized", 401) : plain("", 404))));
    await fireEvent.click(button("Pause"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The server refused (401): unauthorized");
    expect(alert.textContent).toContain("not this server's control token");
    expect(screen.queryByText(/^The server answered/)).toBeNull();
  });

  it("controls: a 403 on a control says the controls are off on this server", async () => {
    await shown(controlServer((call) => (call.path === "/api/resume" ? plain(DISABLED, 403) : plain("", 404))));
    await fireEvent.click(button("Resume"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(`The server refused (403): ${DISABLED}`);
    expect(alert.textContent).toContain("no control token is configured (server_auth_token)");
  });

  it("controls: a request that got no answer claims nothing changed", async () => {
    const inner = controlServer();
    const server = fakeFetch((call) => {
      if (call.method === "POST") {
        throw new TypeError("network down");
      }
      return inner.fetch(call.url, { method: call.method, headers: call.headers });
    });
    await shown(server);
    await fireEvent.click(button("Pause"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("No usable answer: network down");
    expect(alert.textContent).toContain("Nothing is known to have changed.");
  });

  it("controls: start a library scan sends POST /api/rescan and says a scan started only when told so", async () => {
    const { calls } = await shown();
    await fireEvent.click(button("Start a library scan"));
    expect(await screen.findByText("The server answered: a library scan started.")).toBeTruthy();
    expect(screen.getByText("Paused no, scanning yes.")).toBeTruthy();
    expect(sent(calls)).toEqual(["POST /api/rescan"]);
  });

  it("controls: a 409 on a library scan shows the server's reason and claims no scan", async () => {
    await shown(
      controlServer((call) =>
        call.path === "/api/rescan"
          ? json({ paused: true, reason: "paused", scanning: false, started: false }, 409)
          : plain("", 404),
      ),
    );
    await fireEvent.click(button("Start a library scan"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The server refused (409): no scan started: paused");
    expect(alert.textContent).toContain("Paused yes, scanning no.");
    expect(screen.queryByText("The server answered: a library scan started.")).toBeNull();
  });

  it("controls: a 2xx that does not say a scan started is not reported as one", async () => {
    await shown(controlServer((call) => (call.path === "/api/rescan" ? json({ scanning: false }, 202) : plain("", 404))));
    await fireEvent.click(button("Start a library scan"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The server answered without saying whether a scan started.");
  });

  it("controls: scan named paths sends POST /api/scan with the paths and shows the answer for each", async () => {
    const report = {
      accepted: 1,
      rejected: 1,
      retryable: false,
      results: [
        { path: "/library/films/example-a.mkv", accepted: true, resolved: "/library/films/example-a.mkv", retryable: false },
        {
          path: "/library/films/notes.txt",
          accepted: false,
          rule: "not-a-video-extension",
          retryable: false,
          detail: '"notes.txt" carries no configured video extension (video_exts: mkv, mp4)',
        },
      ],
    };
    const { calls, container } = await shown(
      controlServer((call) => (call.path === "/api/scan" ? json(report, 202) : plain("", 404))),
    );
    await fireEvent.input(screen.getByLabelText("Paths to scan"), {
      target: { value: "/library/films/example-a.mkv\n\n  /library/films/notes.txt  \n" },
    });
    await fireEvent.click(button("Scan these paths"));
    expect((await screen.findByText(/^The server answered: 1 accepted, 1 refused\./)).textContent).toContain(
      "An accepted path is queued to be looked at",
    );
    expect(calls.at(-1)?.method).toBe("POST");
    expect(calls.at(-1)?.url).toBe("/api/scan");
    expect(calls.at(-1)?.body).toBe('{"paths":["/library/films/example-a.mkv","/library/films/notes.txt"]}');
    expect(cellsOf(container, "/library/films/example-a.mkv")).toEqual([
      "accepted",
      "",
      "acts on /library/films/example-a.mkv",
    ]);
    expect(cellsOf(container, "/library/films/notes.txt")).toEqual([
      "refused",
      "not-a-video-extension",
      '"notes.txt" carries no configured video extension (video_exts: mkv, mp4)',
    ]);
  });

  it("controls: a 400 where every path was refused shows each path's rule and claims nothing accepted", async () => {
    const report = {
      accepted: 0,
      rejected: 1,
      retryable: false,
      results: [
        {
          path: "/elsewhere/example.mkv",
          accepted: false,
          rule: "outside-library-roots",
          retryable: false,
          detail: "/elsewhere/example.mkv does not lie at or beneath any configured library root (library_roots: /library)",
        },
      ],
    };
    const { container } = await shown(
      controlServer((call) => (call.path === "/api/scan" ? json(report, 400) : plain("", 404))),
    );
    await fireEvent.input(screen.getByLabelText("Paths to scan"), { target: { value: "/elsewhere/example.mkv" } });
    await fireEvent.click(button("Scan these paths"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("The server refused (400): no path was accepted.");
    expect(cellsOf(container, "/elsewhere/example.mkv").slice(0, 2)).toEqual(["refused", "outside-library-roots"]);
    expect(screen.queryByText(/^The server answered/)).toBeNull();
  });

  it("controls: a 409 on a named scan shows the server's own sentence, and that a retry may succeed", async () => {
    const refusal = {
      accepted: 0,
      rejected: 0,
      rule: "paused",
      retryable: true,
      error: "holdfast is paused; nothing was enqueued - POST /api/resume first",
      results: [],
    };
    await shown(controlServer((call) => (call.path === "/api/scan" ? json(refusal, 409) : plain("", 404))));
    await fireEvent.input(screen.getByLabelText("Paths to scan"), { target: { value: "/library/films/example-a.mkv" } });
    await fireEvent.click(button("Scan these paths"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "The server refused (409): holdfast is paused; nothing was enqueued - POST /api/resume first",
    );
    expect(alert.textContent).toContain("The server says the same request may succeed later.");
  });

  it("controls: a named scan with no path sends nothing and says so", async () => {
    const { calls } = await shown();
    await fireEvent.click(button("Scan these paths"));
    expect((await screen.findByRole("alert")).textContent).toContain("Nothing was sent");
    expect(sent(calls)).toEqual([]);
  });

  it("controls: exclusions are listed from GET /api/exclusions, with the server's notice", async () => {
    const { container, calls } = await shown();
    expect(calls[0]?.method).toBe("GET");
    expect(calls[0]?.url).toBe("/api/exclusions");
    expect(calls[0]?.headers["Authorization"]).toBe(`Bearer ${CONTROL}`);
    expect(cellsOf(container, "/library/films/example-x.mkv")[0]).toMatch(/^2023-11-1[45] /);
    expect(screen.getByText(NOTICE)).toBeTruthy();
  });

  it("controls: adding an exclusion sends POST /api/exclusions with the path, shows the answer and reads the list again", async () => {
    const { calls, container } = await shown();
    await fireEvent.input(screen.getByLabelText("Path to withhold"), {
      target: { value: " /library/films/example-y.mkv " },
    });
    await fireEvent.click(button("Withhold this path"));
    expect(await screen.findByText("The server answered: /library/films/example-y.mkv is now withheld.")).toBeTruthy();
    expect(sent(calls)).toEqual(["POST /api/exclusions"]);
    expect(calls.find((c) => c.method === "POST")?.body).toBe('{"path":"/library/films/example-y.mkv"}');
    await waitFor(() => expect(cellsOf(container, "/library/films/example-y.mkv")).toHaveLength(2));
    expect(calls.at(-1)?.method).toBe("GET");
  });

  it("controls: an exclusion the server says changed nothing is reported as nothing changed", async () => {
    await shown();
    await fireEvent.input(screen.getByLabelText("Path to withhold"), { target: { value: "/library/films/example-x.mkv" } });
    await fireEvent.click(button("Withhold this path"));
    expect(
      await screen.findByText(
        "The server answered that nothing changed: /library/films/example-x.mkv was already withheld.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText(/is now withheld/)).toBeNull();
  });

  it("controls: a refused exclusion is an alert in the server's words", async () => {
    const words =
      'nothing changed: "/elsewhere/example.mkv" is outside every configured library root (/library), so nothing this daemon scans could ever match it';
    await shown(
      controlServer((call) =>
        call.method === "POST" && call.path === "/api/exclusions" ? plain(words, 400) : plain("", 404),
      ),
    );
    await fireEvent.input(screen.getByLabelText("Path to withhold"), { target: { value: "/elsewhere/example.mkv" } });
    await fireEvent.click(button("Withhold this path"));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(`The server refused (400): ${words}`);
    expect(screen.queryByText(/is now withheld/)).toBeNull();
  });

  it("controls: removing an exclusion sends DELETE /api/exclusions with the path and shows the answer", async () => {
    const { calls, container } = await shown();
    const row = within(container).getByRole("button", { name: "Stop withholding /library/films/example-x.mkv" });
    await fireEvent.click(row);
    expect(
      await screen.findByText("The server answered: /library/films/example-x.mkv is no longer withheld."),
    ).toBeTruthy();
    expect(sent(calls)).toEqual(["DELETE /api/exclusions"]);
    expect(calls.find((c) => c.method === "DELETE")?.body).toBe('{"path":"/library/films/example-x.mkv"}');
    expect(await screen.findByText("No path is withheld.")).toBeTruthy();
  });

  it("controls: the token goes in the Authorization header of every request and in no URL or body", async () => {
    const { calls } = await shown();
    await fireEvent.click(button("Pause"));
    await screen.findByText(/^The server answered: paused/);
    await fireEvent.click(button("Start a library scan"));
    await screen.findByText("The server answered: a library scan started.");
    expect(calls.length).toBeGreaterThanOrEqual(3);
    for (const call of calls) {
      expect(call.headers["Authorization"]).toBe(`Bearer ${CONTROL}`);
      expect(call.url).not.toContain(CONTROL);
      expect(call.body ?? "").not.toContain(CONTROL);
      expect(call.path.startsWith("/api/")).toBe(true);
    }
  });
});

<script lang="ts">
  import type { ApiResult, Fetch } from "../lib/api";
  import {
    addExclusion,
    EXCLUSIONS_PATH,
    loadExclusions,
    parseScanReport,
    parseScanStart,
    pause,
    removeExclusion,
    resume,
    scanPaths,
    startLibraryScan,
    type ControlState,
    type ExclusionChange,
    type ScanReport,
    type ScanStart,
  } from "../lib/endpoints";
  import { formatTime, NOT_RECORDED, UNAVAILABLE } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";

  // The controls: exactly the mutating endpoints the API offers - pause, resume, a
  // library scan, a scan of named paths, and the withheld paths. Each shows what the
  // server answered and claims nothing it was not told. docs/design/web-ui.md#controls.
  let { token = "", fetch }: { token?: string; fetch?: Fetch } = $props();

  interface Outcome {
    good: boolean;
    lines: string[];
    report?: ScanReport;
  }

  let busy = $state(false);
  let pauseOutcome = $state.raw<Outcome | undefined>(undefined);
  let scanOutcome = $state.raw<Outcome | undefined>(undefined);
  let pathsOutcome = $state.raw<Outcome | undefined>(undefined);
  let exclusionOutcome = $state.raw<Outcome | undefined>(undefined);
  let pathsDraft = $state("");
  let exclusionDraft = $state("");

  // The withheld paths are read through the control gate, so this one read also says
  // which state the gate is in: 403 with no control token configured, 401 for a missing
  // or wrong one. Asking it without a token changes nothing on the server.
  const exclusions = createResource((signal) => loadExclusions({ token, fetch, signal }), {
    intervalMs: () => 0,
  });

  $effect(() => {
    void token;
    exclusions.clear();
    return exclusions.start();
  });

  const gate = $derived(exclusions.result);
  const refusedAt = $derived(gate?.kind === "refused" ? gate.status : null);
  // Only an answer says a token is accepted: the controls are enabled once the gate read
  // made with this token has answered 2xx, and stay disabled while it is in flight, was
  // refused, or got no answer.
  const available = $derived(token !== "" && gate?.kind === "ok");

  const yesNo = (v: boolean | null) => (v === null ? UNAVAILABLE : v ? "yes" : "no");

  function failure(result: Exclude<ApiResult<unknown>, { kind: "ok" }>): string[] {
    if (result.kind === "unreachable") {
      return [`No usable answer: ${result.message}`, "Nothing is known to have changed."];
    }
    const lines = [`The server refused (${result.status}): ${result.message}`];
    if (result.status === 403) {
      lines.push("The controls are off on this server: no control token is configured (server_auth_token).");
    } else if (result.status === 401) {
      lines.push(
        "The token entered is not this server's control token. A read token opens the reads and never a control.",
      );
    }
    return lines;
  }

  function toggled(result: ApiResult<ControlState>): Outcome {
    if (result.kind !== "ok") {
      return { good: false, lines: failure(result) };
    }
    // An answer that does not say is not a success, whatever its status.
    if (result.value.paused === null) {
      return {
        good: false,
        lines: [
          "The server's answer did not state whether it is paused, so nothing is known to have changed.",
          `Scanning ${yesNo(result.value.scanning)}.`,
        ],
      };
    }
    return {
      good: true,
      lines: [`The server answered: paused ${yesNo(result.value.paused)}, scanning ${yesNo(result.value.scanning)}.`],
    };
  }

  function scanStarted(result: ApiResult<ScanStart>): Outcome {
    if (result.kind === "ok") {
      const s = result.value;
      const state = `Paused ${yesNo(s.paused)}, scanning ${yesNo(s.scanning)}.`;
      if (s.started === true) {
        return { good: true, lines: ["The server answered: a library scan started.", state] };
      }
      return {
        good: false,
        lines: [
          s.started === false
            ? `The server answered that no scan started${s.reason === null ? "." : `: ${s.reason}`}`
            : "The server answered without saying whether a scan started.",
          state,
        ],
      };
    }
    if (result.kind === "refused") {
      const said = parseScanStart(result.body);
      if (said !== null && (said.started !== null || said.reason !== null)) {
        return {
          good: false,
          lines: [
            `The server refused (${result.status}): no scan started${said.reason === null ? "." : `: ${said.reason}`}`,
            `Paused ${yesNo(said.paused)}, scanning ${yesNo(said.scanning)}.`,
          ],
        };
      }
    }
    return { good: false, lines: failure(result) };
  }

  function pathsScanned(result: ApiResult<ScanReport>): Outcome {
    if (result.kind === "ok") {
      const r = result.value;
      return {
        good: true,
        lines: [
          `The server answered: ${r.accepted ?? "an unstated number of"} accepted, ${r.rejected ?? "an unstated number"} refused. ` +
            "An accepted path is queued to be looked at; what is decided about it is in the history.",
        ],
        report: r,
      };
    }
    if (result.kind === "refused") {
      const report = parseScanReport(result.body);
      if (report !== null) {
        const lines = [`The server refused (${result.status})${report.error === null ? "." : `: ${report.error}`}`];
        if (report.error === null && report.results.length > 0) {
          lines[0] = `The server refused (${result.status}): no path was accepted.`;
        }
        if (report.retryable === true) {
          lines.push("The server says the same request may succeed later.");
        }
        return { good: false, lines, report };
      }
    }
    return { good: false, lines: failure(result) };
  }

  function exclusionChanged(result: ApiResult<ExclusionChange>, adding: boolean): Outcome {
    if (result.kind !== "ok") {
      return { good: false, lines: failure(result) };
    }
    const c = result.value;
    const path = c.path ?? "the path";
    let line: string;
    if (c.changed === true) {
      line = adding
        ? `The server answered: ${path} is now withheld.`
        : `The server answered: ${path} is no longer withheld.`;
    } else if (c.changed === false) {
      line = adding
        ? `The server answered that nothing changed: ${path} was already withheld.`
        : `The server answered that nothing changed: ${path} was not withheld.`;
    } else {
      line = `The server answered without saying whether anything changed for ${path}.`;
    }
    return { good: c.changed !== null, lines: c.runtimeState === null ? [line] : [line, c.runtimeState] };
  }

  async function run(action: () => Promise<void>) {
    if (!available || busy) {
      return;
    }
    busy = true;
    try {
      await action();
    } finally {
      busy = false;
    }
  }

  const doPause = () =>
    run(async () => {
      pauseOutcome = toggled(await pause({ token, fetch }));
    });
  const doResume = () =>
    run(async () => {
      pauseOutcome = toggled(await resume({ token, fetch }));
    });
  const doLibraryScan = () =>
    run(async () => {
      scanOutcome = scanStarted(await startLibraryScan({ token, fetch }));
    });
  const doScanPaths = () =>
    run(async () => {
      const paths = pathsDraft
        .split("\n")
        .map((line) => line.trim())
        .filter((line) => line !== "");
      if (paths.length === 0) {
        pathsOutcome = { good: false, lines: ["Nothing was sent: name at least one path, one per line."] };
        return;
      }
      pathsOutcome = pathsScanned(await scanPaths(paths, { token, fetch }));
    });
  const doExclude = () =>
    run(async () => {
      const path = exclusionDraft.trim();
      if (path === "") {
        exclusionOutcome = { good: false, lines: ["Nothing was sent: name a path."] };
        return;
      }
      exclusionOutcome = exclusionChanged(await addExclusion(path, { token, fetch }), true);
      if (exclusionOutcome.good) {
        exclusionDraft = "";
      }
      exclusions.refresh();
    });
  const doInclude = (path: string) =>
    run(async () => {
      exclusionOutcome = exclusionChanged(await removeExclusion(path, { token, fetch }), false);
      exclusions.refresh();
    });
</script>

{#snippet answer(o: Outcome | undefined)}
  {#if o !== undefined}
    <div role={o.good ? "status" : "alert"} class={o.good ? "ok" : "bad"}>
      {#each o.lines as line, i (i)}
        <p>{line}</p>
      {/each}
    </div>
    {#if o.report !== undefined && o.report.results.length > 0}
      <div class="scroll">
        <table>
          <caption>What the server said about each path.</caption>
          <thead>
            <tr>
              <th scope="col">Path</th>
              <th scope="col">Answer</th>
              <th scope="col">Rule</th>
              <th scope="col">Detail</th>
            </tr>
          </thead>
          <tbody>
            {#each o.report.results as r, i (i)}
              <tr>
                <th scope="row" class="path">{r.path ?? NOT_RECORDED}</th>
                <td>{r.accepted === null ? UNAVAILABLE : r.accepted ? "accepted" : "refused"}</td>
                <td>{r.rule ?? ""}</td>
                <td class="reason">
                  {r.detail ?? (r.resolved === null ? "" : `acts on ${r.resolved}`)}{r.retryable === true
                    ? " (may succeed if sent again)"
                    : ""}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  {/if}
{/snippet}

<section aria-labelledby="controls-heading">
  <h2 id="controls-heading">Controls</h2>

  <div data-testid="control-gate">
    {#if refusedAt === 403}
      <div role="alert" class="bad">
        <p>The controls are off on this server (403): {gate?.kind === "refused" ? gate.message : ""}</p>
        <p>
          No token turns them on from here: the operator configures server_auth_token, and until then
          the server refuses every control for every caller.
        </p>
      </div>
    {:else if token === ""}
      <p role="status">
        The controls are unavailable: no token is held. Enter this server's control token above.
        {#if refusedAt === 401}
          The server has one configured (it answered 401 to a request without it).
        {/if}
      </p>
    {:else if refusedAt === 401}
      <div role="alert" class="bad">
        <p>The server refused the token entered (401): {gate?.kind === "refused" ? gate.message : ""}</p>
        <p>
          It is not this server's control token. A read token opens the reads and never a control.
        </p>
      </div>
    {:else if gate === undefined}
      <p role="status">
        Asking the server whether it accepts the token for its controls. They are disabled until it
        answers.
      </p>
    {:else if gate.kind === "ok"}
      <p role="status" class="ok">The server accepted the token for its controls.</p>
    {:else if gate.kind === "refused"}
      <div role="alert" class="bad">
        <p>The server refused the read of {EXCLUSIONS_PATH}: {gate.status} {gate.message}</p>
        <p>
          The controls are disabled: that read is how the page learns the server accepts this token,
          and it has not said so.
        </p>
        <p><button type="button" onclick={exclusions.refresh}>Ask again</button></p>
      </div>
    {:else}
      <div role="alert" class="bad">
        <p>{EXCLUSIONS_PATH} could not be read: {gate.message}</p>
        <p>
          The controls are disabled: that read is how the page learns the server accepts this token,
          and it got no answer.
        </p>
        <p><button type="button" onclick={exclusions.refresh}>Ask again</button></p>
      </div>
    {/if}
  </div>

  <section aria-labelledby="pause-heading">
    <h3 id="pause-heading">Pause and resume</h3>
    <p class="muted">
      Pausing stops new files being fed to the pipeline. An encode already running finishes safely.
    </p>
    <p>
      <button type="button" disabled={!available || busy} onclick={doPause}>Pause</button>
      <button type="button" disabled={!available || busy} onclick={doResume}>Resume</button>
    </p>
    {@render answer(pauseOutcome)}
  </section>

  <section aria-labelledby="scan-heading">
    <h3 id="scan-heading">Library scan</h3>
    <p class="muted">Looks at every library root again. The server refuses while paused, while a scan runs, and outside the run window.</p>
    <p><button type="button" disabled={!available || busy} onclick={doLibraryScan}>Start a library scan</button></p>
    {@render answer(scanOutcome)}
  </section>

  <section aria-labelledby="paths-heading">
    <h3 id="paths-heading">Scan named paths</h3>
    <p class="muted">
      Looks at exactly these files now, through every guard a library scan applies. One absolute
      path per line, at most 256.
    </p>
    <label for="scan-paths">Paths to scan</label>
    <textarea id="scan-paths" rows="4" spellcheck="false" bind:value={pathsDraft} disabled={!available}></textarea>
    <p><button type="button" disabled={!available || busy} onclick={doScanPaths}>Scan these paths</button></p>
    {@render answer(pathsOutcome)}
  </section>

  <section aria-labelledby="exclusions-heading">
    <h3 id="exclusions-heading">Withheld paths (exclusions)</h3>
    <p class="muted">
      A withheld path is kept out of the pipeline. It is runtime state the server holds, never a
      configuration key; withholding only ever takes a file out.
    </p>
    <label for="exclusion-path">Path to withhold</label>
    <input id="exclusion-path" type="text" spellcheck="false" autocomplete="off" bind:value={exclusionDraft} disabled={!available} />
    <button type="button" disabled={!available || busy} onclick={doExclude}>Withhold this path</button>
    {@render answer(exclusionOutcome)}

    {#if gate?.kind === "ok"}
      {@const list = gate.value}
      <p>Read at {formatTime(exclusions.readAt)}. <button type="button" onclick={exclusions.refresh}>Refresh</button></p>
      {#if list.runtimeState !== null}
        <p class="muted">{list.runtimeState}</p>
      {/if}
      <div class="scroll">
        <table aria-labelledby="exclusions-heading">
          <thead>
            <tr><th scope="col">Path</th><th scope="col">Withheld since</th><th scope="col">Action</th></tr>
          </thead>
          <tbody>
            {#each list.paths as entry, i (i)}
              <tr>
                <th scope="row" class="path">{entry.path}</th>
                <td>{formatTime(entry.createdAt, NOT_RECORDED)}</td>
                <td>
                  <button
                    type="button"
                    disabled={!available || busy}
                    aria-label={`Stop withholding ${entry.path}`}
                    onclick={() => doInclude(entry.path)}
                  >
                    Stop withholding
                  </button>
                </td>
              </tr>
            {:else}
              <tr><td colspan="3" class="muted">No path is withheld.</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else}
      <p class="muted">The withheld paths are listed once the server accepts a control token.</p>
    {/if}
  </section>
</section>

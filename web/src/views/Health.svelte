<script lang="ts">
  import type { Fetch } from "../lib/api";
  import { HEALTH_PATH, loadHealth, type Sweep } from "../lib/endpoints";
  import { formatCount, formatTime, NOT_RECORDED, UNAVAILABLE } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";
  import Bytes from "./Bytes.svelte";
  import ReadState from "./ReadState.svelte";

  // The health sweep's report: GET /api/health. Report only. The sweep moves, renames,
  // deletes and repairs nothing, no endpoint acts on a finding, and neither does this
  // view: it has no control.
  let { token = "", fetch, intervalMs }: { token?: string; fetch?: Fetch; intervalMs?: number } = $props();

  const health = createResource((signal) => loadHealth({ token, fetch, signal }), {
    intervalMs: () => intervalMs,
  });

  $effect(() => {
    void token;
    return health.start();
  });

  const STATES: Record<string, string> = {
    off: "no sweep is configured",
    idle: "no sweep is due",
    running: "a sweep is running",
    waiting: "a sweep is due or under way, and no new decode may start now",
  };
</script>

{#snippet sweepReport(title: string, id: string, s: Sweep, running: boolean)}
  <section aria-labelledby={id} data-testid={id}>
    <h3 {id}>{title}</h3>
    <dl class="facts">
      <div><dt>Sweep</dt><dd>{formatCount(s.id, NOT_RECORDED)}</dd></div>
      <div><dt>Started</dt><dd>{formatTime(s.startedAt, NOT_RECORDED)}</dd></div>
      <div>
        <dt>Finished</dt>
        <dd>{s.finishedAt === null ? (running ? "not yet" : NOT_RECORDED) : formatTime(s.finishedAt)}</dd>
      </div>
      <div><dt>Files checked{running ? " so far" : ""}</dt><dd>{formatCount(s.checked)}</dd></div>
      <div><dt>Sound</dt><dd>{formatCount(s.ok)}</dd></div>
      <div><dt>Corrupt</dt><dd>{formatCount(s.corrupt)}</dd></div>
      <div><dt>Unreadable</dt><dd>{formatCount(s.unreadable)}</dd></div>
    </dl>
    {#if s.problems.length === 0}
      <p>This sweep lists no corrupt or unreadable file.</p>
    {:else}
      {#if s.problemsTruncated === true}
        <p class="missing">The server cut this list short: it found more than the {s.problems.length} files listed.</p>
      {:else if s.problemsTruncated === null}
        <p class="missing">The server did not say whether this list is complete.</p>
      {/if}
      <div class="scroll">
        <table>
          <caption>Files found corrupt or unreadable. Nothing here acts on them.</caption>
          <thead>
            <tr>
              <th scope="col">Path</th>
              <th scope="col">Result</th>
              <th scope="col">Reason</th>
              <th scope="col" class="num">Size</th>
              <th scope="col">Checked</th>
            </tr>
          </thead>
          <tbody>
            {#each s.problems as problem, i (i)}
              <tr>
                <th scope="row" class="path">{problem.path ?? NOT_RECORDED}</th>
                <td>{problem.result ?? NOT_RECORDED}</td>
                <td class="reason">{problem.reason ?? ""}</td>
                <td class="num"><Bytes value={problem.size} missing={NOT_RECORDED} /></td>
                <td>{formatTime(problem.checkedAt, NOT_RECORDED)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
{/snippet}

<section aria-labelledby="health-heading">
  <h2 id="health-heading">Health</h2>
  <ReadState
    result={health.result}
    readAt={health.readAt}
    path={HEALTH_PATH}
    what="its health report"
    hasToken={token !== ""}
    onrefresh={health.refresh}
  />

  {#if health.result?.kind === "ok"}
    {@const h = health.result.value}
    <dl class="facts">
      <div>
        <dt>State</dt>
        <dd>{h.state}{STATES[h.state] === undefined ? "" : `: ${STATES[h.state]}`}</dd>
      </div>
      {#if h.waiting !== null}
        <div><dt>Waiting on</dt><dd>{h.waiting}</dd></div>
      {/if}
      <div>
        <dt>Scheduled</dt>
        <dd>
          {#if h.enabled === null}
            {UNAVAILABLE}
          {:else if h.enabled}
            every {formatCount(h.intervalHours)} hours
          {:else}
            no (health_sweep_interval_hours is off)
          {/if}
        </dd>
      </div>
      <div><dt>Next due</dt><dd>{formatTime(h.nextDueAt, "no time set")}</dd></div>
    </dl>

    {#if h.current === null}
      <p>No sweep is under way.</p>
    {:else}
      {@render sweepReport("The sweep under way", "health-current", h.current, true)}
    {/if}
    {#if h.lastCompleted === null}
      <p>No sweep has run to the end yet.</p>
    {:else}
      {@render sweepReport("The last finished sweep", "health-last", h.lastCompleted, false)}
    {/if}
    <p class="muted">
      This is a report. The sweep never moves, renames, deletes or repairs a file, and nothing on
      this page does.
    </p>
  {/if}
</section>

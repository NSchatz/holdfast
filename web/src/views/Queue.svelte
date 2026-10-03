<script lang="ts">
  import type { Fetch } from "../lib/api";
  import { loadQueue, QUEUE_PATH } from "../lib/endpoints";
  import {
    formatCount,
    formatDimensions,
    formatDuration,
    formatFraction,
    NOT_RECORDED,
    UNAVAILABLE,
  } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";
  import Bytes from "./Bytes.svelte";
  import ReadState from "./ReadState.svelte";

  // The queue: the rows of GET /api/queue, in the order the server gave them. Priority
  // is shown and is not a control: nothing on the API sets it.
  let { token = "", fetch, intervalMs }: { token?: string; fetch?: Fetch; intervalMs?: number } = $props();

  const queue = createResource((signal) => loadQueue({ token, fetch, signal }), {
    intervalMs: () => intervalMs,
  });

  $effect(() => {
    void token;
    return queue.start();
  });

  // How long a row has been in its state, from the server's own two clocks.
  const inState = (now: number | null, updatedAt: number | null) =>
    now === null || updatedAt === null ? null : Math.max(0, now - updatedAt);
</script>

<section aria-labelledby="queue-heading">
  <h2 id="queue-heading">Queue</h2>
  <ReadState
    result={queue.result}
    readAt={queue.readAt}
    path={QUEUE_PATH}
    what="its queue"
    hasToken={token !== ""}
    onrefresh={queue.refresh}
  />

  {#if queue.result?.kind === "ok"}
    {@const q = queue.result.value}
    <p data-testid="queue-total">
      {#if q.total !== null && q.total.count !== null}
        Showing {q.rows.length} of {q.total.count} pending and active jobs{q.total.cap === null
          ? ""
          : ` (the server sends at most ${q.total.cap})`}.
      {:else}
        Showing {q.rows.length} pending and active jobs. The total behind them is unavailable{q.total
          ?.unavailable
          ? `: ${q.total.unavailable}`
          : ""}.
      {/if}
    </p>
    <div class="scroll">
      <table aria-labelledby="queue-heading">
        <thead>
          <tr>
            <th scope="col">Status</th>
            <th scope="col">Path</th>
            <th scope="col" class="num">Priority</th>
            <th scope="col" class="num">Source size</th>
            <th scope="col">Codec</th>
            <th scope="col">Dimensions</th>
            <th scope="col">Library root</th>
            <th scope="col">Progress</th>
            <th scope="col" class="num">In this state</th>
            <th scope="col">Worker</th>
            <th scope="col" class="num">Failures</th>
          </tr>
        </thead>
        <tbody>
          {#each q.rows as row (row.path)}
            <tr>
              <td>{row.status}</td>
              <th scope="row" class="path">{row.path}</th>
              <td class="num">{formatCount(row.priority, UNAVAILABLE)}</td>
              <td class="num"><Bytes value={row.sourceBytes} missing={NOT_RECORDED} /></td>
              <td>{row.sourceCodec ?? NOT_RECORDED}</td>
              <td>{formatDimensions(row.sourceWidth, row.sourceHeight)}</td>
              <td class="path">{row.libraryRoot ?? NOT_RECORDED}</td>
              <td>
                {#if row.progressFraction !== null}
                  <progress max="1" value={Math.min(1, Math.max(0, row.progressFraction))}></progress>
                  {formatFraction(row.progressFraction)}
                  {#if row.progressSeconds !== null && row.progressDurationSeconds !== null}
                    <span class="muted">
                      ({formatDuration(row.progressSeconds)} of {formatDuration(row.progressDurationSeconds)})
                    </span>
                  {/if}
                {:else if row.status === "encoding"}
                  <span class="missing">not reported yet</span>
                {:else}
                  <span class="muted">none reported in this state</span>
                {/if}
              </td>
              <td class="num">{formatDuration(inState(q.now, row.updatedAt))}</td>
              <td>{row.worker ?? ""}</td>
              <td class="num">{formatCount(row.failCount)}</td>
            </tr>
          {:else}
            <tr><td colspan="11" class="muted">The queue is empty.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

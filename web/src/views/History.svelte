<script lang="ts">
  import type { Fetch } from "../lib/api";
  import { HISTORY_PATH, loadHistory, TERMINAL_STATUSES } from "../lib/endpoints";
  import {
    ageNote,
    formatCount,
    formatDimensions,
    formatFraction,
    formatMillis,
    formatNumber,
    formatTime,
    NOT_RECORDED,
    saving,
    totalBehind,
    UNAVAILABLE,
  } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";
  import Bytes from "./Bytes.svelte";
  import ReadState from "./ReadState.svelte";

  // The history: GET /api/history, filtered and paged by the server. This view filters
  // nothing itself and never reorders: a page is what the server sent for the cursor it
  // gave, and the total is the server's count of the filtered set.
  let { token = "", fetch, intervalMs }: { token?: string; fetch?: Fetch; intervalMs?: number } = $props();

  const PAGE_SIZES = [25, 50, 100, 200] as const;

  let statuses = $state<string[]>([]);
  let limit = $state<number>(50);
  let cursor = $state<string | null>(null);
  let page = $state(1);

  const history = createResource(
    (signal) => loadHistory({ limit, statuses, cursor }, { token, fetch, signal }),
    { intervalMs: () => intervalMs },
  );

  $effect(() => {
    void token;
    return history.start();
  });

  function ask(next: string | null, nextPage: number) {
    cursor = next;
    page = nextPage;
    history.clear();
    history.refresh();
  }

  // Only ever to a cursor the server gave: a null one is the last page.
  function nextPage() {
    const shown = history.result;
    if (shown?.kind === "ok" && shown.value.nextCursor !== null) {
      ask(shown.value.nextCursor, page + 1);
    }
  }

  function toggle(status: string, on: boolean) {
    statuses = TERMINAL_STATUSES.filter((s) => (s === status ? on : statuses.includes(s)));
    ask(null, 1);
  }

  function resize(value: string) {
    const n = Number(value);
    if (PAGE_SIZES.some((size) => size === n)) {
      limit = n;
      ask(null, 1);
    }
  }
</script>

<section aria-labelledby="history-heading">
  <h2 id="history-heading">History</h2>

  <div class="filters">
    <fieldset>
      <legend>Status (none ticked shows every terminal status)</legend>
      {#each TERMINAL_STATUSES as status (status)}
        <label>
          <input
            type="checkbox"
            checked={statuses.includes(status)}
            onchange={(e) => toggle(status, e.currentTarget.checked)}
          />
          {status}
        </label>
      {/each}
    </fieldset>
    <label>
      Rows per page
      <select value={String(limit)} onchange={(e) => resize(e.currentTarget.value)}>
        {#each PAGE_SIZES as size (size)}
          <option value={String(size)}>{size}</option>
        {/each}
      </select>
    </label>
  </div>

  <ReadState
    result={history.result}
    readAt={history.readAt}
    path={HISTORY_PATH}
    what="its history"
    hasToken={token !== ""}
    onrefresh={history.refresh}
  />

  {#if history.result?.kind === "refused" && page > 1}
    <p><button type="button" onclick={() => ask(null, 1)}>First page</button></p>
  {/if}

  {#if history.result?.kind === "ok"}
    {@const h = history.result.value}
    <p data-testid="history-total">
      Page {page}: {h.rows.length} rows.
      {#if h.total !== null && h.total.count !== null && h.rows.length > h.total.count}
        {totalBehind(h.total.count, h.total.ageSeconds)}
      {:else if h.total !== null && h.total.count !== null}
        {@const age = ageNote(h.total.ageSeconds)}
        The ledger holds {h.total.count} rows{statuses.length > 0 ? " matching this filter" : ""}{h
          .total.covers === null
          ? ""
          : ` (${h.total.covers})`}{age === "" ? "" : `, total ${age}`}.
      {:else}
        The total behind them is unavailable{h.total?.unavailable ? `: ${h.total.unavailable}` : ""}.
      {/if}
    </p>
    {#if !h.pages}
      <p class="missing" data-testid="history-unpaged">
        This server answered without a page cursor: it predates filtering and paging, so these are
        its most recent rows of every terminal status, whatever is ticked above.
      </p>
    {/if}
    <p class="pager">
      <button type="button" disabled={page === 1} onclick={() => ask(null, 1)}>First page</button>
      <button type="button" disabled={h.nextCursor === null} onclick={nextPage}>
        Next page
      </button>
      {#if h.pages && h.nextCursor === null}
        <span class="muted">This is the last page.</span>
      {/if}
    </p>
    <div class="scroll">
      <table aria-labelledby="history-heading">
        <thead>
          <tr>
            <th scope="col">Status</th>
            <th scope="col">Path</th>
            <th scope="col">Reason</th>
            <th scope="col" class="num">Source size</th>
            <th scope="col" class="num">Output size</th>
            <th scope="col" class="num">Saved</th>
            <th scope="col">Source codec</th>
            <th scope="col">Source dimensions</th>
            <th scope="col">Encoder</th>
            <th scope="col" class="num">VMAF mean</th>
            <th scope="col" class="num">VMAF worst frame</th>
            <th scope="col" class="num">Encode time</th>
            <th scope="col" class="num">Priority</th>
            <th scope="col">Library root</th>
            <th scope="col">Recorded</th>
          </tr>
        </thead>
        <tbody>
          {#each h.rows as row, i (i)}
            {@const saved = saving(row.sourceBytes, row.outputBytes)}
            <tr>
              <td>{row.status}</td>
              <th scope="row" class="path">{row.path}</th>
              <td class="reason">{row.reason ?? ""}</td>
              <td class="num"><Bytes value={row.sourceBytes} missing={NOT_RECORDED} /></td>
              <td class="num"><Bytes value={row.outputBytes} missing={NOT_RECORDED} /></td>
              <td class="num">
                {#if saved === null}
                  <span class="missing">{NOT_RECORDED}</span>
                {:else}
                  <Bytes value={saved.bytes} /> ({formatFraction(saved.fraction)})
                {/if}
              </td>
              <td>{row.sourceCodec ?? NOT_RECORDED}</td>
              <td>{formatDimensions(row.sourceWidth, row.sourceHeight)}</td>
              <td>{row.encoder ?? NOT_RECORDED}</td>
              <td class="num">{formatNumber(row.vmafMean, 2, NOT_RECORDED)}</td>
              <td class="num">{formatNumber(row.vmafMin, 2, NOT_RECORDED)}</td>
              <td class="num">{formatMillis(row.encodeMs, NOT_RECORDED)}</td>
              <td class="num">{formatCount(row.priority, UNAVAILABLE)}</td>
              <td class="path">{row.libraryRoot ?? NOT_RECORDED}</td>
              <td>{formatTime(row.updatedAt, NOT_RECORDED)}</td>
            </tr>
          {:else}
            <tr><td colspan="15" class="muted">No row on this page.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

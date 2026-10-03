<script lang="ts">
  import type { Fetch } from "../lib/api";
  import { loadSummary, SUMMARY_PATH, type Breakdown, type RootFigures, type Spread } from "../lib/endpoints";
  import { ageNote, formatCount, formatMillis, formatNumber, UNAVAILABLE } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";
  import Bytes from "./Bytes.svelte";
  import ReadState from "./ReadState.svelte";

  // Summary and savings: GET /api/summary and nothing else. Every figure is the server's;
  // this view adds none of its own and sums nothing.
  let { token = "", fetch, intervalMs }: { token?: string; fetch?: Fetch; intervalMs?: number } = $props();

  const summary = createResource((signal) => loadSummary({ token, fetch, signal }), {
    intervalMs: () => intervalMs,
  });

  $effect(() => {
    void token;
    return summary.start();
  });

  const yesNo = (v: boolean | null) => (v === null ? UNAVAILABLE : v ? "yes" : "no");
</script>

{#snippet rootRow(label: string, r: RootFigures, whole: boolean)}
  <tr>
    <th scope="row" class="path">{label}</th>
    <td class="num">{formatCount(r.candidateFiles)}</td>
    <td class="num"><Bytes value={r.candidateBytes} /></td>
    <td class="num">{formatCount(r.candidateExcluded)}</td>
    {#if whole}
      <td class="num">{formatCount(r.projectionBasisFiles)}</td>
      <td class="num"><Bytes value={r.projectedSavingsBytes} /></td>
    {:else}
      <td class="num muted">not applicable</td>
      <td class="num muted">not applicable</td>
    {/if}
    <td class="num"><Bytes value={r.heldByUndoWindow} /></td>
    {#if whole}
      <td class="num"><Bytes value={r.freeBytes} /></td>
    {:else}
      <td class="num muted">not applicable</td>
    {/if}
  </tr>
{/snippet}

{#snippet breakdownTable(title: string, keyHeading: string, b: Breakdown | null)}
  <section class="figure">
    <h4>{title}</h4>
    {#if b === null}
      <p class="missing">Not reported by this server.</p>
    {:else if !b.available}
      <p class="missing">Unavailable: {b.unavailable ?? "the server gave no reason"}</p>
    {:else}
      <table>
        <caption>
          Over {b.covers ?? "a set the server did not name"}{b.window === null ? "" : ` (${b.window})`}:
          {formatCount(b.counted)} counted, {formatCount(b.excluded)} left out{ageNote(b.ageSeconds) === ""
            ? ""
            : `, ${ageNote(b.ageSeconds)}`}.
        </caption>
        <thead>
          <tr><th scope="col">{keyHeading}</th><th scope="col" class="num">Rows</th></tr>
        </thead>
        <tbody>
          {#each b.buckets as bucket, i (i)}
            <tr><th scope="row">{bucket.key}</th><td class="num">{formatCount(bucket.count)}</td></tr>
          {:else}
            <tr><td colspan="2" class="muted">No rows.</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </section>
{/snippet}

{#snippet spreadRow(title: string, s: Spread | null, write: (n: number | null) => string)}
  <tr>
    <th scope="row">{title}</th>
    {#if s === null}
      <td colspan="6" class="missing">Not reported by this server.</td>
    {:else if !s.available}
      <td colspan="6" class="missing">Unavailable: {s.unavailable ?? "the server gave no reason"}</td>
    {:else}
      <td class="num">{write(s.min)}</td>
      <td class="num">{write(s.mean)}</td>
      <td class="num">{write(s.max)}</td>
      <td class="num">{formatCount(s.counted)}</td>
      <td class="num">{formatCount(s.excluded)}</td>
      <td>
        {s.covers ?? "a set the server did not name"}{s.window === null ? "" : ` (${s.window})`}{ageNote(
          s.ageSeconds,
        ) === ""
          ? ""
          : `, ${ageNote(s.ageSeconds)}`}
      </td>
    {/if}
  </tr>
{/snippet}

<section aria-labelledby="summary-heading">
  <h2 id="summary-heading">Summary and savings</h2>
  <ReadState
    result={summary.result}
    readAt={summary.readAt}
    path={SUMMARY_PATH}
    what="its summary"
    hasToken={token !== ""}
    onrefresh={summary.refresh}
  />

  {#if summary.result?.kind === "ok"}
    {@const s = summary.result.value}
    <dl class="facts">
      <div><dt>Paused</dt><dd>{yesNo(s.paused)}</dd></div>
      <div><dt>Scanning</dt><dd>{yesNo(s.scanning)}</dd></div>
      <div><dt>Reclaimed, lifetime</dt><dd><Bytes value={s.reclaimedLifetime} /></dd></div>
      <div><dt>Reclaimed, this run</dt><dd><Bytes value={s.reclaimedSession} /></dd></div>
      <div><dt>Held by the undo window</dt><dd><Bytes value={s.heldByUndoWindow} /></dd></div>
    </dl>

    <h3 id="status-counts-heading">Jobs by status</h3>
    <table aria-labelledby="status-counts-heading">
      <thead>
        <tr><th scope="col">Status</th><th scope="col" class="num">Jobs</th></tr>
      </thead>
      <tbody>
        {#each s.counts as row, i (i)}
          <tr><th scope="row">{row.status}</th><td class="num">{formatCount(row.count)}</td></tr>
        {:else}
          <tr><td colspan="2" class="muted">The ledger holds no job.</td></tr>
        {/each}
      </tbody>
    </table>

    <h3 id="roots-heading">Library roots</h3>
    {#if s.roots === null}
      <p class="missing">This server's summary carries no per-root figures.</p>
    {:else}
      <div class="scroll">
        <table aria-labelledby="roots-heading">
          <thead>
            <tr>
              <th scope="col">Root</th>
              <th scope="col" class="num">Candidate files</th>
              <th scope="col" class="num">Candidate bytes</th>
              <th scope="col" class="num">Excluded</th>
              <th scope="col" class="num">Projection basis (files)</th>
              <th scope="col" class="num">Projected savings</th>
              <th scope="col" class="num">Held by the undo window</th>
              <th scope="col" class="num">Free space</th>
            </tr>
          </thead>
          <tbody>
            {#each s.roots as r, i (i)}
              {@render rootRow(r.root ?? "a root the server did not name", r, true)}
            {:else}
              <tr><td colspan="8" class="muted">No library root is configured.</td></tr>
            {/each}
            {#if s.unattributed !== null}
              {@render rootRow("Under no configured root", s.unattributed, false)}
            {/if}
          </tbody>
        </table>
      </div>
    {/if}

    <h3>Whole-ledger figures</h3>
    {#if s.aggregates === null}
      <p class="missing">This server's summary carries no whole-ledger figures.</p>
    {:else}
      <div class="figures">
        {@render breakdownTable("Outcomes", "Status", s.aggregates.outcomes)}
        {@render breakdownTable("Skips by guard", "Guard", s.aggregates.skipsByGuard)}
      </div>
      <div class="scroll">
        <table>
          <caption>Spreads across files: lowest, mean and highest.</caption>
          <thead>
            <tr>
              <th scope="col">Figure</th>
              <th scope="col" class="num">Low</th>
              <th scope="col" class="num">Mean</th>
              <th scope="col" class="num">High</th>
              <th scope="col" class="num">Counted</th>
              <th scope="col" class="num">Left out</th>
              <th scope="col">Over</th>
            </tr>
          </thead>
          <tbody>
            {@render spreadRow("Output size as a share of the source", s.aggregates.sizeRatio, (n) => formatNumber(n, 3, "no data"))}
            {@render spreadRow("Encode time", s.aggregates.encodeMs, (n) => formatMillis(n, "no data"))}
            {@render spreadRow("VMAF mean", s.aggregates.vmafMean, (n) => formatNumber(n, 2, "no data"))}
            {@render spreadRow("VMAF worst frame", s.aggregates.vmafMin, (n) => formatNumber(n, 2, "no data"))}
          </tbody>
        </table>
      </div>
    {/if}
  {/if}
</section>

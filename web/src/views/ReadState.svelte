<script lang="ts">
  import type { ApiResult } from "../lib/api";
  import { parseRefusal } from "../lib/endpoints";
  import { formatTime } from "../lib/format";

  // What every view says about the read behind it: that it is asking, when what is shown
  // was read, or why there is nothing to show - in the server's own words where it gave
  // any.
  let {
    result,
    readAt,
    path,
    what,
    hasToken,
    onrefresh,
  }: {
    result: ApiResult<unknown> | undefined;
    readAt: number | null;
    path: string;
    what: string;
    hasToken: boolean;
    onrefresh: () => void;
  } = $props();

  const refusal = $derived(result?.kind === "refused" ? parseRefusal(result.body) : null);
</script>

<div class="read-state">
  {#if result === undefined}
    <p role="status">Asking the server for {what}.</p>
  {:else if result.kind === "ok"}
    <p role="status">Read at {formatTime(readAt)}.</p>
  {:else if result.kind === "refused"}
    <div role="alert" class="bad">
      {#if result.status === 404}
        <p>
          This server does not serve {path} (404): it predates this view, so there is nothing to show
          here.
        </p>
      {:else if result.status === 401}
        <p>The server refused the read of {path}: 401 {result.message}.</p>
        <p>
          {#if hasToken}
            The token entered is not one this server accepts for its reads.
          {:else}
            This server asks for a token on its reads (server_read_token is set): enter its read token
            or its control token above.
          {/if}
        </p>
      {:else}
        <p>The server refused the read of {path}: {result.status} {result.message}</p>
      {/if}
      {#if refusal !== null && refusal.parameters.length > 0}
        <ul>
          {#each refusal.parameters as parameter, i (i)}
            <li>{parameter.parameter ?? "a parameter"}: {parameter.error ?? "refused, with no reason given"}</li>
          {/each}
        </ul>
      {/if}
      <p class="muted">Asked at {formatTime(readAt)}.</p>
    </div>
  {:else}
    <div role="alert" class="bad">
      <p>{path} could not be read: {result.message}</p>
      <p class="muted">Asked at {formatTime(readAt)}.</p>
    </div>
  {/if}
  <button type="button" onclick={onrefresh}>Refresh</button>
</div>

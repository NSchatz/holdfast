<script lang="ts">
  import { loadSurface, type ApiResult, type Fetch, type SurfaceDocument } from "./lib/api";
  import { createTokenHolder } from "./lib/token";
  import Controls from "./views/Controls.svelte";
  import Health from "./views/Health.svelte";
  import History from "./views/History.svelte";
  import Nodes from "./views/Nodes.svelte";
  import Queue from "./views/Queue.svelte";
  import Summary from "./views/Summary.svelte";
  import TokenField from "./views/TokenField.svelte";

  // The shell: it states which holdfast answered and that its API is reachable, holds the
  // token, and shows one view at a time once the API has answered. Which view is shown is
  // state of the running page and is not written to the address.
  let {
    load = loadSurface,
    fetch,
    intervalMs,
  }: {
    load?: () => Promise<ApiResult<SurfaceDocument>>;
    fetch?: Fetch;
    intervalMs?: number;
  } = $props();

  const VIEWS = [
    { name: "summary", label: "Summary" },
    { name: "queue", label: "Queue" },
    { name: "history", label: "History" },
    { name: "health", label: "Health" },
    { name: "nodes", label: "Nodes" },
    { name: "controls", label: "Controls" },
  ] as const;
  type ViewName = (typeof VIEWS)[number]["name"];

  let surface = $state<ApiResult<SurfaceDocument> | undefined>(undefined);
  let view = $state<ViewName>("summary");

  // The token lives in the holder and is mirrored here so the views read it reactively.
  // Both are variables of this page; neither is written anywhere (lib/token.ts).
  const tokens = createTokenHolder();
  let token = $state("");
  $effect(() =>
    tokens.subscribe((value) => {
      token = value;
    }),
  );

  $effect(() => {
    let current = true;
    void load().then((result) => {
      if (current) {
        surface = result;
      }
    });
    return () => {
      current = false;
    };
  });
</script>

<header>
  <h1>holdfast</h1>
  {#if surface?.kind === "ok"}
    <span class="version">{surface.value.holdfast_version}</span>
  {/if}
  <TokenField held={token !== ""} onuse={tokens.set} onforget={tokens.forget} />
</header>

<main>
  <section aria-labelledby="api-heading">
    <h2 id="api-heading">API</h2>
    {#if surface === undefined}
      <p role="status">Asking the server for its API surface.</p>
    {:else if surface.kind === "ok"}
      <p role="status" class="ok">
        Reachable: {surface.value.endpoints.length} endpoints, surface version {surface.value
          .schema_version}.
      </p>
    {:else if surface.kind === "refused"}
      <p role="alert" class="bad">
        The server refused the request for its API surface: {surface.status}
        {surface.message}
      </p>
    {:else}
      <p role="alert" class="bad">The API is not reachable: {surface.message}</p>
    {/if}
  </section>

  {#if surface?.kind === "ok"}
    <nav aria-label="Views">
      {#each VIEWS as v (v.name)}
        <button
          type="button"
          aria-current={view === v.name ? "page" : undefined}
          onclick={() => {
            view = v.name;
          }}
        >
          {v.label}
        </button>
      {/each}
    </nav>

    {#if view === "summary"}
      <Summary {token} {fetch} {intervalMs} />
    {:else if view === "queue"}
      <Queue {token} {fetch} {intervalMs} />
    {:else if view === "history"}
      <History {token} {fetch} {intervalMs} />
    {:else if view === "health"}
      <Health {token} {fetch} {intervalMs} />
    {:else if view === "nodes"}
      <Nodes {token} {fetch} {intervalMs} />
    {:else}
      <Controls {token} {fetch} />
    {/if}
  {:else if surface !== undefined}
    <p class="muted">The views are shown once the server has answered for its API surface. Reload the page to ask again.</p>
  {/if}
</main>

<style>
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0.75rem;
    padding: 0.75rem 1rem;
    border-bottom: 1px solid var(--border);
    background: var(--bg-raised);
  }

  h1 {
    margin: 0;
    font-size: 1.25rem;
  }

  .version {
    color: var(--fg-muted);
    font-size: 0.875rem;
  }

  main {
    padding: 1rem;
  }

  nav {
    display: flex;
    flex-wrap: wrap;
    gap: 0.25rem;
    margin: 1rem 0;
    border-bottom: 1px solid var(--border);
  }

  nav button {
    border-bottom: none;
    border-radius: 4px 4px 0 0;
  }

  nav button[aria-current="page"] {
    background: var(--bg);
    font-weight: 600;
    border-bottom: 2px solid var(--fg);
  }

</style>

<script lang="ts">
  import { loadSurface, type ApiResult, type SurfaceDocument } from "./lib/api";

  // The shell: it states which holdfast answered and that its API is reachable, and
  // nothing about the library. The views come on top of it.
  let { load = loadSurface }: { load?: () => Promise<ApiResult<SurfaceDocument>> } = $props();

  let surface = $state<ApiResult<SurfaceDocument> | undefined>(undefined);

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
</main>

<style>
  header {
    display: flex;
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
    max-width: 60rem;
  }

  h2 {
    margin: 0 0 0.5rem;
    font-size: 1rem;
  }

  p {
    margin: 0;
  }

  .ok {
    color: var(--ok);
  }

  .bad {
    color: var(--bad);
  }
</style>

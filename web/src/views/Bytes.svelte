<script lang="ts">
  import { exactBytes, formatBytes, UNAVAILABLE } from "../lib/format";

  // A byte count in binary units, with the exact count behind it. A null is written in
  // words: it is a figure that was not read, never a zero.
  let { value, missing = UNAVAILABLE }: { value: number | null; missing?: string } = $props();
</script>

{#if value === null}
  <span class="missing">{missing}</span>
{:else}
  <span title={exactBytes(value)}>{formatBytes(value)}</span>
{/if}

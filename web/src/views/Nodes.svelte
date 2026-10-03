<script lang="ts">
  import type { Fetch } from "../lib/api";
  import { loadNodes, NODES_PATH } from "../lib/endpoints";
  import { formatCount, formatTime, NOT_RECORDED, UNAVAILABLE } from "../lib/format";
  import { createResource } from "../lib/resource.svelte";
  import Bytes from "./Bytes.svelte";
  import ReadState from "./ReadState.svelte";

  // Worker nodes and their leases: GET /api/nodes. A read: nothing here grants, ends or
  // re-opens a lease.
  let { token = "", fetch, intervalMs }: { token?: string; fetch?: Fetch; intervalMs?: number } = $props();

  const nodes = createResource((signal) => loadNodes({ token, fetch, signal }), {
    intervalMs: () => intervalMs,
  });

  $effect(() => {
    void token;
    return nodes.start();
  });

  const yesNo = (v: boolean | null) => (v === null ? UNAVAILABLE : v ? "yes" : "no");
</script>

<section aria-labelledby="nodes-heading">
  <h2 id="nodes-heading">Nodes</h2>
  <ReadState
    result={nodes.result}
    readAt={nodes.readAt}
    path={NODES_PATH}
    what="its worker nodes"
    hasToken={token !== ""}
    onrefresh={nodes.refresh}
  />

  {#if nodes.result?.kind === "ok"}
    {@const n = nodes.result.value}
    <p data-testid="nodes-enabled">
      {#if n.enabled === null}
        The server did not say whether it takes worker nodes.
      {:else if n.enabled}
        This server takes worker nodes.
      {:else}
        This server takes no worker nodes: none is configured.
      {/if}
    </p>

    <h3 id="node-list-heading">Worker nodes</h3>
    <div class="scroll">
      <table aria-labelledby="node-list-heading">
        <thead>
          <tr>
            <th scope="col">Node</th>
            <th scope="col">Mode</th>
            <th scope="col">Encoders offered</th>
            <th scope="col">Waiting for work</th>
            <th scope="col">Cooling off until</th>
            <th scope="col" class="num">Active leases</th>
          </tr>
        </thead>
        <tbody>
          {#each n.nodes as node, i (i)}
            <tr>
              <th scope="row">{node.node ?? NOT_RECORDED}</th>
              <td>{node.mode ?? "not known"}</td>
              <td>{node.encoders === null ? "not known" : node.encoders.length === 0 ? "none" : node.encoders.join(", ")}</td>
              <td>{yesNo(node.waiting)}</td>
              <td>{formatTime(node.coolingUntil, "not cooling off")}</td>
              <td class="num">{formatCount(node.leasesActive)}</td>
            </tr>
          {:else}
            <tr><td colspan="6" class="muted">No worker node is known to this server.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>

    <h3 id="lease-list-heading">Leases</h3>
    <p data-testid="leases-total">
      {#if n.total !== null && n.total.count !== null}
        Showing {n.leases.length} of {n.total.count} leases, newest first{n.total.cap === null
          ? ""
          : ` (the server sends at most ${n.total.cap})`}.
      {:else}
        Showing {n.leases.length} leases, newest first. The total behind them is unavailable{n.total
          ?.unavailable
          ? `: ${n.total.unavailable}`
          : ""}.
      {/if}
    </p>
    <div class="scroll">
      <table aria-labelledby="lease-list-heading">
        <thead>
          <tr>
            <th scope="col">Node</th>
            <th scope="col">Path</th>
            <th scope="col">State</th>
            <th scope="col">Reason</th>
            <th scope="col" class="num">Epoch</th>
            <th scope="col">Granted</th>
            <th scope="col">Updated</th>
            <th scope="col">Expires</th>
            <th scope="col">Ended</th>
            <th scope="col" class="num">Source size</th>
            <th scope="col" class="num">Output size</th>
          </tr>
        </thead>
        <tbody>
          {#each n.leases as lease, i (i)}
            <tr>
              <td>{lease.node ?? NOT_RECORDED}</td>
              <th scope="row" class="path">{lease.path ?? NOT_RECORDED}</th>
              <td>{lease.state ?? NOT_RECORDED}</td>
              <td class="reason">{lease.reason ?? ""}</td>
              <td class="num">{formatCount(lease.epoch, NOT_RECORDED)}</td>
              <td>{formatTime(lease.grantedAt, NOT_RECORDED)}</td>
              <td>{formatTime(lease.updatedAt, NOT_RECORDED)}</td>
              <td>{formatTime(lease.expiresAt, NOT_RECORDED)}</td>
              <td>{formatTime(lease.endedAt, "not ended")}</td>
              <td class="num"><Bytes value={lease.sourceBytes} missing={NOT_RECORDED} /></td>
              <td class="num"><Bytes value={lease.outputBytes} missing="none uploaded" /></td>
            </tr>
          {:else}
            <tr><td colspan="11" class="muted">No lease is recorded.</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

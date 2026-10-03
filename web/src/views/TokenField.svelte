<script lang="ts">
  // The one field a token is typed into. What is typed is handed to the page's token
  // holder and the field is emptied; nothing here writes it anywhere else. It is not a
  // form: the page's policy gives a form nowhere to post, and a form is what a browser
  // offers to remember a password from.
  let {
    held,
    onuse,
    onforget,
  }: { held: boolean; onuse: (token: string) => void; onforget: () => void } = $props();

  let draft = $state("");

  function use() {
    if (draft.trim() === "") {
      return;
    }
    onuse(draft);
    draft = "";
  }

  function forget() {
    draft = "";
    onforget();
  }
</script>

<div class="token">
  <label for="token-input">Token</label>
  <input
    id="token-input"
    type="password"
    autocomplete="off"
    autocapitalize="off"
    spellcheck="false"
    bind:value={draft}
    onkeydown={(e) => {
      if (e.key === "Enter") {
        use();
      }
    }}
    aria-describedby="token-note"
  />
  <button type="button" onclick={use} disabled={draft.trim() === ""}>Use token</button>
  <button type="button" onclick={forget} disabled={!held && draft === ""}>Forget token</button>
  <p id="token-note" class="muted" data-testid="token-note">
    {#if held}
      A token is held in this page's memory and sent to this server's API only. Reloading the page
      or pressing Forget drops it.
    {:else}
      No token is held. The controls need this server's control token; the reads need a token only
      where server_read_token is set. It is kept in this page's memory and nowhere else.
    {/if}
  </p>
</div>

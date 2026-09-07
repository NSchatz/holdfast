// The wiring: the controls, the ticker and the live stream. This is the only module that
// runs anything at load time; everything above it declares.

// Persist the control token locally so an operator doesn't retype it each visit.
const tokenInput = $("token");
tokenInput.value = localStorage.getItem("holdfast_token") || "";
tokenInput.addEventListener("change", () => localStorage.setItem("holdfast_token", tokenInput.value));

$("filter").addEventListener("input", applyFilter);

// The elapsed ticker. It recomputes from each row's own transition timestamp, so a
// throttled or long-delayed tick produces a correct figure rather than a lagging one -
// the tick decides only HOW OFTEN the page refreshes, never WHAT it says.
setInterval(refreshElapsed, 1000);

// Live updates over SSE (EventSource auto-reconnects on drop).
let es;
function connect() {
  es = new EventSource("/api/events");
  const conn = $("conn");
  es.addEventListener("snapshot", (e) => {
    conn.textContent = "live"; conn.className = "live"; streamIsLive = true;
    // A payload that will not parse is a payload that cannot be read, and the views
    // say so (F7). It is NOT swallowed: a page that silently keeps its loading state
    // for ever is the failure this clause exists to refuse.
    let snap;
    try { snap = JSON.parse(e.data); }
    catch (_) { markUnreadable(); return; }
    render(snap);
  });
  es.onopen = () => { conn.textContent = "live"; conn.className = "live"; streamIsLive = true; };
  es.onerror = () => {
    conn.textContent = "reconnecting…"; conn.className = "down"; streamIsLive = false;
    // A stream that failed BEFORE a single snapshot arrived leaves the page with
    // nothing to show and no route to it: that is the unreadable state (F7), not a
    // permanent "loading". A stream that drops with rows already on screen keeps them
    // (F6) - markUnreadable only speaks for a view that has nothing of its own.
    markUnreadable();
  };
}
connect();

// Controls: POST with the bearer token; show the server's reason on refusal.
// A refusal says, in words, that the action DID NOT HAPPEN, and never renders anything a
// reader could take for success. The rest of the page is untouched and every control
// stays operable: a refused control action costs nothing but itself (F1, F5).
async function control(path) {
  const msg = $("msg"); msg.className = ""; msg.textContent = "working";
  try {
    const res = await fetch(path, {
      method: "POST",
      headers: tokenInput.value ? { "Authorization": "Bearer " + tokenInput.value } : {},
    });
    let body = {};
    try { body = await res.json(); } catch (_) {}
    if (res.status === 403) { msg.className = "err"; msg.textContent = "refused: nothing changed. Control is disabled on this server."; return; }
    if (res.status === 401) { msg.className = "err"; msg.textContent = "refused: nothing changed. Check the control token."; return; }
    if (res.status === 409) { msg.className = "err"; msg.textContent = "not started: " + (body.reason || "already busy"); return; }
    if (!res.ok) { msg.className = "err"; msg.textContent = "refused: nothing changed. The server returned " + res.status + "."; return; }
    msg.textContent = "done";
  } catch (err) {
    msg.className = "err"; msg.textContent = "refused: nothing changed. The request did not reach the server.";
  }
}
$("rescan").addEventListener("click", () => control("/api/rescan"));
$("pause").addEventListener("click", () => control("/api/pause"));
$("resume").addEventListener("click", () => control("/api/resume"));

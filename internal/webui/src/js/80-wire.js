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
    conn.textContent = "live"; conn.className = "live";
    try { render(JSON.parse(e.data)); } catch (_) {}
  });
  es.onopen = () => { conn.textContent = "live"; conn.className = "live"; };
  es.onerror = () => { conn.textContent = "reconnecting…"; conn.className = "down"; };
}
connect();

// Controls: POST with the bearer token; show the server's reason on refusal.
async function control(path) {
  const msg = $("msg"); msg.className = ""; msg.textContent = "…";
  try {
    const res = await fetch(path, {
      method: "POST",
      headers: tokenInput.value ? { "Authorization": "Bearer " + tokenInput.value } : {},
    });
    let body = {};
    try { body = await res.json(); } catch (_) {}
    if (res.status === 403) { msg.className = "err"; msg.textContent = "control disabled — set server_auth_token on the server."; return; }
    if (res.status === 401) { msg.className = "err"; msg.textContent = "unauthorized — check the control token."; return; }
    if (res.status === 409) { msg.textContent = "not started: " + (body.reason || "busy"); return; }
    msg.textContent = "ok";
  } catch (err) {
    msg.className = "err"; msg.textContent = "request failed";
  }
}
$("rescan").addEventListener("click", () => control("/api/rescan"));
$("pause").addEventListener("click", () => control("/api/pause"));
$("resume").addEventListener("click", () => control("/api/resume"));

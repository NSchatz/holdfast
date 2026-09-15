// The wiring: the controls, the ticker and the live stream. This is the only module that
// runs anything at load time; everything above it declares.

// Persist the control token locally so an operator doesn't retype it each visit.
const tokenInput = $("token");
tokenInput.value = localStorage.getItem("holdfast_token") || "";
tokenInput.addEventListener("change", () => localStorage.setItem("holdfast_token", tokenInput.value));

$("filter").addEventListener("input", applyFilter);

// Where the count chips break across lines changes with the viewport and with nothing
// else, so it is answered again whenever the row's own size changes. An observer rather
// than a resize listener: it fires for the box that actually moved, and not for every
// window event a page might otherwise have to filter.
if (typeof ResizeObserver === "function" && $("chips")) {
  new ResizeObserver(markChipGroupBreak).observe($("chips"));
}

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

// --- the ledger search, and the paths this daemon is withholding ---------------------
//
// Both reach endpoints inside the same token-gated group the controls above use, and for
// the same reason the search is there at all: the capped reads ship a few hundred rows, so
// a ledger-wide search and a list of withheld paths serve per-file facts they never have.
//
// Every refusal below says, in words, that NOTHING CHANGED, and none of them renders
// anything a reader could take for a result. The rest of the page is untouched and every
// other control stays operable: a refused action costs nothing but itself.

function bearer() {
  return tokenInput.value ? { "Authorization": "Bearer " + tokenInput.value } : {};
}

// refusalText turns one response status into the reason a reader is owed. It is a
// function of the STATUS and never of the body, because the body of a refusal is the
// server's prose and the page has its own account to give.
function refusalText(status) {
  if (status === 403) return "Unavailable: control is disabled on this server.";
  if (status === 401) return "Unavailable: the control token was refused.";
  if (status === 400) return "Unavailable: the request was refused as malformed.";
  return "Unavailable: the server returned " + status + ".";
}

async function runLedgerSearch() {
  const term = $("ledger-search").value.trim();
  const msg = $("search-msg");
  msg.className = "";
  if (term === "") {
    msg.textContent = "Type part of a path to search for.";
    return;
  }
  msg.textContent = "searching";
  showFound("loading");
  try {
    const res = await fetch("/api/search?path=" + encodeURIComponent(term), { headers: bearer() });
    if (!res.ok) {
      msg.className = "err";
      msg.textContent = "refused: nothing was searched.";
      searchRefused(refusalText(res.status));
      return;
    }
    let body;
    try { body = await res.json(); }
    catch (_) {
      msg.className = "err"; msg.textContent = "refused: the answer could not be read.";
      searchRefused("Unavailable: the answer could not be read.");
      return;
    }
    msg.textContent = "";
    renderSearchResults(body);
  } catch (err) {
    msg.className = "err";
    msg.textContent = "refused: nothing was searched.";
    searchRefused("Unavailable: the request did not reach the server.");
  }
}
$("search-go").addEventListener("click", runLedgerSearch);
$("ledger-search").addEventListener("keydown", (e) => { if (e.key === "Enter") runLedgerSearch(); });

// refreshHeld reads the withholdings in force. It is the ONE reader of that list, so the
// record an operator created and the record they can remove are always the same one.
async function refreshHeld(message) {
  if (!tokenInput.value) { renderHeld([], message); return; }
  try {
    const res = await fetch("/api/exclusions", { headers: bearer() });
    if (!res.ok) { renderHeld([], message || refusalText(res.status)); return; }
    const body = await res.json();
    renderHeld(body && body.exclusions, message);
  } catch (err) {
    renderHeld([], message || "Unavailable: the request did not reach the server.");
  }
}

// holdAction records or removes one withholding and then RE-READS the list, so what is on
// screen is what the daemon holds rather than what the page assumed it would hold.
async function holdAction(method, path) {
  try {
    const res = await fetch("/api/exclusions", {
      method: method,
      headers: Object.assign({ "Content-Type": "application/json" }, bearer()),
      body: JSON.stringify({ path: path }),
    });
    if (!res.ok) {
      // NOTHING CHANGED, said in words, with no part of the page rearranged as if it had.
      await refreshHeld("Nothing changed. " + refusalText(res.status));
      return;
    }
    await refreshHeld("");
  } catch (err) {
    await refreshHeld("Nothing changed. The request did not reach the server.");
  }
}

// One listener for the whole document rather than one per control: rows are rebuilt on
// every snapshot, and a listener attached while a row is built would be a listener the
// next snapshot throws away. The path travels on the control itself.
document.addEventListener("click", (e) => {
  const t = e.target;
  if (!t || !t.dataset) return;
  if (t.dataset.hold) { holdAction("POST", t.dataset.hold); return; }
  if (t.dataset.release) { holdAction("DELETE", t.dataset.release); }
});

// The list is read once at load when a token is already stored, and again whenever the
// operator changes it: the page cannot read it without one, and an empty list and an
// unasked question are different facts.
tokenInput.addEventListener("change", () => refreshHeld(""));
refreshHeld("");

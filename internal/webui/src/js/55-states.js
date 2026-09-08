// The three states every view owes (clause F7): what it shows while LOADING, what it
// shows when there is NOTHING to show, and what it shows when the data CANNOT BE READ.
//
// One mechanism for all four views, because four copies of one mechanism is how one of
// them silently stops being written. A view is a container carrying `data-view`, and
// it holds at most one element carrying `data-state`; the wording comes from VIEW_STATES,
// one phrasing per view, so a reader always knows WHICH view is in which state. The state
// element is markup in the page shell too, already in the loading state, so a view says
// what it is doing before a single byte of snapshot has arrived - and before this module
// has run at all.
//
// The state element for a table view is a full-width row inside the tbody, because a
// paragraph beside a table is not the table's own answer to "what am I showing". For a
// view that is not a table it is a paragraph inside the view's own container.

// renderedOnce records whether a whole snapshot has ever gone through render(). It is the
// difference between "the stream never delivered anything" (unreadable) and "the stream
// stopped after delivering something" (stale, rows kept).
let renderedOnce = false;

// streamIsLive is whether the event stream is currently connected. It gates the elapsed
// ticker: a figure that keeps advancing after its feed died is a figure presented as live
// when it is stale, which F6 refuses outright.
let streamIsLive = false;

function viewHost(view) { return document.querySelector('[data-view="' + view + '"]'); }

// viewHasContent answers whether a view is currently showing something of its own - a
// row, a figure card, a count chip. It is what keeps an unreadable payload arriving AFTER
// a good one from wiping rows off the screen: F6 says a severed feed keeps its rows, and
// F5 says one unreadable figure costs nothing else.
function viewHasContent(view) {
  // The badges and the reclaimed figures are markup, present in every state; only a
  // rendered chip is content the counts view produced from a snapshot.
  if (view === "counts") {
    const chips = $("chips");
    return !!chips && chips.children.length > 0;
  }
  const host = viewHost(view);
  if (!host) return false;
  for (const el of host.children) {
    if (!(el.dataset && el.dataset.state)) return true;
  }
  return false;
}

// setViewState puts one view into one of the three states, or clears the state element
// when the view has its own content to show. It never touches anything else in the view.
function setViewState(view, state) {
  const host = viewHost(view);
  if (!host) return;
  for (const old of host.querySelectorAll("[data-state]")) old.remove();
  if (!state) return;
  const text = (VIEW_STATES[view] || {})[state];
  if (!text) return;
  const cols = VIEW_COLUMNS[view];
  let el;
  if (cols) {
    el = document.createElement("tr");
    const td = mk("td", "empty", text);
    td.colSpan = cols;
    el.appendChild(td);
    // The empty row keeps the marker the rendered graders have always read it by.
    if (state === "empty") el.dataset.empty = "1";
  } else {
    el = mk("p", state === "unreadable" ? "state unreadable" : "state", text);
  }
  el.dataset.state = state;
  host.insertBefore(el, host.firstChild);
}

// markUnreadable is what a payload that cannot be read at all does to the page: every
// view that has nothing of its own on screen says so, in words, in that view. A view that
// IS showing rows keeps them - a stream that dies after a good snapshot has not made the
// rows untrue, it has made them stale, and the connection state alone says that (F6).
function markUnreadable() {
  for (const view of ["counts", "queue", "aggs", "history"]) {
    if (!viewHasContent(view)) setViewState(view, "unreadable");
  }
  if (renderedOnce) return;
  for (const id of ["reclaimed-session", "reclaimed-lifetime"]) {
    const el = $(id);
    if (el) el.replaceChildren(nrNode());
  }
}

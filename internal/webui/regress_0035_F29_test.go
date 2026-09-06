package webui

// regress_0035_F29: AC9's word is "visible" and AC10's verb is "render", and the
// only thing on this branch that asks whether a degraded state is SHOWN rather
// than BUILT is a stylesheet sweep with two structural blind spots.
//
// The sweep (TestDegradedStates_..., the loop over `invisibleValues`) asks, for
// each of five state selectors, whether any rule that REACHES THAT NODE declares
// one of five hiding values. `stateIsReachedBy` decides "reaches", and it:
//
//	a. discards every compound carrying an id -
//	   `if c.pseudoEl || c.root || (c.id != "" && c.id != want.id) { continue }` -
//	   because the states are named by class (`.empty`, `.nr`, `.note.cap`). The
//	   page gives the two cap notices ids of its own, `#queue-cap` and `#hist-cap`,
//	   and addresses them by id in its own script. `#queue-cap { display:none }`
//	   reaches exactly the node that renders `this view is capped` and is skipped
//	   by construction.
//
//	b. asks only about the state's OWN node. Visibility is a property of the box
//	   tree: `display:none` on any ANCESTOR takes the whole subtree off the page.
//	   `.tablewrap { display:none }` - the one class AC6's own width sweep exempts
//	   by name - removes both data tables, so `Nothing queued.`, `No history yet.`
//	   and AC9's entire no-match sentence are built, filled, given their colSpan,
//	   appended, measured to their contrast floor, and seen by nobody.
//
// This is impl-gate finding F20's own property - SHOWN, not BUILT - which loop 6
// closed for the JS and for one shape of CSS rule. It is not closed for the page:
// whether a node is painted is a question about a render, and the render is where
// it is asked here.
//
// The shipped page is CORRECT: the first assertion in each test below drives the
// real filter box in the real browser and reads the words off the screen.
//
// Fix upstream in the graders, not the page.

import (
	"strings"
	"testing"
)

type nodeState struct {
	Shown bool   `json:"shown"`
	Text  string `json:"text"`
}

type rowCount struct {
	All     int `json:"all"`
	Visible int `json:"visible"`
}

type degradedProbe struct {
	Shown   string               `json:"shown"`
	Nodes   map[string]nodeState `json:"nodes"`
	Queue   rowCount             `json:"queue"`
	History rowCount             `json:"history"`
}

// degradedMeasure reads the text an operator can actually SEE, plus the state of
// the two nodes the row-cap notice is written into and the row counts of both
// table bodies.
const degradedMeasure = jsHelpers + `
  var node = function (id) {
    var el = d.getElementById(id);
    if (!el) { return {shown: false, text: ""}; }
    return {shown: isShown(el), text: el.textContent.replace(/\s+/g, " ").trim()};
  };
  var rows = function (id) {
    var b = d.getElementById(id), all = 0, vis = 0;
    for (var i = 0; i < b.children.length; i++) {
      all++;
      if (isShown(b.children[i])) { vis++; }
    }
    return {all: all, visible: vis};
  };
  return {
    shown: shownText(),
    nodes: {"queue-cap": node("queue-cap"), "hist-cap": node("hist-cap")},
    queue: rows("queue"),
    history: rows("history")
  };
`

// filterPrelude types a term that matches no loaded row into the real filter box
// and fires the real input event, so what is measured afterwards is the state an
// operator would be looking at.
const filterPrelude = `
  var f = d.getElementById("filter");
  f.value = "zzz-no-such-path-anywhere";
  f.dispatchEvent(new w.Event("input", {bubbles: true}));
  return;
`

// clearedPrelude types the same term and then clears it, which is the second half
// of AC9 ("WHEN the term is cleared, the previously matching rows SHALL return").
const clearedPrelude = `
  var f = d.getElementById("filter");
  f.value = "zzz-no-such-path-anywhere";
  f.dispatchEvent(new w.Event("input", {bubbles: true}));
  f.value = "";
  f.dispatchEvent(new w.Event("input", {bubbles: true}));
  return;
`

func measureDegraded(t *testing.T, page []byte, snapshot, prelude string) degradedProbe {
	t.Helper()
	var got degradedProbe
	renderInto(t, renderCase{
		page: page, width: 1200, height: 1600,
		snapshot: snapshot, prelude: prelude, measure: degradedMeasure,
	}, &got)
	if len(got.Shown) < 200 {
		t.Fatalf("only %d characters of visible text on the rendered page; the probe is not reading it", len(got.Shown))
	}
	return got
}

func TestRegress0035_F29_AC10ADegradedStateIsHiddenByARuleNamingItsOwnId(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The page as shipped renders both row-cap notices.
	base := measureDegraded(t, nil, defaultSnapshot, "")
	for _, id := range []string{"queue-cap", "hist-cap"} {
		n := base.Nodes[id]
		if !n.Shown || !strings.Contains(n.Text, "this view is capped") {
			t.Errorf("SHIPPED PAGE: #%s shown=%v text=%q; AC10 requires `this view is capped` to be rendered", id, n.Shown, n.Text)
		}
	}
	if strings.Count(base.Shown, "this view is capped") != 2 {
		t.Errorf("SHIPPED PAGE: `this view is capped` is on the screen %d times, expected once per table", strings.Count(base.Shown, "this view is capped"))
	}
	t.Logf("shipped page: #queue-cap shown=%v %q", base.Nodes["queue-cap"].Shown, base.Nodes["queue-cap"].Text)

	// (2) One rule, naming that node by the id the page itself gives it.
	const name = "F29a-degraded-state-hidden-by-its-own-id"
	got := measureDegraded(t, mutantPage(t, name), defaultSnapshot, "")
	if got.Nodes["queue-cap"].Shown {
		t.Fatal("the mutation did not take the queue's cap notice off the page; this artifact is not measuring what it claims")
	}
	if n := strings.Count(got.Shown, "this view is capped"); n != 1 {
		t.Fatalf("expected the history's notice alone to survive, found %d on screen", n)
	}
	t.Logf("mutated page: #queue-cap carries %q and is NOT on the screen; the queue's truncated view now reads as the whole ledger",
		got.Nodes["queue-cap"].Text)

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

func TestRegress0035_F29_AC9AndAC10AnAncestorTakesTheDegradedStateOffThePage(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The page as shipped, driven for real:
	//     - an empty queue and an empty history each say so;
	//     - a filter term matching no loaded row puts AC9's message in BOTH table
	//       bodies, visibly;
	//     - clearing the term brings the rows back.
	empty := measureDegraded(t, nil, emptySnapshot, "")
	for _, want := range []string{"Nothing queued.", "No history yet."} {
		if !strings.Contains(empty.Shown, want) {
			t.Errorf("SHIPPED PAGE: %q is not on the screen with an empty queue and history", want)
		}
	}
	filtered := measureDegraded(t, nil, defaultSnapshot, filterPrelude)
	for _, want := range []string{"No loaded row matches this filter.", "themselves capped"} {
		if !strings.Contains(filtered.Shown, want) {
			t.Errorf("SHIPPED PAGE: a term matching no loaded row leaves %q off the screen", want)
		}
	}
	if filtered.Queue.Visible != 1 || filtered.History.Visible != 1 {
		t.Errorf("SHIPPED PAGE: on a non-matching term the queue shows %d rows and the history %d; each should show exactly the no-match message",
			filtered.Queue.Visible, filtered.History.Visible)
	}
	cleared := measureDegraded(t, nil, defaultSnapshot, clearedPrelude)
	if !strings.Contains(cleared.Shown, "/media/alpha.mkv") || !strings.Contains(cleared.Shown, "/media/bravo.mkv") {
		t.Error("SHIPPED PAGE: clearing the filter term did not bring the previously matching rows back")
	}
	if strings.Contains(cleared.Shown, "No loaded row matches this filter.") {
		t.Error("SHIPPED PAGE: the no-match message survived the term being cleared")
	}
	t.Logf("shipped page: empty tables say so, a non-matching term shows the no-match message in both bodies (queue %+v, history %+v), and clearing it restores the rows",
		filtered.Queue, filtered.History)

	// (2) One rule on an ANCESTOR of every one of those nodes.
	const name = "F29b-degraded-state-hidden-by-an-ancestor"
	page := mutantPage(t, name)
	mEmpty := measureDegraded(t, page, emptySnapshot, "")
	for _, gone := range []string{"Nothing queued.", "No history yet."} {
		if strings.Contains(mEmpty.Shown, gone) {
			t.Fatalf("the mutation left %q on the screen; this artifact is not measuring what it claims", gone)
		}
	}
	mFiltered := measureDegraded(t, page, defaultSnapshot, filterPrelude)
	if strings.Contains(mFiltered.Shown, "No loaded row matches this filter.") {
		t.Fatal("the mutation left AC9's message on the screen; this artifact is not measuring what it claims")
	}
	t.Logf("mutated page: the no-match row is still BUILT (%d rows in the queue body, %d in the history body) and NONE of them is on the screen (%d and %d visible)",
		mFiltered.Queue.All, mFiltered.History.All, mFiltered.Queue.Visible, mFiltered.History.Visible)

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

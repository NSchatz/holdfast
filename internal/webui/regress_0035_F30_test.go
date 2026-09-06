package webui

// S0035 impl-gate ordinal 7, finding F30 (refuter artifact).
//
// AC9 requires a VISIBLE message in the table body when a filter term matches no
// loaded row. AC10 requires the degraded-state wording to be RENDERED. Both are
// graded, on this branch, by a source sweep that reads the stylesheet and by a
// rendered sweep that asks the engine whether the node is "shown".
//
// Neither of them asks whether the node is ON THE SCREEN.
//
//   - The source sweep (TestDegradedStates_...) compares each declaration that
//     reaches a state's node against `invisibleValues`, a fixed table holding
//     display:none, visibility:hidden|collapse, content-visibility:hidden,
//     opacity:0 and font-size:0. A value outside that table is read and
//     discarded - the membership-narrower-than-the-criterion shape that impl
//     ordinals 3, 4 and 5 already blocked on, one language to the left.
//   - The rendered sweep (jsHelpers' isShown, in the ordinal-6 harness) asks for
//     display, visibility, offsetParent and a non-zero rect. A box may satisfy
//     every one of those and still be painted 9999 CSS px outside the viewport,
//     or clipped away to nothing, and this file proves in a real engine that it
//     is.
//
// So a single stylesheet rule takes `Nothing queued.`, `No history yet.`,
// `unavailable`, `not recorded`, `unknown`, `this view is capped` and AC9's
// whole no-match sentence off the operator's screen while every committed
// grader in this package stays green. This test fails while that is true.
//
// Nothing here edits the checkout: the mutation is applied to the embedded bytes
// in this process (for the browser measurement) and in a CHILD process (for the
// committed grader set), exactly as the ordinal-1..6 artifacts do.
//
// The -s0035.old / -s0035.new flags below are the same mechanism made general,
// so any rule can be driven by hand against any grader:
//
//	go test ./internal/webui/ -run '^TestRendered_' -v \
//	  -s0035.old='</style>' \
//	  -s0035.new='  td.empty { transform:translateX(-9999px); }\n</style>'

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

var (
	s0035ProbeOld = flag.String("s0035.old", "", "S0035 probe: a literal to replace in the embedded page")
	s0035ProbeNew = flag.String("s0035.new", "", "S0035 probe: what to replace it with (\\n is expanded)")
)

// f30Unescape expands the one escape a command line cannot carry literally.
func f30Unescape(s string) string { return strings.ReplaceAll(s, `\n`, "\n") }

// f30Carries reports whether this process is a child carrying a probe mutation.
func f30Carries() bool { return *s0035ProbeOld != "" }

// TestMain applies the probe substitution before any test observes indexHTML.
// The flags are not parsed when init() runs, so the edit lands here instead -
// still before the first test, which is all any grader needs: nothing in this
// package reads the embedded page at package-variable initialisation time.
func TestMain(m *testing.M) {
	flag.Parse()
	if old := f30Unescape(*s0035ProbeOld); old != "" {
		s := string(indexHTML)
		if !strings.Contains(s, old) {
			panic("regress_0035 F30: mutation anchor absent from index.html: " + old)
		}
		indexHTML = []byte(strings.Replace(s, old, f30Unescape(*s0035ProbeNew), 1))
	}
	os.Exit(m.Run())
}

// --- the nodes AC9 and AC10 are about -------------------------------------------

// f30Nodes are the degraded states this file measures, in the order f30Measure
// returns them. Each is found by the id or the selector the page itself gives it.
var f30Nodes = []string{
	"the queue's row-cap notice (this view is capped)",
	"the history's row-cap notice (this view is capped)",
	"AC9's no-match message in the queue body",
	"AC9's no-match message in the history body",
	"an aggregate's honest-absence node (unavailable / not recorded)",
}

// --- the mutations ------------------------------------------------------------

// f30Rule is one rule that takes a degraded state off the screen without using
// any value the committed graders recognise. Both are ordinary CSS a restyle
// could plausibly introduce, and neither changes the page's layout: transform
// and clip-path are painting-time properties, so nothing else the graders
// measure moves.
//
// hides names the nodes the rule is expected to take off the screen. It is not
// the same set for both: a transform does not apply to a non-replaced INLINE box
// (CSS Transforms 1), so the honest-absence span keeps its place under the first
// rule while every block-level state moves. Naming the set is what keeps this
// file measuring rather than assuming.
type f30Rule struct {
	name  string
	decl  string
	why   string
	hides []int
}

var f30Rules = []f30Rule{
	{
		name:  "translated-off-the-viewport",
		decl:  "transform:translateX(-9999px);",
		why:   "the row-cap notices and AC9's whole no-match sentence are painted 9999 CSS px to the left of the viewport",
		hides: []int{0, 1, 2, 3},
	},
	{
		name:  "clipped-away",
		decl:  "clip-path:inset(100%);",
		why:   "every degraded state is clipped to nothing, so no pixel of it is painted",
		hides: []int{0, 1, 2, 3, 4},
	},
}

// f30Selector names the nodes AC9 and AC10 are about, by the selectors the
// committed source grader itself uses for them (`.empty`, `.nr`, `.note.cap`,
// `#conn.down`, `#msg.err`).
const f30Selector = "td.empty, .note.cap, #conn.down, #msg.err, .nr"

// css is the whole edit: one rule appended to the page's own stylesheet.
func (r f30Rule) css() string {
	return fmt.Sprintf("  %s { %s }\n</style>", f30Selector, r.decl)
}

func (r f30Rule) mutated() []byte {
	s := string(indexHTML)
	if !strings.Contains(s, "</style>") {
		panic("regress_0035 F30: the page carries no </style> to anchor on")
	}
	return []byte(strings.Replace(s, "</style>", r.css(), 1))
}

func (r f30Rule) hidesNode(i int) bool {
	for _, k := range r.hides {
		if k == i {
			return true
		}
	}
	return false
}

// --- reading "is it on the screen" out of the engine ---------------------------

type f30Node struct {
	Present bool    `json:"present"`
	Text    string  `json:"text"`
	Left    float64 `json:"left"`
	Right   float64 `json:"right"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	InView  bool    `json:"inView"`
	Hit     string  `json:"hit"`
	Seen    bool    `json:"seen"`
	Shown   bool    `json:"shown"`
}

// f30Prelude drives the page's own filter box with a term that matches no loaded
// row, so AC9's no-match message is on the page when the measurement is taken.
const f30Prelude = `
  var f = d.getElementById("filter");
  f.value = "zzz-no-such-path-anywhere";
  f.dispatchEvent(new w.Event("input", {bubbles: true}));
  return;
`

// f30Measure asks the engine two different questions about each node: does its
// border box intersect the viewport at all, and does a hit test at the centre of
// that box land on the node (or inside it)? A node that fails either is not on
// the screen, whatever getComputedStyle says about display and visibility. It
// also reports the harness's own isShown for the same node, which is the value
// the committed rendered graders decide on. The order of the pushes is the order
// f30Nodes names them in.
const f30Measure = `
  var out = [];
  var add = function (el) {
    if (!el) { out.push({present: false}); return; }
    var r = el.getBoundingClientRect();
    var vw = d.documentElement.clientWidth, vh = d.documentElement.clientHeight;
    var inView = r.width > 0 && r.height > 0 &&
                 r.right > 0 && r.bottom > 0 && r.left < vw && r.top < vh;
    var hit = null;
    if (inView) {
      var cx = Math.min(Math.max(r.left + r.width / 2, 1), vw - 1);
      var cy = Math.min(Math.max(r.top + r.height / 2, 1), vh - 1);
      hit = d.elementFromPoint(cx, cy);
    }
    out.push({
      present: true,
      text: (el.textContent || "").replace(/\s+/g, " ").trim().slice(0, 60),
      left: r.left, right: r.right, w: r.width, h: r.height,
      inView: inView, hit: hit ? sel(hit) : "",
      seen: inView && !!hit && (hit === el || el.contains(hit)),
      shown: isShown(el)
    });
  };
  add(d.getElementById("queue-cap"));
  add(d.getElementById("hist-cap"));
  add(d.querySelector("#queue td.empty"));
  add(d.querySelector("#history td.empty"));
  add(d.querySelector("#aggregates .nr"));
  return out;
`

func f30Read(t *testing.T, page []byte) []f30Node {
	t.Helper()
	var got []f30Node
	renderInto(t, renderCase{
		page: page, width: 1200, height: 2000,
		prelude: f30Prelude, measure: jsHelpers + f30Measure,
	}, &got)
	if len(got) != len(f30Nodes) {
		t.Fatalf("the probe measured %d nodes, expected %d: %+v", len(got), len(f30Nodes), got)
	}
	return got
}

// --- the committed grader set, run against the same page -----------------------

// f30CommittedSetIsGreen runs every committed grader in this package (v5Run is
// the pattern the ordinal-5 artifact already uses for "everything that is not a
// regress artifact") in a child process carrying the same mutation, and reports
// whether the whole set passed.
func f30CommittedSetIsGreen(t *testing.T, r f30Rule) (bool, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", v5Run, "-test.v",
		"-s0035.old=</style>", "-s0035.new="+r.css())
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   Test") {
		t.Fatalf("the child ran no committed grader at all, so nothing was graded\n%s", out)
	}
	return err == nil, out
}

// --- the finding ---------------------------------------------------------------

func TestRegress0035F30_ADegradedStateOffTheScreenIsGradedAsRendered(t *testing.T) {
	if f30Carries() || renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}

	// (1) The instrument, proved on the page AS SHIPPED. Every node AC9 and AC10
	// name is on the screen and a hit test at its centre lands on it. Without
	// this the "not seen" readings below would prove nothing.
	for i, n := range f30Read(t, nil) {
		if !n.Present {
			t.Fatalf("%s is not in the page at all; this probe is measuring the wrong thing", f30Nodes[i])
		}
		if !n.Seen {
			t.Fatalf("on the page AS SHIPPED, %s (%q) is not on the screen (inView=%v hit=%q rect left=%g right=%g); the measurement below cannot be trusted",
				f30Nodes[i], n.Text, n.InView, n.Hit, n.Left, n.Right)
		}
	}

	for _, r := range f30Rules {
		t.Run(r.name, func(t *testing.T) {
			// (2) The defect, in a real engine: the named nodes carry the same
			// wording, the harness still calls them shown, and not one of them
			// is on the screen.
			hidden := 0
			for i, n := range f30Read(t, r.mutated()) {
				if !n.Present {
					t.Fatalf("%s vanished from the page; this rule was meant to hide it, not delete it", f30Nodes[i])
				}
				if !r.hidesNode(i) {
					continue
				}
				if n.Seen {
					t.Fatalf("%s is still on the screen under `%s`; this rule does not demonstrate the defect it claims",
						f30Nodes[i], r.decl)
				}
				if !n.Shown {
					t.Fatalf("%s is already refused by the harness's own isShown, so this rule proves nothing the committed graders miss", f30Nodes[i])
				}
				hidden++
				t.Logf("%s: text %q is off the operator's screen (rect left=%g right=%g, hit test found %q) while the harness's isShown still reports true",
					f30Nodes[i], n.Text, n.Left, n.Right, n.Hit)
			}
			if hidden != len(r.hides) {
				t.Fatalf("%d of the %d named nodes were taken off the screen", hidden, len(r.hides))
			}

			// (3) The finding: the whole committed grader set passes anyway.
			green, out := f30CommittedSetIsGreen(t, r)
			if green {
				t.Errorf("AC9 and AC10 are ungraded here: every committed grader in this package stayed GREEN on a page where %s.\n"+
					"rule added: %s\n"+
					"The source sweep only refuses the values in invisibleValues (display:none, visibility:hidden|collapse, content-visibility:hidden, opacity:0, font-size:0), and the rendered sweep's isShown asks for display, visibility, offsetParent and a non-zero rect - neither asks whether the box is on the screen.\n%s",
					r.why, strings.TrimSuffix(r.css(), "\n</style>"), out)
			}
		})
	}
}

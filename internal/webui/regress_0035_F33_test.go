package webui

// S0035 impl-gate ordinal 8, finding F33 (refuter artifact).
//
// AC10: the degraded-state wording "SHALL be painted from a token pair that
// meets AC3's floors, in both themes". AC3's text floor is 4.5:1.
//
// `opacity` is the one property on this page that changes what a colour LOOKS
// like without changing the colour any grader reads, and every reader in this
// package answers a different question about it than the criterion does:
//
//   - the rendered contrast sweep (textElements, webui_test.go:4567) reads
//     rgbOf(cs.color) and composites ONLY when that colour's own alpha is under
//     1. An element painted at `opacity:0.11` keeps `color: rgb(154, 163, 180)`,
//     so the sweep reports the ratio the token pair would have had and never the
//     ratio the engine painted;
//   - the on-screen predicate (renderedOnScreen, webui_test.go:4424) refuses a
//     node whose effective opacity is under MIN_OPACITY, and MIN_OPACITY is the
//     constant 0.1 - a threshold, chosen here, that a rule can simply sit above;
//   - the source sweep's invisibleValues names `opacity:0` and nothing else, the
//     blacklist shape impl ordinals 3, 4, 5 and 7 have each already blocked on.
//
// So `td.empty, .note.cap, #conn.down, #msg.err, .nr { opacity:0.11 }` paints
// every degraded state AC9 and AC10 name at about 1.2:1 against the surface it
// sits on, in BOTH themes, and the entire committed grader set stays green. The
// shipped page is CORRECT; what this file grades is the PROOF, which the spec's
// own acceptance-criteria preamble makes part of the criterion: "a test that
// passes on a mutated page has not graded its criterion."
//
// The measurement is the same compositing arithmetic the committed sweep already
// performs for a translucent `color` (jsHelpers' over(), then contrast()) - it is
// applied to the element's rendered opacity instead of only to its colour's
// alpha, and every input is a value the ENGINE computed.
//
// Nothing here edits the checkout. The mutation is applied to the embedded bytes
// in this process (for the browser measurement) and, through the -s0035.old /
// -s0035.new flags the ordinal-7 artifact defines, in a CHILD process (for the
// committed grader set).
//
// Reproduce the green suite by hand:
//
//	go test ./internal/webui/ -count=1 -run '^Test([A-QS-Z]|R[^e]|Re[^g])' \
//	  -s0035.old='</style>' \
//	  -s0035.new='  td.empty, .note.cap, #conn.down, #msg.err, .nr { opacity:0.11; }\n</style>'
//
// Fix upstream by folding the element's effective opacity into the colour the
// contrast sweep measures - the composite is what an operator sees - rather than
// by lowering MIN_OPACITY, which only moves the threshold a rule has to clear.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// f33Rule is the whole edit. `opacity` as a de-emphasis on muted and
// honest-absence copy is ordinary restyle work, and 0.11 is deliberately just
// above the 0.1 the predicate refuses below.
const f33Rule = "  td.empty, .note.cap, #conn.down, #msg.err, .nr { opacity:0.11; }"

func f33CSS() string { return f33Rule + "\n</style>" }

func f33Mutated() []byte {
	s := string(indexHTML)
	if !strings.Contains(s, "</style>") {
		panic("regress_0035 F33: the page carries no </style> to anchor on")
	}
	return []byte(strings.Replace(s, "</style>", f33CSS(), 1))
}

// f33Nodes are the degraded states measured, in the order f33Measure returns
// them. Each is found by the id or the selector the page itself gives it, and
// each is one AC10 names by its wording.
var f33Nodes = []string{
	"the queue's row-cap notice (this view is capped)",
	"the history's row-cap notice (this view is capped)",
	"AC9's no-match message in the queue body",
	"AC9's no-match message in the history body",
	"an aggregate's honest-absence node (unavailable / not recorded)",
}

// f33TextFloor is AC3's floor for text that is not large. None of these states
// is large text - the measurement reports each element's size and weight so that
// is checked rather than assumed.
const f33TextFloor = 4.5

type f33Node struct {
	Present bool    `json:"present"`
	Sel     string  `json:"sel"`
	Text    string  `json:"text"`
	Size    float64 `json:"size"`
	Weight  int     `json:"weight"`
	Opacity float64 `json:"opacity"`
	// Reported is the ratio the committed rendered sweep measures: the colour
	// the engine computed, against the surface behind, with no opacity in it.
	Reported float64 `json:"reported"`
	// Painted is the ratio the engine actually painted: the same two colours,
	// each composited over the backdrop at the element's effective opacity.
	Painted float64 `json:"painted"`
	FG      string  `json:"fg"`
	BG      string  `json:"bg"`
	Shown   bool    `json:"shown"`
}

func (n f33Node) String() string {
	return fmt.Sprintf("%s (%q, %gpx/%d, opacity %g): the sweep reads %.2f:1, the engine paints %.2f:1",
		n.Sel, n.Text, n.Size, n.Weight, n.Opacity, n.Reported, n.Painted)
}

// f33Prelude drives the page's own filter box with a term that matches no loaded
// row, so AC9's no-match message is on the page when the measurement is taken.
const f33Prelude = `
  var f = d.getElementById("filter");
  f.value = "zzz-no-such-path-anywhere";
  f.dispatchEvent(new w.Event("input", {bubbles: true}));
  return;
`

// f33Measure reads only values the ENGINE computed. `opacity` composites the
// element's whole rendering - its own background included - over the backdrop
// outside it, so BOTH sides of the pair are composited before they are measured,
// which is what a person looking at the screen sees.
const f33Measure = `
  var effOpacity = function (el) {
    var n = el, o = 1;
    while (n && n.nodeType === 1) {
      var v = parseFloat(w.getComputedStyle(n).opacity);
      if (!isNaN(v)) { o *= v; }
      n = n.parentElement;
    }
    return o;
  };
  var out = [];
  var add = function (el) {
    if (!el) { out.push({present: false}); return; }
    var cs = w.getComputedStyle(el);
    var backdrop = surfaceOf(el.parentElement);
    var fg = rgbOf(cs.color);
    var own = rgbOf(cs.backgroundColor);
    if (!fg) { out.push({present: false}); return; }
    // What the committed sweep measures: the element's own surface walk, and the
    // colour's own alpha, with no opacity anywhere in it.
    var sweepBG = surfaceOf(el);
    var sweepFG = fg[3] < 1 ? over(fg, sweepBG) : fg;
    // What the engine paints: the group is composited at the effective opacity.
    var a = effOpacity(el);
    var paintedFG = over([fg[0], fg[1], fg[2], fg[3] * a], backdrop);
    var paintedBG = (own && own[3] > 0)
      ? over([own[0], own[1], own[2], own[3] * a], backdrop)
      : backdrop;
    out.push({
      present: true, sel: sel(el),
      text: (el.textContent || "").replace(/\s+/g, " ").trim().slice(0, 48),
      size: parseFloat(cs.fontSize), weight: parseInt(cs.fontWeight, 10) || 400,
      opacity: a,
      reported: contrast(sweepFG, sweepBG),
      painted: contrast(paintedFG, paintedBG),
      fg: cs.color, bg: cs.backgroundColor,
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

func f33Read(t *testing.T, page []byte, dark bool) []f33Node {
	t.Helper()
	var args []string
	if dark {
		args = darkUA
	}
	var got []f33Node
	renderInto(t, renderCase{
		page: page, width: 1200, height: 2000, chromiumArgs: args,
		prelude: f33Prelude, measure: jsHelpers + renderedOnScreen + f33Measure,
	}, &got)
	if len(got) != len(f33Nodes) {
		t.Fatalf("the probe measured %d nodes, expected %d: %+v", len(got), len(f33Nodes), got)
	}
	return got
}

func f33CommittedSetIsGreen(t *testing.T) (bool, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", v5Run, "-test.v",
		"-s0035.old=</style>", "-s0035.new="+f33CSS())
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   Test") {
		t.Fatalf("the child ran no committed grader at all, so nothing was graded\n%s", out)
	}
	return err == nil, out
}

func TestRegress0035F33_AC10ADegradedStatePaintedAtElevenPercentIsGradedAtFull(t *testing.T) {
	if f30Carries() || renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}

	// (1) The instrument, proved on the page AS SHIPPED: every degraded state is
	// opaque, the sweep's reading and the painted reading agree, and both clear
	// AC3's text floor. Without this the readings below would prove nothing.
	for _, th := range f32Themes {
		for i, n := range f33Read(t, nil, th.dark) {
			if !n.Present {
				t.Fatalf("%s: %s is not on the page at all; this probe is measuring the wrong thing", th.name, f33Nodes[i])
			}
			if n.Opacity != 1 {
				t.Fatalf("%s: %s already renders at opacity %g on the shipped page", th.name, f33Nodes[i], n.Opacity)
			}
			if n.Size >= 24 || (n.Size >= 18.66 && n.Weight >= 700) {
				t.Fatalf("%s: %s is large text (%gpx / weight %d), which AC3 holds to 3:1, not the 4.5:1 this file measures against",
					th.name, f33Nodes[i], n.Size, n.Weight)
			}
			if n.Painted < f33TextFloor || n.Reported < f33TextFloor {
				t.Fatalf("%s theme, the page AS SHIPPED: %s - the shipped page already fails AC3's text floor, so the measurement below cannot be read as the mutation's doing",
					th.name, n)
			}
			t.Logf("shipped/%s: %s", th.name, n)
		}
	}

	// (2) The defect, in a real engine: with one ordinary opacity rule added,
	// every one of those states is painted at about 1.2:1 while the sweep still
	// reads its full ratio, and the on-screen predicate still calls it shown.
	page := f33Mutated()
	painted := 0
	for _, th := range f32Themes {
		for i, n := range f33Read(t, page, th.dark) {
			if !n.Present {
				t.Fatalf("%s: %s vanished; this rule was meant to fade it, not delete it", th.name, f33Nodes[i])
			}
			if n.Painted >= f33TextFloor {
				t.Fatalf("%s theme: %s still clears AC3's %.1f:1 text floor as painted; this rule does not demonstrate the defect it claims",
					th.name, n, f33TextFloor)
			}
			if n.Reported < f33TextFloor {
				t.Fatalf("%s theme: %s is already refused by the reading the committed sweep takes, so this rule proves nothing the committed graders miss",
					th.name, n)
			}
			if !n.Shown {
				t.Fatalf("%s theme: %s is already refused by the on-screen predicate, so this rule proves nothing the committed graders miss",
					th.name, n)
			}
			painted++
			t.Logf("mutated/%s: %s - UNDER AC3's %.1f:1 text floor as painted, and still shown",
				th.name, n, f33TextFloor)
		}
	}
	if want := len(f33Nodes) * len(f32Themes); painted != want {
		t.Fatalf("%d of the %d node/theme pairs were faded under the floor", painted, want)
	}

	// (3) The finding: the whole committed grader set passes anyway.
	green, out := f33CommittedSetIsGreen(t)
	if green {
		t.Errorf("AC10's \"painted from a token pair that meets AC3's floors\" is ungraded here: every committed grader in this package stayed GREEN on a page the browser has just shown paints %d degraded-state/theme pairs at under %.1f:1.\n"+
			"rule added: %s\n"+
			"textElements composites a colour's own alpha and never the element's opacity, so it reports the ratio the tokens would have had; renderedOnScreen refuses only below the constant MIN_OPACITY = 0.1; and the source sweep's invisibleValues names opacity:0 alone. All three are answers to a narrower question than the criterion asks.\n%s",
			painted, f33TextFloor, f33Rule, out)
	}
}

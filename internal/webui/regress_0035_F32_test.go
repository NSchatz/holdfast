package webui

// S0035 impl-gate ordinal 8, finding F32 (refuter artifact).
//
// AC3 names THREE floors, not two: "4.5:1 for text, 3:1 for [large] text ...
// and 3:1 for A NON-TEXT BOUNDARY THAT IDENTIFIES A CONTROL". The third one is
// graded on this branch by exactly one test,
// TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn, and that test is a
// SOURCE reader: it derives each control's fill, edge and surface from the
// stylesheet and resolves every token through themeTokens, which collects
// custom properties from the `:root` blocks ALONE (webui_test.go:1477-1498).
//
// A custom property is INHERITED. `.controls { --border:var(--line) }` therefore
// repaints the edge of every button and input on the page while themeTokens
// still reports --border's :root value (#767e8e / #6b7280) and the source grader
// still measures it at 4.6:1.
//
// That is impl-gate finding F27's shape exactly, and the rendered rewrite closed
// it for the two floors it covers and for nothing else:
//
//   - F27a (--fg redefined on body) is caught, because
//     TestRendered_EveryTextThePagePaintsClearsItsContrastFloor measures the
//     colour the ENGINE gave each text element;
//   - F27b (--target-min redefined on .controls) is caught, because
//     TestRendered_EveryPointerTargetClearsTheTwentyFourPixelFloorAsLaidOut
//     measures the box the engine LAID OUT;
//   - the non-text control boundary has no rendered grader at all. Nothing in
//     this package ever reads a control's computed border-color or
//     background-color and compares it with the surface the engine painted
//     behind it. The focus probe reads outlineColor and backgroundColor, and it
//     measures the RING against the fill - never the fill against the surface.
//
// So the shipped page is CORRECT and AC3's third floor is UNGRADED: one ordinary
// inherited-token rule leaves every enabled control at 1.0:1 and 1.3:1 against
// the surface it sits on, in BOTH themes, with the entire committed grader set
// green. The spec's own acceptance-criteria preamble makes that a failure of the
// criterion: "a test that passes on a mutated page has not graded its criterion."
//
// Nothing here edits the checkout. The mutation is applied to the embedded bytes
// in this process (for the browser measurement) and, through the -s0035.old /
// -s0035.new flags the ordinal-7 artifact already defines, in a CHILD process
// (for the committed grader set).
//
// Reproduce the green suite by hand:
//
//	go test ./internal/webui/ -count=1 -run '^Test([A-QS-Z]|R[^e]|Re[^g])' \
//	  -s0035.old='</style>' \
//	  -s0035.new='  .controls { --border:var(--line); }\n</style>'
//
// Fix upstream by measuring the non-text floor where the other two are now
// measured - of the engine, on the rendered control - rather than by adding
// --border to a list of tokens themeTokens is allowed to see.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// f32Rule is the whole edit: one inherited-token declaration on the rows that
// hold every control. It changes no value the source reader looks at, declares
// no colour literal (so AC1 is untouched) and no length (so AC2 is untouched).
const f32Rule = "  .controls { --border:var(--line); }"

func f32CSS() string { return f32Rule + "\n</style>" }

func f32Mutated() []byte {
	s := string(indexHTML)
	if !strings.Contains(s, "</style>") {
		panic("regress_0035 F32: the page carries no </style> to anchor on")
	}
	return []byte(strings.Replace(s, "</style>", f32CSS(), 1))
}

// f32Controls are the controls this rule reaches. #rescan is deliberately NOT
// among them: the primary button draws its edge from --btn-primary-border and
// its fill from --btn-primary-bg, so --border does not reach it and it stays
// identifiable. Naming the set is what keeps this file measuring rather than
// assuming.
//
// A DISABLED control is exempt, as AC3 says in its own words ("Disabled controls
// are exempt, as WCAG 2.2 1.4.3 and 1.4.11 exempt them") - #resume is disabled
// on the harness's default snapshot, so the measurement below reports it and
// then holds no floor against it.
var f32Controls = []string{"token", "pause", "resume", "filter"}

// f32MinMeasured is how many controls must be ENABLED and measured per theme for
// the reading to mean anything. Four are named above and one of them (#resume,
// on a snapshot that is not paused) is exempt.
const f32MinMeasured = 3

type f32Control struct {
	ID          string  `json:"id"`
	Present     bool    `json:"present"`
	Sel         string  `json:"sel"`
	Disabled    bool    `json:"disabled"`
	Edge        string  `json:"edge"`
	Fill        string  `json:"fill"`
	Behind      string  `json:"behind"`
	BorderWidth float64 `json:"borderWidth"`
	AgainstEdge float64 `json:"againstEdge"`
	AgainstFill float64 `json:"againstFill"`
}

// Identifiable is AC3's non-text test as the criterion states it, and as the
// committed source grader already applies it (reading 3: EITHER the edge or the
// fill clears 3:1 against the surface behind).
func (c f32Control) Identifiable() bool {
	return c.AgainstEdge >= 3.0 || c.AgainstFill >= 3.0
}

func (c f32Control) String() string {
	return fmt.Sprintf("%s: edge %s at %.2f:1, fill %s at %.2f:1, on %s (border %gpx)",
		c.Sel, c.Edge, c.AgainstEdge, c.Fill, c.AgainstFill, c.Behind, c.BorderWidth)
}

// f32Measure reads what the ENGINE computed for each control - its border
// colour, its background colour, and the surface actually composited behind it -
// and measures both against that surface with the same WCAG relative-luminance
// formula webui_test.go's own contrastRatio uses.
const f32Measure = `
  var out = [];
  var ids = __IDS__;
  for (var i = 0; i < ids.length; i++) {
    var el = d.getElementById(ids[i]);
    if (!el) { out.push({id: ids[i], present: false}); continue; }
    var cs = w.getComputedStyle(el);
    var behind = surfaceOf(el.parentElement);
    var edge = rgbOf(cs.borderTopColor);
    var fill = rgbOf(cs.backgroundColor);
    if (edge && edge[3] < 1) { edge = over(edge, behind); }
    if (fill && fill[3] < 1) { fill = over(fill, behind); }
    out.push({
      id: ids[i], present: true, sel: sel(el), disabled: !!el.disabled,
      edge: cs.borderTopColor, fill: cs.backgroundColor,
      behind: "rgb(" + Math.round(behind[0]) + "," + Math.round(behind[1]) + "," +
              Math.round(behind[2]) + ")",
      borderWidth: parseFloat(cs.borderTopWidth) || 0,
      againstEdge: edge ? contrast(edge, behind) : 0,
      againstFill: fill ? contrast(fill, behind) : 0
    });
  }
  return out;
`

func f32Read(t *testing.T, page []byte, dark bool) []f32Control {
	t.Helper()
	ids := `["` + strings.Join(f32Controls, `","`) + `"]`
	var args []string
	if dark {
		args = darkUA
	}
	var got []f32Control
	renderInto(t, renderCase{
		page: page, width: 1200, height: 1400, chromiumArgs: args,
		measure: jsHelpers + strings.Replace(f32Measure, "__IDS__", ids, 1),
	}, &got)
	if len(got) != len(f32Controls) {
		t.Fatalf("the probe measured %d controls, expected %d: %+v", len(got), len(f32Controls), got)
	}
	return got
}

// f32CommittedSetIsGreen runs every committed grader in this package against the
// same page in a child process and reports whether the whole set passed.
func f32CommittedSetIsGreen(t *testing.T) (bool, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", v5Run, "-test.v",
		"-s0035.old=</style>", "-s0035.new="+f32CSS())
	cmd.Env = os.Environ()
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   Test") {
		t.Fatalf("the child ran no committed grader at all, so nothing was graded\n%s", out)
	}
	return err == nil, out
}

var f32Themes = []struct {
	name string
	dark bool
}{{"dark", true}, {"light", false}}

func TestRegress0035F32_AC3NonTextControlFloorIsGradedFromTheRootTokensOnly(t *testing.T) {
	if f30Carries() || renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}

	// (1) The instrument, proved on the page AS SHIPPED. Every ENABLED control
	// clears AC3's 3:1 non-text floor in both themes, by its edge or by its
	// fill. Without this the readings below would prove nothing.
	for _, th := range f32Themes {
		measured := 0
		for _, c := range f32Read(t, nil, th.dark) {
			if !c.Present {
				t.Fatalf("%s: #%s is not on the page at all; this probe is measuring the wrong thing", th.name, c.ID)
			}
			if c.Disabled {
				t.Logf("shipped/%s: %s - DISABLED, exempt under AC3", th.name, c)
				continue
			}
			measured++
			if !c.Identifiable() {
				t.Fatalf("%s theme, the page AS SHIPPED: %s - the shipped page already fails AC3's non-text floor, so the measurement below cannot be read as the mutation's doing",
					th.name, c)
			}
			t.Logf("shipped/%s: %s", th.name, c)
		}
		if measured < f32MinMeasured {
			t.Fatalf("%s: only %d enabled controls were measured on the shipped page; the probe is not seeing the control rows", th.name, measured)
		}
	}

	// (2) The defect, in a real engine: with one inherited-token rule added,
	// every enabled control this rule reaches is under the 3:1 floor by BOTH
	// its edge and its fill, in both themes. Nothing identifies it as a control.
	page := f32Mutated()
	unidentifiable := 0
	for _, th := range f32Themes {
		measured := 0
		for _, c := range f32Read(t, page, th.dark) {
			if !c.Present {
				t.Fatalf("%s: #%s vanished; this rule was meant to repaint it, not delete it", th.name, c.ID)
			}
			if c.Disabled {
				continue
			}
			measured++
			if c.BorderWidth <= 0 {
				t.Fatalf("%s: %s draws no border at all, so this rule does not demonstrate the defect it claims", th.name, c)
			}
			if c.Identifiable() {
				t.Fatalf("%s theme: %s still clears AC3's 3:1 non-text floor under `%s`; this rule does not demonstrate the defect it claims",
					th.name, c, f32Rule)
			}
			unidentifiable++
			t.Logf("mutated/%s: %s - UNDER the 3:1 non-text floor by BOTH edge and fill", th.name, c)
		}
		if measured < f32MinMeasured {
			t.Fatalf("%s: only %d enabled controls were measured on the mutated page", th.name, measured)
		}
	}
	if unidentifiable < f32MinMeasured*len(f32Themes) {
		t.Fatalf("only %d control/theme pairs were repainted under the floor", unidentifiable)
	}

	// (3) The finding: the whole committed grader set passes anyway.
	green, out := f32CommittedSetIsGreen(t)
	if green {
		t.Errorf("AC3's non-text control floor is ungraded here: every committed grader in this package stayed GREEN on a page the browser has just shown puts %d control/theme pairs at under 3:1 against the surface behind them, by both their edge and their fill.\n"+
			"rule added: %s\n"+
			"TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn is the only grader of that floor and it resolves --border through themeTokens, which reads custom properties from the :root blocks alone; a custom property is inherited, so a redefinition anywhere below :root repaints every control while the source reader keeps measuring the :root value. No rendered grader reads a control's computed border-color or background-color against the surface behind it at all - impl-gate F27's shape, at the one AC3 floor the rendered rewrite never covered.\n%s",
			unidentifiable, f32Rule, out)
	}
}

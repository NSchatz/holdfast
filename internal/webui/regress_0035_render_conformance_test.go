package webui

// regress_0035_render_conformance: the criteria this loop's refuter checked
// against a real render of the SHIPPED page, so that "pass" on each of them is a
// measurement rather than a reading of the stylesheet.
//
// Nothing here is a finding. It is the positive half of the evidence tier: a
// refuter may not sign off a testable claim on reasoning either, and every
// assertion below is one the ordinal-6 verdict makes about the page as it stands.

import (
	"fmt"
	"strings"
	"testing"
)

// pausedSnapshot is defaultSnapshot with the queue paused, which swaps which of
// the two pause/resume buttons the page disables. Between the two snapshots every
// one of AC7's and AC8's five pointer targets is measured while it is enabled -
// a disabled control cannot take keyboard focus at all, and WCAG 2.2 exempts it.
var pausedSnapshot = strings.Replace(defaultSnapshot, `"paused": false`, `"paused": true`, 1)

// --- AC5: a reduce preference leaves no perceptible motion ---------------------

type motionProbe struct {
	Sel        string `json:"sel"`
	Transition string `json:"transition"`
	AnimName   string `json:"animName"`
}

const motionMeasure = jsHelpers + `
  var out = [];
  var all = d.querySelectorAll("body, header, main, .chip, .controls, .controls input, button, .badge, .agg, table, td, #conn, #msg");
  for (var i = 0; i < all.length; i++) {
    var cs = w.getComputedStyle(all[i]);
    out.push({
      sel: sel(all[i]),
      transition: cs.transitionDuration + " / " + cs.transitionDelay,
      animName: cs.animationName
    });
  }
  return out;
`

// maxMs reads the longest duration in a computed transition-duration /
// transition-delay pair, whatever unit the engine reports it in.
func maxMs(list string) float64 {
	worst := 0.0
	for _, part := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == '/' }) {
		part = strings.TrimSpace(part)
		var f float64
		var unit string
		if _, err := fmt.Sscanf(part, "%g%s", &f, &unit); err != nil {
			continue
		}
		if unit == "s" {
			f *= 1000
		}
		if f > worst {
			worst = f
		}
	}
	return worst
}

func TestRegress0035_Rendered_AC5AReducePreferenceLeavesNoPerceptibleMotion(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	var normal, reduced []motionProbe
	renderInto(t, renderCase{width: 1200, measure: motionMeasure}, &normal)
	renderInto(t, renderCase{width: 1200, chromiumArgs: reduceUA, measure: motionMeasure}, &reduced)

	moved := 0
	for _, p := range normal {
		if maxMs(p.Transition) > 1 {
			moved++
		}
		if p.AnimName != "none" && p.AnimName != "" {
			t.Errorf("%s runs the animation %q; AC5 requires every state to be readable from a static frame", p.Sel, p.AnimName)
		}
	}
	worst := 0.0
	for _, p := range reduced {
		got := maxMs(p.Transition)
		if got > worst {
			worst = got
		}
		if got > 1 {
			t.Errorf("under (prefers-reduced-motion: reduce), %s still transitions for %gms - that is perceptible motion", p.Sel, got)
		}
		if p.AnimName != "none" && p.AnimName != "" {
			t.Errorf("under (prefers-reduced-motion: reduce), %s still runs the animation %q", p.Sel, p.AnimName)
		}
	}
	t.Logf("%d of %d probed elements carry a perceptible transition with no preference expressed; under reduce the longest computed duration on the page is %gms",
		moved, len(normal), worst)
}

// --- AC8: the focus indicator, as the engine draws it --------------------------

type focusProbe struct {
	ID            string  `json:"id"`
	Disabled      bool    `json:"disabled"`
	FocusVisible  bool    `json:"focusVisible"`
	Style         string  `json:"style"`
	Width         float64 `json:"width"`
	Offset        float64 `json:"offset"`
	Colour        string  `json:"colour"`
	Fill          string  `json:"fill"`
	Behind        string  `json:"behind"`
	AgainstFill   float64 `json:"againstFill"`
	AgainstBehind float64 `json:"againstBehind"`
}

// focusMeasure gives each enabled pointer target keyboard focus and reads the
// indicator the engine draws for it, then measures that colour against the
// control's own RENDERED fill and against the surface rendered immediately behind
// it - both of which AC8 names, and neither of which is a declaration.
const focusMeasure = jsHelpers + `
  var ids = ["token", "rescan", "pause", "resume", "filter"];
  var out = [];
  for (var i = 0; i < ids.length; i++) {
    var el = d.getElementById(ids[i]);
    if (!el) { continue; }
    if (el.disabled) { out.push({id: ids[i], disabled: true}); continue; }
    el.focus();
    var cs = w.getComputedStyle(el);
    var ring = rgbOf(cs.outlineColor);
    var behind = surfaceOf(el.parentElement);
    var fill = rgbOf(cs.backgroundColor);
    if (fill && fill[3] < 1) { fill = over(fill, behind); }
    if (!fill) { fill = behind; }
    out.push({
      id: ids[i],
      disabled: false,
      focusVisible: el.matches(":focus-visible"),
      style: cs.outlineStyle,
      width: parseFloat(cs.outlineWidth) || 0,
      offset: parseFloat(cs.outlineOffset) || 0,
      colour: cs.outlineColor,
      fill: cs.backgroundColor,
      behind: "rgb(" + Math.round(behind[0]) + "," + Math.round(behind[1]) + "," + Math.round(behind[2]) + ")",
      againstFill: ring ? contrast(ring, fill) : 0,
      againstBehind: ring ? contrast(ring, behind) : 0
    });
    el.blur();
  }
  return out;
`

func TestRegress0035_Rendered_AC8TheFocusRingClearsBothSidesInBothThemes(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	for _, th := range []struct {
		name string
		args []string
	}{{"light", nil}, {"dark", darkUA}} {
		measured := map[string]focusProbe{}
		for _, snap := range []string{defaultSnapshot, pausedSnapshot} {
			var got []focusProbe
			renderInto(t, renderCase{width: 1200, chromiumArgs: th.args, snapshot: snap, measure: focusMeasure}, &got)
			if len(got) != 5 {
				t.Fatalf("%s theme: measured %d controls, expected the page's five", th.name, len(got))
			}
			for _, p := range got {
				if !p.Disabled {
					measured[p.ID] = p
				}
			}
		}
		for _, id := range []string{"token", "rescan", "pause", "resume", "filter"} {
			p, ok := measured[id]
			if !ok {
				t.Errorf("%s theme: #%s was disabled under both snapshots, so its focus indicator was never measured", th.name, id)
				continue
			}
			if !p.FocusVisible {
				t.Errorf("%s theme: #%s does not match :focus-visible on keyboard focus, so no indicator is drawn for it", th.name, id)
				continue
			}
			if p.Style == "none" || p.Width < 2 {
				t.Errorf("%s theme: #%s draws a %s outline %gpx wide; WCAG 2.2 2.4.13 puts the floor at a 2 CSS px perimeter", th.name, id, p.Style, p.Width)
			}
			if p.Offset < 0 {
				t.Errorf("%s theme: #%s draws its ring at offset %gpx, inside its own border box, where it never meets the surface behind it", th.name, id, p.Offset)
			}
			if p.AgainstFill < 3 {
				t.Errorf("%s theme: #%s's ring %s is %.2f:1 against the control's own rendered fill %s, under the 3:1 floor", th.name, id, p.Colour, p.AgainstFill, p.Fill)
			}
			if p.AgainstBehind < 3 {
				t.Errorf("%s theme: #%s's ring %s is %.2f:1 against the surface rendered behind it (%s), under the 3:1 floor", th.name, id, p.Colour, p.AgainstBehind, p.Behind)
			}
			t.Logf("%s theme: #%s ring %s at %gpx offset %gpx - %.2f:1 on its own fill %s, %.2f:1 on the surface behind (%s)",
				th.name, id, p.Colour, p.Width, p.Offset, p.AgainstFill, p.Fill, p.AgainstBehind, p.Behind)
		}
	}
}

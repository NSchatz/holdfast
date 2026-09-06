package webui

// regress_0035_F28: AC6 is graded by reading two property NAMES and a list of
// declared widths. Neither is the layout, and both leave the criterion's own
// words - "in a single column", "the page body itself does not scroll
// horizontally" - decided by something the sweep never looks at.
//
//	a. WHICH of flex-direction and grid-template-columns is live at all is decided
//	   by `display`. The region loop finds the last applying rule that declares the
//	   property it expects, and a rule that changes the display type leaves that
//	   declaration exactly where it was and inert. `.controls { display:grid;
//	   grid-template-columns:1fr 1fr }` inside the page's own narrow block lays both
//	   control rows out in TWO columns at 360px while `flex-direction:column` is
//	   still declared, still applies, and is still what the proof reads;
//	   `#aggregates { display:flex }` puts the aggregate cards in a ROW while
//	   `grid-template-columns:1fr` is still declared and no longer used.
//
//	b. The width sweep reads `width`, `min-width`, `inline-size` and
//	   `min-inline-size`. A box overflows its container for reasons that are none
//	   of those: `section .note { white-space:nowrap }` lays the page's own
//	   explanatory prose on one unwrapped line and the BODY scrolls sideways at
//	   360px - which is the harm the criterion states in its own words - with not
//	   one width declared anywhere.
//
// Both are the class the conductor's ruling names: a CSS-text grader cannot
// decide what a rule applies to or what wins a cascade, and it cannot decide a
// layout at all. The shipped page is CORRECT at 360px on both counts, which the
// first assertion in each test below measures in the browser.
//
// Fix upstream in the graders, not the page.

import "testing"

type layoutProbe struct {
	ScrollW  float64        `json:"scrollW"`
	ClientW  float64        `json:"clientW"`
	BodyW    float64        `json:"bodyW"`
	PerRow   map[string]int `json:"perRow"`
	Overlaps []string       `json:"overlaps"`
}

// layoutMeasure reads the rendered layout at the case's viewport width: whether
// the document scrolls sideways, and whether any two visible children of AC6's
// three regions share a horizontal band - which is what "a single column" means
// once it is asked of the render instead of of a declaration.
const layoutMeasure = jsHelpers + `
  var regions = {
    "header": [d.querySelector("header")],
    ".controls": [].slice.call(d.querySelectorAll(".controls")),
    ".aggs": [d.getElementById("aggregates")]
  };
  var perRow = {}, overlaps = [];
  for (var key in regions) {
    var most = 1;
    var hosts = [].concat.apply([], [regions[key]]);
    for (var h = 0; h < hosts.length; h++) {
      var host = hosts[h];
      if (!host) { continue; }
      var kids = [];
      for (var i = 0; i < host.children.length; i++) {
        if (isShown(host.children[i])) { kids.push(host.children[i]); }
      }
      for (var i = 0; i < kids.length; i++) {
        var band = 1;
        var a = kids[i].getBoundingClientRect();
        for (var j = 0; j < kids.length; j++) {
          if (i === j) { continue; }
          var b = kids[j].getBoundingClientRect();
          if (a.top < b.bottom - 0.5 && b.top < a.bottom - 0.5) {
            band++;
            if (band === 2) {
              overlaps.push(key + ": " + sel(kids[i]) + " shares a row with " + sel(kids[j]));
            }
          }
        }
        if (band > most) { most = band; }
      }
    }
    perRow[key] = most;
  }
  return {
    scrollW: d.documentElement.scrollWidth,
    clientW: d.documentElement.clientWidth,
    bodyW: d.body.scrollWidth,
    perRow: perRow,
    overlaps: overlaps
  };
`

func measureLayout(t *testing.T, page []byte) layoutProbe {
	t.Helper()
	var got layoutProbe
	renderInto(t, renderCase{page: page, width: 360, height: 1400, measure: layoutMeasure}, &got)
	if got.ClientW != 360 {
		t.Fatalf("the page under test reports a %gpx viewport, not the 360 the criterion is written at", got.ClientW)
	}
	if len(got.PerRow) != 3 {
		t.Fatalf("measured %d of AC6's three regions: %v", len(got.PerRow), got.PerRow)
	}
	return got
}

func TestRegress0035_F28_AC6SingleColumnIsDecidedByAPropertyNothingReads(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The page as shipped, at 360px: one column in all three regions.
	base := measureLayout(t, nil)
	for region, n := range base.PerRow {
		if n > 1 {
			t.Errorf("SHIPPED PAGE at 360px: %s renders %d elements per row, not a single column (%v)", region, n, base.Overlaps)
		}
	}
	t.Logf("shipped page at 360px: per-row %v, documentElement.scrollWidth=%g clientWidth=%g",
		base.PerRow, base.ScrollW, base.ClientW)

	// (2) The same page with a display change that leaves both declarations the
	// proof reads exactly where they are.
	const name = "F28a-single-column-undone-by-the-display-property"
	got := measureLayout(t, mutantPage(t, name))
	broken := 0
	for region, n := range got.PerRow {
		if n > 1 {
			broken++
			t.Logf("mutated page at 360px: %s RENDERS %d elements per row", region, n)
		}
	}
	for _, o := range got.Overlaps {
		t.Logf("mutated page at 360px: %s", o)
	}
	if broken == 0 {
		t.Fatalf("the mutation left every region a single column (%v); this artifact is not measuring what it claims", got.PerRow)
	}

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

func TestRegress0035_F28_AC6TheBodyScrollsSidewaysWithNoWidthDeclared(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The page as shipped fits the 360px viewport. The two data tables scroll
	// inside their own .tablewrap, which AC6 explicitly permits, and the document
	// itself does not.
	base := measureLayout(t, nil)
	if base.ScrollW > base.ClientW+0.5 {
		t.Errorf("SHIPPED PAGE at 360px: the document scrolls sideways (scrollWidth %g > clientWidth %g)", base.ScrollW, base.ClientW)
	}

	// (2) One declaration that is neither a width nor a minimum width.
	const name = "F28b-body-scrolls-sideways-with-no-width-declared"
	got := measureLayout(t, mutantPage(t, name))
	if got.ScrollW <= got.ClientW+0.5 {
		t.Fatalf("the mutation did not make the document scroll sideways (scrollWidth %g, clientWidth %g); this artifact is not measuring what it claims",
			got.ScrollW, got.ClientW)
	}
	t.Logf("mutated page at 360px: documentElement.scrollWidth=%g against a clientWidth of %g - the page BODY scrolls sideways by %g CSS px",
		got.ScrollW, got.ClientW, got.ScrollW-got.ClientW)

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

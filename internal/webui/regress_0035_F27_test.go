package webui

// regress_0035_F27: the token layer is read ONLY from the :root blocks, and a
// custom property is inherited, so every measured length and every measured
// colour on this page is a value the browser may never use.
//
// AC1 buys the token layer ("declare every colour it paints with inside a
// custom-property block and reference it as var(--...) at every point of use").
// AC3 measures the pairs it paints, AC7 measures --target-min, AC6 measures
// --bp-narrow, AC8 measures --focus and --focus-w. Every one of those numbers is
// resolved by `themeTokens`, which collects declarations from rules whose
// selector is exactly `:root` and from nowhere else:
//
//	for _, r := range rules { if r.sel != ":root" { continue } ... }
//
// A custom property is an ordinary INHERITED property. `body { --fg:var(--line) }`
// or `.controls { --target-min:8px }` re-declares one for a whole subtree, and is:
//
//   - not a colour literal - AC1's two patterns see a var() reference, or a plain
//     length, and stop;
//   - not a font-size or a space length - AC2 reads the property NAME, and
//     `--fs-md` is neither `font-size` nor a member of spaceProps;
//   - not a dangling reference - the var() sweep is satisfied, because every name
//     it uses is declared in :root;
//
// and it is invisible to every reader downstream, which then goes on reporting
// the :root value for a page painted from another one.
//
// This is the CLASS the conductor's ruling names, at the one place the spec's own
// deliverable created it: "a CSS-TEXT grader cannot decide what a rule applies to,
// what wins a cascade, or what is shown rather than built". Resolving a token IS
// deciding what wins a cascade, at the element that uses it.
//
// The shipped page is CORRECT - the first assertion in each test below is the
// browser measuring the page as it stands and finding every rendered pair over
// its floor and every rendered target over 24px. What is graded here is the
// PROOF.
//
// Fix upstream in the graders, not the page.

import "testing"

// richSnapshot draws the states defaultSnapshot does not: both badges ON, a
// failed row with its reason, a skipped row with its guard, a done row carrying
// both VMAF statistics and its viewing condition, a progress figure, and
// aggregates that report a spread, a window and excluded rows. Between the two
// snapshots the contrast sweep below sees the classes the stylesheet paints
// rather than only the ones one fixture happens to reach.
const richSnapshot = `{
  "now": 1757200000,
  "paused": true,
  "scanning": true,
  "bytes_reclaimed_lifetime": 987654321,
  "bytes_reclaimed_session": 262144,
  "summary": {"pending": 3, "probing": 1, "encoding": 1, "verifying": 1,
              "done": 12, "skipped": 4, "failed": 2},
  "queue": [
    {"path": "/media/charlie.mkv", "status": "encoding", "updated_at": 1757199800,
     "worker": "w2", "progress_fraction": 0.42, "progress_seconds": 300,
     "progress_duration_seconds": 720},
    {"path": "/media/delta.mkv", "status": "probing", "updated_at": 1757199950, "worker": "w1"}
  ],
  "history": [
    {"path": "/media/echo.mkv", "status": "done", "updated_at": 1757199000,
     "source_bytes": 4000000000, "output_bytes": 1500000000, "encoder": "cpu",
     "encode_ms": 5400000, "vmaf_mean": 97.4, "vmaf_min": 91.2,
     "vmaf_model": "version=vmaf_v0.6.1"},
    {"path": "/media/foxtrot.mkv", "status": "failed", "updated_at": 1757198000,
     "reason": "verify: packet count differs (source 172800, output 172799)"},
    {"path": "/media/golf.mkv", "status": "skipped", "updated_at": 1757197000,
     "reason": "hardlinked"}
  ],
  "aggregates": {
    "outcomes": {"available": true, "counted": 18, "covers": "every terminal row",
                 "buckets": [{"key": "done", "count": 12}, {"key": "skipped", "count": 4},
                             {"key": "failed", "count": 2}]},
    "skips_by_guard": {"available": true, "counted": 4, "covers": "every skipped row",
                       "buckets": [{"key": "hardlinked", "count": 3},
                                   {"key": "low-bitrate", "count": 1}]},
    "size_ratio": {"available": true, "counted": 12, "excluded": 2, "covers": "every done row",
                   "window": "the last 90 days", "mean": 0.42, "min": 0.21, "max": 0.77},
    "encode_ms": {"available": true, "counted": 12, "covers": "every done row",
                  "mean": 5400000, "min": 900000, "max": 14400000},
    "vmaf_mean": {"available": true, "counted": 12, "covers": "every scored row",
                  "mean": 97.1, "min": 95.4, "max": 99.2},
    "vmaf_min": {"available": false, "unavailable": "the ledger could not be read",
                 "covers": "every scored row"}
  }
}`

// --- AC3: what the engine actually paints --------------------------------------

type paintedText struct {
	Sel    string  `json:"sel"`
	Text   string  `json:"text"`
	FG     string  `json:"fg"`
	BG     string  `json:"bg"`
	Size   float64 `json:"size"`
	Weight int     `json:"weight"`
	Floor  float64 `json:"floor"`
	Ratio  float64 `json:"ratio"`
}

// contrastMeasure walks every element that renders text of its own and measures
// the colour the engine computed for it against the surface actually behind it -
// the first opaque background up the box tree, composited, which is the surface a
// person sees rather than the one a `--paints-on` annotation claims.
//
// Disabled controls are exempt, as WCAG 2.2 1.4.3 exempts them and as AC3 says.
const contrastMeasure = jsHelpers + `
  var out = [];
  var all = d.body.querySelectorAll("*");
  for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (!isShown(el)) { continue; }
    if (el.closest("[disabled]") || el.closest(":disabled")) { continue; }
    var text = "";
    for (var k = 0; k < el.childNodes.length; k++) {
      if (el.childNodes[k].nodeType === 3) { text += el.childNodes[k].nodeValue; }
    }
    text = text.replace(/\s+/g, " ").trim();
    if (!text) { continue; }
    var cs = w.getComputedStyle(el);
    var bg = surfaceOf(el);
    var fg = rgbOf(cs.color);
    if (!fg) { continue; }
    if (fg[3] < 1) { fg = over(fg, bg); }
    var size = parseFloat(cs.fontSize);
    var weight = parseInt(cs.fontWeight, 10) || 400;
    var large = size >= 24 || (size >= 18.66 && weight >= 700);
    out.push({
      sel: sel(el), text: text.slice(0, 48), fg: cs.color,
      bg: "rgb(" + Math.round(bg[0]) + "," + Math.round(bg[1]) + "," + Math.round(bg[2]) + ")",
      size: size, weight: weight, floor: large ? 3 : 4.5, ratio: contrast(fg, bg)
    });
  }
  return out;
`

// measureContrast renders the page in both themes, under both fixtures, and
// returns every text element the engine painted.
func measureContrast(t *testing.T, page []byte) map[string][]paintedText {
	t.Helper()
	out := map[string][]paintedText{}
	for _, th := range []struct {
		name string
		args []string
	}{{"light", nil}, {"dark", darkUA}} {
		for _, snap := range []string{defaultSnapshot, richSnapshot} {
			var got []paintedText
			renderInto(t, renderCase{
				page: page, width: 1200, height: 2000,
				chromiumArgs: th.args, snapshot: snap, measure: contrastMeasure,
			}, &got)
			if len(got) < 20 {
				t.Fatalf("%s theme: only %d text-bearing elements measured; the probe is not seeing the rendered page", th.name, len(got))
			}
			out[th.name] = append(out[th.name], got...)
		}
	}
	return out
}

func worstPair(rows []paintedText) paintedText {
	worst := paintedText{Ratio: 1e9}
	for _, r := range rows {
		if r.Ratio-r.Floor < worst.Ratio-worst.Floor {
			worst = r
		}
	}
	return worst
}

func TestRegress0035_F27_AC3ATokenRedefinedBelowRootRepaintsThePageUnmeasured(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The instrument, and the page as shipped. Every rendered pair clears its
	// floor in both themes - the page is right, which is why what follows is a
	// finding about the proof and not about the page.
	for theme, rows := range measureContrast(t, nil) {
		under := 0
		for _, r := range rows {
			if r.Ratio < r.Floor {
				under++
				t.Errorf("SHIPPED PAGE, %s theme: %s (%q) paints %s on %s at %.2f:1, under the %.1f:1 floor at %gpx/%d",
					theme, r.Sel, r.Text, r.FG, r.BG, r.Ratio, r.Floor, r.Size, r.Weight)
			}
		}
		w := worstPair(rows)
		t.Logf("shipped page, %s theme: %d rendered text elements measured across two fixtures, %d under floor; tightest is %s (%q) at %.2f:1 against a %.1f:1 floor",
			theme, len(rows), under, w.Sel, w.Text, w.Ratio, w.Floor)
	}

	// (2) The same measurement on a page whose --fg is re-declared one element
	// below :root.
	const name = "F27a-token-redefined-below-root-repaints-the-page"
	for theme, rows := range measureContrast(t, mutantPage(t, name)) {
		w := worstPair(rows)
		if w.Ratio >= w.Floor {
			t.Fatalf("%s theme: the mutation did not break the render (tightest pair %s at %.2f:1); this artifact is not measuring what it claims",
				theme, w.Sel, w.Ratio)
		}
		under := 0
		for _, r := range rows {
			if r.Ratio < r.Floor {
				under++
			}
		}
		t.Logf("mutated page, %s theme: %d of %d rendered text elements are under their floor; the worst is %s (%q) at %s on %s, %.2f:1 against %.1f:1",
			theme, under, len(rows), w.Sel, w.Text, w.FG, w.BG, w.Ratio, w.Floor)
	}

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

// --- AC7: what the engine actually lays out ------------------------------------

type targetBox struct {
	ID string  `json:"id"`
	W  float64 `json:"w"`
	H  float64 `json:"h"`
}

// targetMeasure reads the rendered border box of each of AC7's five pointer
// targets - "every button and every input in the two control rows".
const targetMeasure = jsHelpers + `
  var ids = ["token", "rescan", "pause", "resume", "filter"];
  var out = [];
  for (var i = 0; i < ids.length; i++) {
    var el = d.getElementById(ids[i]);
    if (!el) { continue; }
    var r = el.getBoundingClientRect();
    out.push({id: ids[i], w: r.width, h: r.height});
  }
  return out;
`

func measureTargets(t *testing.T, page []byte, width int) []targetBox {
	t.Helper()
	var got []targetBox
	renderInto(t, renderCase{page: page, width: width, height: 1400, measure: targetMeasure}, &got)
	if len(got) != 5 {
		t.Fatalf("measured %d pointer targets, expected the page's five: %v", len(got), got)
	}
	return got
}

func TestRegress0035_F27_AC7ATokenRedefinedBelowRootShrinksATargetUnmeasured(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	// (1) The page as shipped, at a desktop viewport and at the narrow one.
	for _, width := range []int{1200, 360} {
		boxes := measureTargets(t, nil, width)
		for _, b := range boxes {
			if b.W < 24 || b.H < 24 {
				t.Errorf("SHIPPED PAGE at %dpx: #%s renders %.1f x %.1f CSS px, under WCAG 2.2 2.5.8's 24px floor", width, b.ID, b.W, b.H)
			}
		}
		t.Logf("shipped page at %dpx: %v", width, boxes)
	}

	// (2) The same measurement with --target-min, --sp-3 and --fs-md re-declared
	// on the control rows.
	const name = "F27b-token-redefined-below-root-shrinks-a-target"
	under := 0
	for _, b := range measureTargets(t, mutantPage(t, name), 1200) {
		if b.W < 24 || b.H < 24 {
			under++
			t.Logf("mutated page: #%s RENDERS %.1f x %.1f CSS px, under WCAG 2.2 2.5.8's 24px floor", b.ID, b.W, b.H)
		}
	}
	if under == 0 {
		t.Fatal("the mutation did not shrink a single pointer target; this artifact is not measuring what it claims")
	}

	// (3) ...and nothing committed reds on it.
	committedSetStaysGreen(t, name)
}

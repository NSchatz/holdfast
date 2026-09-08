package webui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The CASES the frontend-convention graders are run against (S0053).
//
// Every one of them loads the SERVED document at the top level in a real browser engine,
// under media the engine emulates, and decides its criterion from what the engine
// reported. The theme is never chosen by a class, an attribute or a stylesheet injected
// into the page under test.

// --- fixtures ------------------------------------------------------------------------

// trulyEmptySnapshot is a ledger with nothing in it AND no figures published: no queue
// rows, no history rows, no aggregates. It is the case clause F7's empty state is about.
func trulyEmptySnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":0,"verifying":0,"done":0,"skipped":0,"failed":0},
  "queue": [], "history": [], "aggregates": {},
  "bytes_reclaimed_session": null, "bytes_reclaimed_lifetime": null,
  "paused": false, "scanning": false, "now": %d
}`, snapNow))
}

// nullSnapshot is a snapshot in which EVERY nullable field arrived null: no score, no
// duration, no progress, no size, no worker, no aggregate input. It is the case clause F3
// is about, and every one of those fields must read the page's single absence phrase.
func nullSnapshot() []byte {
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":1,"probing":0,"encoding":1,"verifying":0,"done":1,"skipped":0,"failed":0},
  "queue": [
    {"path":"/media/films/alpha.mkv","status":"pending","worker":null,"updated_at":%d,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null},
    {"path":"/media/films/bravo.mkv","status":"encoding","worker":null,"updated_at":null,
     "progress_seconds":null,"progress_duration_seconds":null,"progress_fraction":null}
  ],
  "history": [
    {"path":"/media/films/delta.mkv","status":"done","worker":null,"updated_at":null,
     "encoder":null,"vmaf_mean":null,"vmaf_min":null,"vmaf_model":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "bytes_reclaimed_session": null, "bytes_reclaimed_lifetime": null,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": %s
}`, snapNow-30, snapNow, unmeasuredAggregates))
}

// unmeasuredAggregates is every figure published, available, and with no row having
// contributed a value to any of them.
const unmeasuredAggregates = `{
  "outcomes": {"available":true,"unavailable":"","covers":"every terminal row in the ledger","window":"",
    "counted":0,"excluded":4,"buckets":[]},
  "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row in the ledger","window":"",
    "counted":0,"excluded":2,"buckets":[]},
  "size_ratio": {"available":true,"unavailable":"","covers":"every done row that recorded both sizes","window":"",
    "counted":0,"excluded":1,"min":null,"mean":null,"max":null},
  "encode_ms": {"available":true,"unavailable":"","covers":"every done row that recorded a duration","window":"",
    "counted":0,"excluded":1,"min":null,"mean":null,"max":null},
  "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row that recorded a mean","window":"",
    "counted":0,"excluded":1,"min":null,"mean":null,"max":null},
  "vmaf_min": {"available":true,"unavailable":"","covers":"every done row that recorded a worst frame","window":"",
    "counted":0,"excluded":1,"min":null,"mean":null,"max":null}
}`

// hostileSnapshot puts attacker-influencable text in the three places the wire carries
// free text a reader sees: a media path, a failure reason and a bucket label.
func hostileSnapshot() []byte {
	const evil = `<img src=x onerror=alert(1)><script>alert(2)</script>\" onmouseover=\"alert(3)`
	return []byte(fmt.Sprintf(`{
  "summary": {"pending":0,"probing":0,"encoding":1,"verifying":0,"done":1,"skipped":0,"failed":1},
  "queue": [
    {"path":"/media/%s/inflight.mkv","status":"encoding","worker":"w1","updated_at":%d,
     "progress_seconds":30,"progress_duration_seconds":60,"progress_fraction":0.5}
  ],
  "history": [
    {"path":"/media/%s/clip.mkv","status":"done","worker":"w1","updated_at":%d,
     "encoder":"cpu","vmaf_mean":98.0,"vmaf_min":92.0,"vmaf_model":"version=vmaf_v0.6.1",
     "source_bytes":1000,"output_bytes":500,"encode_ms":1000},
    {"path":"/media/plain.mkv","status":"failed","worker":"w2","updated_at":%d,
     "reason":"%s","vmaf_mean":null,"vmaf_min":null,
     "source_bytes":null,"output_bytes":null,"encode_ms":null}
  ],
  "bytes_reclaimed_session": 500, "bytes_reclaimed_lifetime": 500,
  "paused": false, "scanning": false, "now": %d,
  "aggregates": {
    "outcomes": {"available":true,"unavailable":"","covers":"every terminal row","window":"",
      "counted":2,"excluded":0,"buckets":[{"key":"%s","count":2},{"key":"done","count":1}]},
    "skips_by_guard": {"available":true,"unavailable":"","covers":"every skipped row","window":"",
      "counted":0,"excluded":0,"buckets":[]},
    "size_ratio": {"available":true,"unavailable":"","covers":"every done row","window":"",
      "counted":1,"excluded":0,"min":0.5,"mean":0.5,"max":0.5},
    "encode_ms": {"available":true,"unavailable":"","covers":"every done row","window":"",
      "counted":1,"excluded":0,"min":1000,"mean":1000,"max":1000},
    "vmaf_mean": {"available":true,"unavailable":"","covers":"every done row","window":"",
      "counted":1,"excluded":0,"min":98,"mean":98,"max":98},
    "vmaf_min": {"available":true,"unavailable":"","covers":"every done row","window":"",
      "counted":1,"excluded":0,"min":92,"mean":92,"max":92}
  }
}`, evil, snapNow-5, evil, snapNow-10, snapNow-20, evil, snapNow, evil))
}

// --- the whole convention set, in both themes and at both widths -----------------------

func TestRenderedCDP_TheShippedPageMeetsEveryConventionInBothThemesAtBothWidths(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	// Which combinations actually produced a MEASUREMENT. Clause F10 says a contrast
	// assertion is run once per theme and that the gate fails "if either run is missing",
	// so the runs are counted rather than assumed: a loop that quietly stopped visiting a
	// theme would otherwise pass by measuring nothing.
	measured := map[string]int{}

	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int{360, 1280} {
			p.loadConventions(t, ps.url, convOpts{theme: theme, width: width, height: 900})
			s := p.collect(t)
			measured[theme] += len(s.TextRuns)
			for _, g := range convGraders() {
				// Wide content taking a scroll of its own is a claim about a NARROW
				// viewport; at 1280 the tables fit and there is nothing to scroll.
				if g.name == "wide content scrolls inside its own container" && width != 360 {
					continue
				}
				if probs := g.probe(s); probs != nil {
					for _, prob := range probs {
						t.Errorf("[%s theme, %dpx] %s: %s", theme, width, g.name, prob)
					}
				}
			}
		}
	}

	for _, theme := range []string{"light", "dark"} {
		if measured[theme] < 20 {
			t.Errorf("the %s theme contributed only %d text measurements; clause F10 requires every contrast assertion to be RUN in both themes, and a run that measured nothing is a missing run",
				theme, measured[theme])
		}
	}
}

// Clause F9 in every STATE, not only the live one: a page that is loading, empty or
// unreadable is still a page an operator is looking at on a phone, and a state row or a
// state line that pushes the body sideways is the same defect as a table that does.
func TestRenderedCDP_TheBodyNeverScrollsSidewaysAtThreeSixtyInEveryThemeAndState(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	for _, theme := range []string{"light", "dark"} {
		for _, c := range []struct {
			state string
			serve serveOpts
		}{
			{"live", serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()}},
			{"loading", serveOpts{url: upstreamForTest, holdStream: true}},
			{"empty", serveOpts{url: upstreamForTest, snapshot: trulyEmptySnapshot()}},
			{"unreadable", serveOpts{url: upstreamForTest, rawSnapshot: `{"summary":`}},
		} {
			ps := serveDocumentWith(t, c.serve)
			wait := convReadyLive
			if c.state != "live" {
				wait = fmt.Sprintf(`__hf.views().length === 4 && __hf.views().every(function (v) { return v.state === %q; })`, c.state)
			}
			p.loadConventions(t, ps.url, convOpts{theme: theme, width: 360, height: 900, waitFor: wait})
			s := p.collect(t)
			if probs := gradeBodyNeverScrollsSideways(s); probs != nil {
				for _, prob := range probs {
					t.Errorf("[%s theme, %s state] %s", theme, c.state, prob)
				}
			}
			if s.Layout.InnerWidth != 360 {
				t.Errorf("[%s theme, %s state] the engine laid the page out against a %.0f px viewport, not 360",
					theme, c.state, s.Layout.InnerWidth)
			}
		}
	}
}

// --- F10: the theme is the operating system's preference, read at the engine ------------

// gradeThemeFollowsTheEnginesPreference is clause F10 over three readings of the same
// document, each taken under a preference the ENGINE emulates: a light one, a dark one,
// and the ABSENCE of the override, which is what an engine reports when the platform
// expresses none. It is a pure function of the three, so the whole of it can be run
// against a document built to defeat it.
func gradeThemeFollowsTheEnginesPreference(light, dark, none convTokens) []string {
	var out []string

	// Two palettes, and they are actually different palettes.
	differing := 0
	for _, name := range s2Vocabulary {
		l, d := light.Resolved[name], dark.Resolved[name]
		if l == "" || d == "" {
			out = append(out, fmt.Sprintf("the engine resolved no value for %s (light %q, dark %q)", name, l, d))
			continue
		}
		if l != d {
			differing++
		} else {
			out = append(out, fmt.Sprintf("%s resolves to the same value %q under a light and a dark preference; the two palettes are not two palettes", name, l))
		}
	}
	if differing != len(s2Vocabulary) {
		out = append(out, fmt.Sprintf("only %d of the %d role tokens change with the preference", differing, len(s2Vocabulary)))
	}

	// The page is actually PAINTED in the palette in force, not merely told about it.
	if light.BodyBackground == dark.BodyBackground {
		out = append(out, fmt.Sprintf("the body is painted %s under both preferences", light.BodyBackground))
	}

	// NO PREFERENCE renders ONE NAMED DEFAULT and not a mixture. Proved as identity with
	// the light run over EVERY token the engine resolved: a mixture would have to differ
	// from both runs on at least one token, and this admits none.
	if len(none.Resolved) == 0 {
		return append(out, "the engine resolved no tokens at all under no preference")
	}
	for name, v := range none.Resolved {
		if v != light.Resolved[name] {
			out = append(out, fmt.Sprintf("under NO operating-system preference %s resolves to %q, which is neither the light default (%q) nor a consistent theme",
				name, v, light.Resolved[name]))
		}
	}
	if none.BodyBackground != light.BodyBackground {
		out = append(out, fmt.Sprintf("under no preference the body is painted %s, and the named default (light) paints it %s",
			none.BodyBackground, light.BodyBackground))
	}
	return out
}

// readThemeTriple takes the three readings gradeThemeFollowsTheEnginesPreference decides
// from. Shared with the counterexample, so the mutation is measured by exactly the same
// route as the shipped page.
func readThemeTriple(t *testing.T, p *cdpPage, url string) (light, dark, none convTokens) {
	t.Helper()
	read := func(theme string) convTokens {
		p.loadConventions(t, url, convOpts{theme: theme})
		return p.collect(t).Tokens
	}
	return read("light"), read("dark"), read("")
}

func TestRenderedCDP_TheEnginesColourSchemePreferenceChoosesTheTheme(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	light, dark, none := readThemeTriple(t, p, ps.url)
	for _, prob := range gradeThemeFollowsTheEnginesPreference(light, dark, none) {
		t.Error(prob)
	}
}

// --- S6 / S7: the recorded ratios are the ratios the engine measures ---------------------

// gradeRecordedRatiosAgreeWithTheEngine is clauses S6 and S7 in one theme: every ratio
// the token file RECORDS beside a pair is recomputed from the two colours the engine
// resolved for that pair in that theme, and must agree with the record and clear the
// floor. A recorded ratio that disagrees with the measurement is worse than none.
func gradeRecordedRatiosAgreeWithTheEngine(theme string, tk convTokens, records []contrastRecord) []string {
	var out []string
	for _, rec := range records {
		fg, okFg := hexToRGBText(tk.Resolved[rec.Fg])
		bg, okBg := hexToRGBText(tk.Resolved[rec.Bg])
		if !okFg || !okBg {
			out = append(out, fmt.Sprintf("[%s] the engine resolved %s as %q and %s as %q; a recorded pair must name two token colours",
				theme, rec.Fg, tk.Resolved[rec.Fg], rec.Bg, tk.Resolved[rec.Bg]))
			continue
		}
		measured, ok := wcagContrast(fg, bg)
		if !ok {
			out = append(out, fmt.Sprintf("[%s] cannot measure %s on %s from %s and %s", theme, rec.Fg, rec.Bg, fg, bg))
			continue
		}
		recorded := rec.Light
		if theme == "dark" {
			recorded = rec.Dark
		}
		if math.Abs(measured-recorded) > 0.05 {
			out = append(out, fmt.Sprintf("[%s] %s records %s on %s as %.2f:1, and the engine measures %.2f:1 (%s on %s). A recorded ratio that disagrees with the measurement is worse than none",
				theme, tokenFileName, rec.Fg, rec.Bg, recorded, measured, fg, bg))
		}
		if measured < rec.Floor-0.005 {
			out = append(out, fmt.Sprintf("[%s] %s on %s measures %.2f:1, under its recorded floor of %.1f:1",
				theme, rec.Fg, rec.Bg, measured, rec.Floor))
		}
	}
	return out
}

func TestRenderedCDP_RecordedContrastRatiosAgreeWithTheEngineInBothThemes(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	records := readContrastRecords(t)
	for _, theme := range []string{"light", "dark"} {
		p.loadConventions(t, ps.url, convOpts{theme: theme})
		tk := p.collect(t).Tokens
		for _, prob := range gradeRecordedRatiosAgreeWithTheEngine(theme, tk, records) {
			t.Error(prob)
		}
	}
}

// --- F1: tab order and a visible focus indicator ------------------------------------------

// tabThroughEveryControl reads the controls the page offers in DOM order and then tabs
// through them for real, with engine key events - a KeyboardEvent constructed in the page
// is untrusted and moves focus nowhere - returning both readings for the grader below.
func tabThroughEveryControl(t *testing.T, p *cdpPage) (expected []convFocusable, reached []convFocused) {
	t.Helper()
	p.mustEval(`__hf.focusables()`, &expected)
	if len(expected) < 5 {
		t.Fatalf("the page offers %d keyboard-reachable controls; the dashboard has a token field, a filter and its buttons", len(expected))
	}
	for i := 0; i < len(expected)+2 && len(reached) < len(expected); i++ {
		p.pressTab(false)
		var f convFocused
		p.mustEval(`__hf.focused()`, &f)
		if f.None {
			continue
		}
		if len(reached) > 0 && reached[len(reached)-1].Index == f.Index {
			continue
		}
		reached = append(reached, f)
	}
	return expected, reached
}

// gradeTabOrderAndFocusRing is clause F1's keyboard half: Tab reaches every interactive
// control in the order the page is read, and each focused control draws an indicator that
// differs from its unfocused state and clears 3:1 against the colours next to it.
func gradeTabOrderAndFocusRing(theme string, expected []convFocusable, reached []convFocused) []string {
	var out []string
	unfocused := map[int]convFocusable{}
	for _, f := range expected {
		unfocused[f.Index] = f
	}

	if len(reached) != len(expected) {
		return []string{fmt.Sprintf("[%s] tabbing reached %d controls, and the page offers %d: %v vs %v",
			theme, len(reached), len(expected), namesOfFocused(reached), namesOfFocusable(expected))}
	}
	// IN THE ORDER THEY ARE READ: the index of each element among every element in the
	// document is its document order, and focus must arrive in that same order.
	for i, f := range reached {
		if f.Index != expected[i].Index {
			out = append(out, fmt.Sprintf("[%s] the %d%s Tab reaches %s; reading order puts %s there",
				theme, i+1, ordinalSuffix(i+1), f.What, expected[i].What))
		}
	}
	// A VISIBLE indicator: the focused control's computed style differs from its
	// unfocused one, and the indicator clears 3:1 against what is drawn next to it.
	for _, f := range reached {
		before, ok := unfocused[f.Index]
		if !ok {
			continue
		}
		if f.Outline == before.Outline && f.BoxShadow == before.BoxShadow && f.Border == before.Border {
			out = append(out, fmt.Sprintf("[%s] %s looks exactly the same focused as unfocused (outline %q, shadow %q, border %q): a keyboard user cannot see where they are",
				theme, f.What, f.Outline, f.BoxShadow, f.Border))
			continue
		}
		if f.OutlineStyle == "none" || f.OutlineWidth <= 0 {
			out = append(out, fmt.Sprintf("[%s] %s draws no focus outline while focused (%q)", theme, f.What, f.Outline))
			continue
		}
		// The colours ADJACENT to the indicator, which is what the 3:1 floor is
		// about. A ring drawn with a positive outline-offset does not touch the
		// control at all: the gap shows the surface behind it, so that surface is
		// what the ring is adjacent to on BOTH sides. A ring at zero or negative
		// offset overlaps the control, and then the control's own face is adjacent
		// to it too.
		adjacent := []struct{ what, colour string }{
			{"the surface it is drawn on", f.BehindIndicator},
		}
		if f.OutlineOffset <= 0 {
			adjacent = append(adjacent, struct{ what, colour string }{"the control's own face", f.OwnBackground})
		}
		for _, against := range adjacent {
			ratio, ok := wcagContrast(f.OutlineColor, against.colour)
			if !ok {
				out = append(out, fmt.Sprintf("[%s] %s: cannot measure the focus indicator %q against %s", theme, f.What, f.OutlineColor, against.colour))
				continue
			}
			if ratio < 2.995 {
				out = append(out, fmt.Sprintf("[%s] %s: the focus indicator %s is %.2f:1 against %s (%s), under the 3:1 floor",
					theme, f.What, f.OutlineColor, ratio, against.what, against.colour))
			}
		}
	}
	return out
}

func TestRenderedCDP_TabReachesEveryControlInReadingOrderWithAVisibleFocusRing(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	for _, theme := range []string{"light", "dark"} {
		p.loadConventions(t, ps.url, convOpts{theme: theme})
		expected, reached := tabThroughEveryControl(t, p)
		for _, prob := range gradeTabOrderAndFocusRing(theme, expected, reached) {
			t.Error(prob)
		}
	}
}

func namesOfFocused(fs []convFocused) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.What)
	}
	return out
}

func namesOfFocusable(fs []convFocusable) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.What)
	}
	return out
}

func ordinalSuffix(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return "th"
	case n%10 == 1:
		return "st"
	case n%10 == 2:
		return "nd"
	case n%10 == 3:
		return "rd"
	}
	return "th"
}

// --- F1: the accessibility tree the engine computed ---------------------------------------

// interactiveAXRoles is the set of roles a control takes in the accessibility tree. A
// node in this set with no accessible name is a control a screen-reader user meets as
// "button" and nothing else.
var interactiveAXRoles = map[string]bool{
	"button": true, "textbox": true, "searchbox": true, "link": true,
	"combobox": true, "checkbox": true, "radio": true, "slider": true,
	"spinbutton": true, "switch": true, "menuitem": true, "tab": true,
}

// gradeAccessibleNames is clause F1 read off the tree the ENGINE computed: every
// interactive control and every region heading exposes a non-empty accessible name, and
// no control is named only by its placeholder text.
func gradeAccessibleNames(nodes []axNode) []string {
	if len(nodes) < 20 {
		return []string{fmt.Sprintf("the engine computed %d accessibility nodes; the dashboard is bigger than that", len(nodes))}
	}
	var out []string
	controls, headings := 0, 0
	for _, n := range nodes {
		if n.Ignored {
			continue
		}
		role := n.Role.Value
		name := ""
		if n.Name != nil {
			name = strings.TrimSpace(n.Name.Value)
		}
		switch {
		case interactiveAXRoles[role]:
			controls++
			if name == "" {
				out = append(out, fmt.Sprintf("the %s at accessibility node %s has NO accessible name; a screen-reader user meets it as its role and nothing else", role, n.NodeID))
				continue
			}
			// A placeholder is not a label. If the ONLY source that contributed the name
			// is the placeholder attribute, the control is named by its own hint text -
			// which disappears the moment the reader types.
			if onlyNamedByPlaceholder(n) {
				out = append(out, fmt.Sprintf("the %s named %q is named ONLY by its placeholder text; F1 requires a real accessible name", role, name))
			}
		case role == "heading":
			headings++
			if name == "" {
				out = append(out, fmt.Sprintf("a region heading at accessibility node %s exposes no accessible name", n.NodeID))
			}
		}
	}
	if controls < 5 {
		out = append(out, fmt.Sprintf("the accessibility tree carries %d interactive controls; the dashboard has a token field, a filter and three buttons", controls))
	}
	if headings < 4 {
		out = append(out, fmt.Sprintf("the accessibility tree carries %d headings; the dashboard has two regions and their blocks", headings))
	}
	return out
}

func TestRenderedCDP_TheAccessibilityTreeNamesEveryControlAndEveryRegionHeading(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})

	for _, prob := range gradeAccessibleNames(p.axTree()) {
		t.Error(prob)
	}
}

func onlyNamedByPlaceholder(n axNode) bool {
	if n.Name == nil {
		return false
	}
	contributing := 0
	placeholder := 0
	for _, src := range n.Name.Sources {
		if src.Superseded || src.Invalid || src.Value == nil || strings.TrimSpace(src.Value.Value) == "" {
			continue
		}
		contributing++
		if src.Attribute == "placeholder" || src.Type == "placeholder" {
			placeholder++
		}
	}
	return contributing > 0 && contributing == placeholder
}

// --- F7: the three states, in every view, all distinct -------------------------------------

func TestRenderedCDP_EveryViewShowsItsLoadingEmptyAndUnreadableStatesInWords(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	cases := []struct {
		state string
		serve serveOpts
	}{
		{"loading", serveOpts{url: upstreamForTest, holdStream: true}},
		{"empty", serveOpts{url: upstreamForTest, snapshot: trulyEmptySnapshot()}},
		{"unreadable", serveOpts{url: upstreamForTest, rawSnapshot: `{"summary":`}},
	}
	// The states each view showed, per state, so they can be compared with each other.
	texts := map[string]map[string]string{}
	for _, c := range cases {
		ps := serveDocumentWith(t, c.serve)
		wait := fmt.Sprintf(`__hf.views().length === 4 && __hf.views().every(function (v) { return v.state === %q; })`, c.state)
		p.loadConventions(t, ps.url, convOpts{theme: "light", waitFor: wait})
		s := p.collect(t)
		if probs := gradeViewsAllIn(c.state)(s); probs != nil {
			for _, prob := range probs {
				t.Errorf("[%s] %s", c.state, prob)
			}
		}
		texts[c.state] = map[string]string{}
		for _, v := range s.Views {
			texts[c.state][v.View] = v.Text
		}
		// The controls stay operable in every one of the three states.
		for _, ctl := range s.Controls {
			if !ctl.Present || !ctl.Rendered {
				t.Errorf("[%s] the control %q is not on the page", c.state, ctl.ID)
			}
		}
		// And in the empty state, no view may still be claiming to be loading.
		if c.state == "empty" {
			for _, v := range s.Views {
				if v.State == "loading" {
					t.Errorf("the %s view still shows a loading state after a snapshot arrived", v.View)
				}
			}
		}
	}

	// DISTINCT: within one view, the three states say three different things. A page that
	// said "no data" for both "nothing to show" and "could not be read" would be lying
	// about one of them.
	for _, view := range []string{"counts", "queue", "aggs", "history"} {
		seen := map[string]string{}
		for _, state := range []string{"loading", "empty", "unreadable"} {
			txt := texts[state][view]
			if txt == "" {
				t.Errorf("the %s view says nothing at all in its %s state", view, state)
				continue
			}
			if other, dup := seen[txt]; dup {
				t.Errorf("the %s view says the same thing in its %s state and its %s state: %q",
					view, other, state, txt)
			}
			seen[txt] = state
		}
	}
}

// --- F3: absence is not zero -----------------------------------------------------------------

func TestRenderedCDP_NoCountTotalOrAggregateReadsAsZeroBeforeASnapshotArrives(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, holdStream: true})
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.connText() === "live" && __hf.views().every(function (v) { return v.state === "loading"; })`})
	s := p.collect(t)
	if probs := gradeNoZeroBeforeASnapshot(s); probs != nil {
		for _, prob := range probs {
			t.Error(prob)
		}
	}
	if strings.Contains(s.BodyText, "0 B") {
		t.Errorf("the page shows %q before any snapshot arrived; a total nobody has reported yet is not 0 bytes", "0 B")
	}
}

// gradeEveryUnmeasuredFieldReadsTheAbsencePhrase is clause F3 over a snapshot in which
// every nullable field arrived null: each such field reads the page's ONE absence phrase,
// in words, and never a zero, a blank, a bare dash or a second word for the same fact.
func gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(s convSnapshot) []string {
	var out []string
	if len(s.Figures.Cells) == 0 {
		return []string{"the page rendered no row cells at all, so this grader asserted nothing"}
	}
	// Every cell of every row: a value, or the ONE absence phrase. Never blank, never 0,
	// never a bare dash, and never a second word for the same fact.
	for _, c := range s.Figures.Cells {
		txt := strings.TrimSpace(c.Text)
		if txt == "" {
			out = append(out, fmt.Sprintf("%s renders an EMPTY cell; a fact nobody recorded reads %q, in words", c.What, absencePhrase))
			continue
		}
		if txt == "0" || txt == "-" || txt == "0 B" || txt == "0.0" || txt == "unknown" || txt == "n/a" {
			out = append(out, fmt.Sprintf("%s renders %q for a value nobody recorded; the page's one absence phrase is %q", c.What, txt, absencePhrase))
		}
	}
	// The fields the criterion names by hand, on a row where every one is null.
	wantAbsent := map[string]bool{
		"history td.size": true, "history td.vmaf": true, "history td.enc": true,
		"history td.dur": true, "history td.upd": true, "queue td.prog": true,
		"queue td.worker": true,
	}
	for _, c := range s.Figures.Cells {
		if wantAbsent[c.What] && strings.TrimSpace(c.Text) != absencePhrase {
			out = append(out, fmt.Sprintf("%s reads %q for an unmeasured value, want the page's one absence phrase %q",
				c.What, c.Text, absencePhrase))
		}
	}
	for _, f := range []struct{ what, v string }{
		{"reclaimed this run", s.Figures.ReclaimedSession},
		{"reclaimed lifetime", s.Figures.ReclaimedLifetime},
	} {
		if strings.TrimSpace(f.v) != absencePhrase {
			out = append(out, fmt.Sprintf("%q reads %q for a total the server sent as null, want %q", f.what, f.v, absencePhrase))
		}
	}
	// A figure no row contributed to reads the same phrase, never a zero or an average
	// of nothing.
	for _, a := range s.Figures.Aggs {
		if a.Out {
			continue
		}
		if strings.TrimSpace(a.Value) != absencePhrase {
			out = append(out, fmt.Sprintf("the figure %q reads %q with no row contributing a value, want %q", a.Title, a.Value, absencePhrase))
		}
	}
	return out
}

func TestRenderedCDP_EveryUnmeasuredFieldRendersTheOneAbsencePhrase(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: nullSnapshot()})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})

	for _, prob := range gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(p.collect(t)) {
		t.Error(prob)
	}
}

// --- F6: a severed stream stops every figure reading as live -----------------------------------

// gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure is clause F6: the connection state
// says so in words, the rows and figures stay on the screen, and nothing on the page goes
// on presenting itself as live. The elapsed figure is the one that would: it is recomputed
// on a one-second timer from each row's own transition timestamp, and that timer knows
// nothing about the stream.
func gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first convSnapshot, before, after []string) []string {
	var out []string
	if !strings.Contains(strings.ToLower(first.ConnText), "reconnect") {
		out = append(out, fmt.Sprintf("with the stream severed the page still reports its connection as %q", first.ConnText))
	}
	if len(first.Figures.Cells) == 0 {
		out = append(out, "a severed stream took the rows off the page; F6 keeps them and says they are stale")
	}
	if len(first.Figures.Aggs) != 6 {
		out = append(out, fmt.Sprintf("a severed stream left %d whole-ledger figures on the page, want all 6", len(first.Figures.Aggs)))
	}
	if len(before) == 0 {
		return append(out, "no elapsed figure was on the page at all, so this grader asserted nothing")
	}
	if strings.Join(before, "|") != strings.Join(after, "|") {
		out = append(out, fmt.Sprintf("with the stream severed the elapsed figures went on advancing: %v became %v. F6 refuses a figure that keeps reading as live",
			before, after))
	}
	return out
}

// severedStreamReading loads a page whose stream is severed and reads it twice, far enough
// apart that a figure still driven by the wall clock cannot help but move.
func severedStreamReading(t *testing.T, p *cdpPage, url string) (first convSnapshot, before, after []string) {
	t.Helper()
	p.loadConventions(t, url, convOpts{theme: "light",
		waitFor: `__hf.connText().indexOf("reconnecting") === 0 && __hf.rendered()`})
	first = p.collect(t)
	p.mustEval(`__hf.elapsedValues()`, &before)
	time.Sleep(3500 * time.Millisecond)
	p.mustEval(`__hf.elapsedValues()`, &after)
	return first, before, after
}

func TestRenderedCDP_ASeveredStreamKeepsTheRowsAndStopsEveryFigureAdvancing(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), streamFails: true})

	first, before, after := severedStreamReading(t, p, ps.url)
	for _, prob := range gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first, before, after) {
		t.Error(prob)
	}
}

// --- S9: reduced motion ------------------------------------------------------------------------

// gradeReducedMotionCostsNoValue is the second half of clause S9: under an emulated
// reduce preference the page "SHALL still render every value, state change and status the
// page carried with motion in place". Motion was decoration, so removing it removed
// nothing.
//
// It compares the two renders WITH THE LIVE CLOCK EXCLUDED, and that exclusion is the
// whole of the grader's honesty. The queue's elapsed column is rewritten every second
// from the wall clock, and the two renders cannot be taken at the same instant: the
// motion render waits for document.getAnimations() to drain, which costs real time and
// costs the reduce render none. A comparison of the whole of innerText therefore compares
// how OLD each page happened to be as well as what reduced motion did, and reds on a
// correct page whenever the difference straddles a second - which is what it did.
//
// The exclusion is structural (a selector in the probe), not a guess at which words look
// like a duration, and it is not a hole: the excluded cells are counted on both sides and
// each is required to still carry a value, so reduced motion cannot take that column
// away, blank it, or add a member to it without this grader saying so.
func gradeReducedMotionCostsNoValue(moving, still convSnapshot) []string {
	var out []string
	if moving.Stable.Cells == 0 {
		return []string{"the page rendered no live-clock figure at all, so the exclusion below covered nothing and this grader asserts less than it claims"}
	}
	if still.Stable.Cells != moving.Stable.Cells {
		out = append(out, fmt.Sprintf("the page renders %d live-clock figures with motion and %d with it reduced; reduced motion took a value off the page",
			moving.Stable.Cells, still.Stable.Cells))
	}
	for _, side := range []struct {
		what   string
		values []string
	}{{"with motion", moving.Stable.Values}, {"with it reduced", still.Stable.Values}} {
		for i, v := range side.values {
			if strings.TrimSpace(v) == "" {
				out = append(out, fmt.Sprintf("live-clock figure %d is EMPTY %s; it is a value the page carries and the comparison below excludes it", i+1, side.what))
			}
		}
	}
	if flattenText(still.Stable.Text) != flattenText(moving.Stable.Text) {
		out = append(out, fmt.Sprintf("the page shows different text with reduced motion (the live clock excluded from both readings).\nwith motion:    %q\nreduced motion: %q",
			flattenText(moving.Stable.Text), flattenText(still.Stable.Text)))
	}
	if still.Badges != moving.Badges {
		out = append(out, fmt.Sprintf("the badges read %+v with motion and %+v with it reduced", moving.Badges, still.Badges))
	}
	return out
}

// motionPair renders the same served document twice: once as it ships, once under an
// emulated prefers-reduced-motion: reduce.
func motionPair(t *testing.T, p *cdpPage, url string) (moving, still convSnapshot) {
	t.Helper()
	p.loadConventions(t, url, convOpts{theme: "light"})
	moving = p.collect(t)
	p.loadConventions(t, url, convOpts{theme: "light", reduce: true})
	still = p.collect(t)
	return moving, still
}

func TestRenderedCDP_ReducedMotionRemovesEveryDurationAndCostsNoValue(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	moving, still := motionPair(t, p, ps.url)
	if probs := gradeMotionExists(moving); probs != nil {
		t.Fatalf("%v", probs)
	}
	for _, prob := range gradeReducedMotion(still) {
		t.Error(prob)
	}
	for _, prob := range gradeReducedMotionCostsNoValue(moving, still) {
		t.Error(prob)
	}
}

// The comparison above must be insensitive to the wall clock BY CONSTRUCTION, not by
// landing inside the same second. This ages the motion render past a tick of the elapsed
// ticker before reading it - the same thing the animation drain does by accident, and the
// mechanism that red `make check` on a correct page - and then asserts two things at once:
// the excluded column really did move, so the reading is not a coincidence, and the
// grader reports nothing anyway.
func TestRenderedCDP_TheReducedMotionGraderIsInsensitiveToTheLiveClock(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})

	p.loadConventions(t, ps.url, convOpts{theme: "light"})
	// Age this page well past a tick before reading it.
	time.Sleep(2500 * time.Millisecond)
	moving := p.collect(t)

	p.loadConventions(t, ps.url, convOpts{theme: "light", reduce: true})
	still := p.collect(t)

	if strings.Join(moving.Stable.Values, "|") == strings.Join(still.Stable.Values, "|") {
		t.Fatalf("the two renders landed on the same second (%v), so this case measured nothing; the elapsed ticker or the fixture changed",
			moving.Stable.Values)
	}
	if probs := gradeReducedMotionCostsNoValue(moving, still); probs != nil {
		t.Errorf("the reduced-motion grader reds on a correct page whose two renders differ only in the wall-clock column (%v vs %v): %v",
			moving.Stable.Values, still.Stable.Values, probs)
	}
}

// --- F11: the Content-Security-Policy, through every state the page has ---------------------------

// gradeNoPolicyRefusal is clause F11 as the ENGINE reports it. Not that a policy header
// was set - that is asserted byte for byte in webui_test.go - but that the browser made
// no Content-Security-Policy or Trusted Types refusal while rendering the shipped page.
//
// It asserts a list is EMPTY, which is the shape of grader that most needs a
// counterexample: a dead instrument and a clean page report the same nothing. Its
// counterexample is TestRenderedCDP_ThePolicyGraderFailsAgainstAnOffOriginImage.
func gradeNoPolicyRefusal(what string, refusals []string) []string {
	var out []string
	for _, r := range refusals {
		out = append(out, fmt.Sprintf("%s: the browser refused something against the page: %s", what, r))
	}
	return out
}

func TestRenderedCDP_NoPolicyViolationInEitherThemeAtEitherWidthThroughEveryInteraction(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	live := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot()})
	hostile := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: hostileSnapshot()})
	refusing := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), controlStatus: 401})

	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int{360, 1280} {
			// A severed-stream server delivers its ONE snapshot to the first connection
			// and refuses every reconnection, so it is spent after a single load: each
			// case gets its own.
			severed := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), streamFails: true})
			for _, c := range []struct {
				name   string
				server *probeServer
				opts   convOpts
				drive  func()
			}{
				{name: "a live snapshot", server: live},
				{name: "a severed stream", server: severed, opts: convOpts{
					waitFor: `__hf.connText().indexOf("reconnecting") === 0 && __hf.rendered()`}},
				{name: "a snapshot carrying hostile text", server: hostile},
				{name: "a filter entry", server: live, drive: func() { p.typeInto("#filter", "alpha") }},
				{name: "a control action", server: refusing, drive: func() { clickControl(t, p, "#rescan") }},
			} {
				mark := b.logMark()
				o := c.opts
				o.theme, o.width, o.height = theme, width, 900
				p.loadConventions(t, c.server.url, o)
				if c.drive != nil {
					c.drive()
					time.Sleep(400 * time.Millisecond)
				}
				for _, prob := range gradeNoPolicyRefusal(
					fmt.Sprintf("[%s theme, %dpx, %s]", theme, width, c.name), b.securityRefusals(mark)) {
					t.Error(prob)
				}
			}
		}
	}

	// The policy is on the response, byte for byte, and that is asserted separately in
	// webui_test.go. What this grader adds is the browser's own verdict: it rendered the
	// page in both themes, at both widths, through four states, and refused nothing.
	if b.logMark() == 0 {
		t.Log("the browser raised no log entries at all")
	}
}

func clickControl(t *testing.T, p *cdpPage, selector string) {
	t.Helper()
	var box struct {
		X  float64 `json:"x"`
		Y  float64 `json:"y"`
		OK bool    `json:"ok"`
	}
	p.mustEval(`(function(){
	  const el = document.querySelector(`+jsString(selector)+`);
	  if (!el) return { ok: false, x: 0, y: 0 };
	  el.scrollIntoView({ block: "center" });
	  const r = el.getBoundingClientRect();
	  return { ok: true, x: r.left + r.width / 2, y: r.top + r.height / 2 };
	})()`, &box)
	if !box.OK {
		t.Fatalf("no control matched %q", selector)
	}
	p.clickAt(box.X, box.Y)
}

// --- F1 / F5: a refused control action ------------------------------------------------------

// gradeRefusedControlActionCostsNothingElse is clauses F1 and F5 on the control path: a
// refusal says IN WORDS that the action did not happen, renders nothing a reader could
// take for success, and costs the page nothing else - not a row, not a figure, not a
// badge, not a control, and not a policy refusal.
func gradeRefusedControlActionCostsNothingElse(before, after convSnapshot, refusals []string) []string {
	var out []string
	msg := strings.ToLower(after.Msg.Text)
	if !strings.Contains(msg, "nothing changed") {
		out = append(out, fmt.Sprintf("a refused control action reports %q; it must say, in words, that the action did not happen", after.Msg.Text))
	}
	for _, success := range []string{"done", " ok", "started", "rescanning"} {
		if strings.Contains(msg, success) {
			out = append(out, fmt.Sprintf("a refused control action reports %q, which reads as the action having succeeded", after.Msg.Text))
		}
	}
	// Every other region is still rendered and every control is still operable.
	if len(after.Figures.Cells) != len(before.Figures.Cells) || len(after.Figures.Aggs) != len(before.Figures.Aggs) {
		out = append(out, fmt.Sprintf("a refused control action cost the page its content: %d cells and %d figures became %d and %d",
			len(before.Figures.Cells), len(before.Figures.Aggs), len(after.Figures.Cells), len(after.Figures.Aggs)))
	}
	if after.Badges != before.Badges {
		out = append(out, fmt.Sprintf("a refused control action moved the badges from %+v to %+v; nothing on the server changed", before.Badges, after.Badges))
	}
	for _, ctl := range after.Controls {
		if !ctl.Rendered {
			out = append(out, fmt.Sprintf("a refused control action left %q off the screen", ctl.ID))
		}
	}
	out = append(out, gradeNoPolicyRefusal("driving a refused control action", refusals)...)
	// And the whole convention set still holds on the page that just refused an action.
	for _, g := range convGraders() {
		if g.name == "wide content scrolls inside its own container" {
			continue
		}
		for _, prob := range g.probe(after) {
			out = append(out, fmt.Sprintf("after a refused control action, %s: %s", g.name, prob))
		}
	}
	return out
}

// refuseAControlAction drives a REAL click on a control the server answers 401 to, and
// reads the page either side of it.
func refuseAControlAction(t *testing.T, b *cdpBrowser, p *cdpPage, url string) (before, after convSnapshot, refusals []string) {
	t.Helper()
	p.loadConventions(t, url, convOpts{theme: "light"})
	before = p.collect(t)
	mark := b.logMark()
	clickControl(t, p, "#rescan")
	if err := p.waitUntil(`__hf.msg().text !== "" && __hf.msg().text !== "working"`, 10*time.Second); err != nil {
		t.Fatalf("the page never reported the outcome of a refused control action: %v", err)
	}
	after = p.collect(t)
	return before, after, b.securityRefusals(mark)
}

func TestRenderedCDP_ARefusedControlActionSaysSoAndCostsNothingElse(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), controlStatus: 401})

	before, after, refusals := refuseAControlAction(t, b, p, ps.url)
	for _, prob := range gradeRefusedControlActionCostsNothingElse(before, after, refusals) {
		t.Error(prob)
	}
}

// --- F7 / F11: the snapshot endpoint answering with a server error ----------------------------

// gradeServerErrorStillRendersTheOfferAndTheControls is clauses F7 and F11 on the path
// where the page HAS scripting, asked the server for a snapshot, and was refused: every
// view shows its unreadable state in words, the AGPL source offer is still on the screen,
// every control is still there, and the browser refused nothing.
func gradeServerErrorStillRendersTheOfferAndTheControls(s convSnapshot, refusals []string) []string {
	out := gradeViewsAllIn("unreadable")(s)
	if !s.Offer.Present || !s.Offer.Shown {
		out = append(out, fmt.Sprintf("the AGPL source offer is not on the screen when the snapshot endpoint errors: %+v", s.Offer))
	}
	if !strings.Contains(s.Offer.Text, "Corresponding Source") {
		out = append(out, fmt.Sprintf("the source offer reads %q", s.Offer.Text))
	}
	for _, ctl := range s.Controls {
		if !ctl.Present || !ctl.Rendered {
			out = append(out, fmt.Sprintf("the control %q is not on the page when the snapshot endpoint errors", ctl.ID))
		}
	}
	return append(out, gradeNoPolicyRefusal("a server error on the snapshot endpoint", refusals)...)
}

func TestRenderedCDP_AServerErrorOnTheSnapshotEndpointStillRendersTheOfferAndTheControls(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, eventsStatus: 500})
	mark := b.logMark()
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.views().length === 4 && __hf.views().every(function (v) { return v.state === "unreadable"; })`})

	for _, prob := range gradeServerErrorStillRendersTheOfferAndTheControls(p.collect(t), b.securityRefusals(mark)) {
		t.Error(prob)
	}
}

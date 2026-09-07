package webui

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The RENDERED graders for the frontend and styling conventions (S0053).
//
// Every criterion decided here is a claim about what a reader SEES, so every one is
// decided by loading the SERVED document in a real browser engine and reading what the
// engine reported: the value it resolved for a custom property after the cascade and the
// media queries, a real layout box, the colour actually painted behind a run of text, the
// font the layout engine actually used, the accessibility tree it computed, and its own
// report of every policy refusal. None of it is decided by matching HTML or CSS text.
//
// The theme under test is set at the ENGINE, by media emulation. No grader in this file
// injects a class, an attribute or a stylesheet to choose a theme: a grader that did
// would be grading its own fixture rather than the shipped page.
//
// Every predicate is a pure function of a collected snapshot, and
// TestRenderedCDP_EveryConventionGraderFailsAgainstItsOwnMutation runs each one against a
// document deliberately mutated to defeat it. A check that cannot fail is not evidence.

// --- the collected snapshot ---------------------------------------------------------

type convTokens struct {
	Resolved       map[string]string `json:"resolved"`
	ColorScheme    string            `json:"colorScheme"`
	BodyBackground string            `json:"bodyBackground"`
	BodyColor      string            `json:"bodyColor"`
}

type convTextRun struct {
	What    string  `json:"what"`
	Text    string  `json:"text"`
	Fg      string  `json:"fg"`
	Bg      string  `json:"bg"`
	FgAlpha float64 `json:"fgAlpha"`
	Size    float64 `json:"size"`
	Weight  int     `json:"weight"`
}

type convTarget struct {
	What     string  `json:"what"`
	Tag      string  `json:"tag"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	InProse  bool    `json:"inProse"`
	Disabled bool    `json:"disabled"`
}

type convFocusable struct {
	Index      int    `json:"index"`
	What       string `json:"what"`
	Outline    string `json:"outline"`
	BoxShadow  string `json:"boxShadow"`
	Border     string `json:"border"`
	Background string `json:"background"`
}

type convFocused struct {
	None            bool    `json:"none"`
	Index           int     `json:"index"`
	What            string  `json:"what"`
	FocusVisible    bool    `json:"focusVisible"`
	Outline         string  `json:"outline"`
	OutlineColor    string  `json:"outlineColor"`
	OutlineWidth    float64 `json:"outlineWidth"`
	OutlineStyle    string  `json:"outlineStyle"`
	OutlineOffset   float64 `json:"outlineOffset"`
	BoxShadow       string  `json:"boxShadow"`
	Border          string  `json:"border"`
	Background      string  `json:"background"`
	BehindIndicator string  `json:"behindIndicator"`
	OwnBackground   string  `json:"ownBackground"`
}

type convView struct {
	View    string `json:"view"`
	State   string `json:"state"`
	Text    string `json:"text"`
	Shown   bool   `json:"shown"`
	Content int    `json:"content"`
}

type convLabel struct {
	What  string `json:"what"`
	Text  string `json:"text"`
	Words int    `json:"words"`
}

type convParagraph struct {
	What   string `json:"what"`
	Graded bool   `json:"graded"`
	Text   string `json:"text"`
}

type convDoclinks struct {
	Region string   `json:"region"`
	Count  int      `json:"count"`
	Hrefs  []string `json:"hrefs"`
	Texts  []string `json:"texts"`
	Shown  []bool   `json:"shown"`
}

type convScroller struct {
	What        string  `json:"what"`
	ScrollWidth float64 `json:"scrollWidth"`
	ClientWidth float64 `json:"clientWidth"`
}

type convLayout struct {
	InnerWidth      float64        `json:"innerWidth"`
	BodyScrollWidth float64        `json:"bodyScrollWidth"`
	BodyClientWidth float64        `json:"bodyClientWidth"`
	DocScrollWidth  float64        `json:"docScrollWidth"`
	DocClientWidth  float64        `json:"docClientWidth"`
	Scrollers       []convScroller `json:"scrollers"`
	Overflowing     []struct {
		What  string  `json:"what"`
		Right float64 `json:"right"`
		Width float64 `json:"width"`
	} `json:"overflowing"`
}

type convFont struct {
	Selector string  `json:"selector"`
	Want     string  `json:"want"`
	What     string  `json:"what"`
	Family   string  `json:"family"`
	Mono     bool    `json:"mono"`
	Missing  bool    `json:"missing"`
	Narrow   float64 `json:"narrow"`
	Wide     float64 `json:"wide"`
}

type convShadow struct {
	What   string `json:"what"`
	Shadow string `json:"shadow"`
}

type convMotion struct {
	Moving []struct {
		What       string  `json:"what"`
		Transition float64 `json:"transition"`
		Animation  float64 `json:"animation"`
	} `json:"moving"`
	Count         int     `json:"count"`
	MaxTransition float64 `json:"maxTransition"`
	MaxAnimation  float64 `json:"maxAnimation"`
}

type convPainted struct {
	Colours []struct {
		What  string `json:"what"`
		Prop  string `json:"prop"`
		Value string `json:"value"`
	} `json:"colours"`
	Lengths []struct {
		What  string  `json:"what"`
		Prop  string  `json:"prop"`
		Value float64 `json:"value"`
	} `json:"lengths"`
}

type convFigures struct {
	Cells []struct {
		What string `json:"what"`
		Text string `json:"text"`
	} `json:"cells"`
	Aggs []struct {
		Title string `json:"title"`
		Value string `json:"value"`
		Out   bool   `json:"out"`
		Text  string `json:"text"`
	} `json:"aggs"`
	Chips []struct {
		Key string `json:"key"`
		N   string `json:"n"`
	} `json:"chips"`
	ReclaimedSession  string `json:"reclaimedSession"`
	ReclaimedLifetime string `json:"reclaimedLifetime"`
}

type convControl struct {
	ID       string `json:"id"`
	Present  bool   `json:"present"`
	Rendered bool   `json:"rendered"`
	Disabled bool   `json:"disabled"`
}

// convSnapshot is one whole reading of the rendered page.
type convSnapshot struct {
	Tokens     convTokens
	TextRuns   []convTextRun
	Targets    []convTarget
	Views      []convView
	Labels     []convLabel
	Paragraphs []convParagraph
	Doclinks   []convDoclinks
	Layout     convLayout
	Fonts      []convFont
	Shadows    []convShadow
	Motion     convMotion
	Painted    convPainted
	Figures    convFigures
	Controls   []convControl
	BodyText   string
	ConnText   string
	Offer      struct {
		Present bool   `json:"present"`
		Shown   bool   `json:"shown"`
		Text    string `json:"text"`
	}
	Msg struct {
		Text string `json:"text"`
		Cls  string `json:"cls"`
	}
	Badges struct {
		Paused string `json:"paused"`
		Scan   string `json:"scan"`
	}
}

// fontExpectations is clause S5 written down: monospace where COLUMN ALIGNMENT carries
// meaning, and the system UI face for headings, explanatory text and controls. Each row
// is measured against the font the engine actually used, never against the stack asked
// for.
var fontExpectations = []struct {
	Selector string `json:"selector"`
	Want     string `json:"want"`
}{
	{"td.path", "mono"},
	{"td.elapsed", "mono"},
	{"td.dur", "mono"},
	{"td.upd", "mono"},
	{"td.worker", "mono"},
	{"td.size", "mono"},
	{"td.vmaf .mean", "mono"},
	{"#chips .chip .n", "mono"},
	{".reclaimed b", "mono"},
	{".buckets .bc", "mono"},
	{".spreadkeys .sv", "mono"},
	{"h1", "ui"},
	{"section > h2", "ui"},
	{"section > h3", "ui"},
	{".scope", "ui"},
	{".lbl", "ui"},
	{"button", "ui"},
	{".controls input", "ui"},
	{"th", "ui"},
	{".agg .agg-k", "ui"},
	{".agg .agg-cov", "ui"},
	{".vmaf .cond", "ui"},
}

// raisedSurfaces is the declared set of genuinely raised surfaces (S8). The header is
// painted OVER the document as it scrolls beneath it, which is what a shadow is for;
// every other surface on this page separates by border and surface token.
var raisedSurfaces = map[string]bool{"header": true}

// --- collecting -----------------------------------------------------------------------

// convOpts is everything a case can vary about the WORLD the page is measured in. The
// server it loads from is a separate argument, because several cases share one server and
// several need one of their own.
type convOpts struct {
	// theme is "light", "dark", or "" for NO PREFERENCE at the engine.
	theme string
	// reduce emulates prefers-reduced-motion: reduce.
	reduce bool
	// width and height are the CSS viewport; 1280x1024 when unset.
	width  int
	height int
	// waitFor is the readiness expression; a live, rendered snapshot when unset.
	waitFor string
}

const convReadyLive = `__hf.connText() === "live" && __hf.rendered()`

// load points the page at the served document under the media the engine is emulating,
// reinstalls the measuring script (a navigation wipes it) and waits for the page to have
// reached the state the case is about.
func (p *cdpPage) loadConventions(t *testing.T, url string, o convOpts) {
	t.Helper()
	if o.width == 0 {
		o.width, o.height = 1280, 1024
	}
	if o.height == 0 {
		o.height = 1024
	}
	p.viewport(o.width, o.height)
	p.emulate(o.theme, o.reduce)
	p.navigate(url + "/")
	p.mustEval(conventionsProbeJS, nil)
	ready := o.waitFor
	if ready == "" {
		ready = convReadyLive
	}
	if err := p.waitUntil(ready, 25*time.Second); err != nil {
		var conn string
		_ = p.eval(`__hf.connText()`, &conn)
		t.Fatalf("%v (connection state %q)\nbrowser output:\n%s", err, conn, p.b.browserLog())
	}
	// Then wait for the page to STOP MOVING. This page has transitions on its affordances
	// (S9 requires that they exist at all, or the reduce grader would assert nothing), and
	// a colour read while one is running is an interpolated value that belongs to no
	// token. document.getAnimations() is the engine's own list of what is still in flight.
	if err := p.waitUntil(`document.getAnimations().every(function (a) { return a.playState !== "running"; })`,
		5*time.Second); err != nil {
		time.Sleep(400 * time.Millisecond)
	}
}

func (p *cdpPage) collect(t *testing.T) convSnapshot {
	t.Helper()
	spec, _ := json.Marshal(fontExpectations)
	var s convSnapshot
	for _, part := range []struct {
		expr string
		out  any
	}{
		{`JSON.stringify(__hf.tokens())`, &s.Tokens},
		{`JSON.stringify(__hf.textRuns())`, &s.TextRuns},
		{`JSON.stringify(__hf.pointerTargets())`, &s.Targets},
		{`JSON.stringify(__hf.views())`, &s.Views},
		{`JSON.stringify(__hf.labels())`, &s.Labels},
		{`JSON.stringify(__hf.mainParagraphs())`, &s.Paragraphs},
		{`JSON.stringify(__hf.doclinks())`, &s.Doclinks},
		{`JSON.stringify(__hf.layout())`, &s.Layout},
		{`JSON.stringify(__hf.fonts(` + string(spec) + `))`, &s.Fonts},
		{`JSON.stringify(__hf.shadows())`, &s.Shadows},
		{`JSON.stringify(__hf.motion())`, &s.Motion},
		{`JSON.stringify(__hf.painted())`, &s.Painted},
		{`JSON.stringify(__hf.figures())`, &s.Figures},
		{`JSON.stringify(__hf.controls())`, &s.Controls},
		{`JSON.stringify(__hf.sourceOffer())`, &s.Offer},
		{`JSON.stringify(__hf.msg())`, &s.Msg},
		{`JSON.stringify(__hf.badges())`, &s.Badges},
	} {
		var raw string
		p.mustEval(part.expr, &raw)
		if err := json.Unmarshal([]byte(raw), part.out); err != nil {
			t.Fatalf("decoding %s: %v\n%s", part.expr, err, raw)
		}
	}
	p.mustEval(`__hf.bodyText()`, &s.BodyText)
	p.mustEval(`__hf.connText()`, &s.ConnText)
	return s
}

// --- the graders, each a pure function of the snapshot ---------------------------------

// gradeTextContrast is clause F1's contrast floor over EVERY run of text the engine
// rendered, measured against the colour it actually painted behind that run. The floors
// are WCAG 2.2's own: 4.5:1, relaxed to 3:1 for large text (24px, or 18.66px and bold).
func gradeTextContrast(s convSnapshot) []string {
	var out []string
	if len(s.TextRuns) < 10 {
		return []string{fmt.Sprintf("only %d runs of text were measured; the page did not render", len(s.TextRuns))}
	}
	for _, r := range s.TextRuns {
		floor, kind := 4.5, "text"
		if r.Size >= 24 || (r.Size >= 18.66 && r.Weight >= 700) {
			floor, kind = 3.0, "large text"
		}
		ratio, ok := wcagContrast(r.Fg, r.Bg)
		if !ok {
			out = append(out, fmt.Sprintf("%s: the engine reported an unreadable colour pair (%s on %s)", r.What, r.Fg, r.Bg))
			continue
		}
		if ratio < floor-0.005 {
			out = append(out, fmt.Sprintf("%s (%q) is %.2f:1 (%s on %s), under the %.1f:1 %s floor",
				r.What, r.Text, ratio, r.Fg, r.Bg, floor, kind))
		}
	}
	return out
}

// gradePointerTargets is clause F1's target size: every pointer target is painted in a box
// at least 24 by 24 CSS pixels, measured from the engine's own layout geometry. A link
// inside a sentence of prose is WCAG 2.2's own exemption and is skipped by name.
func gradePointerTargets(s convSnapshot) []string {
	var out []string
	measured := 0
	for _, t := range s.Targets {
		if t.Tag == "a" && t.InProse {
			continue
		}
		measured++
		if t.W < 23.5 || t.H < 23.5 {
			out = append(out, fmt.Sprintf("%s is painted %.1f by %.1f CSS px, under the 24 by 24 pointer-target floor",
				t.What, t.W, t.H))
		}
	}
	if measured == 0 {
		out = append(out, "no pointer target was measured at all, so this grader asserted nothing")
	}
	return out
}

// gradeLabelLength is clause F8's bound on what stays on the surface: no explanatory or
// scope label longer than 15 words, measured from the text the engine reports as visible.
// It also refuses a paragraph inside a region that is outside the graded set, so a long
// explanation cannot come back by wearing a class the bound does not cover.
func gradeLabelLength(s convSnapshot) []string {
	var out []string
	if len(s.Labels) < 5 {
		out = append(out, fmt.Sprintf("only %d labels were measured; the page did not render its labels", len(s.Labels)))
	}
	for _, l := range s.Labels {
		if l.Words > 15 {
			out = append(out, fmt.Sprintf("%s is %d words long: %q. Clause F8 keeps a scope label to a few words and moves the paragraphs into the repo's docs",
				l.What, l.Words, l.Text))
		}
	}
	for _, p := range s.Paragraphs {
		if !p.Graded {
			out = append(out, fmt.Sprintf("%s is a paragraph inside a region that no label rule covers: %q", p.What, p.Text))
		}
	}
	return out
}

// gradeDocLinks is clause F8's other half: exactly one link per region to the repository's
// own documentation for that region's methodology, and it has to have reached the screen.
func gradeDocLinks(s convSnapshot) []string {
	var out []string
	if len(s.Doclinks) == 0 {
		return []string{"the page rendered no regions at all"}
	}
	for _, r := range s.Doclinks {
		if r.Count != 1 {
			out = append(out, fmt.Sprintf("the region %s renders %d documentation links, want exactly one", r.Region, r.Count))
			continue
		}
		if !r.Shown[0] {
			out = append(out, fmt.Sprintf("the region %s renders a documentation link a reader never sees", r.Region))
		}
		if strings.TrimSpace(r.Texts[0]) == "" {
			out = append(out, fmt.Sprintf("the region %s renders a documentation link with no visible text", r.Region))
		}
	}
	return out
}

// gradeBodyNeverScrollsSideways is clause F9: at a 360px viewport the document body's
// scroll width is no greater than the viewport's.
func gradeBodyNeverScrollsSideways(s convSnapshot) []string {
	var out []string
	if s.Layout.InnerWidth <= 0 {
		return []string{"the engine reported no viewport width"}
	}
	for _, c := range []struct {
		what  string
		width float64
	}{
		{"the document body's", s.Layout.BodyScrollWidth},
		{"the document element's", s.Layout.DocScrollWidth},
	} {
		if c.width > s.Layout.InnerWidth+1 {
			names := []string{}
			for _, o := range s.Layout.Overflowing {
				names = append(names, fmt.Sprintf("%s (right edge %.0f)", o.What, o.Right))
			}
			out = append(out, fmt.Sprintf("%s scroll width is %.0f at a %.0f px viewport, so the page scrolls sideways. Sticking out: %s",
				c.what, c.width, s.Layout.InnerWidth, strings.Join(names, ", ")))
		}
	}
	return out
}

// gradeWideContentScrollsInItsOwnContainer is the rest of F9: wide content takes a scroll
// of its OWN, proved by that container's scroll width exceeding its client width while
// the body's does not.
func gradeWideContentScrollsInItsOwnContainer(s convSnapshot) []string {
	for _, c := range s.Layout.Scrollers {
		if c.ScrollWidth > c.ClientWidth+1 {
			return nil
		}
	}
	return []string{fmt.Sprintf("no container on the page takes a horizontal scroll of its own at a %.0f px viewport, so wide content is either crushed or pushing the body sideways (containers seen: %v)",
		s.Layout.InnerWidth, s.Layout.Scrollers)}
}

// gradeFontSplit is clause S5, measured from the font the engine ACTUALLY USED: two runs
// of text of equal length in that font, one of narrow glyphs and one of wide, have the
// same advance width if and only if the face is fixed-advance.
func gradeFontSplit(s convSnapshot) []string {
	var out []string
	if len(s.Fonts) == 0 {
		return []string{"no font reading was taken at all"}
	}
	for _, f := range s.Fonts {
		if f.Missing {
			out = append(out, fmt.Sprintf("no element matched %q, so the family rule for it asserted nothing", f.Selector))
			continue
		}
		switch f.Want {
		case "mono":
			if !f.Mono {
				out = append(out, fmt.Sprintf("%s (%s) is laid out in a PROPORTIONAL face (%q: 16 narrow glyphs measure %.1f, 16 wide ones %.1f); its column alignment carries meaning, so S5 requires monospace",
					f.Selector, f.What, f.Family, f.Narrow, f.Wide))
			}
		case "ui":
			if f.Mono {
				out = append(out, fmt.Sprintf("%s (%s) is laid out in a MONOSPACE face (%q); S5 puts headings, explanatory text and controls in the system UI face",
					f.Selector, f.What, f.Family))
			}
		}
	}
	return out
}

// gradeDepthScale is clause S8: only a genuinely raised surface computes a shadow, it
// comes from the one shadow token, and every other surface separates by border and
// surface token with no shadow at all.
func gradeDepthScale(s convSnapshot) []string {
	var out []string
	seen := map[string]bool{}
	values := map[string]bool{}
	for _, sh := range s.Shadows {
		tag := strings.SplitN(sh.What, "#", 2)[0]
		tag = strings.SplitN(tag, ".", 2)[0]
		values[sh.Shadow] = true
		if !raisedSurfaces[tag] {
			out = append(out, fmt.Sprintf("%s computes a shadow (%s) but is not a raised surface; S8 separates every other surface by border and surface token",
				sh.What, sh.Shadow))
			continue
		}
		seen[tag] = true
	}
	for want := range raisedSurfaces {
		if !seen[want] {
			out = append(out, fmt.Sprintf("the raised surface %q computes no shadow at all, so the depth scale is never applied and this grader would assert nothing", want))
		}
	}
	if len(values) > 3 {
		out = append(out, fmt.Sprintf("the page paints %d distinct shadow values; S8 allows two or three shadow tokens", len(values)))
	}
	return out
}

// gradeReducedMotion is clause S9 under an emulated `prefers-reduced-motion: reduce`: the
// engine computes no non-zero transition or animation duration anywhere on the page.
func gradeReducedMotion(s convSnapshot) []string {
	if s.Motion.MaxTransition == 0 && s.Motion.MaxAnimation == 0 {
		return nil
	}
	var names []string
	for _, m := range s.Motion.Moving {
		names = append(names, fmt.Sprintf("%s (transition %.3fs, animation %.3fs)", m.What, m.Transition, m.Animation))
	}
	return []string{fmt.Sprintf("%d element(s) still compute a non-zero duration under prefers-reduced-motion: reduce: %s",
		s.Motion.Count, strings.Join(names, ", "))}
}

// gradeMotionExists is the anti-vacuity half of S9. A page with no motion at all honours
// the reduce query trivially, and a grader that only ever measured such a page could
// never fail. So the page must compute motion WITHOUT the preference in force.
func gradeMotionExists(s convSnapshot) []string {
	if s.Motion.MaxTransition > 0 || s.Motion.MaxAnimation > 0 {
		return nil
	}
	return []string{"the page computes no transition or animation at all without prefers-reduced-motion, so the reduce grader asserts nothing"}
}

// gradePaintedValuesComeFromTokens is clauses S1 and S3 as the ENGINE sees them: every
// colour the page actually painted and every spacing length it actually computed is a
// value the token file declares. This is the half no text check can do - a token the
// stylesheet defines but the cascade never applies, or a value arriving from a place the
// text sweep never looked, is caught here and only here.
func gradePaintedValuesComeFromTokens(s convSnapshot) []string {
	colours, lengths := tokenValueSets(s.Tokens)
	if len(colours) == 0 || len(lengths) == 0 {
		return []string{"the engine resolved no token values at all, so nothing could be checked against them"}
	}
	var out []string
	seenColours, seenLengths := 0, 0
	for _, c := range s.Painted.Colours {
		seenColours++
		if !colours[normaliseRGB(c.Value)] {
			out = append(out, fmt.Sprintf("%s paints %s: %s, which is not any value the token file declares",
				c.What, c.Prop, c.Value))
		}
	}
	for _, l := range s.Painted.Lengths {
		seenLengths++
		if !lengths[roundLen(math.Abs(l.Value))] {
			out = append(out, fmt.Sprintf("%s computes %s: %gpx, which is not any length the token file declares",
				l.What, l.Prop, l.Value))
		}
	}
	if seenColours < 10 || seenLengths < 10 {
		out = append(out, fmt.Sprintf("only %d colours and %d lengths were swept; the page did not render",
			seenColours, seenLengths))
	}
	return out
}

// tokenValueSets turns the token values the ENGINE resolved into the two sets every
// painted value must come from.
func tokenValueSets(tk convTokens) (colours map[string]bool, lengths map[int]bool) {
	colours, lengths = map[string]bool{}, map[int]bool{}
	// Transparent is not a token value; it is the absence of paint, and the sweep already
	// skips it. rgba(0,0,0,0) is admitted so a fully transparent computed value that
	// slipped through cannot be reported as an unknown colour.
	colours["rgb(0, 0, 0)/0"] = true
	for _, v := range tk.Resolved {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if rgb, ok := hexToRGBText(v); ok {
			colours[rgb] = true
		}
		for _, m := range regexp.MustCompile(`(\d+(?:\.\d+)?)px`).FindAllStringSubmatch(v, -1) {
			f, err := strconv.ParseFloat(m[1], 64)
			if err == nil {
				lengths[roundLen(f)] = true
			}
		}
	}
	return colours, lengths
}

func roundLen(v float64) int { return int(math.Round(v * 100)) }

var hexRe = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)

func hexToRGBText(v string) (string, bool) {
	m := hexRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return "", false
	}
	r, _ := strconv.ParseInt(m[1][0:2], 16, 32)
	g, _ := strconv.ParseInt(m[1][2:4], 16, 32)
	b, _ := strconv.ParseInt(m[1][4:6], 16, 32)
	return fmt.Sprintf("rgb(%d, %d, %d)", r, g, b), true
}

func normaliseRGB(v string) string { return strings.TrimSpace(v) }

// gradeViewsAllIn is clause F7: every view says, in words, which of the three states it
// is in, and it is the state the case put it in.
func gradeViewsAllIn(want string) func(convSnapshot) []string {
	return func(s convSnapshot) []string {
		var out []string
		if len(s.Views) != 4 {
			return []string{fmt.Sprintf("the page renders %d views, want the four the dashboard has (counts, queue, aggs, history)", len(s.Views))}
		}
		for _, v := range s.Views {
			if v.State != want {
				out = append(out, fmt.Sprintf("the %s view is in state %q, want %q (it shows %q)", v.View, v.State, want, v.Text))
				continue
			}
			if !v.Shown {
				out = append(out, fmt.Sprintf("the %s view's %s state is in the document but never reached the screen", v.View, want))
			}
			if len(strings.Fields(v.Text)) < 2 {
				out = append(out, fmt.Sprintf("the %s view's %s state is %q, which is not a state in words", v.View, want, v.Text))
			}
		}
		return out
	}
}

// gradeNoZeroBeforeASnapshot is clause F3's other half: a page that is connected and has
// been given nothing must not render a count, a total or an aggregate as 0.
func gradeNoZeroBeforeASnapshot(s convSnapshot) []string {
	var out []string
	for _, c := range s.Figures.Chips {
		out = append(out, fmt.Sprintf("a count chip (%s = %q) is on screen before any snapshot arrived", c.Key, c.N))
	}
	for _, a := range s.Figures.Aggs {
		out = append(out, fmt.Sprintf("a whole-ledger figure (%s = %q) is on screen before any snapshot arrived", a.Title, a.Value))
	}
	for _, f := range []struct{ what, v string }{
		{"reclaimed this run", s.Figures.ReclaimedSession},
		{"reclaimed lifetime", s.Figures.ReclaimedLifetime},
	} {
		if zeroLike(f.v) {
			out = append(out, fmt.Sprintf("%q reads %q before any snapshot arrived; a total nobody has reported yet is not 0", f.what, f.v))
		}
	}
	return out
}

var zeroRe = regexp.MustCompile(`(^|[^0-9.])0([^0-9.]|$)`)

// zeroLike reports whether a rendered figure reads as zero to a reader: a bare 0, a 0 with
// a unit, a 0.0, or a dash standing in for one.
func zeroLike(v string) bool {
	t := strings.TrimSpace(v)
	if t == "" {
		return true
	}
	// Every dash a reader would take for a zero, decided by CODE POINT rather than by a
	// dash character written into this file: clause F3 forbids "a dash a reader will read
	// as zero", and that is the ASCII hyphen, the whole Unicode dash block (U+2010 hyphen
	// through U+2015 horizontal bar, which includes the en and em dashes), and the minus
	// sign U+2212.
	if r := []rune(t); len(r) == 1 {
		if c := r[0]; c == '-' || (c >= 0x2010 && c <= 0x2015) || c == 0x2212 {
			return true
		}
	}
	if zeroRe.MatchString(t) {
		return true
	}
	return strings.HasPrefix(t, "0.0") || strings.HasPrefix(t, "0%") || strings.HasPrefix(t, "0 ")
}

// --- reading the token file's recorded contrast ratios (S7) ---------------------------

type contrastRecord struct {
	Fg, Bg string
	Floor  float64
	Dark   float64
	Light  float64
}

var contrastRecordRe = regexp.MustCompile(
	`contrast:\s*(--[-\w]+)\s+on\s+(--[-\w]+)\s*\|\s*floor\s*([0-9.]+)\s*\|\s*dark\s*([0-9.]+)\s*\|\s*light\s*([0-9.]+)`)

func readContrastRecords(t *testing.T) []contrastRecord {
	t.Helper()
	body := readSurfaceSource(t, tokenFileName)
	var out []contrastRecord
	for _, m := range contrastRecordRe.FindAllStringSubmatch(body, -1) {
		floor, _ := strconv.ParseFloat(m[3], 64)
		dark, _ := strconv.ParseFloat(m[4], 64)
		light, _ := strconv.ParseFloat(m[5], 64)
		out = append(out, contrastRecord{Fg: m[1], Bg: m[2], Floor: floor, Dark: dark, Light: light})
	}
	if len(out) == 0 {
		t.Fatalf("%s records no measured contrast ratio at all; clause S7 requires one beside every pair that must clear a floor", tokenFileName)
	}
	return out
}

// --- the graders as a table, so every one can be defeated on purpose --------------------

type convGrader struct {
	name  string
	probe func(convSnapshot) []string
}

// convGraders is every predicate this file decides on a LIVE page, by name. The mutation
// test below drives each one against a document built to defeat it.
func convGraders() []convGrader {
	return []convGrader{
		{"every run of text clears its contrast floor", gradeTextContrast},
		{"every pointer target is at least 24 by 24", gradePointerTargets},
		{"no on-surface label runs past fifteen words", gradeLabelLength},
		{"each region carries exactly one documentation link", gradeDocLinks},
		{"the body never scrolls sideways", gradeBodyNeverScrollsSideways},
		{"wide content scrolls inside its own container", gradeWideContentScrollsInItsOwnContainer},
		{"data is monospace and prose is the system face", gradeFontSplit},
		{"only a raised surface computes a shadow", gradeDepthScale},
		{"every painted value came from the token file", gradePaintedValuesComeFromTokens},
		{"the page computes motion at all", gradeMotionExists},
	}
}

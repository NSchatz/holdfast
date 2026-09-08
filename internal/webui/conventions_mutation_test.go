package webui

import (
	"strings"
	"testing"
	"time"
)

// EVERY grader this item adds is run against a document deliberately built to defeat it.
//
// This is the part S0035 died for the want of. A grader that cannot fail is not evidence,
// and the only way to know one can fail is to defeat it on purpose and watch it report.
// Each row below names the grader it is aimed at, and the test refuses a row whose
// mutation did not change the served document at all - which is how a counterexample
// quietly stops being one.

func TestRenderedCDP_EveryConventionGraderFailsAgainstItsOwnMutation(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	plain := servedDocument(t)

	for _, c := range []struct {
		name    string
		defeats string
		mutate  func([]byte) []byte
		opts    convOpts
	}{
		{
			name:    "an explanatory label recoloured onto the surface behind it",
			defeats: "every run of text clears its contrast floor",
			mutate:  cssMutation(`.scope, .agg-cov { color: var(--line) !important; }`),
		},
		{
			name:    "every button crushed below the target floor",
			defeats: "every pointer target is at least 24 by 24",
			mutate: cssMutation(`button { min-height: 0 !important; min-width: 0 !important;` +
				` padding: 0 !important; font-size: 8px !important; line-height: 1 !important; }`),
		},
		{
			name:    "a paragraph of methodology put back on the surface",
			defeats: "no on-surface label runs past fifteen words",
			mutate: scriptMutation(`var s=document.querySelector(".scope");` +
				`if (s) s.textContent = "Elapsed is how long the file has been in the state it is in now, ` +
				`recomputed from the transition timestamp on each update rather than counted in this page.";`),
		},
		{
			name:    "a region's documentation link taken away",
			defeats: "each region carries exactly one documentation link",
			mutate: func(b []byte) []byte {
				return []byte(strings.Replace(string(b), `class="doclink"`, `class="notadoclink"`, 1))
			},
		},
		{
			name:    "a region's documentation link duplicated",
			defeats: "each region carries exactly one documentation link",
			mutate: func(b []byte) []byte {
				s := string(b)
				i := strings.Index(s, `<p class="docs">`)
				j := strings.Index(s[i:], `</p>`)
				if i < 0 || j < 0 {
					return b
				}
				block := s[i : i+j+4]
				return []byte(strings.Replace(s, block, block+block, 1))
			},
		},
		{
			name:    "content wider than the phone viewport pushing the body sideways",
			defeats: "the body never scrolls sideways",
			mutate:  cssMutation(`main { min-width: 1200px; }`),
			opts:    convOpts{width: 360, height: 900},
		},
		{
			name:    "the tables allowed to spill instead of scrolling in their own box",
			defeats: "wide content scrolls inside its own container",
			mutate:  cssMutation(`.tablewrap { overflow-x: visible !important; } table { min-width: 0 !important; }`),
			opts:    convOpts{width: 360, height: 900},
		},
		{
			name:    "a data column put into the prose face",
			defeats: "data is monospace and prose is the system face",
			mutate:  cssMutation(`td.path { font-family: var(--font-ui) !important; }`),
		},
		{
			name:    "a heading put into the data face",
			defeats: "data is monospace and prose is the system face",
			mutate:  cssMutation(`section > h2 { font-family: var(--font-mono) !important; }`),
		},
		{
			name:    "a shadow given to a surface that is not raised",
			defeats: "only a raised surface computes a shadow",
			mutate:  cssMutation(`.agg { box-shadow: var(--shadow-raised) !important; }`),
		},
		{
			name:    "the raised surface's shadow taken away",
			defeats: "only a raised surface computes a shadow",
			mutate:  cssMutation(`header { box-shadow: none !important; }`),
		},
		{
			name:    "a colour that is in no token painted onto a control",
			defeats: "every painted value came from the token file",
			mutate:  cssMutation(`#chips .chip { background: #123456 !important; }`),
		},
		{
			name:    "an off-scale spacing length applied by the cascade",
			defeats: "every painted value came from the token file",
			mutate:  cssMutation(`#chips .chip { padding: 7px 9px !important; }`),
		},
		{
			name:    "every transition removed, so the reduce grader would assert nothing",
			defeats: "the page computes motion at all",
			mutate:  cssMutation(`* { transition: none !important; animation: none !important; }`),
		},
	} {
		if string(c.mutate([]byte(plain))) == plain {
			t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", c.name)
		}
		ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: c.mutate})
		o := c.opts
		if o.theme == "" {
			o.theme = "light"
		}
		p.loadConventions(t, ps.url, o)
		s := p.collect(t)

		found := false
		for _, g := range convGraders() {
			if g.name != c.defeats {
				continue
			}
			found = true
			probs := g.probe(s)
			if probs == nil {
				t.Errorf("the grader %q PASSED a document mutated to defeat it (%s)", g.name, c.name)
			} else {
				t.Logf("%-62s defeats %-52s -> %s", c.name, c.defeats, probs[0])
			}
		}
		if !found {
			t.Fatalf("the mutation %q names a grader that does not exist: %q", c.name, c.defeats)
		}
	}
}

// The reduce grader gets its own case: its subject is what the engine computes UNDER an
// emulated preference, so its counterexample has to be rendered under that preference
// too. A rule that outlives the reduce block is exactly the regression it exists to catch.
func TestRenderedCDP_TheReducedMotionGraderFailsAgainstAMotionThatSurvivesTheQuery(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	plain := servedDocument(t)

	mutate := cssMutation(`@media (prefers-reduced-motion: reduce) {` +
		` .badge { transition-duration: 3s !important; } }`)
	if string(mutate([]byte(plain))) == plain {
		t.Fatal("the mutation did not change the served document - the assertion below would be vacuous")
	}
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light", reduce: true})
	s := p.collect(t)
	probs := gradeReducedMotion(s)
	if probs == nil {
		t.Error("the reduced-motion grader PASSED a document whose transition survives prefers-reduced-motion: reduce")
	} else {
		t.Logf("a transition that survives the reduce query -> %s", probs[0])
	}
}

// And the three-state grader: a view left claiming to be loading after a snapshot has
// arrived is the failure F7 is written against, and the grader has to report it.
func TestRenderedCDP_TheThreeStateGraderFailsAgainstAViewStuckOnLoading(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	plain := servedDocument(t)

	// Leave the queue view claiming to be loading after its snapshot has arrived - the
	// page that never notices its data landed, which is the failure F7 is written for.
	mutate := scriptMutation(`var q=document.querySelector('[data-view="queue"] [data-state]');` +
		`if (q) { q.dataset.state="loading"; var td=q.querySelector("td");` +
		`if (td) td.textContent="Loading the queue."; }`)
	if string(mutate([]byte(plain))) == plain {
		t.Fatal("the mutation did not change the served document - the assertion below would be vacuous")
	}
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: trulyEmptySnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.views().length === 4 && __hf.views().some(function (v) { return v.view === "queue" && v.state === "loading"; })`})
	s := p.collect(t)
	if probs := gradeViewsAllIn("empty")(s); probs == nil {
		t.Errorf("the three-state grader PASSED a page with a view stuck on its loading state: %+v", s.Views)
	} else {
		t.Logf("a view stuck on loading -> %s", probs[0])
	}
}

// And the "absence is not zero" grader, which is what stops a page full of zeroes from
// reading as a page full of measurements.
func TestRenderedCDP_TheNoZeroBeforeASnapshotGraderFailsAgainstAZeroedTotal(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()
	plain := servedDocument(t)

	mutate := scriptMutation(`var e=document.getElementById("reclaimed-lifetime");` +
		`if (e) e.textContent = "0 B";`)
	if string(mutate([]byte(plain))) == plain {
		t.Fatal("the mutation did not change the served document - the assertion below would be vacuous")
	}
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, holdStream: true, mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.connText() === "live" && document.getElementById("reclaimed-lifetime").textContent === "0 B"`})
	s := p.collect(t)
	if probs := gradeNoZeroBeforeASnapshot(s); probs == nil {
		t.Error("the grader PASSED a page rendering a lifetime total as 0 B before any snapshot arrived")
	} else {
		t.Logf("a zeroed total before a snapshot -> %s", probs[0])
	}
}

// --- the graders that decide from MORE than one reading -------------------------------
//
// Everything above defeats a predicate over a single collected snapshot. The graders
// below decide their criterion from something else - a pair of renders, three renders
// under three preferences, a real tab walk, the accessibility tree the engine computed,
// the engine's own log of what it refused - so none of them could join convGraders()'s
// table, and none of them had a counterexample. Each one owes the same debt the table
// pays: a document built to defeat it, run through the grader's OWN predicate, failing
// the case if the grader passes.
//
// The CSP grader is the sharpest of them and the reason this list exists, because it
// asserts a list is EMPTY: a dead instrument and a clean page report the same nothing,
// and only a page the engine must refuse can tell the two apart.

// mustChangeTheDocument refuses a mutation that did not change the served document at all,
// which is how a counterexample quietly stops being one.
func mustChangeTheDocument(t *testing.T, name string, mutate func([]byte) []byte) func([]byte) []byte {
	t.Helper()
	plain := servedDocument(t)
	if string(mutate([]byte(plain))) == plain {
		t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", name)
	}
	return mutate
}

// F11. An off-origin image, which `default-src 'none'` must refuse. If the engine's
// refusal log does not report it, the grader that asserts that log is empty is blind and
// would pass whatever the page did.
func TestRenderedCDP_ThePolicyGraderFailsAgainstAnOffOriginImage(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	const marker = "mutation-off-origin"
	mutate := mustChangeTheDocument(t, "an off-origin image", func(doc []byte) []byte {
		return []byte(strings.Replace(string(doc), "</body>",
			`<img id="`+marker+`" src="https://example.invalid/pixel.png" alt=""></body>`, 1))
	})
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
	mark := b.logMark()
	p.loadConventions(t, ps.url, convOpts{theme: "light"})
	// The refusal is raised during the render; give the engine's log the same moment the
	// shipped grader gives it after driving an interaction.
	time.Sleep(600 * time.Millisecond)

	// The element really is in the document the engine parsed, so a silent instrument
	// cannot be excused as "the mutation never landed".
	var present bool
	p.mustEval(`!!document.getElementById(`+jsString(marker)+`)`, &present)
	if !present {
		t.Fatal("the off-origin image is not in the rendered document, so this case measured nothing")
	}
	if probs := gradeNoPolicyRefusal("an off-origin image", b.securityRefusals(mark)); probs == nil {
		t.Error("BLIND INSTRUMENT: the engine had to refuse an off-origin image under `default-src 'none'` and the grader reported NOTHING. It asserts that list is empty and would pass whatever the page did")
	} else {
		t.Logf("an off-origin image under default-src 'none' -> %s", probs[0])
	}
}

// F1 / F5. A refusal rewritten to read as success is the exact failure the grader exists
// for: the server changed nothing and the page says it did.
func TestRenderedCDP_TheRefusedControlGraderFailsAgainstAMessageThatReadsAsSuccess(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "a refusal rewritten as success",
		scriptMutation(`var m=document.getElementById("msg");`+
			`if (m && m.textContent.indexOf("refused") === 0) m.textContent = "done";`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(),
		controlStatus: 401, mutate: mutate})

	before, after, refusals := refuseAControlAction(t, b, p, ps.url)
	if probs := gradeRefusedControlActionCostsNothingElse(before, after, refusals); probs == nil {
		t.Errorf("the grader PASSED a page that answered a REFUSED control action with %q", after.Msg.Text)
	} else {
		t.Logf("a refusal rewritten as success -> %s", probs[0])
	}
}

// F7 / F11. The source offer taken off the screen on the very path where a reader most
// needs it: the page could not read the snapshot and has nothing else to show.
func TestRenderedCDP_TheServerErrorGraderFailsAgainstAHiddenSourceOffer(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "the source offer hidden",
		cssMutation(`.source-offer { display: none !important; }`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, eventsStatus: 500, mutate: mutate})
	mark := b.logMark()
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.views().length === 4 && __hf.views().every(function (v) { return v.state === "unreadable"; })`})

	probs := gradeServerErrorStillRendersTheOfferAndTheControls(p.collect(t), b.securityRefusals(mark))
	if probs == nil {
		t.Error("the grader PASSED a page that hides the AGPL source offer when the snapshot endpoint errors")
	} else {
		t.Logf("the source offer hidden on the server-error path -> %s", probs[0])
	}
}

// F10. Both preferences painting the SAME palette is dark-only wearing a light coat, and
// it is precisely the defect this item was filed to close.
func TestRenderedCDP_TheColourSchemeGraderFailsAgainstOnePaletteWearingBothPreferences(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	var rule strings.Builder
	rule.WriteString(":root {")
	for _, name := range s2Vocabulary {
		// One value for every role, so no token can differ between the two preferences.
		// !important rather than source order, so the rule wins wherever it lands.
		rule.WriteString(" " + name + ": #808080 !important;")
	}
	rule.WriteString(" }")
	mutate := mustChangeTheDocument(t, "one palette under both preferences", cssMutation(rule.String()))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})

	light, dark, none := readThemeTriple(t, p, ps.url)
	if probs := gradeThemeFollowsTheEnginesPreference(light, dark, none); probs == nil {
		t.Error("the grader PASSED a page that paints one palette under a light preference and a dark one; F10 is the defect this item closes")
	} else {
		t.Logf("one palette under both preferences -> %s", probs[0])
	}
}

// S6 / S7. A token moved away from the ratio recorded beside it. The record is what makes
// the floors auditable, so a record that no longer describes the value is worse than none.
func TestRenderedCDP_TheRecordedContrastGraderFailsAgainstATokenMovedAwayFromItsRecord(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "a token moved away from its record",
		cssMutation(`:root { --muted: #767676 !important; }`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})

	probs := gradeRecordedRatiosAgreeWithTheEngine("light", p.collect(t).Tokens, readContrastRecords(t))
	if probs == nil {
		t.Errorf("the grader PASSED a page whose %s value no longer matches the ratio recorded beside it in %s",
			"--muted", tokenFileName)
	} else {
		t.Logf("a token moved away from its record -> %s", probs[0])
	}
}

// F1. A control that looks exactly the same focused as unfocused is a keyboard user with
// no idea where they are.
func TestRenderedCDP_TheFocusRingGraderFailsAgainstAControlThatShowsNoFocus(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "the focus ring taken away",
		cssMutation(`:focus, :focus-visible { outline: none !important; }`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})

	expected, reached := tabThroughEveryControl(t, p)
	if probs := gradeTabOrderAndFocusRing("light", expected, reached); probs == nil {
		t.Error("the grader PASSED a page on which no control draws any focus indicator at all")
	} else {
		t.Logf("the focus ring taken away -> %s", probs[0])
	}
}

// F1. A control whose only name is its placeholder is named by hint text that disappears
// the moment the reader types into it.
func TestRenderedCDP_TheAccessibleNameGraderFailsAgainstAControlNamedOnlyByItsPlaceholder(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "a label detached from its control", func(doc []byte) []byte {
		return []byte(strings.Replace(string(doc),
			`<label for="filter" class="lbl">`, `<label class="lbl">`, 1))
	})
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light"})

	if probs := gradeAccessibleNames(p.axTree()); probs == nil {
		t.Error("the grader PASSED a page whose search box is named only by its own placeholder text")
	} else {
		t.Logf("a label detached from its control -> %s", probs[0])
	}
}

// F3. A field nobody measured rendered as a zero is the whole reason the absence phrase
// exists: an operator cannot tell a measured 0 from a measurement that never happened.
func TestRenderedCDP_TheAbsencePhraseGraderFailsAgainstAnUnmeasuredFieldRenderedAsZero(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "an unmeasured size rendered as zero",
		scriptMutation(`for (const td of document.querySelectorAll("#history td.size")) td.textContent = "0 B";`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: nullSnapshot(), mutate: mutate})
	p.loadConventions(t, ps.url, convOpts{theme: "light",
		waitFor: `__hf.connText() === "live" && __hf.rendered() && ` +
			`document.querySelectorAll("#history td.size").length > 0 && ` +
			`document.querySelector("#history td.size").textContent === "0 B"`})

	if probs := gradeEveryUnmeasuredFieldReadsTheAbsencePhrase(p.collect(t)); probs == nil {
		t.Error("the grader PASSED a page rendering a size nobody recorded as 0 B")
	} else {
		t.Logf("an unmeasured size rendered as zero -> %s", probs[0])
	}
}

// F6. The elapsed ticker is the one figure on this page that would go on advancing with
// nothing behind it, so the counterexample is exactly that: a ticker told the stream is
// still live after it died.
func TestRenderedCDP_TheSeveredStreamGraderFailsAgainstATickerThatKeepsRunning(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	mutate := mustChangeTheDocument(t, "a ticker that never notices the stream died",
		scriptMutation(`streamIsLive = true;`))
	ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(),
		streamFails: true, mutate: mutate})

	first, before, after := severedStreamReading(t, p, ps.url)
	if probs := gradeSeveredStreamKeepsItsRowsAndStopsEveryFigure(first, before, after); probs == nil {
		t.Errorf("the grader PASSED a severed page whose elapsed figures kept advancing (%v then %v)", before, after)
	} else {
		t.Logf("a ticker that never notices the stream died -> %s", probs[0])
	}
}

// F11. The markup a hostile media path carries turned into a real element and a real
// event-handler attribute, which is the failure the inert-text grader exists to refuse:
// the path arrives as things the browser will act on instead of as characters on screen.
//
// The counterexample is built with DOM calls rather than by re-parsing the text, and it
// is built deliberately so that NEITHER half reaches the network: `DOMParser` is itself a
// Trusted Types sink under this page's own policy, and an image with a src is refused by
// `default-src 'none'`, so either route would have made this case red for a POLICY reason
// - which is the CSP grader's property and has its own counterexample above. What this
// one has to defeat is inertness, so the element it adds fetches nothing.
func TestRenderedCDP_TheInertTextGraderFailsAgainstHostileTextTurnedIntoMarkup(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	// Appended to the page's OWN inline script, not as a second script element: this
	// grader COUNTS the scripts the document carries, and a mutation that added one would
	// defeat it for a reason the mutation itself introduced.
	mutate := mustChangeTheDocument(t, "a media path turned into an element and a handler", inlineScriptMutation(`
setInterval(function () {
  var td = document.querySelector("#queue td.path");
  if (!td || td.dataset.mutated === "1") return;
  var text = td.textContent;
  if (text.indexOf("<img") < 0) return;
  td.dataset.mutated = "1";
  td.appendChild(document.createElement("img"));
  var handler = /\son(\w+)=/.exec(text);
  if (handler) td.setAttribute("on" + handler[1], "void 0");
}, 20);`))

	base, got, s, refusals := readHostileTextPair(t, b, p, mutate)
	if got.Media == 0 && got.Handlers == 0 {
		t.Fatalf("the mutation introduced neither an element nor a handler attribute (media %d, handlers %d), so this case measured nothing",
			got.Media, got.Handlers)
	}
	if probs := gradeHostileTextIsInert(base, got, s, refusals); probs == nil {
		t.Errorf("the grader PASSED a page that turned a hostile media path into markup (%d media elements, %d handler attributes)",
			got.Media, got.Handlers)
	} else {
		t.Logf("a media path turned into an element and a handler -> %s", probs[0])
	}
}

// S9's second half. The reduce grader now compares the two renders with the page's one
// wall-clock column excluded, so it owes two counterexamples: one where reduced motion
// takes a value off the page OUTSIDE that column, and one INSIDE it - the second is what
// proves the exclusion is a named, guarded gap rather than a blind spot.
func TestRenderedCDP_TheReducedMotionValueGraderFailsAgainstAValueDroppedUnderReduce(t *testing.T) {
	b := launchCDP(t)
	p := b.newPage()

	for _, c := range []struct{ name, rule string }{
		{
			"a data column dropped only under reduced motion",
			`@media (prefers-reduced-motion: reduce) { td.size, td.vmaf { display: none !important; } }`,
		},
		{
			"the excluded live-clock column itself blanked under reduced motion",
			`@media (prefers-reduced-motion: reduce) { #queue td.elapsed { visibility: hidden !important; } }`,
		},
	} {
		mutate := mustChangeTheDocument(t, c.name, cssMutation(c.rule))
		ps := serveDocumentWith(t, serveOpts{url: upstreamForTest, snapshot: fixtureSnapshot(), mutate: mutate})
		moving, still := motionPair(t, p, ps.url)
		if probs := gradeReducedMotionCostsNoValue(moving, still); probs == nil {
			t.Errorf("the grader PASSED a page that drops a value under prefers-reduced-motion (%s)", c.name)
		} else {
			t.Logf("%-58s -> %s", c.name, probs[0])
		}
	}
}

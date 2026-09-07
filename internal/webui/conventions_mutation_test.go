package webui

import (
	"strings"
	"testing"
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

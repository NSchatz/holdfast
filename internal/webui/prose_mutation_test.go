package webui

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
)

// AC17: every grader this item adds must FAIL against a mutation that defeats the
// property it asserts.
//
// This is the assertion that stops the rest of the suite being decoration. A word budget
// that cannot notice a restored paragraph, a redundancy check that cannot notice a
// repeated phrase, a preservation checklist that cannot notice a deleted figure - each
// of those passes for ever while measuring nothing, and each is exactly the failure that
// killed S0035 over five refute ordinals. So every mutation below is served to the SAME
// browser, through the same classifier, and the grader is required to report.
//
// The three the specification names by name are here first: a removed paragraph
// restored, the function-word list emptied, and one operational fact deleted.

// injectInto rewrites the served document, and refuses a rewrite that changed nothing -
// a mutation that did not apply would make the assertion after it vacuous.
func injectInto(t *testing.T, old, new string) func([]byte) []byte {
	t.Helper()
	return func(b []byte) []byte {
		s := string(b)
		out := strings.Replace(s, old, new, 1)
		if out == s {
			t.Errorf("the mutation %q -> %q did not change the served document", short(old), short(new))
		}
		return []byte(out)
	}
}

func short(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// injectCSS appends a rule to the document's own stylesheet.
func injectCSS(t *testing.T, rule string) func([]byte) []byte {
	return injectInto(t, "</style>", rule+"\n</style>")
}

// The anchors the mutations below are written against, quoted from the page shell so a
// change to the markup reds here rather than silently making a mutation a no-op (injectInto
// already refuses a rewrite that changed nothing, which is what turns that into a failure).
const (
	queueHeadingMarkup   = `<h3 id="h-queue">Queue and active</h3>`
	historyHeadingMarkup = `<h3 id="h-hist">Recent history</h3>`
	pausedBadgeMarkup    = `<span id="b-paused" class="badge">running</span>`
	scanBadgeMarkup      = `<span id="b-scan" class="badge scan">idle</span>`
	lifetimeFigureMarkup = `<b id="reclaimed-lifetime">loading</b>`
	chipsMarkup          = `<div class="chips" id="chips"></div>`
	rescanButtonMarkup   = `<button id="rescan" class="primary">Rescan</button>`
)

// renderedDocLink is one region's documentation link exactly as the handler resolves it
// into the served document, so a mutation is written against the form the browser is given
// rather than against the marker the committed page carries.
func renderedDocLink(i int) string {
	return docLinkHTML(sourceoffer.Upstream, docLinks[i].Fragment, docLinks[i].Text)
}

// restoredParagraph is one of the paragraphs this item took OFF the page, put back on it
// exactly as a later change might put it back.
func restoredParagraph(t *testing.T) string {
	t.Helper()
	rec := parseRemovalRecord(t)
	if len(rec.order) < 3 {
		t.Fatal("the removal record moves fewer than three sentences; the restoration mutation would be trivial")
	}
	return `<p class="note">` + strings.Join(rec.order[2:5], " ") + `</p>`
}

// --- the three mutations the specification names ---------------------------------

func TestRendered_ARestoredParagraphBreaksTheBudgetAndTheBlockCeiling(t *testing.T) {
	b := proseBrowser(t)
	para := restoredParagraph(t)
	v := renderProse(t, b, proseOpts{
		mutate: injectInto(t, queueHeadingMarkup, queueHeadingMarkup+para),
	})
	if probs := gradeTotalBudget(v.prose); probs == nil {
		t.Errorf("the page-copy budget accepted a document with a removed paragraph put back on it (%d words counted)", v.prose.total())
	}
	if probs := gradeBlockCeiling(v.prose); probs == nil {
		t.Error("the per-block ceiling accepted a paragraph of restored methodology as one block")
	}
}

func TestRendered_TheRedundancyChecksFailWhenTheFunctionWordListIsEmptied(t *testing.T) {
	b := proseBrowser(t)
	fn := loadFunctionWords(t)
	empty := map[string]bool{}

	// The reading the checks are run against is a real one: an empty ledger, where every
	// row view shows its own empty state. Onto it goes a block whose whole content is a
	// run of FUNCTION words the page already says elsewhere ("and since when", from the
	// queue's own scope label). Under the committed list that run is invisible - every
	// word of it is subtracted - and under an empty one it is a repeated run of three,
	// which is the difference the list makes and the thing this proof is about.
	p := renderProse(t, b, proseOpts{
		snapshot: emptySnapshot(),
		mutate:   injectInto(t, historyHeadingMarkup, `<p class="scope">And since when.</p>`+historyHeadingMarkup),
	}).prose

	if probs := gradeHeadingOverlap(p, fn); probs != nil {
		t.Fatalf("the shipped page already repeats a heading's content word, so the mutation proves nothing: %v", probs)
	}
	if probs := gradeRepeatedRuns(p, fn); probs != nil {
		t.Fatalf("the shipped page already repeats a run of content words, so the mutation proves nothing: %v", probs)
	}
	if probs := gradeHeadingOverlap(p, empty); probs == nil {
		t.Error("the heading-overlap check passes with an EMPTY function-word list, so the committed list is not what it reads")
	}
	if probs := gradeRepeatedRuns(p, empty); probs == nil {
		t.Errorf("the repeated-run check passes with an EMPTY function-word list, so the committed list is not what it reads\n%s", p)
	}
}

func TestRendered_ThePreservationChecklistFailsWhenAnOperationalFactIsDeleted(t *testing.T) {
	b := proseBrowser(t)

	// The unmutated fixture first, so a report below is a signal and not the state of
	// the page.
	clean := renderProse(t, b, proseOpts{snapshot: preservationSnapshot()})
	for _, f := range operationalFacts() {
		if probs := f.probe(clean); probs != nil {
			t.Fatalf("the shipped page already fails %q, so the mutations below prove nothing: %v", f.what, probs)
		}
	}

	for _, c := range []struct {
		name   string
		fact   string
		mutate func([]byte) []byte
	}{
		// The badges and the count chips are written by render() through an element it
		// looks up unconditionally, so DELETING one of them from the markup breaks the
		// script and the page renders nothing at all - a different failure wearing this
		// one's clothes. What each of these takes away is the VALUE, leaving the element
		// where it was, which is the failure the fact is actually about.
		{"the running/paused badge is left blank", "whether this holdfast is running or paused",
			injectInto(t, `bp.textContent = snap.paused ? "paused" : "running";`, `bp.textContent = "";`)},
		{"the scan badge is left blank", "whether a scan is under way",
			injectInto(t, `bs.textContent = snap.scanning ? "scanning" : "idle";`, `bs.textContent = "";`)},
		{"the lifetime reclaimed figure is deleted", "reclaimed this run, and reclaimed lifetime",
			injectInto(t, lifetimeFigureMarkup, "")},
		{"the label beside a reclaimed figure is deleted", "reclaimed this run, and reclaimed lifetime",
			injectInto(t, "reclaimed lifetime: ", "")},
		{"the per-status counts are never appended", "the per-status count of files, one figure per status the page declares",
			injectInto(t, "chips.appendChild(chip);", "void chip;")},
		{"the history cap notice is hidden", "the notice that a table is showing a capped subset, and the cap",
			injectCSS(t, "#hist-cap { display:none; }")},
		// Hidden rather than deleted: the page reads its own controls at load, so a
		// DELETED token field breaks the script and nothing renders at all, which would
		// be a different failure wearing this one's clothes. Hiding it is the failure
		// this fact is actually about - the control is in the document and not on the
		// screen.
		{"the control token input is hidden", "the control token input and the rescan, pause and resume controls; the filter input",
			injectCSS(t, "#token { display:none; }")},
		{"the filter input is hidden", "the control token input and the rescan, pause and resume controls; the filter input",
			injectCSS(t, "#filter { display:none; }")},
		{"the rescan control loses its label", "the control token input and the rescan, pause and resume controls; the filter input",
			injectInto(t, rescanButtonMarkup, `<button id="rescan" class="primary"></button>`)},
		{"the absence phrase is hidden", "the absence phrase, wherever a rendered field has no value",
			injectCSS(t, ".nr { display:none; }")},
		{"the source offer is hidden", "the AGPL section 13 source offer, character for character",
			injectCSS(t, ".source-offer { display:none; }")},
		{"an aggregate's exclusion statement is hidden", "for every aggregate: its name, its value, the set of rows, and the count excluded",
			injectCSS(t, ".agg .agg-ex { display:none; }")},
		{"the worker column is hidden", "for every queue row: path, status, elapsed, progress, worker",
			injectCSS(t, "#queue td.worker { display:none; }")},
		{"a history row loses its result", "for every history row: path, result, size, VMAF, encoder, encode duration, last updated",
			injectCSS(t, "table.hist td.st { display:none; }")},
	} {
		v := renderProse(t, b, proseOpts{snapshot: preservationSnapshot(), mutate: c.mutate})
		var probe func(proseVerdict) []string
		for _, f := range operationalFacts() {
			if f.what == c.fact {
				probe = f.probe
			}
		}
		if probe == nil {
			t.Fatalf("no operational-fact probe is named %q", c.fact)
		}
		if probs := probe(v); probs == nil {
			t.Errorf("the preservation checklist accepted a page where %s (the fact %q was not reported missing)", c.name, c.fact)
		}
	}
}

// --- the other graders this item adds, each against its own defeat ----------------

func TestRendered_TheTwoWidthCheckFailsWhenABlockIsHiddenAtOneWidth(t *testing.T) {
	b := proseBrowser(t)
	// A block that renders at 1280px and is removed from the render at 360px, which is
	// how a page meets a word budget at one width by hiding words at that width.
	mutate := injectCSS(t, "@media (max-width: 400px) { #region-now .scope { display:none; } }")
	narrow := renderProse(t, b, proseOpts{width: 360, height: 800, mutate: mutate}).prose
	wide := renderProse(t, b, proseOpts{width: 1280, height: 1024, mutate: mutate}).prose
	if probs := gradeSameBlocksAtBothWidths(narrow, wide); probs == nil {
		t.Errorf("the two-width check accepted a document that drops a block of page copy at 360px\n360px: %s\n1280px: %s", narrow, wide)
	}
}

func TestRendered_TheAccessibilityCheckFailsWhenRemovedCopyIsHiddenInAName(t *testing.T) {
	b := proseBrowser(t)
	removed := removedSentences(t)
	if len(removed) == 0 {
		t.Fatal("the removal record names no removed sentence")
	}
	// The exact move the criterion exists to refuse: a paragraph taken off the visible
	// page and put back where only a screen reader meets it. The LONGEST removed sentence
	// is chosen so this one mutation exercises both limbs of the criterion at once - the
	// eight-word ceiling and the removed-copy test.
	hidden := removed[0]
	for _, s := range removed {
		if len(strings.Fields(s)) > len(strings.Fields(hidden)) {
			hidden = s
		}
	}
	if len(strings.Fields(hidden)) <= axNameCeiling {
		t.Fatalf("the longest removed sentence is %d words, which is inside the %d-word ceiling; this mutation cannot exercise both limbs",
			len(strings.Fields(hidden)), axNameCeiling)
	}
	v := renderProse(t, b, proseOpts{
		mutate: injectInto(t, `<main>`, `<main aria-label="`+hidden+`">`),
	})
	probs := gradeAccessibleNameLength(v.ax, removed)
	if probs == nil {
		t.Fatal("the accessibility-tree check accepted a name carrying a sentence the removal record says was taken off the page")
	}
	joined := strings.Join(probs, "\n")
	if !strings.Contains(joined, "carries a sentence the removal record") {
		t.Errorf("the check reported something, but not that the name carries removed copy:\n%s", joined)
	}
	if !strings.Contains(joined, "want at most") {
		t.Errorf("the check did not report the eight-word ceiling being broken:\n%s", joined)
	}
	// And a name that is merely too long, carrying no removed sentence, is caught too.
	v2 := renderProse(t, b, proseOpts{
		mutate: injectInto(t, `<main>`, `<main aria-label="one two three four five six seven eight nine">`),
	})
	if probs := gradeAccessibleNameLength(v2.ax, removed); probs == nil {
		t.Error("the accessibility-tree check accepted a nine-word accessible name")
	}
}

func TestRendered_TheDocumentationLinkCheckFailsWhenASectionLosesOrDuplicatesIt(t *testing.T) {
	b := proseBrowser(t)
	// The served document carries the RESOLVED href, so the mutation is written against
	// the form the browser is given.
	resolved := renderedDocLink(0)

	for name, mutate := range map[string]func([]byte) []byte{
		"a section loses its documentation link":      injectInto(t, resolved, ""),
		"a section renders two documentation links":   injectInto(t, resolved, resolved+resolved),
		"a section's documentation link is not shown": injectCSS(t, ".doclink { display:none; }"),
	} {
		v := renderProse(t, b, proseOpts{mutate: mutate})
		if probs := docLinkProblems(v.prose); probs == nil {
			t.Errorf("the documentation-link check accepted a page where %s", name)
		}
	}
}

// The classifier itself must not be able to pass by reclassifying prose. A paragraph put
// inside an EXCLUDED container is still prose, and this is the check that the exclusion
// table subtracts named things rather than whole regions of the page.
func TestRendered_TheClassifierStillCountsProseThatSitsBesideAnExcludedValue(t *testing.T) {
	b := proseBrowser(t)
	para := restoredParagraph(t)
	for name, mutate := range map[string]func([]byte) []byte{
		"beside the badges":         injectInto(t, `<div class="badges">`, `<div class="badges">`+para),
		"beside the counts":         injectInto(t, chipsMarkup, chipsMarkup+para),
		"inside the controls":       injectInto(t, `<div class="controls">`, `<div class="controls">`+para),
		"between the two regions":   injectInto(t, `<section id="region-history"`, para+`<section id="region-history"`),
		"at the foot of the page":   injectInto(t, `<footer>`, `<footer>`+para),
		"above the history heading": injectInto(t, historyHeadingMarkup, para+historyHeadingMarkup),
	} {
		v := renderProse(t, b, proseOpts{mutate: mutate})
		if probs := gradeTotalBudget(v.prose); probs == nil {
			t.Errorf("the classifier did not count a restored paragraph placed %s (%d words counted)\n%s",
				name, v.prose.total(), v.prose)
		}
	}
}

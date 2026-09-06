package webui

// regress_0035_F20: AC9's and AC10's graders prove that the page BUILDS its
// degraded-state rows. Nothing proves that any of them is ever SHOWN.
//
// AC9: "THE SYSTEM SHALL render a VISIBLE MESSAGE in that table's body ..."
// AC10: "THE SYSTEM SHALL RENDER the wording it renders today, verbatim:
// `Nothing queued.`, `No history yet.`, `unavailable`, `not recorded`,
// `unknown`, `this view is capped` ..."
//
// Impl-gate finding F1 was "the row is built and never appended", and it was
// closed by pinning setNoMatchRow's construction path in order. Impl-gate
// finding F17 was "emptyRow builds its cell with an EMPTY string", and it was
// closed by pinning emptyRow's construction path in order. Both fixes stopped at
// the function the finding named. The property both criteria actually assert -
// that an operator SEES these words - is still nowhere asserted, and it breaks
// four different ways with every committed test in this package green:
//
//	a. mk() is the leaf that writes the text for BOTH of those pinned paths, and
//	   for nrNode(). Change its one assignment to `e.textContent = ""` and
//	   `Nothing queued.`, `No history yet.`, `unavailable`, `not recorded`,
//	   `unknown` and AC9's whole no-match sentence all render as empty cells.
//	   Every pin above still reads exactly as written - `mk("td", "empty", text)`,
//	   `mk("span", "nr", "unavailable")` - because each pins the CALL and none
//	   pins what mk does with the argument. That is finding F17's own diagnosis
//	   ("the pins prove the CALL SITE, not the render") one call deeper, at the
//	   single function every one of those call sites delegates the render to.
//
//	b. `tr.hidden = true` beside setNoMatchRow's appendChild. The row is
//	   constructed in the pinned order, given its colSpan and inserted - and is
//	   not visible. AC9's word is "visible".
//
//	c. `tr.hidden = true` in emptyRow. Same shape for AC10's two empty-table
//	   states.
//
//	d. `el.hidden = true` in capNote's total > shown branch. The cap notice is
//	   written into the element and then hidden, so `this view is capped` - the
//	   string that stops a truncated view reading as a complete one, and half of
//	   what AC9's message exists to say - never reaches an operator.
//
// The shipped page is CORRECT: mk assigns the text, no row is hidden, and the
// cap notice is shown. This file grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion: "a test that passes
// on a mutated page has not graded its criterion."
//
// Fix upstream in the graders, not the page: pin what mk() does with its text
// argument the way emptyRow's and setNoMatchRow's bodies are pinned, and assert
// that no path taken to show a degraded state sets `hidden` on the node carrying
// it. This file goes green when a page that renders none of AC10's nine strings
// can no longer pass unread.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	// One env var for every ordinal-5 refuter artifact (F20-F25); the case name
	// selects the mutation.
	v5Env = "S0035_V5_MUT"

	// Every committed test in this package EXCEPT the regress parents, which
	// spawn children of their own. RE2 has no negative lookahead, so "does not
	// start with TestReg" is spelled with character classes - the same regex the
	// loop-4 artifacts use.
	v5Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`
)

type v5Mutation struct {
	criterion string
	old, new  string
	why       string
}

// v5Holes are the ordinal-5 edits that violate their criterion and that the
// committed grader set does NOT red on.
var v5Holes = map[string]v5Mutation{
	"F20a-mk-writes-no-text": {
		criterion: "AC9, AC10",
		old:       `  if (text != null) e.textContent = text;`,
		new:       `  if (text != null) e.textContent = "";`,
		why: "mk() stops writing the text it is handed, so every degraded state built through it - " +
			"`Nothing queued.`, `No history yet.`, `unavailable`, `not recorded`, `unknown` and AC9's " +
			"whole no-match sentence - renders as an EMPTY cell, while every pinned call site reads exactly as pinned",
	},
	"F20b-no-match-row-inserted-then-hidden": {
		criterion: "AC9",
		old:       "  body.appendChild(tr);",
		new:       "  tr.hidden = true;\n  body.appendChild(tr);",
		why: "the no-match row is constructed in the pinned order and inserted, and is then not VISIBLE - " +
			"so a term matching no loaded row still empties the table in silence",
	},
	"F20c-empty-row-inserted-then-hidden": {
		criterion: "AC10",
		old:       `  tr.dataset.empty = "1";`,
		new:       "  tr.dataset.empty = \"1\";\n  tr.hidden = true;",
		why: "the empty-queue and empty-history rows are built, filled and appended, and are then not " +
			"visible, so an operator with an empty queue is shown a table that says nothing",
	},
	"F20d-cap-notice-written-then-hidden": {
		criterion: "AC10",
		old:       "    el.hidden = false;",
		new:       "    el.hidden = true;",
		why: "the row-cap notice is written into its element and then hidden, so `this view is capped` " +
			"never reaches an operator and a truncated view reads as a complete one",
	},

	// --- the remaining ordinal-5 cases, each with its own file ---------------

	"F21a-aggs-collapse-undone-by-a-selector-group": {
		criterion: "AC6",
		old:       "</style>",
		new:       "  .aggs, .chips { grid-template-columns:repeat(2, 1fr); }\n</style>",
		why: "a later top-level rule of equal specificity, written as a selector GROUP, lays the aggregate " +
			"cards out in TWO columns at the 360px viewport - and the region loop's membership test is " +
			"`r.sel != region.sel`, exact string equality, so the rule that decides is never seen",
	},
	"F21b-header-collapse-undone-by-a-selector-group": {
		criterion: "AC6",
		old:       "</style>",
		new:       "  header, footer { flex-direction:row; }\n</style>",
		why:       "a later top-level rule, written as a selector GROUP, keeps the header a ROW at the 360px viewport",
	},
	"F21c-controls-collapse-undone-by-a-selector-group": {
		criterion: "AC6",
		old:       "</style>",
		new:       "  .controls, .chips { flex-direction:row; }\n</style>",
		why:       "a later top-level rule, written as a selector GROUP, keeps both control rows a ROW at the 360px viewport",
	},
	"F22-reduce-cut-undone-by-a-later-reduce-block": {
		criterion: "AC5",
		old:       "</style>",
		new:       "  @media (prefers-reduced-motion: reduce) {\n    * { transition-duration:400ms !important; }\n  }\n</style>",
		why: "a second reduce block of identical origin, importance and specificity, later in source order, " +
			"restores 400ms transitions - so a user who asked for reduced motion gets the page's motion back " +
			"in full, while `cut[fam]` is still set true by the earlier block it overrides",
	},
	"F23-ring-pulled-inside-by-an-offset-only-rule": {
		criterion: "AC8",
		old:       "</style>",
		new:       "  :focus-visible { outline-offset:-8px; }\n</style>",
		why: "a later :focus-visible rule declaring ONLY outline-offset wins the cascade and draws the ring " +
			"INSIDE the control's border box - impl-gate finding F19's own violation, spelled without an " +
			"outline property, which `if !o.stated { continue }` discards before the offset is ever looked at",
	},
	"F24-color-scheme-supports-neither-scheme": {
		criterion: "AC4",
		old:       "color-scheme: light dark;",
		new:       "color-scheme: lightdark;",
		why: "color-scheme names a single unrecognised ident, so the document opts into NEITHER scheme and " +
			"native controls, scrollbars and form fields stay light inside the dark page - the exact harm " +
			"the criterion names - while `strings.Contains(cs, \"light\") && strings.Contains(cs, \"dark\")` " +
			"is satisfied by the one word containing both",
	},
	"F25-target-shrunk-by-a-transform": {
		criterion: "AC7",
		old:       "  #msg { font-size:var(--fs-md);",
		new:       "  #rescan { transform:scale(0.4); }\n  #msg { font-size:var(--fs-md);",
		why: "the Rescan button is RENDERED at 12.8 x 12.8 CSS px, half of WCAG 2.2 2.5.8's floor, by a " +
			"property outside the sweep's four-name membership (min-height / min-width / min-block-size / min-inline-size)",
	},
	"F25b-target-shrunk-by-zoom": {
		criterion: "AC7",
		old:       "  #msg { font-size:var(--fs-md);",
		new:       "  #rescan { zoom:0.4; }\n  #msg { font-size:var(--fs-md);",
		why:       "the same target, shrunk under the floor through `zoom` instead",
	},
}

func init() {
	name := os.Getenv(v5Env)
	if name == "" {
		return
	}
	m, ok := v5Holes[name]
	if !ok {
		panic("regress_0035 ordinal-5 artifacts: no case named " + name)
	}
	s := string(indexHTML)
	if !strings.Contains(s, m.old) {
		panic("regress_0035 ordinal-5 artifacts: mutation anchor absent from index.html: " + m.old)
	}
	indexHTML = []byte(strings.Replace(s, m.old, m.new, 1))
}

// v5IsChild reports whether this process already carries a mutated page, put
// there by this loop's harness or by any earlier one.
func v5IsChild() bool {
	for _, v := range []string{
		v5Env, "S0035_MUT", "S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
		"S0035_F14_MUT", "S0035_F15_MUT", "S0035_F16_MUT", "S0035_F17_MUT", "S0035_F18_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

// v5Assert runs the WHOLE committed grader set against the mutated page and
// fails when it stays green. Running the whole set, rather than one named test,
// is the question a refuter is actually asking: does ANYTHING committed red on a
// page that violates the criterion?
func v5Assert(t *testing.T, name string) {
	t.Helper()
	if v5IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	m, ok := v5Holes[name]
	if !ok {
		t.Fatalf("no case named %s", name)
	}
	cmd := exec.Command(os.Args[0], "-test.run", v5Run, "-test.v")
	cmd.Env = append(os.Environ(), v5Env+"="+name)
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   Test") {
		t.Fatalf("the child ran no committed test at all, so nothing was graded\n%s", out)
	}
	if err == nil {
		t.Errorf("%s is ungraded here: every committed test in this package stayed GREEN on a page where %s\nmutation: %q -> %q\n%s",
			m.criterion, m.why, m.old, m.new, out)
	}
}

func TestRegress0035_F20_AC10MkRendersNoTextAndNothingSaysSo(t *testing.T) {
	v5Assert(t, "F20a-mk-writes-no-text")
}

func TestRegress0035_F20_AC9NoMatchRowIsInsertedAndHidden(t *testing.T) {
	v5Assert(t, "F20b-no-match-row-inserted-then-hidden")
}

func TestRegress0035_F20_AC10EmptyRowIsInsertedAndHidden(t *testing.T) {
	v5Assert(t, "F20c-empty-row-inserted-then-hidden")
}

func TestRegress0035_F20_AC10CapNoticeIsWrittenAndHidden(t *testing.T) {
	v5Assert(t, "F20d-cap-notice-written-then-hidden")
}

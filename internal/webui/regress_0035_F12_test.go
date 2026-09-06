package webui

// regress_0035_F12: AC7's target sweep selects rules by the WORDS "button" and
// "input" appearing in the selector, so a rule that addresses a control by its id
// is never handed to it - and every pointer target on this page has an id.
//
// AC7: "WHEN a pointer target is rendered at the page's base font size, THE SYSTEM
// SHALL give it at least 24 CSS px in both dimensions (WCAG 2.2 2.5.8). The pointer
// targets are every `button` and every `input` in the two control rows."
//
// TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor holds the floor with
// a sweep whose gate is textual:
//
//	if !strings.Contains(c.sel, "button") && !strings.Contains(c.sel, "input") { continue }
//
// Loop 2's F8 widened that sweep from the two top-level base rules to every rule in
// every at-rule block, which closed the `@media` shape. It did not change the
// selector gate. The page's five pointer targets are `#token`, `#rescan`, `#pause`,
// `#resume` and `#filter`, the stylesheet already addresses two neighbouring
// elements by id (`#msg`, `#conn`), and an id selector outranks `button`
// (1,0,0 against 0,0,1) - so `#rescan { min-height:20px; min-width:20px; }` takes
// the Rescan button, the one control that starts a scan, to 20px in BOTH
// dimensions, four under WCAG 2.2 2.5.8's floor, and every committed test in this
// package passes.
//
// Unlike F8, nothing here needs a rendered box: the declaration states the
// violation in the source, as a px length the sweep can already measure. It is
// simply never offered one. The `swept < 4` anti-vacuity guard does not help (the
// four base declarations still sweep), and the markup half of the test only asks
// that every <button>/<input> sit inside a .controls row, which they still do.
//
// The shipped page is CORRECT: `--target-min:32px` is declared as both min-height
// and min-width on `button` and on `.controls input`, and no rule lowers it. This
// test grades the PROOF, which the spec's acceptance-criteria preamble makes part
// of the criterion: "a test that passes on a mutated page has not graded its
// criterion."
//
// Fix upstream in the sweep: choose the rules that can reach a pointer target from
// the page's own markup - the ids and classes those five elements actually carry -
// rather than from two words in the selector text. This file goes green when a rule
// that lowers a control's floor can no longer pass unread.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f12Env = "S0035_F12_MUT"

	f12Old = "  #msg { font-size:var(--fs-md);"
	f12New = "  #rescan { min-height:20px; min-width:20px; }\n  #msg { font-size:var(--fs-md);"

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f12Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f12Grader = "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor"
)

func init() {
	if os.Getenv(f12Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f12Old) {
		panic("regress_0035_F12: mutation anchor absent from index.html: " + f12Old)
	}
	indexHTML = []byte(strings.Replace(s, f12Old, f12New, 1))
}

func TestRegress0035_F12_AC7ARuleAddressingAControlByIDIsUnread(t *testing.T) {
	if os.Getenv(f12Env) != "" || os.Getenv("S0035_MUT") != "" ||
		os.Getenv("S0035_F7_MUT") != "" || os.Getenv("S0035_F11_MUT") != "" {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f12Run, "-test.v")
	cmd.Env = append(os.Environ(), f12Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f12Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f12Grader, out)
	}
	if err == nil {
		t.Errorf("AC7 is ungraded for a rule that addresses a control by its id: every "+
			"committed test in this package stayed GREEN on a page carrying "+
			"`#rescan { min-height:20px; min-width:20px; }`, which outranks "+
			"`button { min-height:var(--target-min); min-width:var(--target-min); }` and "+
			"holds a pointer target at 20px in BOTH dimensions, under WCAG 2.2 2.5.8's "+
			"24px floor. The sweep gates on the words \"button\" and \"input\" appearing in "+
			"the selector text, and all five of the page's pointer targets are addressable "+
			"by id.\nmutation: %q -> %q\n%s", f12Old, f12New, out)
	}
}

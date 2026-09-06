package webui

// regress_0035_F18: AC5's motion sweep is a membership test narrower than the
// criterion's own set. Motion written through the vendor prefix is not seen, and
// the reduce block does not cut it either.
//
// AC5: "IF the page declares ANY `transition`, `animation` or `@keyframes`, THEN
// under `@media (prefers-reduced-motion: reduce)` THE SYSTEM SHALL reduce them to
// no perceptible motion ..."
//
// TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt decides membership with
// two textual tests:
//
//	if strings.Contains(cssComment.ReplaceAllString(styleBlock(t), " "), "@keyframes")
//	if strings.HasPrefix(d.prop, "animation")
//
// `@-webkit-keyframes` does not contain `@keyframes` and `-webkit-animation` does
// not start with `animation`, so a page that declares its motion entirely through
// the WebKit prefix declares, to this grader, no motion at all. `moved` then stays
// 0 and the sweep RETURNS on the criterion's own no-motion branch, which is the
// class this loop was opened on in its purest form: a sweep whose membership test
// is narrower than the criterion's definition of the set, and which therefore takes
// the "nothing to check" exit.
//
// The reduce block does not save it. That block cuts `transition-duration` and
// `transition-delay`; it says nothing about `animation-duration` or
// `-webkit-animation-duration`, so on the mutated page a user who has asked for
// reduced motion watches the encoding dot pulse forever. WebKit and Blink both
// still honour the prefixed property and the prefixed at-rule, so this is motion an
// operator actually sees, not a hypothetical.
//
// The shipped page is CORRECT: it declares transitions only, no animation of either
// spelling. This file grades the PROOF, which the spec's acceptance-criteria
// preamble makes part of the criterion: "a test that passes on a mutated page has
// not graded its criterion."
//
// Fix upstream in the sweep: match the at-rule and the property family the way the
// criterion names them - any `keyframes` at-rule whatever its prefix, and any
// property whose name ends in the `animation` family - and, if an animation is ever
// permitted, require the reduce block to cut its duration as well as the
// transitions'. This file goes green when motion spelled with a vendor prefix can
// no longer pass as no motion at all.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f18Env = "S0035_F18_MUT"

	f18Old = "  .tablewrap { overflow-x:auto; }"
	f18New = "  @-webkit-keyframes pulse { from { opacity:0; } to { opacity:1; } }\n" +
		"  .st-encoding .dot { -webkit-animation:pulse 2s infinite; }\n" +
		"  .tablewrap { overflow-x:auto; }"

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f18Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f18Grader = "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt"
)

func init() {
	if os.Getenv(f18Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f18Old) {
		panic("regress_0035_F18: mutation anchor absent from index.html: " + f18Old)
	}
	indexHTML = []byte(strings.Replace(s, f18Old, f18New, 1))
}

func f18IsChild() bool {
	for _, v := range []string{
		"S0035_F18_MUT", "S0035_F17_MUT", "S0035_F16_MUT", "S0035_F15_MUT",
		"S0035_F14_MUT", "S0035_MUT", "S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func TestRegress0035_F18_AC5MotionBehindTheVendorPrefixIsUnread(t *testing.T) {
	if f18IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f18Run, "-test.v")
	cmd.Env = append(os.Environ(), f18Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f18Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f18Grader, out)
	}
	if err == nil {
		t.Errorf("AC5 is ungraded against motion written with the vendor prefix: every "+
			"committed test in this package stayed GREEN on a page carrying "+
			"`@-webkit-keyframes pulse` and `.st-encoding .dot { -webkit-animation:pulse "+
			"2s infinite }`. `@-webkit-keyframes` does not contain \"@keyframes\" and "+
			"`-webkit-animation` does not start with \"animation\", so the sweep sees no "+
			"motion at all and takes the criterion's own no-motion exit - while the reduce "+
			"block, which cuts transition-duration only, leaves the dot pulsing for a user "+
			"who explicitly asked for reduced motion.\nmutation: %q -> %q\n%s",
			f18Old, f18New, out)
	}
}

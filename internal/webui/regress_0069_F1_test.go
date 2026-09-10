//go:build refuter_s0069_f1

package webui

import (
	"fmt"
	"testing"
	"time"
)

// REFUTER ARTIFACT for S0069, finding F1. It is behind a build tag so that it changes
// nothing about `make check`, `make webui-check` or `make webui-repeat-check`: it
// documents a defect and is expected to FAIL. Run it with
//
//	go test -tags refuter_s0069_f1 -count=1 -run TestRegress0069F1_ ./internal/webui/
//
// The claim under test. B9's queue-row age check used to tie each rendered age to that
// row's own WIRE timestamp - the fixture stamps three rows 3600s, 90s and 45s before the
// snapshot's clock, and the grader required the rendered ages to be those. That tie is
// gone: the replacement asks only that the three rendered ages be consistent with ONE
// page clock, each measured against the basis THE PAGE ITSELF publishes in
// `td.elapsed`'s data-since, with the clock anchored into [snapNow, snapNow+took].
//
// A page that derives every age from a basis it published incorrectly therefore satisfies
// the new check exactly, while showing a reader an age the server's own data does not
// support. This case builds that page: it shifts each row's published basis forward by a
// constant and lets the page's own refreshElapsed ticker do the derivation, so nothing
// here writes an age - the page renders each one itself, from the basis it holds.
//
// The mutation is visible to a reader and invisible to the grader: a job the snapshot
// says transitioned 3600 seconds ago is shown as 59m 30s, one stamped 90s ago as 1m 0s,
// and one stamped 45s ago as 15s. The retired windows failed all three.
const regress0069Skew = 30 // seconds, < 45 so every rendered age stays positive and ordered

func TestRegress0069F1_TheAgeCheckPassesAgesTheWireTimestampsDoNotSupport(t *testing.T) {
	bin := chromium(t)

	// Shift only the BASIS. The page's own ticker recomputes the visible age from it, so
	// the rendered figure is the page's derivation and not a string this test wrote.
	// data-orig makes the shift idempotent under scriptMutation's 20ms interval.
	mutate := scriptMutation(fmt.Sprintf(
		`var c=document.querySelectorAll("#queue td.elapsed");`+
			`for(var i=0;i<c.length;i++){var td=c[i];`+
			`if(td.dataset.orig===undefined){td.dataset.orig=td.dataset.since||"0";`+
			`td.dataset.since=String(Number(td.dataset.orig)+%d);}}`, regress0069Skew))

	plain := servedDocument(t)
	if string(mutate([]byte(plain))) == plain {
		t.Fatalf("the mutation did not change the served document - the assertion below would be vacuous")
	}

	// The reading is held back two seconds so the page's one-second elapsed ticker has
	// certainly recomputed every cell from the shifted basis.
	start := time.Now()
	v, log := mustRender(t, bin, dashOpts{snapshot: fixtureSnapshot(), mutate: mutate, delay: 2 * time.Second})
	took := time.Since(start)

	ages := rowAgeReadings(t, v.Queue)
	if len(ages) != 3 {
		t.Fatalf("the mutated page rendered %d queue rows, want 3\nbrowser output:\n%s", len(ages), log)
	}

	// What a reader is actually shown, against what the snapshot said. wireAges mirrors
	// fixtureSnapshot()'s own stamps: snapNow-3600, snapNow-90, snapNow-45.
	wireAges := []int{3600, 90, 45}
	for i, a := range ages {
		t.Logf("row %d: the snapshot stamps it %ds before its clock; the page shows %q (%ds), derived from a published basis of %d (the wire says %d)",
			i, wireAges[i], a.rendered, a.seconds, a.since, snapNow-int64(wireAges[i]))
	}

	// The grader's own decision, reproduced exactly as B9 now takes it.
	lo, hi, oneClock := oneClockBehind(ages)
	anchored := oneClock && lo >= snapNow && hi <= float64(snapNow)+took.Seconds()+1
	ordered := ages[0].seconds > ages[1].seconds && ages[1].seconds > ages[2].seconds

	// And the property the retired windows decided: a rendered age is the snapshot's own
	// clock less that row's WIRE timestamp, so it can never be shorter than the stamp.
	shortBy := 0
	for i, a := range ages {
		if d := wireAges[i] - a.seconds; d > shortBy {
			shortBy = d
		}
	}

	if anchored && ordered && shortBy > 0 {
		t.Errorf("the queue-row age check PASSED a page whose every rendered age is up to %ds short of what the snapshot's own "+
			"timestamps support (one-clock=[%.0f,%.0f), anchored=%v, ordered=%v): the check now measures each age against a basis "+
			"the PAGE publishes, so a page that publishes the wrong basis grades its own arithmetic and no rendered figure is tied "+
			"to the server's data any more\nages: %v\nbrowser output:\n%s",
			shortBy, lo, hi, anchored, ordered, ages, log)
	}
}

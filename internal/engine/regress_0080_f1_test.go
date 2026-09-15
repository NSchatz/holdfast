package engine

import (
	"context"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// REFUTER ARTIFACT for S0080, finding F1. It documents the defect; it does not fix it.
//
// The contract: "WHEN `holdfast requeue <path>` names a file that has a terminal row, THE
// SYSTEM SHALL re-open that one row, print what it re-opened and print the count, and exit
// zero", where re-opening is defined by the re-opening criteria as offering the file "to
// the pipeline exactly as it offers a file it has never seen"; and "it SHALL never report
// success against an empty match set".
//
// A path selector reaches store.Failed rows (requeueStatuses lists Failed in its default
// branch and protects() does not protect them), but Requeue calls store.Reopen with
// clearFailures wired to sel.Failed, so a PATH requeue clears the recorded decision inputs
// and nothing else. For a row parked at max_failures the recorded inputs are not what holds
// it - the attempt count is - so the row is reported as re-opened, the command exits zero,
// and the next scan refuses the file exactly as before.
//
// The assertion is a DISJUNCTION on purpose, so it does not prescribe the fix: either the
// path selector must actually re-open a parked row (clear the count, as --failed does), or
// it must not report that row as re-opened (report it protected / point at --failed).
// Either repair turns this green.
func TestRegress0080F1_APathRequeueOfAParkedRowIsReportedAsReopenedButTheNextScanStillRefusesIt(t *testing.T) {
	ts := requeueStore(t)
	ctx := context.Background()

	// Park it: three failures against a bound of 3, the same fixture the --failed case uses.
	seedTerminal(t, ts, "/lib/parked.mkv", store.Failed, &store.Outcome{Reason: "ffmpeg died"})
	for i := 0; i < 2; i++ {
		if err := ts.Finish(ctx, "/lib/parked.mkv", "fp", store.Failed,
			&store.Outcome{Reason: "ffmpeg died"}, 3); err != nil {
			t.Fatalf("Finish: %v", err)
		}
	}
	if ok, _ := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{}); ok {
		t.Fatal("the fixture is wrong: the row is not parked, so re-opening it proves nothing")
	}

	// The operator names the one file they care about.
	res, err := Requeue(ctx, ts, RequeueSelector{Path: "/lib/parked.mkv"}, 3)
	reportedReopened := err == nil && len(res.Reopened) == 1 && res.Reopened[0] == "/lib/parked.mkv"

	claimable, cerr := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{})
	if cerr != nil {
		t.Fatalf("Claim: %v", cerr)
	}

	if reportedReopened && !claimable {
		t.Fatalf("`holdfast requeue <path>` printed \"re-opened: /lib/parked.mkv\" and "+
			"\"re-opened 1 row(s); the next scan runs the guards over them again.\" and exited zero, "+
			"but the next scan still refuses the file (Claim=%v). The attempt count is what holds a "+
			"parked row and the path selector never clears it, so the operator is told the file is "+
			"back in the pipeline when nothing about it has changed.\n"+
			"  Requeue result: reopened=%v protected=%+v err=%v",
			claimable, res.Reopened, res.Protected, err)
	}
}

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// REFUTER ARTIFACT for S0080, finding F1, at the command level. It documents the defect;
// it does not fix it.
//
// `holdfast requeue <path>` naming a row parked at max_failures prints what it re-opened,
// prints the count, and exits ZERO - and the next scan refuses the file exactly as before,
// because the attempt count is what holds a parked row and only --failed clears it.
//
// The assertion is a disjunction so it does not prescribe the fix: either the path selector
// re-opens the row for real, or it does not report it as re-opened.
func TestRegress0080F1CLI_APathRequeueOfAParkedRowExitsZeroAndTheNextScanStillRefusesIt(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "max_failures: 3\n")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		ctx := context.Background()
		if ok, err := st.Claim(ctx, "/lib/parked.mkv", "fp", "seed", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		for i := 0; i < 3; i++ {
			if err := st.Finish(ctx, "/lib/parked.mkv", "fp", store.Failed,
				&store.Outcome{Reason: "ffmpeg died"}, 3); err != nil {
				t.Fatalf("seed finish: %v", err)
			}
		}
	})
	if claimable(t, state, "/lib/parked.mkv", in) {
		t.Fatal("the fixture is wrong: the row is not parked, so re-opening it proves nothing")
	}

	code, out, errOut := cli(t, "requeue", "--config", cfgPath, "/lib/parked.mkv")
	t.Logf("exit=%d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)

	reportedReopened := code == 0 && strings.Contains(out, "re-opened 1 row(s)")
	nowClaimable := claimable(t, state, "/lib/parked.mkv", in)

	if reportedReopened && !nowClaimable {
		t.Fatalf("`holdfast requeue <path>` exited 0 reporting a re-opened row, but the next scan "+
			"still refuses the file. An operator told a file is back in the pipeline will not come "+
			"back to check.\nexit=%d\nstdout:\n%s", code, out)
	}
}

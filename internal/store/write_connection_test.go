package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// [AC-9] THE SYSTEM SHALL keep every write serialized on one connection, so concurrent
// workers never observe "database is locked": the write door's single-connection property
// is unchanged by this spec.
//
// This is a PRESERVATION criterion and it is asserted on the property rather than on the
// line that sets it. Opening a second, read-only door beside this one is exactly the kind
// of change that invites somebody to widen the write pool while they are in here, and the
// bound is what actually prevents "database is locked": with only ever one connection
// there is never a second to contend with, so every Claim/Advance/Finish is atomic without
// an explicit transaction around it.
func TestOpen_WriteHandleKeepsOneConnection(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	if got := st.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("the write handle allows %d open connections, want 1 - a second connection is "+
			"what produces \"database is locked\" under concurrent workers", got)
	}

	// And it still serializes in practice: concurrent claimants on distinct fresh keys
	// all succeed, and no caller sees a lock error.
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := "/lib/concurrent" + string(rune('a'+i)) + ".mkv"
			_, errs[i] = st.Claim(ctx, p, "f:f", "w", 3, DecisionInputs{})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent claim %d failed: %v", i, err)
		}
	}
}

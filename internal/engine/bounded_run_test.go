package engine

// The bounded run in the ENGINE (S0100): one named file, or a count of decisions.
//
// A bounded run uses the identical per-file pipeline, so nothing here re-proves a guard,
// a gate or the swap - those are proved over the whole library everywhere else in this
// package. What is new, and what these cases grade, is the SHAPE of the pass: what it
// carries to a terminal outcome, what it stops at, the two whole-library passes it does
// not run, and what it says out loud about being bounded.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// staleTemp writes an orphaned work-in-progress temp of the kind a killed run leaves
// behind - the file the stale-temp sweep exists to discard. It is named after a source
// that is really there, because a temp with no source beside it is one the sweep HOLDS
// (strayReplacementHold): nothing can establish what such a file is, and a fixture built
// that way would be held for a reason that has nothing to do with the bound.
func staleTemp(t *testing.T, root, stem string) string {
	t.Helper()
	p := filepath.Join(root, stem+"."+TempMarker+".mkv")
	if err := os.WriteFile(p, []byte("half an encode"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBoundedRun_DoesNotEnforceRetention grades AC-10, and it grades it BOTH DIRECTIONS
// over ONE fixture library, because this is the single way a bounded run could cost an
// operator audit history that no re-run restores.
//
// Retention is ENABLED here on purpose. It is disabled by the shipped default and
// enforceRetention returns before it reads the store at all in that state, so a fixture
// that left the default in place would assert precisely nothing: the bounded run would
// "prune nothing" because nothing prunes.
//
// The order is the evidence. The bounded pass runs FIRST over rows that are provably
// prunable and removes none of them; the unbounded pass then runs over the SAME library,
// the SAME ledger and the SAME configuration and removes them. Without the second half
// the first is a fixture with nothing to take.
func TestBoundedRun_DoesNotEnforceRetention(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	// Two files already at the target codec: a bounded pass has something real to decide,
	// and deciding it is a skip rather than an encode.
	for i := 0; i < 2; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "already"+strconv.Itoa(i)+".mkv"), "800k")
	}
	temp := staleTemp(t, root, "already0")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 2 // aggressive on purpose: prune everything it may
	})
	if !eng.Cfg.RetentionEnabled() {
		t.Fatal("this fixture asserts nothing unless retention is ENABLED")
	}
	ts := eng.Store.(*testStore)
	// History for files that were in the library and are not any more: this run lists the
	// directory they name and does not find them, which is exactly what makes them
	// removable - and what the bounded pass must not conclude from.
	for i := 0; i < 12; i++ {
		seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	}
	before := len(terminalRows(t, ts))
	if before != 12 {
		t.Fatalf("the fixture seeded %d terminal rows, want 12", before)
	}

	// --- direction one: the bounded pass removes nothing -------------------------------
	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded: %v", err)
	}
	after := terminalRows(t, ts)
	if len(after) < before {
		t.Fatalf("a bounded run removed %d terminal row(s); it must remove none - the pass did not list "+
			"the whole library, so an absent file is no evidence that the file is gone", before-len(after))
	}
	if _, err := os.Stat(temp); err != nil {
		t.Errorf("a bounded run swept the stale temp %s: %v", temp, err)
	}

	// --- direction two: the same fixture DOES prune under an unbounded pass ------------
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 2 {
		t.Fatalf("an unbounded pass over the same fixture left %d terminal rows, want 2 - if it removed "+
			"nothing either, the bounded half above proved nothing", got)
	}
	if _, err := os.Stat(temp); err == nil {
		t.Errorf("an unbounded pass left the stale temp %s in place, so the bounded half proved nothing", temp)
	}
}

package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

func sweepWith(ok, corrupt, unreadable int64) store.HealthSweep {
	return store.HealthSweep{ID: 3, FinishedAt: time.Unix(1_790_000_000, 0),
		Counts: store.HealthCounts{OK: ok, Corrupt: corrupt, Unreadable: unreadable}}
}

// TestNotify_HealthSweep_OneSummaryWhenAFileFailsNoneWhenClean: a clean sweep sends nothing;
// one that found problems sends ONE summary that says it changed nothing, names the files
// with their reasons up to the bound, and counts the rest. No credential is in it.
func TestNotify_HealthSweep_OneSummaryWhenAFileFailsNoneWhenClean(t *testing.T) {
	n, sink := newWithSink(t, "generic://secret-token@example")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.HealthFileChecked(store.HealthCorrupt) // per-file results send nothing
	n.HealthSweepFinished(sweepWith(10, 0, 0), nil)
	if msg, ok := recv(t, sink, 300*time.Millisecond); ok {
		t.Fatalf("a clean sweep notified: %q", msg)
	}

	problems := []store.HealthCheck{
		{Path: "/lib/b.mkv", Result: store.HealthCorrupt, Reason: "File ended prematurely"},
		{Path: "/lib/c.mkv", Result: store.HealthUnreadable, Reason: "not a regular file"},
	}
	n.HealthSweepFinished(sweepWith(8, 1, 1), problems)
	msg, ok := recv(t, sink, 2*time.Second)
	if !ok {
		t.Fatal("no summary for a sweep that found problems")
	}
	want := "holdfast health sweep: 2 of 10 file(s) did not pass a full decode (1 corrupt, 1 unreadable). " +
		"Report only - no file was moved, renamed, repaired or deleted.\n" +
		"corrupt /lib/b.mkv: File ended prematurely\nunreadable /lib/c.mkv: not a regular file"
	if msg != want {
		t.Errorf("summary\n%q\nwant\n%q", msg, want)
	}
	if strings.Contains(msg, "secret-token") {
		t.Error("the summary carries the credential")
	}
	if extra, ok := recv(t, sink, 300*time.Millisecond); ok {
		t.Errorf("a second message was sent: %q", extra)
	}
}

// TestNotify_HealthSweep_NamesAtMostTheBoundAndCountsTheRest.
func TestNotify_HealthSweep_NamesAtMostTheBoundAndCountsTheRest(t *testing.T) {
	n, sink := newWithSink(t, "generic://example")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	var problems []store.HealthCheck
	for i := 0; i < maxNamedProblems+5; i++ {
		problems = append(problems, store.HealthCheck{Path: fmt.Sprintf("/lib/%02d.mkv", i), Result: store.HealthCorrupt, Reason: "x"})
	}
	n.HealthSweepFinished(sweepWith(0, 40, 0), problems)
	msg, ok := recv(t, sink, 2*time.Second)
	if !ok {
		t.Fatal("no summary")
	}
	lines := strings.Split(msg, "\n")
	if len(lines) != 1+maxNamedProblems+1 || lines[len(lines)-1] != "... and 30 more: GET /api/health lists them." ||
		lines[maxNamedProblems] != "corrupt /lib/09.mkv: x" {
		t.Errorf("summary lines %q", lines)
	}
	// Exactly the bound named and nothing left: no trailing count.
	n.HealthSweepFinished(sweepWith(0, 2, 0), problems[:2])
	msg, _ = recv(t, sink, 2*time.Second)
	if strings.Contains(msg, "more") || strings.Count(msg, "\n") != 2 {
		t.Errorf("summary for two problems both named: %q", msg)
	}
}

package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// The door `serve` reads the ledger through (S0096).
//
// store.Open caps the write handle at one connection, which is what prevents "database is
// locked" under concurrent workers - and it also means a reporting read sits in the same
// queue as the engine's next Claim/Advance/Finish. The reporting door is a second handle
// onto the same file that the database itself refuses every write on.

// [AC-7] WHEN the server reads the ledger to publish a frame or answer a read endpoint THE
// SYSTEM SHALL read through a handle the database itself refuses every write on, so a
// reporting read cannot occupy the connection the engine's Claim/Advance/Finish writes
// queue on.
//
// "The database itself refuses" is the assertion, and it is asserted by TRYING A WRITE. A
// promise made in Go above the driver is one a later caller can quietly withdraw; a write
// SQLite rejects is a physical property of the handle.
func TestServe_ReadsGoThroughAReadOnlyHandle(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	write, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = write.Close() }()

	var logs bytes.Buffer
	reads := reportingDoor(dbPath, write, slog.New(slog.NewJSONHandler(&logs, nil)))
	if reads == store.Store(write) {
		t.Fatal("the reporting door IS the write handle: every reporting read still queues " +
			"on the connection the engine's writes go through")
	}
	defer func() { _ = reads.Close() }()

	ctx := context.Background()

	// It reads. A door that refused reads too would satisfy the next assertion and be
	// useless.
	if _, err := reads.Summary(ctx); err != nil {
		t.Fatalf("the reporting door cannot read the ledger: %v", err)
	}
	if _, err := reads.List(ctx, nil, 10); err != nil {
		t.Fatalf("the reporting door cannot list rows: %v", err)
	}
	if tot := reads.CountRows(ctx, nil); tot.Err != nil {
		t.Fatalf("the reporting door cannot count rows: %v", tot.Err)
	}
	for name, a := range map[string]error{
		"outcomes":       reads.Aggregates(ctx).Outcomes.Err,
		"skips_by_guard": reads.Aggregates(ctx).SkipsByGuard.Err,
		"size_ratio":     reads.Aggregates(ctx).SizeRatio.Err,
	} {
		if a != nil {
			t.Errorf("the reporting door cannot compute %s: %v", name, a)
		}
	}

	// And it writes NOTHING. Each of these is a different write path into the store, and
	// every one of them has to come back as a refusal from the driver.
	if _, err := reads.ExcludePath(ctx, "/lib/withheld.mkv"); err == nil {
		t.Error("the reporting door accepted a path exclusion: it is not read-only")
	} else if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") &&
		!strings.Contains(strings.ToLower(err.Error()), "read only") {
		t.Errorf("the write was refused for some other reason than the handle being read-only: %v", err)
	}
	if ok, err := reads.Claim(ctx, "/lib/a.mkv", "f:f", "w", 3, store.DecisionInputs{}); err == nil && ok {
		t.Error("the reporting door claimed a job: a reporting read must never take work")
	}
	if err := reads.Advance(ctx, "/lib/a.mkv", "f:f", store.Encoding); err == nil {
		t.Error("the reporting door advanced a job")
	}
	if err := reads.Finish(ctx, "/lib/a.mkv", "f:f", store.Done, nil, 3); err == nil {
		t.Error("the reporting door finished a job")
	}

	// The write handle is untouched by any of that: the engine still writes.
	if _, err := write.ExcludePath(ctx, "/lib/withheld.mkv"); err != nil {
		t.Errorf("the write handle stopped writing once a read door was opened beside it: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("a door that opened cleanly recorded something: %s", logs.String())
	}
}

// [AC-8] IF the read-only handle cannot be opened THEN the server SHALL still start and
// serve, read through the existing write handle, and record at `warn` which door it tried,
// why it failed and that it is degrading to the write handle.
//
// Reporting is not what this daemon is for. A ledger the read door refuses - one written
// by a different schema version, a state directory that went unreadable - must cost an
// operator their figures at worst and never their encodes.
func TestServe_ReadOnlyOpenFailureDegradesToTheWriteHandle(t *testing.T) {
	dir := t.TempDir()
	write, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = write.Close() }()

	// A path with no ledger at it: the read door creates nothing and refuses, by design.
	missing := filepath.Join(dir, "absent", "jobs.db")

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reads := reportingDoor(missing, write, log)

	if reads != store.Store(write) {
		t.Fatal("a read door that could not be opened did not degrade to the write handle: " +
			"the daemon would be serving through a handle nothing opened")
	}
	// It still reads, which is the whole point of degrading rather than failing.
	if _, err := reads.Summary(context.Background()); err != nil {
		t.Errorf("the degraded door cannot read: %v", err)
	}

	line := logs.String()
	if line == "" {
		t.Fatal("degrading to the write handle recorded nothing: an operator has no way to learn " +
			"their reporting reads are back on the engine's connection")
	}
	if !strings.Contains(line, `"level":"WARN"`) {
		t.Errorf("the degrade was not recorded at warn - it is a continued-but-degraded state, not an error and not an info: %s", line)
	}
	for what, want := range map[string]string{
		"the dependency":    "read-only door",
		"what it tried":     "OpenReadOnly",
		"the ledger":        missing,
		"what it does next": "write handle",
		"why it failed":     "err",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the record does not name %s (%q): %s", what, want, line)
		}
	}
}

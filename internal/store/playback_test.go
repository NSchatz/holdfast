package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedJobRow writes one jobs row with a chosen status and transition time, through the
// database rather than the claim path, so a test can place a swap at a known second.
func seedJobRow(t *testing.T, s *SQLite, path, fp string, st Status, at int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO jobs (path, fingerprint, status, fail_count, updated_at) VALUES (?, ?, ?, 0, ?)`,
		path, fp, string(st), at); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// swapsOf reads the recorded swaps as "path@fingerprint:swapped:superseded:current" strings.
func swapsOf(t *testing.T, s *SQLite) []string {
	t.Helper()
	ws, err := s.PlaybackSwaps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s@%s:%d:%d:%v", w.Path, w.Fingerprint, w.SwappedAt, w.SupersededAt, w.Current))
	}
	return out
}

func sameList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

// TestPlayback_SwapsAreRecordedOnceFromDoneRowsAndLiveRetentions: every done row and every live
// retention's swapped file is a swap, recorded once with the second it committed; a skipped,
// failed or would-transcode row and a restored retention are not.
func TestPlayback_SwapsAreRecordedOnceFromDoneRowsAndLiveRetentions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedJobRow(t, s, "/lib/b.mkv", "20:2", Done, 300)
	seedJobRow(t, s, "/lib/a.mkv", "10:1", Done, 100)
	seedJobRow(t, s, "/lib/skipped.mkv", "1:1", Skipped, 50)
	seedJobRow(t, s, "/lib/failed.mkv", "1:1", Failed, 50)
	seedJobRow(t, s, "/lib/would.mkv", "1:1", WouldTranscode, 50)
	// A retention whose done row is gone (re-opened and re-derived skipped before any poll saw
	// it) still names the swap.
	seedJobRow(t, s, "/lib/kept.mkv", "30:3", Skipped, 900)
	if err := s.Retain(ctx, Retained{SourcePath: "/lib/kept.mkv", SwappedPath: "/lib/kept.mkv",
		RetainedPath: "/lib/.holdfast-undo/kept", SourceBytes: 1, SwappedFingerprint: "30:3", RetainedAt: 400,
		ExpiresAt: 9000}); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, Retained{SourcePath: "/lib/restored.mkv", SwappedPath: "/lib/restored.mkv",
		RetainedPath: "/lib/.holdfast-undo/r", SourceBytes: 1, SwappedFingerprint: "40:4", RetainedAt: 400,
		ExpiresAt: 9000}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRestored(ctx, "/lib/restored.mkv", 500); err != nil {
		t.Fatal(err)
	}

	added, superseded, err := s.RecordSwaps(ctx, 1000)
	if err != nil || added != 3 || superseded != 0 {
		t.Fatalf("first record: %d added, %d superseded, %v", added, superseded, err)
	}
	sameList(t, "swaps", swapsOf(t, s), []string{"/lib/a.mkv@10:1:100:0:true", "/lib/b.mkv@20:2:300:0:true",
		"/lib/kept.mkv@30:3:400:0:true"})
	if added, superseded, err := s.RecordSwaps(ctx, 2000); err != nil || added != 0 || superseded != 0 {
		t.Errorf("a second record: %d added, %d superseded, %v", added, superseded, err)
	}
}

// TestPlayback_AReopenedSwapKeepsItsEvidence: a changed encoder setting or a requeue re-opens the
// done row and the guard re-derives it skipped under the same path and fingerprint. The bytes are
// the swap's, so the swap stays current and its sessions stay evidence.
func TestPlayback_AReopenedSwapKeepsItsEvidence(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedJobRow(t, s, "/lib/film.mkv", "9:9", Done, 1000)
	if _, _, err := s.RecordSwaps(ctx, 1500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPlaybackSessions(ctx, []PlaybackSession{{Source: "tautulli", SessionID: "1",
		Path: "/lib/film.mkv", Fingerprint: "9:9", StartedAt: 2000, RecordedAt: 2100}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status = 'skipped', reason = 'already-at-target-codec', updated_at = 3000
		WHERE path = '/lib/film.mkv'`); err != nil {
		t.Fatal(err)
	}
	if _, superseded, err := s.RecordSwaps(ctx, 4000); err != nil || superseded != 0 {
		t.Fatalf("re-derived skip: %d superseded, %v", superseded, err)
	}
	sameList(t, "swaps", swapsOf(t, s), []string{"/lib/film.mkv@9:9:1000:0:true"})
	if ev, err := s.PlaybackEvidence(ctx); err != nil || len(ev) != 1 {
		t.Errorf("evidence after the re-derived skip: %+v, %v", ev, err)
	}
}

// TestPlayback_AReplacedOrRestoredFileSupersedesItsSwap: another file at the path, a restore, and
// a newer swap each end the swap, at the earliest second the ledger shows for it; its sessions
// are no longer evidence. Between two rows in the same second, the one inserted last is newer.
func TestPlayback_AReplacedOrRestoredFileSupersedesItsSwap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, p := range []string{"/lib/replaced.mkv", "/lib/restored.mkv", "/lib/moved.mp4", "/lib/again.mkv"} {
		seedJobRow(t, s, p, "1:1", Done, 1000)
	}
	if err := s.Retain(ctx, Retained{SourcePath: "/lib/moved.mkv", SwappedPath: "/lib/moved.mp4",
		RetainedPath: "/lib/.holdfast-undo/m", SourceBytes: 1, SwappedFingerprint: "1:1", RetainedAt: 1000,
		ExpiresAt: 9000}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSwaps(ctx, 1100); err != nil {
		t.Fatal(err)
	}
	var sessions []PlaybackSession
	for i, p := range []string{"/lib/replaced.mkv", "/lib/restored.mkv", "/lib/moved.mp4", "/lib/again.mkv"} {
		sessions = append(sessions, PlaybackSession{Source: "tautulli", SessionID: fmt.Sprint(i), Path: p,
			Fingerprint: "1:1", StartedAt: 1200, RecordedAt: 1300})
	}
	if _, err := s.RecordPlaybackSessions(ctx, sessions); err != nil {
		t.Fatal(err)
	}

	// An arr imports a new release over the replaced file: the scan records the new bytes.
	seedJobRow(t, s, "/lib/replaced.mkv", "2:2", Skipped, 2000)
	// An in-place restore: the done row goes and the original is recorded skipped.
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE path = '/lib/restored.mkv'`); err != nil {
		t.Fatal(err)
	}
	seedJobRow(t, s, "/lib/restored.mkv", "8:8", Skipped, 2100)
	// A container-changing restore: the swapped path has no row left, and the retention says so.
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE path = '/lib/moved.mp4'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRestored(ctx, "/lib/moved.mkv", 2200); err != nil {
		t.Fatal(err)
	}
	// Swapped again: a new done row, and a second done row in the same second.
	seedJobRow(t, s, "/lib/again.mkv", "3:3", Done, 2300)
	seedJobRow(t, s, "/lib/again.mkv", "4:4", Done, 2300)

	// Read before the poll records it: the restores and replacements already read as not current.
	for _, w := range swapsOf(t, s) {
		if strings.HasSuffix(w, ":true") {
			t.Errorf("before the poll, %s still reads as current", w)
		}
	}
	added, superseded, err := s.RecordSwaps(ctx, 5000)
	if err != nil || added != 2 || superseded != 5 {
		t.Fatalf("record: %d added, %d superseded, %v", added, superseded, err)
	}
	sameList(t, "swaps", swapsOf(t, s), []string{
		"/lib/again.mkv@1:1:1000:2300:false", "/lib/again.mkv@3:3:2300:2300:false", "/lib/again.mkv@4:4:2300:0:true",
		"/lib/moved.mp4@1:1:1000:2200:false",
		"/lib/replaced.mkv@1:1:1000:2000:false",
		"/lib/restored.mkv@1:1:1000:2100:false",
	})
	if ev, err := s.PlaybackEvidence(ctx); err != nil || len(ev) != 0 {
		t.Errorf("evidence on superseded swaps: %+v, %v", ev, err)
	}
	// A swap superseded before now is never un-superseded, and nothing it superseded moves.
	if _, superseded, err := s.RecordSwaps(ctx, 6000); err != nil || superseded != 0 {
		t.Errorf("a second record superseded %d: %v", superseded, err)
	}
	// With nothing in the ledger to date it, a swap that stops being current is superseded now.
	seedJobRow(t, s, "/lib/now.mkv", "5:5", Done, 7000)
	if _, _, err := s.RecordSwaps(ctx, 7100); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, Retained{SourcePath: "/lib/now.mkv", SwappedPath: "/lib/now.mkv", RetainedPath: "/x",
		SourceBytes: 1, SwappedFingerprint: "5:5", RetainedAt: 7000, ExpiresAt: 9000}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRestored(ctx, "/lib/now.mkv", 9999); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSwaps(ctx, 8000); err != nil {
		t.Fatal(err)
	}
	if got := swapsOf(t, s); !strings.Contains(strings.Join(got, " "), "/lib/now.mkv@5:5:7000:8000:false") {
		t.Errorf("superseded at now: %v", got)
	}
}

// TestPlayback_ARequeueOfAnOlderFileDoesNotSupersedeTheSwap: a row an earlier file at the path
// left, which a requeue re-opens, gets a transition time later than the swap's - it is still the
// older file, and the swap stays current with its evidence. A swap whose mark the ledger no longer
// supports is current again, on positive evidence only.
func TestPlayback_ARequeueOfAnOlderFileDoesNotSupersedeTheSwap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// An earlier download failed at the path, then the file that replaced it was swapped.
	seedJobRow(t, s, "/lib/film.mkv", "500:1", Failed, 100)
	seedJobRow(t, s, "/lib/film.mkv", "300:2", Done, 1000)
	if _, _, err := s.RecordSwaps(ctx, 1100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPlaybackSessions(ctx, []PlaybackSession{{Source: "tautulli", SessionID: "1",
		Path: "/lib/film.mkv", Fingerprint: "300:2", StartedAt: 1200, RecordedAt: 1300}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Reopen(ctx, "/lib/film.mkv", "500:1", true); err != nil || !ok {
		t.Fatalf("requeue the older row: %v %v", ok, err)
	}
	if _, superseded, err := s.RecordSwaps(ctx, 9_000_000_000); err != nil || superseded != 0 {
		t.Fatalf("after the requeue: %d superseded, %v", superseded, err)
	}
	sameList(t, "swaps", swapsOf(t, s), []string{"/lib/film.mkv@300:2:1000:0:true"})
	if ev, err := s.PlaybackEvidence(ctx); err != nil || len(ev) != 1 {
		t.Errorf("evidence after the requeue: %+v, %v", ev, err)
	}

	// A mark nothing in the ledger supports any longer is lifted ...
	if _, err := s.db.Exec(`UPDATE playback_swaps SET superseded_at = 5000`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSwaps(ctx, 9_000_000_001); err != nil {
		t.Fatal(err)
	}
	sameList(t, "swaps after the mark is lifted", swapsOf(t, s), []string{"/lib/film.mkv@300:2:1000:0:true"})
	// ... but only on positive evidence: with no jobs row at the path at all, or with a restore
	// recorded, a superseded swap stays superseded.
	seedJobRow(t, s, "/lib/gone.mkv", "1:1", Done, 1000)
	if err := s.Retain(ctx, Retained{SourcePath: "/lib/kept.mkv", SwappedPath: "/lib/kept.mkv", RetainedPath: "/x",
		SourceBytes: 1, SwappedFingerprint: "2:2", RetainedAt: 1000, ExpiresAt: 9000}); err != nil {
		t.Fatal(err)
	}
	seedJobRow(t, s, "/lib/kept.mkv", "2:2", Done, 1000)
	if _, _, err := s.RecordSwaps(ctx, 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE path = '/lib/gone.mkv'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRestored(ctx, "/lib/kept.mkv", 2500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE playback_swaps SET superseded_at = 3000 WHERE path != '/lib/film.mkv'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordSwaps(ctx, 4000); err != nil {
		t.Fatal(err)
	}
	sameList(t, "swaps", swapsOf(t, s), []string{"/lib/film.mkv@300:2:1000:0:true",
		"/lib/gone.mkv@1:1:1000:3000:false", "/lib/kept.mkv@2:2:1000:3000:false"})
}

// TestPlayback_SessionsAreRecordedOnceAndReadBackWithTheirNulls: a session already recorded is
// left as it was and not counted again; a NULL reads back as nil and never as 0, which for an
// offset would be a session that never left the first frame.
func TestPlayback_SessionsAreRecordedOnceAndReadBackWithTheirNulls(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedJobRow(t, s, "/lib/film.mkv", "9:9", Done, 1000)
	if _, _, err := s.RecordSwaps(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	first := []PlaybackSession{
		{Source: "tautulli", SessionID: "41", Path: "/lib/film.mkv", Fingerprint: "9:9", StartedAt: 2000,
			StoppedAt: i64(2100), OffsetMS: i64(0), OffsetDerived: true, DurationMS: i64(8_700_000),
			PlayedSeconds: i64(95), Client: "c-1", Platform: "Chrome", Player: "Plex Web", Decision: "copy", RecordedAt: 3000},
		{Source: "tautulli", SessionID: "40", Path: "/lib/film.mkv", Fingerprint: "9:9", StartedAt: 1500,
			RecordedAt: 3000},
		// On a fingerprint no swap records: not evidence about anything.
		{Source: "tautulli", SessionID: "39", Path: "/lib/film.mkv", Fingerprint: "8:8", StartedAt: 1400,
			RecordedAt: 3000},
	}
	n, err := s.RecordPlaybackSessions(ctx, first)
	if err != nil || n != 3 {
		t.Fatalf("first record: %d, %v; want 3 new", n, err)
	}
	again := []PlaybackSession{first[0], {Source: "tautulli", SessionID: "42", Path: "/lib/film.mkv",
		Fingerprint: "9:9", StartedAt: 2500, OffsetMS: i64(5), RecordedAt: 4000}}
	again[0].OffsetMS = i64(999) // a re-read row is not rewritten
	n, err = s.RecordPlaybackSessions(ctx, again)
	if err != nil || n != 1 {
		t.Fatalf("second record: %d, %v; want 1 new", n, err)
	}
	if n, err := s.RecordPlaybackSessions(ctx, nil); err != nil || n != 0 {
		t.Fatalf("an empty batch: %d, %v", n, err)
	}
	got, err := s.PlaybackEvidence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].SessionID != "40" || got[1].SessionID != "41" || got[2].SessionID != "42" {
		t.Fatalf("evidence %+v, want 40 41 42 (oldest first)", got)
	}
	if g := got[0]; g.StoppedAt != nil || g.OffsetMS != nil || g.DurationMS != nil || g.PlayedSeconds != nil ||
		g.OffsetDerived || g.Client != "" {
		t.Errorf("a session recorded with nothing optional read back %+v", g)
	}
	g := got[1]
	if g.OffsetMS == nil || *g.OffsetMS != 0 || !g.OffsetDerived || g.PlayedSeconds == nil || *g.PlayedSeconds != 95 ||
		g.StoppedAt == nil || *g.StoppedAt != 2100 || g.DurationMS == nil || *g.DurationMS != 8_700_000 ||
		g.Client != "c-1" || g.Platform != "Chrome" || g.Player != "Plex Web" || g.Decision != "copy" ||
		g.RecordedAt != 3000 || g.Source != "tautulli" || g.Path != "/lib/film.mkv" || g.Fingerprint != "9:9" ||
		g.StartedAt != 2000 {
		t.Errorf("session read back as %+v (the re-read offset 999 must not have replaced 0)", g)
	}
}

// TestPlayback_PollStateRoundTrips: no poll is no row; a poll that never read the history to
// its end reads back as covering nothing.
func TestPlayback_PollStateRoundTrips(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.PlaybackPollOf(ctx, "tautulli"); ok || err != nil {
		t.Fatalf("before any poll: ok %v err %v", ok, err)
	}
	if err := s.SetPlaybackPoll(ctx, PlaybackPoll{Source: "tautulli", Cursor: 10, LastAttemptAt: 20,
		LastFailure: "unreachable"}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.PlaybackPollOf(ctx, "tautulli")
	if err != nil || !ok || p != (PlaybackPoll{Source: "tautulli", Cursor: 10, LastAttemptAt: 20, LastFailure: "unreachable"}) {
		t.Fatalf("first state %+v %v %v", p, ok, err)
	}
	var covered any
	if err := s.db.QueryRow(`SELECT covered_through FROM playback_polls`).Scan(&covered); err != nil || covered != nil {
		t.Errorf("covered_through stored as %v (%v), want NULL", covered, err)
	}
	want := PlaybackPoll{Source: "tautulli", Cursor: 30, CoveredThrough: 25, LastAttemptAt: 40, Unresolved: 6}
	if err := s.SetPlaybackPoll(ctx, want); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := s.PlaybackPollOf(ctx, "tautulli"); p != want {
		t.Errorf("second state %+v, want %+v", p, want)
	}
	if _, ok, _ := s.PlaybackPollOf(ctx, "other"); ok {
		t.Error("another source's state was found")
	}
}

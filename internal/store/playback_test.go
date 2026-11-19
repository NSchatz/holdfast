package store

import (
	"context"
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

// TestPlayback_SwappedFilesAreTheDoneRows: what a session can be attributed to is exactly the
// done rows, path first and oldest swap first within a path, each with its swap time.
func TestPlayback_SwappedFilesAreTheDoneRows(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedJobRow(t, s, "/lib/b.mkv", "20:2", Done, 300)
	seedJobRow(t, s, "/lib/a.mkv", "10:2", Done, 200)
	seedJobRow(t, s, "/lib/a.mkv", "10:1", Done, 100)
	seedJobRow(t, s, "/lib/skipped.mkv", "1:1", Skipped, 50)
	seedJobRow(t, s, "/lib/failed.mkv", "1:1", Failed, 50)
	seedJobRow(t, s, "/lib/would.mkv", "1:1", WouldTranscode, 50)

	got, err := s.SwappedFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []SwappedFile{{"/lib/a.mkv", "10:1", 100}, {"/lib/a.mkv", "10:2", 200}, {"/lib/b.mkv", "20:2", 300}}
	if len(got) != len(want) {
		t.Fatalf("swapped files %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("swapped file %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPlayback_SessionsAreRecordedOnceAndReadBackWithTheirNulls: a session already recorded is
// left as it was and not counted again; a NULL offset reads back as nil and never as 0, which
// would be a session that never left the first frame; and only sessions on a live done row are
// evidence.
func TestPlayback_SessionsAreRecordedOnceAndReadBackWithTheirNulls(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedJobRow(t, s, "/lib/film.mkv", "9:9", Done, 1000)

	first := []PlaybackSession{
		{Source: "tautulli", SessionID: "41", Path: "/lib/film.mkv", Fingerprint: "9:9", StartedAt: 2000,
			StoppedAt: i64(2100), OffsetMS: i64(0), DurationMS: i64(8_700_000), Platform: "Chrome",
			Player: "Plex Web", Decision: "copy", RecordedAt: 3000},
		{Source: "tautulli", SessionID: "40", Path: "/lib/film.mkv", Fingerprint: "9:9", StartedAt: 1500,
			RecordedAt: 3000},
		// Played on a fingerprint no done row carries any longer: not evidence about the file.
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
	if len(got) != 3 {
		t.Fatalf("evidence %+v, want the three sessions on the done row", got)
	}
	if got[0].SessionID != "40" || got[1].SessionID != "41" || got[2].SessionID != "42" {
		t.Errorf("order %s %s %s, want 40 41 42 (oldest first)", got[0].SessionID, got[1].SessionID, got[2].SessionID)
	}
	if got[0].StoppedAt != nil || got[0].OffsetMS != nil || got[0].DurationMS != nil {
		t.Errorf("a session recorded with no stop, offset or duration read back %v %v %v", got[0].StoppedAt,
			got[0].OffsetMS, got[0].DurationMS)
	}
	g := got[1]
	if g.OffsetMS == nil || *g.OffsetMS != 0 {
		t.Errorf("an offset of 0 read back as %v; the re-read value 999 must not have replaced it either", g.OffsetMS)
	}
	if g.StoppedAt == nil || *g.StoppedAt != 2100 || g.DurationMS == nil || *g.DurationMS != 8_700_000 ||
		g.Platform != "Chrome" || g.Player != "Plex Web" || g.Decision != "copy" || g.RecordedAt != 3000 ||
		g.Source != "tautulli" || g.Path != "/lib/film.mkv" || g.Fingerprint != "9:9" || g.StartedAt != 2000 {
		t.Errorf("session read back as %+v", g)
	}

	// The path is swapped again later (a new source arrived and was encoded): the newer done row
	// is the file there now, and the earlier swap's sessions are not evidence about it.
	seedJobRow(t, s, "/lib/film.mkv", "7:7", Done, 5000)
	if got, err := s.PlaybackEvidence(ctx); err != nil || len(got) != 0 {
		t.Errorf("after a newer swap of the path: %+v, %v", got, err)
	}
	if n, err := s.RecordPlaybackSessions(ctx, []PlaybackSession{{Source: "tautulli", SessionID: "50",
		Path: "/lib/film.mkv", Fingerprint: "7:7", StartedAt: 6000, RecordedAt: 6000}}); err != nil || n != 1 {
		t.Fatalf("record on the newer swap: %d %v", n, err)
	}
	if got, err := s.PlaybackEvidence(ctx); err != nil || len(got) != 1 || got[0].SessionID != "50" {
		t.Errorf("the newer swap's own session: %+v, %v", got, err)
	}
	// Two done rows of one path at the same second: the greater fingerprint is the current one,
	// so exactly one of them is reported and never both.
	seedJobRow(t, s, "/lib/tie.mkv", "1:1", Done, 100)
	seedJobRow(t, s, "/lib/tie.mkv", "2:2", Done, 100)
	if _, err := s.RecordPlaybackSessions(ctx, []PlaybackSession{
		{Source: "tautulli", SessionID: "60", Path: "/lib/tie.mkv", Fingerprint: "1:1", StartedAt: 200, RecordedAt: 300},
		{Source: "tautulli", SessionID: "61", Path: "/lib/tie.mkv", Fingerprint: "2:2", StartedAt: 200, RecordedAt: 300},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.PlaybackEvidence(ctx)
	if err != nil || len(got) != 2 || got[1].SessionID != "61" {
		t.Errorf("a tie on the swap second: %+v, %v", got, err)
	}
	if err := s.Delete(ctx, "/lib/film.mkv", "7:7"); err != nil {
		t.Fatal(err)
	}

	// The file is restored: its done row goes, and its sessions are no longer evidence about
	// what is at the path.
	if err := s.Delete(ctx, "/lib/film.mkv", "9:9"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlaybackEvidence(ctx); err != nil || len(got) != 1 || got[0].SessionID != "61" {
		t.Errorf("after the done row went: %+v, %v", got, err)
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
	want := PlaybackPoll{Source: "tautulli", Cursor: 30, CoveredThrough: 25, LastAttemptAt: 40}
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

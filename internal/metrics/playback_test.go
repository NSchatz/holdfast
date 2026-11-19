package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/playback"
	"github.com/NSchatz/holdfast/internal/store"
)

// seedPlayback records two swapped files: one whose two sessions end at one point well before
// the end (a suspected stall) and one a session played through.
func seedPlayback(t *testing.T, st *store.SQLite) {
	t.Helper()
	ctx := context.Background()
	dur := int64(8_700_000)
	for _, f := range []string{"/lib/stalls.mkv", "/lib/plays.mkv"} {
		if _, err := st.Claim(ctx, f, "1:1", "w", 3, store.InputsRead(nil)); err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, f, "1:1", store.Done, &store.Outcome{}, 3); err != nil {
			t.Fatal(err)
		}
	}
	off := func(v int64) *int64 { return &v }
	if _, err := st.RecordPlaybackSessions(ctx, []store.PlaybackSession{
		{Source: "tautulli", SessionID: "1", Path: "/lib/stalls.mkv", Fingerprint: "1:1", OffsetMS: off(2_000_000), DurationMS: &dur},
		{Source: "tautulli", SessionID: "2", Path: "/lib/stalls.mkv", Fingerprint: "1:1", OffsetMS: off(2_050_000), DurationMS: &dur},
		{Source: "tautulli", SessionID: "3", Path: "/lib/plays.mkv", Fingerprint: "1:1", OffsetMS: off(dur), DurationMS: &dur},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMetrics_PlaybackSeries: absent unless this daemon collects evidence, absent until a poll has
// read the history to its end, and then the suspected stalls, the files played through and when
// the history was read. No series names a file.
func TestMetrics_PlaybackSeries(t *testing.T) {
	st := openStore(t)
	seedPlayback(t, st)
	names := []string{"holdfast_playback_suspect_files ", "holdfast_playback_played_through_files ",
		"holdfast_playback_history_read_timestamp_seconds "}

	m := New(st, nil)
	absent := func(when string) {
		body := scrape(t, m)
		for _, n := range names {
			if strings.Contains(body, n) {
				t.Errorf("%s: %s is published", when, n)
			}
		}
	}
	absent("evidence off")
	m.SetPlayback("tautulli", playback.RulesFor(&config.Config{}))
	absent("before any poll")
	if err := st.SetPlaybackPoll(context.Background(), store.PlaybackPoll{Source: "tautulli", LastAttemptAt: 5,
		LastFailure: "request-failed"}); err != nil {
		t.Fatal(err)
	}
	absent("after a poll that never read the history to its end")

	if err := st.SetPlaybackPoll(context.Background(), store.PlaybackPoll{Source: "tautulli", Cursor: 1,
		CoveredThrough: 1_790_000_000, LastAttemptAt: 1_790_000_000}); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	for _, want := range []string{"holdfast_playback_suspect_files 1", "holdfast_playback_played_through_files 1",
		"holdfast_playback_history_read_timestamp_seconds 1.79e+09"} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not carry %q", want)
		}
	}
	if strings.Contains(body, "/lib/") {
		t.Error("a playback series names a file")
	}
	// Another source's state is not this daemon's.
	m.SetPlayback("other", playback.RulesFor(&config.Config{}))
	absent("a source that has not polled")
}

// failingPlayback is a store whose playback reads fail, one at a time.
type failingPlayback struct {
	store.Store
	pollFails bool
}

func (f failingPlayback) PlaybackPollOf(ctx context.Context, s string) (store.PlaybackPoll, bool, error) {
	if f.pollFails {
		return store.PlaybackPoll{}, false, errors.New("disk on fire")
	}
	return f.Store.PlaybackPollOf(ctx, s)
}

func (f failingPlayback) PlaybackEvidence(context.Context) ([]store.PlaybackSession, error) {
	return nil, errors.New("disk on fire")
}

// TestMetrics_AFailedPlaybackReadCostsOnlyItsOwnGauges.
func TestMetrics_AFailedPlaybackReadCostsOnlyItsOwnGauges(t *testing.T) {
	st := openStore(t)
	seedPlayback(t, st)
	if err := st.SetPlaybackPoll(context.Background(), store.PlaybackPoll{Source: "tautulli", CoveredThrough: 9}); err != nil {
		t.Fatal(err)
	}
	for _, pollFails := range []bool{true, false} {
		m := New(failingPlayback{Store: st, pollFails: pollFails}, nil)
		m.SetPlayback("tautulli", playback.RulesFor(&config.Config{}))
		body := scrape(t, m)
		if strings.Contains(body, "holdfast_playback_") {
			t.Errorf("poll read fails %v: a failed read published a gauge", pollFails)
		}
		if !strings.Contains(body, "holdfast_bytes_held_by_undo_window") {
			t.Errorf("poll read fails %v: a failed playback read cost another series", pollFails)
		}
	}
}

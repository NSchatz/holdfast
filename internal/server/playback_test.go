package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/playback"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestPlaybackEndpoint_ReportsASuspectedStallThroughTheReadAPI seeds the ledger with the issue's
// shape - a swapped film whose sessions on three clients end near 33:19-34:02, its original
// still retained, beside a film one session played through and the harness's own done row, which
// no session has played - and reads GET /api/playback behind the read token. The body is printed: it is
// the sample docs/api-reference.md carries. The paths are synthetic.
func TestPlaybackEndpoint_ReportsASuspectedStallThroughTheReadAPI(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	const film, played = "/media/movies/Film (2001)/Film (2001).mkv", "/media/movies/Other/Other.mkv"
	for _, p := range []string{film, played} {
		if _, err := st.Claim(ctx, p, "3200000000:1790000000", "w1", 3, store.InputsRead(nil)); err != nil {
			t.Fatal(err)
		}
		if err := st.Finish(ctx, p, "3200000000:1790000000", store.Done, &store.Outcome{}, 3); err != nil {
			t.Fatal(err)
		}
	}
	files, err := st.SwappedFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var swapped int64
	for _, f := range files {
		if f.Path == film {
			swapped = f.SwappedAt
		}
	}
	if err := st.Retain(ctx, store.Retained{SourcePath: film, SwappedPath: film,
		RetainedPath: "/media/movies/Film (2001)/.holdfast-undo/Film (2001).9656512701-1789000000.__undo__.mkv.holdfast-undo",
		SourceBytes:  9_656_512_701, SwappedFingerprint: "3200000000:1790000000", RetainedAt: swapped,
		ExpiresAt: swapped + 86400}); err != nil {
		t.Fatal(err)
	}
	dur := int64(8_700_000)
	sess := func(id string, started, stopped, off int64, platform, player, decision string) store.PlaybackSession {
		return store.PlaybackSession{Source: "tautulli", SessionID: id, Path: film, Fingerprint: "3200000000:1790000000",
			StartedAt: swapped + started, StoppedAt: &stopped, OffsetMS: &off, DurationMS: &dur, Platform: platform,
			Player: player, Decision: decision, RecordedAt: swapped + 4*86400}
	}
	full := dur
	if _, err := st.RecordPlaybackSessions(ctx, []store.PlaybackSession{
		sess("1101", 3*86400, swapped+3*86400+2400, 1_999_000, "Chrome", "Plex Web", "copy"),
		sess("1102", 3*86400+7200, swapped+3*86400+9800, 2_040_000, "Firefox", "Plex Web", "transcode"),
		sess("1103", 4*86400, swapped+4*86400+200, 2_042_000, "macOS", "Plex for Mac", "direct play"),
		{Source: "tautulli", SessionID: "1104", Path: played, Fingerprint: "3200000000:1790000000", StartedAt: swapped + 60,
			OffsetMS: &full, DurationMS: &dur, Platform: "tvOS", Player: "Apple TV", Decision: "direct play"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlaybackPoll(ctx, store.PlaybackPoll{Source: "tautulli", Cursor: swapped + 3*86400,
		CoveredThrough: swapped + 5*86400, LastAttemptAt: swapped + 5*86400}); err != nil {
		t.Fatal(err)
	}

	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	hub := NewHub(st, ctrl, discard())
	cfg := config.Config{PlaybackSource: "tautulli"}
	srv := New(ctx, cfg, secret.Value{}, secret.NewValue("read-secret"), st, ctrl, hub, nil, discard())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/playback")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/playback without the read token: %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/playback", nil)
	req.Header.Set("Authorization", "Bearer read-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/playback: %d %s", resp.StatusCode, body)
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, body, "", "  ")
	t.Logf("GET /api/playback:\n%s", pretty.String())

	var rep playback.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Enabled || rep.Source != "tautulli" || rep.PollMinutes != 15 || rep.Counts != (playback.ReportCounts{
		Swapped: 3, SuspectStall: 1, PlayedThrough: 1, Unplayed: 1}) || len(rep.Suspects) != 1 {
		t.Fatalf("report %+v", rep)
	}
	f := rep.Suspects[0]
	if f.Path != film || f.StallSessions != 3 || f.StallAtSeconds != 2040 || f.Restore == nil ||
		*f.Restore != "holdfast restore '"+film+"'" || len(f.StalledSessions) != 3 {
		t.Errorf("suspect %+v", f)
	}
}

// TestPlaybackEndpoint_ADaemonWithNoSourceSaysOff, and a ledger it cannot read is a 500 that
// names the endpoint and nothing of the error.
func TestPlaybackEndpoint_ADaemonWithNoSourceSaysOff(t *testing.T) {
	h := newHarness(t, "")
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/playback")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{`"enabled":false`, `"source":""`, `"suspects":[]`, `"covered_through":null`} {
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET /api/playback: %d %s, want 200 carrying %s", resp.StatusCode, body, want)
		}
	}

	ctx := context.Background()
	st := failingEvidence{newStore(t)}
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	srv := New(ctx, config.Config{}, secret.Value{}, secret.Value{}, st, ctrl, NewHub(st, ctrl, discard()), nil, discard())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/playback", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "playback") ||
		strings.Contains(rec.Body.String(), "disk on fire") {
		t.Errorf("an unreadable ledger: %d %q", rec.Code, rec.Body.String())
	}
}

type failingEvidence struct{ store.Store }

func (failingEvidence) PlaybackEvidence(context.Context) ([]store.PlaybackSession, error) {
	return nil, errors.New("disk on fire")
}

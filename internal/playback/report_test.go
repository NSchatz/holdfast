package playback

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

func on(path, fp string, s store.PlaybackSession) store.PlaybackSession {
	s.Path, s.Fingerprint = path, fp
	return s
}

// TestReport_CountsEveryCurrentSwapByVerdict: each path counts once, as its newest swap (the
// greater fingerprint on a tie); a file with no session is unplayed once a poll has covered its
// swap and unknown before; the suspects carry their stall and the sessions it rests on.
func TestReport_CountsEveryCurrentSwapByVerdict(t *testing.T) {
	stopped := int64(4200)
	s1, s2 := at("s1", 2_000_000, film), at("s2", 2_030_000, film)
	s1.StoppedAt, s2.OffsetDerived = &stopped, true
	led := &memLedger{
		swaps: []store.SwappedFile{
			{Path: "/lib/stall.mkv", Fingerprint: "new", SwappedAt: 200}, {Path: "/lib/stall.mkv", Fingerprint: "old", SwappedAt: 100},
			{Path: "/lib/played.mkv", Fingerprint: "1:1", SwappedAt: 300},
			{Path: "/lib/partial.mkv", Fingerprint: "1:1", SwappedAt: 300},
			{Path: "/lib/covered.mkv", Fingerprint: "1:1", SwappedAt: 400},
			{Path: "/lib/late.mkv", Fingerprint: "1:1", SwappedAt: 600},
			{Path: "/lib/tie.mkv", Fingerprint: "b", SwappedAt: 300}, {Path: "/lib/tie.mkv", Fingerprint: "a", SwappedAt: 300},
			{Path: "/lib/at-cover.mkv", Fingerprint: "1:1", SwappedAt: 500},
		},
		sessions: []store.PlaybackSession{
			on("/lib/stall.mkv", "new", s1), on("/lib/stall.mkv", "new", s2),
			on("/lib/played.mkv", "1:1", at("p", film, film)),
			on("/lib/partial.mkv", "1:1", at("q", 1000, film)),
			on("/lib/tie.mkv", "b", at("t", film, film)),
		},
		poll: &store.PlaybackPoll{Source: "tautulli", Cursor: 1, CoveredThrough: 500, LastAttemptAt: 700, LastFailure: ClassRequest},
		retained: map[string]store.Retained{
			"/lib/stall.mkv": {SourcePath: "/lib/stall.mkv", SwappedPath: "/lib/stall.mkv", SwappedFingerprint: "new"},
		},
	}
	cfg := &config.Config{PlaybackSource: " tautulli ", PlaybackPollMinutes: 30, PlaybackPlayedFraction: 0.8}
	rep, err := BuildReport(context.Background(), led, cfg, time.Unix(now0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Enabled || rep.Source != "tautulli" || rep.PollMinutes != 30 {
		t.Errorf("header %+v", rep)
	}
	if rep.Rules != (ReportRule{PlayedFraction: 0.8, EndMarginSeconds: 180, StallBefore: 0.85, StallToleranceSeconds: 90,
		StallMinSessions: 2, RetryWindowSeconds: 300, StallWindowDays: 60}) {
		t.Errorf("rules %+v", rep.Rules)
	}
	if rep.LastAttemptAt == nil || *rep.LastAttemptAt != 700 || rep.LastFailure == nil || *rep.LastFailure != ClassRequest ||
		rep.CoveredThrough == nil || *rep.CoveredThrough != 500 {
		t.Errorf("poll %v %v %v", rep.LastAttemptAt, rep.LastFailure, rep.CoveredThrough)
	}
	// stall, played, partial (unplayed), covered (unplayed, no session), late (unknown: swapped
	// after the newest covering poll began), at-cover (unknown: swapped the second it began),
	// tie (played: the greater fingerprint, whichever order the rows came in).
	want := ReportCounts{Swapped: 7, SuspectStall: 1, PlayedThrough: 2, Unplayed: 2, Unknown: 2}
	if rep.Counts != want {
		t.Errorf("counts %+v, want %+v", rep.Counts, want)
	}
	if len(rep.Suspects) != 1 {
		t.Fatalf("suspects %+v", rep.Suspects)
	}
	f := rep.Suspects[0]
	if f.Path != "/lib/stall.mkv" || f.Fingerprint != "new" || f.SwappedAt != 200 || f.Verdict != "suspect-stall" ||
		f.Sessions != 2 || f.PlayedThrough != 0 || f.DurationSeconds == nil || *f.DurationSeconds != film/1000 ||
		f.StallAtSeconds != 2000 || f.StallFromSeconds != 2000 || f.StallToSeconds != 2030 || f.StallSessions != 2 ||
		f.StallClients != 2 || f.StallRetries != 0 || f.Restore == nil || *f.Restore != "holdfast restore /lib/stall.mkv" {
		t.Errorf("suspect %+v", f)
	}
	if len(f.StalledSessions) != 2 {
		t.Fatalf("stalled sessions %+v", f.StalledSessions)
	}
	if g := f.StalledSessions[0]; g.StartedAt != now0-86400 || g.StoppedAt != &stopped || g.OffsetSeconds != 2000 ||
		g.OffsetFromPercent || g.PlayedSeconds == nil || *g.PlayedSeconds != 3600 || g.Platform != "Chrome" ||
		g.Player != "Plex Web" || g.Decision != "" {
		t.Errorf("first stalled session %+v", g)
	}
	if g := f.StalledSessions[1]; g.StoppedAt != nil || !g.OffsetFromPercent || g.OffsetSeconds != 2030 {
		t.Errorf("second stalled session %+v", g)
	}
}

// TestReport_OffAndEmptyIsStatedAsSuch: a daemon that polls nothing says so, every time it would
// carry is null, the lists are empty rather than null, and a duration nobody stated is null.
func TestReport_OffAndEmptyIsStatedAsSuch(t *testing.T) {
	rep, err := BuildReport(context.Background(), &memLedger{}, &config.Config{}, time.Unix(now0, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	for _, want := range []string{`"enabled":false`, `"source":""`, `"poll_minutes":0`, `"last_attempt_at":null`,
		`"last_failure":null`, `"covered_through":null`, `"suspects":[]`, `"played_fraction":0.9`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not carry %s", b, want)
		}
	}
	// A source that has polled once and never read to the end: the attempt is reported, the
	// coverage is not, and a file with no session is unknown.
	led := &memLedger{swaps: []store.SwappedFile{{Path: "/lib/a.mkv", Fingerprint: "1:1", SwappedAt: 1}},
		poll: &store.PlaybackPoll{Source: "tautulli", LastAttemptAt: 9}}
	rep, err = BuildReport(context.Background(), led, &config.Config{PlaybackSource: "tautulli"}, time.Unix(now0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.LastAttemptAt == nil || rep.LastFailure != nil || rep.CoveredThrough != nil || rep.Counts.Unknown != 1 ||
		rep.PollMinutes != 15 {
		t.Errorf("report %+v", rep)
	}
	// A suspect whose sessions stated no duration reports none.
	a, b2 := at("a", 1000, film), at("b", 2000, film)
	f := reportFile(Suspect{Path: "/x", Evidence: Derive([]store.PlaybackSession{a, b2}, true, rules, now0)}, 5)
	if f.Restore != nil || f.SwappedAt != 5 {
		t.Errorf("file %+v", f)
	}
	f.DurationSeconds = nil
	ev := Derive([]store.PlaybackSession{a, b2}, true, rules, now0)
	ev.DurationMS = 0
	if g := reportFile(Suspect{Path: "/x", Evidence: ev}, 0); g.DurationSeconds != nil {
		t.Errorf("no duration: %v", *g.DurationSeconds)
	}
}

// TestReport_TheRestoreCommandIsOfferedOnlyForThisSwapsLiveRetention: a restored record, a
// record of another swap of the path, and a record found by its source path for a container-
// changing swap's other name are not offered; a path that needs quoting is quoted.
func TestReport_TheRestoreCommandIsOfferedOnlyForThisSwapsLiveRetention(t *testing.T) {
	restored := int64(5)
	k := fileKey{"/lib/Film's Cut (2001).mkv", "9:9"}
	for _, tc := range []struct {
		name string
		rec  *store.Retained
		want string
	}{
		{"none", nil, ""},
		{"live, this swap", &store.Retained{SwappedPath: k.path, SwappedFingerprint: "9:9"},
			`holdfast restore '/lib/Film'\''s Cut (2001).mkv'`},
		{"restored", &store.Retained{SwappedPath: k.path, SwappedFingerprint: "9:9", RestoredAt: &restored}, ""},
		{"another swap's", &store.Retained{SwappedPath: k.path, SwappedFingerprint: "8:8"}, ""},
		{"another path", &store.Retained{SwappedPath: "/lib/other.mkv", SwappedFingerprint: "9:9"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			led := &memLedger{retained: map[string]store.Retained{}}
			if tc.rec != nil {
				led.retained[k.path] = *tc.rec
			}
			got, err := restoreCommand(context.Background(), led, k)
			if err != nil || got != tc.want {
				t.Errorf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for in, want := range map[string]string{
		"/lib/a-b_c.d/E+F,G:H@I.mkv": "/lib/a-b_c.d/E+F,G:H@I.mkv",
		"/lib/a b.mkv":               "'/lib/a b.mkv'",
		"/lib/$x.mkv":                "'/lib/$x.mkv'",
		"":                           "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReport_AnUnreadableLedgerIsAnError: every read the report makes fails it rather than
// reporting a count it could not take.
func TestReport_AnUnreadableLedgerIsAnError(t *testing.T) {
	boom := errors.New("locked")
	two := func() []store.PlaybackSession {
		return []store.PlaybackSession{on("/a", "1", at("1", 1000, film)), on("/a", "1", at("2", 1000, film))}
	}
	for name, led := range map[string]*memLedger{
		"poll":     {pollErr: boom},
		"files":    {swapsErr: boom},
		"evidence": {evErr: boom},
		"retained": {retErr: boom, swaps: []store.SwappedFile{{Path: "/a", Fingerprint: "1", SwappedAt: 1}}, sessions: two()},
	} {
		if _, err := BuildReport(context.Background(), led, &config.Config{PlaybackSource: "tautulli"}, time.Unix(now0, 0)); !errors.Is(err, boom) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Suspects(context.Background(), &memLedger{retErr: boom, sessions: two()}, rules, now0); !errors.Is(err, boom) {
		t.Errorf("Suspects: %v", err)
	}
}

// TestReport_SuspectsAreInPathOrder.
func TestReport_SuspectsAreInPathOrder(t *testing.T) {
	var ss []store.PlaybackSession
	for _, k := range []fileKey{{"/c", "1"}, {"/a", "2"}, {"/b", "1"}, {"/a", "1"}} {
		ss = append(ss, on(k.path, k.fingerprint, at("x", 1000, film)), on(k.path, k.fingerprint, at("y", 1000, film)))
	}
	sus, err := Suspects(context.Background(), &memLedger{sessions: ss}, rules, now0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sus {
		got = append(got, s.Path+"@"+s.Fingerprint)
	}
	if strings.Join(got, " ") != "/a@1 /a@2 /b@1 /c@1" {
		t.Errorf("order %v", got)
	}
}

// TestReport_TallyCountsEachFileOnce: two sessions of one stalled file are one suspect, a played
// file is one played, and an unplayed one is in neither.
func TestReport_TallyCountsEachFileOnce(t *testing.T) {
	ss := []store.PlaybackSession{
		on("/a", "1", at("1", 1000, film)), on("/a", "1", at("2", 2000, film)),
		on("/b", "1", at("3", film, film)), on("/b", "1", at("4", film, film)),
		on("/c", "1", at("5", film, film)),
		on("/d", "1", at("6", 1000, film)),
	}
	if s, p := Tally(ss, rules, now0); s != 1 || p != 2 {
		t.Errorf("tally %d suspect, %d played; want 1 and 2", s, p)
	}
	if s, p := Tally(nil, rules, now0); s != 0 || p != 0 {
		t.Errorf("tally of nothing %d %d", s, p)
	}
}

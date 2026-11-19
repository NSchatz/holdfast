package playback

import (
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

func ms(v int64) *int64 { return &v }

// film is a 2h25m file, the length of the issue's film, in milliseconds.
const film = int64(145 * 60 * 1000)

// now0 is the clock every derivation below is taken at; sessions start shortly before it.
const now0 = int64(1_790_000_000)

// at is one session that ended at off milliseconds into a file of dur milliseconds, on a client
// of its own named by id, having played for an hour.
func at(id string, off, dur int64) store.PlaybackSession {
	return store.PlaybackSession{Source: "tautulli", SessionID: id, Path: "/lib/film.mkv", Fingerprint: "1:1",
		StartedAt: now0 - 86400, OffsetMS: ms(off), DurationMS: ms(dur), PlayedSeconds: ms(3600), Client: "client-" + id,
		Platform: "Chrome", Player: "Plex Web"}
}

// onClient is s on the client named c.
func onClient(s store.PlaybackSession, c string) store.PlaybackSession {
	s.Client = c
	return s
}

// played is s having played for sec seconds.
func played(s store.PlaybackSession, sec int64) store.PlaybackSession {
	s.PlayedSeconds = ms(sec)
	return s
}

var rules = RulesFor(&config.Config{})

func derive(ss ...store.PlaybackSession) Evidence { return Derive(ss, true, rules, now0) }

// TestDerive_TheFixedFiguresAreTheDocumentedOnes pins the figures the design document states, and
// RulesFor's reading of the configuration.
func TestDerive_TheFixedFiguresAreTheDocumentedOnes(t *testing.T) {
	if EndMargin != 3*time.Minute || StallBefore != 0.85 || StallTolerance != 90*time.Second || StallMinSessions != 2 ||
		RetryWindow != 5*time.Minute || StallWindow != 60*24*time.Hour {
		t.Errorf("figures: end margin %v, stall before %g, tolerance %v, sessions %d, retry %v, window %v",
			EndMargin, StallBefore, StallTolerance, StallMinSessions, RetryWindow, StallWindow)
	}
	r := RulesFor(&config.Config{PlaybackPlayedFraction: 0.75})
	if r != (Rules{PlayedFraction: 0.75, EndMargin: EndMargin, StallBefore: StallBefore, StallTolerance: StallTolerance,
		StallMinSessions: 2, RetryWindow: RetryWindow, StallWindow: StallWindow}) {
		t.Errorf("RulesFor = %+v", r)
	}
	if rules.PlayedFraction != 0.9 {
		t.Error("an unwritten fraction is not the default")
	}
}

// TestDerive_PlayedThroughIsTheFractionOrTheEndMargin: a session at the fraction, or within the
// end margin of the end, played the file through; one just short of both did not.
func TestDerive_PlayedThroughIsTheFractionOrTheEndMargin(t *testing.T) {
	short := int64(20 * 60 * 1000) // a 20 minute episode: 0.9 is 2 minutes from the end
	for _, tc := range []struct {
		name     string
		off, dur int64
		want     Verdict
	}{
		{"exactly the fraction", film * 9 / 10, film, PlayedThrough},
		{"a millisecond short of the fraction", film*9/10 - 1, film, Unplayed},
		{"an episode's end margin, short of its fraction", short - EndMargin.Milliseconds(), short, PlayedThrough},
		{"a millisecond before an episode's end margin", short - EndMargin.Milliseconds() - 1, short, Unplayed},
		{"the very end", film, film, PlayedThrough},
		{"half way", film / 2, film, Unplayed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := derive(at("1", tc.off, tc.dur))
			if e.Verdict != tc.want {
				t.Errorf("verdict %s, want %s", e.Verdict, tc.want)
			}
			wantPlayed := 0
			if tc.want == PlayedThrough {
				wantPlayed = 1
			}
			if e.PlayedThrough != wantPlayed || e.Sessions != 1 {
				t.Errorf("played %d of %d, want %d of 1", e.PlayedThrough, e.Sessions, wantPlayed)
			}
		})
	}
	// The fraction is inclusive, at a fraction a float holds exactly, and the end margin decides
	// alone under a fraction of 1.
	half, strict := rules, rules
	half.PlayedFraction, strict.PlayedFraction = 0.5, 1
	for _, tc := range []struct {
		name string
		r    Rules
		off  int64
		want Verdict
	}{
		{"exactly half under 0.5", half, film / 2, PlayedThrough},
		{"a millisecond short of half under 0.5", half, film/2 - 1, Unplayed},
		{"at the end margin under 1", strict, film - EndMargin.Milliseconds(), PlayedThrough},
		{"just outside the end margin under 1", strict, film - EndMargin.Milliseconds() - 1, Unplayed},
	} {
		if e := Derive([]store.PlaybackSession{at("1", tc.off, film)}, true, tc.r, now0); e.Verdict != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, e.Verdict, tc.want)
		}
	}
}

// TestDerive_TheIssuesPatternIsASuspectedStall is #200: Chrome played to 33:19, resumed there and
// got nowhere; the desktop app resumed at 33:59 and froze at 34:02; Firefox stopped at 34:00. An
// Apple TV played it to the end. Several clients, and retries, ending at one point.
func TestDerive_TheIssuesPatternIsASuspectedStall(t *testing.T) {
	chrome := onClient(at("chrome-1", (33*60+19)*1000, film), "chrome")
	retry := played(onClient(at("chrome-2", (33*60+21)*1000, film), "chrome"), 120)
	desktop := played(at("desktop", (34*60+2)*1000, film), 180)
	firefox := at("firefox", (34*60+0)*1000, film)
	appletv := at("appletv", film, film)
	e := derive(chrome, retry, desktop, firefox, appletv)
	if e.Verdict != SuspectStall || e.Stall == nil {
		t.Fatalf("verdict %s, stall %+v", e.Verdict, e.Stall)
	}
	st := e.Stall
	if st.Sessions != 4 || st.Clients != 3 || st.Retries != 2 || st.FromMS != (33*60+19)*1000 ||
		st.ToMS != (34*60+2)*1000 || st.AtMS != (33*60+21)*1000 || len(st.Members) != 4 {
		t.Errorf("stall %+v", st)
	}
	if e.Sessions != 5 || e.PlayedThrough != 1 || e.DurationMS != film {
		t.Errorf("evidence %+v", e)
	}
}

// TestDerive_AStopThatLooksLikeAChoiceIsNotASuspect: viewers leaving at the credits, one viewer
// stopping twice at one scene, and stops long ago are not a stall.
func TestDerive_AStopThatLooksLikeAChoiceIsNotASuspect(t *testing.T) {
	animated := int64(90 * 60 * 1000) // credits start at 0.87, eleven minutes from the end
	credits := []store.PlaybackSession{at("a", animated*87/100, animated), at("b", animated*87/100+20_000, animated),
		at("c", animated*87/100+40_000, animated)}
	if e := derive(credits...); e.Verdict != Unplayed || e.Stall != nil {
		t.Errorf("three clients at the credits: %s %+v", e.Verdict, e.Stall)
	}
	// With them, a film played through is played through.
	if e := derive(append(credits, at("d", animated, animated))...); e.Verdict != PlayedThrough {
		t.Errorf("credits beside a play-through: %s", e.Verdict)
	}

	// One client, two long sessions ending at one point, and no retry: a choice made twice.
	twice := []store.PlaybackSession{onClient(at("1", 2_000_000, film), "tv"), onClient(at("2", 2_030_000, film), "tv")}
	if e := derive(twice...); e.Verdict != Unplayed {
		t.Errorf("one client stopping twice: %s %+v", e.Verdict, e.Stall)
	}
	// The same, where the second got nowhere after a resume: the file's.
	twice[1] = played(twice[1], 60)
	if e := derive(twice...); e.Verdict != SuspectStall || e.Stall.Clients != 1 || e.Stall.Retries != 1 {
		t.Errorf("one client and a retry: %s %+v", e.Verdict, e.Stall)
	}

	// Sessions from before the stall window are no part of a stall, at its boundary inclusively.
	old := []store.PlaybackSession{at("x", 2_000_000, film), at("y", 2_030_000, film)}
	cut := now0 - int64(StallWindow.Seconds())
	old[0].StartedAt, old[1].StartedAt = cut, cut
	if e := derive(old...); e.Verdict != SuspectStall {
		t.Errorf("at the window's edge: %s", e.Verdict)
	}
	old[1].StartedAt = cut - 1
	if e := derive(old...); e.Verdict != Unplayed || e.Sessions != 2 {
		t.Errorf("one session a second outside the window: %s", e.Verdict)
	}
}

// TestDerive_StallBeforeIsExclusive: a session at exactly StallBefore of a file is no part of a
// stall, one a millisecond before it is.
func TestDerive_StallBeforeIsExclusive(t *testing.T) {
	long := int64(100_000_000) // long enough that neither the fraction nor the margin is near
	at85 := long * 85 / 100
	if e := derive(at("a", at85, long), at("b", at85, long)); e.Verdict != Unplayed {
		t.Errorf("at the cut: %s", e.Verdict)
	}
	if e := derive(at("a", at85-1, long), at("b", at85-1, long)); e.Verdict != SuspectStall {
		t.Errorf("a millisecond before the cut: %s", e.Verdict)
	}
}

// TestDerive_TheToleranceIsInclusiveAndTheClusterTheLargest: ends exactly 90 s apart are one
// point and 90.001 s apart are two; the largest cluster wins, the earliest on a tie, and the
// stall is placed at its middle session.
func TestDerive_TheToleranceIsInclusiveAndTheClusterTheLargest(t *testing.T) {
	tol := StallTolerance.Milliseconds()
	if e := derive(at("1", 600_000, film), at("2", 600_000+tol, film)); e.Verdict != SuspectStall {
		t.Errorf("ends exactly the tolerance apart: %s", e.Verdict)
	}
	if e := derive(at("1", 600_000, film), at("2", 600_000+tol+1, film)); e.Verdict != Unplayed {
		t.Errorf("ends a millisecond more than the tolerance apart: %s", e.Verdict)
	}
	// Two at 10:00, three around 40:00: the three win, wherever they sit in the input.
	e := derive(at("late-c", 2_460_000, film), at("early-a", 600_000, film), at("late-a", 2_400_000, film),
		at("early-b", 610_000, film), at("late-b", 2_430_000, film))
	if e.Verdict != SuspectStall || e.Stall.Sessions != 3 || e.Stall.FromMS != 2_400_000 ||
		e.Stall.ToMS != 2_460_000 || e.Stall.AtMS != 2_430_000 {
		t.Fatalf("largest cluster: %s %+v", e.Verdict, e.Stall)
	}
	// A tie goes to the earliest cluster, and an even cluster's middle is its lower one.
	e = derive(at("b1", 3_000_000, film), at("b2", 3_010_000, film), at("a1", 1_000_000, film), at("a2", 1_050_000, film))
	if e.Stall == nil || e.Stall.FromMS != 1_000_000 || e.Stall.AtMS != 1_000_000 || e.Stall.ToMS != 1_050_000 {
		t.Fatalf("tie: %+v", e.Stall)
	}
	// Sessions ending at the same millisecond keep the order they were given in.
	e = derive(at("first", 500_000, film), at("second", 500_000, film))
	if e.Stall == nil || e.Stall.Members[0].SessionID != "first" || e.Stall.Members[1].SessionID != "second" {
		t.Fatalf("equal ends: %+v", e.Stall)
	}
	// A window anchored on each session in turn: 0, 80 and 160 s are two pairs and no triple.
	e = derive(at("x", 0, film), at("y", 80_000, film), at("z", 160_000, film))
	if e.Stall == nil || e.Stall.Sessions != 2 || e.Stall.FromMS != 0 || e.Stall.ToMS != 80_000 {
		t.Fatalf("a chain is not one cluster: %+v", e.Stall)
	}
	// The largest cluster that qualifies wins over a larger one that does not: three stops by
	// one client with no retry, and two on two clients.
	e = derive(onClient(at("t1", 1_000_000, film), "tv"), onClient(at("t2", 1_010_000, film), "tv"),
		onClient(at("t3", 1_020_000, film), "tv"), at("p", 3_000_000, film), at("q", 3_010_000, film))
	if e.Stall == nil || e.Stall.FromMS != 3_000_000 || e.Stall.Sessions != 2 {
		t.Fatalf("the qualifying cluster: %+v", e.Stall)
	}
}

// TestDerive_APercentDerivedOffsetWidensTheTolerance: on a 160-minute film one percent is 96 s,
// so a real stall read through whole percents can land a percent apart; two such ends 108 s
// apart are one point, where two read ends 108 s apart are two.
func TestDerive_APercentDerivedOffsetWidensTheTolerance(t *testing.T) {
	long := int64(160 * 60 * 1000)
	a, b := at("a", 2_016_000, long), at("b", 2_124_000, long)
	if e := derive(a, b); e.Verdict != Unplayed {
		t.Errorf("read offsets 108 s apart: %s", e.Verdict)
	}
	b.OffsetDerived = true
	if e := derive(a, b); e.Verdict != SuspectStall {
		t.Fatalf("a derived offset 108 s away: %s", e.Verdict)
	}
	// Widened by one percent of the file and no more.
	c := at("c", 2_016_000+StallTolerance.Milliseconds()+long/100+1, long)
	c.OffsetDerived = true
	if e := derive(a, c); e.Verdict != Unplayed {
		t.Errorf("a millisecond past the widened tolerance: %s", e.Verdict)
	}
	c.OffsetMS = ms(2_016_000 + StallTolerance.Milliseconds() + long/100)
	if e := derive(a, c); e.Verdict != SuspectStall {
		t.Errorf("at the widened tolerance: %s", e.Verdict)
	}
}

// TestDerive_ClientsAndRetriesAreCountedAsTheyAre: a client is its identifier, or its platform and
// player where it has none; a retry played for no longer than the retry window, inclusively.
func TestDerive_ClientsAndRetriesAreCountedAsTheyAre(t *testing.T) {
	a, b := at("a", 1000, film), at("b", 2000, film)
	a.Client, b.Client = "", ""
	if got := rules.describe([]store.PlaybackSession{a, b}); got.Clients != 1 {
		t.Errorf("two unnamed sessions on one platform and player: %d clients", got.Clients)
	}
	b.Player = "Plex for Mac"
	if got := rules.describe([]store.PlaybackSession{a, b}); got.Clients != 2 {
		t.Errorf("two players: %d clients", got.Clients)
	}
	a.Client, b.Client = "same", "same"
	if got := rules.describe([]store.PlaybackSession{a, b}); got.Clients != 1 {
		t.Errorf("one identifier on two players: %d clients", got.Clients)
	}
	if clientOf(at("x", 1, 1)) != "id:client-x" || clientOf(store.PlaybackSession{Platform: "P", Player: "Q"}) != "name:P/Q" {
		t.Errorf("client names %q %q", clientOf(at("x", 1, 1)), clientOf(store.PlaybackSession{Platform: "P", Player: "Q"}))
	}
	for sec, want := range map[int64]bool{0: true, 300: true, 301: false} {
		if got := rules.isRetry(played(at("r", 1, 1), sec)); got != want {
			t.Errorf("played %d s: retry %v, want %v", sec, got, want)
		}
	}
	none := at("n", 1, 1)
	none.PlayedSeconds = nil
	if rules.isRetry(none) {
		t.Error("a session with no play time is a retry")
	}
}

// TestDerive_AStallWinsOverAPlayThrough: one client that plays the file is not evidence that the
// others can.
func TestDerive_AStallWinsOverAPlayThrough(t *testing.T) {
	e := derive(at("appletv-1", film, film), at("appletv-2", film-1000, film), at("chrome", 2_000_000, film),
		at("desktop", 2_042_000, film))
	if e.Verdict != SuspectStall || e.PlayedThrough != 2 || e.Sessions != 4 {
		t.Errorf("verdict %s, played %d of %d", e.Verdict, e.PlayedThrough, e.Sessions)
	}
	if e := derive(at("1", film, film), at("2", film-1000, film)); e.Verdict != PlayedThrough || e.Stall != nil {
		t.Errorf("two play-throughs: %s %+v", e.Verdict, e.Stall)
	}
}

// TestDerive_ASessionThatSaysNothingIsCountedAndDecidesNothing: no offset, no duration or a
// non-positive duration cannot play a file through or place a stall; one session is never a
// stall; and with no session the verdict is unknown unless a poll has covered the swap.
func TestDerive_ASessionThatSaysNothingIsCountedAndDecidesNothing(t *testing.T) {
	noOffset, noDuration, zero, negative := at("a", 1000, film), at("b", 1000, film), at("c", 1000, 0), at("d", 1000, -5)
	noOffset.OffsetMS, noDuration.DurationMS = nil, nil
	e := Derive([]store.PlaybackSession{noOffset, noDuration, zero, negative}, false, rules, now0)
	if e.Verdict != Unplayed || e.Sessions != 4 || e.PlayedThrough != 0 || e.Stall != nil {
		t.Errorf("sessions that say nothing: %+v", e)
	}
	if e := derive(at("1", 1000, film)); e.Verdict != Unplayed {
		t.Errorf("one early stop: %s", e.Verdict)
	}
	if e := Derive(nil, false, rules, now0); e.Verdict != Unknown || e.Sessions != 0 {
		t.Errorf("nothing, uncovered: %+v", e)
	}
	if e := Derive(nil, true, rules, now0); e.Verdict != Unplayed {
		t.Errorf("nothing, covered: %s", e.Verdict)
	}
	// A hand-built rule set asking for fewer than two sessions still needs one to cluster.
	loose := rules
	loose.StallMinSessions = 0
	if e := Derive(nil, true, loose, now0); e.Verdict != Unplayed || e.Stall != nil {
		t.Errorf("no sessions under a zero minimum: %+v", e)
	}
	if e := Derive([]store.PlaybackSession{played(at("1", 1000, film), 10)}, true, loose, now0); e.Verdict != SuspectStall ||
		e.Stall.Sessions != 1 {
		t.Errorf("one retry under a zero minimum: %+v", e)
	}
	three := rules
	three.StallMinSessions = 3
	if e := Derive([]store.PlaybackSession{at("1", 1000, film), at("2", 2000, film)}, true, three, now0); e.Verdict != Unplayed {
		t.Errorf("two sessions under a minimum of three: %s", e.Verdict)
	}
}

// TestDerive_TheLongestDurationAndTheNewestStart are reported.
func TestDerive_TheLongestDurationAndTheNewestStart(t *testing.T) {
	a, b, c := at("a", 1000, film), at("b", 1000, film+5000), at("c", 1000, film)
	a.StartedAt, b.StartedAt, c.StartedAt = now0-300, now0-100, now0-200
	c.DurationMS = nil
	e := derive(a, b, c)
	if e.DurationMS != film+5000 || e.LastStartedAt != now0-100 {
		t.Errorf("duration %d, last start %d", e.DurationMS, e.LastStartedAt)
	}
}

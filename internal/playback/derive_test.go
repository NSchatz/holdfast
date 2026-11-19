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

// at is one session that ended at off milliseconds into a file of dur milliseconds.
func at(id string, off, dur int64) store.PlaybackSession {
	return store.PlaybackSession{Source: "tautulli", SessionID: id, Path: "/lib/film.mkv", Fingerprint: "1:1",
		StartedAt: 1000, OffsetMS: ms(off), DurationMS: ms(dur), Platform: "Chrome", Player: "Plex Web"}
}

var rules = Rules{PlayedFraction: 0.9, EndMargin: EndMargin, StallTolerance: StallTolerance, StallMinSessions: StallMinSessions}

// TestDerive_TheFixedFiguresAreTheDocumentedOnes pins the three figures the design document
// states, and RulesFor's reading of the configuration.
func TestDerive_TheFixedFiguresAreTheDocumentedOnes(t *testing.T) {
	if EndMargin != 3*time.Minute || StallTolerance != 90*time.Second || StallMinSessions != 2 {
		t.Errorf("figures: end margin %v, stall tolerance %v, stall sessions %d", EndMargin, StallTolerance, StallMinSessions)
	}
	r := RulesFor(&config.Config{PlaybackPlayedFraction: 0.75})
	if r != (Rules{PlayedFraction: 0.75, EndMargin: EndMargin, StallTolerance: StallTolerance, StallMinSessions: 2}) {
		t.Errorf("RulesFor = %+v", r)
	}
	if RulesFor(&config.Config{}).PlayedFraction != 0.9 {
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
		{"exactly the end margin from the end", film - EndMargin.Milliseconds(), film, PlayedThrough},
		{"a millisecond outside the end margin, below the fraction", film - EndMargin.Milliseconds() - 1, 1_000_000_000, Unplayed},
		{"the very end", film, film, PlayedThrough},
		{"half way", film / 2, film, Unplayed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Derive([]store.PlaybackSession{at("1", tc.off, tc.dur)}, true, rules)
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
	// The fraction is inclusive, at a fraction a float holds exactly.
	half := rules
	half.PlayedFraction = 0.5
	if e := Derive([]store.PlaybackSession{at("1", film/2, film)}, true, half); e.Verdict != PlayedThrough {
		t.Errorf("exactly half under a fraction of 0.5: %s", e.Verdict)
	}
	if e := Derive([]store.PlaybackSession{at("1", film/2-1, film)}, true, half); e.Verdict != Unplayed {
		t.Errorf("a millisecond short of half under a fraction of 0.5: %s", e.Verdict)
	}
	// The margin is what decides the long file: 2h25m less 3 minutes is past 0.9 too, so a
	// fraction of 1 isolates it.
	strict := rules
	strict.PlayedFraction = 1
	if e := Derive([]store.PlaybackSession{at("1", film-EndMargin.Milliseconds(), film)}, true, strict); e.Verdict != PlayedThrough {
		t.Errorf("at the end margin under a fraction of 1: %s", e.Verdict)
	}
	if e := Derive([]store.PlaybackSession{at("1", film-EndMargin.Milliseconds()-1, film)}, true, strict); e.Verdict != Unplayed {
		t.Errorf("just outside the end margin under a fraction of 1: %s", e.Verdict)
	}
}

// TestDerive_TwoSessionsEndingAtOnePointWellBeforeTheEndAreASuspectedStall is the issue's case:
// sessions on different clients ending near 33:19 and 34:00 of a 2h25m film.
func TestDerive_TwoSessionsEndingAtOnePointWellBeforeTheEndAreASuspectedStall(t *testing.T) {
	a := at("chrome", (33*60+19)*1000, film)
	b := at("firefox", (34*60+0)*1000, film)
	b.Platform, b.Player = "Firefox", "Plex Web"
	e := Derive([]store.PlaybackSession{a, b}, true, rules)
	if e.Verdict != SuspectStall || e.Stall == nil {
		t.Fatalf("verdict %s, stall %v", e.Verdict, e.Stall)
	}
	if e.Stall.Sessions != 2 || e.Stall.FromMS != *a.OffsetMS || e.Stall.ToMS != *b.OffsetMS || e.Stall.AtMS != *a.OffsetMS {
		t.Errorf("stall %+v", e.Stall)
	}
	if len(e.Stall.Members) != 2 || e.Stall.Members[0].SessionID != "chrome" || e.Stall.Members[1].SessionID != "firefox" {
		t.Errorf("members %+v", e.Stall.Members)
	}
	if e.Sessions != 2 || e.PlayedThrough != 0 || e.DurationMS != film {
		t.Errorf("evidence %+v", e)
	}
}

// TestDerive_TheToleranceIsInclusiveAndTheClusterTheLargest: ends exactly 90 s apart are one
// point and 90.001 s apart are two; the largest cluster wins, the earliest on a tie, and the
// stall is placed at its middle session.
func TestDerive_TheToleranceIsInclusiveAndTheClusterTheLargest(t *testing.T) {
	tol := StallTolerance.Milliseconds()
	if e := Derive([]store.PlaybackSession{at("1", 600_000, film), at("2", 600_000+tol, film)}, true, rules); e.Verdict != SuspectStall {
		t.Errorf("ends exactly the tolerance apart: %s", e.Verdict)
	}
	if e := Derive([]store.PlaybackSession{at("1", 600_000, film), at("2", 600_000+tol+1, film)}, true, rules); e.Verdict != Unplayed {
		t.Errorf("ends a millisecond more than the tolerance apart: %s", e.Verdict)
	}

	// Two at 10:00, three around 40:00: the three win, wherever they sit in the input.
	ss := []store.PlaybackSession{
		at("late-c", 2_460_000, film), at("early-a", 600_000, film), at("late-a", 2_400_000, film),
		at("early-b", 610_000, film), at("late-b", 2_430_000, film),
	}
	e := Derive(ss, true, rules)
	if e.Verdict != SuspectStall || e.Stall.Sessions != 3 || e.Stall.FromMS != 2_400_000 ||
		e.Stall.ToMS != 2_460_000 || e.Stall.AtMS != 2_430_000 {
		t.Fatalf("largest cluster: %s %+v", e.Verdict, e.Stall)
	}
	// A tie goes to the earliest cluster, and an even cluster's middle is its lower one.
	ss = []store.PlaybackSession{
		at("b1", 3_000_000, film), at("b2", 3_010_000, film),
		at("a1", 1_000_000, film), at("a2", 1_050_000, film),
	}
	e = Derive(ss, true, rules)
	if e.Stall == nil || e.Stall.FromMS != 1_000_000 || e.Stall.AtMS != 1_000_000 || e.Stall.ToMS != 1_050_000 {
		t.Fatalf("tie: %+v", e.Stall)
	}
	// A window anchored on each session in turn: 0, 80 and 160 s are two pairs and no triple.
	ss = []store.PlaybackSession{at("x", 0, film), at("y", 80_000, film), at("z", 160_000, film)}
	e = Derive(ss, true, rules)
	if e.Stall == nil || e.Stall.Sessions != 2 || e.Stall.FromMS != 0 || e.Stall.ToMS != 80_000 {
		t.Fatalf("a chain is not one cluster: %+v", e.Stall)
	}
}

// TestDerive_AStallWinsOverAPlayThrough: one client that plays the file is not evidence that the
// others can (the issue's Apple TV played the film to the end twice).
func TestDerive_AStallWinsOverAPlayThrough(t *testing.T) {
	ss := []store.PlaybackSession{at("appletv-1", film, film), at("appletv-2", film-1000, film),
		at("chrome", 2_000_000, film), at("desktop", 2_042_000, film)}
	e := Derive(ss, true, rules)
	if e.Verdict != SuspectStall || e.PlayedThrough != 2 || e.Sessions != 4 {
		t.Errorf("verdict %s, played %d of %d", e.Verdict, e.PlayedThrough, e.Sessions)
	}
	// Sessions that reached the end never join a cluster, however close they are.
	e = Derive([]store.PlaybackSession{at("1", film, film), at("2", film-1000, film)}, true, rules)
	if e.Verdict != PlayedThrough || e.Stall != nil {
		t.Errorf("two play-throughs: %s %+v", e.Verdict, e.Stall)
	}
}

// TestDerive_ASessionThatSaysNothingIsCountedAndDecidesNothing: no offset, no duration or a
// non-positive duration cannot play a file through or place a stall; one session is never a
// stall; and with no session the verdict is unknown unless a poll has covered the swap.
func TestDerive_ASessionThatSaysNothingIsCountedAndDecidesNothing(t *testing.T) {
	noOffset, noDuration, zero, negative := at("a", 1000, film), at("b", 1000, film), at("c", 1000, 0), at("d", 1000, -5)
	noOffset.OffsetMS, noDuration.DurationMS = nil, nil
	e := Derive([]store.PlaybackSession{noOffset, noDuration, zero, negative}, false, rules)
	if e.Verdict != Unplayed || e.Sessions != 4 || e.PlayedThrough != 0 || e.Stall != nil {
		t.Errorf("sessions that say nothing: %+v", e)
	}
	if e := Derive([]store.PlaybackSession{at("1", 1000, film)}, true, rules); e.Verdict != Unplayed {
		t.Errorf("one early stop: %s", e.Verdict)
	}
	if e := Derive(nil, false, rules); e.Verdict != Unknown || e.Sessions != 0 {
		t.Errorf("nothing, uncovered: %+v", e)
	}
	if e := Derive(nil, true, rules); e.Verdict != Unplayed {
		t.Errorf("nothing, covered: %s", e.Verdict)
	}
	// A hand-built rule set asking for fewer than two sessions still needs one to cluster.
	loose := rules
	loose.StallMinSessions = 0
	if e := Derive(nil, true, loose); e.Verdict != Unplayed || e.Stall != nil {
		t.Errorf("no sessions under a zero minimum: %+v", e)
	}
	if e := Derive([]store.PlaybackSession{at("1", 1000, film)}, true, loose); e.Verdict != SuspectStall || e.Stall.Sessions != 1 {
		t.Errorf("one session under a zero minimum: %+v", e)
	}
	three := rules
	three.StallMinSessions = 3
	if e := Derive([]store.PlaybackSession{at("1", 1000, film), at("2", 2000, film)}, true, three); e.Verdict != Unplayed {
		t.Errorf("two sessions under a minimum of three: %s", e.Verdict)
	}
}

// TestDerive_TheLongestDurationAndTheNewestStart are reported.
func TestDerive_TheLongestDurationAndTheNewestStart(t *testing.T) {
	a, b, c := at("a", 1000, film), at("b", 1000, film+5000), at("c", 1000, film)
	a.StartedAt, b.StartedAt, c.StartedAt = 300, 100, 200
	c.DurationMS = nil
	e := Derive([]store.PlaybackSession{a, b, c}, true, rules)
	if e.DurationMS != film+5000 || e.LastStartedAt != 300 {
		t.Errorf("duration %d, last start %d", e.DurationMS, e.LastStartedAt)
	}
}

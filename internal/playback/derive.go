// Package playback ties the files holdfast swapped to what later happened when people played
// them (docs/design/playback-evidence.md#playback-evidence).
//
// Every gate in front of a swap measures whether the replacement is FAITHFUL to its source.
// None of them measures whether real players can play it, and a fault there can take days to
// reach a viewer and weeks to reach the operator. This package reads a media server's session
// history, attributes each session to the swapped file whose bytes it played, and derives one
// verdict per file: played through, a suspected stall (several sessions ending at the same
// point well before the end), not yet played through, or unknown.
//
// It REPORTS and nothing else. It holds no handle that can write a library file, nothing in the
// encode pipeline reads what it records, and a suspected stall is never acted on: it is said in
// the log, on GET /api/playback and in /metrics, with the `holdfast restore` command that would
// walk the swap back while the original is still retained. An evidence source that cannot be
// read leaves every file with the verdict it had; no evidence is "unknown", never "played".
package playback

import (
	"cmp"
	"slices"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// Verdict is what the evidence says about one swapped file. The vocabulary is closed.
type Verdict string

const (
	// Unknown: no evidence source has read the history since the swap, so nothing is known.
	Unknown Verdict = "unknown"
	// Unplayed: the history has been read since the swap and no session has played the file
	// through. Sessions that stopped part way are here too, unless they form a stall.
	Unplayed Verdict = "unplayed"
	// PlayedThrough: at least one session reached the played-through mark, and the sessions
	// that did not form no stall.
	PlayedThrough Verdict = "played-through"
	// SuspectStall: recent sessions stopped at one point well before the end, in a way that
	// looks like the file and not the viewer (see Rules.stall). It wins over a play-through:
	// one client that plays the file is not evidence that the others can.
	SuspectStall Verdict = "suspect-stall"
)

// The derivation's fixed figures. Only the played-through fraction is a configuration key
// (playback_played_fraction); these are stated in the design document and asserted by the
// suite.
const (
	// EndMargin: a session that ended within this of the end played the file through, whatever
	// the fraction, so a long film's credits do not hold it below the mark.
	EndMargin = 3 * time.Minute
	// StallBefore: only a session that stopped before this fraction of the file can be part of
	// a stall. Viewers stop at the credits, and an animated feature's credits can start well
	// before the played-through mark; a cluster there is people leaving, not a file stalling.
	StallBefore = 0.85
	// StallTolerance: sessions whose ends lie within this of one another ended at the same point.
	// Where an offset was computed from a whole percent, each such offset widens it by half a
	// percent of the file, the most the rounding can have moved it.
	StallTolerance = 90 * time.Second
	// StallMinSessions: how many sessions must end at the same point for a stall to be suspected.
	StallMinSessions = 2
	// RetryWindow: a session that played for no longer than this and ended at the point began
	// within this of it - a resume or a retry that got nowhere, which is what a stall looks like
	// from one client.
	RetryWindow = 5 * time.Minute
	// StallWindow: only sessions that started within this of now can form a stall. A stop
	// months ago is not evidence about how the file plays now, and sessions that never expire
	// collide at one point by chance.
	StallWindow = 60 * 24 * time.Hour
)

// Rules is what the derivation is decided under.
type Rules struct {
	// PlayedFraction is the fraction of the file a session must reach to play it through.
	PlayedFraction float64
	// The fixed figures above.
	EndMargin        time.Duration
	StallBefore      float64
	StallTolerance   time.Duration
	StallMinSessions int
	RetryWindow      time.Duration
	StallWindow      time.Duration
}

// RulesFor is the rules a configuration decides under.
func RulesFor(cfg *config.Config) Rules {
	return Rules{
		PlayedFraction:   cfg.EffectivePlaybackPlayedFraction(),
		EndMargin:        EndMargin,
		StallBefore:      StallBefore,
		StallTolerance:   StallTolerance,
		StallMinSessions: StallMinSessions,
		RetryWindow:      RetryWindow,
		StallWindow:      StallWindow,
	}
}

// Stall is the cluster of session ends a suspected stall rests on. Offsets are milliseconds
// into the file.
type Stall struct {
	// AtMS is the middle session's end: where the file stalls.
	AtMS int64
	// FromMS and ToMS are the first and last session ends in the cluster.
	FromMS, ToMS int64
	// Sessions is how many sessions ended in it, Clients on how many distinct clients, Retries
	// how many of them were a resume or retry that got nowhere, and Members are they.
	Sessions int
	Clients  int
	Retries  int
	Members  []store.PlaybackSession
}

// Evidence is the derivation for one swapped file.
type Evidence struct {
	Verdict Verdict
	// Sessions is every session attributed to the file; PlayedThrough how many reached the mark.
	Sessions      int
	PlayedThrough int
	// DurationMS is the longest duration any session stated for the file, 0 when none did.
	DurationMS int64
	// LastStartedAt is the newest session's start, 0 when there is none.
	LastStartedAt int64
	// Stall is the cluster, set only for SuspectStall.
	Stall *Stall
}

// sessionReading is what one session says on its own.
type sessionReading int

const (
	readingNone         sessionReading = iota // no offset, no duration, near the end but short of the mark, or too old
	readingPlayed                             // it reached the mark
	readingStoppedEarly                       // it stopped well before the end, recently
)

// read classifies one session. A session with no offset, or no positive duration, says nothing
// either way: it cannot have played the file through and it cannot be placed in a cluster. One
// that stopped past StallBefore without reaching the mark, or started before the stall window,
// played nothing through and is no part of a stall.
func (r Rules) read(s store.PlaybackSession, now int64) sessionReading {
	if s.OffsetMS == nil || s.DurationMS == nil || *s.DurationMS <= 0 {
		return readingNone
	}
	off, dur := *s.OffsetMS, *s.DurationMS
	switch {
	case float64(off) >= r.PlayedFraction*float64(dur) || dur-off <= r.EndMargin.Milliseconds():
		return readingPlayed
	case float64(off) >= r.StallBefore*float64(dur) || s.StartedAt < now-int64(r.StallWindow.Seconds()):
		return readingNone
	}
	return readingStoppedEarly
}

// Derive is the verdict the given sessions - all of them attributed to one swapped file - support,
// at unix second now. covered says whether a poll has read the history to its end since the file
// was swapped: with no session and no such poll, nothing is known.
func Derive(sessions []store.PlaybackSession, covered bool, r Rules, now int64) Evidence {
	ev := Evidence{Sessions: len(sessions)}
	var early []store.PlaybackSession
	for _, s := range sessions {
		ev.LastStartedAt = max(ev.LastStartedAt, s.StartedAt)
		if s.DurationMS != nil {
			ev.DurationMS = max(ev.DurationMS, *s.DurationMS)
		}
		switch r.read(s, now) {
		case readingPlayed:
			ev.PlayedThrough++
		case readingStoppedEarly:
			early = append(early, s)
		}
	}
	if st := r.stall(early); st != nil {
		ev.Verdict, ev.Stall = SuspectStall, st
		return ev
	}
	switch {
	case ev.PlayedThrough > 0:
		ev.Verdict = PlayedThrough
	case ev.Sessions > 0 || covered:
		ev.Verdict = Unplayed
	default:
		ev.Verdict = Unknown
	}
	return ev
}

// clientOf names the client a session played on: the source's own identifier, or its platform
// and player where it gave none.
func clientOf(s store.PlaybackSession) string {
	if s.Client != "" {
		return "id:" + s.Client
	}
	return "name:" + s.Platform + "/" + s.Player
}

// isRetry reports whether a session played for no longer than the retry window: it began
// within that much of the file before where it ended, since a player cannot move through a
// file faster than it plays it.
func (r Rules) isRetry(s store.PlaybackSession) bool {
	return s.PlayedSeconds != nil && *s.PlayedSeconds <= int64(r.RetryWindow.Seconds())
}

// stall finds the largest set of sessions whose ends lie within the tolerance of one another
// AND that look like the file stalling rather than viewers choosing to stop - sessions on at
// least two distinct clients, or at least one resume or retry that got nowhere - the earliest
// such set on a tie, and returns it when it holds at least StallMinSessions.
func (r Rules) stall(early []store.PlaybackSession) *Stall {
	if len(early) < r.StallMinSessions || len(early) == 0 {
		return nil
	}
	sorted := append([]store.PlaybackSession(nil), early...)
	slices.SortStableFunc(sorted, func(a, b store.PlaybackSession) int { return cmp.Compare(*a.OffsetMS, *b.OffsetMS) })
	tol := r.StallTolerance.Milliseconds()
	for _, s := range sorted {
		if s.OffsetDerived {
			// Each of two percent-derived ends can sit half a percent from where it was.
			tol += *s.DurationMS / 100
			break
		}
	}
	var best *Stall
	for i := range sorted {
		j := i
		for j+1 < len(sorted) && *sorted[j+1].OffsetMS-*sorted[i].OffsetMS <= tol {
			j++
		}
		members := sorted[i : j+1]
		st := r.describe(members)
		if st.Sessions < r.StallMinSessions || (st.Clients < 2 && st.Retries == 0) {
			continue
		}
		if best == nil || st.Sessions > best.Sessions {
			best = st
		}
	}
	return best
}

// describe is one candidate cluster: its place, its size, its clients and its retries.
func (r Rules) describe(members []store.PlaybackSession) *Stall {
	n := len(members)
	clients := map[string]bool{}
	retries := 0
	for _, m := range members {
		clients[clientOf(m)] = true
		if r.isRetry(m) {
			retries++
		}
	}
	return &Stall{
		AtMS:     *members[(n-1)/2].OffsetMS,
		FromMS:   *members[0].OffsetMS,
		ToMS:     *members[n-1].OffsetMS,
		Sessions: n,
		Clients:  len(clients),
		Retries:  retries,
		Members:  append([]store.PlaybackSession(nil), members...),
	}
}

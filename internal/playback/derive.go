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
	// SuspectStall: at least StallMinSessions sessions, on any clients, ended within
	// StallTolerance of one another at a point well before the end. It wins over a play-through:
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
	// StallTolerance: sessions whose ends lie within this of one another ended at the same point.
	StallTolerance = 90 * time.Second
	// StallMinSessions: how many sessions must end at the same point for a stall to be suspected.
	StallMinSessions = 2
)

// Rules is what the derivation is decided under.
type Rules struct {
	// PlayedFraction is the fraction of the file a session must reach to play it through.
	PlayedFraction float64
	// EndMargin, StallTolerance and StallMinSessions are the fixed figures above.
	EndMargin        time.Duration
	StallTolerance   time.Duration
	StallMinSessions int
}

// RulesFor is the rules a configuration decides under.
func RulesFor(cfg *config.Config) Rules {
	return Rules{
		PlayedFraction:   cfg.EffectivePlaybackPlayedFraction(),
		EndMargin:        EndMargin,
		StallTolerance:   StallTolerance,
		StallMinSessions: StallMinSessions,
	}
}

// Stall is the cluster of session ends a suspected stall rests on. Offsets are milliseconds
// into the file.
type Stall struct {
	// AtMS is the middle session's end: where the file stalls.
	AtMS int64
	// FromMS and ToMS are the first and last session ends in the cluster.
	FromMS, ToMS int64
	// Sessions is how many sessions ended in it, and Members are they.
	Sessions int
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
	readingNone         sessionReading = iota // no offset or no duration: it says nothing
	readingPlayed                             // it reached the mark
	readingStoppedEarly                       // it ended well before the end
)

// read classifies one session. A session with no offset, or no positive duration, says nothing
// either way: it cannot have played the file through and it cannot be placed in a cluster.
func (r Rules) read(s store.PlaybackSession) sessionReading {
	if s.OffsetMS == nil || s.DurationMS == nil || *s.DurationMS <= 0 {
		return readingNone
	}
	off, dur := *s.OffsetMS, *s.DurationMS
	if float64(off) >= r.PlayedFraction*float64(dur) || dur-off <= r.EndMargin.Milliseconds() {
		return readingPlayed
	}
	return readingStoppedEarly
}

// Derive is the verdict the given sessions - all of them attributed to one swapped file - support.
// covered says whether a poll has read the history to its end since the file was swapped: with
// no session and no such poll, nothing is known.
func Derive(sessions []store.PlaybackSession, covered bool, r Rules) Evidence {
	ev := Evidence{Sessions: len(sessions)}
	var early []store.PlaybackSession
	for _, s := range sessions {
		ev.LastStartedAt = max(ev.LastStartedAt, s.StartedAt)
		if s.DurationMS != nil {
			ev.DurationMS = max(ev.DurationMS, *s.DurationMS)
		}
		switch r.read(s) {
		case readingPlayed:
			ev.PlayedThrough++
		case readingStoppedEarly:
			early = append(early, s)
		}
	}
	if st := r.cluster(early); st != nil {
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

// cluster finds the largest set of sessions whose ends lie within StallTolerance of one another,
// the earliest such set on a tie, and returns it when it holds at least StallMinSessions.
func (r Rules) cluster(early []store.PlaybackSession) *Stall {
	if len(early) < r.StallMinSessions || len(early) == 0 {
		return nil
	}
	sorted := append([]store.PlaybackSession(nil), early...)
	slices.SortStableFunc(sorted, func(a, b store.PlaybackSession) int { return cmp.Compare(*a.OffsetMS, *b.OffsetMS) })
	tol := r.StallTolerance.Milliseconds()
	bestFrom, bestLen := 0, 0
	j := 0
	for i := range sorted {
		j = max(j, i)
		for j+1 < len(sorted) && *sorted[j+1].OffsetMS-*sorted[i].OffsetMS <= tol {
			j++
		}
		if n := j - i + 1; n > bestLen {
			bestFrom, bestLen = i, n
		}
	}
	if bestLen < r.StallMinSessions {
		return nil
	}
	members := sorted[bestFrom : bestFrom+bestLen]
	return &Stall{
		AtMS:     *members[(bestLen-1)/2].OffsetMS,
		FromMS:   *members[0].OffsetMS,
		ToMS:     *members[bestLen-1].OffsetMS,
		Sessions: bestLen,
		Members:  append([]store.PlaybackSession(nil), members...),
	}
}

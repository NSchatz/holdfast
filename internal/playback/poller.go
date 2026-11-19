package playback

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// HistoryOverlap is how much of the history before the previous poll each poll reads again. A
// session joins the history when it STOPS, carrying the time it started, so one that was still
// playing when the previous poll read is found by the next only if that poll reads back to its
// start. Two days covers any session that is not left paused for longer; a re-read session is
// the same row and is recorded once.
const HistoryOverlap = 48 * time.Hour

// StuckPolls is how many polls in a row may stop at their page bound without moving the cursor
// before the poll says so at warn: a history the bound cannot get past is evidence nobody reads.
const StuckPolls = 3

// Reader is the part of the ledger a poll and a report read.
type Reader interface {
	PlaybackSwaps(ctx context.Context) ([]store.SwappedFile, error)
	PlaybackEvidence(ctx context.Context) ([]store.PlaybackSession, error)
	PlaybackPollOf(ctx context.Context, source string) (store.PlaybackPoll, bool, error)
	GetRetained(ctx context.Context, path string) (store.Retained, bool, error)
}

// Writer is the part of the ledger a poll writes: its own three tables and nothing else.
type Writer interface {
	RecordSwaps(ctx context.Context, now int64) (added, superseded int, err error)
	RecordPlaybackSessions(ctx context.Context, sessions []store.PlaybackSession) (int, error)
	SetPlaybackPoll(ctx context.Context, p store.PlaybackPoll) error
}

// Poller reads an evidence source on an interval and records the sessions played on swapped
// files. It runs in `serve` beside the engine and off its workers: it asks nothing of the
// encode pipeline, and a source that is slow or down costs this goroutine and nothing else.
type Poller struct {
	Source   Source
	Reads    Reader
	Writes   Writer
	Paths    config.PathMap
	Rules    Rules
	Interval time.Duration
	Log      *slog.Logger

	// now is a test seam for the clock; production leaves it nil.
	now func() time.Time

	// failing is whether the newest poll failed, so an outage is said once and a recovery once.
	failing bool
	// stuck is how many polls in a row stopped at their page bound without moving the cursor.
	stuck int
	// reported is the files already reported as suspected stalls, so each is said once per
	// process and again only after it stopped being one.
	reported map[fileKey]bool
}

// NewPoller builds a poller.
func NewPoller(src Source, reads Reader, writes Writer, paths config.PathMap, rules Rules, interval time.Duration, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{Source: src, Reads: reads, Writes: writes, Paths: paths, Rules: rules, Interval: interval, Log: log}
}

func (p *Poller) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Run polls once at once, then every Interval, until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	p.Log.Info("playback evidence: reading the session history of the files holdfast swapped; a file "+
		"several sessions stop at the same point is reported as a suspected stall, and nothing is acted on",
		"source", p.Source.Name(), "poll_interval", p.Interval.String())
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce(ctx)
			t.Reset(p.Interval)
		}
	}
}

// PollResult is what one poll did.
type PollResult struct {
	// Asked is whether the source was asked at all: not when nothing has been swapped.
	Asked bool
	// Read is how many history rows the source returned; Recorded how many of them were new
	// sessions on a swapped file.
	Read, Recorded int
	// Complete is whether the history was read to its end.
	Complete bool
	// Failure is the class the poll failed with, "" when it did not.
	Failure string
	// Suspects is every file the evidence now says stalls.
	Suspects []Suspect
}

// fileKey names one swapped file: a path and the fingerprint of the bytes swapped in there.
type fileKey struct{ path, fingerprint string }

// RecordSwaps records the swaps the ledger shows and supersedes the ones no longer at their path.
// serve calls it once before the first scan, so a done row a changed setting is about to re-open
// is recorded as a swap before the scan re-derives it; every poll calls it again first.
func (p *Poller) RecordSwaps(ctx context.Context) error {
	if _, _, err := p.Writes.RecordSwaps(ctx, p.clock().Unix()); err != nil {
		p.Log.Warn("playback evidence: the swaps could not be recorded from the ledger", "err", err)
		return err
	}
	return nil
}

// PollOnce reads the history once: from where the previous poll left off, or from the earliest
// swap when none has polled. A failure records the attempt and changes no evidence; every file
// keeps the verdict it had.
func (p *Poller) PollOnce(ctx context.Context) PollResult {
	var res PollResult
	start := p.clock()
	if err := p.RecordSwaps(ctx); err != nil {
		res.Failure = ClassLedger
		return res
	}
	swaps, err := p.Reads.PlaybackSwaps(ctx)
	if err != nil {
		p.Log.Warn("playback evidence: the ledger could not be read, so the history was not asked this poll",
			"source", p.Source.Name(), "err", err)
		res.Failure = ClassLedger
		return res
	}
	if len(swaps) == 0 {
		return res // nothing swapped, so there is nothing to ask about
	}
	state, polled, err := p.Reads.PlaybackPollOf(ctx, p.Source.Name())
	if err != nil {
		p.Log.Warn("playback evidence: the poll state could not be read, so the history was not asked this poll",
			"source", p.Source.Name(), "err", err)
		res.Failure = ClassLedger
		return res
	}
	since := earliest(swaps)
	if polled {
		since = max(since, state.Cursor)
	}
	state.Source, state.LastAttemptAt = p.Source.Name(), start.Unix()

	res.Asked = true
	rows, read, err := p.Source.Sessions(ctx, time.Unix(since, 0))
	if err != nil {
		res.Failure = ClassOf(err)
		p.fail(ctx, state, res.Failure, err)
		return res
	}
	res.Read, res.Complete = len(rows), read.Complete

	sessions := p.attribute(rows, swaps, start.Unix())
	n, err := p.Writes.RecordPlaybackSessions(ctx, sessions)
	if err != nil {
		res.Failure = ClassLedger
		p.fail(ctx, state, res.Failure, err)
		return res
	}
	res.Recorded = n

	// Where the next poll reads from. A poll that read to the end covers everything up to the
	// moment it began, and the next reads again from the overlap before that. One that stopped
	// at its page bound continues from the newest row it went through - skipped rows included,
	// so a run of music or of rows before since cannot hold it in place - with no overlap, so a
	// backlog is always read forward.
	state.LastFailure = ""
	switch {
	case read.Complete:
		state.Cursor = start.Add(-HistoryOverlap).Unix()
		state.CoveredThrough = start.Unix()
		p.stuck = 0
	case read.Newest > since:
		state.Cursor = read.Newest
		p.stuck = 0
	default:
		p.stuck++
		if p.stuck == StuckPolls {
			p.Log.Warn("playback evidence: the history read has not moved for several polls; more sessions "+
				"started in one second than one poll reads, so nothing after them is read",
				"source", p.Source.Name(), "polls", p.stuck, "since", since)
		}
	}
	if err := p.Writes.SetPlaybackPoll(ctx, state); err != nil {
		p.Log.Warn("playback evidence: the history was read and its sessions recorded, but the poll state "+
			"could not be written; the next poll reads the same history again", "source", p.Source.Name(), "err", err)
	}
	if p.failing {
		p.failing = false
		p.Log.Info("playback evidence: the history answers again", "source", p.Source.Name())
	}
	if n > 0 {
		p.Log.Info("playback evidence: recorded sessions played on swapped files",
			"source", p.Source.Name(), "sessions", n, "history_rows_read", len(rows), "complete", read.Complete)
	}
	if !read.Complete {
		p.Log.Info("playback evidence: the history is longer than one poll reads; the next poll continues from here",
			"source", p.Source.Name(), "history_rows_read", len(rows))
	}
	res.Suspects = p.reportSuspects(ctx, start.Unix())
	return res
}

// fail records a failed attempt, keeping the cursor and the coverage the previous poll left, and
// says so once per outage.
func (p *Poller) fail(ctx context.Context, state store.PlaybackPoll, class string, cause error) {
	state.LastFailure = class
	if err := p.Writes.SetPlaybackPoll(ctx, state); err != nil {
		p.Log.Warn("playback evidence: could not record the failed poll in the ledger", "err", err)
	}
	if p.failing {
		p.Log.Debug("playback evidence: the history still cannot be read", "source", p.Source.Name(), "failure", class)
		return
	}
	p.failing = true
	p.Log.Warn("playback evidence: the history could not be read, so nothing was recorded; every file keeps "+
		"the verdict it had, and a file swapped since the last poll stays unknown until the history answers",
		"source", p.Source.Name(), "failure", class, "err", cause)
}

// earliest is the earliest swap among swaps.
func earliest(swaps []store.SwappedFile) int64 {
	first := swaps[0].SwappedAt
	for _, f := range swaps[1:] {
		first = min(first, f.SwappedAt)
	}
	return first
}

// attribute ties each history row to the swap whose bytes it played: the row's file, mapped back
// to holdfast's view through the Plex path map, and the newest swap of that path at or before the
// session started, while that swap was still the file there. A session before every swap of its
// path played the source, and one after the swap was superseded - restored, or another file at
// the path - played bytes that are not the swap's; neither is anybody's evidence. A file no map
// entry covers, where a map is written, is outside what holdfast shares with the media server and
// is ignored, as the play hold ignores it.
func (p *Poller) attribute(rows []Observed, swaps []store.SwappedFile, now int64) []store.PlaybackSession {
	byPath := map[string][]store.SwappedFile{}
	for _, f := range swaps {
		byPath[f.Path] = append(byPath[f.Path], f)
	}
	for _, fs := range byPath {
		slices.SortStableFunc(fs, func(a, b store.SwappedFile) int { return cmp.Compare(a.SwappedAt, b.SwappedAt) })
	}
	var out []store.PlaybackSession
	for _, r := range rows {
		if r.File == "" || !strings.HasPrefix(r.File, "/") {
			continue
		}
		local, covered := p.Paths.ReverseMatched(r.File)
		if len(p.Paths) > 0 && !covered {
			continue
		}
		local = path.Clean(local)
		var swap *store.SwappedFile
		for i, f := range byPath[local] {
			if f.SwappedAt <= r.Started {
				swap = &byPath[local][i]
			}
		}
		if swap == nil || (swap.SupersededAt != 0 && r.Started >= swap.SupersededAt) {
			continue
		}
		out = append(out, store.PlaybackSession{
			Source: p.Source.Name(), SessionID: r.ID, Path: swap.Path, Fingerprint: swap.Fingerprint,
			StartedAt: r.Started, StoppedAt: r.Stopped, OffsetMS: r.OffsetMS, OffsetDerived: r.OffsetDerived,
			DurationMS: r.DurationMS, PlayedSeconds: r.PlayedSeconds, Client: r.Client, Platform: r.Platform,
			Player: r.Player, Decision: r.Decision, RecordedAt: now,
		})
	}
	return out
}

// reportSuspects derives every file's verdict from the ledger and says each suspected stall it
// has not said before, at warn, with what would walk it back.
func (p *Poller) reportSuspects(ctx context.Context, now int64) []Suspect {
	sus, err := Suspects(ctx, p.Reads, p.Rules, now)
	if err != nil {
		p.Log.Warn("playback evidence: the recorded sessions could not be read back, so no suspected stall is "+
			"reported this poll", "err", err)
		return nil
	}
	still := make(map[fileKey]bool, len(sus))
	for _, s := range sus {
		k := fileKey{s.Path, s.Fingerprint}
		still[k] = true
		if p.reported[k] {
			continue
		}
		attrs := []any{"file", s.Path, "sessions_ending_there", s.Evidence.Stall.Sessions,
			"stall_at", offsetText(s.Evidence.Stall.AtMS), "clients_stopped_there", s.Evidence.Stall.Clients,
			"retries_that_got_nowhere", s.Evidence.Stall.Retries, "sessions", s.Evidence.Sessions,
			"played_through", s.Evidence.PlayedThrough, "clients", clientList(s.Evidence.Stall.Members)}
		if s.Restore != "" {
			p.Log.Warn("playback evidence: a swapped file looks like it stalls for real viewers. The original is "+
				"still retained, so `"+s.Restore+"` would put it back; nothing is done until someone runs it",
				attrs...)
		} else {
			p.Log.Warn("playback evidence: a swapped file looks like it stalls for real viewers. Its original is "+
				"no longer retained, so replacing it means acquiring it again; nothing is done automatically",
				attrs...)
		}
	}
	p.reported = still
	return sus
}

// offsetText is an offset as h:mm:ss.
func offsetText(ms int64) string {
	sec := ms / 1000
	return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec%3600/60, sec%60)
}

// clientList names the distinct clients of a set of sessions, platform/player, in order.
func clientList(sessions []store.PlaybackSession) string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		c := strings.Trim(s.Platform+"/"+s.Player, "/")
		if c == "" {
			c = "unnamed client"
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return strings.Join(out, ", ")
}

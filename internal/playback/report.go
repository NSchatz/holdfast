package playback

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// Suspect is one swapped file the evidence says stalls.
type Suspect struct {
	Path        string
	Fingerprint string
	Evidence    Evidence
	// Restore is the command that would put the original back, while a live retention of THIS
	// swap still holds it; "" when none does.
	Restore string
}

// groups splits sessions (ordered by path, then fingerprint) into one slice per file.
func groups(sessions []store.PlaybackSession) map[fileKey][]store.PlaybackSession {
	out := map[fileKey][]store.PlaybackSession{}
	for _, s := range sessions {
		k := fileKey{s.Path, s.Fingerprint}
		out[k] = append(out[k], s)
	}
	return out
}

// Suspects is every file the recorded evidence says stalls, path order, each with the restore
// command where the original is still retained.
func Suspects(ctx context.Context, r Reader, rules Rules, now int64) ([]Suspect, error) {
	ev, err := r.PlaybackEvidence(ctx)
	if err != nil {
		return nil, err
	}
	var out []Suspect
	for k, ss := range groups(ev) {
		e := Derive(ss, true, rules, now)
		if e.Verdict != SuspectStall {
			continue
		}
		cmd, err := restoreCommand(ctx, r, k)
		if err != nil {
			return nil, err
		}
		out = append(out, Suspect{Path: k.path, Fingerprint: k.fingerprint, Evidence: e, Restore: cmd})
	}
	sortSuspects(out)
	return out, nil
}

func sortSuspects(s []Suspect) {
	slices.SortFunc(s, func(a, b Suspect) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.Fingerprint, b.Fingerprint))
	})
}

// newerSwap orders two swaps of one path: the later swap, and the greater fingerprint between two
// in the same second, is the newer.
func newerSwap(a, b store.SwappedFile) int {
	return cmp.Or(cmp.Compare(a.SwappedAt, b.SwappedAt), strings.Compare(a.Fingerprint, b.Fingerprint))
}

// restoreCommand is `holdfast restore <path>` when a live retention of this very swap - its
// swapped path and fingerprint - still holds the original, and "" otherwise. A retention of an
// earlier swap of the same path would refuse to restore over these bytes, so it is not offered.
func restoreCommand(ctx context.Context, r Reader, k fileKey) (string, error) {
	rec, ok, err := r.GetRetained(ctx, k.path)
	if err != nil {
		return "", err
	}
	if !ok || rec.RestoredAt != nil || rec.SwappedPath != k.path || rec.SwappedFingerprint != k.fingerprint {
		return "", nil
	}
	return "holdfast restore " + shellQuote(k.path), nil
}

// shellQuote quotes a path for a POSIX shell when it needs it, so the command can be pasted.
func shellQuote(p string) string {
	if p != "" && strings.Trim(p, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+,:@") == "" {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// Report is GET /api/playback: whether evidence is collected, where the poll has got to, how
// many swapped files each verdict holds, and every suspected stall.
type Report struct {
	// Enabled is whether this daemon polls a source; Source names it ("" when off).
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
	// PollMinutes is the interval, 0 when off.
	PollMinutes int        `json:"poll_minutes"`
	Rules       ReportRule `json:"rules"`
	// LastAttemptAt is the newest poll, LastFailure the class it failed with (null when it did
	// not), and CoveredThrough the start of the newest poll that read the history to its end.
	// Each is null until it exists. Unix seconds.
	LastAttemptAt  *int64  `json:"last_attempt_at"`
	LastFailure    *string `json:"last_failure"`
	CoveredThrough *int64  `json:"covered_through"`
	// UnresolvedSessions is how many sessions the newest successful poll read whose item the
	// source resolved to no single file, null before any poll. All of a poll's sessions here is
	// what a Tautulli that cannot reach Plex looks like.
	UnresolvedSessions *int64 `json:"unresolved_sessions"`
	// Counts is every current swapped file by verdict.
	Counts ReportCounts `json:"counts"`
	// Suspects is every suspected stall, path order.
	Suspects []ReportFile `json:"suspects"`
}

// ReportRule is the rules the verdicts were derived under.
type ReportRule struct {
	PlayedFraction        float64 `json:"played_fraction"`
	EndMarginSeconds      int64   `json:"end_margin_seconds"`
	StallBefore           float64 `json:"stall_before"`
	StallToleranceSeconds int64   `json:"stall_tolerance_seconds"`
	StallMinSessions      int     `json:"stall_min_sessions"`
	RetryWindowSeconds    int64   `json:"retry_window_seconds"`
	StallWindowDays       int     `json:"stall_window_days"`
}

// ReportCounts is every current swap - the newest swap of each path that is still the file
// there - by verdict.
type ReportCounts struct {
	Swapped       int `json:"swapped"`
	SuspectStall  int `json:"suspect_stall"`
	PlayedThrough int `json:"played_through"`
	Unplayed      int `json:"unplayed"`
	Unknown       int `json:"unknown"`
}

// ReportFile is one suspected stall.
type ReportFile struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	SwappedAt   int64  `json:"swapped_at"`
	Verdict     string `json:"verdict"`
	// Sessions is every session on the file; PlayedThrough how many reached the mark.
	Sessions      int `json:"sessions"`
	PlayedThrough int `json:"played_through"`
	// DurationSeconds is the file's length as the source stated it, null when it did not.
	DurationSeconds *int64 `json:"duration_seconds"`
	// StallAtSeconds is where it stalls (the middle session's end), StallFromSeconds and
	// StallToSeconds the first and last session ends, StallSessions how many ended there.
	StallAtSeconds   int64 `json:"stall_at_seconds"`
	StallFromSeconds int64 `json:"stall_from_seconds"`
	StallToSeconds   int64 `json:"stall_to_seconds"`
	StallSessions    int   `json:"stall_sessions"`
	// StallClients is on how many distinct clients those sessions played, StallRetries how many
	// of them played no longer than the retry window: what says the stop was the file's.
	StallClients int `json:"stall_clients"`
	StallRetries int `json:"stall_retries"`
	// Restore is the command that walks the swap back, while the original is retained; null when
	// it no longer is. Nothing runs it but an operator.
	Restore *string `json:"restore"`
	// StalledSessions are the sessions that ended there.
	StalledSessions []ReportSession `json:"stalled_sessions"`
}

// ReportSession is one session that ended at the stall.
type ReportSession struct {
	StartedAt     int64  `json:"started_at"`
	StoppedAt     *int64 `json:"stopped_at"`
	OffsetSeconds int64  `json:"offset_seconds"`
	// OffsetFromPercent says the offset was computed from a whole percent of the file.
	OffsetFromPercent bool `json:"offset_from_percent"`
	// PlayedSeconds is how long the session played, null when the source did not say.
	PlayedSeconds *int64 `json:"played_seconds"`
	Platform      string `json:"platform"`
	Player        string `json:"player"`
	Decision      string `json:"decision"`
}

// BuildReport reads the ledger into the report. It reads and writes nothing else.
func BuildReport(ctx context.Context, r Reader, cfg *config.Config, now time.Time) (Report, error) {
	rules := RulesFor(cfg)
	rep := Report{
		Enabled:  cfg.PlaybackEnabled(),
		Source:   strings.TrimSpace(cfg.PlaybackSource),
		Suspects: []ReportFile{},
		Rules: ReportRule{
			PlayedFraction:        rules.PlayedFraction,
			EndMarginSeconds:      int64(rules.EndMargin.Seconds()),
			StallBefore:           rules.StallBefore,
			StallToleranceSeconds: int64(rules.StallTolerance.Seconds()),
			StallMinSessions:      rules.StallMinSessions,
			RetryWindowSeconds:    int64(rules.RetryWindow.Seconds()),
			StallWindowDays:       int(rules.StallWindow.Hours() / 24),
		},
	}
	if rep.Enabled {
		rep.PollMinutes = int(cfg.PlaybackPollInterval().Minutes())
	}
	var covered int64
	if rep.Source != "" {
		poll, ok, err := r.PlaybackPollOf(ctx, rep.Source)
		if err != nil {
			return Report{}, err
		}
		if ok {
			rep.LastAttemptAt = &poll.LastAttemptAt
			rep.UnresolvedSessions = &poll.Unresolved
			if poll.LastFailure != "" {
				f := poll.LastFailure
				rep.LastFailure = &f
			}
			if poll.CoveredThrough > 0 {
				covered = poll.CoveredThrough
				rep.CoveredThrough = &covered
			}
		}
	}

	swaps, err := r.PlaybackSwaps(ctx)
	if err != nil {
		return Report{}, err
	}
	byPath := map[string][]store.SwappedFile{}
	swappedAt := make(map[fileKey]int64, len(swaps))
	for _, f := range swaps {
		swappedAt[fileKey{f.Path, f.Fingerprint}] = f.SwappedAt
		if f.Current {
			byPath[f.Path] = append(byPath[f.Path], f)
		}
	}
	current := make(map[string]store.SwappedFile, len(byPath))
	for p, fs := range byPath {
		current[p] = slices.MaxFunc(fs, newerSwap)
	}
	ev, err := r.PlaybackEvidence(ctx)
	if err != nil {
		return Report{}, err
	}
	byFile := groups(ev)
	for _, f := range current {
		rep.Counts.Swapped++
		e := Derive(byFile[fileKey{f.Path, f.Fingerprint}], covered > f.SwappedAt, rules, now.Unix())
		switch e.Verdict {
		case SuspectStall:
			rep.Counts.SuspectStall++
		case PlayedThrough:
			rep.Counts.PlayedThrough++
		case Unplayed:
			rep.Counts.Unplayed++
		default:
			rep.Counts.Unknown++
		}
	}

	sus, err := Suspects(ctx, r, rules, now.Unix())
	if err != nil {
		return Report{}, err
	}
	for _, s := range sus {
		rep.Suspects = append(rep.Suspects, reportFile(s, swappedAt[fileKey{s.Path, s.Fingerprint}]))
	}
	return rep, nil
}

// reportFile projects one suspect onto the wire.
func reportFile(s Suspect, swappedAt int64) ReportFile {
	st := s.Evidence.Stall
	f := ReportFile{
		Path: s.Path, Fingerprint: s.Fingerprint, SwappedAt: swappedAt, Verdict: string(s.Evidence.Verdict),
		Sessions: s.Evidence.Sessions, PlayedThrough: s.Evidence.PlayedThrough,
		StallAtSeconds: st.AtMS / 1000, StallFromSeconds: st.FromMS / 1000, StallToSeconds: st.ToMS / 1000,
		StallSessions: st.Sessions, StallClients: st.Clients, StallRetries: st.Retries,
		StalledSessions: []ReportSession{},
	}
	if s.Evidence.DurationMS > 0 {
		d := s.Evidence.DurationMS / 1000
		f.DurationSeconds = &d
	}
	if s.Restore != "" {
		cmd := s.Restore
		f.Restore = &cmd
	}
	for _, m := range st.Members {
		f.StalledSessions = append(f.StalledSessions, ReportSession{
			StartedAt: m.StartedAt, StoppedAt: m.StoppedAt, OffsetSeconds: *m.OffsetMS / 1000,
			OffsetFromPercent: m.OffsetDerived, PlayedSeconds: m.PlayedSeconds,
			Platform: m.Platform, Player: m.Player, Decision: m.Decision,
		})
	}
	return f
}

// Tally counts the files the given sessions - every session on a current swap, as
// PlaybackEvidence returns them - say stall and say were played through. A file with no session
// is in neither count.
func Tally(sessions []store.PlaybackSession, rules Rules, now int64) (suspect, played int) {
	for _, ss := range groups(sessions) {
		switch Derive(ss, true, rules, now).Verdict {
		case SuspectStall:
			suspect++
		case PlayedThrough:
			played++
		}
	}
	return suspect, played
}

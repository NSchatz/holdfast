package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Playback evidence's ledger (docs/design/playback-evidence.md#playback-evidence).
//
// It is a REPORT, like the health sweep's: the encode pipeline never reads these two tables, so
// what a viewer's player did on a swapped file cannot hold a file back from an encode, license
// one, move a gate or release a retained original.

// SwappedFile is one file holdfast swapped in, as its done row records it: the path and
// fingerprint the replacement sits under, and the unix second the row was written, which is
// the moment the swap committed.
type SwappedFile struct {
	Path        string
	Fingerprint string
	SwappedAt   int64
}

// PlaybackSession is one session a media server's history says was played on a swapped file,
// attributed to the done row whose bytes it played.
//
// StoppedAt, OffsetMS and DurationMS are nil when the source did not say. OffsetMS is where in
// the file the session ended; DurationMS is the file's length as the source states it.
type PlaybackSession struct {
	Source      string
	SessionID   string
	Path        string
	Fingerprint string
	StartedAt   int64
	StoppedAt   *int64
	OffsetMS    *int64
	DurationMS  *int64
	// Platform, Player and Decision are the client's platform, its name and the server's
	// stream decision (direct play, copy, transcode), as the source names them; "" when it
	// named none.
	Platform   string
	Player     string
	Decision   string
	RecordedAt int64
}

// PlaybackPoll is where one source's poll has got to. Cursor is the unix second the next poll
// reads the history from: sessions that started then or later. CoveredThrough is the unix second the
// newest poll that read the history to its end BEGAN, 0 until one has: a swap before it has had
// every session since it read. LastAttemptAt is the newest poll, and LastFailure the failure
// class that poll ended in, "" when it succeeded.
type PlaybackPoll struct {
	Source         string
	Cursor         int64
	CoveredThrough int64
	LastAttemptAt  int64
	LastFailure    string
}

// PlaybackLedger is playback evidence's half of the store. Nothing in the encode pipeline reads
// it.
type PlaybackLedger interface {
	// SwappedFiles is every done row: the files a session can be attributed to.
	SwappedFiles(ctx context.Context) ([]SwappedFile, error)
	// RecordPlaybackSessions writes the sessions not already recorded and reports how many
	// were new. A session already recorded is left exactly as it is.
	RecordPlaybackSessions(ctx context.Context, sessions []PlaybackSession) (int, error)
	// PlaybackEvidence is every recorded session on the current swap of its path - the newest
	// done row there - grouped by file (path, then fingerprint) and oldest first within one.
	PlaybackEvidence(ctx context.Context) ([]PlaybackSession, error)
	// PlaybackPollOf is one source's poll state, if it has polled.
	PlaybackPollOf(ctx context.Context, source string) (PlaybackPoll, bool, error)
	// SetPlaybackPoll writes one source's poll state.
	SetPlaybackPoll(ctx context.Context, p PlaybackPoll) error
}

// SwappedFiles is documented on PlaybackLedger. Path order, oldest swap first within a path,
// so a caller can attribute a session to the newest swap at or before it in one pass.
func (s *SQLite) SwappedFiles(ctx context.Context) ([]SwappedFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, fingerprint, updated_at FROM jobs WHERE status = ? ORDER BY path ASC, updated_at ASC, fingerprint ASC`,
		string(Done))
	if err != nil {
		return nil, fmt.Errorf("store: swapped files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SwappedFile
	for rows.Next() {
		var f SwappedFile
		if err := rows.Scan(&f.Path, &f.Fingerprint, &f.SwappedAt); err != nil {
			return nil, fmt.Errorf("store: swapped files scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: swapped files rows: %w", err)
	}
	return out, nil
}

// RecordPlaybackSessions is documented on PlaybackLedger. One transaction for the batch, so a
// poll's sessions land together or not at all, and the poll that wrote them can say so.
func (s *SQLite) RecordPlaybackSessions(ctx context.Context, sessions []PlaybackSession) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: record playback sessions begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	added := 0
	for _, ps := range sessions {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO playback_sessions
				(source, session_id, path, fingerprint, started_at, stopped_at, offset_ms, duration_ms,
				 platform, player, decision, recorded_at, schema_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(source, session_id, path) DO NOTHING`,
			ps.Source, ps.SessionID, ps.Path, ps.Fingerprint, ps.StartedAt, nullInt(ps.StoppedAt),
			nullInt(ps.OffsetMS), nullInt(ps.DurationMS), ps.Platform, ps.Player, ps.Decision,
			ps.RecordedAt, currentStamp())
		if err != nil {
			return 0, fmt.Errorf("store: record playback session: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: record playback session rows affected: %w", err)
		}
		added += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: record playback sessions commit: %w", err)
	}
	return added, nil
}

const playbackSessionColumns = `ps.source, ps.session_id, ps.path, ps.fingerprint, ps.started_at, ps.stopped_at,
	ps.offset_ms, ps.duration_ms, ps.platform, ps.player, ps.decision, ps.recorded_at`

// PlaybackEvidence is documented on PlaybackLedger. Only the CURRENT swap of each path is
// evidence about the file there: the newest done row for that path. A session whose done row
// has gone - the file was restored - or has been superseded by a newer swap of the same path is
// about bytes that are no longer the file at that path, and is not returned.
func (s *SQLite) PlaybackEvidence(ctx context.Context) ([]PlaybackSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+playbackSessionColumns+` FROM playback_sessions ps
		 WHERE EXISTS (SELECT 1 FROM jobs j WHERE j.path = ps.path AND j.fingerprint = ps.fingerprint AND j.status = ?
		   AND NOT EXISTS (SELECT 1 FROM jobs k WHERE k.path = j.path AND k.status = ?
		     AND (k.updated_at > j.updated_at OR (k.updated_at = j.updated_at AND k.fingerprint > j.fingerprint))))
		 ORDER BY ps.path ASC, ps.fingerprint ASC, ps.started_at ASC, ps.source ASC, ps.session_id ASC`,
		string(Done), string(Done))
	if err != nil {
		return nil, fmt.Errorf("store: playback evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PlaybackSession
	for rows.Next() {
		var ps PlaybackSession
		var stopped, offset, duration sql.NullInt64
		if err := rows.Scan(&ps.Source, &ps.SessionID, &ps.Path, &ps.Fingerprint, &ps.StartedAt, &stopped,
			&offset, &duration, &ps.Platform, &ps.Player, &ps.Decision, &ps.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: playback evidence scan: %w", err)
		}
		ps.StoppedAt, ps.OffsetMS, ps.DurationMS = nullableInt(stopped), nullableInt(offset), nullableInt(duration)
		out = append(out, ps)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: playback evidence rows: %w", err)
	}
	return out, nil
}

// PlaybackPollOf is documented on PlaybackLedger.
func (s *SQLite) PlaybackPollOf(ctx context.Context, source string) (PlaybackPoll, bool, error) {
	var p PlaybackPoll
	var covered sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT source, cursor, covered_through, last_attempt_at, last_failure FROM playback_polls WHERE source = ?`,
		source).Scan(&p.Source, &p.Cursor, &covered, &p.LastAttemptAt, &p.LastFailure)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaybackPoll{}, false, nil
	}
	if err != nil {
		return PlaybackPoll{}, false, fmt.Errorf("store: playback poll: %w", err)
	}
	p.CoveredThrough = covered.Int64
	return p, true, nil
}

// SetPlaybackPoll is documented on PlaybackLedger. A CoveredThrough of 0 is written as NULL:
// no poll has read the history to its end.
func (s *SQLite) SetPlaybackPoll(ctx context.Context, p PlaybackPoll) error {
	var covered any
	if p.CoveredThrough > 0 {
		covered = p.CoveredThrough
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO playback_polls (source, cursor, covered_through, last_attempt_at, last_failure, schema_version)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET
			cursor = excluded.cursor,
			covered_through = excluded.covered_through,
			last_attempt_at = excluded.last_attempt_at,
			last_failure = excluded.last_failure,
			schema_version = excluded.schema_version`,
		p.Source, p.Cursor, covered, p.LastAttemptAt, p.LastFailure, currentStamp()); err != nil {
		return fmt.Errorf("store: set playback poll: %w", err)
	}
	return nil
}

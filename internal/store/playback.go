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

// SwappedFile is one file holdfast swapped in: the path and fingerprint the replacement sits
// under, and the unix second the swap committed.
//
// SupersededAt is 0 while the swap is the file at its path as far as the newest poll saw, and
// the second it stopped being after that: an operator restored the original, or another file
// arrived at the path. Current is the same question asked of the ledger as it is NOW, so a
// restore since the last poll is not reported as the file there.
type SwappedFile struct {
	Path         string
	Fingerprint  string
	SwappedAt    int64
	SupersededAt int64
	Current      bool
}

// PlaybackSession is one session a media server's history says was played on a swapped file,
// attributed to the swap whose bytes it played.
//
// StoppedAt, OffsetMS, DurationMS and PlayedSeconds are nil when the source did not say.
// OffsetMS is where in the file the session ended, OffsetDerived says it was computed from a
// whole percent rather than read, DurationMS is the file's length as the source states it and
// PlayedSeconds how long the session played. Client is the source's identifier for the playing
// client, "" when it gave none.
type PlaybackSession struct {
	Source        string
	SessionID     string
	Path          string
	Fingerprint   string
	StartedAt     int64
	StoppedAt     *int64
	OffsetMS      *int64
	OffsetDerived bool
	DurationMS    *int64
	PlayedSeconds *int64
	Client        string
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
	// Unresolved is how many sessions the newest successful poll read whose item the source
	// resolved to no single file.
	Unresolved int64
}

// PlaybackLedger is playback evidence's half of the store. Nothing in the encode pipeline reads
// it.
type PlaybackLedger interface {
	// RecordSwaps records every swap the ledger shows - each done row, and each live retention's
	// swapped file - that is not recorded yet, and marks superseded each recorded swap that is
	// no longer the file at its path. It reports how many swaps it added and superseded.
	RecordSwaps(ctx context.Context, now int64) (added, superseded int, err error)
	// PlaybackSwaps is every recorded swap, path order and oldest first within a path.
	PlaybackSwaps(ctx context.Context) ([]SwappedFile, error)
	// RecordPlaybackSessions writes the sessions not already recorded and reports how many
	// were new. A session already recorded is left exactly as it is.
	RecordPlaybackSessions(ctx context.Context, sessions []PlaybackSession) (int, error)
	// PlaybackEvidence is every recorded session on a current swap, grouped by file (path,
	// then fingerprint) and oldest first within one.
	PlaybackEvidence(ctx context.Context) ([]PlaybackSession, error)
	// PlaybackPollOf is one source's poll state, if it has polled.
	PlaybackPollOf(ctx context.Context, source string) (PlaybackPoll, bool, error)
	// SetPlaybackPoll writes one source's poll state.
	SetPlaybackPoll(ctx context.Context, p PlaybackPoll) error
}

// newestAtPath is the fingerprint of the jobs row most recently INSERTED at w's path, of any
// status: the file the ledger last met there. Insertion order is the rowid's, which no UPDATE
// moves - a claim, a finish, a skip and a requeue's re-open rewrite a row in place, and nothing in
// this store replaces or vacuums jobs rows - so an older file's row that a requeue re-opens does
// not become "newer" than the swap by having its transition time bumped. A new row at the path is
// a scan meeting a file with another fingerprint there: different bytes.
func newestAtPath(w string) string {
	return `(SELECT j.fingerprint FROM jobs j WHERE j.path = ` + w + `.path ORDER BY j.rowid DESC LIMIT 1)`
}

// notRestored is the statement that no restore of the swap aliased as w is recorded.
func notRestored(w string) string {
	return `NOT EXISTS (SELECT 1 FROM retained_originals r WHERE r.swapped_path = ` + w + `.path
			AND r.swapped_fingerprint = ` + w + `.fingerprint AND r.restored_at IS NOT NULL)`
}

// stillAtPath is the one statement of whether a recorded swap, aliased as w, is still the file
// at its path in the ledger as it is now: no restore of it is recorded, and the jobs row inserted
// last at the path - of ANY status - carries its fingerprint, or there is none. Any status,
// because a changed encoder setting or a requeue re-opens the done row and the guard records it
// skipped under the same key: the bytes are the swap's, and so is the evidence about them.
func stillAtPath(w string) string {
	return `COALESCE(` + newestAtPath(w) + `, ` + w + `.fingerprint) = ` + w + `.fingerprint AND ` + notRestored(w)
}

// RecordSwaps is documented on PlaybackLedger. One transaction, so a poll that reads the swaps
// after it sees all of what it did or none.
//
// The second a swap was superseded is the earliest of: the transition time of the row inserted
// last at the path (the other file's), the restore of it, and now. A row's time is its LATEST
// transition, so this can be later than the moment the other file arrived, never earlier.
//
// A swap marked superseded is current again when the row inserted last at its path carries its
// fingerprint once more and no restore of it is recorded - positive evidence, never the mere
// absence of rows - so a mark the ledger no longer supports does not erase the evidence for good.
func (s *SQLite) RecordSwaps(ctx context.Context, now int64) (int, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("store: record swaps begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	added := 0
	for _, q := range []string{
		`INSERT INTO playback_swaps (path, fingerprint, swapped_at, recorded_at, schema_version)
		 SELECT path, fingerprint, updated_at, ?, ? FROM jobs WHERE status = 'done'
		 ON CONFLICT(path, fingerprint) DO NOTHING`,
		`INSERT INTO playback_swaps (path, fingerprint, swapped_at, recorded_at, schema_version)
		 SELECT swapped_path, swapped_fingerprint, retained_at, ?, ? FROM retained_originals WHERE restored_at IS NULL
		 ON CONFLICT(path, fingerprint) DO NOTHING`,
	} {
		res, err := tx.ExecContext(ctx, q, now, currentStamp())
		if err != nil {
			return 0, 0, fmt.Errorf("store: record swaps: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, 0, fmt.Errorf("store: record swaps rows affected: %w", err)
		}
		added += int(n)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE playback_swaps SET superseded_at = NULL, schema_version = ?
		 WHERE superseded_at IS NOT NULL AND `+newestAtPath("playback_swaps")+` = playback_swaps.fingerprint
		   AND `+notRestored("playback_swaps"),
		currentStamp()); err != nil {
		return 0, 0, fmt.Errorf("store: restore swaps: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE playback_swaps SET schema_version = ?, superseded_at = MIN(?,
			COALESCE((SELECT j.updated_at FROM jobs j WHERE j.path = playback_swaps.path
				AND j.fingerprint != playback_swaps.fingerprint ORDER BY j.rowid DESC LIMIT 1), ?),
			COALESCE((SELECT MIN(r.restored_at) FROM retained_originals r WHERE r.swapped_path = playback_swaps.path
				AND r.swapped_fingerprint = playback_swaps.fingerprint AND r.restored_at IS NOT NULL), ?))
		 WHERE superseded_at IS NULL AND NOT (`+stillAtPath("playback_swaps")+`)`,
		currentStamp(), now, now, now)
	if err != nil {
		return 0, 0, fmt.Errorf("store: supersede swaps: %w", err)
	}
	superseded, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("store: supersede swaps rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("store: record swaps commit: %w", err)
	}
	return added, int(superseded), nil
}

// PlaybackSwaps is documented on PlaybackLedger.
func (s *SQLite) PlaybackSwaps(ctx context.Context) ([]SwappedFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT w.path, w.fingerprint, w.swapped_at, w.superseded_at,
			(w.superseded_at IS NULL AND `+stillAtPath("w")+`)
		 FROM playback_swaps w ORDER BY w.path ASC, w.swapped_at ASC, w.fingerprint ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: playback swaps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SwappedFile
	for rows.Next() {
		var f SwappedFile
		var superseded sql.NullInt64
		if err := rows.Scan(&f.Path, &f.Fingerprint, &f.SwappedAt, &superseded, &f.Current); err != nil {
			return nil, fmt.Errorf("store: playback swaps scan: %w", err)
		}
		f.SupersededAt = superseded.Int64
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: playback swaps rows: %w", err)
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
				(source, session_id, path, fingerprint, started_at, stopped_at, offset_ms, offset_derived,
				 duration_ms, played_seconds, client, platform, player, decision, recorded_at, schema_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(source, session_id, path) DO NOTHING`,
			ps.Source, ps.SessionID, ps.Path, ps.Fingerprint, ps.StartedAt, nullInt(ps.StoppedAt),
			nullInt(ps.OffsetMS), ps.OffsetDerived, nullInt(ps.DurationMS), nullInt(ps.PlayedSeconds), ps.Client,
			ps.Platform, ps.Player, ps.Decision, ps.RecordedAt, currentStamp())
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
	ps.offset_ms, ps.offset_derived, ps.duration_ms, ps.played_seconds, ps.client, ps.platform, ps.player,
	ps.decision, ps.recorded_at`

// PlaybackEvidence is documented on PlaybackLedger. A session on a swap that is no longer the
// file at its path - restored, or replaced by another file - is about bytes that are no longer
// there, and is not returned.
func (s *SQLite) PlaybackEvidence(ctx context.Context) ([]PlaybackSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+playbackSessionColumns+` FROM playback_sessions ps
		 JOIN playback_swaps w ON w.path = ps.path AND w.fingerprint = ps.fingerprint
		 WHERE w.superseded_at IS NULL AND `+stillAtPath("w")+`
		 ORDER BY ps.path ASC, ps.fingerprint ASC, ps.started_at ASC, ps.source ASC, ps.session_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: playback evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PlaybackSession
	for rows.Next() {
		var ps PlaybackSession
		var stopped, offset, duration, played sql.NullInt64
		if err := rows.Scan(&ps.Source, &ps.SessionID, &ps.Path, &ps.Fingerprint, &ps.StartedAt, &stopped,
			&offset, &ps.OffsetDerived, &duration, &played, &ps.Client, &ps.Platform, &ps.Player, &ps.Decision,
			&ps.RecordedAt); err != nil {
			return nil, fmt.Errorf("store: playback evidence scan: %w", err)
		}
		ps.StoppedAt, ps.OffsetMS = nullableInt(stopped), nullableInt(offset)
		ps.DurationMS, ps.PlayedSeconds = nullableInt(duration), nullableInt(played)
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
		`SELECT source, cursor, covered_through, last_attempt_at, last_failure, unresolved_rows
		 FROM playback_polls WHERE source = ?`,
		source).Scan(&p.Source, &p.Cursor, &covered, &p.LastAttemptAt, &p.LastFailure, &p.Unresolved)
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
		`INSERT INTO playback_polls (source, cursor, covered_through, last_attempt_at, last_failure, unresolved_rows,
			schema_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET
			cursor = excluded.cursor,
			covered_through = excluded.covered_through,
			last_attempt_at = excluded.last_attempt_at,
			last_failure = excluded.last_failure,
			unresolved_rows = excluded.unresolved_rows,
			schema_version = excluded.schema_version`,
		p.Source, p.Cursor, covered, p.LastAttemptAt, p.LastFailure, p.Unresolved, currentStamp()); err != nil {
		return fmt.Errorf("store: set playback poll: %w", err)
	}
	return nil
}

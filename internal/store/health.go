package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The library health sweep's ledger (docs/design/health-sweep.md#health-sweep).
//
// It is a REPORT and nothing reads it to decide anything about a file: the encode pipeline
// never consults these two tables, so what a sweep recorded cannot hold a file back from an
// encode, license one, or move any gate.

// HealthResult is what one full decode of one file established. The vocabulary is closed.
type HealthResult string

const (
	// HealthOK is a file every video stream of which decoded to the end with no error.
	HealthOK HealthResult = "ok"
	// HealthCorrupt is a file that was read and did NOT decode cleanly to the end.
	HealthCorrupt HealthResult = "corrupt"
	// HealthUnreadable is a file this process could not read at all: it could not be
	// stat'd or opened, it is not a regular file, or it kept changing while it was read.
	HealthUnreadable HealthResult = "unreadable"
)

// ValidHealthResult reports whether r is a member of the closed vocabulary.
func ValidHealthResult(r HealthResult) bool {
	return r == HealthOK || r == HealthCorrupt || r == HealthUnreadable
}

// HealthCounts is how many files a sweep checked, by result.
type HealthCounts struct {
	OK         int64
	Corrupt    int64
	Unreadable int64
}

// Checked is the number of files the counts cover.
func (c HealthCounts) Checked() int64 { return c.OK + c.Corrupt + c.Unreadable }

// HealthSweep is one sweep's row. FinishedAt is the zero time while it is running, and
// Counts is only meaningful once it has finished: a running sweep is counted from its
// checks (HealthCountsOf) instead.
type HealthSweep struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt time.Time
	Counts     HealthCounts
}

// Finished reports whether the sweep ran to the end of the enumeration.
func (s HealthSweep) Finished() bool { return !s.FinishedAt.IsZero() }

// HealthCheck is one file's result in one sweep, with the fingerprint (size and
// modification time) of the bytes it was taken against.
type HealthCheck struct {
	SweepID   int64
	Path      string
	Size      int64
	MtimeNS   int64
	CheckedAt time.Time
	Result    HealthResult
	Reason    string
}

// StartHealthSweep opens a new sweep at the given time and returns it. Before it does, it
// drops the per-file checks of every sweep older than the newest FINISHED one: what the
// read surface reports is the sweep under way and the last one that finished, and nothing
// else, so a weekly sweep over a large library does not grow the ledger without bound.
// The sweep rows themselves are kept; each carries its own counts.
func (s *SQLite) StartHealthSweep(ctx context.Context, at time.Time) (HealthSweep, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM health_checks WHERE sweep_id <
		(SELECT COALESCE(MAX(id), 0) FROM health_sweeps WHERE finished_at IS NOT NULL)`); err != nil {
		return HealthSweep{}, fmt.Errorf("store: prune health checks: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO health_sweeps (started_at, schema_version) VALUES (?, ?)`, at.Unix(), currentStamp())
	if err != nil {
		return HealthSweep{}, fmt.Errorf("store: start health sweep: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return HealthSweep{}, fmt.Errorf("store: start health sweep id: %w", err)
	}
	return HealthSweep{ID: id, StartedAt: time.Unix(at.Unix(), 0)}, nil
}

// FinishHealthSweep marks a sweep finished at the given time and writes its counts, read
// from its own checks in the same statement so the row and the checks cannot disagree.
func (s *SQLite) FinishHealthSweep(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE health_sweeps SET finished_at = ?,
		ok_count         = (SELECT COUNT(*) FROM health_checks WHERE sweep_id = ? AND result = 'ok'),
		corrupt_count    = (SELECT COUNT(*) FROM health_checks WHERE sweep_id = ? AND result = 'corrupt'),
		unreadable_count = (SELECT COUNT(*) FROM health_checks WHERE sweep_id = ? AND result = 'unreadable')
		WHERE id = ? AND finished_at IS NULL`, at.Unix(), id, id, id, id)
	if err != nil {
		return fmt.Errorf("store: finish health sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish health sweep rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("store: finish health sweep %d: no running sweep has that id", id)
	}
	return nil
}

const healthSweepColumns = `id, started_at, finished_at, ok_count, corrupt_count, unreadable_count`

func scanHealthSweep(sc interface{ Scan(...any) error }) (HealthSweep, error) {
	var sw HealthSweep
	var started int64
	var finished, ok, corrupt, unreadable sql.NullInt64
	if err := sc.Scan(&sw.ID, &started, &finished, &ok, &corrupt, &unreadable); err != nil {
		return HealthSweep{}, err
	}
	sw.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		sw.FinishedAt = time.Unix(finished.Int64, 0)
		sw.Counts = HealthCounts{OK: ok.Int64, Corrupt: corrupt.Int64, Unreadable: unreadable.Int64}
	}
	return sw, nil
}

// healthSweepWhere reads the newest sweep matching cond, if there is one.
func (s *SQLite) healthSweepWhere(ctx context.Context, cond string) (HealthSweep, bool, error) {
	sw, err := scanHealthSweep(s.db.QueryRowContext(ctx,
		`SELECT `+healthSweepColumns+` FROM health_sweeps `+cond+` ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return HealthSweep{}, false, nil
	}
	if err != nil {
		return HealthSweep{}, false, fmt.Errorf("store: read health sweep: %w", err)
	}
	return sw, true, nil
}

// LatestHealthSweep is the newest sweep, finished or not.
func (s *SQLite) LatestHealthSweep(ctx context.Context) (HealthSweep, bool, error) {
	return s.healthSweepWhere(ctx, "")
}

// LastFinishedHealthSweep is the newest sweep that ran to the end.
func (s *SQLite) LastFinishedHealthSweep(ctx context.Context) (HealthSweep, bool, error) {
	return s.healthSweepWhere(ctx, "WHERE finished_at IS NOT NULL")
}

// RecordHealthCheck writes one file's result in one sweep. Recording the same path in the
// same sweep again REPLACES the earlier result: that only happens when the file's
// fingerprint moved after it was checked, and then the earlier result is about bytes that
// are no longer there.
func (s *SQLite) RecordHealthCheck(ctx context.Context, c HealthCheck) error {
	if !ValidHealthResult(c.Result) {
		return fmt.Errorf("store: record health check: %q is not a health result", c.Result)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO health_checks
		(sweep_id, path, size, mtime_ns, checked_at, result, reason, schema_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sweep_id, path) DO UPDATE SET size = excluded.size, mtime_ns = excluded.mtime_ns,
		checked_at = excluded.checked_at, result = excluded.result, reason = excluded.reason,
		schema_version = excluded.schema_version`,
		c.SweepID, c.Path, c.Size, c.MtimeNS, c.CheckedAt.Unix(), string(c.Result), c.Reason, currentStamp())
	if err != nil {
		return fmt.Errorf("store: record health check: %w", err)
	}
	return nil
}

const healthCheckColumns = `sweep_id, path, size, mtime_ns, checked_at, result, reason`

func scanHealthCheck(sc interface{ Scan(...any) error }) (HealthCheck, error) {
	var c HealthCheck
	var at int64
	var result string
	if err := sc.Scan(&c.SweepID, &c.Path, &c.Size, &c.MtimeNS, &at, &result, &c.Reason); err != nil {
		return HealthCheck{}, err
	}
	c.CheckedAt = time.Unix(at, 0)
	c.Result = HealthResult(result)
	return c, nil
}

// HealthCheckOf is one path's result in one sweep, if that sweep checked it.
func (s *SQLite) HealthCheckOf(ctx context.Context, sweepID int64, path string) (HealthCheck, bool, error) {
	c, err := scanHealthCheck(s.db.QueryRowContext(ctx,
		`SELECT `+healthCheckColumns+` FROM health_checks WHERE sweep_id = ? AND path = ?`, sweepID, path))
	if errors.Is(err, sql.ErrNoRows) {
		return HealthCheck{}, false, nil
	}
	if err != nil {
		return HealthCheck{}, false, fmt.Errorf("store: read health check: %w", err)
	}
	return c, true, nil
}

// HealthCountsOf counts one sweep's checks by result, finished or not.
func (s *SQLite) HealthCountsOf(ctx context.Context, sweepID int64) (HealthCounts, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT result, COUNT(*) FROM health_checks WHERE sweep_id = ? GROUP BY result`, sweepID)
	if err != nil {
		return HealthCounts{}, fmt.Errorf("store: count health checks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var c HealthCounts
	for rows.Next() {
		var result string
		var n int64
		if err := rows.Scan(&result, &n); err != nil {
			return HealthCounts{}, fmt.Errorf("store: count health checks scan: %w", err)
		}
		switch HealthResult(result) {
		case HealthOK:
			c.OK = n
		case HealthCorrupt:
			c.Corrupt = n
		case HealthUnreadable:
			c.Unreadable = n
		}
	}
	if err := rows.Err(); err != nil {
		return HealthCounts{}, fmt.Errorf("store: count health checks rows: %w", err)
	}
	return c, nil
}

// HealthProblems is the files one sweep found corrupt or unreadable, at most limit of them
// (limit <= 0 is no limit), by path.
func (s *SQLite) HealthProblems(ctx context.Context, sweepID int64, limit int) ([]HealthCheck, error) {
	q := `SELECT ` + healthCheckColumns + ` FROM health_checks
		WHERE sweep_id = ? AND result IN ('corrupt', 'unreadable') ORDER BY path`
	args := []any{sweepID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: health problems: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []HealthCheck{}
	for rows.Next() {
		c, err := scanHealthCheck(rows)
		if err != nil {
			return nil, fmt.Errorf("store: health problems scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: health problems rows: %w", err)
	}
	return out, nil
}

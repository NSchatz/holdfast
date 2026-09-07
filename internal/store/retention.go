package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Ledger retention (LEDGER-5).
//
// WHY THIS IS THE DANGEROUS FILE. Everything else in this package records what happened.
// This one DELETES it, and no re-run undoes that: a later scan does not recreate a pruned
// row, it re-derives the file's CURRENT state instead, so a pruned `done` row for a file
// still on disk comes back as a `skipped` row carrying the already-at-target-codec guard.
// The proof that holdfast transcoded that file is replaced by a weaker record. That is why
// retention ships DISABLED (config.HistoryRetentionRows defaults to 0) and why the two
// exclusions below are load-bearing rather than defensive.
//
// THE TOTAL MUST NOT MOVE. The published lifetime reclaimed figure is a SUM over done rows
// that recorded both sizes, and internal/server's Hub reads it ONCE, at construction, as a
// baseline. So a prune that removed contributing rows would show a correct total until the
// next restart and a wrong one after it - the worst shape a defect can have, because the
// only symptom is a number that quietly shrinks weeks later. Every batch therefore moves
// the rows' contribution into ledger_totals in the SAME transaction that deletes them, and
// ReclaimedTotal reads live rows plus that carry-forward.
//
// THE ENGINE MUST NOT NOTICE. Deleting a terminal row makes the engine claim that file
// again on the next scan. For a done or skipped row that is harmless - the guards re-fire
// and it is re-skipped, no encode - but a FAILED row also carries fail_count, which is what
// parks a file that has failed max_failures times. Deleting that row hands the file back to
// the encoder. Parked rows are therefore kept and reported, never pruned.

// pruneBatch is how many rows one transaction removes. A ledger at library scale can be
// hundreds of thousands of rows over the retention, and the store runs every access
// through ONE serialized connection - the same one the engine's Claim/Advance/Finish go
// through. A single DELETE of the whole excess would hold that connection (and so the
// encode pipeline's writes) for as long as it took. Batching bounds each transaction, and
// it is also what makes a failure part way through leave every row it did not remove in
// place, with the carried total already correct for the rows it did.
const pruneBatch = 500

// PruneTerminal is documented on the Store interface.
func (s *SQLite) PruneTerminal(ctx context.Context, maxRows, maxFailures int) (Prune, error) {
	var p Prune
	if maxRows <= 0 {
		// Retention DISABLED: the shipped default. Not "prune nothing this time" - this
		// path reads nothing and writes nothing at all, so a default configuration cannot
		// so much as scan the table it would have deleted from.
		return p, nil
	}
	if maxFailures < 0 {
		maxFailures = 0
	}

	for {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		excess, err := s.terminalExcess(ctx, maxRows)
		if err != nil {
			return p, err
		}
		if excess <= 0 {
			break
		}
		if excess > pruneBatch {
			excess = pruneBatch
		}
		batch, err := s.pruneOldestBatch(ctx, int(excess), maxFailures)
		if err != nil {
			return p, err
		}
		p.Removed += batch.Removed
		p.ReclaimedCarried += batch.ReclaimedCarried
		if batch.Removed == 0 {
			// Nothing in this batch was prunable, so nothing in the next one would be
			// either: every remaining row above the retention is parked. Report how many
			// and stop, rather than spinning on a table that cannot shrink.
			p.Kept, err = s.parkedCount(ctx, maxFailures)
			if err != nil {
				return p, err
			}
			break
		}
	}
	return p, nil
}

// terminalExcess is how many terminal rows the ledger holds beyond maxRows. It is
// recomputed per batch on purpose: the engine is finishing jobs on the same connection
// while this runs, so a count taken once at the start would be stale by the second batch
// and could delete rows the retention now covers.
func (s *SQLite) terminalExcess(ctx context.Context, maxRows int) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE status IN (?, ?, ?)`,
		string(Done), string(Skipped), string(Failed)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: prune count terminal rows: %w", err)
	}
	return n - int64(maxRows), nil
}

// parkedCount is how many terminal rows the prune refuses to remove: failed rows that have
// reached max_failures and are therefore parked. Reported so an operator who configured a
// retention the ledger cannot meet is told why, rather than watching a table stay large.
func (s *SQLite) parkedCount(ctx context.Context, maxFailures int) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE status = ? AND fail_count >= ?`,
		string(Failed), maxFailures).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: prune count parked rows: %w", err)
	}
	return n, nil
}

// pruneOldestBatch removes at most limit of the oldest prunable terminal rows in ONE
// transaction, carrying their reclaimed contribution forward first. Either both landed or
// neither did: a crash or an error between the two would be exactly the silent total-drop
// this whole mechanism exists to prevent.
func (s *SQLite) pruneOldestBatch(ctx context.Context, limit, maxFailures int) (Prune, error) {
	var p Prune
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, fmt.Errorf("store: prune begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	// The candidates, oldest transition first. A parked failed row is excluded HERE rather
	// than filtered after the fact, so the batch is genuinely full of removable rows and a
	// ledger of nothing but parked rows terminates immediately instead of paging through
	// them. path is the tie-break so the order is total and two runs agree.
	rows, err := tx.QueryContext(ctx,
		`SELECT rowid, source_bytes, output_bytes, status FROM jobs
		 WHERE status IN (?, ?, ?) AND NOT (status = ? AND fail_count >= ?)
		 ORDER BY updated_at ASC, path ASC
		 LIMIT ?`,
		string(Done), string(Skipped), string(Failed), string(Failed), maxFailures, limit)
	if err != nil {
		return p, fmt.Errorf("store: prune select: %w", err)
	}
	var ids []int64
	var carried int64
	for rows.Next() {
		var id int64
		var src, out sql.NullInt64
		var status string
		if err := rows.Scan(&id, &src, &out, &status); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("store: prune scan: %w", err)
		}
		ids = append(ids, id)
		// Exactly ReclaimedTotal's rule, so the two cannot disagree about what a row was
		// worth: a done row that recorded BOTH sizes contributes their difference, and
		// anything else contributes nothing (never a NULL read as 0). The clamp at 0
		// mirrors ReclaimedTotal's own, so a future bug in the strictly-smaller gate can
		// never make a carried total run backwards.
		if Status(status) == Done && src.Valid && out.Valid && src.Int64 > out.Int64 {
			carried += src.Int64 - out.Int64
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return p, fmt.Errorf("store: prune rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return p, fmt.Errorf("store: prune close: %w", err)
	}
	if len(ids) == 0 {
		return p, nil
	}

	// Carry FIRST, delete second, both inside this transaction. The order is not
	// cosmetic: it is the statement that no row is removed until what it contributed to
	// the durable total is already somewhere else.
	if carried > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE ledger_totals SET reclaimed_pruned = reclaimed_pruned + ? WHERE id = 1`,
			carried); err != nil {
			return p, fmt.Errorf("store: prune carry reclaimed total: %w", err)
		}
	}

	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE rowid IN (`+strings.Join(ph, ", ")+`)`, args...)
	if err != nil {
		return p, fmt.Errorf("store: prune delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return p, fmt.Errorf("store: prune rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return p, fmt.Errorf("store: prune commit: %w", err)
	}
	p.Removed, p.ReclaimedCarried = n, carried
	return p, nil
}

// CountRows is documented on the Store interface.
func (s *SQLite) CountRows(ctx context.Context, statuses []Status) RowTotal {
	t := RowTotal{Coverage: coverageFor(statuses)}
	q := `SELECT COUNT(*) FROM jobs`
	args := make([]any, 0, len(statuses))
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		q += " WHERE status IN (" + strings.Join(ph, ", ") + ")"
	}
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&t.Count); err != nil {
		t.Count = 0
		t.Err = fmt.Errorf("store: count rows: %w", err)
	}
	return t
}

// coverageFor states the SET a row total is over, in the same words the aggregates use.
// It is produced beside the query rather than written into a handler, so the figure and
// the description of what it counts cannot drift apart.
func coverageFor(statuses []Status) Coverage {
	if len(statuses) == 0 {
		return Coverage{Set: "every row in the ledger"}
	}
	names := make([]string, len(statuses))
	for i, st := range statuses {
		names[i] = string(st)
	}
	return Coverage{Set: "every row in the ledger with status " + strings.Join(names, ", ")}
}

// EachTerminal is documented on the Store interface. Rows are streamed straight off the
// cursor - the export must work on a ledger far larger than memory, which is the whole
// reason it is not List(ctx, terminal, 0).
//
// OLDEST FIRST, unlike List: this is an audit record read from the top, not the "most
// recent activity" the dashboard shows, and appending to a chronological export is the
// shape a consumer expects.
func (s *SQLite) EachTerminal(ctx context.Context, fn func(Job) error) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, fingerprint, status, fail_count, worker, updated_at,
			`+outcomeColumns+`
		 FROM jobs WHERE status IN (?, ?, ?)
		 ORDER BY updated_at ASC, path ASC`,
		string(Done), string(Skipped), string(Failed))
	if err != nil {
		return fmt.Errorf("store: each terminal: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var j Job
		var status string
		var worker sql.NullString
		// The SAME projection List reads through, so the export can never carry a
		// narrower row than the API publishes - which is the promise its format is
		// stated as, and one that a hand-written column list here would quietly break
		// the next time a column is appended.
		var oc outcomeScan
		dest := append([]any{&j.Path, &j.Fingerprint, &status, &j.FailCount, &worker, &j.UpdatedAt}, oc.dest()...)
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("store: each terminal scan: %w", err)
		}
		j.Status = Status(status)
		j.Worker = worker.String
		j.Outcome = oc.outcome()
		if err := fn(j); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: each terminal rows: %w", err)
	}
	return nil
}

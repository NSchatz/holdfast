package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
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
// again on the next scan, and a terminal row is a DECISION and not only a record: Claim
// refuses a done or skipped row outright, and every skip guard that would re-derive the
// same verdict runs AFTER Claim, under whatever configuration is current. So the verdict
// re-derives itself only while the configuration it was taken under has not moved, and two
// supported settings move it - a change of target codec, and a lowered min_bitrate_kbps.
// Under either, deleting the row of a file that is still in the library hands that file to
// the encoder. Which row that is safe to remove is therefore a question about the LIBRARY,
// and this package never touches the filesystem: the caller answers it through Prunable.
// A failed row parked at max_failures is refused here as well, on the store's own columns,
// because that one the store can see for itself.
//
// Both refusals are reported in Prune.Kept rather than hidden: an operator whose retention
// cannot be met is owed the count.

// pruneBatch is how many rows one transaction removes. A ledger at library scale can be
// hundreds of thousands of rows over the retention, and the store runs every access
// through ONE serialized connection - the same one the engine's Claim/Advance/Finish go
// through. A single DELETE of the whole excess would hold that connection (and so the
// encode pipeline's writes) for as long as it took. Batching bounds each transaction, and
// it is also what makes a failure part way through leave every row it did not remove in
// place, with the carried total already correct for the rows it did.
const pruneBatch = 500

// PruneTerminal is documented on the Store interface.
func (s *SQLite) PruneTerminal(ctx context.Context, maxRows, maxFailures int, prunable Prunable) (Prune, error) {
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

	// The walk is a keyset cursor over (updated_at, path) rather than a repeated "oldest
	// N": a row this pass refuses must not be offered again, or a ledger whose oldest rows
	// are all refused would page over them for ever. Rows are examined oldest first, so
	// what IS removed is still the oldest removable.
	cur := cursor{updatedAt: math.MinInt64}
	for {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		// Recomputed per batch on purpose: the engine finishes jobs on the same
		// connection, so a count taken once at the start would be stale by the second
		// batch and could delete rows the retention now covers.
		excess, err := s.terminalExcess(ctx, maxRows)
		if err != nil {
			return p, err
		}
		if excess <= 0 {
			break
		}
		cands, err := s.terminalCandidates(ctx, cur, pruneBatch)
		if err != nil {
			return p, err
		}
		if len(cands) == 0 {
			// The whole terminal table has been offered. Whatever still sits above the
			// retention is there because removing it would change what the engine does.
			break
		}

		// Ask about each candidate in turn, stopping the moment enough are approved to
		// bring the ledger within the retention. The cursor advances over exactly the
		// rows that were EXAMINED, never over rows the batch never reached.
		doomed := make([]candidate, 0, len(cands))
		examined := 0
		for _, c := range cands {
			if int64(len(doomed)) >= excess {
				break
			}
			examined++
			if c.status == Failed && c.failCount >= maxFailures {
				// Parked: the engine refuses to claim it, and deleting it would reset
				// that accounting and hand the file back to the encoder.
				p.Kept++
				continue
			}
			if prunable == nil || !prunable(c.path, c.fingerprint, c.status) {
				// The caller says this row is still holding a file out of the encoder
				// (or cannot tell, which is answered the same way). A nil Prunable is
				// "nothing is prunable": a caller who cannot answer the question must
				// not have audit history deleted on its behalf.
				p.Kept++
				continue
			}
			doomed = append(doomed, c)
		}
		cur = cands[examined-1].cursor

		if len(doomed) == 0 {
			continue
		}
		batch, err := s.deleteBatch(ctx, doomed)
		p.Removed += batch.Removed
		p.ReclaimedCarried += batch.ReclaimedCarried
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// cursor is the keyset position of the candidate walk: the (updated_at, path, fingerprint)
// of the last row examined.
//
// All THREE, because only all three are unique. The table's primary key is
// (path, fingerprint), so one path can carry several rows - a file whose content changed
// leaves the old key behind - and a two-column cursor would step straight over the second
// of two rows that share a path and a transition second. It is the same total order the
// candidate query sorts by, so no row is visited twice and none is skipped.
type cursor struct {
	updatedAt   int64
	path        string
	fingerprint string
}

// candidate is one terminal row the pass is deciding about.
type candidate struct {
	cursor
	rowid     int64
	status    Status
	failCount int
}

// terminalCandidates returns the next terminal rows after cur, oldest transition first.
//
// It reads OUTSIDE any transaction, deliberately: the decision about a candidate is the
// caller's, the caller looks at the filesystem to make it, and holding the single
// serialized write connection's transaction open across that is exactly the stall the
// batching exists to prevent. What that costs is a row that changes between this read and
// the delete, and deleteBatch re-reads every row inside its own transaction rather than
// trusting this one.
func (s *SQLite) terminalCandidates(ctx context.Context, cur cursor, limit int) ([]candidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, path, fingerprint, status, fail_count, updated_at FROM jobs
		 WHERE status IN (?, ?, ?)
		   AND (updated_at > ?
		     OR (updated_at = ? AND path > ?)
		     OR (updated_at = ? AND path = ? AND fingerprint > ?))
		 ORDER BY updated_at ASC, path ASC, fingerprint ASC
		 LIMIT ?`,
		string(Done), string(Skipped), string(Failed),
		cur.updatedAt, cur.updatedAt, cur.path, cur.updatedAt, cur.path, cur.fingerprint, limit)
	if err != nil {
		return nil, fmt.Errorf("store: prune select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []candidate
	for rows.Next() {
		var c candidate
		var status string
		if err := rows.Scan(&c.rowid, &c.path, &c.fingerprint, &status, &c.failCount, &c.updatedAt); err != nil {
			return nil, fmt.Errorf("store: prune scan: %w", err)
		}
		c.status = Status(status)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: prune rows: %w", err)
	}
	return out, nil
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

// deleteBatch removes the approved rows in ONE transaction, carrying their reclaimed
// contribution forward first. Either both landed or neither did: a crash or an error
// between the two would be exactly the silent total-drop this whole mechanism exists to
// prevent.
//
// Every row is RE-READ inside this transaction rather than trusted from the candidate
// walk, and one whose status or transition time has moved since is dropped from the batch.
// The candidates were read without a lock so the caller could look at the filesystem, and
// in that window a failed row can be re-claimed by a worker: deleting it then would remove
// a row that is no longer history, and carrying a size the row no longer records would put
// a byte count into the durable total that no deletion earned.
func (s *SQLite) deleteBatch(ctx context.Context, doomed []candidate) (Prune, error) {
	var p Prune
	if len(doomed) == 0 {
		return p, nil
	}
	expect := make(map[int64]int64, len(doomed))
	ph := make([]string, len(doomed))
	args := make([]any, len(doomed))
	for i, c := range doomed {
		expect[c.rowid] = c.updatedAt
		ph[i] = "?"
		args[i] = c.rowid
	}
	in := strings.Join(ph, ", ")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, fmt.Errorf("store: prune begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	rows, err := tx.QueryContext(ctx,
		`SELECT rowid, status, updated_at, source_bytes, output_bytes FROM jobs
		 WHERE rowid IN (`+in+`)`, args...)
	if err != nil {
		return p, fmt.Errorf("store: prune re-read: %w", err)
	}
	var ids []any
	var carried int64
	for rows.Next() {
		var id, updatedAt int64
		var src, out sql.NullInt64
		var status string
		if err := rows.Scan(&id, &status, &updatedAt, &src, &out); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("store: prune scan: %w", err)
		}
		if at, ok := expect[id]; !ok || at != updatedAt || !Status(status).Terminal() {
			continue // moved under us since the candidate walk: not this pass's row
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
	// the durable total is already somewhere else. The UPDATE's ROW COUNT is checked for
	// the same reason the DELETE's is: a ledger_totals with no id = 1 row would otherwise
	// commit a delete whose carry silently went nowhere, and the only symptom would be a
	// lifetime total that shrank at some restart weeks later.
	if carried > 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE ledger_totals SET reclaimed_pruned = reclaimed_pruned + ? WHERE id = 1`,
			carried)
		if err != nil {
			return p, fmt.Errorf("store: prune carry reclaimed total: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return p, fmt.Errorf("store: prune carry rows affected: %w", err)
		}
		if n != 1 {
			return p, fmt.Errorf("store: prune carry reclaimed total: the ledger_totals row was not there to carry %d bytes into (%d rows updated); nothing was deleted", carried, n)
		}
	}

	dph := make([]string, len(ids))
	for i := range ids {
		dph[i] = "?"
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE rowid IN (`+strings.Join(dph, ", ")+`)`, ids...)
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

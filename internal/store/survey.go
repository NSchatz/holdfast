package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SurveyLedgerDecisionInputs answers "what was this ledger decided under" for a ledger ON
// DISK, without opening it the daemon's way and without needing it to share this build's
// schema. It is what `validate` reads through.
//
// Two properties have to hold at once, and neither may be traded for the other.
//
// It must NOT MIGRATE. A validate that upgraded an operator's store as a side effect of
// describing it would stamp a version the holdfast still running against that file then
// refuses to open (see OpenReadOnly). So the handle is mode=ro: SQLite itself refuses
// every write, and nothing is created where there is nothing to read.
//
// And it must ANSWER for the one ledger the question matters most about: the one the
// PREVIOUS build wrote. Every terminal row in such a file records no decision inputs, so
// the first scan after the upgrade re-opens the whole terminal set - which is precisely
// the population an operator running `validate` on upgrade day needs the figures for. A
// read that refused it would report nothing exactly where the report is load-bearing.
//
// Those reconcile because the counts do not need the column. A jobs table WITHOUT
// decision_inputs is by construction a ledger in which no row records any, so "0 moved, N
// recording none" is a statement the row counts alone support. The read therefore turns on
// the COLUMNS IT NEEDS rather than on the version stamp, which also means a later schema
// that keeps the column keeps working here.
//
// A ledger from the FUTURE is still a refusal, the same one both doors give: a shape this
// build cannot see all of is one whose rows it must not describe.
func SurveyLedgerDecisionInputs(ctx context.Context, path string, current DecisionInputs) (DecisionInputsSurvey, error) {
	var out DecisionInputsSurvey
	db, err := openReadOnlyDB(path)
	if err != nil {
		return out, err
	}
	defer func() { _ = db.Close() }()

	have, err := readSchemaVersion(ctx, db)
	if err != nil {
		return out, err
	}
	if want := schemaVersion(); have > want {
		return out, errSchemaFromTheFuture(have, want)
	}

	cols, err := jobsColumns(ctx, db)
	if err != nil {
		return out, err
	}
	if !cols["status"] {
		return out, fmt.Errorf("store: survey %q: it holds no jobs table to read", path)
	}
	where, args := surveyedRows(cols["reason"])

	if !cols["decision_inputs"] {
		// COUNTED, not decoded. There is no column to decode, and that absence is the
		// answer rather than an obstacle to it: every terminal row in this file was
		// written by a build that recorded nothing, so every one of them reads as not
		// recorded and the next scan will offer all of them to the guards again.
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE `+where, args...).Scan(&n); err != nil {
			return out, fmt.Errorf("store: survey %q: counting terminal rows: %w", path, err)
		}
		out.NotRecorded = n
		return out, nil
	}

	rows, err := db.QueryContext(ctx,
		`SELECT decision_inputs, COUNT(*) FROM jobs WHERE `+where+` GROUP BY decision_inputs`, args...)
	if err != nil {
		return out, fmt.Errorf("store: survey %q: %w", path, err)
	}
	defer func() { _ = rows.Close() }()
	return classifyRecordedInputs(rows, current)
}

// jobsColumns is the set of columns the jobs table actually has, which is what a read of a
// ledger this build may not share a schema with has to ask before it names one. It reads
// the shape rather than trusting the version stamp: the stamp says which migrations ran,
// and what a query needs is a column.
//
// An absent table yields an empty set rather than an error - PRAGMA table_info on a name
// that does not exist returns no rows - so the caller decides what that means.
func jobsColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('jobs')`)
	if err != nil {
		return nil, fmt.Errorf("store: read the jobs table's columns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: read the jobs table's columns: %w", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read the jobs table's columns: %w", err)
	}
	return cols, nil
}

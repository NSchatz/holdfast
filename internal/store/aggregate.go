package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// The whole-ledger aggregates (DASH-7).
//
// WHY THEY LIVE IN THE STORE. The dashboard's queue and history views ship at most
// queueLimit / historyLimit rows, and any figure the browser derives from that payload
// is a statistic about the most recent few hundred files while looking exactly like a
// statistic about the operator's library. On a 300,000-file library the difference is
// the whole meaning of the number. So every published aggregate is computed HERE, in
// SQL, over every matching row, and travels with the set it covers.
//
// WHY ONLY COUNT / MIN / AVG / MAX. A distributional figure wants a median or a
// percentile, and SQLite's median() and percentile() are gated on version 3.51.0 AND a
// build compiled with -DSQLITE_ENABLE_PERCENTILE (or a loadable extension before that)
// — see https://www.sqlite.org/lang_aggfunc.html. Naming one would produce a query that
// resolves on whatever build the author happened to have and fails at RUNTIME, on a
// user's image, inside the one surface whose job is to be trustworthy. The low/mean/high
// triple answers "what is the spread" with functions every SQLite build has, and
// TestAggregates_DoNotDependOnAVersionGatedSQLFunction pins that choice by probing this
// build for median() and asserting the aggregates work regardless of the answer.
//
// WHY EACH ONE CARRIES ITS OWN ERROR. Hub.buildSnapshot returns an error on any store
// read failure and broadcast then skips the frame entirely. Reading these on that same
// all-or-nothing path would let one unreadable figure blank the whole live page, so a
// failure is recorded in the aggregate it belongs to and rendered as "unavailable"
// beside a page that still draws.

// Aggregates is documented on the Store interface.
func (s *SQLite) Aggregates(ctx context.Context) Aggregates {
	return Aggregates{
		Outcomes:     s.outcomeCounts(ctx),
		SkipsByGuard: s.skipsByGuard(ctx),
		SizeRatio:    s.sizeRatio(ctx),
		EncodeMs:     s.encodeDuration(ctx),
		VmafMean:     s.vmafSpread(ctx, "vmaf_mean"),
		VmafMin:      s.vmafSpread(ctx, "vmaf_min"),
	}
}

// outcomeCounts counts terminal rows per status over the whole table. Excluded is
// always 0 here and that is a fact about the schema, not an omission: status is NOT
// NULL, so no row can be terminal without recording which terminal state it reached.
//
// The two FILESYSTEM-1 outcomes are counted HERE, in their own buckets, and are not
// folded into failed. This is a surface that reports job outcomes, so a parked job
// counted as "failed" would tell an operator the source is fine - which is precisely
// what nobody knows - and leaving it out of the set entirely would make the one job
// waiting for a human the only one this figure never sees.
func (s *SQLite) outcomeCounts(ctx context.Context) Breakdown {
	b := Breakdown{Coverage: Coverage{
		Set: "every terminal row in the ledger (done, skipped, failed, indeterminate, applied-despite-error)"}}
	buckets, absent, err := s.groupCount(ctx,
		`SELECT status, COUNT(*) FROM jobs WHERE status IN (?, ?, ?, ?, ?) GROUP BY status`,
		string(Done), string(Skipped), string(Failed), string(Indeterminate), string(AppliedDespiteError))
	if err != nil {
		b.Err = fmt.Errorf("store: aggregate outcome counts: %w", err)
		return b
	}
	b.Buckets = buckets
	b.Excluded = absent
	b.Counted = totalOf(buckets)
	return b
}

// skipsByGuard breaks every skipped row in the table down by the guard token that
// skipped it (internal/engine's Skip* constants — a closed vocabulary the API already
// ships per row as `reason`).
//
// A skipped row with no recorded reason is EXCLUDED and counted, not filed under an
// "unknown" guard: inventing a bucket for it would make a missing record look like a
// guard that exists.
func (s *SQLite) skipsByGuard(ctx context.Context) Breakdown {
	b := Breakdown{Coverage: Coverage{Set: "every skipped row in the ledger"}}
	buckets, absent, err := s.groupCount(ctx,
		`SELECT reason, COUNT(*) FROM jobs WHERE status = ? GROUP BY reason`, string(Skipped))
	if err != nil {
		b.Err = fmt.Errorf("store: aggregate skips by guard: %w", err)
		return b
	}
	b.Buckets = buckets
	b.Excluded = absent
	b.Counted = totalOf(buckets)
	return b
}

// sizeRatio is the spread of output_bytes / source_bytes over done rows — 0.35 meaning
// "the replacement is 35% of the original".
//
// A done row is excluded when it recorded no size pair at all (every row written before
// the outcome columns existed) and also when its source size is 0, because a ratio
// against 0 is undefined rather than infinite: a row that cannot contribute a value is
// counted as excluded, which is the honest report either way.
func (s *SQLite) sizeRatio(ctx context.Context) Spread {
	return s.spread(ctx, Coverage{Set: "every done row in the ledger"},
		"aggregate size ratio",
		`CASE WHEN source_bytes IS NOT NULL AND output_bytes IS NOT NULL AND source_bytes > 0
			THEN CAST(output_bytes AS REAL) / CAST(source_bytes AS REAL) END`,
		string(Done))
}

// encodeDuration is the spread of recorded encode wall-clock time, in milliseconds,
// over done rows. A done row that recorded none is excluded and counted.
func (s *SQLite) encodeDuration(ctx context.Context) Spread {
	return s.spread(ctx, Coverage{Set: "every done row in the ledger"},
		"aggregate encode duration", `CAST(encode_ms AS REAL)`, string(Done))
}

// vmafSpread is the spread of one pooled VMAF statistic over done rows. col is one of
// the two VMAF columns — a package-internal constant, never a caller's string.
//
// A done row with no score (the VMAF gate disabled, or a row older than the outcome
// columns) is EXCLUDED and counted. It must never be read as 0: a VMAF of 0.0 is a
// destroyed frame, so folding an unmeasured swap in as a zero would drag the published
// figure down with evidence nobody collected.
func (s *SQLite) vmafSpread(ctx context.Context, col string) Spread {
	return s.spread(ctx, Coverage{Set: "every done row in the ledger"},
		"aggregate "+col, col, string(Done))
}

// --- the two query shapes ------------------------------------------------------

// spread runs one numeric aggregate: expr is evaluated per matching row and the rows
// where it is NULL are the excluded ones.
//
// The subquery is what makes the two counts mean different things in one pass:
// COUNT(v) counts the rows that produced a value, COUNT(*) counts every matching row,
// and the difference is the exclusion count the criteria require. MIN/AVG/MAX over an
// all-NULL column return NULL, which becomes nil pointers — "no data", never a zero.
//
// expr and the status are package constants, never caller input; the status is bound as
// a parameter anyway.
func (s *SQLite) spread(ctx context.Context, cov Coverage, what, expr, status string) Spread {
	sp := Spread{Coverage: cov}
	q := `SELECT COUNT(v), MIN(v), AVG(v), MAX(v), COUNT(*) FROM
		(SELECT ` + expr + ` AS v FROM jobs WHERE status = ?)`

	var counted, matching int64
	var lo, mean, hi sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, q, status).Scan(&counted, &lo, &mean, &hi, &matching); err != nil {
		sp.Err = fmt.Errorf("store: %s: %w", what, err)
		return sp
	}
	sp.Counted = counted
	sp.Excluded = matching - counted
	if lo.Valid {
		v := lo.Float64
		sp.Min = &v
	}
	if mean.Valid {
		v := mean.Float64
		sp.Mean = &v
	}
	if hi.Valid {
		v := hi.Float64
		sp.Max = &v
	}
	return sp
}

// groupCount runs a two-column GROUP BY (key, count) and splits the NULL key out as the
// excluded count. Buckets come back ordered by count descending then key ascending, so
// the page's ordering is the store's and does not wobble between snapshots.
func (s *SQLite) groupCount(ctx context.Context, q string, args ...any) ([]Bucket, int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var buckets []Bucket
	var absent int64
	for rows.Next() {
		var key sql.NullString
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			return nil, 0, err
		}
		if !key.Valid || key.String == "" {
			absent += n
			continue
		}
		buckets = append(buckets, Bucket{Key: key.String, Count: n})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Count != buckets[j].Count {
			return buckets[i].Count > buckets[j].Count
		}
		return buckets[i].Key < buckets[j].Key
	})
	return buckets, absent, nil
}

func totalOf(buckets []Bucket) int64 {
	var n int64
	for _, b := range buckets {
		n += b.Count
	}
	return n
}

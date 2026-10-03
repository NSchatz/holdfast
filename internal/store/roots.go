package store

import (
	"context"
	"fmt"
)

// The per-root sizing reads (S0169): what `GET /api/summary` needs to say how big the job
// is, per library root, without an operator copying jobs.db off the host.
//
// Both reads here are READS and nothing else. Neither writes a jobs row, and neither
// touches a retention: a retained original is the only undo path a swap leaves, and a
// reporting read that released, restored or re-dated one would destroy it.

// liveRetention is the ONE spelling of "this retention is still holding its bytes": a row
// not restored (a released one is deleted outright, so it is not in the table at all).
//
// It is a constant, and every read of the live set is built from it - the gauge's sum
// (HeldByUndoWindow), the per-source read below and ListRetained - so the total and its
// per-root split cannot come to mean two different sets by being written twice.
const liveRetention = `restored_at IS NULL`

// HeldSource is one live retention as the held-bytes figure counts it: the path the
// original was swapped out of, and the bytes its retained link is still holding.
type HeldSource struct {
	SourcePath  string
	SourceBytes int64
}

// RootTotal is one RECORDED library root's sizing figures, over the rows that recorded it.
//
// LibraryRoot is the cleaned root a row recorded (Decision.LibraryRoot), and "" for the
// rows that recorded none - every row written before the column existed. It is what the
// ledger says and not what the configuration says now: a caller attributes it to a
// configured root, or to none.
//
// The candidate figures are over `would-transcode` rows, the decisions a dry run recorded.
// A row that recorded no source size is COUNTED in CandidateExcluded and summed nowhere: an
// absent size is not a size of zero.
//
// The basis figures are over `done` rows that recorded BOTH sizes, the only rows a
// projection can be measured on: BasisSourceBytes is the sum of their source sizes and
// BasisSavedBytes the sum of source minus output.
type RootTotal struct {
	LibraryRoot string

	CandidateFiles    int64
	CandidateExcluded int64
	CandidateBytes    int64

	BasisFiles       int64
	BasisSourceBytes int64
	BasisSavedBytes  int64
}

// RootTotals is documented on the Store interface.
//
// One query, grouped by the recorded root, so no root's figure can disagree with another's
// by being computed a second way. SQLite's SUM over integers raises an error on overflow
// rather than wrapping, so a sum this cannot hold is a failed read and never a wrong number.
func (s *SQLite) RootTotals(ctx context.Context) ([]RootTotal, error) {
	const (
		candidate = `status = ?1`
		basis     = `status = ?2 AND source_bytes IS NOT NULL AND output_bytes IS NOT NULL`
	)
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(library_root, '') AS root,
			COUNT(CASE WHEN `+candidate+` AND source_bytes IS NOT NULL THEN 1 END),
			COUNT(CASE WHEN `+candidate+` AND source_bytes IS NULL THEN 1 END),
			COALESCE(SUM(CASE WHEN `+candidate+` THEN source_bytes END), 0),
			COUNT(CASE WHEN `+basis+` THEN 1 END),
			COALESCE(SUM(CASE WHEN `+basis+` THEN source_bytes END), 0),
			COALESCE(SUM(CASE WHEN `+basis+` THEN source_bytes - output_bytes END), 0)
		 FROM jobs WHERE status IN (?1, ?2)
		 GROUP BY root ORDER BY root`,
		string(WouldTranscode), string(Done))
	if err != nil {
		return nil, fmt.Errorf("store: root totals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []RootTotal{}
	for rows.Next() {
		var t RootTotal
		if err := rows.Scan(&t.LibraryRoot, &t.CandidateFiles, &t.CandidateExcluded, &t.CandidateBytes,
			&t.BasisFiles, &t.BasisSourceBytes, &t.BasisSavedBytes); err != nil {
			return nil, fmt.Errorf("store: root totals scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: root totals rows: %w", err)
	}
	return out, nil
}

// HeldBySource is documented on the Store interface. It selects on liveRetention, the
// predicate HeldByUndoWindow sums over, so the rows it returns are exactly the rows that
// figure counts.
func (s *SQLite) HeldBySource(ctx context.Context) ([]HeldSource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_path, source_bytes FROM retained_originals
		 WHERE `+liveRetention+` ORDER BY source_path ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: held by source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []HeldSource{}
	for rows.Next() {
		var h HeldSource
		if err := rows.Scan(&h.SourcePath, &h.SourceBytes); err != nil {
			return nil, fmt.Errorf("store: held by source scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: held by source rows: %w", err)
	}
	return out, nil
}

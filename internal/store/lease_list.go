package store

import (
	"context"
	"fmt"
)

// The lease ledger's REPORTING reads (GET /api/nodes). They are reads and nothing else: no
// transaction, no decision, no row written, so listing the leases can never grant, end,
// adopt or re-open one.

// leasesCoverage is the set both reads are over, stated once so the listing and its total
// cannot describe different sets.
const leasesCoverage = "every lease in the ledger"

// recentLeasesOrder is newest grant first. granted_at is in whole seconds, so several
// leases share one; path and then epoch break the tie, and (path, epoch) is unique, so the
// order is total and two reads of an unchanged ledger return one sequence.
const recentLeasesOrder = `ORDER BY granted_at DESC, path ASC, epoch DESC LIMIT ?`

// RecentLeases is documented on LeaseLedger.
func (s *SQLite) RecentLeases(ctx context.Context, limit int) ([]Lease, error) {
	if limit < 1 {
		return nil, fmt.Errorf("store: recent leases: a listing of %d rows is not a listing", limit)
	}
	out, err := queryLeases(ctx, s.db, recentLeasesOrder, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read recent leases: %w", err)
	}
	return out, nil
}

// CountLeases is documented on LeaseLedger.
func (s *SQLite) CountLeases(ctx context.Context) RowTotal {
	t := RowTotal{Coverage: Coverage{Set: leasesCoverage}}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_leases`).Scan(&t.Count); err != nil {
		t.Count = 0
		t.Err = fmt.Errorf("store: count leases: %w", err)
	}
	return t
}

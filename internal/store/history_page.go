package store

import (
	"context"
	"fmt"
	"strings"
)

// The paged ledger read (S0170).
//
// List answers "the newest N rows" and nothing after them, so a ledger longer than one
// response could only be read whole by copying the database out. ListPage reads it one
// bounded page at a time, and what makes that honest is the ORDER: a page boundary falls
// between two rows, so the order has to say, for every pair of rows, which comes first.
//
// `updated_at DESC, path ASC` does not. updated_at is unix SECONDS, so rows sharing a second
// are the common case, and the primary key is (path, fingerprint), so two rows can share a
// path as well. The fingerprint is therefore the final tie-break: (updated_at, path,
// fingerprint) is unique per row, the order over it is total, and "strictly after this
// position" then names an exact set of rows - none of them on both sides of a boundary, and
// none on neither.
//
// The position is a VALUE, not a row. A page continues from the place a row stood, whether
// or not that row is still there, so a row deleted between two reads costs the traversal
// nothing but that row.

// PagePosition is one row's place in the paged order: the three columns the order is over.
type PagePosition struct {
	UpdatedAt   int64
	Path        string
	Fingerprint string
}

// PositionOf is the place j stands in the paged order.
func PositionOf(j Job) PagePosition {
	return PagePosition{UpdatedAt: j.UpdatedAt, Path: j.Path, Fingerprint: j.Fingerprint}
}

// pageOrder is the total order, and pageAfter the rows strictly after one position in it.
// They are written side by side because each is the other's meaning: a comparison that
// disagreed with the ORDER BY would drop or repeat rows at exactly the boundaries the
// order exists to decide.
const (
	pageOrder = ` ORDER BY updated_at DESC, path ASC, fingerprint ASC`
	pageAfter = `(updated_at < ? OR (updated_at = ? AND (path > ? OR (path = ? AND fingerprint > ?))))`
)

// ListPage is documented on the Store interface.
//
// It reads one row MORE than it returns: that row is the answer to "does anything follow",
// taken in the same statement as the page, so the last full page of a set is never followed
// by a promise of a page that turns out empty. The statuses are expanded to a placeholder
// list exactly as List does, so a Status can never be interpolated into SQL text.
func (s *SQLite) ListPage(ctx context.Context, statuses []Status, after *PagePosition, limit int) ([]Job, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: list page: a page of %d rows is not a page", limit)
	}
	var where []string
	args := make([]any, 0, len(statuses)+6)
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "status IN ("+strings.Join(ph, ", ")+")")
	}
	if after != nil {
		where = append(where, pageAfter)
		args = append(args, after.UpdatedAt, after.UpdatedAt, after.Path, after.Path, after.Fingerprint)
	}
	q := `SELECT ` + jobColumns + ` FROM jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += pageOrder + ` LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: list page: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Job, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			more = true
			break
		}
		j, err := scanJob(rows)
		if err != nil {
			return nil, false, fmt.Errorf("store: list page scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: list page rows: %w", err)
	}
	return out, more, nil
}

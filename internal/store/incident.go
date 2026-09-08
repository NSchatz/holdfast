package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The swap-incident half of the SQLite store: the durable record of a swap that did
// not complete cleanly, and the operator's way out of the state it creates.
//
// Read the whole file with one rule in mind: a record here is the ONLY thing standing
// between a file holdfast wrote and a later run treating it as somebody's source. That
// is why the writes are transactional, why a disposition is mandatory, and why a
// replacement path can never be recorded kept-in-place.

// ErrNotParked is returned by ResolveIncident when the id names no incident, or one
// that is not parked - already resolved, or never indeterminate in the first place.
// The caller reports it DISTINCTLY from "the recorded file is not there", because they
// are opposite situations: one is "there is nothing to resolve", the other is "there is
// something to resolve and one of its files has gone".
var ErrNotParked = errors.New("store: no parked job with that id")

// RecordSwapIncident is documented on the Store interface.
//
// The incident row and the job row move together, in one transaction, deliberately.
// Two separate writes could leave a job sitting in "indeterminate" with no record of
// which two files it is about (unreportable, unresolvable), or an incident with no job
// state behind it (a hold-back nothing explains). Either half alone is worse than
// neither, so the caller gets exactly one answer: it landed, or nothing did.
func (s *SQLite) RecordSwapIncident(ctx context.Context, in SwapIncident) error {
	if in.Outcome != Indeterminate && in.Outcome != AppliedDespiteError {
		return fmt.Errorf("store: record swap incident: outcome %q is not one this record can carry", in.Outcome)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record swap incident begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO swap_incidents (
			source_path, source_fingerprint, replacement_path,
			source_attrs, replacement_attrs, observed_attrs,
			outcome, swap_error, swap_cause, storage_class, storage_type, created_at,
			disposition_replacement)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.SourcePath, in.SourceFingerprint, in.ReplacementPath,
		in.SourceAttrs, in.ReplacementAttrs, nullString(in.ObservedAttrs),
		string(in.Outcome), nullString(in.SwapError), nullString(in.SwapCause),
		nullString(in.StorageClass), nullString(in.StorageType), now(),
		nullString(string(initialReplacementDisposition(in.Outcome))),
	); err != nil {
		return fmt.Errorf("store: record swap incident insert: %w", err)
	}

	o := &Outcome{}
	if in.JobOutcome != nil {
		copy := *in.JobOutcome
		o = &copy
	}
	// The rename's error and its distinct cause belong on the job row too: an operator
	// reading the ledger must not have to join to the incident table to learn WHY the
	// swap did not complete.
	o.Reason, o.SwapCause = in.SwapError, in.SwapCause
	if _, err := tx.ExecContext(ctx, finishQuery(in.Outcome),
		finishArgs(in.Outcome, o, in.SourcePath, in.SourceFingerprint)...); err != nil {
		return fmt.Errorf("store: record swap incident job state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: record swap incident commit: %w", err)
	}
	return nil
}

// initialReplacementDisposition is the disposition an incident is BORN with.
//
// A parked (indeterminate) job gets none: nobody has decided anything yet, and NULL is
// what keeps the replacement path excluded from enumeration until somebody does.
//
// An applied-despite-error job gets Absent, and that is not a shortcut. Reaching that
// outcome REQUIRES that no file remains at the recorded replacement path - a rename
// that took effect cannot account for two files, so if one is still there the outcome
// is indeterminate instead and the pair is parked. "Absent" is therefore an established
// fact at the moment the record is written, not an assumption, and recording it is what
// stops a path with nothing at it holding back a scan forever.
func initialReplacementDisposition(outcome Status) Disposition {
	if outcome == AppliedDespiteError {
		return Absent
	}
	return ""
}

const incidentColumns = `id, source_path, source_fingerprint, replacement_path,
	source_attrs, replacement_attrs, observed_attrs,
	outcome, swap_error, swap_cause, storage_class, storage_type, created_at,
	resolution, resolved_by, resolved_at, observed_source, observed_replacement,
	disposition_source, disposition_replacement, removal_error`

func scanIncident(sc interface{ Scan(...any) error }) (SwapIncident, error) {
	var in SwapIncident
	var observed, swapErr, swapCause, storageClass, storageType sql.NullString
	var resolution, resolvedBy, obsSrc, obsRepl, dispSrc, dispRepl, removalErr sql.NullString
	var resolvedAt sql.NullInt64
	var outcome string
	if err := sc.Scan(&in.ID, &in.SourcePath, &in.SourceFingerprint, &in.ReplacementPath,
		&in.SourceAttrs, &in.ReplacementAttrs, &observed,
		&outcome, &swapErr, &swapCause, &storageClass, &storageType, &in.CreatedAt,
		&resolution, &resolvedBy, &resolvedAt, &obsSrc, &obsRepl,
		&dispSrc, &dispRepl, &removalErr); err != nil {
		return SwapIncident{}, err
	}
	in.ObservedAttrs = observed.String
	in.Outcome = Status(outcome)
	in.SwapError, in.SwapCause = swapErr.String, swapCause.String
	in.StorageClass, in.StorageType = storageClass.String, storageType.String
	in.Resolution = Determination(resolution.String)
	in.ResolvedBy, in.ResolvedAt = resolvedBy.String, resolvedAt.Int64
	in.ObservedAtResolutionSource, in.ObservedAtResolutionReplacement = obsSrc.String, obsRepl.String
	in.DispositionSource, in.DispositionReplacement = Disposition(dispSrc.String), Disposition(dispRepl.String)
	in.RemovalError = removalErr.String
	return in, nil
}

// ParkedIncidents is documented on the Store interface.
func (s *SQLite) ParkedIncidents(ctx context.Context) ([]SwapIncident, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+incidentColumns+` FROM swap_incidents
		 WHERE outcome = ? AND resolution IS NULL ORDER BY id ASC`, string(Indeterminate))
	if err != nil {
		return nil, fmt.Errorf("store: parked incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SwapIncident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("store: parked incidents scan: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: parked incidents rows: %w", err)
	}
	return out, nil
}

// IncidentByID is documented on the Store interface.
func (s *SQLite) IncidentByID(ctx context.Context, id int64) (SwapIncident, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM swap_incidents WHERE id = ?`, id)
	in, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SwapIncident{}, false, nil
	}
	if err != nil {
		return SwapIncident{}, false, fmt.Errorf("store: incident by id: %w", err)
	}
	return in, true, nil
}

// ExcludedReplacementPaths is documented on the Store interface. The predicate is the
// criterion word for word: a recorded replacement path is excluded for as long as the
// record exists and its disposition is NOT deleted and NOT absent. Note what it does
// NOT say - nothing about the parked state - because a replacement retained through a
// resolution is still a file holdfast wrote.
func (s *SQLite) ExcludedReplacementPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT replacement_path FROM swap_incidents
		 WHERE disposition_replacement IS NULL
		    OR disposition_replacement NOT IN (?, ?)`, string(Deleted), string(Absent))
	if err != nil {
		return nil, fmt.Errorf("store: excluded replacement paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: excluded replacement paths scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: excluded replacement paths rows: %w", err)
	}
	return out, nil
}

// ResolveIncident is documented on the Store interface.
//
// Two refusals are enforced HERE rather than trusted to callers, because they are the
// two ways a resolution could quietly undo the thing this whole record exists for:
//
//  1. A resolution missing a disposition for EITHER path is refused. An unstated
//     disposition is precisely the state in which nobody can say whether a file
//     holdfast wrote is still out there.
//  2. A recorded REPLACEMENT path may never be kept-in-place, under either
//     determination. kept-in-place means "later runs treat that path normally", and
//     handing a gate-passed encode back to enumeration as if it were somebody's source
//     is how a library acquires a permanent duplicate.
//
// The jobs row for (source path, source fingerprint) is DELETED in the same
// transaction. That is what releases the job: a later run finds no row, so the source
// path is new work under the ordinary rules - encoded and swapped again if they select
// it, skipped by the ordinary already-at-target-codec guard if the file there is now
// the replacement. It is a NEW job, never a re-park of this one.
func (s *SQLite) ResolveIncident(ctx context.Context, id int64, r Resolution) error {
	if !r.Determination.Valid() {
		return fmt.Errorf("store: resolve incident: %q is not a determination", r.Determination)
	}
	if !r.DispositionSource.Valid() {
		return fmt.Errorf("store: resolve incident: the source path has no disposition")
	}
	if !r.DispositionReplacement.Valid() {
		return fmt.Errorf("store: resolve incident: the replacement path has no disposition")
	}
	if r.DispositionReplacement == KeptInPlace {
		return fmt.Errorf("store: resolve incident: a recorded replacement path is never kept-in-place " +
			"(a file holdfast wrote is never handed back to enumeration)")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: resolve incident begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sourcePath, sourceFingerprint, outcome string
	var resolution sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT source_path, source_fingerprint, outcome, resolution FROM swap_incidents WHERE id = ?`, id).
		Scan(&sourcePath, &sourceFingerprint, &outcome, &resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotParked
	}
	if err != nil {
		return fmt.Errorf("store: resolve incident select: %w", err)
	}
	if Status(outcome) != Indeterminate || resolution.Valid {
		return ErrNotParked
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE swap_incidents SET resolution = ?, resolved_by = ?, resolved_at = ?,
			observed_source = ?, observed_replacement = ?,
			disposition_source = ?, disposition_replacement = ?, removal_error = ?
		 WHERE id = ?`,
		string(r.Determination), nullString(r.By), now(),
		nullString(r.ObservedSource), nullString(r.ObservedReplacement),
		string(r.DispositionSource), string(r.DispositionReplacement),
		nullString(r.RemovalError), id); err != nil {
		return fmt.Errorf("store: resolve incident update: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM jobs WHERE path = ? AND fingerprint = ?`, sourcePath, sourceFingerprint); err != nil {
		return fmt.Errorf("store: resolve incident release job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: resolve incident commit: %w", err)
	}
	return nil
}

// AmendReplacementDisposition is documented on the Store interface.
func (s *SQLite) AmendReplacementDisposition(ctx context.Context, id int64, d Disposition, removalErr string) error {
	if !d.Valid() || d == KeptInPlace {
		return fmt.Errorf("store: amend replacement disposition: %q is not a legal replacement disposition", d)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE swap_incidents SET disposition_replacement = ?, removal_error = ? WHERE id = ?`,
		string(d), nullString(removalErr), id)
	if err != nil {
		return fmt.Errorf("store: amend replacement disposition: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: amend replacement disposition rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotParked
	}
	return nil
}

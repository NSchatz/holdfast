package main

import (
	"errors"
	"log/slog"

	"github.com/NSchatz/holdfast/internal/store"
)

// What the daemon says about a migration it just ran.
//
// The store counts every table's rows on either side of every step it applies and returns
// that record to whoever opened it. This is the half that reaches a human: one structured
// record per step, emitted where the daemon opens the ledger, so an operator upgrading a
// live install can see what the upgrade did to the evidence their library's history is
// kept in rather than infer it from the fact that nothing crashed.
//
// Two rules govern the emission, and both are the observability conventions applied
// literally:
//
//   - It is a RECORD, never a sentence. Every field is an attribute, so a log pipeline can
//     select on the step, the version or a table's counts. The message string is one field
//     among them.
//   - Levels mean what they say. A step that applied is `info`: it is what happened, and a
//     process that logs at `error` for a handled condition trains its reader to ignore it.
//     `error` is reserved for the one case where a human must act - a step rolled back for
//     moving rows it never declared, which is the store refusing to open at all.

// migrationComponent names the emitter on every record here, so a line can be joined to
// the part of the daemon that produced it.
const migrationComponent = "store.migrate"

// reportMigrations emits one record per step the open applied. An open that applied
// nothing emits nothing: there is no event, and a line saying so would be noise on every
// restart of a daemon that migrates once in its life.
func reportMigrations(log *slog.Logger, applied []store.MigrationStep) {
	for _, step := range applied {
		log.Info("migration step applied",
			"component", migrationComponent,
			"step", step.Version,
			"step_name", step.Name,
			"stamped_schema_version", step.Version,
			slog.Group("rows", rowAttrs(step)...))
	}
}

// reportMigrationRefusal emits the record of a step the store ROLLED BACK because it moved
// rows it had not declared, and reports whether err was that refusal.
//
// It is the one migration record at `error`, and it earns the level: the daemon is not
// starting, the ledger is at the version it had before the step, and what happens next is
// an operator's decision. Every other reason an open can fail is reported by the caller in
// its own words - this one gets a record because the numbers are the finding.
func reportMigrationRefusal(log *slog.Logger, err error) bool {
	var refused *store.UndeclaredRowChangeError
	if !errors.As(err, &refused) {
		return false
	}
	log.Error("migration step refused: it moved rows it did not declare",
		"component", migrationComponent,
		"step", refused.Version,
		"step_name", refused.Step,
		"table", refused.Table,
		"rows_before", refused.Before,
		"rows_after", refused.After,
		"rows_changed", refused.Observed,
		"rows_declared", refused.Declared)
	return true
}

// rowAttrs renders one step's per-table counts as attributes: a group per table, each
// carrying the count before the step and the count after it. A table is a key rather than
// a value in a list so that "how many rows did jobs have after step 13" is a selectable
// field rather than something a reader has to parse out of a rendered string.
func rowAttrs(step store.MigrationStep) []any {
	attrs := make([]any, 0, len(step.Tables))
	for _, t := range step.Tables {
		attrs = append(attrs, slog.Group(t.Table, "before", t.Before, "after", t.After))
	}
	return attrs
}

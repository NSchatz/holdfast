package store

import (
	"fmt"
	"strconv"
)

// SchemaStamp is what one record says about the schema version that WROTE it.
//
// It exists because a reader holding a row could not tell which build's semantics filled
// it. The answer was inferred from absent fields, one column at a time, and that
// inference is a coincidence rather than a fact: every nullable column is also
// legitimately empty on a row the current build wrote.
//
// THREE states, and keeping them apart is the whole of it:
//
//   - NOT RECORDED. The record was written before the stamp column existed. This is the
//     zero value, deliberately - a record nobody stamped must read as unstamped whatever
//     path it arrives through, and it is never presented as a schema version, because
//     naming one would be inventing evidence about which build wrote it.
//   - RECORDED. The value is a whole number inside the schema history this build knows,
//     so it names a step that really shipped.
//   - UNRECOGNISED. Something is stored there and it is not one of those versions: a
//     number past the end of this build's history (a record written by a newer holdfast,
//     read from a database an operator restored), a zero or a negative, or a value that
//     is not a number at all. It is reported as unrecognised and never as this build's
//     version, and the rest of the record still reads - refusing the read would make one
//     unreadable field cost the evidence in every other one.
//
// A REAL cannot reach the unrecognised state by being whole: the column has INTEGER
// affinity, so SQLite converts a losslessly-integral value on the way in. A fractional
// one stays a REAL and is unrecognised, which is what it is.
type SchemaStamp struct {
	version    int
	recorded   bool
	recognised bool
	raw        string
}

// Recorded reports whether the record carries a stamp at all. False is a record written
// before the stamp existed.
func (s SchemaStamp) Recorded() bool { return s.recorded }

// Recognised reports whether what the record carries is a schema version this build
// knows. It is false for an unstamped record too: there is nothing there to recognise.
func (s SchemaStamp) Recognised() bool { return s.recognised }

// Version returns the schema version that wrote this record, and whether that version is
// one this build recognises. The int is ZERO whenever ok is false, so a caller that
// ignores ok cannot read an unstamped or unrecognised record as a version.
func (s SchemaStamp) Version() (int, bool) {
	if !s.recognised {
		return 0, false
	}
	return s.version, true
}

// Raw is the stored value as text, for a stamp this build does not recognise. It is ""
// for a recognised one and for a record that carries none, so it is only ever the thing
// that needs quoting back to a human.
func (s SchemaStamp) Raw() string { return s.raw }

// String renders the stamp for a log line or an operator's screen. The unstamped case
// says what it means rather than printing a bare zero, which is the one rendering that
// would read as a version.
func (s SchemaStamp) String() string {
	switch {
	case !s.recorded:
		return "not recorded (written before the version stamp existed)"
	case !s.recognised:
		return fmt.Sprintf("unrecognised (%q)", s.raw)
	default:
		return strconv.Itoa(s.version)
	}
}

// currentStamp is the value every write puts in the column: the schema version in force,
// which is the version this build's history ends at and the version stamped in the
// database header after a successful open. There is no second source for it, so a
// record's stamp and the header can never disagree about what wrote the record.
func currentStamp() int { return schemaVersion() }

// parseSchemaStamp reads the column as the driver hands it over: nil for a record written
// before the column existed, an int64 for a stamp, and anything else for a value that got
// there some other way (an operator's repair script, a newer build, a hand-edited file).
//
// The recognition test is the schema history itself - 1 through the number of steps this
// build carries - so it needs no list to maintain and cannot drift from the migrations.
func parseSchemaStamp(raw any) SchemaStamp {
	switch v := raw.(type) {
	case nil:
		return SchemaStamp{}
	case int64:
		if v >= 1 && v <= int64(schemaVersion()) {
			return SchemaStamp{version: int(v), recorded: true, recognised: true}
		}
		return SchemaStamp{recorded: true, raw: strconv.FormatInt(v, 10)}
	case []byte:
		return SchemaStamp{recorded: true, raw: string(v)}
	case string:
		return SchemaStamp{recorded: true, raw: v}
	default:
		return SchemaStamp{recorded: true, raw: fmt.Sprintf("%v", v)}
	}
}

// stampScan holds a record's raw stamp on the way out of the driver. It is a bare `any`
// because the COLUMN is what is being read rather than a value this package wrote: a
// typed destination would fail the scan on a value it did not expect, and one unreadable
// field must not cost the whole record (see SchemaStamp's unrecognised state).
type stampScan struct{ raw any }

// dest is the scan destination for the stamp column.
func (s *stampScan) dest() any { return &s.raw }

// stamp is what the scanned value means.
func (s *stampScan) stamp() SchemaStamp { return parseSchemaStamp(s.raw) }

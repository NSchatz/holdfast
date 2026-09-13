package store

import (
	"fmt"
	"strconv"
)

// SchemaStamp is what one record says about the schema version that WROTE it. Without it
// a reader infers that from which fields are empty, which is a coincidence and not a fact:
// every nullable column is also legitimately empty on a row this build wrote.
//
// THREE states, and keeping them apart is the whole of it:
//
//   - NOT RECORDED, the zero value: written before the stamp column existed. Never
//     presented as a schema version, because naming one invents evidence about which build
//     wrote the record.
//   - RECORDED: a whole number inside the schema history this build knows.
//   - UNRECOGNISED: anything else in the column - a version past the end of this build's
//     history, a zero or a negative, or a value that is not a number. Never read as this
//     build's version, and the rest of the record still reads, because one unreadable
//     field must not cost the evidence in every other one.
type SchemaStamp struct {
	version    int
	recorded   bool
	recognised bool
	raw        string
}

// Recorded reports whether the record carries a stamp at all.
func (s SchemaStamp) Recorded() bool { return s.recorded }

// Recognised reports whether what it carries is a version this build knows. False for an
// unstamped record too: there is nothing there to recognise.
func (s SchemaStamp) Recognised() bool { return s.recognised }

// Version returns the schema version that wrote this record and whether this build
// recognises it. The int is ZERO whenever ok is false, so a caller that ignores ok cannot
// read an unstamped or unrecognised record as a version.
func (s SchemaStamp) Version() (int, bool) {
	if !s.recognised {
		return 0, false
	}
	return s.version, true
}

// Raw is the stored value as text, and "" unless the stamp is unrecognised: it is only
// ever the thing that needs quoting back to a human.
func (s SchemaStamp) Raw() string { return s.raw }

// String renders the stamp for a log line or an operator's screen. The unstamped case says
// what it means rather than printing a bare zero, the one rendering that reads as a version.
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
// read from the migration history itself, so a record's stamp and the version in the
// database header can never disagree about what wrote the record.
func currentStamp() int { return schemaVersion() }

// parseSchemaStamp reads the column as the driver hands it over: nil for a record written
// before the column existed, an int64 for a stamp, anything else for a value that got there
// some other way (a repair script, a newer build, a hand-edited file). The recognition test
// is the history itself, so it cannot drift from the migrations.
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

// stampScan holds a record's raw stamp on the way out of the driver. A bare `any` because
// the COLUMN is what is being read rather than a value this package wrote: a typed
// destination would fail the scan on a value it did not expect, and one unreadable field
// must not cost the whole record.
type stampScan struct{ raw any }

func (s *stampScan) dest() any          { return &s.raw }
func (s *stampScan) stamp() SchemaStamp { return parseSchemaStamp(s.raw) }

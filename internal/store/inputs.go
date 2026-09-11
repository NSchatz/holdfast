package store

import (
	"net/url"
	"sort"
	"strings"
)

// DecisionInputs is what one terminal decision READ from the configuration, recorded on
// the row that decision wrote.
//
// It exists because a terminal row is a permanent exclusion keyed on path+fingerprint
// (see Claim) while the values that produced it are ordinary editable YAML keys. Lower
// min_bitrate_kbps and the files skipped `low-bitrate` stay skipped; move `encoder` to a
// different target codec and every file skipped `already-at-target-codec` stays skipped.
// The tool then runs, reports a green queue, and does nothing. Recording the inputs is
// what makes that re-derivable: a row whose recorded values have moved is offered to the
// pipeline again, and a row whose values still hold is not.
//
// It is the values the decision ACTUALLY READ and no others, never a digest or a copy of
// the whole configuration. A digest would tie every row to every key, so correcting a
// notification URL would re-open a library's worth of files that no guard would decide
// differently - which is not a re-derivation, it is a re-scan of everything, and an
// operator would learn to distrust it.
//
// Absence is REPRESENTABLE and is not an empty set. A row written before the column
// existed recorded nothing, and "nothing recorded" must never read as "read no
// configuration" (which always matches) or as a set of empty strings (which never
// does). The two are distinct states here and stay distinct all the way into the
// column, where "not recorded" is NULL - see Recorded.
type DecisionInputs struct {
	read     map[string]string
	recorded bool
}

// InputsRead records read as the values a decision consulted. An EMPTY map is a real
// record and not an absence: a guard that consults no configuration at all - interlaced,
// Dolby Vision, a symlinked source - has a verdict no configuration change can move, and
// saying so is what stops the next scan re-opening that row for ever.
func InputsRead(read map[string]string) DecisionInputs {
	copied := make(map[string]string, len(read))
	for k, v := range read {
		copied[k] = v
	}
	return DecisionInputs{read: copied, recorded: true}
}

// Recorded reports whether this row recorded anything at all. The zero DecisionInputs is
// NOT RECORDED, which is what every row written before the column existed carries.
func (d DecisionInputs) Recorded() bool { return d.recorded }

// Value returns the value recorded for key, and whether it was recorded.
func (d DecisionInputs) Value(key string) (string, bool) {
	v, ok := d.read[key]
	return v, ok
}

// Keys returns the keys this record carries, sorted. It is what an assertion that a
// decision recorded the inputs its guard read AND NO OTHERS is made against.
func (d DecisionInputs) Keys() []string {
	keys := make([]string, 0, len(d.read))
	for k := range d.read {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// StillMatches reports whether everything d recorded is still what current holds.
//
// Three answers and each is a deliberate direction:
//
//   - A record of nothing NEVER matches. It cannot be re-derived, so it is re-opened
//     once and the decision it then reaches records what that decision read - after
//     which the scan after that finds a record and leaves the row alone.
//   - A record of an EMPTY set always matches. The decision read no configuration, so
//     no configuration change can move it.
//   - A key current does not offer never matches. Only a hand-edited row or a build that
//     no longer reads that key can produce one, and both are "this verdict cannot be
//     re-derived from the configuration in front of me", which is the same case as a
//     record of nothing.
func (d DecisionInputs) StillMatches(current DecisionInputs) bool {
	if !d.recorded {
		return false
	}
	for k, v := range d.read {
		cur, ok := current.read[k]
		if !ok || cur != v {
			return false
		}
	}
	return true
}

// noInputsRead is what a decision that consulted NO configuration value stores. It has
// to be a non-empty token because "" is how this package spells NULL in every other
// column (nullString), so an empty encoding would land in the database as "not
// recorded" and re-open the row on every scan for ever. It can never collide with a real
// encoding: every encoded pair carries a "=", and this carries none.
const noInputsRead = "none"

// Encode renders the record as the single TEXT value the column holds: the recorded
// pairs, key-sorted and ";"-joined, or the empty string when nothing was recorded (which
// nullString then stores as NULL).
//
// Sorted, because the value is COMPARED as a whole by the survey the startup report
// reads (one GROUP BY over distinct values rather than a scan that decodes every row),
// and a map's iteration order would make two identical records two different strings.
//
// Both halves of every pair are escaped. A value here is whatever the operator put in
// their YAML - a preset name, a container extension - and an unescaped ";" or "=" in one
// would silently split into a pair that was never recorded. QueryEscape leaves every
// value this build actually writes (hevc, cpu, 24, slow, auto, mkv, 2500) untouched, so
// the stored text stays something an operator can read.
func (d DecisionInputs) Encode() string {
	if !d.recorded {
		return ""
	}
	if len(d.read) == 0 {
		return noInputsRead
	}
	pairs := make([]string, 0, len(d.read))
	for _, k := range d.Keys() {
		pairs = append(pairs, url.QueryEscape(k)+"="+url.QueryEscape(d.read[k]))
	}
	return strings.Join(pairs, ";")
}

// ParseDecisionInputs is Encode's inverse: the column's text back into a record.
//
// Anything it cannot read is NOT RECORDED, which is the fail-safe direction: the row is
// re-opened once, the guards run again, and the decision they reach records something
// this build can read. Treating an unreadable value as a match would do the opposite -
// hold a file out of the pipeline on the strength of a record nothing can interpret.
func ParseDecisionInputs(s string) DecisionInputs {
	if s == "" {
		return DecisionInputs{}
	}
	if s == noInputsRead {
		return InputsRead(nil)
	}
	read := make(map[string]string)
	for _, pair := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return DecisionInputs{}
		}
		key, err := url.QueryUnescape(k)
		if err != nil {
			return DecisionInputs{}
		}
		val, err := url.QueryUnescape(v)
		if err != nil {
			return DecisionInputs{}
		}
		read[key] = val
	}
	return InputsRead(read)
}

// DecisionInputsSurvey is what the ledger says about the configuration its terminal
// decisions were taken under, over the done and skipped rows - the two statuses whose
// rows a configuration change may re-open.
//
// Moved is how many record inputs that differ from the configuration in front of them;
// NotRecorded is how many record nothing at all. Both are files the next scan will offer
// to the pipeline again, so a run announces them before the scan rather than leaving an
// operator to discover a burst of activity and guess at its cause.
type DecisionInputsSurvey struct {
	Moved       int64
	NotRecorded int64
	Matching    int64
}

// Reopening is how many rows the next scan will re-open.
func (s DecisionInputsSurvey) Reopening() int64 { return s.Moved + s.NotRecorded }

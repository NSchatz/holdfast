package docscheck

import (
	"fmt"
	"strings"
)

// The shipped documentation's statements about HOW A SCAN ENUMERATES, checked mechanically
// over the corpus - a documentation obligation nothing enforces is one that quietly lapses.
//
// There are two of them and they are checked separately because they lapse separately.
//
// # The order (S0099 AC-3)
//
// The scan hands files to its workers in an order, and a later decision - a declared queue
// order, largest first or newest first - is layered on top of that one. A rule nobody wrote
// down is a rule the next change is free to break without noticing, and the way it would be
// noticed is a library that re-encodes in a different sequence on every pass, which looks
// like nothing at all until somebody is relying on the sequence.
//
// # The figures (S0099 AC-9, AC-10)
//
// The peak heap a scan holds and the time it takes to reach its first worker are MEASURED,
// and a measurement without its machine, its build and its date is a number rather than a
// figure (performance PB5): nothing can be compared with it, including a later measurement
// of the same thing. So the check is that the figures are written down AND that they carry
// what makes them mean something. It is presence and nothing more - whether the numbers are
// still TRUE is not a question any mechanical check can answer, and one that pretended to
// would be a check nobody could trust either way.

const (
	// EnumerationDocFile is where both statements live today. It is named so a failure
	// points somewhere, and neither check is narrowed to it: each is satisfied by its anchor
	// wherever in the shipped corpus that anchor appears.
	EnumerationDocFile = "docs/enumeration.md"

	// AnchorEnumerationOrder introduces the hand-out order. The anchor is a FIXED constant,
	// because a check free to pick its own anchor per run is a check that can be made to
	// pass by moving the goalposts.
	AnchorEnumerationOrder = "enumeration-order"

	// AnchorEnumerationFigures introduces the measured cost of the enumeration.
	AnchorEnumerationFigures = "enumeration-memory-figures"
)

// EnumerationOrderClauses is the whole obligation the order statement carries. Each token is
// the shortest string that carries its clause and could not plausibly be written by accident
// while meaning something else.
//
// None of them implies another, and the last two are the ones a later queue-ordering spec
// reads: an order that did not know this one is per-directory rather than global would be
// written against a sequence that does not exist, and an order that did not know the sequence
// is independent of the worker count would have to re-derive that for itself.
var EnumerationOrderClauses = []Clause{
	{
		Token: "directory by directory, in the order the coverage set names them",
		Clause: "the DIRECTORY sequence is named: the coverage set's own order, which is the " +
			"startup walk's traversal",
	},
	{
		Token:  "entry-name order",
		Clause: "the order WITHIN one directory is named",
	},
	{
		Token: "total and deterministic",
		Clause: "the property is stated: every source is handed out exactly once, and two scans " +
			"over an unchanged library agree",
	},
	{
		Token: "does not depend on the worker count",
		Clause: "the order is independent of how many workers are configured, which is what lets " +
			"an operator change that number without changing what runs first",
	},
	{
		Token: "does not depend on the order the workers happen to finish in",
		Clause: "the order is independent of which worker finishes when, so it is a property of " +
			"the listing rather than of the encoding",
	},
	{
		Token: "not a global sort of the full paths",
		Clause: "the ONE thing a reader would otherwise assume is denied: this is a per-directory " +
			"sequence, not a sort of every path, and a global sort cannot be produced without " +
			"holding every path at once",
	},
}

// EnumerationFigureClauses is what a recorded figure owes. The first four are the figures
// themselves - two sizes, two quantities - and the rest are what a figure needs to MEAN
// anything: without the hardware, the build and the date, nothing may be compared with it,
// and without the run count and the spread a single reading invites a comparison it cannot
// support (performance PB5).
var EnumerationFigureClauses = []Clause{
	{Token: "100,000 paths", Clause: "the smaller library the figures were taken over is named"},
	{Token: "1,000,000 paths", Clause: "the larger library the figures were taken over is named"},
	{Token: "peak heap", Clause: "the memory figure is recorded"},
	{Token: "time to first file", Clause: "the latency figure is recorded"},
	{Token: "hardware:", Clause: "the MACHINE the figures were taken on is named"},
	{Token: "build:", Clause: "the BUILD the figures were taken against is named"},
	{Token: "date:", Clause: "the DATE the figures were taken on is named"},
	{Token: "runs:", Clause: "how many runs the figures are made of is named"},
	{Token: "spread:", Clause: "the spread across those runs is named, so a single reading is not " +
		"reported as though it were a measurement"},
}

// CheckEnumerationOrder applies the anchored-statement rule to the hand-out order: some
// shipped document introduces it under the fixed anchor, that occurrence has text, and ONE
// occurrence carries a token for every clause it owes.
func CheckEnumerationOrder(files []string) error {
	return checkStatement(files, AnchorEnumerationOrder, EnumerationOrderClauses,
		"the enumeration's hand-out order is MISSING: a scan hands source files to its workers in "+
			"an order, a later declared queue order is layered on that order, and nothing the "+
			"repository ships now says what it is")
}

// CheckEnumerationFigures applies the same rule to the measured cost of the enumeration, and
// it is what makes `make check-enumeration-memory` fail on a repository that measured
// something and recorded nothing.
func CheckEnumerationFigures(files []string) error {
	return checkStatement(files, AnchorEnumerationFigures, EnumerationFigureClauses,
		"the enumeration's measured figures are MISSING: what a scan holds and how quickly it "+
			"reaches its first worker are measured at two library sizes, and a measurement that "+
			"is not recorded with the machine, the build and the date it was taken on is one no "+
			"later measurement can be compared against")
}

// checkStatement is the rule both of the above apply, and the reason they are one function:
// the anchored-statement shape is the shape, and two copies of it would be two places for the
// rule to drift.
//
// An anchor appearing in more than one file is not an error: the check wants the statement to
// EXIST in what the repository ships, so ANY occurrence satisfying the rule satisfies it, and
// a problem is reported only when NONE does. The message then names the strongest near-miss,
// so a failure points at a file to edit rather than saying "nowhere".
func checkStatement(files []string, anchor string, clauses []Clause, missing string) error {
	if len(files) == 0 {
		return fmt.Errorf("docscheck: no documents to search - a check with no corpus passes everything")
	}
	sts, err := statements(files, anchor)
	if err != nil {
		return err
	}
	if len(sts) == 0 {
		return fmt.Errorf("no shipped document carries the anchor %q, so %s (it belongs in %s)",
			anchor, missing, EnumerationDocFile)
	}

	var present []Statement
	for _, s := range sts {
		if s.Present() {
			present = append(present, s)
		}
	}
	if len(present) == 0 {
		return fmt.Errorf("%s: the anchor %q is present but no line of text follows it before the next "+
			"anchor or heading - an anchor with nothing under it is not a statement, so %s",
			sts[0].File, anchor, missing)
	}

	for _, s := range present {
		if len(missingClauses(s, clauses)) == 0 {
			return nil
		}
	}

	if len(present) > 1 && carriedBetweenThem(present, clauses) {
		var where []string
		for _, s := range present {
			where = append(where, s.File)
		}
		return fmt.Errorf("the statement under %q is SPLIT across %d documents (%s) and no single one "+
			"carries all %d clauses: a reader has to find them in one place, so they belong in one "+
			"statement", anchor, len(present), strings.Join(where, ", "), len(clauses))
	}

	best := bestStatement(present, clauses)
	var says []string
	for _, c := range missingClauses(best, clauses) {
		says = append(says, fmt.Sprintf("never says %q, so the statement that %s is MISSING", c.Token, c.Clause))
	}
	return fmt.Errorf("%s comes closest of the %d document(s) carrying %q, and it %s",
		best.File, len(present), anchor, strings.Join(says, "; it "))
}

package docscheck_test

// The mechanical documentation checks for S0099: the enumeration's hand-out order, and the
// figures it was measured at. Each pair is one case over the corpus this repository actually
// SHIPS, and one case proving that check can fail - a guard nobody tries to defeat is a guard
// nobody knows works.

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/docscheck"
)

// TestEnumerationOrderIsStated grades [AC-3]: the shipped documentation carries the rule that
// produces the hand-out order, precise enough for a later spec to declare a queue order on
// top of it.
func TestEnumerationOrderIsStated(t *testing.T) {
	if err := docscheck.CheckEnumerationOrder(shippedCorpus(t)); err != nil {
		t.Errorf("the shipped documentation does not state the enumeration's hand-out order: %v\n"+
			"A scan hands files to its workers in a sequence and a later declared queue order is "+
			"built on that sequence; a rule nobody wrote down is one the next change breaks "+
			"without noticing.", err)
	}
}

// TestEnumerationOrderCheckBites proves the check above can FAIL, which is [AC-3]'s other
// half: every way a documentation check goes wrong is a way it goes GREEN. An anchor nobody
// wrote, an anchor with nothing under it, a statement missing one clause and a corpus that
// was never read are each defeated on purpose here.
func TestEnumerationOrderCheckBites(t *testing.T) {
	full := make([]string, 0, len(docscheck.EnumerationOrderClauses))
	for _, c := range docscheck.EnumerationOrderClauses {
		full = append(full, "A scan is "+c.Token+".")
	}

	t.Run("a corpus with nothing in it is not a pass", func(t *testing.T) {
		if err := docscheck.CheckEnumerationOrder(nil); err == nil {
			t.Error("a check with no documents to search reported the order stated")
		}
	})

	t.Run("the whole statement passes", func(t *testing.T) {
		if err := docscheck.CheckEnumerationOrder(writeCorpus(t,
			statementDoc(docscheck.AnchorEnumerationOrder, full))); err != nil {
			t.Fatalf("a statement carrying every clause was refused: %v", err)
		}
	})

	t.Run("a statement missing one clause is refused", func(t *testing.T) {
		for i, c := range docscheck.EnumerationOrderClauses {
			lines := append(append([]string(nil), full[:i]...), full[i+1:]...)
			err := docscheck.CheckEnumerationOrder(writeCorpus(t,
				statementDoc(docscheck.AnchorEnumerationOrder, lines)))
			if err == nil {
				t.Errorf("a statement that never says %q passed", c.Token)
				continue
			}
			if !strings.Contains(err.Error(), c.Token) {
				t.Errorf("the refusal for the missing clause %q does not name it: %v", c.Token, err)
			}
		}
	})

	t.Run("an anchor nobody wrote is refused", func(t *testing.T) {
		if err := docscheck.CheckEnumerationOrder(writeCorpus(t,
			"# Fixture\n\nProse that never introduces the statement.\n")); err == nil {
			t.Error("a corpus carrying no anchor at all reported the order stated")
		}
	})

	t.Run("an anchor with nothing under it is refused", func(t *testing.T) {
		if err := docscheck.CheckEnumerationOrder(writeCorpus(t,
			statementDoc(docscheck.AnchorEnumerationOrder, nil))); err == nil {
			t.Error("an anchor introducing no text reported the order stated")
		}
	})
}

// TestEnumerationFiguresAreRecorded grades the recording half of [AC-9] and [AC-10], and it is
// what `make check-enumeration-memory` runs beside the measurement itself: the two figures are
// written down, at both library sizes, carrying the hardware, the build and the date they
// were taken on, plus the run count and the spread a figure needs to be comparable at all
// (performance PB5).
func TestEnumerationFiguresAreRecorded(t *testing.T) {
	if err := docscheck.CheckEnumerationFigures(shippedCorpus(t)); err != nil {
		t.Errorf("the enumeration's measured figures are not recorded in the shipped documentation: %v\n"+
			"A measurement nobody wrote down cannot be compared with the next one, and a figure "+
			"without its machine, its build and its date cannot be compared with anything.", err)
	}
}

// TestEnumerationFiguresCheckBites proves that check can FAIL, which is the other half of the
// recording clause [AC-9] and [AC-10] share: dropping the hardware, the build, the date, the
// run count, the spread or either figure is refused, and the refusal names what went missing.
func TestEnumerationFiguresCheckBites(t *testing.T) {
	full := make([]string, 0, len(docscheck.EnumerationFigureClauses))
	for _, c := range docscheck.EnumerationFigureClauses {
		full = append(full, "Recorded: "+c.Token+" something.")
	}

	t.Run("a corpus with nothing in it is not a pass", func(t *testing.T) {
		if err := docscheck.CheckEnumerationFigures(nil); err == nil {
			t.Error("a check with no documents to search reported the figures recorded")
		}
	})

	t.Run("the whole record passes", func(t *testing.T) {
		if err := docscheck.CheckEnumerationFigures(writeCorpus(t,
			statementDoc(docscheck.AnchorEnumerationFigures, full))); err != nil {
			t.Fatalf("a record carrying every clause was refused: %v", err)
		}
	})

	t.Run("a record missing one clause is refused", func(t *testing.T) {
		for i, c := range docscheck.EnumerationFigureClauses {
			lines := append(append([]string(nil), full[:i]...), full[i+1:]...)
			err := docscheck.CheckEnumerationFigures(writeCorpus(t,
				statementDoc(docscheck.AnchorEnumerationFigures, lines)))
			if err == nil {
				t.Errorf("a record that never says %q passed", c.Token)
				continue
			}
			if !strings.Contains(err.Error(), c.Token) {
				t.Errorf("the refusal for the missing clause %q does not name it: %v", c.Token, err)
			}
		}
	})

	t.Run("figures nobody recorded at all are refused", func(t *testing.T) {
		if err := docscheck.CheckEnumerationFigures(writeCorpus(t,
			"# Fixture\n\nProse that records no measurement.\n")); err == nil {
			t.Error("a corpus carrying no anchor at all reported the figures recorded")
		}
	})
}

package notify

import (
	"fmt"
	"strings"

	"github.com/NSchatz/holdfast/internal/store"
)

// maxNamedProblems bounds how many files one health-sweep summary names; the counts are
// always the whole figure.
const maxNamedProblems = 10

// HealthFileChecked implements the health sweep's reporter. A notification is one summary
// per sweep, not one per file, so there is nothing to do here.
func (n *Notifier) HealthFileChecked(store.HealthResult) {}

// HealthSweepFinished sends ONE summary for a finished sweep that found any file corrupt or
// unreadable, naming up to maxNamedProblems of them with their reasons. A clean sweep sends
// nothing. It is a report: the message says nothing was changed, because nothing was.
func (n *Notifier) HealthSweepFinished(sw store.HealthSweep, problems []store.HealthCheck) {
	bad := sw.Counts.Corrupt + sw.Counts.Unreadable
	if bad == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "holdfast health sweep: %d of %d file(s) did not pass a full decode (%d corrupt, %d unreadable). "+
		"Report only - no file was moved, renamed, repaired or deleted.",
		bad, sw.Counts.Checked(), sw.Counts.Corrupt, sw.Counts.Unreadable)
	named := 0
	for _, p := range problems {
		if named == maxNamedProblems {
			break
		}
		fmt.Fprintf(&b, "\n%s %s: %s", p.Result, p.Path, p.Reason)
		named++
	}
	if rest := bad - int64(named); rest > 0 {
		fmt.Fprintf(&b, "\n... and %d more: GET /api/health lists them.", rest)
	}
	n.enqueue(b.String())
}

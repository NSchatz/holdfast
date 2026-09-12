// Package heapmeasure is this repository's ONE implementation of "what did that
// piece of work retain in memory". It reads live heap after a forced collection
// and reports it RELATIVE to a baseline read before the work began, because the
// absolute reading is not a property of the code under test.
//
// Live heap at an instant is a function of the machine, the Go build, the
// allocator's arena layout, the race detector's shadow state, the coverage
// counters and of whatever else the same test binary has already run: the same
// fixture in this repository measured 399 KiB and 464 KiB at the same instant in
// two different positions, and an absolute ceiling taken on one machine reds on
// the next one with no change to the code it grades. Worse, it mis-grades in the
// dangerous direction - a real regression of a few kilobytes is invisible beside
// a baseline that is already over the line.
//
// So a figure from this package is only ever compared with ANOTHER figure from
// this package taken in the SAME process and the same run, where both readings
// carry the same instrumentation. Two such readings are the only two heap
// figures in this repository that are comparable with each other.
//
// It is an ordinary package rather than a test helper so that a test in any
// package can import it: the measurement exists once, and a second copy of it
// somewhere else would be a second set of conventions about what a heap reading
// means.
package heapmeasure

import (
	"fmt"
	"runtime"
)

// Retained is live heap after a forced collection: everything unreachable has
// gone, so what is left is what is still HELD. Collected twice, because the
// first collection can leave finalisable objects for the second to reclaim.
func Retained() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// Probe is one measurement: a baseline read before the work began, and the
// highest reading taken while it ran. What it reports is the difference, which
// is the only part of either reading that describes the work rather than the
// machine.
type Probe struct {
	what    string
	base    uint64
	high    uint64
	samples int
}

// Start reads the baseline and begins a measurement. what names the thing being
// measured and appears in every refusal this probe can report, so a run that
// could not measure says which measurement it was.
func Start(what string) *Probe { return From(what, Retained()) }

// From begins a measurement from a baseline the caller has ALREADY read. It is
// how several measurements of one operation share one baseline: two readings
// taken against the same baseline are comparable with each other as well as with
// it, where two independently-taken baselines differ by whatever the runtime did
// between them.
func From(what string, base uint64) *Probe {
	return &Probe{what: what, base: base}
}

// FromReadings builds a probe from readings the caller supplies instead of from
// the runtime. It is how the suite drives every refusal this package can produce
// without having to contrive a runtime that misbehaves: a refusal path nothing
// exercises is a refusal path that rots, and this one guards the difference
// between a measurement and a number.
func FromReadings(what string, base, high uint64, samples int) *Probe {
	return &Probe{what: what, base: base, high: high, samples: samples}
}

// Sample records a reading at this instant and keeps the highest one taken so
// far, so a caller may sample as often as it likes and still be measuring the
// peak.
func (p *Probe) Sample() {
	now := Retained()
	p.samples++
	if now > p.high {
		p.high = now
	}
}

// Peak reports what the work held above the baseline at its highest sampled
// instant, for a measurement whose figure CANNOT legitimately sit below its
// baseline - the peak of an operation that is provably holding something at the
// instant it is sampled. It refuses rather than returning a number it cannot
// mean; see unusable for what it refuses and why.
func (p *Probe) Peak() (int64, error) {
	if err := p.unusable(true); err != nil {
		return 0, err
	}
	return int64(p.high) - int64(p.base), nil
}

// Delta reports the same subtraction for a measurement whose figure MAY sit
// below its baseline: what a completed operation left behind, where leaving
// nothing behind is the passing answer and a reading under the baseline only
// says the run gave back more than it took. The other refusals still apply -
// neither an absent reading nor a zero baseline is a measurement.
func (p *Probe) Delta() (int64, error) {
	if err := p.unusable(false); err != nil {
		return 0, err
	}
	return int64(p.high) - int64(p.base), nil
}

// unusable names what could not be measured, or returns nil. Each case is a way
// the pair of readings cannot MEAN "what the work retained", and each reports a
// failure instead of a default: a measurement harness that answers 0 when it
// could not measure is worse than one that answers nothing, because 0 passes
// every comparison anybody will ever write against it.
func (p *Probe) unusable(requireRise bool) error {
	if p.samples == 0 {
		return fmt.Errorf("heapmeasure: %s could not be measured: no reading was taken while the work ran, "+
			"so there is nothing to compare with the baseline of %d bytes. The instant that was to be sampled "+
			"was never reached", p.what, p.base)
	}
	if p.base == 0 {
		return fmt.Errorf("heapmeasure: %s could not be measured: the baseline read 0 bytes of live heap, "+
			"which no running Go program has, so the reading of %d bytes is relative to nothing", p.what, p.high)
	}
	if requireRise && p.high < p.base {
		return fmt.Errorf("heapmeasure: %s could not be measured: the peak of %d bytes over %d reading(s) is "+
			"BELOW the baseline of %d bytes, so live heap was not reported monotonically across the "+
			"measurement and the pair cannot say what the work held", p.what, p.high, p.samples, p.base)
	}
	return nil
}

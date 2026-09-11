package heapmeasure

import (
	"strings"
	"testing"
)

// Every case below asserts on readings this file SUPPLIES rather than on any it
// measured, so its conditions carry supplied figures and never a ceiling: the
// arithmetic and the refusals are what is graded here, and what a machine reads
// out of the runtime has no part in either. The one case that does read the
// runtime asserts only that the reading RESPONDED, never how many bytes it said.

// TestPeakRetainedMeasurement_ReportsWhatTheWorkHeldAboveItsBaseline. The
// measurement this package exists to take, asserted on its own: what is reported
// is the difference between the highest reading and the baseline, and neither
// absolute figure survives into the answer.
func TestPeakRetainedMeasurement_ReportsWhatTheWorkHeldAboveItsBaseline(t *testing.T) {
	// 100000 and 140000 are not measurements and are not a threshold: they are
	// the two readings this case hands the probe, so the difference it must
	// report is arithmetic and is the same on every machine.
	p := FromReadings("a fixture", 100_000, 140_000, 3)
	got, err := p.Peak()
	if err != nil {
		t.Fatalf("Peak() over a usable pair of readings refused it: %v", err)
	}
	if got != 40_000 {
		t.Errorf("Peak() = %d, want 40000 - the difference between the two readings it was handed", got)
	}
	// Delta is the same subtraction, and says so: the two differ only in what
	// they refuse, never in what they compute.
	if d, err := FromReadings("a fixture", 100_000, 140_000, 3).Delta(); err != nil || d != got {
		t.Errorf("Delta() = %d, %v, want %d and no error - the two report the same difference", d, err, got)
	}
}

// TestPeakRetainedMeasurement_RefusesAMeasurementWithNoReading. The instant a
// caller meant to sample is not guaranteed to be reached: a scan whose feed loop
// never ran never called back. A probe that answered 0 there would pass every
// comparison anybody ever writes against it, for ever, silently - so an absent
// reading is a FAILURE naming the measurement, and never a default.
//
// This refusal is DRIVEN here rather than waited for. It cannot arise from a
// green run, which is exactly why it would otherwise rot unnoticed.
func TestPeakRetainedMeasurement_RefusesAMeasurementWithNoReading(t *testing.T) {
	p := FromReadings("the peak a scan holds of its own enumeration", 100_000, 0, 0)
	for _, tc := range []struct {
		route string
		got   func() (int64, error)
	}{
		{"Peak", p.Peak},
		{"Delta", p.Delta},
	} {
		n, err := tc.got()
		if err == nil {
			t.Errorf("%s() over a measurement that took NO reading returned %d and no error; a harness that "+
				"answers a figure where it measured nothing cannot be told apart from one that measured nothing "+
				"held", tc.route, n)
			continue
		}
		if !strings.Contains(err.Error(), "no reading was taken") ||
			!strings.Contains(err.Error(), "the peak a scan holds of its own enumeration") {
			t.Errorf("%s() refused without naming what could not be measured: %v", tc.route, err)
		}
	}
}

// TestPeakRetainedMeasurement_RefusesAZeroBaseline. A baseline of zero live heap
// is not a quiet program, it is a reading that did not happen: no running Go
// program holds nothing. Zero is the one heap figure that IS portable across
// every machine, and a difference taken against it is the absolute reading this
// package exists to stop anybody asserting on.
func TestPeakRetainedMeasurement_RefusesAZeroBaseline(t *testing.T) {
	p := FromReadings("what a scan left behind", 0, 140_000, 1)
	for _, tc := range []struct {
		route string
		got   func() (int64, error)
	}{
		{"Peak", p.Peak},
		{"Delta", p.Delta},
	} {
		n, err := tc.got()
		if err == nil {
			t.Errorf("%s() over a ZERO baseline returned %d and no error, which is the absolute reading "+
				"dressed as a relative one", tc.route, n)
			continue
		}
		if !strings.Contains(err.Error(), "baseline read 0 bytes") ||
			!strings.Contains(err.Error(), "what a scan left behind") {
			t.Errorf("%s() refused without naming what could not be measured: %v", tc.route, err)
		}
	}
}

// TestPeakRetainedMeasurement_RefusesAPeakBelowItsBaseline. A peak reading under
// its baseline means live heap was not reported monotonically across the
// measurement - the baseline was taken over a heap that had not settled - so the
// pair says nothing about what the work held and its difference is negative
// noise. Refused for a peak, and deliberately ALLOWED for what a completed
// operation LEFT BEHIND, where under the baseline is the good answer and
// refusing it would red a run that retained nothing.
func TestPeakRetainedMeasurement_RefusesAPeakBelowItsBaseline(t *testing.T) {
	// Again two supplied readings, ordered the wrong way round on purpose.
	p := FromReadings("the peak a scan holds of its own enumeration", 140_000, 100_000, 2)

	n, err := p.Peak()
	if err == nil {
		t.Fatalf("Peak() over a reading BELOW its baseline returned %d and no error; the pair cannot say what "+
			"the work held, and a negative peak clears every comparison there is", n)
	}
	if !strings.Contains(err.Error(), "BELOW the baseline") ||
		!strings.Contains(err.Error(), "the peak a scan holds of its own enumeration") {
		t.Errorf("Peak() refused without naming what could not be measured: %v", err)
	}

	left, err := p.Delta()
	if err != nil {
		t.Fatalf("Delta() refused a reading below its baseline: %v. What a completed operation left behind is "+
			"allowed to sit under the baseline - that is the answer for an operation that left nothing", err)
	}
	if left != -40_000 {
		t.Errorf("Delta() = %d, want -40000 - the difference between the two readings it was handed", left)
	}
}

// TestPeakRetainedMeasurement_RespondsToWhatThisProcessActuallyHolds. The
// instrument itself, and the ONLY case here that reads the runtime. It asserts
// that the reading responds - non-zero for a running program, and higher at an
// instant this process is holding something it was not holding at the baseline -
// and asserts NO byte count, because a byte count is the machine's answer and
// not this package's.
func TestPeakRetainedMeasurement_RespondsToWhatThisProcessActuallyHolds(t *testing.T) {
	p := Start("a slab this case holds on purpose")
	if p.base == 0 {
		// Not a threshold: zero live heap is impossible on every machine, so this
		// condition is portable in the only way a bare figure ever is.
		t.Fatal("the baseline read 0 bytes of live heap in a running Go program; the instrument is not reading")
	}

	const slab = 8 << 20 // far above whatever a forced collection leaves behind
	held := make([]byte, slab)
	for i := range held {
		held[i] = byte(i)
	}
	p.Sample()

	got, err := p.Peak()
	if err != nil {
		t.Fatalf("Peak() over a live measurement refused it: %v", err)
	}
	t.Logf("holding a slab of %d bytes moved the measurement by %d bytes", slab, got)
	if got <= 0 {
		t.Errorf("this case held a slab across the reading and the measurement moved by %d bytes; the "+
			"instrument is not responding to what the process retains", got)
	}
	// The slab has to be reachable AT the reading or the collection inside Sample
	// would have taken it, and the measurement would be of nothing.
	if held[slab-1] != byte((slab-1)%256) {
		t.Fatal("the slab this case held was not the slab it wrote")
	}
}

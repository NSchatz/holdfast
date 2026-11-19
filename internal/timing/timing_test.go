package timing

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// listing is a timed listing in the time base num/den.
func listing(num, den int64, ticks ...int64) probe.Timestamps {
	return probe.Timestamps{TimeBaseNum: num, TimeBaseDen: den, Packets: len(ticks), Ticks: ticks}
}

// ms is a listing in Matroska's milliseconds.
func ms(ticks ...int64) probe.Timestamps { return listing(1, 1000, ticks...) }

// vfr is a source at 24, then 60, then 30 fps, in milliseconds as Matroska stores it, listed in
// decode order (a B-frame ahead of the frames it is shown after).
var vfr = ms(0, 83, 42, 125, 167, 183, 200, 217, 233, 267, 300)

func TestCheck_Passes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
	}{
		{"the same ticks, in another decode order", vfr, ms(0, 42, 83, 125, 167, 183, 200, 217, 233, 267, 300)},
		{"the whole timeline moved: an encode rebased to 0, a muxer's delay", vfr,
			ms(1400, 1483, 1442, 1525, 1567, 1583, 1600, 1617, 1633, 1667, 1700)},
		{"MPEG-TS's 90 kHz into Matroska's milliseconds, each rounded once", listing(1, 90000, 0, 3750, 7500, 9000),
			ms(0, 42, 83, 100)},
		{"each position off by exactly one tick of the coarser base, the output's", listing(1, 90000, 0, 3690, 7560),
			ms(0, 42, 83)},
		{"each position off by exactly one tick of the coarser base, the source's", ms(0, 42, 83),
			listing(1, 90000, 0, 3690, 7560)},
		{"a base whose tick is not one unit", listing(1001, 24000, 0, 1, 2, 3), listing(1001, 48000, 0, 2, 4, 6)},
		{"an AVI read by its decode timestamps", probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 24, Packets: 3,
			Ticks: []int64{0, 1, 2}, FromDTS: true}, ms(0, 42, 83)},
		{"into an AVI read by its decode timestamps", ms(0, 42, 83), probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 24,
			Packets: 3, Ticks: []int64{5, 6, 7}, FromDTS: true}},
		{"an untimed elementary stream: only the count and the output's own instants",
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1200000, Packets: 3, Untimed: true}, ms(0, 100, 200)},
		{"a long film in nanoseconds against milliseconds", listing(1, 1000000000, 0, 10800000000000, 10800041708333),
			ms(0, 10800000, 10800042)},
		{"a long film in nanoseconds on both sides, whose products pass 64 bits",
			listing(1, 1000000000, 0, 10800000000000, 10800041708333),
			listing(1, 1000000000, 0, 10800000000001, 10800041708332)},
		{"one frame", ms(7), ms(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check(tc.src, tc.out); err != nil {
				t.Errorf("Check refused a faithful output: %v", err)
			}
		})
	}
}

func TestCheck_Refuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		want     []string
	}{
		{"a frame fewer", vfr, ms(0, 42, 83, 125, 167, 183, 200, 217, 233, 267),
			[]string{"timing parity failed: the output's video carries 10 packets and the source's 11"}},
		{"a frame more", ms(0, 42), ms(0, 42, 83),
			[]string{"the output's video carries 3 packets and the source's 2"}},
		{"neither carries a packet", ms(), ms(),
			[]string{"timing parity could not be established: neither video stream carries a packet"}},
		{"the 60 and 30 fps frames rounded onto a 24 fps grid", vfr,
			ms(1000, 1083, 1042, 1125, 1167, 1167, 1208, 1208, 1250, 1250, 1292),
			[]string{"3 of the output's 11 video frames are presented at the same instant as the frame before them",
				"the first at 0.167000s from its first frame", "collapsed"}},
		{"one frame moved by more than a tick", ms(500, 542, 583, 625, 667, 683, 700, 717, 733, 767, 800),
			ms(1000, 1042, 1083, 1125, 1167, 1183, 1200, 1219, 1233, 1267, 1300),
			[]string{"the output's video frame 7 is presented at 0.219000s from its first frame and the source's at 0.217000s",
				"one tick of the coarser time base (1/1000)", "moved"}},
		{"a 24 fps stretch rounded onto a 30 fps grid: never colliding, every frame under half a frame off",
			ms(0, 42, 83, 125, 167, 208), ms(0, 33, 67, 133, 167, 200),
			[]string{"frame 1 is presented at 0.033000s"}},
		{"one tick and one finer tick past the output's coarser base", listing(1, 90000, 900, 4590, 8461), ms(10, 52, 93),
			[]string{"frame 2 is presented at 0.083000s from its first frame and the source's at 0.084011s", "(1/1000)"}},
		{"one tick and one finer tick past the source's coarser base", ms(10, 52, 93), listing(1, 90000, 900, 4590, 8461),
			[]string{"frame 2 is presented at 0.084011s from its first frame and the source's at 0.083000s", "(1/1000)"}},
		{"the first frame alone moved: every other position shifts", ms(0, 42, 83), ms(5, 42, 83),
			[]string{"frame 1 is presented at 0.037000s"}},
		{"a long film one frame off", listing(1, 1000000000, 0, 10800000000000),
			ms(0, 10800042), []string{"frame 1 is presented at 10800.042000s from its first frame and the source's at 10800.000000s"}},
		{"a long film two ticks off in nanoseconds on both sides, whose products pass 64 bits",
			listing(1, 1000000000, 0, 10800000000000), listing(1, 1000000000, 0, 10800000000002),
			[]string{"frame 1 is presented at 10800.000000s", "(1/1000000000)"}},
		{"an untimed output", ms(0, 42), probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: 2, Untimed: true},
			[]string{"timing parity could not be established: not every packet of the output's video carries a timestamp"}},
		{"an untimed source still holds the output's instants", probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1, Packets: 3,
			Untimed: true}, ms(0, 100, 100), []string{"1 of the output's 3 video frames are presented at the same instant"}},
		{"an output listing short of its packets", ms(0, 42),
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: 2, Ticks: []int64{0}},
			[]string{"the output's video carries 1 timestamps for 2 packets and the source's 2 for 2"}},
		{"a timed source listing short of its packets", probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: 2,
			Ticks: []int64{0}}, ms(0, 42), []string{"the source's 1 for 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.src, tc.out)
			if err == nil {
				t.Fatal("Check passed an output whose timing is not the source's")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal %q does not say %q", err, w)
				}
			}
		})
	}
}

// dtsListing is a source timed by its decode timestamps alone, in ticks of 1/600 s (AVI's).
func dtsListing(ticks ...int64) probe.Timestamps {
	ts := listing(1, 600, ticks...)
	ts.FromDTS = true
	return ts
}

// growing is n decode timestamps whose gaps grow by one tick each (1, 2, 3, ...), so that no
// two shifts of it line up: only the shift it was built with matches.
func growing(n int) []int64 {
	out := make([]int64, n)
	for k := 1; k < n; k++ {
		out[k] = out[k-1] + int64(k)
	}
	return out
}

// shown is what a decoder holding d frames back shows a decode-timed source at: the decode
// timestamps from packet d on, then one extrapolated instant for each of the last d frames.
func shown(dts []int64, d int) []int64 {
	out := append([]int64(nil), dts[d:]...)
	for k := 1; k <= d; k++ {
		out = append(out, dts[len(dts)-1]+int64(1000*k))
	}
	return out
}

// TestCheck_ADecodeTimedSourceIsShownAtItsReorderingDelay: a source timed by decode
// timestamps alone (AVI, ASF) is shown d frames on, d being the decoder's reordering delay,
// which the listing does not carry; an output shown at any delay up to MaxReorder passes, and
// one at no delay up to it does not.
func TestCheck_ADecodeTimedSourceIsShownAtItsReorderingDelay(t *testing.T) {
	// 24 fps, then 60, then 30, in AVI's 1/600 s slots.
	vfrSlots := []int64{0, 25, 50, 75, 100, 110, 120, 130, 140, 160, 180, 200}
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		pass     bool
	}{
		{"an H.264 AVI with two B-frames, shown two frames on", dtsListing(vfrSlots...),
			listing(1, 600, shown(vfrSlots, 2)...), true},
		{"shown at no delay", dtsListing(vfrSlots...), listing(1, 600, vfrSlots...), true},
		{"shown at the most a decoder holds back", dtsListing(growing(40)...), listing(1, 600, shown(growing(40), 16)...), true},
		{"shown one frame further on than any decoder holds back", dtsListing(growing(40)...),
			listing(1, 600, shown(growing(40), 17)...), false},
		{"a short file shown at a delay that leaves out more frames than it compares", dtsListing(growing(5)...),
			listing(1, 600, shown(growing(5), 3)...), false},
		{"a short file shown at a delay that compares as many frames as it leaves out", dtsListing(growing(5)...),
			listing(1, 600, shown(growing(5), 2)...), true},
		{"a source timed by presentation timestamps is held to no delay", listing(1, 600, vfrSlots...),
			listing(1, 600, shown(vfrSlots, 2)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.src, tc.out)
			if tc.pass && err != nil {
				t.Errorf("Check refused an output shown as the decoder shows the source: %v", err)
			}
			if !tc.pass && (err == nil || !strings.Contains(err.Error(), "the encode moved the source's frame timing")) {
				t.Errorf("Check = %v, want the output refused as moved", err)
			}
		})
	}
}

func TestRepeats(t *testing.T) {
	for _, tc := range []struct {
		in       []int64
		n, first int
	}{
		{nil, 0, 0},
		{[]int64{5}, 0, 0},
		{[]int64{1, 2, 3}, 0, 0},
		{[]int64{1, 1}, 1, 1},
		{[]int64{1, 2, 2, 2, 3, 4, 4}, 3, 2},
	} {
		if n, first := repeats(tc.in); n != tc.n || first != tc.first {
			t.Errorf("repeats(%v) = %d, %d; want %d, %d", tc.in, n, first, tc.n, tc.first)
		}
	}
}

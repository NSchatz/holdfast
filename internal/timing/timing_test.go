package timing

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// listing is a listing timed by presentation timestamps in the base num/den, in file order,
// its first packet a keyframe.
func listing(num, den int64, ticks ...int64) probe.Timestamps {
	ts := probe.Timestamps{TimeBaseNum: num, TimeBaseDen: den}
	for i, t := range ticks {
		ts.Packets = append(ts.Packets, probe.Packet{PTS: t, HasPTS: true, Key: i == 0})
	}
	return ts
}

// ms is a listing in Matroska's milliseconds.
func ms(ticks ...int64) probe.Timestamps { return listing(1, 1000, ticks...) }

// dtsListing is a listing timed by decode timestamps alone (AVI, ASF), its first packet a
// keyframe.
func dtsListing(num, den int64, ticks ...int64) probe.Timestamps {
	ts := probe.Timestamps{TimeBaseNum: num, TimeBaseDen: den}
	for i, t := range ticks {
		ts.Packets = append(ts.Packets, probe.Packet{DTS: t, HasDTS: true, Key: i == 0})
	}
	return ts
}

// untimed is n packets that carry no timestamp, a raw elementary stream's.
func untimed(n int) probe.Timestamps {
	return probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1200000, Packets: make([]probe.Packet, n)}
}

// decoded is what the decode of a source's first n packets showed: frames at the times given,
// in the order shown; na is a frame shown with no timestamp.
func decoded(n int, ts ...int64) *probe.Start {
	s := &probe.Start{Packets: n}
	for _, t := range ts {
		s.Frames = append(s.Frames, probe.Frame{TS: t, HasTS: t != na})
	}
	return s
}

// na stands for a decoded frame with no timestamp.
const na = -1 << 62

// packets is a listing of hand-built packets in Matroska's milliseconds.
func packets(p ...probe.Packet) probe.Timestamps {
	return probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: p}
}

// at is a packet presented at t; key, na (no timestamp) and discard flag it.
func at(t int64, flags ...string) probe.Packet {
	p := probe.Packet{PTS: t, HasPTS: true}
	for _, f := range flags {
		switch f {
		case "key":
			p.Key = true
		case "na":
			p.HasPTS = false
		case "discard":
			p.Discard = true
		}
	}
	return p
}

// vfr is a source at 24, then 60, then 30 fps, in milliseconds as Matroska stores it, listed in
// decode order (a B-frame ahead of the frames it is shown after).
var vfr = ms(0, 83, 42, 125, 167, 183, 200, 217, 233, 267, 300)

func TestCheck_Passes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		start    *probe.Start
	}{
		{"the same ticks, in another decode order", vfr, ms(0, 42, 83, 125, 167, 183, 200, 217, 233, 267, 300), nil},
		{"the whole timeline moved: an encode rebased to 0, a muxer's delay", vfr,
			ms(1400, 1483, 1442, 1525, 1567, 1583, 1600, 1617, 1633, 1667, 1700), nil},
		{"MPEG-TS's 90 kHz into Matroska's milliseconds, each rounded once", listing(1, 90000, 0, 3750, 7500, 9000),
			ms(0, 42, 83, 100), nil},
		{"each position off by exactly one tick of the coarser base, the output's", listing(1, 90000, 0, 3690, 7560),
			ms(0, 42, 83), nil},
		{"each position off by exactly one tick of the coarser base, the source's", ms(0, 42, 83),
			listing(1, 90000, 0, 3690, 7560), nil},
		{"a base whose tick is not one unit", listing(1001, 24000, 0, 1, 2, 3), listing(1001, 48000, 0, 2, 4, 6), nil},
		{"an AVI read by its decode timestamps", dtsListing(1, 24, 0, 1, 2), ms(0, 42, 83), nil},
		{"into an AVI read by its decode timestamps", ms(0, 42, 83), dtsListing(1, 24, 5, 6, 7), nil},
		{"an untimed elementary stream, its start decoded: only the count and the output's own instants", untimed(3),
			ms(0, 100, 200), decoded(3, na, na, na)},
		{"packets flagged to discard are not shown: an MP4 cut between keyframes",
			packets(at(-84, "key", "discard"), at(-42, "discard"), at(0), at(42), at(83)), ms(0, 42, 83), nil},
		{"a stream cut between keyframes, its start decoded: the packets before the keyframe show nothing",
			packets(at(0), at(42), at(83, "key"), at(125), at(167)), ms(0, 42, 84), decoded(5, 83, 125, 167)},
		{"an open GOP's leading pictures, dropped by the decoder",
			packets(at(125, "key"), at(42), at(83), at(250), at(167), at(208)), ms(0, 42, 83, 125),
			decoded(6, 125, 167, 208, 250)},
		{"a decode window shorter than the stream: the rest are its packets",
			packets(at(125, "key"), at(42), at(83), at(250), at(167), at(208)), ms(0, 42, 83, 125),
			decoded(3, 125)},
		{"leading pictures timed before zero, which Matroska reads back with none, dropped by the decoder",
			packets(at(0, "key"), at(0, "na"), at(0, "na"), at(125), at(42), at(83)), ms(0, 42, 83, 125),
			decoded(6, 0, 42, 83, 125)},
		{"a stream timed by decode timestamps alone, cut between keyframes, its start decoded",
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 24, Packets: []probe.Packet{{HasDTS: true},
				{DTS: 1, HasDTS: true, Key: true}, {DTS: 2, HasDTS: true}, {DTS: 3, HasDTS: true}}}, ms(0, 42, 83),
			decoded(4, 1, 2, 3)},
		{"a source's own frames on one instant, reproduced: a remux", ms(0, 42, 42, 83), ms(0, 42, 42, 83), nil},
		{"a long film in nanoseconds against milliseconds", listing(1, 1000000000, 0, 10800000000000, 10800041708333),
			ms(0, 10800000, 10800042), nil},
		{"a long film in nanoseconds on both sides, whose products pass 64 bits",
			listing(1, 1000000000, 0, 10800000000000, 10800041708333),
			listing(1, 1000000000, 0, 10800000000001, 10800041708332), nil},
		{"one frame", ms(7), ms(0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check(tc.src, tc.out, tc.start); err != nil {
				t.Errorf("Check refused a faithful output: %v", err)
			}
		})
	}
}

func TestCheck_Refuses(t *testing.T) {
	// A closed GOP's leading B-frames, presented before its keyframe, which a decoder shows.
	closedGOP := packets(at(120, "key"), at(40), at(80), at(240), at(160), at(200))
	// An IDR's RADL pictures, timed before zero (none, read back from Matroska), shown.
	radl := packets(at(0, "key"), at(0, "na"), at(0, "na"), at(125), at(42), at(83))
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		start    *probe.Start
		want     []string
	}{
		{"a frame fewer, the source's start decoded and showing every packet", vfr,
			ms(0, 42, 83, 125, 167, 183, 200, 217, 233, 267), decoded(11, 0, 42, 83, 125, 167, 183, 200, 217, 233, 267, 300),
			[]string{"timing parity failed: the output's video shows 10 frames and the source's 11 (its first 11 packets " +
				"decoded show 11)"}},
		{"a frame more", ms(0, 42), ms(0, 42, 83), decoded(2, 0, 42),
			[]string{"the output's video shows 3 frames and the source's 2"}},
		{"a closed GOP's leading pictures, which the decoder shows, missing at the start", closedGOP,
			ms(80, 120, 160, 200), decoded(6, 40, 80, 120, 160, 200, 240),
			[]string{"shows 4 frames and the source's 6 (its first 6 packets decoded show 6)"}},
		{"a closed GOP's leading pictures shown, the last two frames missing", closedGOP,
			ms(40, 80, 120, 160), decoded(6, 40, 80, 120, 160, 200, 240),
			[]string{"shows 4 frames and the source's 6"}},
		{"an IDR's RADL pictures timed before zero and shown: where they are shown cannot be read", radl,
			ms(-83, -42, 0, 42, 83, 125), decoded(6, na, na, 0, 42, 83, 125),
			[]string{"2 of the 6 frames a decoder shows of the source carry no timestamp"}},
		{"a frame dropped mid-film: the start explains nothing", ms(0, 42, 83, 125), ms(0, 42, 125),
			decoded(4, 0, 42, 83, 125), []string{"shows 3 frames and the source's 4"}},
		{"neither shows a frame", packets(at(0, "key", "discard")), packets(at(0, "key", "discard")), nil,
			[]string{"timing parity could not be established: neither video stream shows a frame"}},
		{"the 60 and 30 fps frames rounded onto a 24 fps grid", vfr,
			ms(1000, 1083, 1042, 1125, 1167, 1167, 1208, 1208, 1250, 1250, 1292), nil,
			[]string{"3 of the output's 11 video frames are presented at the same instant as the frame before them " +
				"where 0 of the source's are", "the first at 0.167000s from its first frame", "collapsed"}},
		{"frames on one instant beyond the source's own", ms(0, 42, 42, 83, 125), ms(0, 42, 42, 83, 83), nil,
			[]string{"2 of the output's 5 video frames are presented at the same instant as the frame before them " +
				"where 1 of the source's are"}},
		{"one frame moved by more than a tick", ms(500, 542, 583, 625, 667, 683, 700, 717, 733, 767, 800),
			ms(1000, 1042, 1083, 1125, 1167, 1183, 1200, 1219, 1233, 1267, 1300), nil,
			[]string{"the output's video frame 7 is presented at 0.219000s from its first frame and the source's at 0.217000s",
				"one tick of the coarser time base (1/1000)", "moved"}},
		{"a 24 fps stretch rounded onto a 30 fps grid: never colliding, every frame under half a frame off",
			ms(0, 42, 83, 125, 167, 208), ms(0, 33, 67, 133, 167, 200), nil,
			[]string{"frame 1 is presented at 0.033000s"}},
		{"one tick and one finer tick past the output's coarser base", listing(1, 90000, 900, 4590, 8461), ms(10, 52, 93), nil,
			[]string{"frame 2 is presented at 0.083000s from its first frame and the source's at 0.084011s", "(1/1000)"}},
		{"one tick and one finer tick past the source's coarser base", ms(10, 52, 93), listing(1, 90000, 900, 4590, 8461), nil,
			[]string{"frame 2 is presented at 0.084011s from its first frame and the source's at 0.083000s", "(1/1000)"}},
		{"the first frame alone moved: every other position shifts", ms(0, 42, 83), ms(5, 42, 83), nil,
			[]string{"frame 1 is presented at 0.037000s"}},
		{"a long film one frame off", listing(1, 1000000000, 0, 10800000000000), ms(0, 10800042), nil,
			[]string{"frame 1 is presented at 10800.042000s from its first frame and the source's at 10800.000000s"}},
		{"a long film two ticks off in nanoseconds on both sides, whose products pass 64 bits",
			listing(1, 1000000000, 0, 10800000000000), listing(1, 1000000000, 0, 10800000000002), nil,
			[]string{"frame 1 is presented at 10800.000000s", "(1/1000000000)"}},
		{"an untimed output", ms(0, 42), untimed(2), nil,
			[]string{"timing parity could not be established: not every frame of the output's video carries a timestamp"}},
		{"an output timed in part", ms(0, 42), packets(at(0, "key"), at(0, "na")), nil,
			[]string{"not every frame of the output's video carries a timestamp"}},
		{"an untimed source still holds the output's instants", untimed(3), ms(0, 100, 100), decoded(3, na, na, na),
			[]string{"1 of the output's 3 video frames are presented at the same instant as the frame before them " +
				"where 0 of the source's are"}},
		{"an open GOP's untimed leading pictures, dropped, and two frames padded at the end", radl,
			ms(0, 42, 83, 125, 167, 208), decoded(6, 0, 42, 83, 125),
			[]string{"shows 6 frames and the source's 4 (its first 6 packets decoded show 4)"}},
		{"a frame with no timestamp past the decoded start", packets(at(0, "key"), at(42), at(0, "na"), at(83)),
			ms(0, 42, 63, 83), decoded(2, 0, 42), []string{"1 of the 4 frames a decoder shows of the source carry no timestamp"}},
		{"a decode-timed source cut between keyframes, its start decoded, one frame moved mid-film",
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 25, Packets: append([]probe.Packet{{DTS: 0, HasDTS: true}},
				dtsListing(1, 25, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10).Packets...)}, ms(0, 40, 80, 120, 170, 200, 240, 280, 320, 360),
			decoded(11, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10), []string{"frame 4 is presented at 0.170000s", "moved"}},
		{"a decode-timed source cut between keyframes, a frame dropped mid-film and one padded at the end",
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 25, Packets: append([]probe.Packet{{DTS: 0, HasDTS: true}},
				dtsListing(1, 25, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10).Packets...)}, ms(0, 40, 80, 120, 200, 240, 280, 320, 360, 400),
			decoded(11, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10), []string{"frame 4 is presented at 0.200000s", "moved"}},
		{"a decode-timed source whose decoder drops more than the packets before its keyframe",
			probe.Timestamps{TimeBaseNum: 1, TimeBaseDen: 25, Packets: append([]probe.Packet{{DTS: 0, HasDTS: true}},
				dtsListing(1, 25, 1, 2, 3, 4).Packets...)}, ms(0, 40, 80), decoded(5, 2, 3, 4),
			[]string{"a decoder drops 2 of the source's first 5 video packets where 1 come before its first keyframe"}},
		{"an AVI at one frame per tick written back into AVI, a frame dropped and one padded",
			dtsListing(1, 25, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), dtsListing(1, 25, 0, 1, 2, 3, 4, 6, 7, 8, 9, 10), nil,
			[]string{"frame 5 is presented at 0.240000s from its first frame and the source's at 0.200000s",
				"further apart than the same tick"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.src, tc.out, tc.start)
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

// TestCheck_AskForTheStartWhereTheCountsDiffer: without a decode of the source's start, a count
// that differs is not a verdict: Check asks for the source's first packets to be decoded, up to
// its first keyframe and 3*MaxReorder after it, never more than the source has.
func TestCheck_AskForTheStartWhereTheCountsDiffer(t *testing.T) {
	long := make([]int64, 60)
	for i := range long {
		long[i] = int64(i) * 42
	}
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		want     int
	}{
		{"a short source: every packet", packets(at(0), at(42), at(83, "key"), at(125)), ms(0, 42), 4},
		{"a long source: up to its keyframe and 48 after", ms(long...), ms(0, 42), 49},
		{"a long source keyed late", packets(append([]probe.Packet{at(0), at(1)}, ms(long...).Packets...)...), ms(0), 51},
		{"no keyframe: from the first packet", untimed(60), ms(0, 42), 49},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var need *NeedsStart
			err := Check(tc.src, tc.out, nil)
			if !errors.As(err, &need) || need.Packets != tc.want {
				t.Fatalf("Check = %v, want a request for the first %d packets decoded", err, tc.want)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("the source's first %d video packets decoded", tc.want)) {
				t.Errorf("the request does not say how many packets: %q", err)
			}
		})
	}
}

// growing is n decode timestamps whose gaps grow by ten ticks each (10, 20, 30, ...), so that
// no two shifts of it line up within a tick: only the shift it was built with matches.
func growing(n int) []int64 {
	out := make([]int64, n)
	for k := 1; k < n; k++ {
		out[k] = out[k-1] + int64(10*k)
	}
	return out
}

// delayed is what a decoder holding d frames back shows a decode-timed source at: the decode
// timestamps from packet d on, then its last d frames each one more of its last interval on,
// plus extra ticks to every one of those.
func delayed(dts []int64, d int, extra int64) []int64 {
	out := append([]int64(nil), dts[d:]...)
	last := dts[len(dts)-1] - dts[len(dts)-2]
	for k := 1; k <= d; k++ {
		out = append(out, out[len(out)-1]+last+extra)
	}
	return out
}

// TestCheck_ADecodeTimedSourceIsShownAtItsReorderingDelay: a source timed by decode
// timestamps alone (AVI, ASF) is shown d frames on, d being the decoder's reordering delay,
// which the listing does not carry, and its last d frames each one more of its last interval
// on; an output shown at any delay up to MaxReorder passes, one at no delay up to it does not,
// and neither does one whose last frames run past the source's recent cadence.
func TestCheck_ADecodeTimedSourceIsShownAtItsReorderingDelay(t *testing.T) {
	// 24 fps, then 60, then 30, in AVI's 1/600 s slots.
	vfrSlots := []int64{0, 25, 50, 75, 100, 110, 120, 130, 140, 160, 180, 200}
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		pass     bool
	}{
		{"an H.264 AVI with two B-frames, shown two frames on", dtsListing(1, 600, vfrSlots...),
			listing(1, 600, delayed(vfrSlots, 2, 0)...), true},
		{"shown at no delay", dtsListing(1, 600, vfrSlots...), listing(1, 600, vfrSlots...), true},
		{"shown at the most a decoder holds back", dtsListing(1, 600, growing(40)...), listing(1, 600, delayed(growing(40), 16, 0)...), true},
		{"shown one frame further on than any decoder holds back", dtsListing(1, 600, growing(40)...),
			listing(1, 600, delayed(growing(40), 17, 0)...), false},
		{"a short file shown at a delay that leaves out more frames than it compares", dtsListing(1, 600, growing(5)...),
			listing(1, 600, delayed(growing(5), 3, 0)...), false},
		{"a short file shown at a delay that compares as many frames as it leaves out", dtsListing(1, 600, growing(5)...),
			listing(1, 600, delayed(growing(5), 2, 0)...), true},
		{"its last frames extrapolated a tick past its longest recent interval", dtsListing(1, 600, vfrSlots...),
			listing(1, 600, delayed(vfrSlots, 2, 1)...), true},
		{"its last frames extrapolated two ticks past it", dtsListing(1, 600, vfrSlots...),
			listing(1, 600, delayed(vfrSlots, 2, 2)...), false},
		{"its last frames presented seconds on", dtsListing(1, 600, vfrSlots...),
			listing(1, 600, delayed(vfrSlots, 2, 6000)...), false},
		{"a source timed by presentation timestamps is held to no delay", listing(1, 600, vfrSlots...),
			listing(1, 600, delayed(vfrSlots, 2, 0)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.src, tc.out, nil)
			if tc.pass && err != nil {
				t.Errorf("Check refused an output shown as the decoder shows the source: %v", err)
			}
			if !tc.pass && (err == nil || !strings.Contains(err.Error(), "the encode moved the source's frame timing")) {
				t.Errorf("Check = %v, want the output refused as moved", err)
			}
		})
	}
}

// TestCheck_AnUntimedShownPacketAsksForTheStart: a packet a decoder may show with no timestamp
// of either kind is never counted as a frame on the packets' word alone - a decoder may drop it
// (an open GOP's leading picture) - so the start is asked for even where the counts agree.
func TestCheck_AnUntimedShownPacketAsksForTheStart(t *testing.T) {
	var need *NeedsStart
	err := Check(packets(at(0, "key"), at(0, "na"), at(0, "na"), at(125), at(42), at(83)), ms(0, 42, 83, 125, 167, 208), nil)
	if !errors.As(err, &need) || need.Packets != 6 {
		t.Errorf("Check = %v, want the start asked for", err)
	}
	if err := Check(untimed(3), ms(0, 42, 83), nil); !errors.As(err, &need) {
		t.Errorf("Check of an untimed source = %v, want the start asked for", err)
	}
}

// TestCheck_TheLastFramesOfADecodeTimedSourceKeepItsRecentCadence: the last d frames of a source
// shown d frames on may each follow the one before by the longest of the source's last d+1
// intervals, the earliest of them included.
func TestCheck_TheLastFramesOfADecodeTimedSourceKeepItsRecentCadence(t *testing.T) {
	slots := []int64{0, 25, 50, 75, 100, 130, 150, 170}
	out := append(append([]int64(nil), slots[2:]...), 200, 230)
	if err := Check(dtsListing(1, 600, slots...), listing(1, 600, out...), nil); err != nil {
		t.Errorf("Check refused last frames within the source's longest recent interval: %v", err)
	}
	out[len(out)-1]++
	out[len(out)-1]++
	if err := Check(dtsListing(1, 600, slots...), listing(1, 600, out...), nil); err == nil {
		t.Error("Check passed a last frame two ticks past the source's longest recent interval")
	}
}

// TestCheck_ADecodeTimedSourcesTolerance: a source timed by decode timestamps stores exact slots,
// so only the output's rounding is allowed for - one tick of the output's base - and none where
// that tick is as long as the source's shortest frame.
func TestCheck_ADecodeTimedSourcesTolerance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, out probe.Timestamps
		pass     bool
	}{
		{"an AVI at one frame per tick written back into AVI, the same slots", dtsListing(1, 25, 0, 1, 2, 3, 4),
			dtsListing(1, 25, 3, 4, 5, 6, 7), true},
		{"an AVI at one frame per tick into Matroska", dtsListing(1, 25, 0, 1, 2, 3, 4), ms(0, 40, 80, 120, 160), true},
		{"into Matroska, each frame rounded to the millisecond", dtsListing(1, 600, 0, 25, 25, 50),
			ms(0, 42, 42, 83), true},
		{"a source whose shortest frame is one tick of the output's base: the same tick", dtsListing(1, 1000, 0, 1, 41),
			dtsListing(1, 1000, 0, 2, 41), false},
		{"a source whose frames are longer than the output's tick: one tick", dtsListing(1, 1000, 0, 40, 81),
			dtsListing(1, 1000, 0, 41, 81), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Check(tc.src, tc.out, nil); tc.pass != (err == nil) {
				t.Errorf("Check = %v, want pass %v", err, tc.pass)
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

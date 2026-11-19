// Package timing is the timestamp half of the length gate: an output replaces its source only
// when its video is presented at the source's own times, frame for frame
// (docs/design/swap.md#timing-parity).
//
// No other gate sees presentation timing. The duration and packet-count checks measure how
// long the video is and how many frames it has, and both hold when an encoder rounds every
// timestamp onto a coarser grid; the full decode is a linear decode that never asks when a
// frame is shown; the perceptual gate compares frame by frame. An encode that collapsed a
// variable-frame-rate source onto one rate, presenting two frames at the same instant and the
// rest off their times, passes every one of them.
package timing

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/NSchatz/holdfast/internal/probe"
)

// NeedsStart is what Check returns, instead of a verdict, where the output shows a different
// number of frames than the source has packets left to show, or where a packet the source shows
// carries no timestamp: a decoder starting at the source's first packet may not show all of them
// (a stream cut between keyframes, an open GOP's or a CRA's leading pictures), and only a decode
// of that start says how many it does, and when. The caller decodes the source's first Packets
// packets (probe.Prober.VideoStart) and asks again.
type NeedsStart struct {
	Packets int
}

func (n *NeedsStart) Error() string {
	return fmt.Sprintf("timing parity needs the source's first %d video packets decoded to count the frames "+
		"they show", n.Packets)
}

// Check is the gate: nil when out's video is presented at src's times, an error naming the
// first difference otherwise, or a *NeedsStart where the source's start has to be decoded first
// (start is that decode, nil until it is asked for). It compares the frames a decoder shows of
// each file, and holds the output to three things:
//
//   - as many frames as the source shows: an encode makes one frame of each;
//   - no more frames presented at the same instant as the one before than the source has: a
//     faithful encode adds none, and a collapsed one does;
//   - each output frame presented where the source's frame at the same place in presentation
//     order is, within the tolerance below. Each file's times are read as positions from its
//     own first presented frame, because an encode may move the whole timeline (ffmpeg
//     rebases a copy to start at 0, and a muxer may delay it), and a constant move changes
//     nothing a viewer sees.
//
// # The tolerance: one tick
//
// An encode that carries timestamps through moves none of them: it rescales each from the
// source's time base into the output's, and the only difference that can leave is the
// rounding of that rescale - at most half a tick at a frame, and half again at the first frame
// its position is read from, so one tick in all (1 ms in Matroska, 1/90000 s in MPEG-TS). For
// a source timed by presentation timestamps it is a tick of the coarser of the two bases, which
// also takes the rounding a source's own muxer made of a rate the encode reads exactly (a
// converted Dolby Vision stream). A source timed by decode timestamps alone stores each frame's
// exact slot, so the only rounding is the output's: one tick of the output's base, and none at
// all where that tick is as long as the source's shortest frame (an AVI at 1/25 written back
// into AVI), since a rounding that long would be a whole frame. Anything beyond that is a time
// the encode changed.
//
// A tolerance of half a source frame would be too loose for the class this gate exists for.
// An encoder that rounds a 24 fps stretch onto a 30 fps grid moves frames by up to 16.7 ms,
// under half a 24 fps frame (20.8 ms), with no two frames colliding; this tolerance refuses
// it at the first frame it moves by more than a tick.
func Check(src, out probe.Timestamps, start *probe.Start) error {
	o, timed := outputTimes(out)
	if !timed {
		return fmt.Errorf("timing parity could not be established: not every frame of the output's video " +
			"carries a timestamp, so when its frames are shown cannot be read")
	}
	s, err := sourceTimes(src, start)
	if err != nil {
		return err
	}
	if s.needsStart || (start == nil && s.n != len(o)) {
		return &NeedsStart{Packets: window(src)}
	}
	if s.n != len(o) {
		return fmt.Errorf("timing parity failed: the output's video shows %d frames and the source's %d (its "+
			"first %d packets decoded show %d) - an encode makes one frame of each frame the source shows",
			len(o), s.n, start.Packets, len(start.Frames))
	}
	if s.n == 0 {
		return fmt.Errorf("timing parity could not be established: neither video stream shows a frame")
	}

	// A source can carry frames on one instant itself; an output that reproduces them has
	// damaged nothing, and one that adds any has collapsed the timing.
	srcRepeats, _ := repeats(s.ticks)
	if n, first := repeats(o); n > srcRepeats {
		return fmt.Errorf("timing parity failed: %d of the output's %d video frames are presented at the same "+
			"instant as the frame before them where %d of the source's are (the first at %s from its first "+
			"frame) - the encode collapsed the source's frame timing", n, len(o), srcRepeats,
			seconds(o[first]-o[0], out))
	}
	// A source none of whose frames carries a time - a raw elementary stream, which ffmpeg times
	// at one guessed rate as it reads it - offers none to hold the output's frames to. What is
	// checkable is checked, the count and an output that never shows two frames at once, and it
	// is not refused for what it does not carry.
	if s.ticks == nil {
		return nil
	}
	m := newMatcher(src, out, s)
	if s.fromDTS {
		return m.shifted(o, s.ticks, src, out)
	}
	if at, moved := mismatch(m, o, s.ticks); moved {
		return moveError(o, s.ticks, at, src, out, m)
	}
	return nil
}

// times are the frames a decoder shows of the source, in time: how many, and their times,
// sorted, or no times at all where none of them carries one.
type times struct {
	n     int
	ticks []int64
	// fromDTS: the source is timed by decode timestamps alone - every shown packet carries one,
	// and not every one a presentation timestamp (AVI and ASF, which store none, and an MPEG-4
	// Part 2 stream whose B-frames carry none).
	fromDTS bool
	// needsStart: some shown packet carries no timestamp, so only a decode can say which of them
	// a decoder shows and when.
	needsStart bool
}

// sourceTimes reads the frames a decoder shows of src.
//
// Without start they are its packets but those flagged to discard, each timed by its
// presentation timestamp, or every one by its decode timestamp where they all carry one and not
// all a presentation timestamp; where some carry neither, the start has to be decoded.
//
// With start, a source timed by decode timestamps is its packets from its first keyframe on: the
// decode must show that a decoder drops exactly the packets before that keyframe, and the rest
// keep their decode timestamps (and the reordering delay they are shown at). Any other source is
// the frames the decode showed, timed as the decoder timed them, then the rest of its packets,
// timed by their presentation timestamps; a frame among them with no time is refused, since where
// it is shown cannot be read and ffmpeg itself gives it another frame's instant, unless no frame
// carries a time at all.
func sourceTimes(src probe.Timestamps, start *probe.Start) (times, error) {
	all := shown(src.Packets)
	allPTS := !slices.ContainsFunc(all, func(k probe.Packet) bool { return !k.HasPTS })
	allDTS := !slices.ContainsFunc(all, func(k probe.Packet) bool { return !k.HasDTS })
	var t times
	switch {
	case !allPTS && allDTS:
		t.fromDTS = true
		p := all
		if start != nil {
			cut := min(start.Packets, len(src.Packets))
			dropped := len(shown(src.Packets[:cut])) - len(start.Frames)
			key := max(slices.IndexFunc(p, func(k probe.Packet) bool { return k.Key }), 0)
			if dropped != key {
				return times{}, fmt.Errorf("timing parity could not be established: a decoder drops %d of the "+
					"source's first %d video packets where %d come before its first keyframe, so which frames it "+
					"shows cannot be read from its decode timestamps", dropped, cut, key)
			}
			p = p[key:]
		}
		for _, k := range p {
			t.ticks = append(t.ticks, k.DTS)
		}
	case start == nil:
		if !allPTS {
			return times{needsStart: true}, nil
		}
		for _, k := range all {
			t.ticks = append(t.ticks, k.PTS)
		}
	default:
		untimed := 0
		for _, f := range start.Frames {
			if f.HasTS {
				t.ticks = append(t.ticks, f.TS)
			} else {
				untimed++
			}
		}
		for _, k := range shown(src.Packets[min(start.Packets, len(src.Packets)):]) {
			if k.HasPTS {
				t.ticks = append(t.ticks, k.PTS)
			} else {
				untimed++
			}
		}
		t.n = len(t.ticks) + untimed
		if untimed > 0 && len(t.ticks) > 0 {
			return times{}, fmt.Errorf("timing parity could not be established: %d of the %d frames a decoder "+
				"shows of the source carry no timestamp, so where they are shown cannot be read", untimed, t.n)
		}
		slices.Sort(t.ticks)
		return t, nil
	}
	t.n = len(t.ticks)
	slices.Sort(t.ticks)
	return t, nil
}

// outputTimes are the output's shown frames' times, sorted - presentation timestamps, or decode
// timestamps where none carries a presentation timestamp and every one a decode timestamp (an
// output in AVI or ASF) - and whether every frame is timed.
func outputTimes(out probe.Timestamps) ([]int64, bool) {
	p := shown(out.Packets)
	var pts, dts []int64
	for _, k := range p {
		if k.HasPTS {
			pts = append(pts, k.PTS)
		}
		if k.HasDTS {
			dts = append(dts, k.DTS)
		}
	}
	switch {
	case len(pts) == len(p):
		return slices.Sorted(slices.Values(pts)), true
	case len(pts) == 0 && len(dts) == len(p):
		return slices.Sorted(slices.Values(dts)), true
	}
	return nil, false
}

// shown is the packets a decoder shows a frame for, in file order: every one but those the
// demuxer flags to discard (an MP4 edit list that starts after the keyframe a cut began at keeps
// the packets it needs to decode from there and flags them).
func shown(p []probe.Packet) []probe.Packet {
	var out []probe.Packet
	for _, k := range p {
		if !k.Discard {
			out = append(out, k)
		}
	}
	return out
}

// window is how many of the source's first packets a decode has to read to see every frame a
// decoder starting there might not show: the packets up to and including its first keyframe (a
// stream cut between keyframes), and 3*MaxReorder more - room for the leading pictures a GOP
// places before its keyframe (an open GOP's or a CRA's, which a decoder drops, and a closed GOP's
// or an IDR's, which it shows), the pictures they reference and the decoder's reordering delay,
// each at most MaxReorder.
func window(src probe.Timestamps) int {
	k := max(slices.IndexFunc(src.Packets, func(q probe.Packet) bool { return q.Key }), 0)
	return min(k+1+3*MaxReorder, len(src.Packets))
}

// MaxReorder is the most frames a decoder holds back to reorder them: H.264's
// max_num_reorder_frames and HEVC's sps_max_num_reorder_pics are both at most 16.
const MaxReorder = 16

// matcher compares positions in two time bases exactly. With the source's base sn/sd and the
// output's on/od, a span do of output ticks and ds of source ticks differ by
// (do*on*sd - ds*sn*od) / (od*sd) seconds, and one tick of the output's base is on*sd / (od*sd),
// one of the source's sn*od / (od*sd): so the two spans are within the tolerance exactly when
// |do*on*sd - ds*sn*od| <= bound, the tolerance in those units. The products overflow 64 bits on
// a long film in a fine time base, so they are taken in big integers, reused across frames.
type matcher struct {
	outScale, srcScale, bound *big.Int
	a, b, diff                big.Int
}

// newMatcher sets the tolerance Check documents: a tick of the coarser base for a source timed
// by presentation timestamps; for one timed by decode timestamps, a tick of the output's base, or
// nothing where that tick is at least the source's shortest frame.
func newMatcher(src, out probe.Timestamps, s times) *matcher {
	m := &matcher{
		outScale: new(big.Int).Mul(big.NewInt(out.TimeBaseNum), big.NewInt(src.TimeBaseDen)),
		srcScale: new(big.Int).Mul(big.NewInt(src.TimeBaseNum), big.NewInt(out.TimeBaseDen)),
	}
	m.bound = m.outScale
	switch {
	case !s.fromDTS && m.srcScale.Cmp(m.outScale) > 0:
		m.bound = m.srcScale
	case s.fromDTS:
		if shortest, ok := shortestInterval(s.ticks); ok &&
			m.outScale.Cmp(new(big.Int).Mul(big.NewInt(shortest), m.srcScale)) >= 0 {
			m.bound = new(big.Int)
		}
	}
	return m
}

// shortestInterval is the shortest positive interval between consecutive sorted ticks, and
// whether there is one.
func shortestInterval(sorted []int64) (int64, bool) {
	shortest, ok := int64(0), false
	for i := 1; i < len(sorted); i++ {
		if d := sorted[i] - sorted[i-1]; d > 0 && (!ok || d < shortest) {
			shortest, ok = d, true
		}
	}
	return shortest, ok
}

// cmp is -1 where the output span do is shorter than the source span ds by more than the
// tolerance, 1 where it is longer by more, and 0 where the two are within it.
func (m *matcher) cmp(do, ds int64) int {
	m.a.Mul(m.a.SetInt64(do), m.outScale)
	m.b.Mul(m.b.SetInt64(ds), m.srcScale)
	m.diff.Sub(&m.a, &m.b)
	switch {
	case m.diff.CmpAbs(m.bound) <= 0:
		return 0
	case m.diff.Sign() < 0:
		return -1
	}
	return 1
}

// shifted holds the output to a source timed by decode timestamps alone. Such a source is shown
// at them moved along by its reordering delay: a decoder holding d frames back times the frame it
// shows i-th by the decode timestamp of packet i+d (measured: an H.264 AVI with two B-frames is
// shown at dts[i+2], an MPEG-4 Part 2 one at dts[i+1]), and times its last d frames by repeating
// its last frame's duration (fftools/ffmpeg_dec.c video_duration_estimate at 5d4d3bdc61). The
// delay is not in the listing, so each d up to MaxReorder is tried, never one that leaves out
// more frames than it compares, and the output must match one exactly: frame for frame for every
// frame with a decode timestamp d packets on, and each of the last d frames presented after the
// one before it by no more than the source's longest interval among its last d+1, plus the
// tolerance.
func (m *matcher) shifted(o, s []int64, src, out probe.Timestamps) error {
	at, moved := mismatch(m, o, s)
	if !moved {
		return nil
	}
	for d := 1; d <= min(MaxReorder, (len(s)-1)/2); d++ {
		if _, shiftMoved := mismatch(m, o, s[d:]); !shiftMoved && m.tail(o, s, d) {
			return nil
		}
	}
	return moveError(o, s, at, src, out, m)
}

// moveError names the output frame at that is not where the source's frame at the same place is.
func moveError(o, s []int64, at int, src, out probe.Timestamps, m *matcher) error {
	tolerance := "one tick of the coarser time base (" + coarser(src, out) + ")"
	if m.bound.Sign() == 0 {
		tolerance = "the same tick"
	}
	return fmt.Errorf("timing parity failed: the output's video frame %d is presented at %s from its "+
		"first frame and the source's at %s - further apart than %s - the encode moved the source's frame "+
		"timing", at, seconds(o[at]-o[0], out), seconds(s[at]-s[0], src), tolerance)
}

// mismatch is the first frame of o not within the tolerance of s's frame at the same place, both
// read from their own first frame, over the frames s has, and whether there is one.
func mismatch(m *matcher, o, s []int64) (at int, found bool) {
	for i := range s {
		if m.cmp(o[i]-o[0], s[i]-s[0]) != 0 {
			return i, true
		}
	}
	return 0, false
}

// tail reports whether each of the output's last d frames follows the one before it by no more
// than the source's longest interval among its last d+1, plus the tolerance.
func (m *matcher) tail(o, s []int64, d int) bool {
	longest := int64(0)
	for i := len(s) - d - 1; i < len(s); i++ {
		longest = max(longest, s[i]-s[i-1])
	}
	for i := len(o) - d; i < len(o); i++ {
		if m.cmp(o[i]-o[i-1], longest) > 0 {
			return false
		}
	}
	return true
}

// repeats counts the frames of a sorted list presented at the same instant as the one before,
// and the index of the first.
func repeats(sorted []int64) (n, first int) {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			if n == 0 {
				first = i
			}
			n++
		}
	}
	return n, first
}

// seconds renders a span of ticks in t's time base as seconds, to the microsecond.
func seconds(ticks int64, t probe.Timestamps) string {
	span := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(t.TimeBaseNum))
	return new(big.Rat).SetFrac(span, big.NewInt(t.TimeBaseDen)).FloatString(6) + "s"
}

// coarser names the larger of the two files' ticks, as a time base.
func coarser(src, out probe.Timestamps) string {
	a := big.NewRat(src.TimeBaseNum, src.TimeBaseDen)
	b := big.NewRat(out.TimeBaseNum, out.TimeBaseDen)
	if b.Cmp(a) > 0 {
		a = b
	}
	return a.String()
}

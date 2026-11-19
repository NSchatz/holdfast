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

// Check is the gate: nil when out's video is presented at src's times, an error naming the
// first difference otherwise. It compares the frames a decoder shows of each file (shown), and
// holds the output to three things:
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
// # The tolerance: one tick of the coarser time base
//
// An encode that carries timestamps through moves none of them: it rescales each from the
// source's time base into the output's, and the only difference that can leave is the
// rounding of that rescale - at most half a tick of the coarser base at a frame, and half
// again at the first frame its position is read from, so one tick in all (1 ms in
// Matroska, 1/90000 s in MPEG-TS). Anything beyond that is a time the encode changed.
//
// A tolerance of half a source frame would be too loose for the class this gate exists for.
// An encoder that rounds a 24 fps stretch onto a 30 fps grid moves frames by up to 16.7 ms,
// under half a 24 fps frame (20.8 ms), with no two frames colliding; this tolerance refuses
// it at the first frame it moves by more than a tick.
func Check(src, out probe.Timestamps) error {
	sp, op := shown(src), shown(out)
	if len(sp) != len(op) {
		// A source that starts where a decoder cannot show its first packets - a cut between
		// keyframes, an open GOP, an HEVC CRA with leading pictures - is accepted only where
		// leaving those packets out accounts for the difference exactly.
		lead := leading(sp)
		if len(lead) != len(op) {
			return fmt.Errorf("timing parity failed: the output's video shows %d frames and the source's %d, or "+
				"%d without the %d leading packets a decoder starting there cannot show - an encode makes one frame "+
				"of each frame the source shows", len(op), len(sp), len(lead), len(sp)-len(lead))
		}
		sp = lead
	}
	if len(sp) == 0 {
		return fmt.Errorf("timing parity could not be established: neither video stream shows a frame")
	}
	s, o := read(src, sp), read(out, op)
	if o.untimed {
		return fmt.Errorf("timing parity could not be established: not every frame of the output's video " +
			"carries a timestamp, so when its frames are shown cannot be read")
	}

	// A source can carry frames on one instant itself; an output that reproduces them has
	// damaged nothing, and one that adds any has collapsed the timing.
	srcRepeats := 0
	if !s.untimed {
		srcRepeats, _ = repeats(s.ticks)
	}
	if n, first := repeats(o.ticks); n > srcRepeats {
		return fmt.Errorf("timing parity failed: %d of the output's %d video frames are presented at the same "+
			"instant as the frame before them where %d of the source's are (the first at %s from its first "+
			"frame) - the encode collapsed the source's frame timing", n, o.n, srcRepeats,
			seconds(o.ticks[first]-o.ticks[0], o))
	}
	// A source that does not time every frame - a raw elementary stream, which stores no time
	// at all and which ffmpeg times at one guessed rate as it reads it - offers no time to hold
	// the output's frames to. What is checkable is checked, the count and an output that never
	// shows two frames at once, and it is not refused for what it does not carry.
	if s.untimed {
		return nil
	}

	// A source timed only by decode timestamps is shown at them moved along by its reordering
	// delay: a decoder holding d frames back times the frame it shows i-th by the decode
	// timestamp of packet i+d (measured: an H.264 AVI with two B-frames is shown at dts[i+2],
	// an MPEG-4 Part 2 one at dts[i+1]). The delay is not in the listing, so each d up to
	// MaxReorder is tried, and the output must match one exactly, frame for frame, for every
	// frame that has a decode timestamp d packets on; the last d frames, which a decoder times
	// by extrapolation at the end of the stream, are held to the count and to never sharing an
	// instant. A shift never leaves out more frames than it compares, so a short file is not
	// matched on one frame. A source timed by presentation timestamps is shown at them, and is
	// held to d = 0 alone.
	m := newMatcher(s, o)
	at, moved := m.mismatch(o.ticks, s.ticks)
	if !moved {
		return nil
	}
	if s.fromDTS {
		for d := 1; d <= min(MaxReorder, (len(s.ticks)-1)/2); d++ {
			if _, shifted := m.mismatch(o.ticks, s.ticks[d:]); !shifted {
				return nil
			}
		}
	}
	return fmt.Errorf("timing parity failed: the output's video frame %d is presented at %s from its "+
		"first frame and the source's at %s - further apart than one tick of the coarser time base (%s) - "+
		"the encode moved the source's frame timing", at, seconds(o.ticks[at]-o.ticks[0], o),
		seconds(s.ticks[at]-s.ticks[0], s), coarser(s, o))
}

// frames is what a decoder shows of a listing, read in time: how many frames, and the times
// they are shown at, sorted.
type frames struct {
	n        int
	num, den int64
	// ticks are the frames' presentation timestamps where every shown packet carries one;
	// otherwise their decode timestamps where every one carries that (fromDTS: AVI and ASF,
	// which store none, and an MPEG-4 Part 2 stream whose B-frames carry none); otherwise
	// nothing, and the listing is untimed: a raw elementary stream, or a stream timed in part.
	ticks   []int64
	fromDTS bool
	untimed bool
}

// read is the frames a listing's shown packets p are.
func read(ts probe.Timestamps, p []probe.Packet) frames {
	f := frames{n: len(p), num: ts.TimeBaseNum, den: ts.TimeBaseDen}
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
		f.ticks = pts
	case len(dts) == len(p):
		f.ticks, f.fromDTS = dts, true
	default:
		f.untimed = true
	}
	slices.Sort(f.ticks)
	return f
}

// shown is the packets of a listing a decoder shows a frame for, in file order: every one but
// those the demuxer flags to discard (an MP4 edit list that starts after the keyframe a cut
// began at keeps the packets it needs to decode from there and flags them).
func shown(ts probe.Timestamps) []probe.Packet {
	var p []probe.Packet
	for _, k := range ts.Packets {
		if !k.Discard {
			p = append(p, k)
		}
	}
	return p
}

// leading is p without the packets a decoder starting at the stream's first packet cannot show:
// those before the first keyframe in file order, which reference a picture the file does not
// carry, and those after it presented before it (an open GOP's leading pictures, an HEVC CRA's
// skipped leading pictures), whose references lie before it too. A packet with no presentation
// timestamp after a keyframe that has one is presented before it: that is how Matroska reads
// back a block timed before zero. With no keyframe flagged, nothing is left out.
func leading(p []probe.Packet) []probe.Packet {
	k := slices.IndexFunc(p, func(q probe.Packet) bool { return q.Key })
	if k < 0 {
		return p
	}
	key := p[k]
	kept := []probe.Packet{key}
	for _, q := range p[k+1:] {
		if !key.HasPTS || (q.HasPTS && q.PTS >= key.PTS) {
			kept = append(kept, q)
		}
	}
	return kept
}

// MaxReorder is the most frames a decoder holds back to reorder them: H.264's
// max_num_reorder_frames and HEVC's sps_max_num_reorder_pics are both at most 16.
const MaxReorder = 16

// matcher compares positions in two time bases exactly. With the source's base sn/sd and the
// output's on/od, frame i's two positions differ by (do*on*sd - ds*sn*od) / (od*sd) seconds,
// and one tick of the coarser base is max(on*sd, sn*od) / (od*sd): so a frame is within the
// tolerance exactly when |do*on*sd - ds*sn*od| <= max(on*sd, sn*od). The products overflow 64
// bits on a long film in a fine time base, so they are taken in big integers, reused across
// frames.
type matcher struct {
	outScale, srcScale, bound *big.Int
	a, b, diff                big.Int
}

func newMatcher(src, out frames) *matcher {
	m := &matcher{
		outScale: new(big.Int).Mul(big.NewInt(out.num), big.NewInt(src.den)),
		srcScale: new(big.Int).Mul(big.NewInt(src.num), big.NewInt(out.den)),
	}
	m.bound = m.outScale
	if m.srcScale.Cmp(m.outScale) > 0 {
		m.bound = m.srcScale
	}
	return m
}

// mismatch is the first frame of o not within one tick of s's frame at the same place, both read
// from their own first frame, over the frames s has, and whether there is one.
func (m *matcher) mismatch(o, s []int64) (at int, found bool) {
	for i := range s {
		m.a.Mul(m.a.SetInt64(o[i]-o[0]), m.outScale)
		m.b.Mul(m.b.SetInt64(s[i]-s[0]), m.srcScale)
		m.diff.Sub(&m.a, &m.b)
		if m.diff.CmpAbs(m.bound) > 0 {
			return i, true
		}
	}
	return 0, false
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

// seconds renders a span of ticks in f's time base as seconds, to the microsecond.
func seconds(ticks int64, f frames) string {
	span := new(big.Int).Mul(big.NewInt(ticks), big.NewInt(f.num))
	return new(big.Rat).SetFrac(span, big.NewInt(f.den)).FloatString(6) + "s"
}

// coarser names the larger of the two files' ticks, as a time base.
func coarser(src, out frames) string {
	a := big.NewRat(src.num, src.den)
	b := big.NewRat(out.num, out.den)
	if b.Cmp(a) > 0 {
		a = b
	}
	return a.String()
}

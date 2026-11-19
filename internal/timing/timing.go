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
// first difference otherwise. It holds the output to three things:
//
//   - the same number of video packets as the source: an encode makes one frame of each;
//   - no two output packets presented at the same instant: a faithful encode never shows two
//     frames at once, and a collapsed one does;
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
	if out.Packets != src.Packets {
		return fmt.Errorf("timing parity failed: the output's video carries %d packets and the source's %d - "+
			"an encode makes one frame of each source frame", out.Packets, src.Packets)
	}
	if src.Packets == 0 {
		return fmt.Errorf("timing parity could not be established: neither video stream carries a packet")
	}
	if out.Untimed {
		return fmt.Errorf("timing parity could not be established: not every packet of the output's video " +
			"carries a timestamp, so when its frames are shown cannot be read")
	}
	if len(out.Ticks) != out.Packets || (!src.Untimed && len(src.Ticks) != src.Packets) {
		return fmt.Errorf("timing parity could not be established: the output's video carries %d timestamps "+
			"for %d packets and the source's %d for %d", len(out.Ticks), out.Packets, len(src.Ticks), src.Packets)
	}
	o := slices.Sorted(slices.Values(out.Ticks))

	if n, first := repeats(o); n > 0 {
		return fmt.Errorf("timing parity failed: %d of the output's %d video frames are presented at the same "+
			"instant as the frame before them (the first at %s from its first frame) - the encode collapsed the "+
			"source's frame timing", n, len(o), seconds(o[first]-o[0], out))
	}
	// A source that does not time every frame - a raw elementary stream, which stores no time
	// at all and which ffmpeg times at one guessed rate as it reads it - offers no time to hold
	// the output's frames to. What is checkable is checked, the count and an output that never
	// shows two frames at once, and it is not refused for what it does not carry.
	if src.Untimed {
		return nil
	}
	s := slices.Sorted(slices.Values(src.Ticks))

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
	m := newMatcher(src, out)
	at := m.mismatch(o, s)
	if at < 0 {
		return nil
	}
	if src.FromDTS {
		for d := 1; d <= min(MaxReorder, (len(s)-1)/2); d++ {
			if m.mismatch(o, s[d:]) < 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("timing parity failed: the output's video frame %d is presented at %s from its "+
		"first frame and the source's at %s - further apart than one tick of the coarser time base (%s) - "+
		"the encode moved the source's frame timing", at, seconds(o[at]-o[0], out), seconds(s[at]-s[0], src),
		coarser(src, out))
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

func newMatcher(src, out probe.Timestamps) *matcher {
	m := &matcher{
		outScale: new(big.Int).Mul(big.NewInt(out.TimeBaseNum), big.NewInt(src.TimeBaseDen)),
		srcScale: new(big.Int).Mul(big.NewInt(src.TimeBaseNum), big.NewInt(out.TimeBaseDen)),
	}
	m.bound = m.outScale
	if m.srcScale.Cmp(m.outScale) > 0 {
		m.bound = m.srcScale
	}
	return m
}

// mismatch is the first frame of o not within one tick of s's frame at the same place, both read
// from their own first frame, over the frames s has, or -1 where every one is.
func (m *matcher) mismatch(o, s []int64) int {
	for i := range s {
		m.a.Mul(m.a.SetInt64(o[i]-o[0]), m.outScale)
		m.b.Mul(m.b.SetInt64(s[i]-s[0]), m.srcScale)
		m.diff.Sub(&m.a, &m.b)
		if m.diff.CmpAbs(m.bound) > 0 {
			return i
		}
	}
	return -1
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

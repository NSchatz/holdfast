// Package chapters is the chapter-parity gate: an output replaces its source only when it
// carries the source's chapter table - the same number of chapters, with the same titles,
// starting and ending where the source's do - and every chapter the source places inside the
// film lies inside the output (docs/design/swap.md#chapter-parity).
//
// No other gate sees a chapter table. Chapters are container metadata: they are not a
// stream, so the intended-stream check cannot count them, and they are not pictures, so the
// decode and perceptual gates cannot score them. An output whose chapters were dropped,
// shifted or pushed past its end passes every other gate exactly as a faithful one does.
package chapters

import (
	"fmt"
	"math"

	"github.com/NSchatz/holdfast/internal/probe"
)

// Tolerance is how far, in seconds, an output chapter's start or end may sit from its
// source chapter's and still be the same chapter: 100 ms.
//
// An encode does not re-time chapters, it copies them, so the only differences a faithful
// copy can carry are these two, and the bound covers both with room to spare:
//
//   - the rounding of a rescale between time bases: Matroska stores a chapter in
//     nanoseconds and MP4 in its movie timescale (milliseconds in ffmpeg's muxer), so one
//     tick of the coarser base, at most 1 ms;
//   - where each file's timeline starts. Chapters are compared as positions into the file
//     (a time minus the file's start_time, the position a player shows), and an encode can
//     move the first presented timestamp by an encoder delay or a reordering of a frame or
//     two - two frames at 23.976 fps is 83 ms.
//
// A crop, a scale or a deinterlace changes no timestamp, so none of them widens it. It is
// still far below anything a viewer can see: a player seeking to a chapter lands on the
// keyframe before it, usually seconds away, and the damage this gate exists for is measured
// in whole chapters and thousands of seconds.
const Tolerance = 0.1

// epsilon absorbs the binary rounding of the decimal times ffprobe prints, so a bound means
// what it says: 30.1 s against 30 s is a difference of exactly the tolerance, not one
// rounding error past it.
const epsilon = 1e-9

// beyond reports whether a exceeds the bound b by more than a rounding error.
func beyond(a, b float64) bool { return a > b+epsilon }

// Check is the gate: nil when out carries src's chapter table, an error naming the first
// difference otherwise.
//
// durationTolerance is the length gate's own drift allowance (duration_tolerance_sec). It
// widens the in-duration check by exactly what the length gate already accepted between the
// two files, so a source chapter ending on the source's last frame is not refused because a
// faithful output is a few milliseconds shorter.
//
// # A source whose own table is malformed
//
// A chapter the SOURCE places past its own end, ending before it starts, or overlapping or
// out of order with the chapter before it, is a defect of the source, and malformed lists
// each one. It does not refuse the job: the question this gate answers is whether the encode
// damaged what was there, and an output that reproduces the source's table exactly has not.
// Refusing it would keep a file whose table is just as broken and make every source with a
// sloppy chapter (a last chapter ending a frame past the end, the commonest) unencodable for
// a defect the swap neither causes nor changes. So parity is still enforced chapter by
// chapter, and only the in-duration check is waived for a chapter the source itself places
// outside the film; the caller logs malformed so the operator learns the source's table was
// already broken. Where the source's duration is unknown nothing is waived: an output chapter
// past the output's end is then refused, the fail-safe direction.
func Check(src, out probe.ChapterTable, durationTolerance float64) (malformed []string, err error) {
	if len(out.Chapters) != len(src.Chapters) {
		return nil, fmt.Errorf("chapter parity failed: the output carries %d chapter(s) and the source carries %d",
			len(out.Chapters), len(src.Chapters))
	}
	if len(src.Chapters) == 0 {
		return nil, nil
	}
	switch {
	case !src.StartKnown:
		return nil, fmt.Errorf("chapter parity could not be established: the source reports no start time, " +
			"so its chapters cannot be placed on its timeline")
	case !out.StartKnown:
		return nil, fmt.Errorf("chapter parity could not be established: the output reports no start time, " +
			"so its chapters cannot be placed on its timeline")
	}
	for i := range src.Chapters {
		s, o := position(src, i), position(out, i)
		n := i + 1
		if s.Title != o.Title {
			return nil, fmt.Errorf("chapter parity failed: chapter %d is titled %q in the output and %q in the source",
				n, o.Title, s.Title)
		}
		if beyond(math.Abs(o.Start-s.Start), Tolerance) {
			return nil, fmt.Errorf("chapter parity failed: chapter %d (%q) starts at %.3fs in the output and "+
				"%.3fs in the source (tolerance %gs)", n, s.Title, o.Start, s.Start, Tolerance)
		}
		if beyond(math.Abs(o.End-s.End), Tolerance) {
			return nil, fmt.Errorf("chapter parity failed: chapter %d (%q) ends at %.3fs in the output and "+
				"%.3fs in the source (tolerance %gs)", n, s.Title, o.End, s.End, Tolerance)
		}
		defects, outside := sourceDefects(src, i)
		for _, d := range defects {
			malformed = append(malformed, fmt.Sprintf("chapter %d (%q) %s", n, s.Title, d))
		}
		if outside || !out.DurationKnown {
			continue
		}
		if !beyond(out.Duration, o.Start) {
			return nil, fmt.Errorf("chapter range check failed: chapter %d (%q) starts at %.3fs, at or after "+
				"the end of the %.3fs output", n, s.Title, o.Start, out.Duration)
		}
		if beyond(o.End, out.Duration+Tolerance+durationTolerance) {
			return nil, fmt.Errorf("chapter range check failed: chapter %d (%q) ends at %.3fs, after the end "+
				"of the %.3fs output", n, s.Title, o.End, out.Duration)
		}
	}
	return malformed, nil
}

// position is chapter i of t as a position into the file: its start and end less the file's
// start time, never before the file's first instant (which is where ffmpeg places a chapter
// that began before the first packet when it rebases a copy to start at 0).
func position(t probe.ChapterTable, i int) probe.Chapter {
	c := t.Chapters[i]
	return probe.Chapter{
		Start: math.Max(0, c.Start-t.StartTime),
		End:   math.Max(0, c.End-t.StartTime),
		Title: c.Title,
	}
}

// sourceDefects names what is wrong with chapter i of the source's own table, and whether
// the source places it outside its own duration (the one defect that waives the output's
// in-duration check). A source of unknown duration places nothing outside it.
func sourceDefects(src probe.ChapterTable, i int) (defects []string, outside bool) {
	c := position(src, i)
	if beyond(c.Start, c.End) {
		defects = append(defects, fmt.Sprintf("ends at %.3fs, before it starts at %.3fs", c.End, c.Start))
	}
	if src.DurationKnown {
		if !beyond(src.Duration, c.Start) {
			defects = append(defects, fmt.Sprintf("starts at %.3fs, at or after the end of the %.3fs source",
				c.Start, src.Duration))
			outside = true
		} else if beyond(c.End, src.Duration+Tolerance) {
			defects = append(defects, fmt.Sprintf("ends at %.3fs, after the end of the %.3fs source",
				c.End, src.Duration))
			outside = true
		}
	}
	if i > 0 {
		prev := position(src, i-1)
		switch {
		case beyond(prev.Start, c.Start):
			defects = append(defects, fmt.Sprintf("starts at %.3fs, before the chapter ahead of it (%.3fs)",
				c.Start, prev.Start))
		case beyond(prev.End-Tolerance, c.Start):
			defects = append(defects, fmt.Sprintf("starts at %.3fs, inside the chapter ahead of it (which ends at %.3fs)",
				c.Start, prev.End))
		}
	}
	return defects, outside
}

// Package playability decodes a video file the way a player or a media server starts one,
// rather than the way the decode-integrity gate does. The decode-integrity gate is one
// linear decode from the first frame; a player that seeks or resumes, a segmenter that cuts
// HLS or DASH, and a hardware decoder handed one segment each start at a keyframe in the
// middle of the stream, with nothing decoded before it. This package exercises that path:
//
//   - cold starts (ColdStarts): a fresh decoder handed the stream from a keyframe the
//     container flags, with nothing before it, and a few seconds decoded from there, for a
//     sample of the keyframes spread across the stream and for the keyframes followed by
//     leading pictures;
//   - segments (Segments): the stream cut at its keyframes into segments of at least six
//     seconds, as an HLS or DASH segmenter cuts it, each decoded on its own;
//   - the GOP structure of an HEVC stream (ReadGOP): the NAL unit type of every picture,
//     so a stream declared closed can be held to carrying no RASL picture and no keyframe
//     that is not an IDR or BLA picture.
//
// The first two read one remux of the video into fragmented MP4, the shape HLS and DASH
// stream it in, cut at every keyframe: it reads the file end to end and seeks nothing, so it
// works on a container with no keyframe index (MPEG-TS) exactly as on Matroska. Every decode
// is ffmpeg with -xerror and -err_detect +explode, and any error it prints fails the decode
// even where it exits 0, the reading the health sweep's decode takes. ffprobe only lists
// packets: it swallows decode errors and exits 0, so no verdict is ever read from it.
//
// It is a leaf: it runs the two binaries by path and imports nothing of holdfast's, so the
// engine's gate and a sweep over files already swapped can each call it on one file. What
// it proves and what it does not is docs/design/swap.md#playability.
package playability

import (
	"context"
	"errors"
	"fmt"
)

// DefaultColdStarts is how many keyframes spread across a stream are each decoded from a
// cold start where nothing says otherwise.
const DefaultColdStarts = 16

// LeadingPerColdStart bounds the cold starts at keyframes followed by leading pictures: up
// to this many times the spread sample, themselves spread across every such keyframe. In
// an open-GOP stream they are the keyframes a cold start treats differently from a linear
// decode (it drops their RASL pictures), so they are sampled more densely than the rest.
const LeadingPerColdStart = 4

// Options says which checks Check runs.
type Options struct {
	// ColdStarts is the size of the spread sample of keyframes decoded from a cold start;
	// up to LeadingPerColdStart times as many keyframes followed by leading pictures are
	// decoded from one too. 0 runs no cold start.
	ColdStarts int
	// Segments runs the fragmented-MP4 segment check.
	Segments bool
}

// Checker runs the checks with the two binaries it names.
type Checker struct {
	FFmpeg, FFprobe string
}

// Check runs the checks o asks for over file's first video stream, in one remux of it
// (decodeFragments). s is the stream's packet listing (ReadStream) where the caller has it,
// and nil to have the cold starts read it. The first failure is returned: a *Failure where a
// decode ran and did not come out clean, an *Unrun where a check could not be run at all,
// nil where every check asked for passed or none was.
func (c Checker) Check(ctx context.Context, file string, s *Stream, o Options) error {
	if o.ColdStarts > 0 && s == nil {
		var err error
		if s, err = c.ReadStream(ctx, file); err != nil {
			return err
		}
	}
	return c.decodeFragments(ctx, file, s, o.ColdStarts, o.Segments)
}

// Failure is a check that ran and found the file does not play the way it was checked: a
// decode that failed, printed an error or produced no picture, or a stream that cannot be
// listed or remuxed. It is a property of the file, and the same file fails the same way
// again.
type Failure struct {
	// Check names the check: "cold start", "segment", "remux" or "stream".
	Check string
	// At is where in the file it failed, in words ("the keyframe at 2069.069 s").
	At string
	// Reason is what went wrong: the first error line ffmpeg printed, or what the check found.
	Reason string
}

func (f *Failure) Error() string {
	if f.At == "" {
		return "playability " + f.Check + ": " + f.Reason
	}
	return "playability " + f.Check + " at " + f.At + ": " + f.Reason
}

// Unrun is a check that could not be run: a binary that did not start, a process killed by
// a signal, a cancelled context, or an answer this package could not read. It says nothing
// about the file.
type Unrun struct {
	What string
	Err  error
}

func (u *Unrun) Error() string {
	return "playability: " + u.What + " could not be run: " + u.Err.Error()
}

func (u *Unrun) Unwrap() error { return u.Err }

// IsFailure reports whether err is a verdict about the file (a *Failure) rather than a
// check that could not be run.
func IsFailure(err error) bool {
	var f *Failure
	return errors.As(err, &f)
}

// seconds renders a time in seconds the way every message here does.
func seconds(s float64) string { return fmt.Sprintf("%.3f s", s) }

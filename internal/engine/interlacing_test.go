package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// mkUnknownFieldOrder writes a NON-HEVC source whose container says NOTHING about its scan
// type: ffprobe answers `field_order=unknown` for it.
//
// It is an ordinary library file rather than a contrivance. A DivX-era MS-MPEG4 AVI is
// exactly the pre-2010 material this guard is about, and the codec is what decides the
// answer: an h264 or hevc elementary stream carries its own picture structure, while this
// one does not, so nothing downstream of the probe can tell progressive from interlaced.
func mkUnknownFieldOrder(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "msmpeg4", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// TestFieldOrderUnknown_IsClassifiedExplicitly grades [AC-9] of
// S0107-holdfast-interlacing-decision: a source whose field order ffprobe could not
// establish is classified under its OWN guard token and its own recorded outcome, rather
// than reaching the progressive encode path by omission.
//
// The defect it closes is a silent one. The interlace guard named the four interlaced
// spellings and let everything else through, so a file whose container never stated its
// scan type was re-encoded by a progressive-assuming pipeline - which bakes combing in
// permanently - and then had its source deleted by the swap. Nothing in the ledger said a
// guess had been made, because nothing knew one had been.
//
// The second arm is what stops the first from being vacuous: the identical configuration
// over a source that DOES declare itself progressive still transcodes, so the skip is the
// field order and not the fixture.
func TestFieldOrderUnknown_IsClassifiedExplicitly(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	prober := probe.New(ffmpeg, ffprobe)

	t.Run("an unestablished field order skips under its own token", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.avi")
		mkUnknownFieldOrder(t, ffmpeg, src, "3M")

		// The fixture has to BE the case under test, or the assertions below grade nothing.
		if fo := prober.FieldOrder(context.Background(), src); fo != "" {
			t.Fatalf("fixture's field order normalised to %q, want \"\" - this fixture is meant to "+
				"be the source ffprobe cannot establish a field order for", fo)
		}

		before := md5f(t, src)
		led := run(t, ffmpeg, ffprobe, d, nil, nil)

		if got := skipReason(t, led, "movie.avi"); got != SkipUnknownFieldOrder {
			t.Errorf("recorded reason = %q, want %q - an unestablished field order must be "+
				"classified explicitly, so an operator can see that the file was held back and "+
				"`requeue --guard %s` can offer it again", got, SkipUnknownFieldOrder, SkipUnknownFieldOrder)
		}
		if !ledgerHas(t, led, store.Skipped, "movie.avi") {
			t.Error("expected a skipped row for a source whose field order could not be established")
		}
		if md5f(t, src) != before {
			t.Error("the source was modified - a file nothing could classify must be left byte-for-byte alone")
		}
		if codecOf(t, ffprobe, src) != "msmpeg4v3" {
			t.Error("the source was transcoded - the unknown field order reached the encode path by omission")
		}
		if nTemp(t, d) != 0 {
			t.Error("temp left behind")
		}
	})

	t.Run("a declared-progressive source still transcodes", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")

		if fo := prober.FieldOrder(context.Background(), src); fo != "progressive" {
			t.Fatalf("control fixture's field order = %q, want \"progressive\"", fo)
		}
		run(t, ffmpeg, ffprobe, d, nil, nil)
		if got := codecOf(t, ffprobe, src); got != "hevc" {
			t.Errorf("control source codec = %q, want hevc - the new guard must refuse only the "+
				"sources nobody can classify, not every source", got)
		}
	})
}

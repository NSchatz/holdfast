package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/store"
)

// Regression evidence for S0088 refute-impl finding F1, against [AC-7]:
//
//	"THE SYSTEM SHALL reject it and keep the source ... IF it carries any stream the map
//	does not intend"
//
// The intended-map check compares a MULTISET OF (type, language) - streamTally in
// streams.go - so it cannot tell two streams of the same type and the same language apart.
// Every other fact the plan itself decided on (the container's `comment` disposition, the
// `attached_pic` disposition) is dropped from the comparison, even though probe.Stream
// carries both on BOTH sides of the gate.
//
// The consequence is the loss AC-7 exists to prevent. With `keep_commentary: false` on a
// source whose commentary track shares its language with the main track - an English
// commentary on an English film, the ordinary case - an output that carried the COMMENTARY
// track and dropped the MAIN one tallies identically to the intended map. The gate accepts
// it, the source is deleted, and the main English audio is gone for good.
//
// This test FAILS on sdd/S0088-holdfast-audio-subtitle-stream-selection at 6e56754.
func TestRegress0088F1_AcceptsAnOutputCarryingADroppedCommentaryTrack(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	// a:0 the main English track, a:1 the container-marked English commentary, a:2 French.
	mkSourceWithStreams(t, ffmpeg, src,
		audioStream("eng"), commentaryAudio("eng"), audioStream("fre"))

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	prof := selectionProfile(t, "    keep_commentary: false\n")
	plan := planFor(t, eng, src, prof)

	// The premise: the map intends the main English track and the French one, and drops
	// exactly the container-marked commentary.
	dropped := plan.Dropped()
	if len(dropped) != 1 || !dropped[0].Commentary || dropped[0].Language != "eng" {
		t.Fatalf("the plan dropped %+v, want exactly the container-marked eng commentary track",
			dropped)
	}

	// The output the gate must reject: the COMMENTARY track survived and the MAIN English
	// track did not. It is a stream the map does not intend, present in the output, beside
	// a stream the map does intend, absent from it.
	out := filepath.Join(dir, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-map", "0:v", "-map", "0:a:1", "-map", "0:a:2",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "-c:a", "copy", "--", out)

	outStreams := streamsOf(t, eng, out)
	commentaryCarried := false
	for _, s := range outStreams {
		if s.Commentary {
			commentaryCarried = true
		}
	}
	if !commentaryCarried {
		t.Fatalf("the fixture drifted: the output carries no container-marked commentary "+
			"stream (%+v), so it is not the output this case is about", outStreams)
	}
	if n := countByType(outStreams); n["audio"] != 2 {
		t.Fatalf("the fixture drifted: the output carries %d audio stream(s), want 2", n["audio"])
	}

	// THE FINDING: the intended-map check accepts it.
	if err := plan.CheckOutput(outStreams); err == nil {
		t.Errorf("the intended-map check ACCEPTED an output that carries the commentary track "+
			"the map dropped and is missing the main English track the map intends: a "+
			"multiset of (type, language) cannot tell them apart, and accepting this deletes "+
			"a source whose main English audio the replacement does not have. intended=%+v "+
			"output=%+v", plan.Intended(), outStreams)
	}

	// And so does the whole verification gate, which is what actually licenses the swap.
	_, _, class, err := eng.verifyOutput(context.Background(), src, out, prof,
		targetCodecFor(eng.Cfg.TranscodeIn(prof, src).Encoder), plan, deinterlace.Filter{})
	if err == nil {
		t.Errorf("verifyOutput ACCEPTED it (class=%q): every gate in front of the deletion of "+
			"the source is green on an output that lost the main English audio track", class)
	} else if class != store.FailureDeterministic {
		t.Logf("verifyOutput rejected for another reason (class=%q): %v", class, err)
	}
}

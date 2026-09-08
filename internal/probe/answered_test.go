package probe

// The three-way distinction VideoCodec cannot make, and Usable, which confirms that a
// refusal came from a working ffprobe.
//
// Why this is a safety primitive and not a convenience: the engine's record-free
// hold-back (engine.strayReplacementHold, FILESYSTEM-1/AC15i) decides whether a file
// holdfast wrote may be DELETED, and it decides it from what ffprobe says. VideoCodec
// returns "" for "I read this and it has no video stream" and for "I never ran" alike,
// so a hold built on it fails OPEN the moment the tool is missing or the run is
// cancelled - it deletes the protected file on a negative answer it never obtained.
// These are the assertions that keep the two apart.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// brokenFFprobe writes an executable that RUNS and exits non-zero for every question,
// which is the shape of a half-installed build or one whose shared library is missing.
// It is the case that looks EXACTLY like ffprobe reading a file and refusing it.
func brokenFFprobe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "broken-ffprobe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVideoCodecAnswered_SeparatesARefusalFromAQuestionThatWasNeverAsked(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	ctx := context.Background()

	// 1. ffprobe ran and answered about the file.
	if codec, answered := fakeProber().VideoCodecAnswered(ctx, "sdr.mkv"); codec != "h264" || !answered {
		t.Errorf("a readable file = (%q, %v), want (\"h264\", true)", codec, answered)
	}

	// 2. ffprobe ran, read the path and REFUSED it - which is a real answer about the
	// file, and the one that lets a caller reclaim a killed run's garbage.
	if codec, answered := fakeProber().VideoCodecAnswered(ctx, "not-media.mkv"); codec != "" || !answered {
		t.Errorf("a file ffprobe refuses = (%q, %v), want (\"\", true) - ffprobe DID answer", codec, answered)
	}

	// 3. The binary is not there: nothing was asked, so nothing was answered.
	missing := &Prober{FFprobe: filepath.Join(t.TempDir(), "no-such-ffprobe")}
	if codec, answered := missing.VideoCodecAnswered(ctx, "sdr.mkv"); codec != "" || answered {
		t.Errorf("a missing ffprobe = (%q, %v), want (\"\", false)", codec, answered)
	}

	// 4. The context is cancelled: CommandContext kills the subprocess mid-question, and
	// the corpse is not evidence about the file.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if codec, answered := fakeProber().VideoCodecAnswered(cancelled, "sdr.mkv"); codec != "" || answered {
		t.Errorf("a cancelled context = (%q, %v), want (\"\", false)", codec, answered)
	}

	// 5. And the conflation this exists to fix: VideoCodec cannot tell 2 from 3.
	both := []string{
		fakeProber().VideoCodec(ctx, "not-media.mkv"),
		missing.VideoCodec(ctx, "sdr.mkv"),
	}
	if both[0] != "" || both[1] != "" {
		t.Fatalf("VideoCodec = %q - this test's premise (both read as \"\") no longer holds", both)
	}
}

// TestUsable_TellsAWorkingFfprobeFromOneThatAnswersNothing pins the confirmation the
// hold-back asks for before it acts on a refusal. A broken ffprobe's "exit 1" and a
// working ffprobe's "this file is not media" are the same bytes on the same exit code;
// only a question with a known answer separates them.
func TestUsable_TellsAWorkingFfprobeFromOneThatAnswersNothing(t *testing.T) {
	os.Setenv(fakeFFprobeEnv, "1")
	defer os.Unsetenv(fakeFFprobeEnv)
	ctx := context.Background()

	if !fakeProber().Usable(ctx) {
		t.Error("a working ffprobe reports itself unusable")
	}
	if (&Prober{FFprobe: brokenFFprobe(t)}).Usable(ctx) {
		t.Error("an ffprobe that exits non-zero for EVERY question reports itself usable")
	}
	if (&Prober{FFprobe: filepath.Join(t.TempDir(), "no-such-ffprobe")}).Usable(ctx) {
		t.Error("a missing ffprobe reports itself usable")
	}

	// The broken one still ANSWERS a file question, in the sense that matters: it
	// exited on its own. That is precisely why Usable has to be asked as well.
	if codec, answered := (&Prober{FFprobe: brokenFFprobe(t)}).VideoCodecAnswered(ctx, "sdr.mkv"); codec != "" || !answered {
		t.Errorf("a broken ffprobe = (%q, %v), want (\"\", true) - it exited by itself, so the "+
			"file question alone cannot tell it from a refusal", codec, answered)
	}
}

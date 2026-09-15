package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CensusProps - the batched probe a library census reads, and the one property that
// makes batching safe: it must answer exactly what the single-field probes answer.
//
// A census asks every question about every file in the library, so three subprocesses
// per file where one would do is three times the cost of counting a library. The risk
// that buys is drift - a batched parse that disagrees with the field the engine's own
// guards read - which is why the comparison below is field for field against the very
// methods those guards use, on a REAL file through a REAL ffprobe.

func censusFixture(t *testing.T, name, pixFmt, size string) string {
	t.Helper()
	ffmpeg := os.Getenv("HOLDFAST_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required to build the census fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size="+size+":rate=5", "-frames:v", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", pixFmt, "--", path).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	return path
}

func realProber() *Prober {
	ffmpeg, ffprobe := os.Getenv("HOLDFAST_FFMPEG"), os.Getenv("HOLDFAST_FFPROBE")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	return New(ffmpeg, ffprobe)
}

func TestCensusProps_AnswersExactlyWhatTheSingleFieldProbesAnswer(t *testing.T) {
	ctx := context.Background()
	p := realProber()
	for _, tc := range []struct{ pixFmt, size string }{
		{"yuv420p", "320x240"},
		{"yuv420p10le", "1280x720"},
	} {
		f := censusFixture(t, "fixture.mkv", tc.pixFmt, tc.size)
		got := p.CensusProps(ctx, f)
		if !got.Answered {
			t.Fatalf("%s: ffprobe did not answer for a file it wrote itself", tc.pixFmt)
		}
		wantCodec, answered := p.VideoCodecAnswered(ctx, f)
		if !answered || got.Codec != wantCodec {
			t.Fatalf("codec = %q, want the single-field probe's %q", got.Codec, wantCodec)
		}
		if want := p.Height(ctx, f); got.Height != want {
			t.Fatalf("height = %d, want the single-field probe's %d", got.Height, want)
		}
		if want := p.PixFmt(ctx, f); got.PixFmt != want {
			t.Fatalf("pix_fmt = %q, want the single-field probe's %q", got.PixFmt, want)
		}
		if got.Height == 0 || got.Codec == "" || got.PixFmt == "" {
			t.Fatalf("a real file answered nothing: %+v", got)
		}
	}
}

// TestCensusProps_SeparatesARefusalFromAQuestionNeverAsked is the distinction the
// census's unknown bucket rests on: ffprobe reading a file and finding nothing in it is
// an answer ABOUT THAT FILE, while a binary that could not run says nothing about any
// file and must not be allowed to fill a report with unknowns that look like findings.
func TestCensusProps_SeparatesARefusalFromAQuestionNeverAsked(t *testing.T) {
	ctx := context.Background()
	notMedia := filepath.Join(t.TempDir(), "notreally.mkv")
	if err := os.WriteFile(notMedia, []byte("this is not a matroska file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := realProber().CensusProps(ctx, notMedia)
	if !got.Answered {
		t.Fatal("ffprobe read the path and refused it, which IS an answer about the file")
	}
	if got.Codec != "" || got.Height != 0 || got.PixFmt != "" {
		t.Fatalf("a file with no video stream carries properties: %+v", got)
	}

	never := New("no-such-ffmpeg-anywhere", "no-such-ffprobe-anywhere")
	if got := never.CensusProps(ctx, notMedia); got.Answered {
		t.Fatalf("a binary that cannot be run reported an answer: %+v", got)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := realProber().CensusProps(cancelled, notMedia); got.Answered {
		t.Fatalf("a cancelled question reported an answer: %+v", got)
	}

	// A binary that RUNS and exits non-zero for everything looks exactly like a refusal
	// about the file, which is why the census asks Usable before it trusts a negative.
	broken := &Prober{FFmpeg: brokenFFprobe(t), FFprobe: brokenFFprobe(t)}
	if got := broken.CensusProps(ctx, notMedia); !got.Answered {
		t.Fatalf("a broken binary is indistinguishable from a refusal here, by design: %+v", got)
	}
	if broken.Usable(ctx) {
		t.Fatal("Usable is what tells the two apart, and it did not")
	}
}

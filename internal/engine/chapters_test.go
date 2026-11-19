package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// chapterMeta writes an ffmetadata file carrying one chapter per {start, end, title} triple,
// in milliseconds.
func chapterMeta(t *testing.T, path string, chapters ...[3]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(";FFMETADATA1\n")
	for _, c := range chapters {
		b.WriteString("[CHAPTER]\nTIMEBASE=1/1000\nSTART=" + c[0] + "\nEND=" + c[1] + "\ntitle=" + c[2] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// mkChapteredH264 writes the 2 s h264 source every chapter case starts from, carrying the
// chapters in meta ("" for none).
func mkChapteredH264(t *testing.T, ffmpeg, path, meta string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10"}
	if meta != "" {
		args = append(args, "-i", meta, "-map", "0:v", "-map_chapters", "1")
	}
	ff(t, ffmpeg, append(args, "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M",
		"-pix_fmt", "yuv420p", "--", path)...)
}

// mkHevcOf re-encodes src to an HEVC output that passes every gate before the chapter gate,
// taking its chapters from meta: "" keeps the source's (ffmpeg's default), "-1" drops them,
// and a path replaces them with that file's.
func mkHevcOf(t *testing.T, ffmpeg, src, out, meta string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}
	switch meta {
	case "":
		args = append(args, "-map", "0:v:0")
	case "-1":
		args = append(args, "-map", "0:v:0", "-map_chapters", "-1")
	default:
		args = append(args, "-i", meta, "-map", "0:v:0", "-map_chapters", "1")
	}
	ff(t, ffmpeg, append(args, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", "--", out)...)
}

// TestVerify_ChapterParity drives the gate over real files ffmpeg wrote: a source with two
// titled chapters, and outputs that are right in every way the gates before this one check
// and differ only in their chapters. Every damaged table is refused by the chapter gate,
// deterministically, with the source left as it was; the faithful one, a pair with no
// chapters, and a source whose own table runs past its end reproduced exactly all pass.
func TestVerify_ChapterParity(t *testing.T) {
	ffmpeg, _ := tools(t)
	dir := t.TempDir()
	p := func(name string) string { return filepath.Join(dir, name) }

	good := chapterMeta(t, p("good.txt"), [3]string{"0", "1000", "Opening"}, [3]string{"1000", "2000", "Close"})
	src := p("src.mkv")
	mkChapteredH264(t, ffmpeg, src, good)

	// The table issue #200 found on a real source, scaled to this 2 s file: a first chapter
	// spanning the film and three starting after its end. The source's own defect, which a
	// follow-up on the issue confirmed the real source carries:
	// https://github.com/NSchatz/holdfast/issues/200#issuecomment-6730994015
	broken := chapterMeta(t, p("broken.txt"), [3]string{"0", "2000", "Film"}, [3]string{"10000", "44000", "Two"},
		[3]string{"44000", "78000", "Three"}, [3]string{"78000", "90000", "Four"})
	srcBroken := p("src-broken.mkv")
	mkChapteredH264(t, ffmpeg, srcBroken, broken)

	srcNone := p("src-none.mkv")
	mkChapteredH264(t, ffmpeg, srcNone, "")

	outs := map[string]string{
		"faithful":     "",
		"dropped":      "-1",
		"shifted":      chapterMeta(t, p("shifted.txt"), [3]string{"0", "1000", "Opening"}, [3]string{"1500", "2000", "Close"}),
		"one-lost":     chapterMeta(t, p("one.txt"), [3]string{"0", "2000", "Opening"}),
		"retitled":     chapterMeta(t, p("retitled.txt"), [3]string{"0", "1000", "Opening"}, [3]string{"1000", "2000", "Credits"}),
		"out-of-range": chapterMeta(t, p("range.txt"), [3]string{"0", "1000", "Opening"}, [3]string{"10000", "44000", "Close"}),
	}
	for name, meta := range outs {
		mkHevcOf(t, ffmpeg, src, p("out-"+name+".mkv"), meta)
	}
	mkHevcOf(t, ffmpeg, srcBroken, p("out-broken.mkv"), "")
	mkHevcOf(t, ffmpeg, srcBroken, p("out-broken-dropped.mkv"), "-1")
	mkHevcOf(t, ffmpeg, srcNone, p("out-none.mkv"), "")
	mkHevcOf(t, ffmpeg, srcNone, p("out-added.mkv"), good)

	eng := buildEngine(t, ffmpeg, envOr("HOLDFAST_FFPROBE", "ffprobe"), dir, nil, nil)
	verify := func(in, out string) (string, store.FailureClass, error) {
		top := eng.Cfg.TopLevelProfile()
		target := targetCodecFor(eng.Cfg.TranscodeIn(top, in).Encoder)
		_, gate, class, err := eng.verifyOutput(context.Background(), in, out, top, target,
			planFor(t, eng, in, top), deinterlace.Filter{}, downscale.Scale{})
		return gate, class, err
	}

	for _, tc := range []struct{ name, in, out, want string }{
		{"every chapter dropped", src, p("out-dropped.mkv"), "the output carries 0 chapter(s) and the source carries 2"},
		{"a chapter lost", src, p("out-one-lost.mkv"), "the output carries 1 chapter(s) and the source carries 2"},
		{"a chapter shifted", src, p("out-shifted.mkv"), `chapter 2 ("Close") starts at 1.500s in the output and 1.000s in the source`},
		{"a chapter retitled", src, p("out-retitled.mkv"), `chapter 2 is titled "Credits" in the output and "Close" in the source`},
		{"a chapter moved past the end", src, p("out-out-of-range.mkv"), `chapter 2 ("Close") starts at 10.000s in the output`},
		{"a malformed source's chapters dropped", srcBroken, p("out-broken-dropped.mkv"), "the output carries 0 chapter(s) and the source carries 4"},
		{"chapters added to a source with none", srcNone, p("out-added.mkv"), "the output carries 2 chapter(s) and the source carries 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := md5f(t, tc.in)
			gate, class, err := verify(tc.in, tc.out)
			if err == nil {
				t.Fatal("the gates ACCEPTED an output whose chapter table is not the source's")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "The source is kept") {
				t.Fatalf("a different rejection: got %q, want one containing %q", err, tc.want)
			}
			if gate != GateChapters || class != store.FailureDeterministic {
				t.Errorf("gate=%q class=%q, want %q %q", gate, class, GateChapters, store.FailureDeterministic)
			}
			if md5f(t, tc.in) != before {
				t.Error("the source changed under a rejected verification")
			}
		})
	}

	for _, tc := range []struct{ name, in, out string }{
		{"a faithful copy of the chapters", src, p("out-faithful.mkv")},
		{"no chapters on either side", srcNone, p("out-none.mkv")},
		{"a source's own chapters past its end, reproduced exactly", srcBroken, p("out-broken.mkv")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := verify(tc.in, tc.out); err != nil {
				t.Fatalf("the gates refused an output that damaged no chapter: %v", err)
			}
		})
	}
}

// TestChapterParity_RealEncodeCarriesTheChaptersAndSwaps runs the production encoder over a
// chaptered source whose own table runs past its end: the encode copies the table, the
// chapter gate passes it, the swap happens, and the replacement carries exactly the
// source's chapters. A gate that refused a source's own defect would leave this file
// unencodable; an encode that lost the chapters would be refused.
func TestChapterParity_RealEncodeCarriesTheChaptersAndSwaps(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	meta := chapterMeta(t, filepath.Join(t.TempDir(), "broken.txt"), [3]string{"0", "2000", "Film"},
		[3]string{"10000", "44000", "Two"})
	src := filepath.Join(dir, "movie.mkv")
	mkChapteredH264(t, ffmpeg, src, meta)
	prober := probe.New(ffmpeg, ffprobe)
	want, ok := prober.Chapters(context.Background(), src)
	if !ok || len(want.Chapters) != 2 {
		t.Fatalf("fixture drifted: the source's chapters are %+v (%v)", want, ok)
	}

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := rowForFile(t, eng.Store.(*testStore), "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatal("the source was not replaced")
	}
	got, ok := prober.Chapters(context.Background(), src)
	if !ok || len(got.Chapters) != len(want.Chapters) {
		t.Fatalf("the replacement's chapters are %+v (%v), want %+v", got.Chapters, ok, want.Chapters)
	}
	for i := range want.Chapters {
		if got.Chapters[i] != want.Chapters[i] {
			t.Errorf("chapter %d = %+v, want %+v", i+1, got.Chapters[i], want.Chapters[i])
		}
	}
}

package engine

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/chapters"
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- a dangling QuickTime chapter reference is not container damage ------------------

// qtChapterLine is the one line the mov demuxer logs for an MP4 whose `tref` `chap` names a
// track the file does not carry.
const qtChapterLine = "Referenced QT chapter track not found"

// movDemuxerLine is one line the mov demuxer logged, its message captured.
var movDemuxerLine = regexp.MustCompile(`^\[mov,mp4,m4a,3gp,3g2,mj2 @ 0x[0-9a-f]+\] (.*)$`)

// mkDanglingChapterMP4 writes a 2 s h264 and AAC MP4 muxed with two titled chapters, which
// ffmpeg writes twice: as a QuickTime chapter track every other track references (`tref`
// `chap`), and as Nero `chpl` chapters. It then frees the chapter track (its box renamed
// `free`, which every reader skips and which moves no byte), leaving the reference dangling:
// the shape a real library's MP4s were found in. nero also frees the Nero chapters, so the
// file has no chapter table at all; stts also frees the audio track's `stts`, a real demuxer
// error beside the benign one.
//
// It returns the mov demuxer's messages, read off the pinned ffprobe directly, and fails the
// test if ffprobe wrote any line the mov demuxer did not.
func mkDanglingChapterMP4(t *testing.T, ffmpeg, ffprobe, path string, nero, stts bool) []string {
	t.Helper()
	meta := chapterMeta(t, filepath.Join(t.TempDir(), "chapters.txt"),
		[3]string{"0", "1000", "Opening"}, [3]string{"1000", "2000", "Close"})
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10", "-f", "lavfi", "-i", "sine=duration=2",
		"-i", meta, "-map", "0:v", "-map", "1:a", "-map_chapters", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "-c:a", "aac", "--", path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	freeChapterTrack(t, b, nero, stts)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	codec, stderr := ffprobeCodec(t, ffprobe, path)
	if codec != "h264" {
		t.Fatalf("precondition: the pinned ffprobe reads codec %q for %s, want h264 (stderr %q)", codec, path, stderr)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		m := movDemuxerLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("precondition: ffprobe wrote a line the mov demuxer did not: %q", line)
		}
		lines = append(lines, m[1])
	}
	return lines
}

// freeChapterTrack frees, in the MP4 ffmpeg wrote, the track each `tref` `chap` names, and
// the Nero chapters (nero) and the audio track's `stts` (stts) if asked.
func freeChapterTrack(t *testing.T, b []byte, nero, stts bool) {
	t.Helper()
	var moov mp4Box
	for _, x := range mp4Boxes(t, b, 0, len(b)) {
		if x.typ == "moov" {
			moov = x
		}
	}
	var traks []mp4Box
	for _, x := range mp4Boxes(t, b, moov.off+8, moov.end) {
		if x.typ == "trak" {
			traks = append(traks, x)
		}
	}
	refs := map[uint32]bool{}
	for _, trak := range traks {
		if chap, ok := mp4Path(t, b, trak, "tref", "chap"); ok {
			for i := chap.off + 8; i+4 <= chap.end; i += 4 {
				refs[binary.BigEndian.Uint32(b[i:])] = true
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("fixture: ffmpeg wrote no chapter reference, so there is none to leave dangling")
	}
	freed := 0
	for _, trak := range traks {
		tkhd, okT := mp4Path(t, b, trak, "tkhd")
		hdlr, okH := mp4Path(t, b, trak, "mdia", "hdlr")
		if !okT || !okH {
			t.Fatal("fixture: a track without a tkhd or a hdlr")
		}
		id := tkhd.off + 20 // size, type, version and flags, creation and modification times
		if b[tkhd.off+8] == 1 {
			id = tkhd.off + 28 // version 1 times are 64-bit
		}
		if refs[binary.BigEndian.Uint32(b[id:])] {
			mp4Free(b, trak)
			freed++
		}
		if stts && string(b[hdlr.off+16:hdlr.off+20]) == "soun" {
			x, ok := mp4Path(t, b, trak, "mdia", "minf", "stbl", "stts")
			if !ok {
				t.Fatal("fixture: the audio track has no stts")
			}
			mp4Free(b, x)
		}
	}
	if freed != len(refs) {
		t.Fatalf("fixture: freed %d track(s) of the %d the chapter references name", freed, len(refs))
	}
	if nero {
		x, ok := mp4Path(t, b, moov, "udta", "chpl")
		if !ok {
			t.Fatal("fixture: ffmpeg wrote no Nero chapters")
		}
		mp4Free(b, x)
	}
}

// mp4Box is one box of an ISO BMFF file: the offset of its header, the end of its payload.
type mp4Box struct {
	typ      string
	off, end int
}

// mp4Boxes lists the boxes laid end to end in b[off:end]. A size this reader does not walk (0,
// "to the end of the file", or 1, a 64-bit size) fails the test rather than being guessed at.
func mp4Boxes(t *testing.T, b []byte, off, end int) []mp4Box {
	t.Helper()
	var out []mp4Box
	for off < end {
		if end-off < 8 {
			t.Fatalf("fixture: %d stray byte(s) at %d", end-off, off)
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		if size < 8 || off+size > end {
			t.Fatalf("fixture: the box at %d has size %d, which this reader does not walk", off, size)
		}
		out = append(out, mp4Box{typ: string(b[off+4 : off+8]), off: off, end: off + size})
		off += size
	}
	return out
}

// mp4Path follows typs down from parent, each the first child of its type.
func mp4Path(t *testing.T, b []byte, parent mp4Box, typs ...string) (mp4Box, bool) {
	t.Helper()
	x := parent
	for _, typ := range typs {
		found := false
		for _, c := range mp4Boxes(t, b, x.off+8, x.end) {
			if c.typ == typ {
				x, found = c, true
				break
			}
		}
		if !found {
			return mp4Box{}, false
		}
	}
	return x, true
}

// mp4Free renames x to `free`.
func mp4Free(b []byte, x mp4Box) { copy(b[x.off+4:x.off+8], "free") }

// TestSourceDamaged_ADanglingQTChapterReferenceIsEncoded runs the real pipeline over an MP4
// whose only demuxer line is the dangling chapter reference. It was recorded source-damaged and
// flagged for a human; it is not damaged, so it is encoded and faces every gate like any other
// source, and it passes them: the full decode, the intended-stream check, chapter parity
// against the chapter table ffmpeg reads (the Nero chapters where the file has them, none where
// it does not) and VMAF, into Matroska and back into MP4. The replacement carries exactly that table and
// no dangling reference of its own.
func TestSourceDamaged_ADanglingQTChapterReferenceIsEncoded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name      string
		nero      bool // the Nero chapters freed too
		container string
		chapters  int
	}{
		{name: "the Nero chapters kept, into Matroska", container: "mkv", chapters: 2},
		{name: "the Nero chapters kept, into MP4", container: "source", chapters: 2},
		{name: "no chapter table at all, into Matroska", nero: true, container: "mkv"},
		{name: "no chapter table at all, into MP4", nero: true, container: "source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "movie.mp4")
			if got := mkDanglingChapterMP4(t, ffmpeg, ffprobe, src, tc.nero, false); len(got) != 1 || got[0] != qtChapterLine {
				t.Fatalf("precondition: the mov demuxer logged %q, want only %q", got, qtChapterLine)
			}
			prober := probe.New(ffmpeg, ffprobe)
			want, ok := prober.Chapters(context.Background(), src)
			if !ok || len(want.Chapters) != tc.chapters {
				t.Fatalf("precondition: the source's chapters read as %+v (%v), want %d", want.Chapters, ok, tc.chapters)
			}

			eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
				c.ContainerExt = tc.container
				everyGate(c)
			})
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatal(err)
			}
			row := rowForFile(t, eng.Store.(*testStore), "movie.")
			if row.Status != store.Done {
				t.Fatalf("row %s %q, want done: the dangling chapter reference kept a sound source from its encode",
					row.Status, row.Outcome.Reason)
			}
			if row.Outcome.VmafMean == nil {
				t.Fatalf("the done row carries no VMAF measurement (%+v): the perceptual gate must have run", row.Outcome)
			}
			final := filepath.Join(dir, "movie.mp4")
			if tc.container == "mkv" {
				final = filepath.Join(dir, "movie.mkv")
			}
			if codec, stderr := ffprobeCodec(t, ffprobe, final); codec != "hevc" || stderr != "" {
				t.Fatalf("the replacement %s reads as codec %q with stderr %q, want hevc and nothing", final, codec, stderr)
			}
			got, ok := prober.Chapters(context.Background(), final)
			if !ok {
				t.Fatal("the replacement's chapter table could not be read")
			}
			if _, err := chapters.Check(want, got, eng.Cfg.DurationToleranceSec); err != nil || len(got.Chapters) != tc.chapters {
				t.Errorf("the replacement's chapters are %+v, want the source's %+v: %v", got.Chapters, want.Chapters, err)
			}
		})
	}
}

// TestSourceDamaged_ADanglingQTChapterReferenceBesideDamageIsStillDamaged: the benign line
// excuses only itself. The same MP4 with its audio track's `stts` gone makes the mov demuxer
// log a broken header beside the dangling reference, and that file is recorded source-damaged,
// flagged for a human with the broken header as its diagnostic, with no ffmpeg run and the
// source untouched.
func TestSourceDamaged_ADanglingQTChapterReferenceBesideDamageIsStillDamaged(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mp4")
	const broken = "stream 1, missing mandatory atoms, broken header"
	if got := mkDanglingChapterMP4(t, ffmpeg, ffprobe, src, true, true); len(got) != 2 ||
		got[0] != broken || got[1] != qtChapterLine {
		t.Fatalf("precondition: the mov demuxer logged %q, want %q then %q", got, broken, qtChapterLine)
	}
	before := md5f(t, src)

	eng, ts, ffmpegCalls, records := damageEngine(t, ffmpeg, ffprobe, dir, nil)
	oneshot(t, eng)

	if got := skipReason(t, ts, "movie.mp4"); got != SkipSourceDamaged {
		t.Fatalf("skip reason = %q, want %q", got, SkipSourceDamaged)
	}
	if n := ffmpegCalls(); n != 0 {
		t.Errorf("ffmpeg ran %d time(s) for a source the probe found damaged", n)
	}
	if md5f(t, src) != before {
		t.Error("the damaged source changed")
	}
	var flagged []map[string]any
	for _, rec := range records() {
		if rec["file"] == src && rec["level"] == "ERROR" && rec["guard"] == SkipSourceDamaged {
			flagged = append(flagged, rec)
		}
	}
	if len(flagged) != 1 || flagged[0]["diagnostic"] != broken {
		t.Errorf("want one error record carrying diagnostic %q, got %v", broken, flagged)
	}
}

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Cover art headed for a Matroska output. A Matroska source keeps its pictures as image
// attachments, which ffmpeg surfaces as one-frame video streams with the attached_pic
// disposition; the cases below build that shape with the pinned ffmpeg, assert it through
// ffprobe before use, and grade what the job's output carries as BYTES.

// The audio and subtitle tracks the report-shaped fixture carries between its moving
// picture and its pictures. Three and five put the two pictures at stream indexes 9 and 10,
// where the reported file carries them.
var (
	reportAudio     = []string{"eng", "fre", "ger"}
	reportSubtitles = []string{"eng", "fre", "ger", "spa", "ita"}
	reportPictures  = []string{"cover.jpg", "cover_land.jpg"}
)

// reportShape selects a variant of the report-shaped Matroska fixture.
type reportShape struct {
	pictures bool // the two mjpeg pictures after the subtitle streams
	font     bool // a non-image attachment ahead of the pictures
}

// mkReportMatroska writes a Matroska source shaped like the reported file: one h264 moving
// picture, the audio and subtitle tracks above, then (per shape) a font and two mjpeg
// pictures of different content, and asserts that shape through ffprobe before returning.
//
// The file carries index padding a muxer leaves behind, which a remux does not reproduce: a
// remux-only job is held to the size gate like any other, and without it a pure remux of this
// file comes out no smaller than its source.
func mkReportMatroska(t *testing.T, ffmpeg, ffprobe, path string, shape reportShape) {
	t.Helper()
	inputs := t.TempDir() // never under the library root, so nothing here is enumerated
	srt := filepath.Join(inputs, "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n\n"), 0o644); err != nil {
		t.Fatalf("write the subtitle input: %v", err)
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "srt", "-i", srt,
		"-map", "0:v"}
	for range reportAudio {
		args = append(args, "-map", "1:a")
	}
	for range reportSubtitles {
		args = append(args, "-map", "2:s")
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "copy", "-reserve_index_space", "65536")
	for i, l := range reportAudio {
		args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), "language="+l)
	}
	for i, l := range reportSubtitles {
		args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+l)
	}
	att := 0
	if shape.font {
		font := filepath.Join(inputs, "font.ttf")
		if err := os.WriteFile(font, []byte("a font the muxer never looks inside"), 0o644); err != nil {
			t.Fatalf("write the font input: %v", err)
		}
		args = append(args, "-attach", font,
			fmt.Sprintf("-metadata:s:t:%d", att), "mimetype=application/x-truetype-font",
			fmt.Sprintf("-metadata:s:t:%d", att), "filename=font.ttf")
		att++
	}
	if shape.pictures {
		for i, lavfi := range []string{"testsrc2=size=160x120", "smptebars=size=128x96"} {
			pic := filepath.Join(inputs, reportPictures[i])
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", lavfi,
				"-frames:v", "1", "-c:v", "mjpeg", "-pix_fmt", "yuvj420p", "-f", "image2", "--", pic)
			args = append(args, "-attach", pic,
				fmt.Sprintf("-metadata:s:t:%d", att), "mimetype=image/jpeg",
				fmt.Sprintf("-metadata:s:t:%d", att), "filename="+reportPictures[i])
			att++
		}
	}
	args = append(args, "--", path)
	ff(t, ffmpeg, args...)
	assertReportShape(t, ffprobe, path, shape)
}

// assertReportShape fails unless path carries exactly the streams mkReportMatroska means it
// to, in order, read through ffprobe directly rather than through internal/probe.
func assertReportShape(t *testing.T, ffprobe, path string, shape reportShape) {
	t.Helper()
	want := []string{"video/h264"}
	for _, l := range reportAudio {
		want = append(want, "audio/"+l)
	}
	for _, l := range reportSubtitles {
		want = append(want, "subtitle/"+l)
	}
	if shape.font {
		want = append(want, "attachment/font.ttf")
	}
	if shape.pictures {
		want = append(want, "picture/mjpeg", "picture/mjpeg")
	}
	var got []string
	for _, s := range probedStreams(t, ffprobe, path) {
		got = append(got, s.kind())
	}
	if !equalStrings(got, want) {
		t.Fatalf("fixture %s carries %v, want %v - the fixture is not the shape the test is about",
			path, got, want)
	}
	if shape.pictures && !shape.font {
		rows := probedStreams(t, ffprobe, path)
		if rows[9].Index != 9 || rows[10].Index != 10 || !rows[9].AttachedPic || !rows[10].AttachedPic {
			t.Fatalf("fixture %s: the pictures are not at stream indexes 9 and 10 as in the report: %+v",
				path, rows[9:])
		}
	}
}

// probedStream is one stream as ffprobe reports it.
type probedStream struct {
	Index       int
	Type        string
	Codec       string
	AttachedPic bool
	Language    string
	Filename    string
	Mimetype    string
}

// kind names a stream for a shape comparison: a picture by its codec, an attachment by its
// filename, a moving picture by its codec and anything else by its language.
func (s probedStream) kind() string {
	switch {
	case s.Type == "video" && s.AttachedPic:
		return "picture/" + s.Codec
	case s.Type == "video":
		return "video/" + s.Codec
	case s.Type == "attachment":
		return "attachment/" + s.Filename
	default:
		lang := s.Language
		if lang == "und" {
			lang = ""
		}
		return s.Type + "/" + lang
	}
}

func probedStreams(t *testing.T, ffprobe, path string) []probedStream {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries",
		"stream=index,codec_type,codec_name:stream_disposition=attached_pic:stream_tags=language,filename,mimetype",
		"-of", "json", "--", path).Output()
	if err != nil {
		t.Fatalf("ffprobe could not read %s: %v", path, err)
	}
	var parsed struct {
		Streams []struct {
			Index       int               `json:"index"`
			CodecType   string            `json:"codec_type"`
			CodecName   string            `json:"codec_name"`
			Disposition map[string]int    `json:"disposition"`
			Tags        map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parse the ffprobe answer for %s: %v", path, err)
	}
	var rows []probedStream
	for _, s := range parsed.Streams {
		rows = append(rows, probedStream{
			Index: s.Index, Type: s.CodecType, Codec: s.CodecName,
			AttachedPic: s.Disposition["attached_pic"] == 1,
			Language:    s.Tags["language"], Filename: s.Tags["filename"], Mimetype: s.Tags["mimetype"],
		})
	}
	return rows
}

// picturesOf extracts every picture path carries, in container order, with `-c copy`.
func picturesOf(t *testing.T, ffmpeg, ffprobe, path, side, label string) [][]byte {
	t.Helper()
	var pics [][]byte
	v := -1
	for _, s := range probedStreams(t, ffprobe, path) {
		if s.Type != "video" {
			continue
		}
		v++
		if s.AttachedPic {
			dst := filepath.Join(side, fmt.Sprintf("%s-v%d.img", label, v))
			pics = append(pics, extractVideoStream(t, ffmpeg, path, v, dst))
		}
	}
	return pics
}

// assertPicturesCarried fails unless got holds exactly the pictures of want, in order, each
// byte for byte.
func assertPicturesCarried(t *testing.T, want, got [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("the output carries %d picture(s), the source carried %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("picture %d changed across the job: %d bytes in, %d bytes out (or another "+
				"picture in its place) - a picture must be carried, in source order, byte for byte",
				i, len(want[i]), len(got[i]))
		}
	}
}

// nonPictureKinds is every stream of path that is not a picture, as kind() names it, with
// the moving picture's codec left out (the job re-encodes it).
func nonPictureKinds(t *testing.T, ffprobe, path string) []string {
	t.Helper()
	var out []string
	for _, s := range probedStreams(t, ffprobe, path) {
		switch {
		case s.Type == "video" && s.AttachedPic:
		case s.Type == "video":
			out = append(out, "video")
		default:
			out = append(out, s.kind())
		}
	}
	return out
}

// doneWithVmaf asserts the one row is DONE and carries what a real perceptual measurement
// records.
func doneWithVmaf(t *testing.T, ts *testStore) store.Outcome {
	t.Helper()
	row := onlyRow(t, ts)
	if row.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
	}
	o := row.Outcome
	if o.VmafMean == nil || o.VmafMin == nil || o.VmafChroma == nil || o.VmafModel == "" {
		t.Fatalf("the done row carries no VMAF measurement (%+v): the gate must have run", o)
	}
	return o
}

// everyGate turns the perceptual gate on at the floors the MP4 cover-art proof uses.
func everyGate(c *config.Config) {
	c.VmafEnable = boolPtr(true)
	c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 90, 50, 25
}

// sourceState is what "the job was not swapped" is judged against.
type sourceState struct {
	sum     string
	mtime   time.Time
	listing []string
}

func captureSource(t *testing.T, src string) sourceState {
	t.Helper()
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat %s: %v", src, err)
	}
	return sourceState{sum: sha256f(t, src), mtime: fi.ModTime(), listing: dirListing(t, filepath.Dir(src))}
}

// assertNotSwapped: no row reached done, the source's bytes and mtime are unchanged, and no
// file the job wrote remains in the source directory or (when set) the scratch directory.
func assertNotSwapped(t *testing.T, ts *testStore, src string, before sourceState, scratch string) {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Status == store.Done {
			t.Fatalf("a row reached done (%s): the job was swapped", r.Path)
		}
	}
	if got := sha256f(t, src); got != before.sum {
		t.Fatalf("the source's bytes changed: sha256 %s, was %s", got, before.sum)
	}
	if fi, err := os.Stat(src); err != nil || !fi.ModTime().Equal(before.mtime) {
		t.Fatalf("the source's mtime changed (or it is gone): %v", err)
	}
	if got := dirListing(t, filepath.Dir(src)); !equalStrings(got, before.listing) {
		t.Fatalf("the source directory changed: before %v, after %v", before.listing, got)
	}
	if scratch != "" {
		if got := dirListing(t, scratch); !equalStrings(got, []string{"."}) {
			t.Fatalf("the scratch directory is not empty: %v", got)
		}
	}
}

// failedReason is the reason the one row recorded, asserting it did not reach done.
func failedReason(t *testing.T, ts *testStore) string {
	t.Helper()
	row := onlyRow(t, ts)
	if row.Status == store.Done {
		t.Fatalf("the job reached done: nothing rejected it")
	}
	return row.Outcome.Reason
}

// substitutedEncoder runs one ffmpeg command in place of the production encoder and records
// the shape of what it wrote, so a case can prove its output was the shape it is about.
type substitutedEncoder struct {
	mu    sync.Mutex
	shape []string
}

func (s *substitutedEncoder) encoder(t *testing.T, ffmpeg, ffprobe string,
	argv func(in, out string) [][]string) EncoderFunc {
	return func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		for _, args := range argv(in, out) {
			if b, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				return fmt.Errorf("substituted encoder: %v: %s", err, b)
			}
		}
		var kinds []string
		for _, r := range probedStreams(t, ffprobe, out) {
			kinds = append(kinds, r.kind())
		}
		s.mu.Lock()
		s.shape = kinds
		s.mu.Unlock()
		return nil
	}
}

func (s *substitutedEncoder) wrote() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shape
}

// hevcBody is the encode half of a substituted encoder's argv.
var hevcBody = []string{"-pix_fmt", "yuv420p10le", "-preset", "ultrafast", "-crf", "22",
	"-x265-params", "log-level=error"}

func ffArgs(in string, body []string, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in}
	args = append(args, body...)
	return append(args, "--", out)
}

// [AC-1] The report's file: a Matroska source with two mjpeg pictures at stream indexes 9 and
// 10, output container mkv, every gate on. It swaps, and the output carries exactly those two
// pictures, in order, byte for byte, plus every non-picture stream the source carried.
func TestCoverArt_AC1_ReportShapedMatroskaSwapsWithItsPictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d, side := t.TempDir(), t.TempDir()
	src := filepath.Join(d, "The Secret Agent (2025).mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
	wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")
	wantOthers := nonPictureKinds(t, ffprobe, src)

	ts := run(t, ffmpeg, ffprobe, d, nil, everyGate)

	doneWithVmaf(t, ts)
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("the file at the target path is %q, want hevc: no swap happened", got)
	}
	assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))
	if got := nonPictureKinds(t, ffprobe, src); !equalStrings(got, wantOthers) {
		t.Fatalf("the output's non-picture streams are %v, the source carried %v", got, wantOthers)
	}
}

// [AC-2] The same source under a remux-only profile swaps and keeps both pictures, in order,
// byte for byte.
func TestCoverArt_AC2_RemuxOnlyCarriesThePictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d, side := t.TempDir(), t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
	wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.RemuxOnly = boolPtr(true) })

	if row := onlyRow(t, ts); row.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
	}
	assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))
}

// [AC-3] An MP4 source with one picture, output container mkv, swaps to the .mkv target and
// the output carries the picture byte for byte.
func TestCoverArt_AC3_MP4PictureCarriedIntoMatroska(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d, side := t.TempDir(), t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	target := filepath.Join(d, "movie.mkv")
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, src, "8M")
	wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.ContainerExt = "mkv" })

	if row := onlyRow(t, ts); row.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
	}
	if got := codecOf(t, ffprobe, target); got != "hevc" {
		t.Fatalf("the .mkv target is %q, want hevc", got)
	}
	if exists(src) {
		t.Fatalf("the .mp4 source is still there beside the .mkv target")
	}
	assertVideoStreamShape(t, ffprobe, target, []bool{false, true})
	assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, target, side, "output"))
}

// [AC-5] The report-shaped source under an audio language list that drops one of its audio
// tracks: it swaps, keeps both pictures, does not carry the dropped track, and records the
// drop exactly as the same source without pictures records it.
func TestCoverArt_AC5_SelectionDropsATrackAndKeepsThePictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	keep := func(c *config.Config) { c.AudioLanguages = []string{"eng", "fre"} }

	// The control: the identical stream layout with no pictures.
	cd := t.TempDir()
	control := filepath.Join(cd, "movie.mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, control, reportShape{})
	controlRow := onlyRow(t, run(t, ffmpeg, ffprobe, cd, nil, keep))
	if controlRow.Status != store.Done {
		t.Fatalf("the no-picture control did not swap: %s", controlRow.Outcome.Reason)
	}

	d, side := t.TempDir(), t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
	wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")

	row := onlyRow(t, run(t, ffmpeg, ffprobe, d, nil, keep))
	if row.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
	}
	assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))

	var audio []string
	for _, s := range probedStreams(t, ffprobe, src) {
		if s.Type == "audio" {
			audio = append(audio, s.Language)
		}
	}
	if !equalStrings(audio, []string{"eng", "fre"}) {
		t.Fatalf("the output carries audio %v, want [eng fre]: the ger track was selected away", audio)
	}
	want := store.RecordDroppedStreams([]store.DroppedStream{{Index: 3, Type: "audio", Language: "ger"}})
	if got := row.Outcome.DroppedStreams.Encode(); got != want.Encode() {
		t.Fatalf("the row records the drop as %q, want %q", got, want.Encode())
	}
	if got := controlRow.Outcome.DroppedStreams.Encode(); got != row.Outcome.DroppedStreams.Encode() {
		t.Fatalf("the picture source records its drop as %q and the same layout without pictures "+
			"as %q: a drop must be recorded the same way", row.Outcome.DroppedStreams.Encode(), got)
	}
}

// [AC-6] A Matroska source carrying a font ahead of its two pictures swaps, keeps the font
// with its filename, and keeps both pictures byte for byte.
func TestCoverArt_AC6_FontAttachmentSurvivesBesideThePictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d, side := t.TempDir(), t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true, font: true})
	wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")
	fonts := func(path string) []string {
		var names []string
		for _, s := range probedStreams(t, ffprobe, path) {
			if s.Type == "attachment" {
				names = append(names, s.Filename)
			}
		}
		return names
	}
	wantFonts := fonts(src)

	ts := run(t, ffmpeg, ffprobe, d, nil, nil)

	if row := onlyRow(t, ts); row.Status != store.Done {
		t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
	}
	if got := fonts(src); !equalStrings(got, wantFonts) {
		t.Fatalf("the output carries non-image attachments %v, the source carried %v", got, wantFonts)
	}
	assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))
}

// [AC-7] An output carrying the pictures only as plain video streams, the reported failure's
// shape, is not swapped, and the reason names the check and the attached picture.
func TestCoverArt_AC7_PicturesAsPlainVideoAreRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
	before := captureSource(t, src)

	var sub substitutedEncoder
	enc := sub.encoder(t, ffmpeg, ffprobe, func(in, out string) [][]string {
		body := append([]string{"-map", "0", "-map", "-0:d?", "-c", "copy", "-c:v", "libx265",
			"-c:v:1", "copy", "-c:v:2", "copy"}, hevcBody...)
		return [][]string{ffArgs(in, body, out)}
	})
	ts := run(t, ffmpeg, ffprobe, d, enc, nil)

	if got := sub.wrote(); strings.Count(strings.Join(got, " "), "video/") != 3 ||
		strings.Contains(strings.Join(got, " "), "picture/") {
		t.Fatalf("the substituted output was %v, want three plain video streams and no picture", got)
	}
	reason := failedReason(t, ts)
	for _, want := range []string{"intended-stream check failed", "attached picture"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the recorded reason does not contain %q: %s", want, reason)
		}
	}
	assertNotSwapped(t, ts, src, before, "")
}

// [AC-8] An output carrying zero pictures, or one of the two, is not swapped, and the reason
// names the check and the attached picture.
func TestCoverArt_AC8_LostPicturesAreRejected(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name     string
		pictures int
		argv     func(in, out string) [][]string
	}{
		{"zero pictures", 0, func(in, out string) [][]string {
			body := append([]string{"-map", "0", "-map", "-0:d?", "-map", "-0:9", "-map", "-0:10",
				"-c", "copy", "-c:v", "libx265"}, hevcBody...)
			return [][]string{ffArgs(in, body, out)}
		}},
		{"one of the two", 1, func(in, out string) [][]string {
			pic := filepath.Join(t.TempDir(), "first-picture")
			extract := ffArgs(in, []string{"-map", "0:9", "-c", "copy", "-frames:v", "1", "-f", "image2"}, pic)
			body := append([]string{"-map", "0", "-map", "-0:d?", "-map", "-0:9", "-map", "-0:10",
				"-c", "copy", "-c:v", "libx265", "-attach", pic,
				"-metadata:s:t:0", "mimetype=image/jpeg", "-metadata:s:t:0", "filename=cover.jpg"}, hevcBody...)
			return [][]string{extract, ffArgs(in, body, out)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
			before := captureSource(t, src)

			var sub substitutedEncoder
			ts := run(t, ffmpeg, ffprobe, d, sub.encoder(t, ffmpeg, ffprobe, tc.argv), nil)

			if got := strings.Count(strings.Join(sub.wrote(), " "), "picture/"); got != tc.pictures {
				t.Fatalf("the substituted output carried %d picture(s) (%v), want %d",
					got, sub.wrote(), tc.pictures)
			}
			reason := failedReason(t, ts)
			for _, want := range []string{"intended-stream check failed", "attached picture"} {
				if !strings.Contains(reason, want) {
					t.Errorf("the recorded reason does not contain %q: %s", want, reason)
				}
			}
			assertNotSwapped(t, ts, src, before, "")
		})
	}
}

// failingFFmpeg writes a wrapper around the real ffmpeg that numbers every invocation. On
// invocation failAt it runs the real binary, so whatever that invocation writes is on disk,
// and then exits non-zero; every other invocation is the real binary. failAt 0 fails none.
func failingFFmpeg(t *testing.T, dir, real string, failAt int) (wrapper, counter string) {
	t.Helper()
	wrapper = filepath.Join(dir, "failing-ffmpeg.sh")
	counter = filepath.Join(dir, "failing-ffmpeg.count")
	script := "#!/bin/sh\n" +
		"n=$(( $(cat \"" + counter + "\" 2>/dev/null || echo 0) + 1 ))\n" +
		"echo \"$n\" > \"" + counter + "\"\n" +
		"if [ \"$n\" = \"" + strconv.Itoa(failAt) + "\" ]; then\n" +
		"  \"" + real + "\" \"$@\"\n" +
		"  exit 1\n" +
		"fi\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write the failing ffmpeg wrapper: %v", err)
	}
	return wrapper, counter
}

func invocations(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read %s: %v", counter, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse %s: %v", counter, err)
	}
	return n
}

// [AC-9] For each ffmpeg invocation a passing run of the report-shaped source makes, a run in
// which that invocation exits non-zero is not swapped and leaves no file the job wrote in the
// source directory or the scratch directory - with and without a scratch directory.
func TestCoverArt_AC9_AFailedInvocationLeavesNothingBehind(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, withScratch := range []bool{false, true} {
		t.Run("scratch="+strconv.FormatBool(withScratch), func(t *testing.T) {
			job := func(t *testing.T, failAt int) (ts *testStore, src, scratch, counter string, before sourceState) {
				d := t.TempDir()
				src = filepath.Join(d, "movie.mkv")
				mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
				before = captureSource(t, src)
				if withScratch {
					scratch = t.TempDir()
				}
				wrapper, counter := failingFFmpeg(t, t.TempDir(), ffmpeg, failAt)
				ts = run(t, wrapper, ffprobe, d, nil, func(c *config.Config) {
					everyGate(c)
					c.ScratchDir = scratch
				})
				return ts, src, scratch, counter, before
			}

			ts, _, _, counter, _ := job(t, 0)
			if row := onlyRow(t, ts); row.Status != store.Done {
				t.Fatalf("the passing run did not swap (%s), so its invocations are not the job's", row.Outcome.Reason)
			}
			n := invocations(t, counter)
			if n < 3 {
				t.Fatalf("the passing run made %d ffmpeg invocation(s); the pictures and the encode alone are 3", n)
			}
			for k := 1; k <= n; k++ {
				t.Run(fmt.Sprintf("invocation %d of %d fails", k, n), func(t *testing.T) {
					ts, src, scratch, _, before := job(t, k)
					assertNotSwapped(t, ts, src, before, scratch)
				})
			}
		})
	}
}

// argvLoggingFFmpeg writes a wrapper around the real ffmpeg that appends each invocation's
// argv to a log, one line per invocation, arguments separated by 0x1f.
func argvLoggingFFmpeg(t *testing.T, dir, real string) (wrapper, log string) {
	t.Helper()
	wrapper = filepath.Join(dir, "argv-ffmpeg.sh")
	log = filepath.Join(dir, "argv-ffmpeg.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\037' \"$a\" >> \"" + log + "\"; done\n" +
		"printf '\\n' >> \"" + log + "\"\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write the argv-logging ffmpeg wrapper: %v", err)
	}
	return wrapper, log
}

// loggedArgv reads the log back, each per-run path under root rewritten to <ROOT>.
func loggedArgv(t *testing.T, log, root string) [][]string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read %s: %v", log, err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		args := strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f")
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], root, "<ROOT>")
		}
		calls = append(calls, args)
	}
	return calls
}

// [AC-10] A source with no picture gets exactly the sequence of ffmpeg invocations the base
// commit builds for it, on the re-encode path, the remux-only path and under a selection that
// drops a track. The sequences below were captured from the base commit's tree.
func TestCoverArt_AC10_NoPictureArgvIsTheBaseCommits(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	arms := []struct {
		name   string
		mutate func(*config.Config)
		want   [][]string
	}{
		{"re-encode", nil, ac10ReEncode},
		{"remux-only", func(c *config.Config) { c.RemuxOnly = boolPtr(true) }, ac10RemuxOnly},
		{"selection drops a track", func(c *config.Config) { c.AudioLanguages = []string{"eng", "fre"} }, ac10Selection},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{})
			wrapper, log := argvLoggingFFmpeg(t, t.TempDir(), ffmpeg)

			ts := run(t, wrapper, ffprobe, d, nil, arm.mutate)

			if row := onlyRow(t, ts); row.Status != store.Done {
				t.Fatalf("status = %q, want %q: %s", row.Status, store.Done, row.Outcome.Reason)
			}
			got := loggedArgv(t, log, d)
			if len(got) != len(arm.want) {
				t.Fatalf("the job made %d ffmpeg invocation(s), the base commit makes %d:\n%s",
					len(got), len(arm.want), renderArgv(got))
			}
			for i := range got {
				if !equalStrings(got[i], arm.want[i]) {
					t.Errorf("invocation %d is\n  %q\nthe base commit builds\n  %q", i+1, got[i], arm.want[i])
				}
			}
		})
	}
}

func renderArgv(calls [][]string) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "%#v,\n", c)
	}
	return b.String()
}

// The base commit's invocations for the no-picture report-shaped source, captured through
// argvLoggingFFmpeg from that tree. decodeCheck and the two stream hashes are the gate's own
// invocations; the first entry of each sequence is the encode.
var (
	ac10DecodeCheck = []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-err_detect", "+explode",
		"-i", "<ROOT>/movie.__transcoding__.mkv", "-map", "0:v", "-f", "null", "-"}

	ac10ReEncode = [][]string{
		{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", "<ROOT>/movie.mkv",
			"-map", "0", "-map", "-0:d?", "-c", "copy", "-c:v", "libx265",
			"-pix_fmt", "yuv420p10le", "-color_range", "tv", "-fps_mode", "passthrough",
			"-preset", "ultrafast", "-crf", "22", "-x265-params", "log-level=error",
			"--", "<ROOT>/movie.__transcoding__.mkv"},
		ac10DecodeCheck,
	}

	ac10RemuxOnly = [][]string{
		{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", "<ROOT>/movie.mkv",
			"-map", "0", "-map", "-0:d?", "-c", "copy", "--", "<ROOT>/movie.__transcoding__.mkv"},
		ac10DecodeCheck,
		{"-hide_banner", "-nostdin", "-v", "error", "-i", "<ROOT>/movie.mkv",
			"-map", "0:v", "-c", "copy", "-f", "streamhash", "-hash", "md5", "-"},
		{"-hide_banner", "-nostdin", "-v", "error", "-i", "<ROOT>/movie.__transcoding__.mkv",
			"-map", "0:v", "-c", "copy", "-f", "streamhash", "-hash", "md5", "-"},
	}

	ac10Selection = [][]string{
		{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", "<ROOT>/movie.mkv",
			"-map", "0:0", "-map", "0:1", "-map", "0:2", "-map", "0:4", "-map", "0:5",
			"-map", "0:6", "-map", "0:7", "-map", "0:8", "-c", "copy", "-c:v", "libx265",
			"-pix_fmt", "yuv420p10le", "-color_range", "tv", "-fps_mode", "passthrough",
			"-preset", "ultrafast", "-crf", "22", "-x265-params", "log-level=error",
			"--", "<ROOT>/movie.__transcoding__.mkv"},
		ac10DecodeCheck,
	}
)

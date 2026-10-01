package subtitle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The fixtures are synthetic: a lavfi picture and a three-cue subtitle written here, muxed by
// the pinned ffmpeg. Nothing is read from a real library.

func tools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, ffprobe = "ffmpeg", "ffprobe"
	if v := os.Getenv("HOLDFAST_FFMPEG"); v != "" {
		ffmpeg = v
	}
	if v := os.Getenv("HOLDFAST_FFPROBE"); v != "" {
		ffprobe = v
	}
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found - the sidecar proofs need the pinned ffmpeg and ffprobe: %v", b, err)
		}
	}
	return ffmpeg, ffprobe
}

const threeCues = "1\n00:00:00,100 --> 00:00:00,500\nHello\n\n" +
	"2\n00:00:00,600 --> 00:00:01,000\nWorld\n\n" +
	"3\n00:00:01,100 --> 00:00:01,500\nAgain\n\n"

func ff(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// track is one subtitle stream of a fixture: its format, language tag and forced flag.
type track struct {
	format string // srt, ass or vtt input file
	lang   string // "" writes no tag
	forced bool
}

// mkSource writes a Matroska file carrying a short picture and the given subtitle tracks
// (three cues each), plus a font attachment when font is set, and returns its stream list as
// internal/probe reads it.
func mkSource(t *testing.T, ffmpeg, ffprobe, path string, font bool, tracks ...track) []probe.Stream {
	t.Helper()
	in := t.TempDir()
	srt := filepath.Join(in, "cues.srt")
	if err := os.WriteFile(srt, []byte(threeCues), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{"srt": srt}
	for _, f := range []string{"ass", "vtt"} {
		p := filepath.Join(in, "cues."+f)
		ff(t, ffmpeg, "-i", srt, p)
		inputs[f] = p
	}
	args := []string{"-f", "lavfi", "-i", "testsrc=d=2:s=64x48:r=5"}
	for _, tr := range tracks {
		args = append(args, "-i", inputs[tr.format])
	}
	args = append(args, "-map", "0:v")
	for i := range tracks {
		args = append(args, "-map", fmt.Sprintf("%d:s", i+1))
	}
	args = append(args, "-c:v", "mpeg4", "-c:s", "copy")
	for i, tr := range tracks {
		if tr.lang != "" {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+tr.lang)
		}
		disp := "0"
		if tr.forced {
			disp = "forced"
		}
		args = append(args, fmt.Sprintf("-disposition:s:%d", i), disp)
	}
	if font {
		fp := filepath.Join(in, "font.ttf")
		if err := os.WriteFile(fp, []byte("not a real font"), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-attach", fp, "-metadata:s:t:0", "mimetype=application/x-truetype-font",
			"-metadata:s:t:0", "filename=font.ttf")
	}
	ff(t, ffmpeg, append(args, path)...)
	streams, ok := probe.New(ffmpeg, ffprobe).Streams(context.Background(), path)
	if !ok {
		t.Fatalf("probe could not enumerate the fixture %s", path)
	}
	return streams
}

// request is a Request over src publishing into dir under stem, with temps named as the
// engine names them (after a working file, with a .subtitle<i> suffix).
func request(ffmpeg, ffprobe, src, dir, stem string, streams []probe.Stream) Request {
	return Request{
		FFmpeg: ffmpeg, FFprobe: ffprobe, Source: src, Dir: dir, Stem: stem, Streams: streams,
		TempPath: func(i int) string {
			return filepath.Join(dir, fmt.Sprintf("%s.__transcoding__.mkv.holdfast-part.subtitle%d", stem, i))
		},
		Perm: 0o644,
	}
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func events(t *testing.T, ffprobe, path, demuxer string) (string, int) {
	t.Helper()
	codec, n, err := countEvents(context.Background(), ffprobe, path, demuxer)
	if err != nil {
		t.Fatalf("%s does not parse back: %v", path, err)
	}
	return codec, n
}

// SubRip, ASS and WebVTT each go to `<name>.<lang>[.forced].<ext>` beside the replacement,
// in their own format, with the source stream's three events; the forced ASS track is named
// `.forced`, the untagged WebVTT track `und`, an upper-case tag is lowercased; the ASS
// sidecar is recorded as losing the source's font; and no temp is left behind.
func TestSidecar_SRTASSWebVTTNamedByLanguageAndForced(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "film.avi.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, true,
		track{"srt", "ENG", false}, track{"ass", "fre", true}, track{"vtt", "", false})

	p := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams))
	if len(p.Temps()) != 3 {
		t.Fatalf("gated %d sidecars, want 3: %+v", len(p.Temps()), p.Records())
	}
	for _, tmp := range p.Temps() {
		if _, err := os.Stat(tmp); err != nil {
			t.Fatalf("a gated sidecar is not at its temp: %v", err)
		}
	}
	if got := listing(t, dir); len(got) != 4 {
		t.Fatalf("before Publish the directory holds %v: a final name appeared early", got)
	}
	recs := p.Publish(0o640)

	want := []struct {
		name, demuxer, codec string
		forced, fonts        bool
		lang                 string
	}{
		{"film.eng.srt", "srt", "subrip", false, false, "eng"},
		{"film.fre.forced.ass", "ass", "ass", true, true, "fre"},
		{"film.und.vtt", "webvtt", "webvtt", false, false, ""},
	}
	if len(recs) != len(want) {
		t.Fatalf("recorded %d streams, want %d: %+v", len(recs), len(want), recs)
	}
	for i, w := range want {
		r := recs[i]
		path := filepath.Join(dir, w.name)
		if r.Path != path || r.Skipped != "" || r.Detail != "" {
			t.Errorf("stream %d: path %q skipped %q detail %q, want published at %s", i, r.Path, r.Skipped, r.Detail, path)
		}
		if r.Index != i+1 || r.Codec != w.codec || r.Forced != w.forced || r.FontsLost != w.fonts || r.Language != w.lang {
			t.Errorf("stream %d recorded as %+v, want index %d codec %s forced %v fonts_lost %v language %q",
				i, r, i+1, w.codec, w.forced, w.fonts, w.lang)
		}
		if r.Events == nil || *r.Events != 3 {
			t.Errorf("stream %d: events %v, want 3", i, r.Events)
		}
		codec, n := events(t, ffprobe, path, w.demuxer)
		if codec != w.codec || n != 3 {
			t.Errorf("%s parses back as %s with %d events, want %s with 3", w.name, codec, n, w.codec)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o640 {
			t.Errorf("%s: mode %v (%v), want 0640", w.name, fi, err)
		}
	}
	got := listing(t, dir)
	wantList := []string{"film.avi.mkv", "film.eng.srt", "film.fre.forced.ass", "film.und.vtt"}
	if !reflect.DeepEqual(got, wantList) {
		t.Fatalf("directory holds %v, want %v (no temp left)", got, wantList)
	}
	// Discard after Publish touches nothing.
	p.Discard()
	if got := listing(t, dir); !reflect.DeepEqual(got, wantList) {
		t.Fatalf("Discard after Publish changed the directory: %v", got)
	}
	if recs2 := p.Records(); !reflect.DeepEqual(recs2, recs) {
		t.Fatalf("Discard after Publish changed the records: %+v", recs2)
	}
}

// A file already at a sidecar's name is never overwritten: skipped at Prepare when it is
// there first, and at Publish when it appears in between; its bytes are unchanged either
// way, the skip is recorded, and the temp is gone.
func TestSidecar_ExistingSidecarIsNeverOverwritten(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false}, track{"srt", "ger", false})
	mine := []byte("an operator's own subtitle file")
	eng := filepath.Join(dir, "film.eng.srt")
	if err := os.WriteFile(eng, mine, 0o600); err != nil {
		t.Fatal(err)
	}

	p := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams))
	if len(p.Temps()) != 1 {
		t.Fatalf("gated %d sidecars, want 1 (the eng name is taken): %+v", len(p.Temps()), p.Records())
	}
	// The ger name appears after the gate and before the publish.
	ger := filepath.Join(dir, "film.ger.srt")
	if err := os.WriteFile(ger, mine, 0o600); err != nil {
		t.Fatal(err)
	}
	recs := p.Publish(0o644)
	for i, path := range []string{eng, ger} {
		if recs[i].Skipped != SkipExists || recs[i].Path != "" {
			t.Errorf("stream %d: recorded %+v, want skipped %s", i, recs[i], SkipExists)
		}
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, mine) {
			t.Errorf("%s was overwritten: %q (%v)", path, b, err)
		}
	}
	if recs[0].Events != nil {
		t.Errorf("a stream skipped before extraction records events %v", *recs[0].Events)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"film.eng.srt", "film.ger.srt", "src.mkv"}) {
		t.Fatalf("directory holds %v: a temp was left behind", got)
	}
}

// Two streams that would share a name: the first is published, the later one is skipped
// with its reason, and the bytes under the name are the first stream's.
func TestSidecar_ANameTakenByAnEarlierStreamIsSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false,
		track{"srt", "eng", false}, track{"srt", "eng", false}, track{"srt", "eng", true})
	recs := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams)).Publish(0o644)
	if recs[0].Path != filepath.Join(dir, "film.eng.srt") {
		t.Errorf("first stream: %+v, want published as film.eng.srt", recs[0])
	}
	if recs[1].Skipped != SkipNameTaken || recs[1].Path != "" {
		t.Errorf("second stream: %+v, want skipped %s", recs[1], SkipNameTaken)
	}
	if recs[2].Path != filepath.Join(dir, "film.eng.forced.srt") {
		t.Errorf("forced stream: %+v, want published as film.eng.forced.srt", recs[2])
	}
}

// THE PARSE-BACK GATE REDS ON A TRUNCATED SIDECAR. The written file is cut short after
// extraction: it still parses, with one event where the source stream has three, so the gate
// refuses it, nothing is published, the temp is removed, and the source is unchanged. An
// emptied file does not parse at all and is refused under the parse-back token.
func TestSidecar_TruncatedSidecarFailsTheCountGate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, c := range []struct {
		name  string
		keep  int64
		token string
	}{
		{"truncated", 60, FailEventCount},
		{"emptied", 0, FailParseBack},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.mkv")
			streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false})
			before, _ := os.ReadFile(src)
			r := request(ffmpeg, ffprobe, src, dir, "film", streams)
			r.AfterExtract = func(temp string) {
				if err := os.Truncate(temp, c.keep); err != nil {
					t.Fatal(err)
				}
			}
			p := Prepare(context.Background(), r)
			if len(p.Temps()) != 0 {
				t.Fatalf("a %s sidecar passed the gate: %+v", c.name, p.Records())
			}
			recs := p.Publish(0o644)
			if recs[0].Skipped != c.token || recs[0].Path != "" || recs[0].Detail == "" {
				t.Fatalf("recorded %+v, want %s with its detail", recs[0], c.token)
			}
			if c.token == FailEventCount && !strings.Contains(recs[0].Detail, "3 events, sidecar has 1") {
				t.Errorf("detail %q does not name both counts", recs[0].Detail)
			}
			if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mkv"}) {
				t.Fatalf("directory holds %v, want only the source", got)
			}
			if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
				t.Fatal("the source changed")
			}
		})
	}
}

// mov_text is skipped with its reason, never converted (I17), and nothing is written.
func TestSidecar_MovTextSkipsWithItsReason(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	in := filepath.Join(t.TempDir(), "cues.srt")
	if err := os.WriteFile(in, []byte(threeCues), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.mp4")
	ff(t, ffmpeg, "-f", "lavfi", "-i", "testsrc=d=2:s=64x48:r=5", "-i", in, "-map", "0", "-map", "1",
		"-c:v", "mpeg4", "-c:s", "mov_text", "-metadata:s:s:0", "language=eng", src)
	streams, ok := probe.New(ffmpeg, ffprobe).Streams(context.Background(), src)
	if !ok || streams[1].Codec != "mov_text" {
		t.Fatalf("fixture is not an MP4 with a mov_text stream: %+v", streams)
	}
	recs := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams)).Publish(0o644)
	if len(recs) != 1 || recs[0].Skipped != SkipMovText || recs[0].Language != "eng" || recs[0].Codec != "mov_text" {
		t.Fatalf("recorded %+v, want one %s record", recs, SkipMovText)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mp4"}) {
		t.Fatalf("directory holds %v, want only the source", got)
	}
}

// Bitmap subtitles skip with their reason. The pinned ffmpeg cannot write a bitmap subtitle
// from anything a test can synthesise ("Subtitle encoding currently only possible from text
// to text or bitmap to bitmap"), so the bitmap streams here are stream-list entries beside a
// real file's own: the decision is taken on the codec the stream list names, before any
// extraction, so this is the path a real PGS or VobSub stream takes.
func TestSidecar_BitmapSkipsWithItsReason(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false})
	for i, c := range []string{"hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "xsub", "eia_608"} {
		streams = append(streams, probe.Stream{Index: 10 + i, Type: probe.TypeSubtitle, Codec: c, Language: "eng"})
	}
	recs := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams)).Publish(0o644)
	want := []string{"", SkipBitmap, SkipBitmap, SkipBitmap, SkipBitmap, SkipUnsupported}
	for i, w := range want {
		if recs[i].Skipped != w {
			t.Errorf("record %d (%s): skipped %q, want %q", i, recs[i].Codec, recs[i].Skipped, w)
		}
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"film.eng.srt", "src.mkv"}) {
		t.Fatalf("directory holds %v", got)
	}
}

// A language tag a file name cannot carry is skipped, never cleaned into a name.
func TestSidecar_UnusableLanguageTagIsSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false})
	streams[1].Language = "../x"
	recs := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams)).Publish(0o644)
	if recs[0].Skipped != SkipLanguageTag || recs[0].Events != nil {
		t.Fatalf("recorded %+v, want %s before any extraction", recs[0], SkipLanguageTag)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mkv"}) {
		t.Fatalf("directory holds %v", got)
	}
}

// A job that carries no subtitle stream runs no probe at all and records an empty set.
func TestSidecar_NoSubtitleStreamRunsNoProbe(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-binary")
	r := Request{FFmpeg: missing, FFprobe: missing, Source: "x", Dir: t.TempDir(), Stem: "film",
		Streams:  []probe.Stream{{Index: 0, Type: probe.TypeVideo}, {Index: 1, Type: probe.TypeAudio}},
		TempPath: func(int) string { t.Fatal("a temp was asked for"); return "" }}
	p := Prepare(context.Background(), r)
	if recs := p.Records(); recs == nil || len(recs) != 0 {
		t.Fatalf("records %#v, want a recorded empty set", recs)
	}
}

// The source's counts cannot be read: every text stream is a failure under its token, its
// font note still recorded, and nothing is extracted.
func TestSidecar_UnreadableSourceCountsFailEveryTextStream(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, true, track{"ass", "eng", false}, track{"srt", "fre", false})
	r := request(ffmpeg, filepath.Join(dir, "no-such-ffprobe"), src, dir, "film", streams)
	recs := Prepare(context.Background(), r).Publish(0o644)
	for i, rec := range recs {
		if rec.Skipped != FailSourceCount || rec.Detail == "" || rec.Events != nil {
			t.Errorf("record %d: %+v, want %s with detail", i, rec, FailSourceCount)
		}
	}
	if !recs[0].FontsLost || recs[1].FontsLost {
		t.Errorf("fonts_lost recorded as %v/%v, want the ASS stream only", recs[0].FontsLost, recs[1].FontsLost)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mkv"}) {
		t.Fatalf("directory holds %v", got)
	}
}

// An extraction that fails removes its temp and records the failure.
func TestSidecar_FailedExtractionLeavesNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false})
	r := request("false", ffprobe, src, dir, "film", streams)
	recs := Prepare(context.Background(), r).Publish(0o644)
	if recs[0].Skipped != FailExtract || recs[0].Detail == "" {
		t.Fatalf("recorded %+v, want %s", recs[0], FailExtract)
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mkv"}) {
		t.Fatalf("directory holds %v", got)
	}
	// A temp that is already taken is never written over.
	r = request(ffmpeg, ffprobe, src, dir, "film", streams)
	taken := r.TempPath(0)
	if err := os.WriteFile(taken, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	recs = Prepare(context.Background(), r).Publish(0o644)
	if recs[0].Skipped != FailExtract || !strings.Contains(recs[0].Detail, "claim the temp") {
		t.Fatalf("recorded %+v, want %s on the claim", recs[0], FailExtract)
	}
}

// A job that ends without a committed swap discards every gated temp and records each as
// not published.
func TestSidecar_DiscardRemovesEveryGatedTemp(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false}, track{"vtt", "fre", false})
	p := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams))
	if len(p.Temps()) != 2 {
		t.Fatalf("gated %d, want 2", len(p.Temps()))
	}
	p.Discard()
	for _, rec := range p.Records() {
		if rec.Skipped != FailNotPublished || rec.Path != "" {
			t.Errorf("record %+v, want %s", rec, FailNotPublished)
		}
	}
	if len(p.Temps()) != 0 {
		t.Fatal("temps still pending after Discard")
	}
	if got := listing(t, dir); !reflect.DeepEqual(got, []string{"src.mkv"}) {
		t.Fatalf("directory holds %v", got)
	}
	if recs := p.Publish(0o644); recs[0].Path != "" || recs[1].Path != "" {
		t.Fatal("Publish after Discard published something")
	}
}

// A gated temp that vanished before Publish is a publish failure, not a published sidecar.
func TestSidecar_PublishFailureIsRecorded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	streams := mkSource(t, ffmpeg, ffprobe, src, false, track{"srt", "eng", false})
	p := Prepare(context.Background(), request(ffmpeg, ffprobe, src, dir, "film", streams))
	_ = os.Remove(p.Temps()[0])
	recs := p.Publish(0o644)
	if recs[0].Skipped != FailPublish || recs[0].Detail == "" || recs[0].Path != "" {
		t.Fatalf("recorded %+v, want %s", recs[0], FailPublish)
	}
	if _, err := os.Lstat(filepath.Join(dir, "film.eng.srt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a sidecar was published: %v", err)
	}
}

func TestSubtitle_ClassifyEveryCodec(t *testing.T) {
	for codec, want := range map[string]struct {
		f    Format
		skip string
	}{
		"subrip":            {Format{"subrip", "srt", "srt"}, ""},
		" SubRip ":          {Format{"subrip", "srt", "srt"}, ""},
		"ass":               {Format{"ass", "ass", "ass"}, ""},
		"webvtt":            {Format{"webvtt", "webvtt", "vtt"}, ""},
		"hdmv_pgs_subtitle": {Format{}, SkipBitmap},
		"dvd_subtitle":      {Format{}, SkipBitmap},
		"dvb_subtitle":      {Format{}, SkipBitmap},
		"xsub":              {Format{}, SkipBitmap},
		"mov_text":          {Format{}, SkipMovText},
		"ssa":               {Format{}, SkipUnsupported},
		"text":              {Format{}, SkipUnsupported},
		"":                  {Format{}, SkipUnsupported},
	} {
		f, skip := Classify(codec)
		if f != want.f || skip != want.skip {
			t.Errorf("Classify(%q) = %+v, %q; want %+v, %q", codec, f, skip, want.f, want.skip)
		}
	}
}

func TestSubtitle_LanguageTokenAndName(t *testing.T) {
	for in, want := range map[string]string{
		"": "und", "  ": "und", "und": "und", "ENG": "eng", " fre ": "fre", "en": "en",
		"pt-BR": "pt-br", "zh-hant-tw": "zh-hant-tw",
		"e": "", "engl": "", "en_US": "", "../x": "", "en.forced": "", "e n": "", "en-": "", "123": "",
	} {
		got, ok := LanguageToken(in)
		if ok != (want != "") || got != want {
			t.Errorf("LanguageToken(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got := Name("A Film (2025)", "eng", true, "srt"); got != "A Film (2025).eng.forced.srt" {
		t.Errorf("Name forced = %q", got)
	}
	if got := Name("A Film (2025)", "und", false, "vtt"); got != "A Film (2025).und.vtt" {
		t.Errorf("Name = %q", got)
	}
}

func TestSubtitle_IsFontAndIsFailure(t *testing.T) {
	att := func(mime, name string) probe.Stream {
		return probe.Stream{Type: probe.TypeAttachment, MimeType: mime, Filename: name}
	}
	for _, c := range []struct {
		s    probe.Stream
		want bool
	}{
		{att("application/x-truetype-font", ""), true},
		{att("font/otf", ""), true},
		{att("FONT/TTF", ""), true},
		{att("application/vnd.ms-opentype", ""), true},
		{att("application/octet-stream", "Title.TTF"), true},
		{att("", "x.woff2"), true},
		{att("image/jpeg", "cover.jpg"), false},
		{att("application/octet-stream", "notes.txt"), false},
		{probe.Stream{Type: probe.TypeVideo, MimeType: "font/ttf"}, false},
	} {
		if got := IsFont(c.s); got != c.want {
			t.Errorf("IsFont(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
	for _, tok := range []string{FailSourceCount, FailExtract, FailParseBack, FailEventCount, FailPublish, FailNotPublished} {
		if !IsFailure(tok) {
			t.Errorf("%s is not a failure", tok)
		}
	}
	for _, tok := range []string{SkipBitmap, SkipMovText, SkipUnsupported, SkipLanguageTag, SkipExists, SkipNameTaken, ""} {
		if IsFailure(tok) {
			t.Errorf("%s is a failure", tok)
		}
	}
}

// The extraction is a stream copy of one stream into its own format, nothing else.
func TestSubtitle_ExtractArgs(t *testing.T) {
	got := ExtractArgs("/lib/a.mkv", 3, "srt", "/lib/a.tmp")
	want := []string{"-hide_banner", "-nostdin", "-v", "error", "-y", "-i", "/lib/a.mkv",
		"-map", "0:3", "-c", "copy", "-f", "srt", "file:/lib/a.tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractArgs = %v, want %v", got, want)
	}
}

// fakeProbe writes an ffprobe stand-in that prints out and exits with code.
func fakeProbe(t *testing.T, out string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffprobe")
	script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", out, code)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every way the parse-back can fail to establish a count is a refusal, and a count is
// accepted only for the expected codec.
func TestSubtitle_GateReadsOnlyOneStreamOfItsOwnCodec(t *testing.T) {
	srt := Format{"subrip", "srt", "srt"}
	for _, c := range []struct {
		name, out string
		code      int
		token     string
	}{
		{"ok", `{"streams":[{"codec_name":"subrip","nb_read_packets":"3"}]}`, 0, ""},
		{"fewer", `{"streams":[{"codec_name":"subrip","nb_read_packets":"2"}]}`, 0, FailEventCount},
		{"more", `{"streams":[{"codec_name":"subrip","nb_read_packets":"4"}]}`, 0, FailEventCount},
		{"other codec", `{"streams":[{"codec_name":"ass","nb_read_packets":"3"}]}`, 0, FailParseBack},
		{"two streams", `{"streams":[{"codec_name":"subrip","nb_read_packets":"3"},{"codec_name":"subrip","nb_read_packets":"3"}]}`, 0, FailParseBack},
		{"no stream", `{"streams":[]}`, 0, FailParseBack},
		{"no count", `{"streams":[{"codec_name":"subrip","nb_read_packets":"N/A"}]}`, 0, FailParseBack},
		{"not json", `streams: 3`, 0, FailParseBack},
		{"exit", `{"streams":[{"codec_name":"subrip","nb_read_packets":"3"}]}`, 1, FailParseBack},
	} {
		token, err := gate(context.Background(), fakeProbe(t, c.out, c.code), "x", srt, 3)
		if token != c.token || (err == nil) != (c.token == "") {
			t.Errorf("%s: token %q err %v, want %q", c.name, token, err, c.token)
		}
	}
}

// The source probe reads forced and the count per stream, and a count it cannot read is not
// known; an answer it cannot parse is an error.
func TestSubtitle_SourceProbeReadsForcedAndCounts(t *testing.T) {
	out := `{"streams":[{"index":2,"codec_name":"subrip","nb_read_packets":"5","disposition":{"forced":1}},` +
		`{"index":4,"codec_name":"ass","nb_read_packets":"N/A","disposition":{"forced":0}},` +
		`{"index":5,"codec_name":"ass","nb_read_packets":"0","disposition":{"forced":0}},` +
		`{"index":6,"codec_name":"ass","nb_read_packets":"-1","disposition":{"forced":0}}]}`
	m, err := probeSource(context.Background(), fakeProbe(t, out, 0), "x")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]facts{2: {forced: true, events: 5, known: true}, 4: {known: false},
		5: {events: 0, known: true}, 6: {events: -1, known: false}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("probeSource = %+v, want %+v", m, want)
	}
	if _, err := probeSource(context.Background(), fakeProbe(t, "nope", 0), "x"); err == nil {
		t.Fatal("an unparseable answer was accepted")
	}
	if _, err := probeSource(context.Background(), fakeProbe(t, out, 1), "x"); err == nil {
		t.Fatal("a failing ffprobe was accepted")
	}
}

// Encode and parse round-trip, and the zero record is not recorded.
func TestSubtitle_StoreRecordRoundTrips(t *testing.T) {
	n := 3
	in := store.RecordSidecars([]store.Sidecar{{Index: 2, Codec: "subrip", Language: "eng", Path: "/l/f.eng.srt", Events: &n}})
	out := store.ParseSidecars(in.Encode())
	if !out.Recorded() || !reflect.DeepEqual(out.List(), in.List()) {
		t.Fatalf("round trip: %+v -> %+v", in.List(), out.List())
	}
}

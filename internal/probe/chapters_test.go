package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseChapters(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want ChapterTable
		ok   bool
	}{
		{"two titled chapters", `{"chapters":[
			{"id":1,"time_base":"1/1000000000","start":0,"start_time":"0.000000","end":1000000000,"end_time":"1.000000","tags":{"title":"Opening"}},
			{"id":2,"time_base":"1/1000000000","start":1000000000,"start_time":"1.000000","end":2000000000,"end_time":"2.500000","tags":{"title":"Close"}}],
			"format":{"start_time":"0.007000","duration":"2.500000"}}`,
			ChapterTable{Chapters: []Chapter{{0, 1, "Opening"}, {1, 2.5, "Close"}},
				StartTime: 0.007, StartKnown: true, Duration: 2.5, DurationKnown: true}, true},
		{"no chapters", `{"chapters":[],"format":{"start_time":"0.000000","duration":"2.000000"}}`,
			ChapterTable{StartKnown: true, Duration: 2, DurationKnown: true}, true},
		{"a chapter without a title", `{"chapters":[{"start_time":"0.000000","end_time":"1.000000"}],"format":{"start_time":"0.000000"}}`,
			ChapterTable{Chapters: []Chapter{{0, 1, ""}}, StartKnown: true}, true},
		{"no start time and an N/A duration", `{"chapters":[],"format":{"start_time":"N/A","duration":"N/A"}}`,
			ChapterTable{}, true},
		{"a chapter start that is not a number", `{"chapters":[{"start_time":"N/A","end_time":"1.000000"}]}`, ChapterTable{}, false},
		{"a chapter end that is not a number", `{"chapters":[{"start_time":"0.000000","end_time":"nan"}]}`, ChapterTable{}, false},
		{"output that is not JSON", `chapter|id=1`, ChapterTable{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseChapters([]byte(tc.in))
			if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseChapters = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	for _, s := range []string{"inf", "-Inf", "NaN", "", "N/A"} {
		if _, ok := seconds(s); ok {
			t.Errorf("seconds(%q) was read as a time", s)
		}
	}
}

// TestChapters_ReadsWhatFFmpegWrote runs the pinned ffmpeg: a Matroska file muxed with two
// titled chapters reads back as exactly those chapters, a file without any reads as an
// established empty table, and a file ffprobe cannot read is not established at all.
func TestChapters_ReadsWhatFFmpegWrote(t *testing.T) {
	ffmpeg, err := exec.LookPath(envOrDefault("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("the pinned ffmpeg is required: %v", err)
	}
	p := realProber()
	dir := t.TempDir()
	meta := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(meta, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=Opening\n"+
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=Close\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	with, without := filepath.Join(dir, "with.mkv"), filepath.Join(dir, "without.mkv")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=duration=2:size=64x48:rate=5", "-i", meta,
			"-map", "0:v", "-map_chapters", "1", "-c:v", "libx264", "-preset", "ultrafast", "--", with},
		{"-f", "lavfi", "-i", "testsrc2=duration=2:size=64x48:rate=5",
			"-c:v", "libx264", "-preset", "ultrafast", "--", without},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v\n%s", err, out)
		}
	}
	ctx := context.Background()
	got, ok := p.Chapters(ctx, with)
	if !ok {
		t.Fatal("ffprobe did not establish the chapter table of a file ffmpeg wrote")
	}
	if want := []Chapter{{0, 1, "Opening"}, {1, 2, "Close"}}; !reflect.DeepEqual(got.Chapters, want) {
		t.Errorf("chapters = %+v, want %+v", got.Chapters, want)
	}
	if !got.StartKnown || got.StartTime != 0 || !got.DurationKnown || got.Duration != 2 {
		t.Errorf("start/duration = %+v", got)
	}
	none, ok := p.Chapters(ctx, without)
	if !ok || len(none.Chapters) != 0 || !none.DurationKnown {
		t.Errorf("a file without chapters = %+v, %v; want an established empty table", none, ok)
	}
	if _, ok := p.Chapters(ctx, filepath.Join(dir, "missing.mkv")); ok {
		t.Error("a missing file's chapter table was established")
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

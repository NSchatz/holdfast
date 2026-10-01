package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/subtitle"
)

// Subtitle sidecars through a real job (docs/design/subtitles.md#sidecars). The extraction,
// naming and gate are proven case by case with the real ffmpeg in internal/subtitle; these
// cases are the few that need the engine: the sidecars appear beside the replacement only
// after its swap, a refused sidecar does not block the swap, and with the key off nothing at
// all happens. Each is one tiny real encode.

// mkSubtitledMatroska writes a two-second MPEG-4 Part 2 Matroska source carrying an English
// SubRip, a forced French ASS, an untagged WebVTT and a German SubRip track, three cues each.
func mkSubtitledMatroska(t *testing.T, ffmpeg, path string) {
	t.Helper()
	in := t.TempDir()
	srt := filepath.Join(in, "cues.srt")
	cues := "1\n00:00:00,100 --> 00:00:00,500\nHello\n\n2\n00:00:00,600 --> 00:00:01,000\nWorld\n\n" +
		"3\n00:00:01,100 --> 00:00:01,500\nAgain\n\n"
	if err := os.WriteFile(srt, []byte(cues), 0o644); err != nil {
		t.Fatal(err)
	}
	ass, vtt := filepath.Join(in, "cues.ass"), filepath.Join(in, "cues.vtt")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", srt, ass)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", srt, vtt)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-i", srt, "-i", ass, "-i", vtt, "-i", srt,
		"-map", "0:v", "-map", "1:s", "-map", "2:s", "-map", "3:s", "-map", "4:s",
		"-c:v", "mpeg4", "-q:v", "2", "-c:s", "copy",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=fre", "-metadata:s:s:3", "language=ger",
		"-disposition:s:0", "0", "-disposition:s:1", "forced", "-disposition:s:2", "0", "-disposition:s:3", "0",
		"--", path)
}

// sidecarRun runs one oneshot pass over root with subtitle_sidecars set to mode and the
// extract hook set, and returns the one row it recorded.
func sidecarRun(t *testing.T, root, mode string, hook func(string)) store.Job {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.SubtitleSidecars = mode })
	eng.hookAfterSidecarExtract = hook
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	row := onlyRow(t, eng.Store.(*testStore))
	if row.Status != store.Done {
		t.Fatalf("status %q (%s), want done: a sidecar must never stop a swap", row.Status, row.Outcome.Reason)
	}
	return row
}

func subtitleKinds(t *testing.T, ffprobe, path string) []string {
	t.Helper()
	var got []string
	for _, s := range probedStreams(t, ffprobe, path) {
		if s.Type == "subtitle" {
			got = append(got, s.Codec+"/"+s.Language)
		}
	}
	return got
}

// SubRip, ASS and WebVTT go to `<name>.<lang>[.forced].<ext>` beside the replacement through
// a real job; a sidecar name already on disk is never overwritten and its skip is recorded;
// the replacement still carries all four embedded streams; no temp is left.
func TestSidecar_EngineWritesSidecarsBesideTheReplacement(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "film.mkv")
	mkSubtitledMatroska(t, ffmpeg, src)
	wantSubs := subtitleKinds(t, ffprobe, src)
	mine := []byte("an operator's own German subtitles\n")
	ger := filepath.Join(root, "film.ger.srt")
	if err := os.WriteFile(ger, mine, 0o644); err != nil {
		t.Fatal(err)
	}

	row := sidecarRun(t, root, config.SubtitleSidecarsText, nil)

	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("the file at the source path is %q, want the hevc replacement", got)
	}
	if got := subtitleKinds(t, ffprobe, src); !equalStrings(got, wantSubs) {
		t.Fatalf("the replacement carries subtitles %v, the source carried %v", got, wantSubs)
	}
	want := []string{".", "film.eng.srt", "film.fre.forced.ass", "film.ger.srt", "film.mkv", "film.und.vtt"}
	if got := dirListing(t, root); !equalStrings(got, want) {
		t.Fatalf("the library holds %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(ger); !bytes.Equal(b, mine) {
		t.Fatalf("an existing sidecar was overwritten: %q", b)
	}
	recs := row.Outcome.SubtitleSidecars.List()
	if !row.Outcome.SubtitleSidecars.Recorded() || len(recs) != 4 {
		t.Fatalf("recorded %+v, want four records", recs)
	}
	for i, w := range []string{"film.eng.srt", "film.fre.forced.ass", "film.und.vtt", ""} {
		r := recs[i]
		got := ""
		if r.Path != "" {
			got = filepath.Base(r.Path)
		}
		if got != w {
			t.Errorf("record %d: %+v, want published as %q", i, r, w)
		}
	}
	if recs[3].Skipped != subtitle.SkipExists {
		t.Errorf("the German record is %+v, want skipped %s", recs[3], subtitle.SkipExists)
	}
	if !recs[1].Forced || recs[0].Forced || recs[1].Events == nil || *recs[1].Events != 3 {
		t.Errorf("records %+v: want the French one forced with 3 events", recs)
	}
}

// THE PARSE-BACK GATE THROUGH A REAL JOB. Every sidecar is cut short after extraction: the
// gate refuses each, nothing is published, no temp is left, the refusal is recorded with its
// counts - and the swap still happens, because a sidecar is a copy of a stream the
// replacement still carries.
func TestSidecar_EngineTruncatedSidecarIsRefusedAndTheSwapStands(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "film.mkv")
	mkSubtitledMatroska(t, ffmpeg, src)
	wantSubs := subtitleKinds(t, ffprobe, src)
	calls := 0
	row := sidecarRun(t, root, config.SubtitleSidecarsText, func(temp string) {
		calls++
		if !isTempName(filepath.Base(temp)) || filepath.Dir(temp) != root {
			t.Errorf("sidecar temp %s is not a temp name in the source's directory", temp)
		}
		if err := os.Truncate(temp, 40); err != nil {
			t.Error(err)
		}
	})
	if calls != 4 {
		t.Fatalf("the extract hook ran %d times, want 4", calls)
	}
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("the swap did not happen: %q at the source path", got)
	}
	if got := subtitleKinds(t, ffprobe, src); !equalStrings(got, wantSubs) {
		t.Fatalf("the replacement carries subtitles %v, the source carried %v", got, wantSubs)
	}
	if got := dirListing(t, root); !equalStrings(got, []string{".", "film.mkv"}) {
		t.Fatalf("the library holds %v, want only the replacement", got)
	}
	for i, r := range row.Outcome.SubtitleSidecars.List() {
		if r.Path != "" || !subtitle.IsFailure(r.Skipped) {
			t.Errorf("record %d: %+v, want a refusal", i, r)
		}
	}
	if r := row.Outcome.SubtitleSidecars.List()[0]; r.Skipped != subtitle.FailEventCount {
		t.Errorf("the SubRip record is %+v, want %s", r, subtitle.FailEventCount)
	}
}

// KEYS UNSET: no sidecar is extracted or written, the row records nothing about sidecars, and
// the replacement is the same job it always was. (The argv goldens in golden_argv_test.go are
// unchanged files: the encode's command line does not read the key at all.)
func TestSidecar_EngineKeyUnsetWritesNothing(t *testing.T) {
	ffmpeg, _ := tools(t)
	for _, mode := range []string{""} { // what baseCfg, and every existing fixture, carries
		t.Run(fmt.Sprintf("mode=%q", mode), func(t *testing.T) {
			root := t.TempDir()
			mkSubtitledMatroska(t, ffmpeg, filepath.Join(root, "film.mkv"))
			row := sidecarRun(t, root, mode, func(string) { t.Error("a sidecar was extracted with the key off") })
			if got := dirListing(t, root); !equalStrings(got, []string{".", "film.mkv"}) {
				t.Fatalf("the library holds %v, want only the replacement", got)
			}
			if row.Outcome.SubtitleSidecars.Recorded() {
				t.Fatalf("the row records sidecars with the key off: %+v", row.Outcome.SubtitleSidecars.List())
			}
		})
	}
}

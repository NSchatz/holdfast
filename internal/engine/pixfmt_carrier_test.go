package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The chroma/bit-depth guard, extended to the encoder: a plan whose pixel format - derived or
// forced - the job's encoder lists no exact carrier for is SKIPPED under exotic-pixel-format
// before anything is encoded, recording both keys the verdict read. Without it, ffmpeg would
// auto-select a lossier format behind -loglevel error: a 4:2:2 plan into libsvtav1 silently
// becomes 4:2:0. libsvtav1 is the encoder these cases run on because it is a real encoder
// this container has whose list (yuv420p yuv420p10le) cannot carry 4:2:2: were the guard
// removed, the plan derivation's backstop would FAIL the job instead of skipping it, and the
// status assertion reds.

func mkPixFmtSource(t *testing.T, ffmpeg, path, pixFmt string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", pixFmt, "--", path)
}

func TestPixelFormatGuard_SkipsAPlanTheEncoderCannotCarry(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, c := range []struct {
		name, source, pixFmt, wantFormat string
	}{
		{"derived 4:2:2", "yuv422p", "auto", "auto"},
		{"derived 4:4:4", "yuv444p", "auto", "auto"},
		{"forced 4:2:2 on a 4:2:0 source", "yuv420p", "yuv422p10le", "yuv422p10le"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkPixFmtSource(t, ffmpeg, src, c.source)
			before := md5f(t, src)
			led := run(t, ffmpeg, ffprobe, d, nil, func(cfg *config.Config) {
				cfg.Encoder, cfg.PixelFormat = "svtav1", c.pixFmt
			})
			if md5f(t, src) != before {
				t.Error("the source was modified")
			}
			if codecOf(t, ffprobe, src) != "h264" {
				t.Error("the source was transcoded")
			}
			out, status, found := outcomeFor(t, led, src)
			if !found || status != store.Skipped || out.Reason != SkipExoticPixelFormat {
				t.Fatalf("row %v %q (found %v), want skipped %s", status, out.Reason, found, SkipExoticPixelFormat)
			}
			in := out.DecisionInputs
			if got := strings.Join(in.Keys(), ","); got != InputEncoder+","+InputPixelFormat {
				t.Errorf("the row recorded inputs %q, want the encoder and the pixel format the verdict read", got)
			}
			if v, _ := in.Value(InputEncoder); v != "svtav1" {
				t.Errorf("recorded encoder %q", v)
			}
			if v, _ := in.Value(InputPixelFormat); v != c.wantFormat {
				t.Errorf("recorded pixel_format %q, want %q", v, c.wantFormat)
			}
			if nTemp(t, d) != 0 {
				t.Error("a temp was left behind")
			}
		})
	}
}

// TestPixelFormatGuard_AnExoticSourceStillRecordsOnlyThePixelFormat: the original branch - a
// source pix_fmt with no derivation at all - is unchanged, and records only the pixel format,
// since no encoder could carry what was never derived.
func TestPixelFormatGuard_AnExoticSourceStillRecordsOnlyThePixelFormat(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "ffv1", "-pix_fmt", "yuv411p", "--", src)
	led := run(t, ffmpeg, ffprobe, d, nil, func(cfg *config.Config) { cfg.Encoder, cfg.PixelFormat = "svtav1", "auto" })
	out, status, found := outcomeFor(t, led, src)
	if !found || status != store.Skipped || out.Reason != SkipExoticPixelFormat {
		t.Fatalf("row %v %q, want skipped %s", status, out.Reason, SkipExoticPixelFormat)
	}
	if got := strings.Join(out.DecisionInputs.Keys(), ","); got != InputPixelFormat {
		t.Errorf("recorded inputs %q, want only %s", got, InputPixelFormat)
	}
}

// TestPixelFormatGuard_CarriesWhatTheEncoderLists: the guard skips only what the encoder
// cannot carry. The same 4:2:2 source under libx265, which lists yuv422p10le, is encoded and
// swapped with its chroma kept; and under a remux-only root, which hands no encoder anything,
// it is not held to the encoder's list at all.
func TestPixelFormatGuard_CarriesWhatTheEncoderLists(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	t.Run("libx265 carries 4:2:2", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkPixFmtSource(t, ffmpeg, src, "yuv422p")
		led := run(t, ffmpeg, ffprobe, d, nil, func(cfg *config.Config) { cfg.Encoder, cfg.PixelFormat = "cpu", "auto" })
		if !ledgerHas(t, led, store.Done, "movie.mkv") {
			t.Fatalf("the 4:2:2 source was not encoded under libx265 (reason %q)", skipReason(t, led, "movie.mkv"))
		}
		if got := probe.New(ffmpeg, ffprobe).PixFmt(context.Background(), src); got != "yuv422p10le" {
			t.Errorf("the replacement is %s, want yuv422p10le", got)
		}
	})
	t.Run("a remux-only root", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkPixFmtSource(t, ffmpeg, src, "yuv422p")
		led := run(t, ffmpeg, ffprobe, d, nil, func(cfg *config.Config) {
			cfg.Encoder, cfg.PixelFormat, cfg.RemuxOnly = "svtav1", "auto", boolPtr(true)
		})
		if out, status, _ := outcomeFor(t, led, src); status == store.Skipped && out.Reason == SkipExoticPixelFormat {
			t.Error("a remux-only job, which encodes nothing, was held to the encoder's pixel formats")
		}
	})
}

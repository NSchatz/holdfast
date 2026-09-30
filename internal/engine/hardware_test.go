package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Every case here is on fakes: a Hardware map stands in for the start-time probes, and where
// an encode runs through a "hardware" encoder, its ffmpeg is a stand-in (hardwareStub) that
// either refuses it or runs it as libx265. No device is ever opened.

func usable(eight, ten bool) encoder.Capability {
	c := encoder.Capability{EightBit: eight, TenBit: ten}
	if !eight || !ten {
		c.Reason = "stand-in: not at every depth"
	}
	return c
}

func TestResolveEncoder_AutoAndFallbackChoosePerJob(t *testing.T) {
	none := Hardware{"nvenc": {Reason: "no CUDA"}, "qsv": {Reason: "no node"}, "vaapi": {Reason: "no node"},
		"amf": {Reason: "refused in the image"}}
	cases := []struct {
		name     string
		hw       Hardware
		enc      string
		fallback string
		planFmt  string
		want     string
		skip     bool
	}{
		{"auto takes the first usable in order", Hardware{"nvenc": usable(true, true), "vaapi": usable(true, true)},
			"auto", "", "yuv420p10le", "nvenc", false},
		{"auto passes over an encoder that cannot do the plan's depth",
			Hardware{"nvenc": usable(true, false), "qsv": usable(true, true)}, "auto", "", "yuv420p10le", "qsv", false},
		{"auto hands an 8-bit plan to the 8-bit-only encoder",
			Hardware{"nvenc": usable(true, false), "qsv": usable(true, true)}, "auto", "", "yuv420p", "nvenc", false},
		{"auto hands a 4:2:2 plan to no hardware, and skips", Hardware{"nvenc": usable(true, true)},
			"auto", "", "yuv422p10le", "", true},
		{"auto hands a 4:2:2 plan to no hardware, and falls back to cpu", Hardware{"nvenc": usable(true, true)},
			"auto", "software", "yuv422p10le", "cpu", false},
		{"auto with nothing usable skips by default", none, "auto", "", "yuv420p10le", "", true},
		{"auto with nothing usable skips under skip", none, "auto", "skip", "yuv420p10le", "", true},
		{"auto with nothing usable falls back to cpu", none, "auto", "software", "yuv420p10le", "cpu", false},
		{"auto with no probe at all finds no hardware", nil, "auto", "software", "yuv420p10le", "cpu", false},
		{"a usable hardware encoder runs as named", Hardware{"vaapi": usable(true, true)}, "vaapi", "", "yuv420p10le", "vaapi", false},
		{"an 8-bit-only encoder is skipped for a 10-bit plan", Hardware{"vaapi": usable(true, false)}, "vaapi", "",
			"yuv420p10le", "", true},
		{"an unusable hevc encoder falls back to cpu", none, "vaapi", "software", "yuv420p10le", "cpu", false},
		{"an unusable av1 encoder falls back to svtav1", Hardware{"av1_nvenc": {Reason: "no CUDA"}}, "av1_nvenc",
			"software", "yuv420p10le", "svtav1", false},
		{"an alias resolves to its key", Hardware{"vaapi": usable(true, true)}, "hevc_vaapi", "", "yuv420p10le", "vaapi", false},
		{"an encoder the run did not probe runs as named", Hardware{}, "nvenc", "", "yuv420p10le", "nvenc", false},
		{"a run that probed nothing runs every encoder as named", nil, "nvenc", "", "yuv420p10le", "nvenc", false},
		{"a software encoder is never replaced", none, "svtav1", "software", "yuv420p10le", "svtav1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{Hardware: c.hw}
			got, v := e.resolveEncoder(config.Profile{HWFallback: c.fallback}, config.Transcode{Encoder: c.enc}, c.planFmt, "h264")
			if v.stopped() != c.skip {
				t.Fatalf("stopped = %v (%+v), want %v", v.stopped(), v, c.skip)
			}
			if c.skip {
				if v.guard != SkipHardwareUnavailable || got != "" || v.codec != "h264" {
					t.Errorf("skip verdict = %q %+v", got, v)
				}
				said := strings.Join(fmtArgs(v.logArgs), " ")
				if !strings.Contains(said, "hw_fallback: software") {
					t.Errorf("the skip does not name its lever: %s", said)
				}
				return
			}
			if got != c.want {
				t.Errorf("resolveEncoder = %q, want %q", got, c.want)
			}
		})
	}
}

func fmtArgs(args []any) []string {
	var out []string
	for _, a := range args {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// The skip is a condition of this host, not a verdict about the file: it is re-derived on
// every pass, so a file skipped for want of hardware is encoded once the fallback allows it
// (or the hardware is there), without a requeue.
func TestHardwareUnavailable_IsAMutableSkip(t *testing.T) {
	for _, g := range mutableGuardSkips {
		if g == SkipHardwareUnavailable {
			return
		}
	}
	t.Fatalf("%s is not in mutableGuardSkips %v", SkipHardwareUnavailable, mutableGuardSkips)
}

// A whole pass, `encoder: auto`, on a host whose probe found no usable hardware: under the
// default the file is skipped hardware-unavailable and left byte-identical, and the next
// pass under hw_fallback: software encodes it with cpu and records cpu as its encoder.
func TestAuto_NoUsableHardwareSkipsThenFallsBackToSoftware(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	none := Hardware{"nvenc": {Reason: "no CUDA"}, "qsv": {Reason: "no node"}, "vaapi": {Reason: "no node"}}

	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.Encoder = encoder.Auto })
	eng.Hardware = none
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := eng.Store.(*testStore)
	o, status, ok := outcomeFor(t, st, src)
	if !ok || status != store.Skipped || o.Reason != SkipHardwareUnavailable {
		t.Fatalf("auto with no hardware under skip: %v %q %+v", ok, status, o)
	}
	if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
		t.Fatal("the source changed under a hardware-unavailable skip")
	}

	// The same library and ledger, the root now falling back to software.
	next := New(func() config.Config {
		c := baseCfg(d)
		c.Encoder, c.HWFallback = encoder.Auto, config.HWFallbackSoftware
		return c
	}(), eng.Probe, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: baseCfg(d), Probe: eng.Probe}, st, discardLogger())
	next.Hardware = none
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, status, ok = outcomeFor(t, st, src)
	if !ok || status != store.Done || o.Encoder != "cpu" {
		t.Fatalf("auto falling back to software: %v %q encoder %q reason %q", ok, status, o.Encoder, o.Reason)
	}
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("the replacement is %q, want hevc", got)
	}
}

// A whole pass, `encoder: auto`, on a host whose probe found VAAPI usable: the job runs
// hevc_vaapi, through the job's own VAAPI command line, and the row records vaapi. The
// "device" is a stand-in ffmpeg that runs that command line as libx265 at the depth its upload
// chose, so every gate judges a real output.
func TestAuto_ChoosesTheUsableHardwareEncoderAndRecordsIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	stub, log := hardwareStub(t, ffmpeg, "hevc_vaapi", "faithful")

	var cfg config.Config
	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.Encoder = encoder.Auto
		cfg = *c
	})
	eng.Enc = FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe)}
	eng.Hardware = Hardware{"nvenc": {Reason: "no CUDA"}, "qsv": {Reason: "no node"}, "vaapi": usable(true, true)}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, status, ok := outcomeFor(t, eng.Store.(*testStore), src)
	if !ok || status != store.Done || o.Encoder != "vaapi" {
		t.Fatalf("auto with VAAPI usable: %v %q encoder %q reason %q", ok, status, o.Encoder, o.Reason)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.Contains(lines[0], "-vaapi_device /dev/dri/renderD128,connection_type=drm") ||
		!strings.Contains(lines[0], "format=p010le,hwupload -profile:v main10") {
		t.Errorf("the job did not run VAAPI's own command line:\n%s", strings.Join(lines, "\n"))
	}
}

// A hardware encode that FAILS at run time: under hw_fallback software the job is encoded
// again with cpu and swapped, recording cpu; under the default it fails as a hardware encode
// always has, and the source is byte-identical.
func TestHardwareEncodeFailure_FallsBackOnlyUnderSoftware(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, c := range []struct {
		fallback string
		status   store.Status
		encoder  string
	}{
		{config.HWFallbackSoftware, store.Done, "cpu"},
		{"", store.Failed, "vaapi"},
	} {
		t.Run("hw_fallback="+c.fallback, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			before, _ := os.ReadFile(src)
			stub, log := hardwareStub(t, ffmpeg, "hevc_vaapi", "refuse")
			var cfg config.Config
			eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(cc *config.Config) {
				cc.Encoder, cc.HWFallback = "vaapi", c.fallback
				cfg = *cc
			})
			eng.Enc = FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe)}
			eng.Hardware = Hardware{"vaapi": usable(true, true)}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatal(err)
			}
			o, status, ok := outcomeFor(t, eng.Store.(*testStore), src)
			if !ok || status != c.status || o.Encoder != c.encoder {
				t.Fatalf("%v %q encoder %q reason %q; want %q %q", ok, status, o.Encoder, o.Reason, c.status, c.encoder)
			}
			if n := len(readLines(t, log)); n != 1 {
				t.Errorf("the hardware encode ran %d times, want 1", n)
			}
			if c.status == store.Failed {
				if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
					t.Error("a failed hardware encode changed the source")
				}
			} else if got := codecOf(t, ffprobe, src); got != "hevc" {
				t.Errorf("the replacement is %q, want hevc", got)
			}
		})
	}
}

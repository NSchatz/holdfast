package engine

// keep_bit_depth (docs/design/encode-plan.md#bit-depth): under `pixel_format: auto` an 8-bit
// source is planned at its own 8 bits rather than floored at 10, and a deeper source keeps the
// depth the floor gives it, so nothing is ever planned shallower than its source. The cases
// here prove it on the plan for every registry encoder, through the gates on real encodes with
// the pinned ffmpeg, and on the record a skipped row keeps.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// keepBitDepthCfg is cfg with pixel_format auto and keep_bit_depth set to keep.
func keepBitDepthCfg(keep bool) func(*config.Config) {
	return func(c *config.Config) {
		c.PixelFormat = "auto"
		c.KeepBitDepth = boolPtr(keep)
	}
}

// keepPlanFor derives src's encode plan under encoder key with keep_bit_depth set to keep, the way
// ProcessFile does, and returns the derivation's refusal rather than failing on it.
func keepPlanFor(t *testing.T, prober *probe.Prober, key, src string, keep bool) (*EncodePlan, error) {
	t.Helper()
	cfg := baseCfg(filepath.Dir(src))
	cfg.Encoder = key
	keepBitDepthCfg(keep)(&cfg)
	prof := cfg.TopLevelProfile()
	ctx := context.Background()
	streams, ok := prober.Streams(ctx, src)
	if !ok {
		t.Fatalf("ffprobe could not enumerate the streams of %s", src)
	}
	props := prober.VideoProps(ctx, src)
	return deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src,
		output:   filepath.Join(t.TempDir(), "film.mkv"),
		streams:  DeriveStreamPlan(streams, prof, props.Codec()),
		snapshot: func() (*probe.VideoProps, error) { return props, nil },
	})
}

// TestKeepBitDepth_ThePlanOnEveryEncoder walks the whole registry. An 8-bit source under the
// key is planned at 8 bits, handed to each encoder in a format it lists at 8 bits (VAAPI
// uploads nv12, so no Main 10 profile), and declared 8-bit to the fidelity gate; without the
// key the plan is today's 10-bit one, or today's refusal for the encoders that carry 8 bits
// only. A 10-bit HDR10 source is a 10-bit plan under the key on every encoder that carries
// 10 bits and a named refusal on every one that does not - never an 8-bit plan.
func TestKeepBitDepth_ThePlanOnEveryEncoder(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	sdr, hdr10 := filepath.Join(d, "sdr.mkv"), filepath.Join(d, "hdr10.mkv")
	mkH264(t, ffmpeg, sdr, "8M")
	mkH264HDR10(t, ffmpeg, hdr10, "8M")
	prober := probe.New(ffmpeg, ffprobe)

	for _, key := range encoder.Known() {
		spec, _ := encoder.Lookup(key)
		t.Run(key, func(t *testing.T) {
			// 8-bit source, the key on: 8 bits all the way.
			kept, err := keepPlanFor(t, prober, key, sdr, true)
			if err != nil {
				t.Fatalf("an 8-bit source under keep_bit_depth was refused: %v", err)
			}
			want8, ok := spec.InputFormat("yuv420p")
			if !ok {
				t.Fatalf("%s lists no 8-bit 4:2:0 format; the keep_bit_depth matrix assumes every encoder does", key)
			}
			if kept.Video.PixelFormat != "yuv420p" || kept.Video.InputFormat != want8 {
				t.Errorf("plan %s handed %s, want yuv420p handed %s", kept.Video.PixelFormat, kept.Video.InputFormat, want8)
			}
			if f := kept.Metadata.Fidelity; f.Depth != 8 || f.Chroma != "420" {
				t.Errorf("the fidelity gate is told %d-bit %s, want 8-bit 420", f.Depth, f.Chroma)
			}
			if layout, _ := hdr.PixelLayout(kept.Video.InputFormat); layout.Depth != 8 {
				t.Errorf("the encoder is handed %s, which is %d-bit", kept.Video.InputFormat, layout.Depth)
			}

			// 8-bit source, the key off: today's derivation, unchanged.
			floored, err := keepPlanFor(t, prober, key, sdr, false)
			if _, carries10 := spec.InputFormat("yuv420p10le"); !carries10 {
				if err == nil {
					t.Errorf("without the key an 8-bit source on %s (8-bit only) was planned at %s; today it is refused",
						key, floored.Video.PixelFormat)
				}
			} else {
				if err != nil {
					t.Fatalf("without the key: %v", err)
				}
				if floored.Video.PixelFormat != "yuv420p10le" || floored.Metadata.Fidelity.Depth != 10 {
					t.Errorf("without the key the plan is %s (fidelity %d-bit), want the 10-bit floor",
						floored.Video.PixelFormat, floored.Metadata.Fidelity.Depth)
				}
			}

			// 10-bit HDR10 source, the key on: kept at 10 bits, or refused by name.
			deep, err := keepPlanFor(t, prober, key, hdr10, true)
			if _, carries10 := spec.InputFormat("yuv420p10le"); carries10 {
				if err != nil {
					t.Fatalf("a 10-bit HDR10 source under keep_bit_depth: %v", err)
				}
				if deep.Video.PixelFormat != "yuv420p10le" || deep.Metadata.Fidelity.Depth != 10 {
					t.Errorf("a 10-bit HDR10 source is planned %s (fidelity %d-bit): keep_bit_depth lowered it",
						deep.Video.PixelFormat, deep.Metadata.Fidelity.Depth)
				}
				if deep.Metadata.Fidelity.Transfer != "smpte2084" {
					t.Errorf("the HDR10 plan declares transfer %q, want smpte2084", deep.Metadata.Fidelity.Transfer)
				}
			} else {
				if err == nil {
					t.Fatalf("a 10-bit source on %s (8-bit only) was planned at %s", key, deep.Video.PixelFormat)
				}
				for _, part := range []string{"cannot carry the pixel format yuv420p10le", spec.FFmpegCodec} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("the refusal %q does not name %q", err, part)
					}
				}
			}
		})
	}
}

// TestKeepBitDepth_TheFidelityGateHoldsTheOutputToTheKeptDepth: the plan's declaration is what
// the output fidelity gate checks, so an encoder that promoted an 8-bit plan to 10 bits behind
// the key's back would be rejected by bit depth, and the 8-bit output it asked for passes.
func TestKeepBitDepth_TheFidelityGateHoldsTheOutputToTheKeptDepth(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	src := filepath.Join(t.TempDir(), "sdr.mkv")
	mkH264(t, ffmpeg, src, "8M")
	plan, err := keepPlanFor(t, probe.New(ffmpeg, ffprobe), "cpu", src, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	f := plan.Metadata.Fidelity
	depthMismatch := func(pixFmt string) bool {
		for _, m := range f.Check(hdr.Observed{PixFmt: pixFmt}) {
			if m.Field == hdr.FieldBitDepth {
				return true
			}
		}
		return false
	}
	if !depthMismatch("yuv420p10le") {
		t.Error("a 10-bit output of an 8-bit keep_bit_depth plan passed the fidelity gate's bit-depth field")
	}
	if depthMismatch("yuv420p") {
		t.Error("the 8-bit output a keep_bit_depth plan asked for failed the fidelity gate's bit-depth field")
	}
}

// streamFacts reads the output video stream's pix_fmt and profile with the pinned ffprobe.
func streamFacts(t *testing.T, ffprobe, path string) (pixFmt, profile string) {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt,profile", "-of", "default=noprint_wrappers=1", "--", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("ffprobe %s: %v: %s", path, err, ee.Stderr)
		}
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(l, "=")
		switch k {
		case "pix_fmt":
			pixFmt = v
		case "profile":
			profile = v
		}
	}
	return pixFmt, profile
}

// TestKeepBitDepth_An8BitSourceIsReplacedAt8Bits is the fixture proof, with the pinned ffmpeg
// and every gate on (VMAF included): an 8-bit H.264 source under keep_bit_depth is replaced by
// an 8-bit output - HEVC Main under libx265 - that passed every gate, and the VMAF gate scored
// it in yuv420p, the depth both sides carry. The control is the same source without the key,
// replaced at 10 bits (Main 10), which is the default this key must not move; a build that
// ignored the key reds the first half, and one that moved the default reds the second.
func TestKeepBitDepth_An8BitSourceIsReplacedAt8Bits(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	mpeg4 := func(t *testing.T, path string) {
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "mpeg4", "-q:v", "1", "-pix_fmt", "yuv420p", "--", path)
	}
	h264 := func(t *testing.T, path string) { mkH264(t, ffmpeg, path, "8M") }
	for _, c := range []struct {
		encoder, codec string
		source         func(*testing.T, string)
		keep           bool
		wantPixFmt     string
		wantProfile    string // "" where the profile name is not part of the claim
	}{
		{"cpu", "hevc", h264, true, "yuv420p", "Main"},
		{"cpu", "hevc", h264, false, "yuv420p10le", "Main 10"},
		{"svtav1", "av1", h264, true, "yuv420p", ""},
		// libx264 writes H.264, so its source is one outside every family it would skip.
		{"x264", "h264", mpeg4, true, "yuv420p", ""},
	} {
		name := c.encoder + "/keep-" + map[bool]string{true: "on", false: "off"}[c.keep]
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			c.source(t, src)
			if got, _ := streamFacts(t, ffprobe, src); got != "yuv420p" {
				t.Fatalf("the fixture is %s, want an 8-bit yuv420p source", got)
			}
			led := run(t, ffmpeg, ffprobe, d, nil, func(cfg *config.Config) {
				cfg.Encoder = c.encoder
				if c.encoder == "svtav1" {
					svtav1Cfg(cfg)
				}
				keepBitDepthCfg(c.keep)(cfg)
				cfg.VmafEnable, cfg.MinVmaf = boolPtr(true), 80
			})
			out, status, found := outcomeFor(t, led, src)
			if !found || status != store.Done {
				t.Fatalf("row %v %q (found %v), want done: the replacement did not pass every gate", status, out.Reason, found)
			}
			if got := codecOf(t, ffprobe, src); got != c.codec {
				t.Fatalf("the file is %s, want the %s replacement", got, c.codec)
			}
			pixFmt, profile := streamFacts(t, ffprobe, src)
			if pixFmt != c.wantPixFmt {
				t.Errorf("the replacement is %s, want %s", pixFmt, c.wantPixFmt)
			}
			if c.wantProfile != "" && profile != c.wantProfile {
				t.Errorf("the replacement's profile is %q, want %q", profile, c.wantProfile)
			}
			if out.VmafPixFmt != c.wantPixFmt {
				t.Errorf("the VMAF gate compared in %q, want %q (the depth both sides carry)", out.VmafPixFmt, c.wantPixFmt)
			}
			if nTemp(t, d) != 0 {
				t.Error("a temp was left behind")
			}
		})
	}
}

// TestKeepBitDepth_A10BitHDRSourceIsNeverLowered: under keep_bit_depth a 10-bit HDR10 source
// is replaced at 10 bits (Main 10) with its mastering display kept, exactly as without the key. The key keeps a
// depth and never lowers one.
func TestKeepBitDepth_A10BitHDRSourceIsNeverLowered(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264HDR10(t, ffmpeg, src, "8M")
	// The side data the plan itself reads (the fidelity declaration's source), read afresh.
	sideData := func() string { return probe.New(ffmpeg, ffprobe).VideoProps(context.Background(), src).SideData() }
	wantMD := hdr.MasterDisplay(sideData())
	if wantMD == "" {
		t.Fatal("the HDR10 fixture carries no mastering display")
	}
	led := run(t, ffmpeg, ffprobe, d, nil, keepBitDepthCfg(true))
	out, status, found := outcomeFor(t, led, src)
	if !found || status != store.Done {
		t.Fatalf("row %v %q (found %v), want done", status, out.Reason, found)
	}
	if pixFmt, profile := streamFacts(t, ffprobe, src); pixFmt != "yuv420p10le" || profile != "Main 10" {
		t.Errorf("the HDR10 replacement is %s %q, want yuv420p10le Main 10", pixFmt, profile)
	}
	if got := hdr.MasterDisplay(sideData()); got != wantMD {
		t.Errorf("the HDR10 replacement's mastering display is %q, want the source's %q", got, wantMD)
	}
}

// TestKeepBitDepth_IsADecisionInputOfEveryJob: keep_bit_depth is offered on every job at its
// resolved value, so the one verdict that records it (the encoder half of the pixel-format
// guard) is re-derived when it changes.
func TestKeepBitDepth_IsADecisionInputOfEveryJob(t *testing.T) {
	cfg := baseCfg(t.TempDir())
	cfg.PixelFormat = "auto"
	for _, keep := range []*bool{nil, boolPtr(false), boolPtr(true)} {
		cfg.KeepBitDepth = keep
		want := "false"
		if keep != nil && *keep {
			want = "true"
		}
		if v, ok := DecisionInputsFor(cfg).Value(InputKeepBitDepth); !ok || v != want {
			t.Errorf("keep_bit_depth %s: offered %q (%v), want %q", pointerText(keep), v, ok, want)
		}
	}
}

// TestGoldenArgv_KeepBitDepthIsTheForced8BitCommandLine reads the golden files and holds
// keep_bit_depth to its two claims on every encoder's command line: set false it builds
// exactly what an unset key builds (the default is byte-identical), and set true on an 8-bit
// 4:2:0 source it builds exactly what `pixel_format: yuv420p` builds - VAAPI uploading nv12
// with no Main 10 profile - while a 10-bit or 12-bit source builds what it builds without the
// key. It needs no ffmpeg: it grades what TestGoldenArgv recorded.
func TestGoldenArgv_KeepBitDepthIsTheForced8BitCommandLine(t *testing.T) {
	read := func(t *testing.T, name string) *goldenBlocks {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(goldenArgvDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b, err := parseGoldenBlocks(data)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return b
	}
	same := func(t *testing.T, file string, b *goldenBlocks, got, want string) {
		t.Helper()
		g, okG := b.lines[got]
		w, okW := b.lines[want]
		if !okG || !okW {
			t.Errorf("%s: case %q (%v) or %q (%v) is missing", file, got, okG, want, okW)
			return
		}
		if strings.Join(g, "\n") != strings.Join(w, "\n") {
			t.Errorf("%s: %q builds\n  %q\nwhich is not what %q builds\n  %q", file, got, g, want, w)
		}
	}
	for _, key := range goldenEncoderKeys() {
		enc := "encoder-" + key + ".txt"
		b := read(t, enc)
		same(t, enc, b, "keep-bit-depth/false", "base")
		same(t, enc, b, "keep-bit-depth", "pixel-format-yuv420p")
		same(t, enc, b, "keep-bit-depth/bitrate-8000k", "pixel-format-yuv420p/bitrate-8000k")
		same(t, enc, b, "keep-bit-depth/hw-decode", "hw-decode/pixel-format-yuv420p")
		for _, deep := range []string{"source-10bit", "source-12bit", "colour-hlg", "colour-pq", "colour-hdr10"} {
			same(t, enc, b, "keep-bit-depth/"+deep, deep)
		}
		if spec, _ := encoder.Lookup(key); spec.API == encoder.APIVAAPI {
			kept := strings.Join(b.lines["keep-bit-depth"], "\n")
			if !strings.Contains(kept, "format=nv12,hwupload") || strings.Contains(kept, "main10") {
				t.Errorf("%s: an 8-bit keep_bit_depth plan does not upload nv12 without Main 10:\n%s", enc, kept)
			}
		}

		eng := "engine-" + key + ".txt"
		e := read(t, eng)
		same(t, eng, e, "keep-bit-depth/false/every-encoder", "base")
		same(t, eng, e, "keep-bit-depth/source-mpeg4/every-encoder", "source-mpeg4/pixel-format-yuv420p/every-encoder")
		same(t, eng, e, "keep-bit-depth/source-ffv1-pq/every-encoder", "source-ffv1-pq/every-encoder")
	}
}

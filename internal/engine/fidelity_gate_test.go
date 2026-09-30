package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// THE OUTPUT FIDELITY GATE, one fixture per field (docs/design/encode-plan.md#fidelity).
//
// Every case encodes the same source - a 4:2:2 10-bit H.264 clip that declares bt2020
// primaries, the PQ transfer, the bt2020nc matrix and the limited range, and carries both
// HDR10 static-metadata blocks - through an encoder that is faithful in every respect but ONE,
// and requires that the job is rejected by the fidelity gate naming exactly that field, with
// the source byte-identical and no temp left behind. The control case runs holdfast's own
// encoder over the same source and requires that it passes the gate, so the fixtures prove the
// gate tells a faithful output from an unfaithful one rather than rejecting everything.
//
// Every lossy output is smaller than the source, as long, carries the same streams and decodes,
// so each case reaches the fidelity gate and no gate in front of it refuses first. The
// perceptual gate is switched off: it runs after this gate, and the control is about fidelity.

// fidelityMasterDisplay and fidelityMaxCLL are the fixture's HDR10 static metadata.
const (
	fidelityMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"
	fidelityMaxCLL        = "1000,400"
)

// mkFidelitySource writes the fixture: an H.264 High 4:2:2 10-bit clip with the colour
// description written into its VUI by the h264_metadata bitstream filter (the pinned libx264
// writes only the matrix of -color_primaries/-color_trc/-colorspace), both HDR10 blocks written
// by libx264 as SEI, and grain so the source is large enough for any honest encode to beat. It is
// written raw and then remuxed into Matroska, so the container's colour elements are read back
// from the stream rather than left unset.
func mkFidelitySource(t *testing.T, ffmpeg, path string) {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "fidelity.h264")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10,noise=alls=12:allf=t",
		"-c:v", "libx264", "-preset", "ultrafast", "-qp", "4",
		"-pix_fmt", "yuv422p10le", "-profile:v", "high422",
		"-x264-params", "mastering-display="+fidelityMasterDisplay+":cll="+fidelityMaxCLL,
		"-bsf:v", "h264_metadata=colour_primaries=9:transfer_characteristics=16:matrix_coefficients=9:video_full_range_flag=0",
		"--", raw)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-r", "10", "-i", raw, "-c", "copy", "--", path)
}

// fidelityLossyEncoder is an encoder faithful in every field but the one its options change:
// libx265 at the fixture's own pixel format and colour tags, with the source's side data
// carried by ffmpeg, and then `change` - options appended after the faithful ones, so each
// overrides the one faithful option it names.
func fidelityLossyEncoder(ffmpeg string, change ...string) func(config.Config, *probe.Prober) Encoder {
	return func(config.Config, *probe.Prober) Encoder { return lossyEncoder(ffmpeg, change...) }
}

func lossyEncoder(ffmpeg string, change ...string) Encoder {
	return EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		args := []string{"-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", "30",
			"-pix_fmt", "yuv422p10le",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv",
			"-x265-params", "log-level=error"}
		args = append(args, change...)
		args = append(args, "-f", "matroska", "--", out)
		return exec.CommandContext(ctx, ffmpeg, args...).Run()
	})
}

// fidelityRun runs one oneshot pass over a fresh copy of the fixture with the encoder mkEnc
// builds from the run's configuration and prober (holdfast's own FFmpegEncoder where mkEnc is
// nil), and returns the source's path, its md5 before the run, the store and the events the
// run emitted.
func fidelityRun(t *testing.T, mkEnc func(config.Config, *probe.Prober) Encoder, mutate func(*config.Config)) (src, before string, st *testStore, events []Event) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src = filepath.Join(root, "movie.mkv")
	mkFidelitySource(t, ffmpeg, src)
	before = md5f(t, src)

	cfg := baseCfg(root)
	cfg.VmafEnable = boolPtr(false)
	// The production default: the plan's pixel format is derived from the source's, so it
	// keeps the source's chroma subsampling and floors its depth at 10. The shared test
	// configuration forces 4:2:0, which is a declared change (see the case below).
	cfg.PixelFormat = "auto"
	if mutate != nil {
		mutate(&cfg)
	}
	st = newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	var enc Encoder = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	if mkEnc != nil {
		enc = mkEnc(cfg, prober)
	}
	eng := New(cfg, prober, enc, st, discardLogger())
	var mu sync.Mutex
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return src, before, st, events
}

// requireFidelityReject asserts that the run rejected the job at the fidelity gate naming
// field (and only field), and that the source is exactly what it was.
func requireFidelityReject(t *testing.T, field hdr.Field, src, before string, st *testStore, events []Event) {
	t.Helper()
	if got := md5f(t, src); got != before {
		t.Fatalf("the source changed (md5 %s -> %s): a rejected encode must leave it byte-identical", before, got)
	}
	if n := nTemp(t, filepath.Dir(src)); n != 0 {
		t.Errorf("%d temp file(s) left behind", n)
	}
	row := rowFor(t, st, src)
	if row.Status != store.Failed {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Failed, row.Outcome.Reason)
	}
	var failed []Event
	for _, ev := range events {
		if ev.Status == store.Failed {
			failed = append(failed, ev)
		}
	}
	if len(failed) != 1 || failed[0].Gate != GateFidelity {
		t.Fatalf("failed events %+v, want exactly one at gate %q", failed, GateFidelity)
	}
	reason := row.Outcome.Reason
	if !strings.Contains(reason, "output fidelity:") || !strings.Contains(reason, string(field)+": want ") {
		t.Fatalf("the reason does not name the field %q: %q", field, reason)
	}
	for _, other := range hdr.Fields {
		if other != field && strings.Contains(reason, string(other)+": want ") {
			t.Errorf("the reason names %q as well as %q: %q", other, field, reason)
		}
	}
	if row.Outcome.FailureClass != store.FailureDeterministic {
		t.Errorf("failure class %q, want %q", row.Outcome.FailureClass, store.FailureDeterministic)
	}
}

// TestFidelityGate_FaithfulEncodePasses is the control: holdfast's own encoder over the
// fixture carries every field, so the gate passes it and the source is replaced.
func TestFidelityGate_FaithfulEncodePasses(t *testing.T) {
	src, _, st, events := fidelityRun(t, nil, nil)
	row := rowFor(t, st, src)
	if row.Status != store.Done {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Done, row.Outcome.Reason)
	}
	for _, ev := range events {
		if ev.Gate == GateFidelity {
			t.Fatalf("the fidelity gate refused a faithful encode: %+v", ev)
		}
	}
	_, ffprobe := tools(t)
	if c := codecOf(t, ffprobe, src); c != "hevc" {
		t.Fatalf("the source was not replaced by the encode (codec %q)", c)
	}
}

// TestFidelityGate_LossyControlPassesWithNothingLost proves the lossy encoder, with no change,
// is itself faithful: every red case below differs from it by one option.
func TestFidelityGate_LossyControlPassesWithNothingLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, _, st, _ := fidelityRun(t, fidelityLossyEncoder(ffmpeg), nil)
	if row := rowFor(t, st, src); row.Status != store.Done {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Done, row.Outcome.Reason)
	}
}

// TestFidelityGate_AForcedPixelFormatIsADeclaredChange: a configured pixel_format is a
// change the plan declares, so an output at that format is faithful to the plan even where it
// differs from the source - here the 4:2:2 source encoded 4:2:0 because the configuration says
// so, by holdfast's own encoder, is accepted and replaces the source.
func TestFidelityGate_AForcedPixelFormatIsADeclaredChange(t *testing.T) {
	src, _, st, _ := fidelityRun(t, nil, func(c *config.Config) { c.PixelFormat = "yuv420p10le" })
	if row := rowFor(t, st, src); row.Status != store.Done {
		t.Fatalf("row status %q, want %q (reason %q)", row.Status, store.Done, row.Outcome.Reason)
	}
	_, ffprobe := tools(t)
	if pf := probe.New("", ffprobe).PixFmt(context.Background(), src); pf != "yuv420p10le" {
		t.Fatalf("the replacement's pixel format is %q, want the configured yuv420p10le", pf)
	}
}

func TestFidelityGate_RedsWhenBitDepthIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-pix_fmt", "yuv422p"), nil)
	requireFidelityReject(t, hdr.FieldBitDepth, src, before, st, events)
}

func TestFidelityGate_RedsWhenChromaSubsamplingIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-pix_fmt", "yuv420p10le"), nil)
	requireFidelityReject(t, hdr.FieldChroma, src, before, st, events)
}

// The pinned ffmpeg takes an encode's primaries and transfer from the decoded frames rather
// than from -color_primaries/-color_trc, so the two cases below mis-tag the bitstream the way
// an encoder writing the wrong VUI does: hevc_metadata rewrites it to bt709 (code 1).
func TestFidelityGate_RedsWhenPrimariesAreLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-bsf:v", "hevc_metadata=colour_primaries=1"), nil)
	requireFidelityReject(t, hdr.FieldPrimaries, src, before, st, events)
}

func TestFidelityGate_RedsWhenTransferIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-bsf:v", "hevc_metadata=transfer_characteristics=1"), nil)
	requireFidelityReject(t, hdr.FieldTransfer, src, before, st, events)
}

func TestFidelityGate_RedsWhenMatrixIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-colorspace", "bt709"), nil)
	requireFidelityReject(t, hdr.FieldMatrix, src, before, st, events)
}

func TestFidelityGate_RedsWhenRangeIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg, "-color_range", "pc"), nil)
	requireFidelityReject(t, hdr.FieldRange, src, before, st, events)
}

// The two HDR10 blocks are removed from the decoded frames by the sidedata filter, which is
// how an encoder that does not carry them behaves: the output simply has none.
func TestFidelityGate_RedsWhenMasteringDisplayIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg,
		"-vf", "sidedata=mode=delete:type=MASTERING_DISPLAY_METADATA"), nil)
	requireFidelityReject(t, hdr.FieldMastering, src, before, st, events)
}

func TestFidelityGate_RedsWhenContentLightLevelIsLost(t *testing.T) {
	ffmpeg, _ := tools(t)
	src, before, st, events := fidelityRun(t, fidelityLossyEncoder(ffmpeg,
		"-vf", "sidedata=mode=delete:type=CONTENT_LIGHT_LEVEL"), nil)
	requireFidelityReject(t, hdr.FieldContentLight, src, before, st, events)
}

// TestFidelityGate_RedsAFakeHardwareEncodeThatWrites8Bit is the hole P2 names on hardware: an
// encoder whose command line asks for 10-bit but whose output is 8-bit (every VAAPI job before
// this goal uploaded nv12). A stand-in for the encoder's ffmpeg, run through holdfast's own
// FFmpegEncoder with a hardware encoder configured, ignores the hardware command line and
// writes an 8-bit HEVC of the same source; the gate rejects it by bit depth and the source is
// kept. No device is opened: the stand-in is a shell script.
func TestFidelityGate_RedsAFakeHardwareEncodeThatWrites8Bit(t *testing.T) {
	ffmpeg, _ := tools(t)
	realFFmpeg, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatalf("look up ffmpeg: %v", err)
	}
	// The stand-in takes the input after -i and the output as the last argument, exactly
	// where FFmpegEncoder puts them, and encodes 8-bit 4:2:2 whatever it was asked.
	script := "#!/bin/sh\n" +
		"in=''; prev=''; for a in \"$@\"; do if [ \"$prev\" = '-i' ]; then in=\"$a\"; fi; prev=\"$a\"; out=\"$a\"; done\n" +
		"exec '" + realFFmpeg + "' -hide_banner -nostdin -v error -y -i \"$in\" -c:v libx265 -preset ultrafast -crf 30 " +
		"-pix_fmt yuv422p -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -color_range tv " +
		"-x265-params log-level=error -f matroska -- \"$out\"\n"
	standIn := filepath.Join(t.TempDir(), "ffmpeg-hw-stand-in")
	if err := os.WriteFile(standIn, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}

	for _, key := range []string{"vaapi", "nvenc"} {
		t.Run(key, func(t *testing.T) {
			standInEncoder := func(cfg config.Config, prober *probe.Prober) Encoder {
				return FFmpegEncoder{FFmpeg: standIn, Cfg: cfg, Probe: prober}
			}
			src, before, st, events := fidelityRun(t, standInEncoder, func(c *config.Config) { c.Encoder = key })
			requireFidelityReject(t, hdr.FieldBitDepth, src, before, st, events)
		})
	}
}

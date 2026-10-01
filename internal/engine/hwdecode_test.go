package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// HARDWARE DECODE, ON FAKES. No device is ever opened (brief T9): a job's hardware command line
// - its -hwaccel, its decode and encode devices, its upload and its hardware encoder - is run
// by a stand-in ffmpeg (hwDecodeStandIn) that logs it, removes the device and decode options,
// and encodes through the job's own filter chain with the software encoder of the same codec.
// What that proves is what a fake can: the per-vendor command line (also pinned in the golden
// argv), and that a hardware-decode job reaches every gate and is held to its plan's fidelity
// declaration - a faithful output replaces the source, and one that lost the bit depth or the
// HDR10 metadata a hardware path could lose is rejected with the source byte-identical.

// hwDecodeStandIn writes the stand-in. mode "faithful" runs the job's own chain; "eight-bit"
// ends the chain in an 8-bit 4:2:0 format, as a download or upload that cut the depth would;
// "drop-mastering" deletes the mastering-display block from every frame, as a download that did
// not copy the frame's side data would. Every other ffmpeg call (the probes, the gates) runs on
// the real binary.
func hwDecodeStandIn(t *testing.T, real, mode string) (stub, log string) {
	t.Helper()
	dir := t.TempDir()
	stub, log = filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "hw.log")
	lossy, pixFmt := "", `set -- "$@" "$a"`
	switch mode {
	case "eight-bit":
		// The chain ends 8-bit, and an explicit -pix_fmt is made 8-bit too, so no later
		// conversion re-expands 8-bit samples into a 10-bit format the depth check would pass.
		lossy = ",format=yuv420p"
		pixFmt = `set -- "$@" yuv420p`
	case "drop-mastering":
		lossy = ",sidedata=mode=delete:type=MASTERING_DISPLAY_METADATA"
	}
	script := `#!/bin/sh
hw=
for a in "$@"; do
  case "$a" in *_nvenc|*_qsv|*_vaapi|*_amf) hw=1 ;; esac
done
[ -z "$hw" ] && exec ` + real + ` "$@"
printf '%s\n' "$*" >> ` + log + `
skip=0; first=1; vf=; pf=
for a in "$@"; do
  if [ $first = 1 ]; then set --; first=0; fi
  if [ $skip = 1 ]; then skip=0; continue; fi
  if [ "$vf" = 1 ]; then vf=; a="${a%,hwupload}` + lossy + `"; set -- "$@" "$a"; continue; fi
  if [ "$pf" = 1 ]; then pf=; ` + pixFmt + `; continue; fi
  case "$a" in
    -pix_fmt) pf=1; set -- "$@" "$a"; continue ;;
    -hwaccel|-hwaccel_device|-init_hw_device|-filter_hw_device|-vaapi_device|-profile:v|-qp|-rc|-cq|-b:v|-preset|-global_quality|-rc_mode|-qp_i|-qp_p) skip=1; continue ;;
    -vf) vf=1; set -- "$@" "$a"; continue ;;
    hevc_nvenc|hevc_qsv|hevc_vaapi|hevc_amf) set -- "$@" libx265 -preset ultrafast -crf 28; continue ;;
    av1_nvenc|av1_qsv|av1_vaapi|av1_amf) set -- "$@" libsvtav1 -preset 12 -crf 40; continue ;;
    h264_nvenc|h264_qsv|h264_vaapi|h264_amf) set -- "$@" libx264 -preset ultrafast -crf 23; continue ;;
  esac
  set -- "$@" "$a"
done
exec ` + real + ` "$@"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, log
}

// mkFFV1HDR10 writes a 10-bit 4:2:0 HDR10 source in FFV1: bt2020/PQ, with the mastering-display
// and content-light blocks at stream and frame level (copied from a libx265 HDR10 encode). FFV1
// is in no codec family, so an H.264 target re-encodes it as every other target does.
func mkFFV1HDR10(t *testing.T, ffmpeg, path string) {
	t.Helper()
	hevc := filepath.Join(t.TempDir(), "hdr10.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10,noise=alls=12:allf=t",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "8", "-pix_fmt", "yuv420p10le",
		"-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:"+
			"master-display="+fidelityMasterDisplay+":max-cll="+fidelityMaxCLL, "--", hevc)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", hevc, "-c:v", "ffv1", "--", path)
}

// hwDecodeRun runs one pass of key under hw_decode: hardware through the stand-in, over the
// source mkSrc writes, and returns what fidelityRunFrom does plus the stand-in's log.
func hwDecodeRun(t *testing.T, key, mode string, mkSrc func(t *testing.T, ffmpeg, path string),
	mutate func(*config.Config)) (src, before string, st *testStore, events []Event, logged []string) {
	t.Helper()
	ffmpeg, _ := tools(t)
	real, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatalf("look up ffmpeg: %v", err)
	}
	stub, log := hwDecodeStandIn(t, real, mode)
	mkEnc := func(cfg config.Config, prober *probe.Prober) Encoder {
		return FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: prober}
	}
	src, before, st, events = fidelityRunFrom(t, mkSrc, mkEnc, func(c *config.Config) {
		c.Encoder = key
		c.HWDecode = config.HWDecodeHardware
		if mutate != nil {
			mutate(c)
		}
	})
	return src, before, st, events, readLines(t, log)
}

// hwDecodeArgvOf is the device and decode options each vendor's pipeline opens with.
var hwDecodeArgvOf = map[string]string{
	encoder.APINVENC: "-hwaccel cuda -i ",
	encoder.APIVAAPI: "-init_hw_device vaapi=hfva:/dev/dri/renderD128,connection_type=drm -filter_hw_device hfva " +
		"-hwaccel vaapi -hwaccel_device hfva -i ",
	encoder.APIQSV: "-init_hw_device vaapi=hfva:/dev/dri/renderD128,connection_type=drm -init_hw_device qsv=hfqsv@hfva " +
		"-hwaccel vaapi -hwaccel_device hfva -i ",
	encoder.APIAMF: "-init_hw_device vaapi=hfva:/dev/dri/renderD128,connection_type=drm -hwaccel vaapi -hwaccel_device hfva -i ",
}

// TestHWDecode_EveryVendorKeeps10BitAndHDR10: for every hardware encoder that carries a 10-bit
// plan, a 10-bit HDR10 source under hw_decode: hardware runs its vendor's decode pipeline -
// -hwaccel and its device before the input, and no -hwaccel_output_format, so the frames come
// back to system memory - and the output passes the fidelity gate at 10 bits with the
// primaries, transfer, mastering display and content light the plan declares, and replaces the
// source. The H.264 encoders read an FFV1 HDR10 source (an H.264 one is already at their
// target); the two that carry 8-bit only (h264_qsv, h264_vaapi) are the next test's.
func TestHWDecode_EveryVendorKeeps10BitAndHDR10(t *testing.T) {
	checked := 0
	for _, key := range encoder.Known() {
		spec, _ := encoder.Lookup(key)
		if !spec.Hardware {
			continue
		}
		if _, ok := spec.InputFormat("yuv420p10le"); !ok {
			continue
		}
		checked++
		t.Run(key, func(t *testing.T) {
			mkSrc := func(t *testing.T, ffmpeg, path string) {
				mkFidelitySourceAs(t, ffmpeg, path, "yuv420p10le", "high10")
			}
			if spec.TargetCodec == "h264" {
				mkSrc = mkFFV1HDR10
			}
			src, before, st, events, logged := hwDecodeRun(t, key, "faithful", mkSrc, nil)
			for _, ev := range events {
				if ev.Gate == GateFidelity || ev.Status == store.Failed {
					t.Fatalf("the %s hardware-decode job was refused: %+v", key, ev)
				}
			}
			row := rowFor(t, st, src)
			if row.Status != store.Done {
				t.Fatalf("row %q %q, want done", row.Status, row.Outcome.Reason)
			}
			if md5f(t, src) == before {
				t.Fatal("the source was not replaced")
			}
			if len(logged) != 1 || !strings.Contains(logged[0], hwDecodeArgvOf[spec.API]) ||
				strings.Contains(logged[0], "-hwaccel_output_format") || !strings.Contains(logged[0], "-c:v "+spec.FFmpegCodec) {
				t.Errorf("the job did not run %s's decode pipeline %q:\n%s", key, hwDecodeArgvOf[spec.API], strings.Join(logged, "\n"))
			}
			// What the gate passed, read back: 10 bits and every HDR10 block.
			ffmpeg, ffprobe := tools(t)
			props := probe.New(ffmpeg, ffprobe).VideoProps(context.Background(), src)
			if layout, ok := hdr.PixelLayout(props.PixFmt()); !ok || layout.Depth != 10 {
				t.Errorf("the replacement is %q, not 10-bit", props.PixFmt())
			}
			if tr := props.Color("color_transfer"); tr != "smpte2084" {
				t.Errorf("the replacement's transfer is %q, want smpte2084", tr)
			}
		})
	}
	if checked < 10 {
		t.Errorf("only %d hardware encoders were checked; every one that carries 10 bits must be", checked)
	}
}

// TestHWDecode_EightBitOnlyEncodersDecodeOnTheirVendor: h264_qsv and h264_vaapi carry 8-bit
// plans only; under hw_decode: hardware with pixel_format yuv420p they decode on their vendor
// and replace the source.
func TestHWDecode_EightBitOnlyEncodersDecodeOnTheirVendor(t *testing.T) {
	for _, key := range []string{"h264_qsv", "h264_vaapi"} {
		t.Run(key, func(t *testing.T) {
			spec, _ := encoder.Lookup(key)
			mpeg4 := func(t *testing.T, ffmpeg, path string) {
				ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
					"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "mpeg4", "-q:v", "2",
					"-pix_fmt", "yuv420p", "--", path)
			}
			src, before, st, _, logged := hwDecodeRun(t, key, "faithful", mpeg4, func(c *config.Config) { c.PixelFormat = "yuv420p" })
			if row := rowFor(t, st, src); row.Status != store.Done || md5f(t, src) == before {
				t.Fatalf("row %q %q, want done and the source replaced", row.Status, row.Outcome.Reason)
			}
			if len(logged) != 1 || !strings.Contains(logged[0], hwDecodeArgvOf[spec.API]) {
				t.Errorf("the job did not run %s's decode pipeline:\n%s", key, strings.Join(logged, "\n"))
			}
		})
	}
}

// TestHWDecode_TheFidelityGateRejectsALossyHardwarePath: a hardware path that cut the depth to
// 8 bits, or dropped the mastering-display block, is rejected by the output fidelity gate
// naming that field, and the source is byte-identical - on the CUDA path and on the VAAPI path,
// for HEVC, AV1 and H.264 targets.
func TestHWDecode_TheFidelityGateRejectsALossyHardwarePath(t *testing.T) {
	src420 := func(t *testing.T, ffmpeg, path string) { mkFidelitySourceAs(t, ffmpeg, path, "yuv420p10le", "high10") }
	for _, c := range []struct {
		key, mode string
		field     hdr.Field
		mkSrc     func(t *testing.T, ffmpeg, path string)
	}{
		{"nvenc", "eight-bit", hdr.FieldBitDepth, src420},
		{"vaapi", "eight-bit", hdr.FieldBitDepth, src420},
		{"qsv", "drop-mastering", hdr.FieldMastering, src420},
		{"av1_vaapi", "drop-mastering", hdr.FieldMastering, src420},
		{"h264_nvenc", "drop-mastering", hdr.FieldMastering, mkFFV1HDR10},
		{"h264_amf", "eight-bit", hdr.FieldBitDepth, mkFFV1HDR10},
	} {
		t.Run(c.key+"/"+c.mode, func(t *testing.T) {
			src, before, st, events, logged := hwDecodeRun(t, c.key, c.mode, c.mkSrc, nil)
			if len(logged) != 1 || !strings.Contains(logged[0], "-hwaccel ") {
				t.Fatalf("the job did not run a hardware decode:\n%s", strings.Join(logged, "\n"))
			}
			requireFidelityReject(t, c.field, src, before, st, events)
		})
	}
}

// TestHWDecode_DeclaredPerVendorAndRefusedWhenForged: the plan declares the decode path from the
// root's hw_decode and the encoder's API - CUDA for NVENC, VAAPI on the encoder's own node for
// VAAPI and QSV and on the VAAPI node for AMF, software for a software encoder or under the
// default - and the command-line builder refuses a plan whose decode was not that derivation's.
func TestHWDecode_DeclaredPerVendorAndRefusedWhenForged(t *testing.T) {
	devices := hwdevice.Assignment{VAAPI: "/dev/dri/renderD129", QSV: "/dev/dri/renderD130"}
	hw := config.Profile{HWDecode: config.HWDecodeHardware}
	for _, c := range []struct {
		key          string
		prof         config.Profile
		decode, node string
	}{
		{"nvenc", hw, DecodeCUDA, ""},
		{"h264_nvenc", hw, DecodeCUDA, ""},
		{"av1_nvenc", hw, DecodeCUDA, ""},
		{"vaapi", hw, DecodeVAAPI, "/dev/dri/renderD129"},
		{"av1_vaapi", hw, DecodeVAAPI, "/dev/dri/renderD129"},
		{"qsv", hw, DecodeVAAPI, "/dev/dri/renderD130"},
		{"h264_qsv", hw, DecodeVAAPI, "/dev/dri/renderD130"},
		{"amf", hw, DecodeVAAPI, "/dev/dri/renderD129"},
		{"av1_amf", hw, DecodeVAAPI, "/dev/dri/renderD129"},
		{"cpu", hw, DecodeSoftware, ""},
		{"svtav1", hw, DecodeSoftware, ""},
		{"x264", hw, DecodeSoftware, ""},
		{"nvenc", config.Profile{}, DecodeSoftware, ""},
		{"vaapi", config.Profile{HWDecode: config.HWDecodeSoftware}, DecodeSoftware, ""},
	} {
		spec, _ := encoder.Lookup(c.key)
		if d, n := decodeFor(spec, c.prof, devices); d != c.decode || n != c.node {
			t.Errorf("decodeFor(%s, %q) = %q %q, want %q %q", c.key, c.prof.HWDecode, d, n, c.decode, c.node)
		}
	}
	// AMF with no VAAPI node assigned decodes on the first one.
	if _, n := decodeFor(mustLookup("amf"), hw, hwdevice.Assignment{}); n != defaultRenderNode {
		t.Errorf("amf decodes on %q with no assignment, want %q", n, defaultRenderNode)
	}

	// A plan's decode that is not what its derivation says is refused before any argv.
	ffmpeg, ffprobe := tools(t)
	src := filepath.Join(t.TempDir(), "movie.mkv")
	mkFidelitySourceAs(t, ffmpeg, src, "yuv420p10le", "high10")
	prober := probe.New(ffmpeg, ffprobe)
	for _, forge := range []func(p *EncodePlan){
		func(p *EncodePlan) { p.Video.Decode = DecodeCUDA },
		func(p *EncodePlan) { p.Video.DecodeDevice = "/dev/dri/renderD200" },
		func(p *EncodePlan) { p.Video.Decode = "d3d11va" },
	} {
		p, err := deriveEncodePlan(planInputs{
			settings: config.Transcode{Encoder: "vaapi", CRF: 22, Preset: "slow", PixelFormat: "auto"},
			prof:     hw, source: src, output: filepath.Join(t.TempDir(), "out.mkv"),
			snapshot: func() (*probe.VideoProps, error) { return prober.VideoProps(context.Background(), src), nil },
		})
		if err != nil {
			t.Fatalf("derive: %v", err)
		}
		if _, _, err := p.args(encoder.X265Parallelism{}); err != nil {
			t.Fatalf("the derived plan does not build: %v", err)
		}
		forge(p)
		var unbuildable *UnbuildablePlanError
		if _, _, err := p.args(encoder.X265Parallelism{}); err == nil || !asUnbuildable(err, &unbuildable) ||
			!strings.Contains(err.Error(), "decode path") {
			t.Errorf("a forged decode built: %v", err)
		}
	}
}

func asUnbuildable(err error, target **UnbuildablePlanError) bool {
	u, ok := err.(*UnbuildablePlanError)
	if ok {
		*target = u
	}
	return ok
}

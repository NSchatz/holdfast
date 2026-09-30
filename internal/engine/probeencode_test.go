package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/probe"
)

// A probe's encode is the job's own: every hardware encoder's probe command line comes out of
// the plan derivation and the command-line builder a job goes through (ProbeEncode), so it
// carries the device a job opens, with connection_type=drm, the explicit pixel format or
// upload, the Main 10 profile for 10-bit VAAPI and the encoder's own quality option - none of
// which the generic `-c:v <codec>` probe this replaced had, which is why its VAAPI probe could
// never pass on a working host (verify-hw-encode.md claim 7b).
//
// No device is opened: the ffmpeg the probe runs is a stub that records any command line
// naming the hardware codec and exits 1, and hands every other one (the probe clip's
// generation, ffprobe's work) to the real binary.
func TestProbeEncode_AvailableProbesThroughTheJobsOwnCommandLine(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	devices := hwdevice.Assignment{VAAPI: "/dev/dri/renderD129", QSV: "/dev/dri/renderD130"}
	cases := []struct {
		key           string
		eight, ten    []string
		neverOnEither []string
		neverOnEight  []string
	}{
		{key: "vaapi",
			eight: []string{"-vaapi_device /dev/dri/renderD129,connection_type=drm -i ", "-c:v hevc_vaapi",
				"format=nv12,hwupload", "-qp 22"},
			ten: []string{"-vaapi_device /dev/dri/renderD129,connection_type=drm -i ", "-c:v hevc_vaapi",
				"format=p010le,hwupload -profile:v main10", "-qp 22"},
			neverOnEither: []string{"-pix_fmt"},
			neverOnEight:  []string{"main10"}},
		{key: "qsv",
			eight: []string{"-init_hw_device vaapi=hfva:/dev/dri/renderD130,connection_type=drm " +
				"-init_hw_device qsv=hfqsv@hfva -i ", "-c:v hevc_qsv -pix_fmt nv12", "-global_quality 22"},
			ten: []string{"-init_hw_device vaapi=hfva:/dev/dri/renderD130,connection_type=drm " +
				"-init_hw_device qsv=hfqsv@hfva -i ", "-c:v hevc_qsv -pix_fmt p010le", "-global_quality 22"},
			neverOnEither: []string{"hwupload", "-vaapi_device"}},
		{key: "nvenc",
			eight:         []string{"-c:v hevc_nvenc -pix_fmt yuv420p ", "-rc vbr -cq 22"},
			ten:           []string{"-c:v hevc_nvenc -pix_fmt p010le ", "-rc vbr -cq 22"},
			neverOnEither: []string{"-init_hw_device", "-vaapi_device", "hwupload"}},
		{key: "av1_nvenc",
			eight:         []string{"-c:v av1_nvenc -pix_fmt yuv420p ", "-rc vbr -cq 22"},
			ten:           []string{"-c:v av1_nvenc -pix_fmt p010le ", "-rc vbr -cq 22"},
			neverOnEither: []string{"-init_hw_device", "-vaapi_device", "hwupload"}},
		{key: "amf",
			eight:         []string{"-c:v hevc_amf -pix_fmt yuv420p ", "-rc cqp -qp_i 22 -qp_p 22"},
			ten:           []string{"-c:v hevc_amf -pix_fmt p010le ", "-rc cqp -qp_i 22 -qp_p 22"},
			neverOnEither: []string{"-init_hw_device", "-vaapi_device", "hwupload"}},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			spec, _ := encoder.Lookup(c.key)
			stub, log := hardwareStub(t, ffmpeg, spec.FFmpegCodec, "refuse")
			cfg := baseCfg(t.TempDir())
			got := encoder.Available(context.Background(), stub, ffprobe, spec,
				ProbeEncode(cfg, stub, probe.New(stub, ffprobe), devices))
			if got.Usable() {
				t.Fatalf("Available through a refusing stub = %+v", got)
			}
			lines := readLines(t, log)
			if len(lines) != 2 {
				t.Fatalf("the probe ran %d hardware command lines, want 2 (8 and 10 bits):\n%s", len(lines), strings.Join(lines, "\n"))
			}
			check := func(depth, line string, want, never []string) {
				for _, w := range want {
					if !strings.Contains(line, w) {
						t.Errorf("%s-bit probe argv does not carry %q:\n%s", depth, w, line)
					}
				}
				for _, n := range append(never, c.neverOnEither...) {
					if strings.Contains(line, n) {
						t.Errorf("%s-bit probe argv carries %q:\n%s", depth, n, line)
					}
				}
			}
			check("8", lines[0], c.eight, c.neverOnEight)
			check("10", lines[1], c.ten, nil)
		})
	}
}

// The probe that runs through the job's command line sees what the job would produce: a VAAPI
// stand-in that honours the upload format writes 10-bit for the 10-bit probe and is usable at
// both depths, and the same stand-in with the 8-bit upload this build used to hand every VAAPI
// job (`format=nv12,hwupload`, verify-hw-encode.md claim 7a) fails the 10-bit probe by name.
func TestProbeEncode_A10BitProbeSeesTheDepthTheUploadCarries(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := encoder.Lookup("vaapi")
	cfg := baseCfg(t.TempDir())

	faithful, _ := hardwareStub(t, ffmpeg, spec.FFmpegCodec, "faithful")
	got := encoder.Available(context.Background(), faithful, ffprobe, spec,
		ProbeEncode(cfg, faithful, probe.New(faithful, ffprobe), hwdevice.Assignment{}))
	if !got.EightBit || !got.TenBit {
		t.Fatalf("a VAAPI stand-in honouring its upload = %+v, want usable at 8 and 10 bits", got)
	}

	cuts, _ := hardwareStub(t, ffmpeg, spec.FFmpegCodec, "eight-bit-upload")
	got = encoder.Available(context.Background(), cuts, ffprobe, spec,
		ProbeEncode(cfg, cuts, probe.New(cuts, ffprobe), hwdevice.Assignment{}))
	if !got.EightBit || got.TenBit {
		t.Fatalf("a VAAPI stand-in uploading 8-bit surfaces = %+v, want 8-bit only", got)
	}
	if !strings.Contains(got.Reason, "10-bit") || !strings.Contains(got.Reason, "not 10-bit") {
		t.Errorf("Reason %q does not say the 10-bit probe came out at another depth", got.Reason)
	}
}

// The software encoders probe through the same path and pass at both depths on the pinned
// ffmpeg - the control that the path itself is sound.
func TestProbeEncode_SoftwareEncodersPassAtBothDepths(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cfg := baseCfg(t.TempDir())
	for _, key := range []string{"cpu", "svtav1"} {
		spec, _ := encoder.Lookup(key)
		got := encoder.Available(context.Background(), ffmpeg, ffprobe, spec,
			ProbeEncode(cfg, ffmpeg, probe.New(ffmpeg, ffprobe), hwdevice.Assignment{}))
		if !got.EightBit || !got.TenBit || got.Reason != "" {
			t.Errorf("Available(%s) through the job's command line = %+v", key, got)
		}
	}
}

// hardwareStub writes an ffmpeg stand-in. A command line naming codec is appended to the
// returned log and then, by mode: "refuse" exits 1 without writing anything (no device);
// "faithful" runs it on the real binary as a libx265 encode, dropping the VAAPI device and
// profile options and keeping the upload's format (so the frames reach the encoder at the
// depth the command line chose); "eight-bit-upload" does the same but uploads nv12 for a
// 10-bit plan. Every other command line runs on the real binary unchanged.
func hardwareStub(t *testing.T, real, codec, mode string) (stub, log string) {
	t.Helper()
	dir := t.TempDir()
	stub, log = filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "hardware.log")
	tenBit := "a=${a%,hwupload}"
	if mode == "eight-bit-upload" {
		tenBit = "a=$(printf '%s' \"$a\" | sed 's/format=p010le,hwupload$/format=nv12/')"
	}
	script := `#!/bin/sh
hw=
for a in "$@"; do [ "$a" = ` + codec + ` ] && hw=1; done
[ -z "$hw" ] && exec ` + real + ` "$@"
printf '%s\n' "$*" >> ` + log + `
[ ` + mode + ` = refuse ] && exit 1
skip=0; first=1
for a in "$@"; do
  if [ $first = 1 ]; then set --; first=0; fi
  if [ $skip = 1 ]; then skip=0; continue; fi
  case "$a" in
    -vaapi_device|-profile:v|-qp) skip=1; continue ;;
    ` + codec + `) a=libx265 ;;
    *format=p010le,hwupload) ` + tenBit + ` ;;
    *format=nv12,hwupload) a=${a%,hwupload} ;;
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

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// Each encoder that opens a render node opens the one this host assigned it, and the first
// node where none was assigned; no other encoder is told a node. A plan carrying another
// node than its assignment's is refused before its command line is assembled.
func TestDeviceFor_TheAssignedNodeOrTheFirst(t *testing.T) {
	assigned := hwdevice.Assignment{VAAPI: "/dev/dri/renderD129", QSV: "/dev/dri/renderD130"}
	for _, c := range []struct {
		key      string
		devices  hwdevice.Assignment
		want     string
		wantArgs string
	}{
		{"vaapi", assigned, "/dev/dri/renderD129", "-vaapi_device /dev/dri/renderD129,connection_type=drm"},
		{"qsv", assigned, "/dev/dri/renderD130",
			"-init_hw_device vaapi=hfva:/dev/dri/renderD130,connection_type=drm -init_hw_device qsv=hfqsv@hfva"},
		{"vaapi", hwdevice.Assignment{}, "/dev/dri/renderD128", "-vaapi_device /dev/dri/renderD128,connection_type=drm"},
		{"qsv", hwdevice.Assignment{}, "/dev/dri/renderD128",
			"-init_hw_device vaapi=hfva:/dev/dri/renderD128,connection_type=drm -init_hw_device qsv=hfqsv@hfva"},
		{"nvenc", assigned, "", ""}, {"av1_nvenc", assigned, "", ""}, {"amf", assigned, "", ""},
		{"cpu", assigned, "", ""}, {"svtav1", assigned, "", ""},
	} {
		spec, _ := encoder.Lookup(c.key)
		got := deviceFor(spec, c.devices)
		if got != c.want {
			t.Errorf("deviceFor(%s, %+v) = %q, want %q", c.key, c.devices, got, c.want)
		}
		if args := strings.Join(deviceArgs(VideoPlan{Encoder: spec, Device: got}), " "); args != c.wantArgs {
			t.Errorf("deviceArgs(%s) = %q, want %q", c.key, args, c.wantArgs)
		}
	}
	spec, _ := encoder.Lookup("vaapi")
	p := &EncodePlan{id: 1, Audio: CopyStreams, Subtitles: CopyStreams, devices: assigned,
		Video: VideoPlan{Encoder: spec, Device: "/dev/dri/renderD128", Decode: DecodeSoftware,
			PixelFormat: "yuv420p10le", InputFormat: "p010le"}}
	var unbuildable *UnbuildablePlanError
	if err := p.buildable(); !errors.As(err, &unbuildable) || !strings.Contains(err.Error(), "renderD128") {
		t.Errorf("a plan on another node than its assignment's: buildable() = %v", err)
	}
	p.Video.Device = "/dev/dri/renderD129"
	if err := p.buildable(); err != nil {
		t.Errorf("a plan on its assigned node: buildable() = %v", err)
	}
}

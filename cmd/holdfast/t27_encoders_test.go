package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// t27Encoders are the eight encoders the owner decided to add (T27), by the name the
// configuration may write them under: libx264 by its codec name (its registry key is x264),
// the rest by their registry keys, which are their codec names.
var t27Encoders = []string{"libx264", "h264_nvenc", "h264_qsv", "h264_vaapi", "h264_amf", "av1_qsv", "av1_vaapi", "av1_amf"}

// TestValidate_AcceptsAConfigNamingEachT27Encoder: `holdfast validate` accepts a configuration
// whose encoder is each of the eight, printing it as the root's encoder, and one that names
// every one of them at once - the top level, one encode profile per other encoder, and each
// hardware encoder's own quality key at an edge of its scale. validate probes nothing, so no
// device is ever opened.
func TestValidate_AcceptsAConfigNamingEachT27Encoder(t *testing.T) {
	for _, name := range append([]string{"x264"}, t27Encoders...) {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, "encoder: "+name+"\n")}, &out, &errOut); code != 0 {
				t.Fatalf("validate refused encoder: %s (exit %d): %s", name, code, errOut.String())
			}
			if !strings.Contains(out.String(), name) {
				t.Errorf("validate does not print the encoder %s:\n%s", name, out.String())
			}
		})
	}

	t.Run("all eight in one configuration", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("encoder: libx264\nencode_profiles:\n")
		for _, name := range t27Encoders[1:] {
			b.WriteString("  - name: " + name + "-files\n    match: \"**/" + name + "/**\"\n    encoder: " + name + "\n")
		}
		b.WriteString("quality:\n  h264_nvenc: 28\n  h264_qsv: 29\n  h264_vaapi: 51\n  h264_amf: 0\n" +
			"  av1_qsv: 33\n  av1_vaapi: 255\n  av1_amf: 255\n")
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, b.String())}, &out, &errOut); code != 0 {
			t.Fatalf("validate refused a configuration naming all eight (exit %d): %s\n%s", code, errOut.String(), b.String())
		}
		if !strings.HasPrefix(strings.ToLower(out.String()), "config ok") || !strings.Contains(out.String(), "encoder              libx264") {
			t.Errorf("validate did not report the configuration OK with libx264 as its encoder:\n%s", out.String())
		}
	})

	// The other half of acceptance: a value off an encoder's own scale is refused naming the
	// key and the scale, so an accepted configuration is one whose values mean something.
	for _, c := range []struct{ entry, want string }{
		{"h264_vaapi: 52", "quality.h264_vaapi 52 is outside h264_vaapi's scale (-qp 1-51)"},
		{"av1_vaapi: 0", "quality.av1_vaapi 0 is outside av1_vaapi's scale (-global_quality 1-255)"},
		{"av1_amf: 256", "quality.av1_amf 256 is outside av1_amf's scale (-qp_i/-qp_p 0-255)"},
		{"libx264: 20", "is not a key: libx264's quality is set by crf"},
	} {
		t.Run("refused "+c.entry, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", emptyLibraryConfig(t, "quality:\n  "+c.entry+"\n")}, &out, &errOut); code == 0 {
				t.Fatalf("validate accepted quality.%s:\n%s", c.entry, out.String())
			}
			if !strings.Contains(errOut.String(), c.want) {
				t.Errorf("the refusal does not say %q:\n%s", c.want, errOut.String())
			}
		})
	}
}

// TestPreflight_T27HardwareEncodersProbeTheirOwnCommandLine: each new hardware encoder is
// probed at start through its own command line - its codec, its vendor's device options and
// its quality shape - on a stand-in that refuses every hardware codec, so no device is ever
// opened; the refusal names the encoder, and the VAAPI and QSV ones name why they have no
// render node, exactly as their HEVC siblings do.
func TestPreflight_T27HardwareEncodersProbeTheirOwnCommandLine(t *testing.T) {
	requireWorkingEncoder(t)
	_, noDRI := os.Stat("/dev/dri")
	for _, c := range []struct {
		key  string
		argv []string // each must appear in the probe's command line
		node bool     // the encoder needs a render node
	}{
		{"h264_nvenc", []string{"-c:v h264_nvenc", "-rc vbr -cq 22 -b:v 0 -preset p5"}, false},
		{"h264_qsv", []string{"-init_hw_device vaapi=hfva:/dev/dri/renderD128,connection_type=drm " +
			"-init_hw_device qsv=hfqsv@hfva", "-c:v h264_qsv", "-pix_fmt nv12", "-global_quality 22"}, true},
		{"h264_vaapi", []string{"-vaapi_device /dev/dri/renderD128,connection_type=drm", "-c:v h264_vaapi",
			"format=nv12,hwupload", "-qp 22"}, true},
		{"h264_amf", []string{"-c:v h264_amf", "-rc cqp -qp_i 22 -qp_p 22"}, false},
		{"av1_qsv", []string{"-init_hw_device qsv=hfqsv@hfva", "-c:v av1_qsv", "-pix_fmt p010le", "-global_quality 22"}, true},
		{"av1_vaapi", []string{"-vaapi_device /dev/dri/renderD128,connection_type=drm", "-c:v av1_vaapi",
			"format=p010le,hwupload", "-rc_mode CQP -global_quality 22"}, true},
		{"av1_amf", []string{"-c:v av1_amf", "-pix_fmt p010le", "-rc cqp -qp_i 22 -qp_p 22"}, false},
	} {
		t.Run(c.key, func(t *testing.T) {
			stub, log := hardwareRefusingFFmpeg(t)
			cfgPath, _ := hardwareConfig(t, "encoder: "+c.key+"\n")
			code, said := dispatchWith(t, stub, "run", "--config", cfgPath)
			if code == 0 {
				t.Fatalf("run started with an unavailable %s:\n%s", c.key, said)
			}
			want := []string{`encoder "` + c.key + `"`, "not available"}
			if c.node && noDRI != nil {
				want = append(want, "render node: no render node under /dev/dri")
			}
			for _, w := range want {
				if !strings.Contains(said, w) {
					t.Errorf("the refusal does not carry %q:\n%s", w, said)
				}
			}
			b, _ := os.ReadFile(log)
			for _, a := range c.argv {
				if !strings.Contains(string(b), a) {
					t.Errorf("the %s probe's command line does not carry %q:\n%s", c.key, a, b)
				}
			}
		})
	}
}

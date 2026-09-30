package encoder

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// encoderHelp is `ffmpeg -h encoder=<name>` from the pinned binary the gate runs. It never
// skips: a proof about the binary's own lists that did not read the binary is a false green.
func encoderHelp(t *testing.T, ffmpeg, name string) string {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-h", "encoder="+name).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg -h encoder=%s: %v\n%s", name, err, out)
	}
	return string(out)
}

var supportedPixFmtsRe = regexp.MustCompile(`(?m)^\s*Supported pixel formats:\s*(.*)$`)

// TestPixelFormats_MatchThePinnedBinary: every registry encoder's PixelFormats is the pinned
// binary's "Supported pixel formats:" line, exactly. InputFormat chooses from this copy, so a
// copy that drifted from the binary would choose a format the encoder does not take - or miss
// one it does. A bump of the pinned ffmpeg that changes a list reds here until the table is
// re-verified.
func TestPixelFormats_MatchThePinnedBinary(t *testing.T) {
	ffmpeg, _ := tools(t)
	for _, key := range Known() {
		spec, _ := Lookup(key)
		m := supportedPixFmtsRe.FindStringSubmatch(encoderHelp(t, ffmpeg, spec.FFmpegCodec))
		if m == nil {
			t.Errorf("%s: the pinned binary prints no \"Supported pixel formats:\" line", spec.FFmpegCodec)
			continue
		}
		if got := strings.Join(strings.Fields(m[1]), " "); got != spec.PixelFormats {
			t.Errorf("%s: the pinned binary lists\n  %q\nthe registry records\n  %q", spec.FFmpegCodec, got, spec.PixelFormats)
		}
	}
}

// planFormats are every plan pixel format a derivation or a forced pixel_format can name that
// this build reasons about: each chroma at each depth, planar and semi-planar, the full-range
// spelling, a big-endian one, and names outside the family.
var planFormats = []string{
	"yuv420p", "yuv420p10le", "yuv420p12le", "yuv422p", "yuv422p10le", "yuv422p12le",
	"yuv444p", "yuv444p10le", "yuv444p12le", "yuvj420p", "yuvj422p", "yuvj444p",
	"nv12", "p010le", "p012le", "nv16", "p210le", "p212le", "nv24", "p410le", "p412le",
	"yuv420p10be", "yuv420p16le", "yuv411p", "gray", "rgb24", "",
}

// TestInputFormat_IsInTheEncodersListAndCarriesThePlanExactly is the table's proof, against
// the pinned binary: every format InputFormat maps a plan TO is in that encoder's "Supported
// pixel formats" line as the binary prints it (for VAAPI, whose line is only `vaapi`, the
// format is the software format uploaded, one of UploadFormats), and it carries the plan's
// chroma subsampling and bit depth exactly.
func TestInputFormat_IsInTheEncodersListAndCarriesThePlanExactly(t *testing.T) {
	ffmpeg, _ := tools(t)
	for _, key := range Known() {
		spec, _ := Lookup(key)
		m := supportedPixFmtsRe.FindStringSubmatch(encoderHelp(t, ffmpeg, spec.FFmpegCodec))
		if m == nil {
			t.Fatalf("%s: the pinned binary prints no \"Supported pixel formats:\" line", spec.FFmpegCodec)
		}
		binaryList := strings.Join(strings.Fields(m[1]), " ")
		carried := 0
		for _, plan := range planFormats {
			got, ok := spec.InputFormat(plan)
			if !ok {
				if got != "" {
					t.Errorf("%s: %q refused but named %q", key, plan, got)
				}
				continue
			}
			carried++
			if spec.Uploads() {
				if binaryList != "vaapi" {
					t.Errorf("%s uploads, but the binary lists %q rather than only vaapi surfaces", key, binaryList)
				}
				if !listed(spec.UploadFormats, got) {
					t.Errorf("%s: %q -> %q, which is not an upload format (%q)", key, plan, got, spec.UploadFormats)
				}
			} else if !listed(binaryList, got) {
				t.Errorf("%s: %q -> %q, which the pinned binary does not list (%q)", key, plan, got, binaryList)
			}
			if listed(binaryList, plan) || (spec.Uploads() && listed(spec.UploadFormats, plan)) {
				if got != plan {
					t.Errorf("%s: %q is listed, so it is handed as itself, not as %q", key, plan, got)
				}
				continue
			}
			want, okW := parseLayout(plan)
			have, okH := parseLayout(got)
			if !okW || !okH || want.Chroma != have.Chroma || want.Depth != have.Depth {
				t.Errorf("%s: %q -> %q does not carry the plan's chroma and depth (%+v, %+v)", key, plan, got, want, have)
			}
		}
		if carried == 0 {
			t.Errorf("%s carries none of the plan formats: the table is vacuous", key)
		}
	}
}

// TestInputFormat_Table pins the answer for the plans that matter, per encoder, so a change to
// the choice order is a named failure rather than a quiet reshuffle.
func TestInputFormat_Table(t *testing.T) {
	cases := []struct {
		key, plan, want string // want "" = refused
	}{
		// libx265 lists every planar YUV plan, so it is handed each as itself: its command
		// line does not move.
		{"cpu", "yuv420p10le", "yuv420p10le"},
		{"cpu", "yuv422p10le", "yuv422p10le"},
		{"cpu", "yuv444p12le", "yuv444p12le"},
		{"cpu", "yuvj420p", "yuvj420p"},
		{"cpu", "gray", "gray"},
		{"cpu", "nv12", "yuv420p"},
		{"cpu", "p010le", "yuv420p10le"},
		{"cpu", "yuv420p10be", "yuv420p10le"},
		{"cpu", "yuv411p", ""},
		{"cpu", "", ""},
		// libsvtav1 lists 4:2:0 at 8 and 10 bits only.
		{"svtav1", "yuv420p10le", "yuv420p10le"},
		{"svtav1", "yuv420p", "yuv420p"},
		{"svtav1", "nv12", "yuv420p"},
		{"svtav1", "yuv420p12le", ""},
		{"svtav1", "yuv422p10le", ""},
		{"svtav1", "yuv444p10le", ""},
		// NVENC lists yuv420p and yuv444p planar, and every semi-planar 4:2:0, 4:2:2 and
		// 4:4:4 at 8, 10 and 12 bits.
		{"nvenc", "yuv420p10le", "p010le"},
		{"nvenc", "yuv420p", "yuv420p"},
		{"nvenc", "yuv420p12le", "p012le"},
		{"nvenc", "yuv422p", "nv16"},
		{"nvenc", "yuv422p10le", "p210le"},
		{"nvenc", "yuv422p12le", "p212le"},
		{"nvenc", "yuv444p", "yuv444p"},
		{"nvenc", "yuv444p10le", "p410le"},
		{"nvenc", "yuv444p12le", "p412le"},
		{"nvenc", "yuvj420p", "yuv420p"},
		{"nvenc", "yuv420p10be", "p010le"},
		{"av1_nvenc", "yuv420p10le", "p010le"},
		{"av1_nvenc", "yuv422p10le", "p210le"},
		// QSV lists 4:2:0 semi-planar at 8, 10 and 12 bits and no planar or semi-planar 4:2:2
		// or 4:4:4.
		{"qsv", "yuv420p10le", "p010le"},
		{"qsv", "yuv420p", "nv12"},
		{"qsv", "yuv420p12le", "p012le"},
		{"qsv", "yuv422p10le", ""},
		{"qsv", "yuv444p10le", ""},
		// AMF lists yuv420p, nv12 and p010le.
		{"amf", "yuv420p10le", "p010le"},
		{"amf", "yuv420p", "yuv420p"},
		{"amf", "nv12", "nv12"},
		{"amf", "yuv420p12le", ""},
		{"amf", "yuv422p10le", ""},
		// VAAPI uploads nv12 or p010le, and nothing else.
		{"vaapi", "yuv420p10le", "p010le"},
		{"vaapi", "yuv420p", "nv12"},
		{"vaapi", "nv12", "nv12"},
		{"vaapi", "p010le", "p010le"},
		{"vaapi", "yuv420p12le", ""},
		{"vaapi", "yuv422p10le", ""},
		{"vaapi", "yuv444p", ""},
		{"vaapi", "vaapi", ""},
		// libx264 lists planar 4:2:0, 4:2:2 and 4:4:4 at 8 and 10 bits, no 12-bit.
		{"x264", "yuv420p10le", "yuv420p10le"},
		{"x264", "yuv420p", "yuv420p"},
		{"x264", "yuv422p10le", "yuv422p10le"},
		{"x264", "yuv444p", "yuv444p"},
		{"x264", "p010le", "yuv420p10le"},
		{"x264", "yuv420p12le", ""},
		// h264_nvenc lists what hevc_nvenc does; whether the device encodes 10-bit H.264 is
		// the probe's to say, not the list's.
		{"h264_nvenc", "yuv420p10le", "p010le"},
		{"h264_nvenc", "yuv420p", "yuv420p"},
		// h264_qsv lists nv12 only: no 10-bit, no 4:2:2 or 4:4:4.
		{"h264_qsv", "yuv420p", "nv12"},
		{"h264_qsv", "yuv420p10le", ""},
		{"h264_qsv", "yuv422p", ""},
		// av1_qsv lists nv12 and p010le.
		{"av1_qsv", "yuv420p10le", "p010le"},
		{"av1_qsv", "yuv420p", "nv12"},
		{"av1_qsv", "yuv420p12le", ""},
		// h264_vaapi uploads nv12 only; av1_vaapi uploads what vaapi does.
		{"h264_vaapi", "yuv420p", "nv12"},
		{"h264_vaapi", "yuv420p10le", ""},
		{"av1_vaapi", "yuv420p10le", "p010le"},
		{"av1_vaapi", "yuv420p", "nv12"},
		{"av1_vaapi", "yuv422p10le", ""},
		// h264_amf and av1_amf list what amf does.
		{"h264_amf", "yuv420p10le", "p010le"},
		{"av1_amf", "yuv420p10le", "p010le"},
		{"av1_amf", "yuv422p10le", ""},
	}
	for _, c := range cases {
		spec, _ := Lookup(c.key)
		got, ok := spec.InputFormat(c.plan)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("%s.InputFormat(%q) = %q, %v; want %q", c.key, c.plan, got, ok, c.want)
		}
	}
}

// TestUploads_OnlyVAAPI: the VAAPI encoders are the ones that take only hardware surfaces,
// so they are the ones whose format is uploaded rather than named by -pix_fmt.
func TestUploads_OnlyVAAPI(t *testing.T) {
	for _, key := range Known() {
		spec, _ := Lookup(key)
		if spec.Uploads() != (spec.API == APIVAAPI) {
			t.Errorf("%s.Uploads() = %v", key, spec.Uploads())
		}
	}
}

// optionRangeRe reads one option's line from `ffmpeg -h encoder=`: its name, then "(from A to
// B)".
func optionRange(t *testing.T, help, option string) (lo, hi int) {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(option) + `\s+<\w+>.*\(from (-?\d+) to (-?\d+)\)`)
	m := re.FindStringSubmatch(help)
	if m == nil {
		t.Fatalf("the pinned binary prints no range for %s", option)
	}
	lo, _ = strconv.Atoi(m[1])
	hi, _ = strconv.Atoi(m[2])
	return lo, hi
}

// TestQualityScales_MatchThePinnedBinary: every hardware scale lies inside the range the
// pinned binary gives its option, its top IS that range's top, and its bottom is the
// binary's bottom raised only past the values the encoder reads as "not a target" (NVENC's
// 0 "automatic", VAAPI's 0 "unset", AMF's -1 default). QSV's -global_quality is a generic
// option with no per-encoder range in the binary; its 1-51 is doc/encoders.texi's.
func TestQualityScales_MatchThePinnedBinary(t *testing.T) {
	ffmpeg, _ := tools(t)
	for _, c := range []struct {
		key, option string
		binaryLo    int // the binary's own bottom, which the scale raises by exactly one
	}{
		{"nvenc", "-cq", 0},
		{"av1_nvenc", "-cq", 0},
		{"vaapi", "-qp", 0},
		{"amf", "-qp_i", -1},
		{"amf", "-qp_p", -1},
		{"h264_nvenc", "-cq", 0},
		{"h264_amf", "-qp_i", -1},
		{"h264_amf", "-qp_p", -1},
		{"av1_amf", "-qp_i", -1},
		{"av1_amf", "-qp_p", -1},
	} {
		spec, _ := Lookup(c.key)
		lo, hi := optionRange(t, encoderHelp(t, ffmpeg, spec.FFmpegCodec), c.option)
		if lo != c.binaryLo {
			t.Errorf("%s %s: the binary's range starts at %d, the table expected %d", c.key, c.option, lo, c.binaryLo)
		}
		if spec.Quality.Max != hi {
			t.Errorf("%s: scale top %d, the binary's %s tops out at %d", c.key, spec.Quality.Max, c.option, hi)
		}
		if spec.Quality.Min != lo+1 {
			t.Errorf("%s: scale bottom %d, want the binary's %d raised past its not-a-target value", c.key, spec.Quality.Min, lo)
		}
	}
	if q := registry["qsv"].Quality; q.Min != 1 || q.Max != 51 {
		t.Errorf("qsv scale %v, want 1-51 (doc/encoders.texi:3739-3740)", q)
	}
	// h264_vaapi's -qp is 0-52 on the binary, and the encoder clips it to 1-51
	// (vaapi_encode_h264.c:885), so the scale stops one below the binary's top.
	h264VAAPI := registry["h264_vaapi"]
	if lo, hi := optionRange(t, encoderHelp(t, ffmpeg, "h264_vaapi"), "-qp"); lo != 0 || hi != 52 ||
		h264VAAPI.Quality.Min != 1 || h264VAAPI.Quality.Max != 51 {
		t.Errorf("h264_vaapi: binary -qp %d-%d, scale %v; want 0-52 and 1-51", lo, hi, h264VAAPI.Quality)
	}
	// av1_vaapi has no -qp: its target is -global_quality under -rc_mode CQP, a generic option
	// the binary prints no range for, and the encoder reads it as the AV1 q_idx, 0-255
	// (vaapi_encode_av1.c:34, 140); 0 means "not set". The binary must still offer CQP.
	av1Help := encoderHelp(t, ffmpeg, "av1_vaapi")
	if regexp.MustCompile(`(?m)^\s+-qp\s`).MatchString(av1Help) {
		t.Error("av1_vaapi now prints a -qp option: re-read its scale")
	}
	if !regexp.MustCompile(`(?m)^\s+CQP\s+1\s`).MatchString(av1Help) {
		t.Error("av1_vaapi's -rc_mode no longer offers CQP")
	}
	if q := registry["av1_vaapi"].Quality; q.Min != 1 || q.Max != 255 || q.Option != "-global_quality" {
		t.Errorf("av1_vaapi scale %v, want -global_quality 1-255", q)
	}
	for _, key := range []string{"h264_qsv", "av1_qsv"} {
		if q := registry[key].Quality; q.Min != 1 || q.Max != 51 || q.Option != "-global_quality" {
			t.Errorf("%s scale %v, want -global_quality 1-51 (qsvenc.c:957 clips every QSV codec's ICQ there)", key, q)
		}
	}
}

// TestQualityScales_EveryEncoder pins each encoder's key, option and range.
func TestQualityScales_EveryEncoder(t *testing.T) {
	want := map[string]QualityScale{
		"cpu":        {ConfigKey: "crf", Option: "-crf", Min: 0, Max: 51},
		"svtav1":     {ConfigKey: "crf", Option: "-crf", Min: 0, Max: 51},
		"nvenc":      {ConfigKey: "quality.nvenc", Option: "-cq", Min: 1, Max: 51},
		"av1_nvenc":  {ConfigKey: "quality.av1_nvenc", Option: "-cq", Min: 1, Max: 63},
		"qsv":        {ConfigKey: "quality.qsv", Option: "-global_quality", Min: 1, Max: 51},
		"vaapi":      {ConfigKey: "quality.vaapi", Option: "-qp", Min: 1, Max: 52},
		"amf":        {ConfigKey: "quality.amf", Option: "-qp_i/-qp_p", Min: 0, Max: 51},
		"x264":       {ConfigKey: "crf", Option: "-crf", Min: 0, Max: 51},
		"h264_nvenc": {ConfigKey: "quality.h264_nvenc", Option: "-cq", Min: 1, Max: 51},
		"h264_qsv":   {ConfigKey: "quality.h264_qsv", Option: "-global_quality", Min: 1, Max: 51},
		"h264_vaapi": {ConfigKey: "quality.h264_vaapi", Option: "-qp", Min: 1, Max: 51},
		"h264_amf":   {ConfigKey: "quality.h264_amf", Option: "-qp_i/-qp_p", Min: 0, Max: 51},
		"av1_qsv":    {ConfigKey: "quality.av1_qsv", Option: "-global_quality", Min: 1, Max: 51},
		"av1_vaapi":  {ConfigKey: "quality.av1_vaapi", Option: "-global_quality", Min: 1, Max: 255},
		"av1_amf":    {ConfigKey: "quality.av1_amf", Option: "-qp_i/-qp_p", Min: 0, Max: 255},
	}
	if len(want) != len(Known()) {
		t.Fatalf("the registry ships %v; this table covers %d", Known(), len(want))
	}
	for key, w := range want {
		spec, ok := Lookup(key)
		if !ok || spec.Quality != w {
			t.Errorf("%s scale = %+v, want %+v", key, spec.Quality, w)
		}
	}
	if got := strings.Join(QualityKeys(), " "); got != "amf av1_amf av1_nvenc av1_qsv av1_vaapi h264_amf h264_nvenc "+
		"h264_qsv h264_vaapi nvenc qsv vaapi" {
		t.Errorf("QualityKeys() = %q", got)
	}
}

// TestValidateQualityKey_RedsAtEachScalesEdges: each scale's two edges are accepted and the
// value one past each is refused naming the key and the scale; a software encoder's key, an
// unknown key and an ffmpeg alias are refused naming why.
func TestValidateQualityKey_RedsAtEachScalesEdges(t *testing.T) {
	for _, key := range QualityKeys() {
		q := registry[key].Quality
		for _, v := range []int{q.Min, q.Max} {
			if err := ValidateQualityKey(key, v); err != nil {
				t.Errorf("%s %d is on the scale %s and was refused: %v", key, v, q, err)
			}
		}
		for _, v := range []int{q.Min - 1, q.Max + 1} {
			err := ValidateQualityKey(key, v)
			if err == nil {
				t.Errorf("%s %d is off the scale %s and was accepted", key, v, q)
				continue
			}
			for _, part := range []string{"quality." + key, strconv.Itoa(v), q.String()} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s %d: refusal %q does not name %q", key, v, err, part)
				}
			}
		}
	}
	// The named edges of the proposal: 0 for nvenc, 64 for av1_nvenc, 0 and 53 for vaapi.
	for _, c := range []struct {
		key string
		v   int
	}{{"nvenc", 0}, {"av1_nvenc", 64}, {"vaapi", 0}, {"vaapi", 53}, {"qsv", 0}, {"qsv", 52}, {"amf", -1}, {"amf", 52},
		{"h264_nvenc", 0}, {"h264_nvenc", 52}, {"h264_qsv", 52}, {"h264_vaapi", 52}, {"h264_amf", 52},
		{"av1_qsv", 52}, {"av1_vaapi", 0}, {"av1_vaapi", 256}, {"av1_amf", -1}, {"av1_amf", 256}} {
		if ValidateQualityKey(c.key, c.v) == nil {
			t.Errorf("quality.%s %d accepted", c.key, c.v)
		}
	}
	for _, c := range []struct{ key, want string }{
		{"cpu", "set by crf"},
		{"svtav1", "set by crf"},
		{"libx265", "set by crf"},
		{"x264", "set by crf"},
		{"libx264", "set by crf"},
		{"hevc_nvenc", "write quality.nvenc"},
		{"nvidia", "names no encoder"},
		{"", "names no encoder"},
	} {
		err := ValidateQualityKey(c.key, 22)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateQualityKey(%q) = %v, want a refusal containing %q", c.key, err, c.want)
		}
	}
}

// TestQualityScale_Contains marks the boundaries exactly.
func TestQualityScale_Contains(t *testing.T) {
	q := QualityScale{Min: 1, Max: 51}
	for v, want := range map[int]bool{0: false, 1: true, 51: true, 52: false} {
		if q.Contains(v) != want {
			t.Errorf("Contains(%d) = %v", v, !want)
		}
	}
	if (QualityScale{ConfigKey: CRFKey}).PerEncoder() || !(QualityScale{ConfigKey: "quality.x"}).PerEncoder() {
		t.Error("PerEncoder mis-reports")
	}
}

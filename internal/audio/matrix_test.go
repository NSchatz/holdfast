package audio

import (
	"math"
	"strings"
	"testing"
)

func TestAudioMatrix_CodecsAndTheirEncoders(t *testing.T) {
	want := map[string]string{"aac": "aac", "ac3": "ac3", "eac3": "eac3", "opus": "libopus"}
	for c, enc := range want {
		if !KnownCodec(c) || Encoder(c) != enc {
			t.Errorf("%s: known=%v encoder=%q, want known and %q", c, KnownCodec(c), Encoder(c), enc)
		}
		if OutputCodecName(c) != c {
			t.Errorf("%s reads back as %q", c, OutputCodecName(c))
		}
	}
	for _, c := range []string{"", "libfdk_aac", "libopus", "mp3", "AAC", "flac", "truehd"} {
		if KnownCodec(c) || Encoder(c) != "" {
			t.Errorf("%q is a codec a re-encode may target", c)
		}
	}
	if strings.Join(Codecs, ",") != "aac,ac3,eac3,opus" {
		t.Errorf("Codecs = %v", Codecs)
	}
}

func TestAudioMatrix_FamiliesOfTheLayoutsInTheMatrix(t *testing.T) {
	for layout, want := range map[string]Family{"mono": Mono, "stereo": Stereo, "5.1": Surround51,
		"5.1(side)": Surround51, " 7.1 ": Surround71} {
		f, ok := FamilyOf(layout)
		if !ok || f != want {
			t.Errorf("FamilyOf(%q) = %v, %v; want %v", layout, f, ok, want)
		}
	}
	for _, layout := range []string{"", "7.1(wide)", "6.1", "quad", "6 channels", "2.1", "5.0", "downmix"} {
		if f, ok := FamilyOf(layout); ok {
			t.Errorf("FamilyOf(%q) = %v, in the matrix", layout, f)
		}
	}
	names := map[Family]string{Mono: "mono", Stereo: "stereo", Surround51: "5.1", Surround71: "7.1", Family(3): "unknown"}
	for f, n := range names {
		if f.String() != n {
			t.Errorf("Family(%d) = %q, want %q", int(f), f.String(), n)
		}
	}
}

// The matrix, cell by cell: what each codec writes each family as, and that AC-3 and E-AC-3
// carry no 7.1.
func TestAudioMatrix_EveryCell(t *testing.T) {
	want := map[string][4]string{
		"aac":  {"mono", "stereo", "5.1", "7.1"},
		"opus": {"mono", "stereo", "5.1", "7.1"},
		"ac3":  {"mono", "stereo", "5.1(side)", ""},
		"eac3": {"mono", "stereo", "5.1(side)", ""},
		"mp3":  {"", "", "", ""},
	}
	for codec, row := range want {
		for i, f := range Families {
			got, ok := OutputLayout(codec, f)
			if got != row[i] || ok != (row[i] != "") {
				t.Errorf("OutputLayout(%s, %v) = %q, %v; want %q", codec, f, got, ok, row[i])
			}
		}
	}
	if l, ok := OutputLayout("aac", Family(3)); ok {
		t.Errorf("a family outside the matrix gets %q", l)
	}
}

func TestAudioMatrix_SampleRateIsTheSourcesWhereTheCodecTakesIt(t *testing.T) {
	cases := []struct {
		codec     string
		src, want int
	}{
		{"aac", 44100, 44100}, {"aac", 96000, 96000}, {"aac", 7350, 7350}, {"aac", 192000, 48000},
		{"ac3", 44100, 44100}, {"ac3", 32000, 32000}, {"ac3", 96000, 48000}, {"eac3", 88200, 48000},
		{"opus", 44100, 48000}, {"opus", 24000, 24000}, {"opus", 48000, 48000}, {"opus", 8000, 8000},
		{"mp3", 44100, 48000},
	}
	for _, c := range cases {
		if got := SampleRateFor(c.codec, c.src); got != c.want {
			t.Errorf("SampleRateFor(%s, %d) = %d, want %d", c.codec, c.src, got, c.want)
		}
	}
}

func TestAudioMatrix_AC3BitratesAreTheTableAnd640IsTheCeiling(t *testing.T) {
	if len(AC3Bitrates) != 19 || AC3Bitrates[0] != 32 || AC3Bitrates[18] != AC3MaxKbps || AC3MaxKbps != 640 {
		t.Fatalf("AC3Bitrates = %v", AC3Bitrates)
	}
	for _, b := range AC3Bitrates {
		if !ValidAC3Bitrate(b) {
			t.Errorf("%d refused", b)
		}
	}
	for _, b := range []int{0, 31, 33, 100, 641, 768, 1024} {
		if ValidAC3Bitrate(b) {
			t.Errorf("%d accepted", b)
		}
	}
}

func TestAudioMatrix_DefaultAndMaximumBitrates(t *testing.T) {
	want := map[string][4]int{
		"aac": {64, 128, 384, 512}, "ac3": {96, 192, 640, 0}, "eac3": {96, 192, 640, 0}, "opus": {64, 128, 256, 450},
	}
	for codec, row := range want {
		for i, f := range Families {
			if got := DefaultBitrateKbps(codec, f); got != row[i] {
				t.Errorf("DefaultBitrateKbps(%s, %v) = %d, want %d", codec, f, got, row[i])
			}
			if row[i] > 0 && row[i] > MaxBitrateKbps(codec, int(f), 48000) {
				t.Errorf("the default %s %v (%d) is beyond the codec's maximum", codec, f, row[i])
			}
		}
	}
	for _, c := range []struct {
		codec          string
		channels, rate int
		want           int
	}{
		{"aac", 2, 48000, 576}, {"aac", 1, 44100, 264}, {"aac", 6, 32000, 1152},
		{"ac3", 6, 48000, 640}, {"ac3", 1, 32000, 640},
		{"eac3", 6, 48000, 6144}, {"eac3", 2, 32000, 4096},
		{"opus", 2, 48000, 512}, {"opus", 8, 48000, 2048}, {"mp3", 2, 48000, 0},
	} {
		if got := MaxBitrateKbps(c.codec, c.channels, c.rate); got != c.want {
			t.Errorf("MaxBitrateKbps(%s, %d, %d) = %d, want %d", c.codec, c.channels, c.rate, got, c.want)
		}
	}
}

func TestAudioMatrix_OnlyLosslessTracksQualify(t *testing.T) {
	yes := [][2]string{{"truehd", ""}, {"flac", ""}, {"pcm_s16le", ""}, {"pcm_s24be", ""}, {"pcm_f32le", ""},
		{"dts", "DTS-HD MA"}, {"dts", " DTS-HD MA + DTS:X"}, {"dts", "DTS-HD MA + DTS:X IMAX"}}
	no := [][2]string{{"dts", "DTS"}, {"dts", "DTS-HD HRA"}, {"dts", "DTS-ES"}, {"dts", ""}, {"ac3", ""},
		{"eac3", ""}, {"aac", ""}, {"opus", ""}, {"mp3", ""}, {"pcm", ""}, {"alac", ""}, {"truehd ", ""}}
	for _, c := range yes {
		if !Lossless(c[0], c[1]) {
			t.Errorf("Lossless(%q, %q) = false", c[0], c[1])
		}
	}
	for _, c := range no {
		if Lossless(c[0], c[1]) {
			t.Errorf("Lossless(%q, %q) = true", c[0], c[1])
		}
	}
}

func TestAudioMatrix_ContainersAnAudioTransformIsWrittenInto(t *testing.T) {
	for ext, want := range map[string]bool{"mkv": true, "MKV": true, "mp4": true, "mov": false, "ts": false,
		"avi": false, "m4v": false, "webm": false, "": false} {
		if SupportedContainer(ext) != want {
			t.Errorf("SupportedContainer(%q) = %v", ext, !want)
		}
	}
}

func TestAudioGate_DurationToleranceIsTwoFramesOfTheOutputCodec(t *testing.T) {
	for _, c := range []struct {
		codec string
		rate  int
		want  float64
	}{
		{"aac", 48000, 2048.0 / 48000}, {"aac", 44100, 2048.0 / 44100}, {"ac3", 48000, 0.064},
		{"eac3", 32000, 0.096}, {"opus", 48000, 0.040}, {"opus", 24000, 0.040}, {"mp3", 48000, 0}, {"aac", 0, 0},
	} {
		if got := DurationTolerance(c.codec, c.rate); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("DurationTolerance(%s, %d) = %v, want %v", c.codec, c.rate, got, c.want)
		}
	}
	o := Op{Codec: "ac3", SampleRate: 48000, Output: 1, Source: Source{Index: 2}, Action: ActionReencoded}
	for _, c := range []struct {
		src, out float64
		ok       bool
	}{
		{10, 10, true}, {10, 10.063, true}, {10, 9.937, true}, {10, 10.065, false}, {10, 9.9, false},
		{10, 5, false}, {math.NaN(), 10, false}, {10, math.NaN(), false},
	} {
		err := CheckDuration(o, c.src, c.out)
		if (err == nil) != c.ok {
			t.Errorf("CheckDuration(%v, %v) = %v, want ok=%v", c.src, c.out, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "a:1") {
			t.Errorf("the refusal does not name the output track: %v", err)
		}
	}
}

func TestAudioGate_LayoutAndSampleRateEqualThePlan(t *testing.T) {
	o := Op{Codec: "eac3", Layout: "5.1(side)", Channels: 6, SampleRate: 48000, Output: 0,
		Source: Source{Index: 1}, Action: ActionReencoded}
	good := Observed{Codec: "eac3", Channels: 6, Layout: "5.1(side)", SampleRate: 48000}
	if err := CheckLayout(o, good); err != nil {
		t.Errorf("a faithful track refused: %v", err)
	}
	if err := CheckSampleRate(o, good); err != nil {
		t.Errorf("a faithful rate refused: %v", err)
	}
	for name, c := range map[string]struct {
		mut  func(*Observed)
		want string
	}{
		"codec":    {func(g *Observed) { g.Codec = "ac3" }, `codec "ac3"`},
		"channels": {func(g *Observed) { g.Channels = 8 }, "8 channels"},
		"layout":   {func(g *Observed) { g.Layout = "5.1" }, `channel layout "5.1"`},
	} {
		g := good
		c.mut(&g)
		err := CheckLayout(o, g)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: CheckLayout = %v, want naming %q", name, err, c.want)
		}
	}
	bad := good
	bad.SampleRate = 192000
	if err := CheckSampleRate(o, bad); err == nil || !strings.Contains(err.Error(), "192000 Hz") {
		t.Errorf("CheckSampleRate = %v", err)
	}
}

func TestAudioGate_LoudnessWithinOneLUOfTheTarget(t *testing.T) {
	if TargetLUFS != -23 || TargetTruePeak != -1 || ToleranceLU != 1 || LoudnessRangeLU != 20 {
		t.Fatalf("targets moved: %v %v %v %v", TargetLUFS, TargetTruePeak, ToleranceLU, LoudnessRangeLU)
	}
	for v, ok := range map[float64]bool{-23: true, -22: true, -24: true, -21.99: false, -24.01: false,
		-16: false, -40: false, math.Inf(-1): false, math.NaN(): false} {
		if err := CheckLoudness(v); (err == nil) != ok {
			t.Errorf("CheckLoudness(%v) = %v, want ok=%v", v, err, ok)
		}
	}
}

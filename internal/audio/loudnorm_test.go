package audio

import (
	"math"
	"strings"
	"testing"
)

const report = `{
	"input_i" : "-32.25",
	"input_tp" : "-28.52",
	"input_lra" : "2.30",
	"input_thresh" : "-42.25",
	"output_i" : "-22.95",
	"output_tp" : "-19.27",
	"output_lra" : "2.10",
	"output_thresh" : "-32.95",
	"normalization_type" : "linear",
	"target_offset" : "-0.05"
}`

func TestAudioLoudnorm_ParseStatsReadsEveryFigureAndTheMode(t *testing.T) {
	s, err := ParseStats([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{InputI: -32.25, InputTP: -28.52, InputLRA: 2.3, InputThresh: -42.25, OutputI: -22.95,
		TargetOffset: -0.05, Mode: ModeLinear}
	if s != want {
		t.Errorf("ParseStats = %+v, want %+v", s, want)
	}
	if Mode([]byte(report)) != ModeLinear {
		t.Errorf("Mode = %q", Mode([]byte(report)))
	}
	dyn := strings.Replace(report, `"linear"`, `"dynamic"`, 1)
	if Mode([]byte(dyn)) != ModeDynamic {
		t.Errorf("Mode(dynamic) = %q", Mode([]byte(dyn)))
	}
	for name, bad := range map[string]string{
		"not json":       "loudnorm",
		"empty":          "",
		"no mode":        strings.Replace(report, `"linear"`, `""`, 1),
		"an unknown one": strings.Replace(report, `"linear"`, `"Linear"`, 1),
		"no input_i":     strings.Replace(report, `"input_i" : "-32.25",`, "", 1),
		"no input_tp":    strings.Replace(report, `"-28.52"`, `"x"`, 1),
		"no input_lra":   strings.Replace(report, `"2.30"`, `""`, 1),
		"no thresh":      strings.Replace(report, `"-42.25"`, `"?"`, 1),
		"no output_i":    strings.Replace(report, `"-22.95"`, `"n/a"`, 1),
		"no offset":      strings.Replace(report, `"-0.05"`, `"-"`, 1),
	} {
		if _, err := ParseStats([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if Mode([]byte(bad)) != ModeNotRecorded {
			t.Errorf("%s: Mode = %q, want not recorded", name, Mode([]byte(bad)))
		}
	}
	inf, err := ParseStats([]byte(strings.Replace(report, `"-32.25"`, `"-inf"`, 1)))
	if err != nil || !math.IsInf(inf.InputI, -1) || inf.Usable() {
		t.Errorf("silence: %+v, %v", inf, err)
	}
}

func TestAudioLoudnorm_UsableIsLoudnormsOwnOptionRanges(t *testing.T) {
	ok := Stats{InputI: -30, InputTP: -5, InputLRA: 3, InputThresh: -40, TargetOffset: 0.2}
	if !ok.Usable() {
		t.Fatal("ordinary figures refused")
	}
	edges := []Stats{
		{InputI: 0, InputTP: 99, InputLRA: 99, InputThresh: 0, TargetOffset: 99},
		{InputI: -99, InputTP: -99, InputLRA: 0, InputThresh: -99, TargetOffset: -99},
	}
	for _, s := range edges {
		if !s.Usable() {
			t.Errorf("%+v refused", s)
		}
	}
	for name, mut := range map[string]func(*Stats){
		"I high": func(s *Stats) { s.InputI = 0.01 }, "I low": func(s *Stats) { s.InputI = -99.01 },
		"TP high": func(s *Stats) { s.InputTP = 99.01 }, "TP low": func(s *Stats) { s.InputTP = -99.01 },
		"LRA high": func(s *Stats) { s.InputLRA = 99.01 }, "LRA low": func(s *Stats) { s.InputLRA = -0.01 },
		"thresh high": func(s *Stats) { s.InputThresh = 0.01 }, "thresh low": func(s *Stats) { s.InputThresh = -99.01 },
		"offset high": func(s *Stats) { s.TargetOffset = 99.01 }, "offset low": func(s *Stats) { s.TargetOffset = -99.01 },
		"NaN": func(s *Stats) { s.InputLRA = math.NaN() }, "inf": func(s *Stats) { s.InputTP = math.Inf(1) },
	} {
		s := ok
		mut(&s)
		if s.Usable() {
			t.Errorf("%s: usable", name)
		}
	}
}

func TestAudioLoudnorm_FiltersAreR128AndEndInAresample(t *testing.T) {
	if got := MeasureFilter("/proc/self/fd/3"); got !=
		"loudnorm=I=-23.00:TP=-1.00:LRA=20.00:print_format=json:stats_file=/proc/self/fd/3" {
		t.Errorf("MeasureFilter = %s", got)
	}
	got := NormalizeFilter(Stats{InputI: -30.456, InputTP: -2, InputLRA: 7.1, InputThresh: -40.5, TargetOffset: -0.1},
		"/proc/self/fd/5", 44100)
	want := "loudnorm=I=-23.00:TP=-1.00:LRA=20.00:measured_I=-30.46:measured_TP=-2.00:measured_LRA=7.10:" +
		"measured_thresh=-40.50:offset=-0.10:linear=true:print_format=json:stats_file=/proc/self/fd/5,aresample=44100"
	if got != want {
		t.Errorf("NormalizeFilter =\n%s\nwant\n%s", got, want)
	}
	if DownmixFilter != "aformat=channel_layouts=stereo" {
		t.Errorf("DownmixFilter = %s", DownmixFilter)
	}
}

func TestAudioMeasure_LastDurationIsTheFinalProgressFigure(t *testing.T) {
	for in, want := range map[string]float64{
		"out_time_us=100\nprogress=continue\nout_time_us=3300000\nprogress=end\n": 3.3,
		"out_time_us=0\n": 0,
	} {
		got, ok := lastDuration([]byte(in))
		if !ok || got != want {
			t.Errorf("lastDuration(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "progress=end\n", "out_time_us=N/A\n", "out_time_us=-5\n", "out_time_us=12\nout_time_us=x\n"} {
		if _, ok := lastDuration([]byte(in)); ok {
			t.Errorf("lastDuration(%q) established a length", in)
		}
	}
	if truncate("  abc ", 5) != "abc" || truncate("abcdef", 3) != "abc..." {
		t.Error("truncate")
	}
}

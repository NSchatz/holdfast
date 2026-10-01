package audio

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TWO-PASS EBU R128 LOUDNESS (docs/design/audio.md#loudness).
//
// The targets are EBU R 128's own (EBU R 128-2023, "Loudness normalisation and permitted
// maximum level of audio signals", https://tech.ebu.ch/docs/r/r128.pdf , read 2026-10-01):
// "the Programme Loudness Level shall be normalised to a Target Level of -23.0 LUFS. Where
// attaining the Target Level is not achievable practically (for example, live programmes), a
// tolerance of +-1.0 LU is permitted", and "the True Peak Level of a programme shall not
// exceed -1 dBTP". R 128 sets no loudness-range target, so the one loudnorm needs is
// LoudnessRangeLU below.
const (
	// TargetLUFS is R 128's Target Level.
	TargetLUFS = -23.0
	// TargetTruePeak is R 128's maximum True Peak Level, in dBTP.
	TargetTruePeak = -1.0
	// LoudnessRangeLU is loudnorm's loudness-range target. ASSUMED: R 128 sets none, and
	// loudnorm keeps its linear (dynamics-preserving) mode only where the measured range is
	// at or below this target (libavfilter/af_loudnorm.c:820-822), so it is set at 20 LU,
	// wide enough that ordinary film and music measured with a range under it are
	// normalised by one gain.
	LoudnessRangeLU = 20.0
	// ToleranceLU is how far the output's measured integrated loudness may sit from
	// TargetLUFS: R 128's +-1.0 LU for where the target is not achievable exactly, which a
	// lossy encode after the normalisation is.
	ToleranceLU = 1.0
)

// The normalization_type values loudnorm reports, and the one this build records where no
// report was read.
const (
	ModeLinear      = "linear"
	ModeDynamic     = "dynamic"
	ModeNotRecorded = "not-recorded"
)

// Stats are the figures loudnorm reports with print_format=json: what it measured on its
// input, what it produced, and which mode it ran. The input figures of a first pass are what
// the second pass is told (measured_*).
type Stats struct {
	InputI, InputTP, InputLRA, InputThresh float64
	OutputI                                float64
	TargetOffset                           float64
	// Mode is normalization_type as loudnorm printed it: "linear" or "dynamic".
	Mode string
}

// statsJSON is loudnorm's JSON block; every figure is printed as a string ("-23.02").
type statsJSON struct {
	InputI            string `json:"input_i"`
	InputTP           string `json:"input_tp"`
	InputLRA          string `json:"input_lra"`
	InputThresh       string `json:"input_thresh"`
	OutputI           string `json:"output_i"`
	NormalizationType string `json:"normalization_type"`
	TargetOffset      string `json:"target_offset"`
}

// ParseStats reads one loudnorm JSON report. Every figure must be present and a number
// ("-inf", which loudnorm prints for silence, parses as one and is refused by Usable), and the
// mode must be one loudnorm prints: a report missing anything is refused rather than read as
// zeros, which would be a fabricated measurement.
func ParseStats(b []byte) (Stats, error) {
	var j statsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Stats{}, fmt.Errorf("loudnorm report is not JSON: %w", err)
	}
	var s Stats
	for _, f := range []struct {
		name string
		v    string
		to   *float64
	}{
		{"input_i", j.InputI, &s.InputI}, {"input_tp", j.InputTP, &s.InputTP},
		{"input_lra", j.InputLRA, &s.InputLRA}, {"input_thresh", j.InputThresh, &s.InputThresh},
		{"output_i", j.OutputI, &s.OutputI}, {"target_offset", j.TargetOffset, &s.TargetOffset},
	} {
		v, err := strconv.ParseFloat(strings.TrimSpace(f.v), 64)
		if err != nil {
			return Stats{}, fmt.Errorf("loudnorm report has no readable %s (%q)", f.name, f.v)
		}
		*f.to = v
	}
	switch j.NormalizationType {
	case ModeLinear, ModeDynamic:
		s.Mode = j.NormalizationType
	default:
		return Stats{}, fmt.Errorf("loudnorm report names no mode this build knows (%q)", j.NormalizationType)
	}
	return s, nil
}

// Usable reports whether a first pass's figures can be handed to a second: each finite and
// inside the range loudnorm accepts for its measured_* option (measured_I -99..0,
// measured_TP -99..99, measured_LRA 0..99, measured_thresh -99..0, offset -99..99;
// `ffmpeg -h filter=loudnorm` on the pinned build). Silence measures -inf, which no option
// takes; such a track is re-encoded without normalisation, and the row says why.
func (s Stats) Usable() bool {
	in := func(v, lo, hi float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi }
	return in(s.InputI, -99, 0) && in(s.InputTP, -99, 99) && in(s.InputLRA, 0, 99) &&
		in(s.InputThresh, -99, 0) && in(s.TargetOffset, -99, 99)
}

// targets are the three target options both passes run with.
func targets() string {
	return "I=" + num(TargetLUFS) + ":TP=" + num(TargetTruePeak) + ":LRA=" + num(LoudnessRangeLU)
}

// num renders a figure the way loudnorm printed it, two decimals.
func num(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// MeasureFilter is the first pass: loudnorm at the targets, measuring only, its report
// written as JSON to statsFile.
func MeasureFilter(statsFile string) string {
	return "loudnorm=" + targets() + ":print_format=json:stats_file=" + statsFile
}

// NormalizeFilter is the second pass: loudnorm told the first pass's figures and asked for
// linear mode, its report written to statsFile, and then aresample to rate. The aresample is
// always there: loudnorm upsamples to 192 kHz in its dynamic mode, and it reverts to dynamic
// on its own - where the measured range is above the target, where the gain would take the
// true peak past the ceiling, or where the measured range is exactly 0
// (libavfilter/af_loudnorm.c:820-822 and :740-751 at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01;
// verify-streams-hdr.md claim 10). Which mode ran is read back from the report.
func NormalizeFilter(m Stats, statsFile string, rate int) string {
	return "loudnorm=" + targets() +
		":measured_I=" + num(m.InputI) + ":measured_TP=" + num(m.InputTP) +
		":measured_LRA=" + num(m.InputLRA) + ":measured_thresh=" + num(m.InputThresh) +
		":offset=" + num(m.TargetOffset) +
		":linear=true:print_format=json:stats_file=" + statsFile +
		",aresample=" + strconv.Itoa(rate)
}

// DownmixFilter folds a track to stereo through swresample's default matrix: centre and
// surrounds at -3 dB into the fronts, LFE left out, the whole normalised so it cannot clip
// (research-streams-hdr.md section 2.5). Normalising the loudness afterwards recovers the
// level the matrix gives up.
const DownmixFilter = "aformat=channel_layouts=stereo"

// CheckLoudness is the loudness gate over one output stream: its measured integrated loudness
// within ToleranceLU of TargetLUFS.
func CheckLoudness(measured float64) error {
	if math.IsNaN(measured) || math.IsInf(measured, 0) || math.Abs(measured-TargetLUFS) > ToleranceLU {
		return fmt.Errorf("integrated loudness %s LUFS is outside %s +- %s LU",
			num(measured), num(TargetLUFS), num(ToleranceLU))
	}
	return nil
}

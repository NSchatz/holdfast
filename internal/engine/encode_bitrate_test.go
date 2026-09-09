package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
)

// pinArgs is the argument list this repository produced for each encoder key BEFORE
// S0079, at crf 22 / preset slow / pix_fmt yuv420p10le with no colour args and no
// x265 colour block.
//
// It is written out as literals rather than derived from the code under test,
// deliberately: a golden table derived from buildArgs would agree with buildArgs
// however buildArgs changes, which is the one thing AC-A1 needs it not to do. These
// are the strings a config file that predates this item has always produced, and if
// a future edit changes one of them, this table is what says so.
var pinArgs = map[string][]string{
	"cpu": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-preset", "slow", "-crf", "22", "-x265-params", "log-level=error",
	},
	"svtav1": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-preset", "6", "-crf", "22",
	},
	"nvenc": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-rc", "vbr", "-cq", "22", "-b:v", "0", "-preset", "p5",
	},
	"av1_nvenc": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-rc", "vbr", "-cq", "22", "-b:v", "0", "-preset", "p5",
	},
	"qsv": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-global_quality", "22",
	},
	"vaapi": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-vf", "format=nv12,hwupload", "-qp", "22",
	},
	"amf": {
		"-pix_fmt", "yuv420p10le", "-fps_mode", "passthrough",
		"-rc", "cqp", "-qp_i", "22", "-qp_p", "22",
	},
}

// AC-A1: a config file that predates this item - no bitrate_kbps, no
// transcode_profiles, no scratch_dir - produces the SAME encoder argument list this
// repository produced before, for every encoder key in the registry, and the same
// effective container extension and pixel format.
//
// The registry is walked rather than listed, so an encoder added later is covered by
// this criterion the moment it exists rather than the moment somebody remembers to
// add a case.
func TestPreS0079Config_ProducesTheSameArgumentsForEveryEncoderInTheRegistry(t *testing.T) {
	cfg := config.Config{
		Encoder: "cpu", CRF: 22, Preset: "slow",
		PixelFormat: "auto", ContainerExt: "source",
		// bitrate_kbps, transcode_profiles and scratch_dir are all absent, which is
		// exactly what a config written before this item is.
	}

	known := encoder.Known()
	if len(known) != len(pinArgs) {
		t.Fatalf("the registry ships %d encoders (%v) and this table covers %d - a new encoder is ungraded by AC-A1",
			len(known), known, len(pinArgs))
	}
	for _, key := range known {
		want, ok := pinArgs[key]
		if !ok {
			t.Fatalf("encoder %q is in the registry and not in the pin table - AC-A1 does not cover it", key)
		}
		spec, _ := encoder.Lookup(key)
		cfg.Encoder = key
		ts := cfg.TranscodeIn(cfg.TopLevelProfile(), "/srv/media/film.mkv")
		got := buildArgs(spec, ts, "yuv420p10le", nil, "")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("encoder %q:\n  got  %v\n  want %v (what the pin produced)", key, got, want)
		}
	}

	// The container extension and the pixel format an unchanged config resolves to.
	// "source" and "auto" are the sentinels the pin used and they must still be the
	// sentinels, or a config that says nothing has silently started forcing one.
	ts := cfg.TranscodeIn(cfg.TopLevelProfile(), "/srv/media/film.mkv")
	if !ts.ContainerMatchesSource() {
		t.Errorf("container_ext %q no longer matches the source", ts.ContainerExt)
	}
	if !ts.PixelFormatAuto() {
		t.Errorf("pixel_format %q is no longer derived per source", ts.PixelFormat)
	}
	if ts.Profile != "" {
		t.Errorf("a config with no profiles resolved to profile %q", ts.Profile)
	}
	if ts.TargetsBitrate() {
		t.Errorf("a config with no bitrate_kbps resolved to a target bitrate of %d", ts.BitrateKbps)
	}
}

// AC-A2: a positive bitrate_kbps runs the encode under a target-bitrate rate control
// at that bitrate, for every encoder key the registry supports, and passes NO
// quality/CRF target for that job.
//
// Both halves are asserted, and the second is the one that matters: an encoder handed
// both a rate control and a quality target decides between them by its own precedence
// rules, so an argument list carrying `-b:v 8000k` beside `-crf 22` is not the encode
// the operator asked for however plausible it looks.
func TestBitrateKbps_TargetsTheBitrateAndPassesNoQualityTarget_ForEveryEncoder(t *testing.T) {
	// Every spelling of a quality target this repository's own arg builder has ever
	// used. A new encoder family's quality option would need adding here, which is
	// the point: the list is what "no quality target" MEANS, written down.
	qualityFlags := []string{"-crf", "-cq", "-global_quality", "-qp", "-qp_i", "-qp_p"}

	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "slow", BitrateKbps: 8000}
	for _, key := range encoder.Known() {
		t.Run(key, func(t *testing.T) {
			spec, _ := encoder.Lookup(key)
			cfg.Encoder = key
			ts := cfg.TranscodeIn(cfg.TopLevelProfile(), "/srv/media/film.mkv")
			got := buildArgs(spec, ts, "yuv420p10le", nil, "")

			if !hasArgPair(got, "-b:v", "8000k") {
				t.Errorf("%s: no -b:v 8000k in %v", key, got)
			}
			for _, q := range qualityFlags {
				for i, a := range got {
					if a == q {
						t.Errorf("%s: a quality target %s %v survived a target-bitrate encode: %v",
							key, q, valueAfter(got, i), got)
					}
				}
			}
			// NVENC spells constant-quality as `-cq N -b:v 0`, so a rate control
			// that left the zero behind would ask for a bitrate of nothing.
			if hasArgPair(got, "-b:v", "0") {
				t.Errorf("%s: -b:v 0 survived a target-bitrate encode: %v", key, got)
			}
			// AMF's cqp is a fixed-quantiser mode that ignores -b:v outright, so a
			// rate control that left it selected would be silently ignored.
			if hasArgPair(got, "-rc", "cqp") {
				t.Errorf("%s: -rc cqp survived a target-bitrate encode: %v", key, got)
			}
			// The universal source-fidelity args are unchanged on this path: a
			// bitrate-targeted encode carries the same colour and frame-timing
			// fidelity as a quality-targeted one.
			if !hasArgPair(got, "-pix_fmt", "yuv420p10le") || !hasArgPair(got, "-fps_mode", "passthrough") {
				t.Errorf("%s: a target-bitrate encode lost a universal fidelity arg: %v", key, got)
			}
		})
	}

	// The libx265 HDR10 static-metadata block travels on the bitrate path too. It is
	// libx265's only mechanism for carrying mastering-display and MaxCLL, and losing
	// it would silently drop HDR metadata for every bitrate-targeted cpu encode.
	t.Run("the x265 colour block survives", func(t *testing.T) {
		spec, _ := encoder.Lookup("cpu")
		cfg.Encoder = "cpu"
		got := buildArgs(spec, cfg.TranscodeIn(cfg.TopLevelProfile(), "/x.mkv"), "yuv420p10le", nil, ":hdr-opt=1")
		if !hasArgPair(got, "-x265-params", "log-level=error:hdr-opt=1") {
			t.Errorf("the x265 colour block was dropped on the bitrate path: %v", got)
		}
	})

	// A profile's bitrate reaches the encoder too, and a job under a profile that
	// does NOT set one keeps the quality target - so the switch is per job and not
	// per run.
	t.Run("per job, through a profile", func(t *testing.T) {
		three := 3000
		pcfg := config.Config{
			Encoder: "cpu", CRF: 22, Preset: "slow",
			TranscodeProfiles: []config.EncodeProfile{
				{Name: "bulk", Match: "**/TV/**", BitrateKbps: &three},
				{Name: "films", Match: "**/Films/**"},
			},
		}
		spec, _ := encoder.Lookup("cpu")
		ptop := pcfg.TopLevelProfile()
		tv := buildArgs(spec, pcfg.TranscodeIn(ptop, "/srv/TV/s1/ep.mkv"), "yuv420p10le", nil, "")
		if !hasArgPair(tv, "-b:v", "3000k") || strings.Contains(strings.Join(tv, " "), "-crf") {
			t.Errorf("the profile's bitrate did not reach the encoder: %v", tv)
		}
		film := buildArgs(spec, pcfg.TranscodeIn(ptop, "/srv/Films/a.mkv"), "yuv420p10le", nil, "")
		if !hasArgPair(film, "-crf", "22") || strings.Contains(strings.Join(film, " "), "-b:v") {
			t.Errorf("a profile that sets no bitrate lost the quality target: %v", film)
		}
	})
}

// valueAfter renders the argument following index i, for a failure message that says
// which value survived rather than only which flag.
func valueAfter(args []string, i int) string {
	if i+1 < len(args) {
		return args[i+1]
	}
	return "<nothing>"
}

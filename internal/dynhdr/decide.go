package dynhdr

import "fmt"

// CarryingEncoder is the one registry encoder that carries dynamic HDR: cpu, libx265. Its
// ffmpeg wrapper codes a Dolby Vision RPU (`-dolbyvision`, FFmpeg commit 39ca87ed1e,
// https://github.com/FFmpeg/FFmpeg/commit/39ca87ed1ef876af9622a5aa331e18167fdfdf27 , read
// 2026-10-01) and x265 inserts HDR10+ from a JSON file (`--dhdr10-info`,
// https://x265.readthedocs.io/en/master/cli.html#cmdoption-dhdr10-info , read 2026-10-01).
// No other encoder in the registry is shown here to carry either.
const CarryingEncoder = "cpu"

// The two Dolby Vision profiles this build carries, and the one compatibility id: profile 8
// with an HDR10-compatible base layer (8.1), and profile 7 converted to it.
const (
	ProfileConverted = 7
	ProfileCarried   = 8
	CompatHDR10      = 1
)

// The values of the dolby_vision_p7 key, as internal/config spells them.
const (
	P7Skip    = "skip"
	P7Convert = "convert"
)

// Source is what the guards already read off a source that classifies as Dolby Vision or
// HDR10+: nothing here probes.
type Source struct {
	// DolbyVision reports that the HDR classifier named the source Dolby Vision (a dvhe/dvh1
	// codec tag, a DOVI configuration record or Dolby Vision side data).
	DolbyVision bool
	// Record is the source's DOVI configuration record, read off its stream side data.
	Record Record
	// HDR10Plus reports that the source's side data names HDR10+.
	HDR10Plus bool
	// Primaries, Transfer and Matrix are the colour tags the plan writes (hdr.DeriveColor),
	// which is what the encoder codes the RPU's compatibility id from.
	Primaries, Transfer, Matrix string
	// MasterDisplay is the plan's mastering display in libx265's spelling, "" where the
	// source carries no complete block.
	MasterDisplay string
}

// Settings are the configuration a verdict reads.
type Settings struct {
	// Encoder is the job's effective encoder key; RemuxOnly whether its root re-encodes
	// nothing; P7 its resolved dolby_vision_p7.
	Encoder   string
	RemuxOnly bool
	P7        string
}

// Intent is what a job carries: the declaration its encode plan is derived from once the
// pre-pass (Prepare) has run.
type Intent struct {
	// DolbyVision is true where the output carries a Dolby Vision RPU; SourceProfile is the
	// source's profile (8, or 7 converted), and Convert whether it is converted first.
	DolbyVision   bool
	SourceProfile int
	Convert       bool
	// HDR10Plus is true where the output carries HDR10+.
	HDR10Plus bool
	// ZeroL5 is true where the job crops a Dolby Vision source to the active area its RPU
	// names (docs/design/crop.md#dolby-vision): the pre-pass rewrites the RPU with L5 zeroed
	// into a raw stream the encode reads, as a converted profile 7 stream is read, and the L5
	// gate holds every output frame to 0/0/0/0. Set by the engine's crop decision, never here.
	ZeroL5 bool
}

// Rewrites reports whether the pre-pass writes a raw stream the encode reads in place of the
// source's video: a profile 7 conversion, or an L5 zeroing.
func (i Intent) Rewrites() bool { return i.Convert || (i.DolbyVision && i.ZeroL5) }

// Carries reports whether the intent carries anything.
func (i Intent) Carries() bool { return i.DolbyVision || i.HDR10Plus }

// NeedsDoviTool and NeedsHDR10PlusTool name the tools the intent's pre-pass runs.
func (i Intent) NeedsDoviTool() bool      { return i.Rewrites() }
func (i Intent) NeedsHDR10PlusTool() bool { return i.HDR10Plus }

// Inputs are the configuration keys a verdict read, as the engine records them
// (docs/requeue.md). They are spelled here as the engine's InputEncoder and the config key.
const (
	InputEncoder = "encoder"
	InputP7      = "dolby_vision_p7"
)

// Verdict is Decide's answer: an intent to carry, or a skip with its token, the sentence the
// daemon logs, and the configuration keys the verdict read.
type Verdict struct {
	Intent Intent
	Skip   string
	Why    string
	Inputs []string
}

// Skipped reports whether the verdict skips the source.
func (v Verdict) Skipped() bool { return v.Skip != "" }

// Decide is which dynamic metadata a job carries, or why it skips the source. It is a pure
// function of what the guards already read, so the read-only plan pass and the daemon ask it
// the same question and get the same answer. src must classify as Dolby Vision or HDR10+;
// a source that is neither carries nothing and is not skipped.
func Decide(src Source, set Settings) Verdict {
	if !src.DolbyVision && !src.HDR10Plus {
		return Verdict{}
	}
	reason, what := ReasonHDR10Plus, "HDR10+ dynamic metadata"
	if src.DolbyVision {
		reason, what = ReasonDolbyVision, "Dolby Vision"
	}
	if set.RemuxOnly {
		return Verdict{Skip: reason, Why: what + " on a remux-only root: a stream copy carries it but this " +
			"build carries dynamic metadata only through a re-encode it can gate"}
	}
	if set.Encoder != CarryingEncoder {
		return Verdict{Skip: reason, Inputs: []string{InputEncoder},
			Why: fmt.Sprintf("%s is carried only by the cpu encoder (libx265); the encoder %q would strip it", what, set.Encoder)}
	}
	in := Intent{HDR10Plus: src.HDR10Plus}
	if src.DolbyVision {
		v := decideDolbyVision(src, set)
		if v.Skipped() {
			return v
		}
		in.DolbyVision, in.SourceProfile, in.Convert = true, v.Intent.SourceProfile, v.Intent.Convert
	}
	return Verdict{Intent: in}
}

// decideDolbyVision is the Dolby Vision half of Decide, for the cpu encoder.
func decideDolbyVision(src Source, set Settings) Verdict {
	r := src.Record
	if !r.Present || !r.Readable {
		return Verdict{Skip: ReasonDolbyVision, Why: "Dolby Vision whose DOVI configuration record could not be read, " +
			"so its profile is unknown"}
	}
	in := Intent{SourceProfile: r.Profile}
	switch {
	case r.Profile == ProfileCarried && r.CompatID == CompatHDR10 && r.RPU && r.BL && !r.EL:
	case r.Profile == ProfileConverted:
		if set.P7 != P7Convert {
			return Verdict{Skip: ReasonProfile7, Inputs: []string{InputP7, InputEncoder},
				Why: "Dolby Vision profile 7 is converted to profile 8.1 only where dolby_vision_p7 is convert " +
					"(the conversion discards the enhancement layer)"}
		}
		in.Convert = true
	default:
		return Verdict{Skip: ReasonDolbyVision, Why: fmt.Sprintf("Dolby Vision profile %d with compatibility id %d "+
			"(rpu %t, el %t, bl %t) is not carried by this build: it carries profile 8.1 and converts profile 7",
			r.Profile, r.CompatID, r.RPU, r.EL, r.BL)}
	}
	// libx265 codes the RPU's compatibility id from the colour the encode writes
	// (dovi_rpuenc.c, FFmpeg 5d4d3bdc61 lines 140-153): BT.2020 primaries and matrix with the
	// PQ transfer is id 1, and nothing else is.
	if src.Primaries != "bt2020" || src.Transfer != "smpte2084" || src.Matrix != "bt2020nc" {
		return Verdict{Skip: ReasonDolbyVision, Why: fmt.Sprintf("Dolby Vision whose colour (%s/%s/%s) would not "+
			"code profile 8.1's compatibility id 1 (bt2020/smpte2084/bt2020nc)", src.Primaries, src.Transfer, src.Matrix)}
	}
	if src.MasterDisplay == "" {
		return Verdict{Skip: ReasonNoMasteringDisplay, Inputs: []string{InputEncoder},
			Why: "Dolby Vision without a complete mastering display, which x265 requires to code profile 8.1"}
	}
	return Verdict{Intent: in}
}

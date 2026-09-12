package engine

import (
	"strconv"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/store"
)

// The decision-input keys, and they are a STORED WIRE FORMAT exactly as the skip tokens
// above them are: each is written onto a terminal row and read back by a later build to
// decide whether that row's verdict still re-derives. Add to them freely; renaming one
// changes what an existing row means, and a row whose key this build no longer offers is
// re-opened once (see store.DecisionInputs.StillMatches).
//
// Every one of them is spelled exactly as the YAML key it reads, with one exception
// called out below, so an operator meeting `min_bitrate_kbps=2500` on a row can go
// straight to the line in their config that produced it.
const (
	InputEncoder        = "encoder"
	InputCRF            = "crf"
	InputPreset         = "preset"
	InputPixelFormat    = "pixel_format"
	InputMinBitrateKbps = "min_bitrate_kbps"
	InputContainerExt   = "container_ext"

	// InputTargetCodec is the exception: it is not a configuration key but what
	// `encoder` RESOLVES to (cpu -> hevc, svtav1 -> av1). The already-at-target-codec
	// guard reads the resolved codec and nothing else, so the resolved codec is what it
	// records - which is why moving `encoder: nvenc` to `encoder: qsv` re-opens nothing
	// it skipped (both target hevc) while moving it to `svtav1` re-opens all of it.
	InputTargetCodec = "target_codec"
)

// DecisionInputsForProfile is every decision input ONE RESOLVED PROFILE offers: the one
// place the value of each key is read, so the value a guard RECORDS and the value a later
// scan COMPARES it against cannot come from two different readings of the same key.
//
// The unit is a profile rather than the whole configuration because every key above is a
// per-root knob: two roots may run different encoders at different CRFs behind one
// `crf:` at the top level. A row decided under a root's profile has to record what THAT
// decision read, or the row says it read a value no guard ever looked at - and the
// re-derivation built on it would re-open a library whose profile never moved while
// leaving alone the one that did.
func DecisionInputsForProfile(prof config.Profile) store.DecisionInputs {
	return store.InputsRead(map[string]string{
		InputTargetCodec:    targetCodecFor(prof),
		InputEncoder:        prof.Encoder,
		InputCRF:            strconv.Itoa(prof.CRF),
		InputPreset:         prof.Preset,
		InputPixelFormat:    prof.PixelFormat,
		InputMinBitrateKbps: strconv.Itoa(prof.MinBitrateKbps),
		InputContainerExt:   prof.ContainerExt,
	})
}

// DecisionInputsFor is the TOP-LEVEL profile's decision inputs: what a root that
// overrides nothing decides by, and what a configuration with no per-root profile at all
// decides every file by.
//
// It is a package function rather than a method because `holdfast validate` has to
// answer the same question with no engine, no ffmpeg and no store open for writing.
func DecisionInputsFor(cfg config.Config) store.DecisionInputs {
	return DecisionInputsForProfile(cfg.TopLevelProfile())
}

// targetCodecFor is what ffprobe should report codec_name as for a SUCCESSFUL output
// under this profile - "hevc" for the cpu/nvenc/qsv/vaapi/amf encoders, "av1" for
// svtav1/av1_nvenc. It defaults to "hevc" for an unknown or empty key (Validate rejects
// an unknown encoder before the engine is ever built, so the default is a fallback and
// not a live path).
//
// It is PER PROFILE because `encoder` is: one root may re-encode to hevc while another
// re-encodes to av1, and the skip-already-target guard and the output-codec check must
// each ask about the encoder that root actually uses. Asking about the top-level encoder
// would skip every av1 file under an av1 root as "already at target" and reject every
// hevc output under an hevc root.
func targetCodecFor(prof config.Profile) string {
	if spec, ok := encoder.Lookup(prof.Encoder); ok {
		return spec.TargetCodec
	}
	return "hevc"
}

// inputsFor is what the profile deciding THIS file offers right now, handed to its Claim
// so a terminal row is measured against the configuration in force for its own root
// rather than treated as a permanent answer.
func (e *Engine) inputsFor(prof config.Profile) store.DecisionInputs {
	return DecisionInputsForProfile(prof)
}

// inputsRead is the record ONE decision writes: the current value, under the profile that
// decided it, of exactly the keys that decision read, and no others. Called with no keys
// it records the empty set, which is a record (a verdict no configuration change can
// move) and not an absence.
//
// The values are taken from inputsFor rather than re-read from the config, which is what
// makes "recorded under" and "still matches" the same reading of the same key.
func (e *Engine) inputsRead(prof config.Profile, keys ...string) store.DecisionInputs {
	current := e.inputsFor(prof)
	read := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := current.Value(k); ok {
			read[k] = v
		}
	}
	return store.InputsRead(read)
}

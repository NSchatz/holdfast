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

// DecisionInputsFor is every decision input this configuration offers: the one place the
// value of each key is read, so the value a guard RECORDS and the value a later scan
// COMPARES it against cannot come from two different readings of the same key.
//
// It is a package function rather than a method because `holdfast validate` has to
// answer the same question with no engine, no ffmpeg and no store open for writing.
func DecisionInputsFor(cfg config.Config) store.DecisionInputs {
	return store.InputsRead(map[string]string{
		InputTargetCodec:    targetCodecFor(cfg),
		InputEncoder:        cfg.Encoder,
		InputCRF:            strconv.Itoa(cfg.CRF),
		InputPreset:         cfg.Preset,
		InputPixelFormat:    cfg.PixelFormat,
		InputMinBitrateKbps: strconv.Itoa(cfg.MinBitrateKbps),
		InputContainerExt:   cfg.ContainerExt,
	})
}

// targetCodecFor resolves the encoder key to the codec a successful output must be in,
// defaulting to hevc for an unknown or empty key exactly as New does (Validate rejects an
// unknown encoder long before either is reached, so the default is a fallback and not a
// live path).
func targetCodecFor(cfg config.Config) string {
	if spec, ok := encoder.Lookup(cfg.Encoder); ok {
		return spec.TargetCodec
	}
	return "hevc"
}

// currentInputs is what this engine's configuration offers right now, handed to every
// Claim so a terminal row is measured against the configuration in force rather than
// treated as a permanent answer.
func (e *Engine) currentInputs() store.DecisionInputs { return DecisionInputsFor(e.Cfg) }

// inputsRead is the record ONE decision writes: the current value of exactly the keys
// that decision read, and no others. Called with no keys it records the empty set, which
// is a record (a verdict no configuration change can move) and not an absence.
//
// The values are taken from currentInputs rather than re-read from the config, which is
// what makes "recorded under" and "still matches" the same reading of the same key.
func (e *Engine) inputsRead(keys ...string) store.DecisionInputs {
	current := e.currentInputs()
	read := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := current.Value(k); ok {
			read[k] = v
		}
	}
	return store.InputsRead(read)
}

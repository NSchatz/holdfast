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

// EVERY VALUE HERE IS RESOLVED FOR ONE PATH, through the whole layering the decision that
// read it used for that path: the built-in default, then the top level, then the profile of
// the library root the file lives under, then the encode profile whose match selects it.
// The keys an encode profile can move are therefore read from the job's effective settings
// (config.Transcode) and the gates it can never reach are read from the root's own profile,
// which is where each of them is decided.
//
// Two properties hold this together and neither may be traded for the other:
//
//   - SYMMETRY. Record and compare are one resolution, of one key, for one path, through
//     DecisionInputsForJob. A row written under a configuration therefore matches under
//     that configuration - and a recorded value that nothing could resolve again would be
//     a row re-opened on every scan for ever, on a library-sized ledger a re-encode of
//     everything, each accepted encode deleting its source.
//   - MINIMALITY. Only the keys a decision actually read are recorded, never a digest of
//     the configuration. An edit that moves no key a row read honours that row, whatever
//     else it changed - a notification URL, an added library root, an encode profile whose
//     match does not select that path, or a key on the profile that does select it but
//     that this row's guard never looked at.
//
// docs/requeue.md states the same rule for an operator.

// DecisionInputsForJob is every decision input ONE JOB offers: the one place the value of
// each key is read, so the value a guard RECORDS and the value a later scan COMPARES it
// against cannot come from two different readings of the same key.
//
// ts is that job's effective encode settings - the root's profile with the matching encode
// profile's overrides laid over it - and it supplies every key an encode profile can move.
// prof is the library root's own profile and supplies the ones it cannot: min_bitrate_kbps
// is a gate that decides whether a source may be destroyed, no encode profile carries it,
// and the guard that reads it reads the root's value (see Engine.ProcessFile).
//
// target_codec is what ts.Encoder RESOLVES to, because that is what the guard compares
// against: a job whose profile targets av1 is decided against av1 while its neighbour under
// the inherited settings is decided against hevc, in the same run.
func DecisionInputsForJob(prof config.Profile, ts config.Transcode) store.DecisionInputs {
	return store.InputsRead(map[string]string{
		InputTargetCodec:    targetCodecFor(ts.Encoder),
		InputEncoder:        ts.Encoder,
		InputCRF:            strconv.Itoa(ts.CRF),
		InputPreset:         ts.Preset,
		InputPixelFormat:    ts.PixelFormat,
		InputMinBitrateKbps: strconv.Itoa(prof.MinBitrateKbps),
		InputContainerExt:   ts.ContainerExt,
	})
}

// DecisionInputsForProfile is one resolved library profile's decision inputs with NO encode
// profile laid over them: what every file under that root resolves to when no encode
// profile's match selects it, which is every file in a configuration that has none.
func DecisionInputsForProfile(prof config.Profile) store.DecisionInputs {
	return DecisionInputsForJob(prof, config.Transcode{
		Encoder:      prof.Encoder,
		CRF:          prof.CRF,
		Preset:       prof.Preset,
		PixelFormat:  prof.PixelFormat,
		ContainerExt: prof.ContainerExt,
	})
}

// DecisionInputsFor is the TOP-LEVEL profile's decision inputs: what a root that
// overrides nothing decides by, and what a configuration with no per-root profile and no
// encode profile decides every file by.
//
// It is a package function rather than a method because `holdfast validate` has to
// answer the same question with no engine, no ffmpeg and no store open for writing.
func DecisionInputsFor(cfg config.Config) store.DecisionInputs {
	return DecisionInputsForProfile(cfg.TopLevelProfile())
}

// DecisionInputsPerPath is how the ledger survey behind the startup report and `validate`
// asks the read-set question: once per row, about that row's own path.
//
// It is what overturns the survey's old shape. That read compared the whole ledger against
// one value with no file path in hand, so no encode profile could move either side of the
// comparison and a row decided under one was counted as still matching for ever. A ledger
// row carries its own path, so the survey can resolve exactly as a claim does - and it must,
// or the counts an operator reads describe a rule the scan does not apply.
//
// The resolution is memoized on (library root, encode profile), which is the whole of what
// it depends on: two files under one root that the same profile selects resolve to the same
// record, so a 300,000-row ledger costs a handful of resolutions and one match per row. The
// closure is stateful and is called row by row from the survey's single goroutine.
func DecisionInputsPerPath(cfg config.Config) store.InputsForPath {
	roots := cfg.RootProfiles()
	top := cfg.TopLevelProfile()
	cached := map[string]store.DecisionInputs{}
	return func(path string) (store.DecisionInputs, bool) {
		prof, rooted, key := top, false, ""
		for _, r := range roots {
			if r.Contains(path) {
				prof, rooted, key = r.Profile, true, r.Clean
				break
			}
		}
		ts := cfg.TranscodeIn(prof, path)
		key += "\x00" + ts.Profile
		if in, ok := cached[key]; ok {
			return in, rooted
		}
		in := DecisionInputsForJob(prof, ts)
		cached[key] = in
		return in, rooted
	}
}

// targetCodecFor is what ffprobe should report codec_name as for a SUCCESSFUL output
// under this profile - "hevc" for the cpu/nvenc/qsv/vaapi/amf encoders, "av1" for
// svtav1/av1_nvenc. It defaults to "hevc" for an unknown or empty key (Validate rejects
// an unknown encoder before the engine is ever built, so the default is a fallback and
// not a live path). It takes the KEY and not a profile because each job's guard has to ask
// about the encoder THAT JOB uses, which an encode profile may have overridden.
func targetCodecFor(key string) string {
	if spec, ok := encoder.Lookup(key); ok {
		return spec.TargetCodec
	}
	return "hevc"
}

// inputsFor is what the configuration in force offers for THIS JOB right now, handed to its
// Claim. ts is the job's effective settings, resolved once per file from that file's own
// path, so the value handed to Claim is the value a decision on that path would record.
func (e *Engine) inputsFor(prof config.Profile, ts config.Transcode) store.DecisionInputs {
	return DecisionInputsForJob(prof, ts)
}

// inputsRead is the record ONE decision writes: the current value, under the settings that
// decided it, of exactly the keys that decision read, and no others. Called with no keys
// it records the empty set, which is a record (a verdict no configuration change can
// move) and not an absence.
//
// The values are taken from inputsFor rather than re-read from the config, which is what
// makes "recorded under" and "still matches" the same reading of the same key.
func (e *Engine) inputsRead(prof config.Profile, ts config.Transcode, keys ...string) store.DecisionInputs {
	current := e.inputsFor(prof, ts)
	read := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := current.Value(k); ok {
			read[k] = v
		}
	}
	return store.InputsRead(read)
}

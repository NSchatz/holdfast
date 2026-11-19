package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/encoder"
)

// x265OpenGOPKey is the one place the libx265 open-GOP key is spelled.
const x265OpenGOPKey = "x265_open_gop"

// X265OpenGOPUnset is how an absent x265_open_gop renders, in `validate` and in the values
// the digest reads: the key says nothing, so libx265 is told nothing and keeps its own
// default (open GOP). See docs/design/encode-plan.md#gop.
const X265OpenGOPUnset = "unset"

// x265Codec is the ffmpeg codec the key is a parameter of.
const x265Codec = "libx265"

// OpenGOP reports what this root tells libx265 about open GOPs: set is false where the
// key is absent (nothing is passed, libx265's own default stands), and open is then
// meaningless. Where set, open is the value the operator wrote: true passes open-gop=1 and
// false open-gop=0 in -x265-params.
func (p Profile) OpenGOP() (open, set bool) {
	if p.X265OpenGOP == nil {
		return false, false
	}
	return *p.X265OpenGOP, true
}

// OpenGOPMode is this root's x265_open_gop as `validate` prints it and the digest reads
// it: "true", "false", or X265OpenGOPUnset.
func (p Profile) OpenGOPMode() string {
	open, set := p.OpenGOP()
	if !set {
		return X265OpenGOPUnset
	}
	return strconv.FormatBool(open)
}

// checkX265OpenGOPValue refuses a RAW x265_open_gop that is not a boolean, naming the key and
// where it was written. The weakly typed decoder would read 3 as true and "no" as false
// without a word; a GOP structure nobody wrote is refused rather than guessed at. A YAML
// boolean arrives as a bool and an environment override as a string, so both spellings of a
// genuine boolean are accepted.
func checkX265OpenGOPValue(where string, raw any) error {
	switch v := raw.(type) {
	case bool:
		return nil
	case string:
		if _, err := strconv.ParseBool(v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: %s must be true or false: %#v is neither", where, x265OpenGOPKey, raw)
}

// validateX265OpenGOP refuses x265_open_gop on a root none of whose files can be encoded by
// libx265, naming the key, the root and every encoder the configuration routes that root's
// files to. The key is a libx265 parameter and every other encoder ignores it, so on such a
// root it is a setting nothing reads: an operator who wrote `x265_open_gop: false` for a
// library their players need closed GOPs in would believe they had them. Refused at start
// rather than ignored file by file.
//
// A root whose files CAN reach libx265 is accepted, and the key applies to exactly the jobs
// libx265 encodes: the root's own encoder, a rule's, an encode profile's, or the software
// fallback hw_fallback: software runs where a hardware HEVC encoder is missing or fails. A
// job some other encoder writes is told nothing about GOPs, and its command line is the one
// it always was.
func (c *Config) validateX265OpenGOP(r Root) error {
	if _, set := r.Profile.OpenGOP(); !set {
		return nil
	}
	named := c.encodersFor(r.Profile)
	for _, key := range named {
		if reachesLibx265(key, r.Profile.HWFallbackMode()) {
			return nil
		}
	}
	return fmt.Errorf("library root %s: %s is set, but no file under this root can be encoded by %s "+
		"(the encoders this configuration routes it to: %s; hw_fallback: %s): the key is a %s parameter "+
		"and every other encoder ignores it. Remove %s from this root, or encode it with encoder: cpu",
		r.Clean, x265OpenGOPKey, x265Codec, strings.Join(named, ", "), r.Profile.HWFallbackMode(), x265Codec,
		x265OpenGOPKey)
}

// encodersFor is every `encoder:` value a file under a root with profile p can be encoded
// under, as written: the root's own, each of its rules', and each encode profile's (an encode
// profile matches by path pattern, so any of them may reach any root).
func (c *Config) encodersFor(p Profile) []string {
	out := []string{p.Encoder}
	for _, r := range p.Rules {
		if r.Encoder != nil {
			out = append(out, *r.Encoder)
		}
	}
	for _, ep := range c.EncodeProfiles {
		if ep.Encoder != nil {
			out = append(out, *ep.Encoder)
		}
	}
	return out
}

// reachesLibx265 reports whether a job under the `encoder:` value key can be encoded by
// libx265: the key names it, or it names a hardware HEVC encoder (or `auto`, which chooses
// only those) and hw_fallback runs the software encoder of the same codec where it is missing
// or fails.
func reachesLibx265(key, fallback string) bool {
	if key == encoder.Auto {
		return fallback == HWFallbackSoftware && encoder.AutoTargetCodec == "hevc"
	}
	// An unknown key is the zero Spec, which names no codec and falls back to nothing; Validate
	// refuses the key itself by name.
	spec, _ := encoder.Lookup(key)
	if spec.FFmpegCodec == x265Codec {
		return true
	}
	return fallback == HWFallbackSoftware && encoder.SoftwareFallback(spec).FFmpegCodec == x265Codec
}

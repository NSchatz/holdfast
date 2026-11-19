package config

import (
	"fmt"
	"strconv"

	"github.com/NSchatz/holdfast/internal/playability"
)

// The playability gate's two keys (docs/design/swap.md#playability), each the one place it is
// spelled. The gate is on for every encode by default, as chapter parity and output fidelity
// are; these keys tune it or turn its two halves off, per library root.
//
// playability_cold_starts is how many keyframes spread across an output are each decoded from
// a cold start (up to four times as many keyframes followed by leading pictures are too); 0
// turns cold starts off. playability_segments is whether the output is cut into HLS-style
// segments and each decoded on its own.
const (
	playabilityColdStartsKey = "playability_cold_starts"
	playabilitySegmentsKey   = "playability_segments"
)

// PlayabilityColdStartCount is how many keyframes spread across an output under this root are
// each decoded from a cold start: the root's playability_cold_starts, or
// playability.DefaultColdStarts where it is unset. 0 is off.
func (p Profile) PlayabilityColdStartCount() int {
	if p.PlayabilityColdStarts == nil {
		return playability.DefaultColdStarts
	}
	return *p.PlayabilityColdStarts
}

// PlayabilitySegmentsOn reports whether an output under this root is cut into segments and
// each decoded on its own (playability_segments), defaulting to true when unset.
func (p Profile) PlayabilitySegmentsOn() bool {
	return p.PlayabilitySegments == nil || *p.PlayabilitySegments
}

// playabilityValues are the two keys as `validate` prints them and the digest reads them,
// resolved, in profileKnobs order.
func (p Profile) playabilityValues() []string {
	return []string{strconv.Itoa(p.PlayabilityColdStartCount()), strconv.FormatBool(p.PlayabilitySegmentsOn())}
}

// playabilityDigestSilent is the digest rule for the two keys: each contributes nothing at the
// shipped default, so a root that sets neither digests as it did before the keys existed.
func playabilityDigestSilent(knob, value string) (silent, ok bool) {
	switch knob {
	case playabilityColdStartsKey:
		return value == strconv.Itoa(playability.DefaultColdStarts), true
	case playabilitySegmentsKey:
		return value == "true", true
	}
	return false, false
}

// checkPlayabilityValue refuses a RAW value of either key that is not of its type, naming the
// key and where it was written: the weakly typed decoder would read 3 as true, 16.5 as 16 and
// "yes" as nothing it says, on keys that decide whether a check stands between an output and
// the deletion of its source. A YAML value arrives typed and an environment override as a
// string, so both spellings of a genuine value are accepted. Keys other than the two pass.
func checkPlayabilityValue(where, key string, raw any) error {
	switch key {
	case playabilitySegmentsKey:
		switch v := raw.(type) {
		case bool:
			return nil
		case string:
			if _, err := strconv.ParseBool(v); err == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: %s must be true or false: %#v is neither", where, key, raw)
	case playabilityColdStartsKey:
		if n, ok := wholeNumber(raw); ok && isWholeNumber(raw) && n >= 0 {
			return nil
		}
		return fmt.Errorf("%s: %s must be a whole number of keyframes, 0 or more (0 turns cold starts off): "+
			"%#v is not one", where, key, raw)
	}
	return nil
}

// validatePlayability refuses a negative playability_cold_starts on a resolved profile: a
// Profile assembled in Go never went through checkPlayabilityValue.
func (p Profile) validatePlayability() error {
	if n := p.PlayabilityColdStartCount(); n < 0 {
		return fmt.Errorf("%s %d must be 0 or more (0 turns cold starts off)", playabilityColdStartsKey, n)
	}
	return nil
}

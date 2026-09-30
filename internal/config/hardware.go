package config

import "fmt"

// hwFallbackKey is the one place the fallback key is spelled.
const hwFallbackKey = "hw_fallback"

// The values of hw_fallback. See docs/design/hardware.md#fallback for why skip is the default.
const (
	// HWFallbackSkip: a job whose hardware encoder is missing or fails is encoded by no
	// other encoder, and the source stays as it is.
	HWFallbackSkip = "skip"
	// HWFallbackSoftware: such a job is encoded by the software encoder of the same codec.
	HWFallbackSoftware = "software"
)

// HWFallbackMode is this root's resolved hw_fallback: the value it carries, or skip.
func (p Profile) HWFallbackMode() string {
	if p.HWFallback == "" {
		return HWFallbackSkip
	}
	return p.HWFallback
}

// validateHWFallback refuses any value but the two this build knows. "" is a Profile
// assembled in Go, which means the default.
func validateHWFallback(v string) error {
	switch v {
	case "", HWFallbackSkip, HWFallbackSoftware:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)",
		hwFallbackKey, v, HWFallbackSkip, HWFallbackSoftware)
}

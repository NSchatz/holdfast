package config

import "fmt"

// dolbyVisionP7Key is the one place the Dolby Vision profile 7 key is spelled.
const dolbyVisionP7Key = "dolby_vision_p7"

// The values of dolby_vision_p7. See docs/design/dynamic-hdr.md#profile-7.
const (
	// DolbyVisionP7Skip: a Dolby Vision profile 7 source is skipped, exactly as every Dolby
	// Vision source was before dynamic HDR was carried. The default (brief I5: a new
	// transformation is off until configured).
	DolbyVisionP7Skip = "skip"
	// DolbyVisionP7Convert: a profile 7 source is converted to profile 8.1 before the encode
	// (dovi_tool mode 2, the enhancement layer discarded) and its converted RPU carried, on the
	// cpu encoder only and only where every dynamic-HDR gate passes.
	DolbyVisionP7Convert = "convert"
)

// DolbyVisionP7Mode is this root's resolved dolby_vision_p7: the value it carries, or skip.
func (p Profile) DolbyVisionP7Mode() string {
	if p.DolbyVisionP7 == "" {
		return DolbyVisionP7Skip
	}
	return p.DolbyVisionP7
}

// validateDolbyVisionP7 refuses any value but the two this build knows. "" is a Profile
// assembled in Go, which means the default.
func validateDolbyVisionP7(v string) error {
	switch v {
	case "", DolbyVisionP7Skip, DolbyVisionP7Convert:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)",
		dolbyVisionP7Key, v, DolbyVisionP7Skip, DolbyVisionP7Convert)
}

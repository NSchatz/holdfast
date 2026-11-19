package config

import "fmt"

// keepBitDepthKey is the one place the bit-depth key is spelled.
//
// keep_bit_depth changes one thing: how `pixel_format: auto` derives a job's output depth.
// Unset or false (the default), it floors the depth at 10, as it always has, so an 8-bit
// source becomes 10-bit (HEVC Main 10 under libx265). True keeps the source's own depth, so
// an 8-bit source stays 8-bit (HEVC Main) and a 10- or 12-bit one keeps its depth exactly as
// the floor does. It never lowers a depth: a 10-bit HDR10 or HLG source is a 10-bit plan
// either way. See docs/design/encode-plan.md#bit-depth for the trade-off.
const keepBitDepthKey = "keep_bit_depth"

// BitDepthKept reports whether this root keeps its sources' bit depth (keep_bit_depth),
// defaulting to false - the 10-bit floor - when unset.
func (p Profile) BitDepthKept() bool { return p.KeepBitDepth != nil && *p.KeepBitDepth }

// validateKeepBitDepth refuses keep_bit_depth beside a forced pixel_format on the same
// resolved profile. The key only changes how `pixel_format: auto` derives a depth, and a
// forced format derives nothing, so the pair would ask for two different depths and one of
// them would be silently ignored: an operator who wrote `pixel_format: yuv420p10le` and
// `keep_bit_depth: true` gets told which of the two wins rather than finding out from the
// files. (An encode profile's own pixel_format, which applies to the files its match selects,
// names the format outright for those files and wins there; docs/profiles.md says so.)
func (p Profile) validateKeepBitDepth() error {
	if !p.BitDepthKept() || p.PixelFormatAuto() {
		return nil
	}
	return fmt.Errorf("%s: true keeps the source's bit depth where pixel_format is auto, and pixel_format is %q, "+
		"which names the output format outright: set pixel_format: auto, or remove %s",
		keepBitDepthKey, p.PixelFormat, keepBitDepthKey)
}

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

// hwDecodeKey is the one place the hardware decode key is spelled.
const hwDecodeKey = "hw_decode"

// The values of hw_decode. See docs/design/hardware.md#decode.
const (
	// HWDecodeSoftware: the source is decoded by ffmpeg's software decoders, as every job
	// always was. The default (brief I5: a new transformation is off until configured).
	HWDecodeSoftware = "software"
	// HWDecodeHardware: a job whose encoder is a hardware one decodes its source on that
	// encoder's vendor hardware (CUDA for NVENC, VAAPI on the render node for VAAPI, QSV and
	// AMF), with every frame downloaded to system memory before the filters and the encoder
	// see it. A job encoded in software decodes in software.
	HWDecodeHardware = "hardware"
)

// HWDecodeMode is this root's resolved hw_decode: the value it carries, or software.
func (p Profile) HWDecodeMode() string {
	if p.HWDecode == "" {
		return HWDecodeSoftware
	}
	return p.HWDecode
}

// validateHWDecode refuses any value but the two this build knows. "" is a Profile
// assembled in Go, which means the default.
func validateHWDecode(v string) error {
	switch v {
	case "", HWDecodeSoftware, HWDecodeHardware:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)",
		hwDecodeKey, v, HWDecodeSoftware, HWDecodeHardware)
}

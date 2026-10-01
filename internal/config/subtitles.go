package config

import "fmt"

// subtitleSidecarsKey is the one place the subtitle sidecar key is spelled.
const subtitleSidecarsKey = "subtitle_sidecars"

// The values of subtitle_sidecars. See docs/design/subtitles.md#sidecars.
const (
	// SubtitleSidecarsOff: no sidecar is written, no subtitle is probed for one, and a job
	// does exactly what it did before the key existed. The default (brief I5: a new
	// transformation is off until configured).
	SubtitleSidecarsOff = "off"
	// SubtitleSidecarsText: every carried SubRip, ASS and WebVTT stream is also copied, in
	// its own format, to a `<name>.<lang>[.forced].<ext>` file beside the replacement once
	// the swap has committed. The embedded streams are carried as before.
	SubtitleSidecarsText = "text"
)

// SubtitleSidecarsMode is this root's resolved subtitle_sidecars: the value it carries, or
// off.
func (p Profile) SubtitleSidecarsMode() string {
	if p.SubtitleSidecars == "" {
		return SubtitleSidecarsOff
	}
	return p.SubtitleSidecars
}

// validateSubtitleSidecars refuses any value but the two this build knows. "" is a Profile
// assembled in Go, which means the default.
func validateSubtitleSidecars(v string) error {
	switch v {
	case "", SubtitleSidecarsOff, SubtitleSidecarsText:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)",
		subtitleSidecarsKey, v, SubtitleSidecarsOff, SubtitleSidecarsText)
}

package config

import (
	"fmt"

	"github.com/NSchatz/holdfast/internal/crop"
)

// cropKey is the one place the crop key is spelled.
const cropKey = "crop"

// CropMode is this root's resolved crop: the value it carries, or off. See
// docs/design/crop.md.
func (p Profile) CropMode() string {
	if p.Crop == "" {
		return crop.Off
	}
	return p.Crop
}

// CropEnabled reports whether this root asks for its sources' black bars to be detected and
// cut away.
func (p Profile) CropEnabled() bool { return p.CropMode() == crop.Auto }

// cropNotices is what Notices says about a configuration that crops: one statement per root
// that sets `crop: auto`, naming the root, and nothing at all for the shipped default. It is
// read off the RESOLVED profiles, as deinterlaceNotices is and for its reason.
func (c *Config) cropNotices() []string {
	roots := c.RootProfiles()
	if len(roots) == 0 {
		return cropNotice(c.TopLevelProfile(), "")
	}
	var n []string
	for _, r := range roots {
		n = append(n, cropNotice(r.Profile, r.Clean)...)
	}
	return n
}

// cropNotice is the statement one resolved profile earns. root names the tree it is about; ""
// names none, which is what a configuration with no roots at all gets.
func cropNotice(p Profile, root string) []string {
	if !p.CropEnabled() {
		return nil
	}
	where := ""
	if root != "" {
		where = "library root " + root + ": "
	}
	return []string{where + cropKey + " is " + crop.Auto + " - THE REPLACEMENT IS NO LONGER THE SAME CONTENT AS " +
		"THE SOURCE where a file is cropped. Each source's black bars are sampled and cut away before it is " +
		"encoded, only where the samples agree and the area removed is black on every frame, and a Dolby Vision " +
		"source only to its RPU's own active area, zeroed and gated; the rows and columns removed cannot be recovered from the replacement, and the swap " +
		"deletes the original. Every gate still applies at full strength - the perceptual gate scores the " +
		"encode against the source put through the SAME crop, and a crop gate holds the output to its declared " +
		"size and the removed area to black. Set " + cropKey + ": " + crop.Off + " (the default) to keep every frame whole."}
}

// validateCrop refuses any value but the two this build knows. "" is a Profile assembled in
// Go, which means the default.
func validateCrop(v string) error {
	switch v {
	case "", crop.Off, crop.Auto:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)", cropKey, v, crop.Off, crop.Auto)
}

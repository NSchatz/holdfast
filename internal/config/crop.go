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

// validateCrop refuses any value but the two this build knows. "" is a Profile assembled in
// Go, which means the default.
func validateCrop(v string) error {
	switch v {
	case "", crop.Off, crop.Auto:
		return nil
	}
	return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)", cropKey, v, crop.Off, crop.Auto)
}

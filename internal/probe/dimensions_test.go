package probe

import "testing"

// TestParseDimensions_ADolbyVisionStreamsTrailingSeparatorIsASize is the regression: ffprobe's
// csv form prints `320x240x` for a stream carrying side data (every Dolby Vision stream's DOVI
// configuration record), and Dimensions read that as not established, so a cropped Dolby
// Vision output's size could never be checked.
func TestParseDimensions_ADolbyVisionStreamsTrailingSeparatorIsASize(t *testing.T) {
	for in, want := range map[string][2]int{"320x240": {320, 240}, "320x240x": {320, 240}, "1920x1080xx\n": {1920, 1080}} {
		w, h, ok := parseDimensions(in)
		if !ok || w != want[0] || h != want[1] {
			t.Errorf("parseDimensions(%q) = %d, %d, %v; want %v", in, w, h, ok, want)
		}
	}
	for _, in := range []string{"", "x", "320", "320x", "x240", "320x240x5", "320x240xa", "0x240", "320x0", "-1x240", "axb"} {
		if _, _, ok := parseDimensions(in); ok {
			t.Errorf("parseDimensions(%q) established a size", in)
		}
	}
}

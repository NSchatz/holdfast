// Package hdr ports the bash transcoder's colour/HDR classification and metadata
// extraction (media/transcoder/transcode.sh, HOMELAB-MEDIA-2-HDR) to Go. A generic
// libx265 re-encode does NOT auto-carry HDR/colour signalling: HDR10 static
// metadata (SMPTE ST 2086 mastering-display + MaxCLL/FALL) and the primaries/
// transfer/matrix tags are dropped unless passed explicitly, and Dolby Vision RPUs
// / HDR10+ dynamic metadata cannot be preserved by a generic ffmpeg+libx265
// pipeline at all (they need an external RPU toolchain — out of scope). So this
// package DETECTS DV/HDR10+ (the engine skips them) and derives the args to
// PROPAGATE colour tags + HDR10 static metadata on everything else.
//
// The pure functions here (ClassFrom, MasterDisplay, MaxCLL, StaticMetadataIncomplete)
// take already-probed strings and are deterministic — no ffmpeg/ffprobe dependency,
// so the non-HEVC DV/HDR10+ sources that trigger the real-world SKIP (which cannot be
// synthesized with ffmpeg+libx265 — DV needs an RPU toolchain, HDR10+ needs a
// libx265 built with libhdr10plus) can still be unit-tested via synthetic probe
// strings, mirroring the bash suite's approach.
package hdr

import (
	"regexp"
	"strconv"
	"strings"
)

// Classification values returned by ClassFrom/Classify.
const (
	ClassDV        = "dv"
	ClassHDR10Plus = "hdr10plus"
	ClassHDR10     = "hdr10"
	ClassOther     = "other"
)

// ClassFrom is the pure classifier — a faithful port of the bash hdr_class_from.
// Args: codecTag (stream codec_tag_string), flatSideData (frame-level side data
// concatenated with stream-level side data, ffprobe flat format), colorTransfer
// (the video stream's color_transfer tag).
//
//   - dv:        codec tag dvhe/dvh1/dvav/dav1 (case-insensitive), OR a "DOVI
//     configuration record" / "Dolby Vision" side-data block (the DOVI
//     config record is stream-level, an ISOBMFF dvcC/dvvC box).
//   - hdr10plus: an "SMPTE2094-40" / "HDR10+" / "HDR Dynamic Metadata" side-data
//     block.
//   - hdr10:     color_transfer == smpte2084 (PQ), OR a "Mastering display
//     metadata" side-data block.
//   - other:     none of the above.
func ClassFrom(codecTag, flatSideData, colorTransfer string) string {
	switch strings.ToLower(codecTag) {
	case "dvhe", "dvh1", "dvav", "dav1":
		return ClassDV
	}
	if strings.Contains(flatSideData, "DOVI configuration record") || strings.Contains(flatSideData, "Dolby Vision") {
		return ClassDV
	}
	if strings.Contains(flatSideData, "SMPTE2094-40") || strings.Contains(flatSideData, "HDR10+") || strings.Contains(flatSideData, "HDR Dynamic Metadata") {
		return ClassHDR10Plus
	}
	if colorTransfer == "smpte2084" {
		return ClassHDR10
	}
	if strings.Contains(flatSideData, "Mastering display metadata") {
		return ClassHDR10
	}
	return ClassOther
}

// hdrField pulls one flat side-data field's value by its trailing name (e.g.
// red_x, max_content) — a port of the bash `_hdr_field` (grep -m1 + sed strip
// quotes). Returns "" if the field is absent. Mirrors grep -m1: first match wins.
func hdrField(flat, name string) string {
	for _, line := range strings.Split(flat, "\n") {
		idx := strings.Index(line, "."+name+"=")
		if idx < 0 {
			continue
		}
		// Must be the trailing field name: what follows "." + name is exactly "=".
		val := line[idx+len(name)+2:]
		val = strings.TrimSuffix(strings.TrimPrefix(val, `"`), `"`)
		return val
	}
	return ""
}

var rationalRe = regexp.MustCompile(`^(-?[0-9]+)/(-?[0-9]+)$`)
var plainNumRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// hdrScaled scales one ffprobe rational ("34000/50000") or plain number to the
// integer units x265's master-display expects (chroma coords x50000, luminance
// x10000) — a port of the bash `_hdr_scaled` (awk rounding: value*scale+0.5,
// truncated). Empty input yields empty output ("" -> not "0").
func hdrScaled(r string, scale int) string {
	if r == "" {
		return ""
	}
	if m := rationalRe.FindStringSubmatch(r); m != nil {
		num, errN := strconv.ParseFloat(m[1], 64)
		den, errD := strconv.ParseFloat(m[2], 64)
		if errN != nil || errD != nil || den == 0 {
			return ""
		}
		v := int64((num/den)*float64(scale) + 0.5)
		return strconv.FormatInt(v, 10)
	}
	if plainNumRe.MatchString(r) {
		f, err := strconv.ParseFloat(r, 64)
		if err != nil {
			return ""
		}
		v := int64(f*float64(scale) + 0.5)
		return strconv.FormatInt(v, 10)
	}
	return ""
}

// MasterDisplay builds the x265 master-display string from flat side-data, or ""
// when the source does not carry a COMPLETE mastering-display block (all ten
// fields required — a partial block would mis-signal the display volume; a
// half-signalled display is worse than none). Port of bash hdr_master_display.
func MasterDisplay(flat string) string {
	gx := hdrScaled(hdrField(flat, "green_x"), 50000)
	gy := hdrScaled(hdrField(flat, "green_y"), 50000)
	bx := hdrScaled(hdrField(flat, "blue_x"), 50000)
	by := hdrScaled(hdrField(flat, "blue_y"), 50000)
	rx := hdrScaled(hdrField(flat, "red_x"), 50000)
	ry := hdrScaled(hdrField(flat, "red_y"), 50000)
	wx := hdrScaled(hdrField(flat, "white_point_x"), 50000)
	wy := hdrScaled(hdrField(flat, "white_point_y"), 50000)
	lmax := hdrScaled(hdrField(flat, "max_luminance"), 10000)
	lmin := hdrScaled(hdrField(flat, "min_luminance"), 10000)
	for _, v := range []string{gx, gy, bx, by, rx, ry, wx, wy, lmax, lmin} {
		if v == "" {
			return ""
		}
	}
	return "G(" + gx + "," + gy + ")B(" + bx + "," + by + ")R(" + rx + "," + ry + ")WP(" + wx + "," + wy + ")L(" + lmax + "," + lmin + ")"
}

// MaxCLL builds the x265 max-cll string ("maxCLL,maxFALL") from flat side-data, or
// "" when either field is missing. Port of bash hdr_max_cll.
func MaxCLL(flat string) string {
	mc := hdrField(flat, "max_content")
	fa := hdrField(flat, "max_average")
	if mc == "" || fa == "" {
		return ""
	}
	return mc + "," + fa
}

// StaticMetadataIncomplete is the fail-safe predicate — true when an HDR10 source
// carries an HDR10 static-metadata block (mastering-display SMPTE ST 2086, or
// content-light MaxCLL/FALL) that cannot be fully parsed. Re-encoding such a file
// would silently drop metadata that IS present, so the caller SKIPS it rather than
// blind-encoding. A source with NO such block has nothing to lose (not flagged); a
// block that parses (including an all-zero MaxCLL, which ffprobe emits as 0/0)
// proceeds. Port of bash hdr10_static_metadata_incomplete.
func StaticMetadataIncomplete(flat string) bool {
	if strings.Contains(flat, "Mastering display metadata") && MasterDisplay(flat) == "" {
		return true
	}
	if strings.Contains(flat, "Content light level metadata") && MaxCLL(flat) == "" {
		return true
	}
	return false
}

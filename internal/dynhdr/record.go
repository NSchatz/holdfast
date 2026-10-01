package dynhdr

import (
	"strconv"
	"strings"
)

// Record is a Dolby Vision configuration record (ETSI GS CCM 001 / Dolby's "Dolby Vision
// Streams Within the ISO Base Media File Format", the dvcC/dvvC box ffprobe reports as a
// "DOVI configuration record"), as read off ffprobe's flat side-data output.
type Record struct {
	// Present reports that a record was found at all.
	Present bool
	// Readable reports that every field below was found and parsed. A present record that
	// is not readable is never read as any profile.
	Readable bool
	// Profile is dv_profile, Level dv_level, and CompatID dv_bl_signal_compatibility_id.
	Profile, Level, CompatID int
	// RPU, EL and BL are rpu_present_flag, el_present_flag and bl_present_flag.
	RPU, EL, BL bool
}

// RecordFrom reads the first DOVI configuration record in flat, ffprobe's `flat=s=.` output
// of a file's side data (the stream-level list the guards and the fidelity gate already read).
// A record whose fields cannot all be parsed is reported Present and not Readable.
func RecordFrom(flat string) Record {
	fields := map[string]string{}
	prefix := ""
	for _, line := range strings.Split(flat, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		if prefix == "" {
			if strings.HasSuffix(k, ".side_data_type") && v == DoviRecordType {
				prefix = strings.TrimSuffix(k, "side_data_type")
			}
			continue
		}
		if rest, found := strings.CutPrefix(k, prefix); found && !strings.Contains(rest, ".") {
			fields[rest] = v
		}
	}
	if prefix == "" {
		return Record{}
	}
	r := Record{Present: true}
	ints := map[string]*int{"dv_profile": &r.Profile, "dv_level": &r.Level, "dv_bl_signal_compatibility_id": &r.CompatID}
	for name, dst := range ints {
		n, err := strconv.Atoi(fields[name])
		if err != nil || n < 0 {
			return r
		}
		*dst = n
	}
	flags := map[string]*bool{"rpu_present_flag": &r.RPU, "el_present_flag": &r.EL, "bl_present_flag": &r.BL}
	for name, dst := range flags {
		switch fields[name] {
		case "1":
			*dst = true
		case "0":
		default:
			return r
		}
	}
	r.Readable = true
	return r
}

// HasHDR10Plus reports whether flat side data names HDR10+ dynamic metadata, by the same
// three spellings the HDR classifier has always matched (internal/hdr.ClassFrom), so a source
// that classifies as HDR10+ there is one this reads as carrying it - including a Dolby Vision
// source, which that classifier names Dolby Vision first.
func HasHDR10Plus(flat string) bool {
	return strings.Contains(flat, "SMPTE2094-40") || strings.Contains(flat, "HDR10+") ||
		strings.Contains(flat, "HDR Dynamic Metadata")
}

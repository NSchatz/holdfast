package dynhdr

import "fmt"

// VBV is the ceiling a Dolby Vision encode runs under, in libx265's units (kbit/s and kbit).
//
// x265 refuses to code a Dolby Vision RPU without one: "Dolby Vision requires VBV settings to
// enable HRD." (exit 183 on the pinned ffmpeg's x265 4.2, verify-streams-hdr.md claim 3,
// re-observed 2026-10-01). holdfast's encodes are quality-targeted, so the ceiling is not a
// rate the operator chose; it is DERIVED, never guessed: the maximum bit rate and CPB size of
// the lowest HEVC level whose luma picture size and luma sample rate admit the source, high
// tier where the level has one (levels 4 and up) and main tier below it. That is the largest
// ceiling a decoder conforming to that level is required to accept, so it constrains peaks
// only where a conforming stream must be constrained, and leaves CRF everywhere else.
type VBV struct {
	MaxrateKbps, BufsizeKbit int
	// Level is the HEVC level the ceiling was taken from, as x265 names it ("5.1").
	Level string
}

// hevcLevel is one row of the HEVC level limits (ITU-T H.265 Annex A, Table A.8), as x265
// carries them in its own level table: source/encoder/level.cpp, `LevelSpec levels[]`,
// https://bitbucket.org/multicoreware/x265_git/raw/master/source/encoder/level.cpp , read
// 2026-10-01. Columns: MaxLumaPs, MaxLumaSr, MaxBR main and high (kbit/s), MaxCPB main and
// high (kbit); 0 where the level has no high tier. Levels above 6.2 are left out: no Dolby
// Vision level reaches them (dv_levels in libavcodec/dovi_rpuenc.c at 5d4d3bdc61 stops at
// 7680x4320 at 120 Hz, which is level 6.2), and a source past it is refused by name.
type hevcLevel struct {
	name                 string
	maxLumaPs, maxLumaSr uint64
	brMain, brHigh       int
	cpbMain, cpbHigh     int
}

var hevcLevels = []hevcLevel{
	{"1", 36864, 552960, 128, 0, 350, 0},
	{"2", 122880, 3686400, 1500, 0, 1500, 0},
	{"2.1", 245760, 7372800, 3000, 0, 3000, 0},
	{"3", 552960, 16588800, 6000, 0, 6000, 0},
	{"3.1", 983040, 33177600, 10000, 0, 10000, 0},
	{"4", 2228224, 66846720, 12000, 30000, 12000, 30000},
	{"4.1", 2228224, 133693440, 20000, 50000, 20000, 50000},
	{"5", 8912896, 267386880, 25000, 100000, 25000, 100000},
	{"5.1", 8912896, 534773760, 40000, 160000, 40000, 160000},
	{"5.2", 8912896, 1069547520, 60000, 240000, 60000, 240000},
	{"6", 35651584, 1069547520, 60000, 240000, 60000, 240000},
	{"6.1", 35651584, 2139095040, 120000, 480000, 120000, 480000},
	{"6.2", 35651584, 4278190080, 240000, 800000, 240000, 800000},
}

// VBVFor is the ceiling of the lowest level admitting a width x height picture at num/den
// frames per second. Anything it cannot place - a dimension or rate that is not positive, or
// a source past level 6.2 - is refused, never given a typical value.
func VBVFor(width, height int, rate Rate) (VBV, error) {
	if width <= 0 || height <= 0 || !rate.Valid() {
		return VBV{}, fmt.Errorf("no HEVC level can be chosen for %dx%d at %s frames per second", width, height, rate)
	}
	ps := uint64(width) * uint64(height)
	for _, l := range hevcLevels {
		// The sample rate is compared exactly: ps*num <= MaxLumaSr*den.
		if ps > l.maxLumaPs || ps*uint64(rate.Num) > l.maxLumaSr*uint64(rate.Den) {
			continue
		}
		if l.brHigh > 0 {
			return VBV{MaxrateKbps: l.brHigh, BufsizeKbit: l.cpbHigh, Level: l.name}, nil
		}
		return VBV{MaxrateKbps: l.brMain, BufsizeKbit: l.cpbMain, Level: l.name}, nil
	}
	return VBV{}, fmt.Errorf("%dx%d at %s frames per second is beyond HEVC level 6.2, the highest a Dolby Vision "+
		"level reaches", width, height, rate)
}

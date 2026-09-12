package config

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/NSchatz/holdfast/internal/encoder"
)

// EncodeProfile is one entry of encode_profiles: a match over the source plus
// overrides for the transcode settings.
//
// It is NOT the per-library Profile in profile.go, and the two names are kept apart
// because the things are: a library Profile is one ROOT's resolved knobs, chosen by
// where a file lives and carrying the gates that decide whether it may be destroyed,
// while this is chosen by a PATTERN over the source path and may only change what the
// encoder produces. A job is decided by both (see TranscodeIn).
//
// Every override is a POINTER, and that is load-bearing rather than tidy. Zero is
// a legal, meaningful value for crf (lossless) and for bitrate_kbps (the quality
// target), so "the profile did not mention this key" and "the profile set it to
// zero" must stay distinguishable - the same zero-vs-absent problem the layered
// loader solves for the top-level keys, one level down. A nil field keeps the
// inherited value; a non-nil field replaces it, zero included.
type EncodeProfile struct {
	// Name identifies the profile. It is recorded on the terminal ledger row of
	// every job the profile supplied settings for, which is what makes "what ran"
	// answerable once more than one set of settings exists. It must be non-empty
	// and unique across the list.
	Name string `yaml:"name"`

	// Match selects the sources this profile applies to. It is a glob over the
	// source's path:
	//
	//   - a pattern with NO "/" is matched against the file's BASENAME, so
	//     `*.mkv` selects every mkv wherever it lives;
	//   - a pattern containing "/" is matched against the WHOLE path, segment by
	//     segment, where `*` and `?` and `[...]` are path.Match's and do not cross
	//     a separator, and `**` matches zero or more whole segments. So
	//     `**/4K/**` selects everything under any directory named 4K, and
	//     `/srv/media/films/*.mkv` selects one directory's mkvs.
	//
	// An EMPTY match selects every source. That is the catch-all profile - "and
	// everything else gets these settings" - and it is only ever reached by the
	// sources no earlier profile matched, since the first match wins.
	Match string `yaml:"match"`

	Encoder      *string `yaml:"encoder"`
	CRF          *int    `yaml:"crf"`
	Preset       *string `yaml:"preset"`
	PixelFormat  *string `yaml:"pixel_format"`
	ContainerExt *string `yaml:"container_ext"`
	BitrateKbps  *int    `yaml:"bitrate_kbps"`
}

// Transcode is ONE JOB's effective encode settings: the profile of the library root
// the file was enumerated under, overlaid with the first matching encode profile's
// overrides, plus the name of the encode profile that supplied them.
//
// It exists so that nothing downstream reads a run-global setting where a per-job
// one is meant. The engine's already-at-target-codec skip and the output-codec
// acceptance check are both decided from THIS value's encoder, and the encoder's
// argument construction is built from it too - so a job whose profile targets AV1
// is skipped, encoded, accepted and rejected against av1 while its neighbour under
// the inherited settings is judged against hevc, in the same run.
//
// It carries exactly what the encoder PRODUCES and nothing that decides whether a
// source may be destroyed. The gates - the VMAF floors, the savings floor, the
// bitrate floor, the hardlink guard - stay on the library root's Profile, read from
// there, and no encode profile can reach them.
type Transcode struct {
	// Profile is the name of the ENCODE profile that supplied these settings, empty
	// when nothing matched and the root's own values stood. It is what a terminal
	// ledger row records beside the root that decided the file.
	Profile string

	Encoder      string
	CRF          int
	Preset       string
	PixelFormat  string
	ContainerExt string
	BitrateKbps  int
}

// ContainerMatchesSource reports whether ContainerExt is the "match the source"
// sentinel ("source"/"auto"/"") rather than a forced extension.
func (t Transcode) ContainerMatchesSource() bool {
	switch t.ContainerExt {
	case "source", "auto", "":
		return true
	default:
		return false
	}
}

// PixelFormatAuto reports whether PixelFormat is the "derive per source" sentinel
// ("auto"/"") rather than a forced pixel format.
func (t Transcode) PixelFormatAuto() bool {
	switch t.PixelFormat {
	case "auto", "":
		return true
	default:
		return false
	}
}

// TargetsBitrate reports whether this job encodes under a target-bitrate rate
// control rather than the quality target. It is the ONE reading of "is 0 the
// disabled sentinel" for bitrate_kbps, so the encoder's argument construction and
// the startup notice cannot disagree about what the setting means.
func (t Transcode) TargetsBitrate() bool { return t.BitrateKbps > 0 }

// BaseTranscode is one library root's resolved profile as a job's settings: what every
// file under that root gets when no encode profile matches it, and what a matching
// encode profile's overrides are laid over.
//
// bitrate_kbps comes from the top level because it is not a per-root knob: a library
// root's profile may not carry it, so there is no per-root value for this to read.
func (c *Config) BaseTranscode(prof Profile) Transcode {
	return Transcode{
		Encoder:      prof.Encoder,
		CRF:          prof.CRF,
		Preset:       prof.Preset,
		PixelFormat:  prof.PixelFormat,
		ContainerExt: prof.ContainerExt,
		BitrateKbps:  c.BitrateKbps,
	}
}

// TranscodeIn resolves the effective settings for one source path under one library
// root's profile: that profile overlaid with the FIRST matching encode profile's
// overrides, or the profile alone when nothing matches.
//
// This is the INNERMOST layer of the four, and it is innermost because it is the most
// specific: the built-in default, then the top-level configuration, then the profile of
// the root the file lives under, then the encode profile whose pattern selected this
// file by name. An operator reading their configuration top to bottom meets them in
// that order, and the last one to mention a key decides it.
//
// First match wins and the search stops there, so a later matching encode profile has
// no effect on that job - which is what makes the list an ordered decision an operator
// can read top to bottom rather than a set of rules that combine.
//
// A match pattern that cannot be parsed is refused by Validate before the engine is
// ever built, so a parse failure here can only mean a profile that never went
// through Validate; it is treated as NOT MATCHING, which resolves the job to the
// root's own settings rather than to settings nobody wrote.
func (c *Config) TranscodeIn(prof Profile, sourcePath string) Transcode {
	t := c.BaseTranscode(prof)
	for i := range c.EncodeProfiles {
		p := &c.EncodeProfiles[i]
		ok, err := MatchSource(p.Match, sourcePath)
		if err != nil || !ok {
			continue
		}
		t.Profile = p.Name
		if p.Encoder != nil {
			t.Encoder = *p.Encoder
		}
		if p.CRF != nil {
			t.CRF = *p.CRF
		}
		if p.Preset != nil {
			t.Preset = *p.Preset
		}
		if p.PixelFormat != nil {
			t.PixelFormat = *p.PixelFormat
		}
		if p.ContainerExt != nil {
			t.ContainerExt = *p.ContainerExt
		}
		if p.BitrateKbps != nil {
			t.BitrateKbps = *p.BitrateKbps
		}
		return t
	}
	return t
}

// MatchSource reports whether pattern selects sourcePath, and returns
// path.ErrBadPattern for a pattern that cannot be parsed.
//
// A malformed pattern is an ERROR and never a silent non-match: a profile whose
// match nobody can parse is a profile that would quietly stop applying, and the
// settings the operator wrote would be replaced by ones they did not.
func MatchSource(pattern, sourcePath string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	if !strings.Contains(pattern, "/") {
		return path.Match(pattern, filepath.Base(sourcePath))
	}
	name := strings.Split(filepath.ToSlash(filepath.Clean(sourcePath)), "/")
	return matchSegments(strings.Split(pattern, "/"), name)
}

// ValidateMatch reports whether a match pattern can be parsed at all. It is what
// Validate calls, so a malformed pattern is a startup refusal naming the profile
// rather than a rule that silently never fires.
func ValidateMatch(pattern string) error {
	if pattern == "" {
		return nil
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// matchSegments matches a segmented pattern against a segmented path, with `**`
// matching zero or more whole segments. It is the ordinary single-backtrack star
// match (one remembered `**` position), so its cost is linear in the path even for
// a pattern carrying several of them - an operator-authored pattern must not be
// able to make the scan quadratic over a library.
func matchSegments(pat, name []string) (bool, error) {
	px, nx := 0, 0
	starPx, starNx := -1, 0
	for nx < len(name) {
		if px < len(pat) && pat[px] == "**" {
			starPx, starNx = px, nx
			px++
			continue
		}
		if px < len(pat) {
			ok, err := path.Match(pat[px], name[nx])
			if err != nil {
				return false, err
			}
			if ok {
				px++
				nx++
				continue
			}
		}
		if starPx >= 0 {
			starNx++
			px, nx = starPx+1, starNx
			continue
		}
		return false, validateSegments(pat)
	}
	for px < len(pat) && pat[px] == "**" {
		px++
	}
	if px != len(pat) {
		return false, validateSegments(pat)
	}
	return true, nil
}

// validateSegments re-asks the parse question on the way OUT of a failed match.
// path.Match stops at the first segment that does not match, so a syntax error in a
// later segment would otherwise be reported for the paths that reach it and not for
// the ones that do not - the same pattern answering "no" for one file and "that is
// malformed" for another.
func validateSegments(pat []string) error {
	for _, seg := range pat {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// validateEncoderKey refuses an encoder key this build does not ship. It is spelled
// here rather than inline so an encode profile cannot reach the engine carrying a key
// the library profile's own check (Profile.validate) would have refused.
func validateEncoderKey(key string) error {
	if key == "" {
		return nil
	}
	if _, ok := encoder.Lookup(key); !ok {
		return fmt.Errorf("encoder %q is not supported (known: %v)", key, encoder.Known())
	}
	return nil
}

// validateProfiles applies the per-profile rules, naming the profile and the
// offending key and value in every refusal. A profile that reached here has already
// passed the unknown-key refusal in Load.
func (c *Config) validateProfiles() error {
	seen := make(map[string]int, len(c.EncodeProfiles))
	for i := range c.EncodeProfiles {
		p := &c.EncodeProfiles[i]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("encode_profiles[%d] has an empty name: a profile's name is what its jobs' ledger rows record, so it must be set", i)
		}
		if j, dup := seen[name]; dup {
			return fmt.Errorf("encode_profiles[%d] name %q duplicates encode_profiles[%d]: two profiles with one name make a ledger row ambiguous about which ran", i, name, j)
		}
		seen[name] = i

		if err := ValidateMatch(p.Match); err != nil {
			return fmt.Errorf("encode_profiles[%d] (%s) match %q cannot be parsed: %w", i, name, p.Match, err)
		}
		if p.Encoder != nil {
			if err := validateEncoderKey(*p.Encoder); err != nil {
				return fmt.Errorf("encode_profiles[%d] (%s) %w", i, name, err)
			}
		}
		if p.CRF != nil && (*p.CRF < 0 || *p.CRF > 51) {
			return fmt.Errorf("encode_profiles[%d] (%s) crf %d out of range (0-51)", i, name, *p.CRF)
		}
		if p.ContainerExt != nil && strings.ContainsAny(*p.ContainerExt, "./\\") {
			return fmt.Errorf("encode_profiles[%d] (%s) container_ext %q must be a bare extension (no dot or slash)", i, name, *p.ContainerExt)
		}
		if p.BitrateKbps != nil && *p.BitrateKbps < 0 {
			return fmt.Errorf("encode_profiles[%d] (%s) bitrate_kbps %d must be >= 0 (0 keeps the quality target)", i, name, *p.BitrateKbps)
		}
	}
	return nil
}

// EncoderInEffect is one encoder key an encode profile can override a root's with, and
// the name of the profile that asks for it.
//
// The name travels with the key because the capability preflight's whole value is
// operator-facing. "nvenc is not available on this host" sends someone to a config whose
// top-level encoder is `cpu`, and they then have to find which of their profiles asked
// for it; the name is the answer, and it is free here.
type EncoderInEffect struct {
	Key     string
	Profile string
}

// EncodeProfileEncoders is every encoder key an ENCODE PROFILE overrides a root's with,
// in configuration order, deduplicated, each carrying the name of the first profile that
// asks for it.
//
// It exists for the startup capability preflight, and it covers exactly the keys that
// preflight would otherwise miss: the root profiles' own encoders are walked separately
// (they inherit the top-level value, so that walk covers a configuration with no encode
// profiles at all). Validate confirms a key is KNOWN; only a run with ffmpeg in hand can
// confirm the encoder WORKS on this host, and an encode profile naming one is reached by
// every file its pattern selects - so a preflight blind to it lets a run start and then
// fail those files one at a time, hours in, or, for some hardware encoders, appear to
// succeed while writing nothing.
//
// Deduplicated because the check runs a real encode, and several profiles usually share
// one encoder. Empty when no encode profile overrides the encoder, which is every
// configuration written before they existed.
func (c *Config) EncodeProfileEncoders() []EncoderInEffect {
	var keys []EncoderInEffect
	seen := map[string]bool{}
	for i := range c.EncodeProfiles {
		p := &c.EncodeProfiles[i]
		if p.Encoder == nil || seen[*p.Encoder] {
			continue
		}
		seen[*p.Encoder] = true
		keys = append(keys, EncoderInEffect{Key: *p.Encoder, Profile: p.Name})
	}
	return keys
}

// bitrateInEffect reports whether any job this configuration can produce encodes
// under a target bitrate - the top-level setting, or any profile that overrides it
// with a positive value. It is what decides whether the startup notice is owed.
func (c *Config) bitrateInEffect() bool {
	if c.BitrateKbps > 0 {
		return true
	}
	for i := range c.EncodeProfiles {
		if b := c.EncodeProfiles[i].BitrateKbps; b != nil && *b > 0 {
			return true
		}
	}
	return false
}

// errBadPattern is re-exported so a caller can classify a match failure without
// importing path for one sentinel.
var errBadPattern = path.ErrBadPattern

// IsBadPattern reports whether err is the malformed-pattern error MatchSource and
// ValidateMatch return.
func IsBadPattern(err error) bool { return errors.Is(err, errBadPattern) }

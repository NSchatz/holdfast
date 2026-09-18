package config

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/NSchatz/holdfast/internal/downscale"
)

// The OUTPUT HEIGHT CEILING: `max_height`, and the acknowledgement that stands beside it.
//
// `max_height` is the one knob in this configuration that makes a replacement a different
// picture from its source. Every other knob decides how faithfully the same frames are
// re-encoded; this one throws pixels away on purpose, because for most libraries 4K-to-1080p
// is the largest single reclaim available and the operator may well want that trade. What is
// lost with it is the SAME-CONTENT claim, and this build says so rather than absorbing it:
// the key is off by default, `validate` states what setting it means, and a job that would
// downscale into a FINAL swap is refused unless the operator affirmed it separately.
//
// # Two keys, deliberately, and why one would not do
//
// `downscale_acknowledged` is not a second spelling of `max_height`. Setting a ceiling says
// what the replacement should look like; acknowledging says the operator accepts that the
// original is not coming back. Those are the same statement only where the undo window is
// open, and with `undo_window_hours: 0` - the shipped default - the swap is FINAL. A single
// key would make the irreversible case indistinguishable from the recoverable one, on a tool
// whose whole argument is that it never destroys a source it has not proved a replacement
// for.
//
// # Why an odd ceiling is refused rather than rounded
//
// Every pixel format this build encodes to is 4:2:0 chroma-subsampled, which has no
// representation for an odd dimension. `max_height: 1081` is therefore a height nothing here
// can target, and rounding it to 1080 or 1082 would be this build choosing which of two
// libraries the operator gets. The fail-safe rule says an ambiguous value is refused by name;
// so it is, at start, before a file is touched.
const (
	// maxHeightKey is the one place the ceiling key is spelled: knownKeys, defaultLayer,
	// profileKnobs, ruleKnobs, the raw-value refusals and the `when` refusal that redirects
	// an operator who wrote it in the wrong place all read it from here.
	maxHeightKey = "max_height"

	// downscaleAckKey is the one place the acknowledgement key is spelled.
	downscaleAckKey = "downscale_acknowledged"
)

// ErrMaxHeightUnusable is what every refusal of a `max_height` value wraps, so a caller can
// tell THIS refusal from any other configuration error without matching on message text.
//
// It is one sentinel for every unusable value - not a whole number, not positive, odd, or a
// present zero - because they are one decision to the caller: the configuration names a
// height this build cannot target, so nothing may start. The MESSAGE tells the operator
// which of the four they wrote and what to write instead; the type tells a program that the
// key is the ceiling.
var ErrMaxHeightUnusable = errors.New("max_height is not a height this build can target")

// noCeiling is how an unset `max_height` renders in `validate`'s printed configuration and
// in the digest. A rendered token rather than "0" because 0 is not a height and printing one
// would read as a ceiling of zero pixels; `off` is the same word `deinterlace` uses for the
// same state, so a reader meets one vocabulary and not two.
const noCeiling = "off"

// MaxHeightDefault is the ceiling this build SHIPS, read off the defaults layer rather than
// restated here. 0 means no ceiling, which is what every configuration written before this
// key existed resolves to.
//
// It is exported because the shipped documentation is graded against it: the anchored
// statement in the README has to say what the default IS, and a documentation check holding
// its own copy of that number would go on passing on the day the default moved. See
// internal/docscheck.
func MaxHeightDefault() int {
	v, ok := defaultLayer()[maxHeightKey]
	if !ok {
		return 0
	}
	n, _ := v.(int)
	return n
}

// MaxHeightCeiling is the output height ceiling in force for this profile, 0 when none is.
func (p Profile) MaxHeightCeiling() int { return p.MaxHeight }

// DownscaleAcknowledged reports whether this root's operator affirmed that a replacement
// with fewer pixels than its source is acceptable, defaulting to FALSE when unset - the
// fail-safe direction, and the value every configuration written before the key existed
// resolves to.
func (p Profile) DownscaleAcknowledged() bool {
	if p.DownscaleAck == nil {
		return false
	}
	return *p.DownscaleAck
}

// DownscaleFor resolves what happens to a source of these dimensions under this profile. It
// is THE reading of the ceiling: the guard that refuses an unacknowledged final swap, the
// encoder that applies the filter and the perceptual gate that scales the distorted back up
// all go through here, so no two of them can resolve one value differently.
func (p Profile) DownscaleFor(sourceWidth, sourceHeight int) downscale.Scale {
	return downscale.Resolve(p.MaxHeight, sourceWidth, sourceHeight)
}

// DownscaleEnabled reports whether this profile sets a ceiling at all. It says nothing about
// any particular source: a root with `max_height: 1080` has it enabled whether or not the
// file in front of it is taller than that.
func (p Profile) DownscaleEnabled() bool { return p.MaxHeight >= downscale.MinHeight }

// renderMaxHeight is the ceiling as `validate` prints it and as the digest reads it: the
// `off` token where there is none, and the height in pixels where there is. Two profiles
// that scale nothing must render alike, exactly as two that deinterlace nothing do.
func renderMaxHeight(v int) string {
	if v <= 0 {
		return noCeiling
	}
	return strconv.Itoa(v)
}

// validateCeiling refuses a `max_height` a Profile carries that this build will not target,
// at START and by name, so a configuration asking for something impossible never reaches a
// file.
//
// It is the BACKSTOP behind ceilingValue, which refuses the same values against what the
// operator actually WROTE and can therefore also refuse a present 0. This one runs over the
// resolved struct, where 0 and absent are one state, so it refuses only what remains
// impossible there: a negative height, and an odd one.
func (p Profile) validateCeiling() error {
	if p.MaxHeight == 0 {
		return nil
	}
	if p.MaxHeight < downscale.MinHeight {
		return fmt.Errorf("%w: %s %d is not one - a ceiling is a POSITIVE whole number of pixels, at "+
			"least %d. Remove the key to leave the resolution alone, which is the default",
			ErrMaxHeightUnusable, maxHeightKey, p.MaxHeight, downscale.MinHeight)
	}
	if p.MaxHeight%2 != 0 {
		return fmt.Errorf("%w: %s %d is ODD - every pixel format this build encodes to is 4:2:0 "+
			"chroma-subsampled and has no representation for an odd dimension. Rounding it would be "+
			"holdfast choosing between two libraries you did not ask for, so write an even height "+
			"(%d or %d)", ErrMaxHeightUnusable, maxHeightKey, p.MaxHeight, p.MaxHeight-1, p.MaxHeight+1)
	}
	return nil
}

// ceilingValue reads one `max_height` as the operator WROTE it, refusing every value this
// build cannot target, and it is the one function all three writing sites go through: the
// top level, a `library_roots` entry, and a rule.
//
// It is checked against the RAW value rather than through the weakly-typed decoder the other
// knobs go through, for the reason boundValue is: the decoder would read 1080.5 as 1080 and
// "1080" as 1080 without a word, and a ceiling the operator did not write is a library
// encoded to a resolution they did not choose. A PRESENT 0 is refused here and only here -
// past this point 0 is the no-ceiling sentinel and cannot be told from an absent key - and it
// is refused rather than read as "off" because `max_height` is a height, and S0089 already
// refuses a band bound of 0 on the same reasoning: no source sits below it, so it can only be
// a typo or an empty value.
func ceilingValue(where string, raw any) (int, error) {
	if !isWholeNumber(raw) {
		return 0, fmt.Errorf("%s: %w: %s is %#v, which is not a whole number of pixels. It is the OUTPUT "+
			"height ceiling - a source taller than it is scaled down to it, one at or below it is left "+
			"alone - and removing the key leaves every resolution alone, which is the default",
			where, ErrMaxHeightUnusable, maxHeightKey, raw)
	}
	v, ok := wholeNumber(raw)
	if !ok || v <= 0 {
		return 0, fmt.Errorf("%s: %w: %s is %#v, which is not a POSITIVE whole number of pixels. A ceiling "+
			"of zero pixels is not a picture, so this can only be a typo or an empty value - remove the key "+
			"to leave the resolution alone, which is the default", where, ErrMaxHeightUnusable, maxHeightKey, raw)
	}
	p := Profile{MaxHeight: v}
	if err := p.validateCeiling(); err != nil {
		return 0, fmt.Errorf("%s: %w", where, err)
	}
	return v, nil
}

// downscaleNotices is what Notices says about a configuration that sets a ceiling: one
// statement per root that sets one, naming the root and the height, and nothing at all for
// the shipped default.
//
// It is read off the RESOLVED profiles, and off each root's RULES as well, the same reading
// the deinterlace notice takes and for the same reason: the value that decides a file is its
// own root's, and a notice derived from the top level would announce a downscale to an
// operator whose roots all override it away, or stay silent for the one who wrote it inside a
// single entry. A rule counts because a rule that sets `max_height` downscales the band it
// admits, and an operator who wrote it in a rule is owed the same statement as one who wrote
// it on the root.
func (c *Config) downscaleNotices() []string {
	roots := c.RootProfiles()
	if len(roots) == 0 {
		return downscaleNotice(c.TopLevelProfile(), "")
	}
	var n []string
	for _, r := range roots {
		n = append(n, downscaleNotice(r.Profile, r.Clean)...)
	}
	return n
}

// downscaleNotice is the statement one resolved profile earns. root names the tree it is
// about; "" names none, which is what a configuration with no roots at all gets.
//
// The height it names is the root's own where the root sets one, and otherwise the highest a
// rule under it sets - the statement is about the key being IN FORCE for this tree, and one
// line per band would bury the sentence that matters under a list.
func downscaleNotice(p Profile, root string) []string {
	h := p.MaxHeight
	for _, r := range p.Rules {
		if v, ok := r.knob(maxHeightKey); ok && v > h {
			h = v
		}
	}
	if h < downscale.MinHeight {
		return nil
	}
	where := ""
	if root != "" {
		where = "library root " + root + ": "
	}
	ack := ""
	if !p.DownscaleAcknowledged() {
		ack = " " + downscaleAckKey + " is NOT set on this root, so while the undo window is disabled " +
			"(undo_window_hours: 0, the default) every file this key would scale is SKIPPED rather than " +
			"encoded: a swap is final there, and holdfast will not make an irreversible swap to fewer " +
			"pixels on the strength of one key. Set " + downscaleAckKey + ": true to accept that, or open " +
			"the undo window."
	}
	return []string{where + maxHeightKey + " is " + strconv.Itoa(h) + " - THE REPLACEMENT IS NO LONGER " +
		"THE SAME CONTENT AS THE SOURCE. Every file under this root taller than " + strconv.Itoa(h) +
		" pixels is scaled down to it before it is encoded, in the source's own aspect ratio: the pixels " +
		"the source carried above that height are gone from the replacement, they cannot be recovered " +
		"from it, and the swap deletes the original. Every gate still applies at full strength - the " +
		"perceptual gate scales the OUTPUT back up to the source's resolution and scores there, so it " +
		"measures what the downscale cost rather than hiding it - but a file decided under this key is a " +
		"smaller picture, not a re-encoded copy of what you had. Remove " + maxHeightKey +
		" (the default) to leave every resolution alone." + ack}
}

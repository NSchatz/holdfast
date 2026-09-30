package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/encoder"
)

// Resolution rules: an ordered, first-match list of per-band overrides inside ONE
// `library_roots` entry.
//
// The problem they exist for is that a threshold is one number for a 480p DVD rip and a
// 2160p remux alike. `min_bitrate_kbps: 2500` either excludes most of a 1080p library or
// lets SD files through that will bloat, and the same is true of the crf the encoder aims
// at and the savings floor the output is held to. A rule says "under this band of source
// heights, these three knobs are different".
//
// FIRST MATCH WINS AND NOTHING MERGES. The first rule whose `when` admits the file
// supplies every knob it names; every knob it does not name comes from the root's own
// resolved profile, and no later rule contributes anything at all. Merging would be the
// wrong reading on a tool that deletes sources: a file would be judged by a floor from one
// rule and encoded at a crf from another, and no line of the operator's file would say so.
// The cost of the rule is that an early broad rule shadows a later specific one, which is
// why `holdfast validate` prints the list in order and why a rule naming no knob at all is
// refused (it would shadow everything after it while doing nothing).
//
// A rule lives in the ENTRY, beside `path`, and is not a fourteenth knob: it is a list of
// mappings, so it has no top-level counterpart to inherit from and no `HOLDFAST_*`
// spelling. Writing `rules` at the top level is refused by name (see Load).
//
// # Why the bounds are spelled min_source_height / max_source_height
//
// They select which files a rule applies TO. `max_height` is an output CEILING - a target,
// and a knob a rule may carry beside its `when` - and two keys one word apart, one selecting
// inputs and one shaping outputs, is a configuration an operator gets wrong once and does not
// notice. So `min_height` and `max_height` are REFUSED INSIDE A `when` by name, with the
// distinction stated and with the place `max_height` does belong named, rather than accepted
// as synonyms.

const (
	// rulesKey is the one place the key is spelled: the entry parser, the top-level
	// refusal and every message that names it read this.
	rulesKey = "rules"
	// whenKey is the optional band selector inside a rule. A rule without one matches
	// every file under its root.
	whenKey = "when"

	minSourceHeightKey = "min_source_height"
	maxSourceHeightKey = "max_source_height"

	// The two spellings a `when` REFUSES, named here because the refusal has to name
	// them. `max_height` is a real key - the output ceiling, written beside `when` rather
	// than inside it (see ceiling.go, which spells it) - and `min_height` is nothing at
	// all; an operator who writes either inside a `when` meant one of the two bounds above.
	minHeightKey = "min_height"
)

// ruleKnobs is THE enumeration of the knobs a rule may override, in the order a rule is
// printed and digested. It is CLOSED, it is a SUBSET of profileKnobs, and it is the one
// list the refusal for an unknown rule key is rendered from - so a knob added here by a
// later item is named by that message with no second list to edit.
//
// It is deliberately five and not fourteen. Four are the knobs whose right value depends
// on how many pixels the source has: the floor below which re-encoding reclaims nothing,
// the quality target, the saving that makes the attempt worth taking, and the ceiling the
// output's own height is held to. `vmaf_model` and the rest do not vary by band, and a rule
// that could move the VMAF floors would be a rule that could weaken the gate standing
// between a band of files and the deletion of their sources - so every VMAF key is REFUSED
// inside a rule by name (see gateKeys), not merely left out.
//
// `max_height` is the most obviously band-shaped of the four: capping a 2160p band at 1080
// while leaving a 576p band alone is the whole reason an operator writes a ceiling per band
// rather than per library. It is a CEILING and never a floor - a rule cannot raise a
// source's resolution - so a band that sets one can only ever produce a smaller picture,
// never a bigger one.
//
// `encoder` is the fifth, and the newest (S0165): it lets an operator put the cheap band of
// a library on a hardware encoder and keep a software one for the rest, without splitting
// the library into roots. It changes WHICH encoder writes a band's replacement and nothing
// about how that replacement is judged: the VMAF floors every output is held to stay the
// root's, whichever encoder produced it. It is the one knob here that is not a number, and
// it is rendered - in the canonical text, the digest and the `validate` line - only when a
// rule names it, so a rule list written without it renders byte-identical to the build
// before it existed. An encode profile matching the same file still has the last word on
// its encoder: it is laid over the rule-resolved profile (config.TranscodeIn), the order
// `crf` already follows between the two.
var ruleKnobs = []string{"min_bitrate_kbps", "crf", "min_savings_percent", maxHeightKey, encoderKey}

// RuleKnobs returns the closed set of knobs a rule may override, in the order a rule is
// printed. It returns a copy: the set is closed, and a caller must not be able to open it.
func RuleKnobs() []string { return append([]string(nil), ruleKnobs...) }

// isRuleKnob reports whether key names a knob a rule may override.
func isRuleKnob(key string) bool {
	for _, k := range ruleKnobs {
		if k == key {
			return true
		}
	}
	return false
}

// gateKeys are the VMAF gate's own keys, which a rule is REFUSED by name rather than as an
// unknown key. Each is a profile knob, so an operator who has just learned that a rule may
// carry `encoder` can reasonably expect it may carry the floor beside it - and a rule that
// could lower the floor for the band a faster encoder writes would be exactly the per-rule
// floor this build refuses: every output under one root is held to that root's floors,
// whichever encoder produced it.
var gateKeys = []string{"vmaf_enable", "min_vmaf", "vmaf_min_pool", "vmaf_min_chroma", "vmaf_subsample", "vmaf_model"}

// isGateKey reports whether key is one of the VMAF gate's keys.
func isGateKey(key string) bool {
	for _, k := range gateKeys {
		if k == key {
			return true
		}
	}
	return false
}

// ruleKeyList renders everything a rule may carry, for the refusal that tells an operator
// what is accepted. Derived from the one enumeration rather than restated.
func ruleKeyList() string {
	return strings.Join(append([]string{whenKey}, ruleKnobs...), ", ")
}

// Band is the range of SOURCE heights, in pixels, a rule applies to. A nil bound leaves
// that side unbounded, and both bounds are INCLUSIVE: a band written 720 to 1080 covers a
// 720p file and a 1080p file, which is the reading an operator who wrote those two numbers
// meant.
type Band struct {
	MinSourceHeight *int
	MaxSourceHeight *int
}

// Bounded reports whether this band constrains anything at all. An unbounded band - an
// absent `when`, or one carrying neither bound - matches every file, and a root whose
// rules are all unbounded needs no source height to pick one.
func (b Band) Bounded() bool { return b.MinSourceHeight != nil || b.MaxSourceHeight != nil }

// Matches reports whether a source of this height in pixels falls in the band.
func (b Band) Matches(sourceHeight int) bool {
	if b.MinSourceHeight != nil && sourceHeight < *b.MinSourceHeight {
		return false
	}
	if b.MaxSourceHeight != nil && sourceHeight > *b.MaxSourceHeight {
		return false
	}
	return true
}

// String renders the band the way `holdfast validate` prints it and the way a refusal
// names it: both bounds, with the word `any` where a side is unbounded.
func (b Band) String() string {
	lo, hi := "any", "any"
	if b.MinSourceHeight != nil {
		lo = strconv.Itoa(*b.MinSourceHeight)
	}
	if b.MaxSourceHeight != nil {
		hi = strconv.Itoa(*b.MaxSourceHeight)
	}
	return "source height " + lo + " to " + hi
}

// Rule is one band's overrides: which files it applies to, and the knobs it supplies for
// them. Every knob is a POINTER, keeping the same present-overrides / absent-inherits
// discipline a profile keeps - so `crf: 0` in a rule means 0 and an absent `crf` means
// "whatever the root resolved".
type Rule struct {
	When Band

	MinBitrateKbps    *int
	CRF               *int
	MinSavingsPercent *int
	MaxHeight         *int

	// Encoder is the encoder key this band's files are encoded with, as written (an ffmpeg
	// codec alias resolves exactly as its key does, through encoder.Lookup, just as it does
	// at the top level, in a root and in an encode profile). nil is "the root's own".
	Encoder *string
}

// knob returns the value this rule supplies for key, and whether it supplies one. It is
// the ONE place a rule knob's name is mapped to its field, so the applier, the printer and
// the canonical rendering cannot disagree about which knob a rule names.
func (r Rule) knob(key string) (int, bool) {
	var p *int
	switch key {
	case "min_bitrate_kbps":
		p = r.MinBitrateKbps
	case "crf":
		p = r.CRF
	case "min_savings_percent":
		p = r.MinSavingsPercent
	case maxHeightKey:
		p = r.MaxHeight
	}
	if p == nil {
		return 0, false
	}
	return *p, true
}

// render returns the text this rule supplies for key, and whether it supplies one. It is
// the ONE reading every printer and the canonical text go through, so the numeric knobs and
// the encoder cannot be rendered by two different rules.
func (r Rule) render(key string) (string, bool) {
	if key == encoderKey {
		if r.Encoder == nil {
			return "", false
		}
		return *r.Encoder, true
	}
	v, ok := r.knob(key)
	if !ok {
		return "", false
	}
	return strconv.Itoa(v), true
}

// Knobs returns the knobs this rule names, in ruleKnobs order.
func (r Rule) Knobs() []string {
	var out []string
	for _, k := range ruleKnobs {
		if _, ok := r.render(k); ok {
			out = append(out, k)
		}
	}
	return out
}

// applyTo returns p with this rule's knobs laid over it. Every knob the rule does not name
// is left exactly as the root resolved it.
func (r Rule) applyTo(p Profile) Profile {
	if v, ok := r.knob("min_bitrate_kbps"); ok {
		p.MinBitrateKbps = v
	}
	if v, ok := r.knob("crf"); ok {
		p.CRF = v
	}
	if v, ok := r.knob("min_savings_percent"); ok {
		p.MinSavingsPercent = v
	}
	if v, ok := r.knob(maxHeightKey); ok {
		p.MaxHeight = v
	}
	if r.Encoder != nil {
		p.Encoder = *r.Encoder
	}
	return p
}

// String renders one rule the way `holdfast validate` prints it: its band, then the knobs
// it overrides with their values.
func (r Rule) String() string {
	var b strings.Builder
	b.WriteString(r.When.String())
	b.WriteString(": ")
	parts := make([]string, 0, len(ruleKnobs))
	for _, k := range ruleKnobs {
		if v, ok := r.render(k); ok {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) == 0 {
		// Unreachable from a loaded configuration (such a rule is refused at start), and
		// spelled anyway so a Rule assembled by hand never prints a dangling colon.
		b.WriteString("no override")
		return b.String()
	}
	b.WriteString(strings.Join(parts, " "))
	return b.String()
}

// Rules is one root's rule list, in the order it was written. The order IS the semantics:
// the first match decides.
type Rules []Rule

// NeedsSourceHeight reports whether picking a rule for a file requires knowing that file's
// source height. It is false for a root with no rules and for one whose rules are all
// unbounded, and those are exactly the roots whose files are decided without a probe.
//
// It asks whether ANY rule carries a bound, not whether a bounded rule is reachable. A root
// whose first rule is unconditional shadows everything after it, so the height is not
// strictly needed there - but the fail-safe rule this build keeps everywhere else applies
// here too: a shadowed band is a configuration the operator did not mean, and reading the
// height is what lets `holdfast validate` and the skip in front of the guards say so
// rather than silently deciding the file under a rule the operator thought was scoped.
func (rs Rules) NeedsSourceHeight() bool {
	for _, r := range rs {
		if r.When.Bounded() {
			return true
		}
	}
	return false
}

// Match returns the FIRST rule whose band admits a source of this height, and whether
// there was one. Callers that have no height must have checked NeedsSourceHeight first:
// with no bounded rule in the list the height is not read at all.
func (rs Rules) Match(sourceHeight int) (Rule, bool) {
	i := rs.matchIndex(sourceHeight)
	if i < 0 {
		return Rule{}, false
	}
	return rs[i], true
}

// matchIndex is Match's index: the position of the first rule whose band admits a source of
// this height, or -1 when none does. It is what a refusal naming a rule by its index reads,
// so the refusal and the resolver cannot disagree about which rule decides a height.
func (rs Rules) matchIndex(sourceHeight int) int {
	for i, r := range rs {
		if r.When.Matches(sourceHeight) {
			return i
		}
	}
	return -1
}

// Canonical renders the whole list as the stable text two things are taken over: the
// profile digest, and the decision input a file skipped for an undetermined height
// records. Two rule lists that decide every file identically render to one text, and two
// that differ anywhere render to two.
//
// The knob NAME is written beside its value and the bounds are named rather than
// positional, so no two different lists can produce one text.
func (rs Rules) Canonical() string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.canonical())
	}
	return strings.Join(parts, ";")
}

// canonical renders ONE rule for Canonical.
func (r Rule) canonical() string {
	fields := make([]string, 0, len(ruleKnobs)+2)
	if r.When.MinSourceHeight != nil {
		fields = append(fields, fmt.Sprintf("%s=%d", minSourceHeightKey, *r.When.MinSourceHeight))
	}
	if r.When.MaxSourceHeight != nil {
		fields = append(fields, fmt.Sprintf("%s=%d", maxSourceHeightKey, *r.When.MaxSourceHeight))
	}
	for _, k := range ruleKnobs {
		if v, ok := r.render(k); ok {
			fields = append(fields, k+"="+v)
		}
	}
	return "[" + strings.Join(fields, ",") + "]"
}

// WithRules returns the profile that decides a source of this height under p's root: p
// with the first matching rule's knobs laid over it, or p itself when no rule matches.
//
// It is the ONE place the first-match rule is applied. The engine resolves it once per
// file and threads the result through every guard, the encoder and the terminal row, for
// the reason the root's own profile is resolved once: re-deriving it at each use would let
// a file be guarded by one band's floor and encoded at another band's crf.
func (p Profile) WithRules(sourceHeight int) Profile {
	r, ok := p.Rules.Match(sourceHeight)
	if !ok {
		return p
	}
	return r.applyTo(p)
}

// parseRules turns one entry's raw `rules` value into the ordered list, refusing every
// shape that is not one.
//
// where is the entry as the other refusals in this package name it - the index, the file
// and the root's own path - and every refusal here adds the rule's INDEX to it, because an
// index is the only thing an operator can use to find one rule in a list of them.
func parseRules(where string, raw any) (Rules, error) {
	if raw == nil {
		return nil, fmt.Errorf("%s sets %q with no value: %s is an ordered list of rules "+
			"(each a mapping of %s), and a key written with no value at all is neither a list nor "+
			"absent - write the rules, or remove the key", where, rulesKey, rulesKey, ruleKeyList())
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s has a %s that is not a list (%#v): %s is an ORDERED list of "+
			"rules, first match wins, and each entry is a mapping of %s",
			where, rulesKey, raw, rulesKey, ruleKeyList())
	}
	rules := make(Rules, 0, len(items))
	for i, item := range items {
		r, err := parseRule(fmt.Sprintf("%s %s[%d]", where, rulesKey, i), item)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		// An empty list and an absent key are ONE state: both mean this root has no bands,
		// and collapsing them here is what makes their profile digests identical rather
		// than merely equivalent.
		return nil, nil
	}
	return rules, nil
}

// parseRule parses ONE rule entry. See parseRules for the refusal discipline.
func parseRule(where string, item any) (Rule, error) {
	m, err := stringKeyedMap(where, item, fmt.Sprintf("a rule is a mapping of %s", ruleKeyList()))
	if err != nil {
		return Rule{}, err
	}
	var r Rule
	for key, val := range m {
		switch {
		case key == whenKey:
			band, err := parseBand(where, val)
			if err != nil {
				return Rule{}, err
			}
			r.When = band
		case key == encoderKey:
			// The encoder is read as WRITTEN and judged later, by the same function an
			// encode profile's encoder is judged by (validateEncoderKey, from validateRules),
			// so whatever that accepts a rule accepts and in the same words. A key written
			// with no value is carried as "" for that function to refuse by name.
			v, err := ruleEncoderValue(where, val)
			if err != nil {
				return Rule{}, err
			}
			r.Encoder = &v
		case isRuleKnob(key):
			// `max_height` goes through its OWN reading, which refuses every value this
			// build cannot target - a present 0 and an odd height included - in the same
			// words the top level and a `library_roots` entry refuse them. The generic
			// whole-number read below would accept both and leave the ceiling to be
			// discovered as a failed encode halfway through a library.
			var v int
			var err error
			if key == maxHeightKey {
				v, err = ceilingValue(where, val)
			} else {
				v, err = ruleKnobValue(where, key, val)
			}
			if err != nil {
				return Rule{}, err
			}
			switch key {
			case "min_bitrate_kbps":
				r.MinBitrateKbps = &v
			case "crf":
				r.CRF = &v
			case "min_savings_percent":
				r.MinSavingsPercent = &v
			case maxHeightKey:
				r.MaxHeight = &v
			}
		case isGateKey(key):
			return Rule{}, fmt.Errorf("%s carries %q, which a rule may NOT carry: the VMAF gate's settings "+
				"are the library root's own, and every output under a root is held to that root's floors "+
				"whichever encoder or band produced it - a rule that could move them would be a rule that "+
				"weakens the gate for the files it takes. Set %s on the root or at the top level. A rule may "+
				"carry %s and nothing else", where, key, key, ruleKeyList())
		default:
			return Rule{}, fmt.Errorf("%s has unknown key %q: a rule may carry %s and nothing else",
				where, key, ruleKeyList())
		}
	}
	if len(r.Knobs()) == 0 {
		return Rule{}, fmt.Errorf("%s names no knob at all: a rule that overrides nothing still "+
			"MATCHES, so under first-match it would silently shadow every rule after it and change "+
			"nothing about the files it took. Give it one of %s, or remove it",
			where, strings.Join(ruleKnobs, ", "))
	}
	return r, nil
}

// parseBand parses one rule's `when`. An absent `when` never reaches here; a present one
// that carries neither bound is accepted and matches every file, which is the same
// statement written differently.
func parseBand(where string, raw any) (Band, error) {
	m, err := stringKeyedMap(where, raw, fmt.Sprintf("a %s is a mapping of %s and %s",
		whenKey, minSourceHeightKey, maxSourceHeightKey))
	if err != nil {
		return Band{}, err
	}
	var b Band
	for key, val := range m {
		switch key {
		case minSourceHeightKey, maxSourceHeightKey:
			v, err := boundValue(where, key, val)
			if err != nil {
				return Band{}, err
			}
			if key == minSourceHeightKey {
				b.MinSourceHeight = &v
			} else {
				b.MaxSourceHeight = &v
			}
		case minHeightKey, maxHeightKey:
			return Band{}, fmt.Errorf("%s: a %s bound is spelled %s / %s, not %q. A %s bound "+
				"selects which files a rule applies TO - it is a property of the SOURCE - and is "+
				"not an output ceiling. %s IS an output ceiling and this rule may carry one, but "+
				"BESIDE the %s rather than inside it: a rule whose %s selects a band, and whose "+
				"%s caps what that band is encoded to",
				where, whenKey, minSourceHeightKey, maxSourceHeightKey, key, whenKey,
				maxHeightKey, whenKey, whenKey, maxHeightKey)
		default:
			return Band{}, fmt.Errorf("%s: a %s has unknown key %q: it may carry %s and %s and "+
				"nothing else", where, whenKey, key, minSourceHeightKey, maxSourceHeightKey)
		}
	}
	if b.MinSourceHeight != nil && b.MaxSourceHeight != nil && *b.MinSourceHeight > *b.MaxSourceHeight {
		return Band{}, fmt.Errorf("%s: %s %d is greater than %s %d, so the band covers no source "+
			"height at all and the rule could never apply to anything",
			where, minSourceHeightKey, *b.MinSourceHeight, maxSourceHeightKey, *b.MaxSourceHeight)
	}
	return b, nil
}

// ruleEncoderValue reads a rule's `encoder` as the text written. A key with no value reads as
// "" so that validateEncoderKey refuses it naming the value and the known encoders, exactly
// as it refuses `encoder: ""` in an encode profile; anything that is not text is refused
// here, since no encoder key is a number or a list.
func ruleEncoderValue(where string, raw any) (string, error) {
	switch v := raw.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	}
	return "", fmt.Errorf("%s: %s must be an encoder key (known: %v): %#v is not one",
		where, encoderKey, encoder.Known(), raw)
}

// boundValue reads one band bound, refusing anything that is not a POSITIVE whole number
// of pixels.
//
// It is checked against the RAW value rather than through the weakly-typed decoder the
// knobs go through, and that difference is deliberate: the decoder would read 575.5 as
// 575 and "576" as 576 without a word, and a band whose edge the operator did not write
// selects a different set of files than the one they meant. A height of 0 is refused for
// the same reason - it is a bound no source can sit below, so it can only be a typo or an
// empty value.
func boundValue(where, key string, raw any) (int, error) {
	if !isWholeNumber(raw) {
		return 0, fmt.Errorf("%s: %s must be a whole number of pixels: %#v is not one", where, key, raw)
	}
	v, ok := wholeNumber(raw)
	if !ok || v <= 0 {
		return 0, fmt.Errorf("%s: %s must be a POSITIVE whole number of pixels: %#v is not one",
			where, key, raw)
	}
	return v, nil
}

// ruleKnobValue reads one rule knob. The value is read the way the TOP LEVEL reads the
// same key - a YAML integer and the string spelling of one both land the same way - so a
// rule and the top level cannot disagree about what a number is. What the value MEANS is
// checked later, by the same per-value validation the top level runs (see
// Config.validateRules), so a rule and the top level refuse a bad crf in the same words.
func ruleKnobValue(where, key string, raw any) (int, error) {
	if !isWholeNumber(raw) {
		return 0, fmt.Errorf("%s: %s must be a whole number: %#v is not one", where, key, raw)
	}
	v, ok := wholeNumber(raw)
	if !ok {
		return 0, fmt.Errorf("%s: %s must be a whole number: %#v is not one", where, key, raw)
	}
	return v, nil
}

// wholeNumber converts a RAW layered value that isWholeNumber has already accepted into
// the int it spells.
func wholeNumber(raw any) (int, bool) {
	switch v := raw.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	case float32:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

// stringKeyedMap reads a raw YAML value as a mapping with string keys, refusing anything
// else in the words the caller supplies.
//
// The map[any]any arm is defensive, exactly as parseRootEntry's is: this build's YAML
// parser produces string-keyed maps, and a non-string key must be a refusal naming the key
// rather than a panic or a silently dropped knob.
func stringKeyedMap(where string, raw any, expected string) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case map[any]any:
		m := make(map[string]any, len(v))
		for key, val := range v {
			s, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("%s has a non-string key %#v: %s", where, key, expected)
			}
			m[s] = val
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%s is %#v: %s", where, raw, expected)
	}
}

// validateRules runs the per-value refusals over every rule every root carries: a rule's
// knob is held to the SAME bar the top level holds that key to, in the same words.
//
// It is done by laying the rule over the root's own RESOLVED profile and running the
// profile validation on the result, which is what makes "a rule value the top level would
// refuse is refused" true by construction rather than by two copies of the same arithmetic
// agreeing. The root's own profile has already been validated by the time this runs, so
// anything this finds came from the rule.
//
// A rule's `encoder` is judged FIRST and by validateEncoderKey, the function an encode
// profile's encoder is judged by, rather than by Profile.validate: a profile's plain-string
// encoder spells "inherit" as "", which that validation therefore accepts, while a rule that
// wrote `encoder: ""` wrote a value and must be refused. Then two refusals that only a rule
// can earn: an encoder under a root that re-encodes nothing, and a ceiling that writes one
// band's output into a band targeting another codec (see bandCrossing).
func (c *Config) validateRules() error {
	for _, r := range c.RootProfiles() {
		for i, rule := range r.Profile.Rules {
			if rule.Encoder != nil {
				if err := validateEncoderKey(*rule.Encoder); err != nil {
					return fmt.Errorf("library root %s: %s[%d]: %w", r.Clean, rulesKey, i, err)
				}
				if r.Profile.RemuxOnlyEnabled() {
					return fmt.Errorf("library root %s: %s[%d] names %s %q, but this root resolves %s "+
						"to true, which re-encodes nothing: the rule's encoder could never run. Remove the "+
						"rule's %s, or set %s: false on the root", r.Clean, rulesKey, i, encoderKey,
						*rule.Encoder, remuxOnlyKey, encoderKey, remuxOnlyKey)
				}
			}
			if err := rule.applyTo(r.Profile).validate(); err != nil {
				return fmt.Errorf("library root %s: %s[%d]: %w", r.Clean, rulesKey, i, err)
			}
		}
		if err := r.Profile.bandCrossing(); err != nil {
			return fmt.Errorf("library root %s: %w", r.Clean, err)
		}
	}
	return nil
}

// bandCrossing refuses a root on which a `max_height` ceiling writes one band's output at a
// height the first-match resolution puts under a DIFFERENT rule (or under no rule), when the
// two resolved encoders target different codecs.
//
// Bands select on SOURCE height, and a ceiling makes the output shorter than its source. So
// a band capped below its own lower bound writes files the next scan decides under another
// band; while every band of a root targeted one codec that was harmless, because the
// already-at-target skip took the output. Once a rule carries an encoder it is not: an av1
// output at 1080 lands in an hevc band, is not at that band's target, and is encoded a
// second time - after a swap that already deleted the original. That is refused at start.
//
// The output height is the one the encode really writes: it is read off downscale.Resolve,
// through Profile.DownscaleFor, which is what the encoder's scale filter is built from - not
// assumed to equal the ceiling. The width handed to it is a stand-in, since the height that
// function returns does not depend on it.
//
// Every height is covered without walking them all. The first matching rule, and with it
// the ceiling in force, can only change at a band bound, and whether a source is above its
// ceiling only at a ceiling plus one; so the answer is constant between consecutive
// members of that set of breakpoints, and asking at each breakpoint asks about every
// height there is.
//
// Encode profiles are not consulted. One is laid over the rule-resolved profile by PATH,
// and what this refuses is the route the RULES open, by height; a source and its
// replacement share a directory and a stem, so only a pattern that tells them apart by
// extension could put them under two encode profiles, and that is a property of the encode
// profile list with or without rules.
func (p Profile) bandCrossing() error {
	if len(p.Rules) == 0 {
		return nil
	}
	points := map[int]bool{1: true}
	ceiling := func(h int) {
		if h >= downscale.MinHeight {
			points[h+1] = true
		}
	}
	ceiling(p.MaxHeight)
	for _, r := range p.Rules {
		if r.When.MinSourceHeight != nil {
			points[*r.When.MinSourceHeight] = true
		}
		if r.When.MaxSourceHeight != nil {
			points[*r.When.MaxSourceHeight+1] = true
		}
		if v, ok := r.knob(maxHeightKey); ok {
			ceiling(v)
		}
	}
	heights := make([]int, 0, len(points))
	for h := range points {
		heights = append(heights, h)
	}
	sort.Ints(heights)
	for _, h := range heights {
		from := p.WithRules(h)
		scale := from.DownscaleFor(2*h, h)
		if !scale.Enabled() {
			continue
		}
		out := scale.Height
		fi, oi := p.Rules.matchIndex(h), p.Rules.matchIndex(out)
		if fi == oi {
			continue
		}
		to := p.WithRules(out)
		fc, tc := targetCodecOf(from.Encoder), targetCodecOf(to.Encoder)
		if fc == tc {
			continue
		}
		return fmt.Errorf("%s writes a source of height %d at %s %d, and a %d-line output is decided "+
			"by %s on the next scan: %s targets %s (encoder %s) and %s targets %s (encoder %s), so the "+
			"replacement would not be at its new band's target codec and would be encoded a SECOND time, "+
			"after the swap already deleted the original. Give both bands one target codec, or move the "+
			"band bound or the %s so the output stays in the band that wrote it",
			bandName(fi), h, maxHeightKey, from.MaxHeight, out, bandName(oi),
			bandName(fi), fc, from.Encoder, bandName(oi), tc, to.Encoder, maxHeightKey)
	}
	return nil
}

// bandName names the band a first-match index denotes, the way a refusal names it: a rule by
// its index, and the heights no rule admits by the profile that decides them.
func bandName(i int) string {
	if i < 0 {
		return "the root's own profile"
	}
	return fmt.Sprintf("%s[%d]", rulesKey, i)
}

// targetCodecOf is the codec an encoder key writes. A key the registry does not resolve (one
// validation has not reached, or a selection made per job) is its own codec name, so two
// different such keys are never taken to agree.
func targetCodecOf(key string) string {
	if spec, ok := encoder.Lookup(key); ok {
		return spec.TargetCodec
	}
	return "unresolved encoder " + strconv.Quote(key)
}

// RuleEncoder is one encoder a resolution rule names, with the root and the rule index that
// name it - which is what the startup capability preflight's refusal has to tell an operator
// whose top-level and root encoders both work.
type RuleEncoder struct {
	Key  string
	Root string
	Rule int
}

// RuleEncoders is every encoder a resolution rule names, in configuration order,
// deduplicated by key, each carrying the first root and rule index that asks for it.
//
// It exists for the startup capability preflight, beside the root walk and
// EncodeProfileEncoders, and covers exactly the keys those two miss: a rule's encoder is
// reached by every file its band admits, so a preflight blind to it would let a run start and
// then fail that band's files one at a time. Empty when no rule names an encoder, which is
// every configuration written before rules could.
func (c *Config) RuleEncoders() []RuleEncoder {
	var out []RuleEncoder
	seen := map[string]bool{}
	for _, r := range c.RootProfiles() {
		for i, rule := range r.Profile.Rules {
			if rule.Encoder == nil || seen[*rule.Encoder] {
				continue
			}
			seen[*rule.Encoder] = true
			out = append(out, RuleEncoder{Key: *rule.Encoder, Root: r.Clean, Rule: i})
		}
	}
	return out
}

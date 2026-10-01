package config

import (
	"fmt"
)

// Queue PRIORITY: which candidates a scan offers first, ahead of the declared queue_order.
//
// It decides SEQUENCE and nothing else (docs/design/queue-order.md#priority). It is not a
// knob: no guard, no gate, no encoder argument and no decision input reads it, so it is
// kept OFF the profile digest, off the rule list's canonical text and off every terminal
// row. Adding, changing or removing a priority therefore re-opens no row and moves no
// digest - which is the whole of why it can be edited freely while a library is half done.
//
// It may be written in three places, and a file's priority is the FIRST of them that names
// one:
//
//  1. the resolution rule that decides the file (rules.go): the first rule whose band
//     admits the source, exactly as first match decides every knob;
//  2. the encode profile that decides the file (transcode.go): the first whose match
//     selects the source path;
//  3. the library root the file lives under (a `library_roots` entry, beside `path`).
//
// Otherwise it is 0. Higher is offered first; the declared queue_order then orders files
// of equal priority, and the full path breaks any tie left.
//
// It is never a top-level key: there is nothing at the top level to prioritise one file
// over another by, and a `priority` written there would be a configuration that changes
// nothing while reading as though it did. It is refused by name (see Load).

// priorityKey is the one place the key is spelled: the root-entry parser, a rule's parser,
// the encode-profile key set, the top-level refusal and every message read it.
const priorityKey = "priority"

// The accepted range. Wide enough for any banding an operator can mean (a handful of tiers,
// spaced so one can be inserted between two), narrow enough that a value outside it is a
// typo - a pasted byte count or a year - rather than an intention.
const (
	MinPriority = -1000
	MaxPriority = 1000
)

// priorityValue reads one written priority, refusing anything that is not a whole number in
// [MinPriority, MaxPriority]. It is checked against the RAW value, for the reason boundValue
// is: the weakly-typed decoder would read 2.5 as 2 and `true` as 1, two priorities the
// operator did not write.
func priorityValue(where string, raw any) (int, error) {
	if raw == nil {
		return 0, fmt.Errorf("%s sets %q with no value: a %s is a whole number from %d to %d "+
			"(higher is offered first; absent is 0)", where, priorityKey, priorityKey, MinPriority, MaxPriority)
	}
	if !isWholeNumber(raw) {
		return 0, fmt.Errorf("%s: %s must be a whole number from %d to %d: %#v is not one",
			where, priorityKey, MinPriority, MaxPriority, raw)
	}
	v, ok := wholeNumber(raw)
	if !ok {
		return 0, fmt.Errorf("%s: %s must be a whole number from %d to %d: %#v is not one",
			where, priorityKey, MinPriority, MaxPriority, raw)
	}
	if err := checkPriority(v); err != nil {
		return 0, fmt.Errorf("%s: %w", where, err)
	}
	return v, nil
}

// checkPriority refuses a priority outside the accepted range, naming the key, the value
// and the range. It is what Validate holds a Config assembled in Go to, as well as what the
// parsers hold a written value to.
func checkPriority(v int) error {
	if v < MinPriority || v > MaxPriority {
		return fmt.Errorf("%s %d out of range (%d to %d; higher is offered first, absent is 0)",
			priorityKey, v, MinPriority, MaxPriority)
	}
	return nil
}

// misplacedPriorityError is the refusal for `priority` written at the top level (the file
// or HOLDFAST_PRIORITY). where names the file or the environment variable that carried it.
func misplacedPriorityError(where string) error {
	return fmt.Errorf("%q is a TOP-LEVEL key in %s: a priority orders some files ahead of others, so "+
		"it is written on what selects those files - a library_roots entry (beside its %q), a "+
		"resolution rule inside one, or an encode_profiles entry - and there is nothing at the top "+
		"level for it to select. Write it as:\n"+
		"  library_roots:\n"+
		"    - path: /media/films\n"+
		"      %s: 10",
		priorityKey, where, rootPathKey, priorityKey)
}

// PriorityConfigured reports whether any root, rule or encode profile names a priority.
// It is false for every configuration written before the key existed, and that is what
// keeps those configurations' queues exactly what they were: the `path` order still
// streams, and no keyed order reads anything more than it did.
func (c *Config) PriorityConfigured() bool {
	for _, r := range c.RootProfiles() {
		if r.Priority != nil {
			return true
		}
		for _, rule := range r.Profile.Rules {
			if rule.Priority != nil {
				return true
			}
		}
	}
	for i := range c.EncodeProfiles {
		if c.EncodeProfiles[i].Priority != nil {
			return true
		}
	}
	return false
}

// PriorityNeedsSourceHeight reports whether resolving a priority for a file under this root
// needs the source's height: only when a rule NAMES a priority and some rule bands on the
// height, because only then can the height change which rule decides the file and so
// whether a rule's priority applies. A root whose rules name no priority reads no height
// for this, whatever its bands - the priority then comes from the encode profile or the
// root, and neither depends on the height.
func (r Root) PriorityNeedsSourceHeight() bool {
	if !r.Profile.Rules.NeedsSourceHeight() {
		return false
	}
	for _, rule := range r.Profile.Rules {
		if rule.Priority != nil {
			return true
		}
	}
	return false
}

// PriorityOf is ONE FILE's priority: its deciding rule's, else its deciding encode
// profile's, else its root's, else 0.
//
// sourceHeight is read only where PriorityNeedsSourceHeight says it is needed; a caller
// that has no height passes 0 for a root that does not need one, and Rules.Match then
// answers exactly as WithRules(0) does for the knobs (an unbounded list's first rule).
func (c *Config) PriorityOf(root Root, sourcePath string, sourceHeight int) int {
	if rule, ok := root.Profile.Rules.Match(sourceHeight); ok && rule.Priority != nil {
		return *rule.Priority
	}
	if ep, ok := c.EncodeProfileFor(sourcePath); ok && ep.Priority != nil {
		return *ep.Priority
	}
	if root.Priority != nil {
		return *root.Priority
	}
	return 0
}

// EncodeProfileFor returns the encode profile that decides sourcePath - the FIRST whose
// match selects it, which is the one TranscodeIn applies - and whether there is one. A
// pattern that cannot be parsed does not match, as in TranscodeIn.
func (c *Config) EncodeProfileFor(sourcePath string) (EncodeProfile, bool) {
	for i := range c.EncodeProfiles {
		if ok, err := MatchSource(c.EncodeProfiles[i].Match, sourcePath); err == nil && ok {
			return c.EncodeProfiles[i], true
		}
	}
	return EncodeProfile{}, false
}

// validatePriorities holds every priority a Config carries to the accepted range, naming
// where it was written. A loaded Config has already been held to it by the parsers; this is
// the door a Config assembled in Go comes through.
func (c *Config) validatePriorities() error {
	for _, r := range c.RootProfiles() {
		if r.Priority != nil {
			if err := checkPriority(*r.Priority); err != nil {
				return fmt.Errorf("library root %s: %w", r.Clean, err)
			}
		}
		for i, rule := range r.Profile.Rules {
			if rule.Priority != nil {
				if err := checkPriority(*rule.Priority); err != nil {
					return fmt.Errorf("library root %s: %s[%d]: %w", r.Clean, rulesKey, i, err)
				}
			}
		}
	}
	for i := range c.EncodeProfiles {
		p := &c.EncodeProfiles[i]
		if p.Priority != nil {
			if err := checkPriority(*p.Priority); err != nil {
				return fmt.Errorf("encode_profiles[%d] (%s) %w", i, p.Name, err)
			}
		}
	}
	return nil
}

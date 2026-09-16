package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Stream selection: which of a source's streams a replacement may carry.
//
// Four optional keys, at the top level and inside a `library_roots` entry, every one of
// them defaulting to what this tool did before they existed:
//
//   - `audio_languages` / `subtitle_languages` - lists of ISO-639-2 codes, EMPTY by
//     default, and empty means "carry every stream of that type", which is today's
//     behaviour and the behaviour of an absent key.
//   - `keep_commentary` - TRUE by default, so dropping a commentary track is a choice
//     and never a side effect of a language filter.
//   - `remux_only` - FALSE by default. When true the video is stream-copied too and
//     nothing is re-encoded.
//
// They are PROFILE KNOBS and the digest covers them (see profileKnobs), which is the
// opposite ruling from the one the path filters got and for a reason that survives: a
// path filter decides whether a file is OFFERED, and these decide what is DONE to it.
// Two roots that differ only in `audio_languages` produce materially different outputs,
// and a digest that could not tell them apart would be a digest that lies about what
// decided a row.
const (
	audioLanguagesKey    = "audio_languages"
	subtitleLanguagesKey = "subtitle_languages"
	keepCommentaryKey    = "keep_commentary"
	remuxOnlyKey         = "remux_only"

	// encoderKey is spelled here because the per-layer mutual exclusion below names it,
	// and a refusal that named a key by a literal could go on naming one that had been
	// renamed.
	encoderKey = "encoder"
)

// selectionKnobs are the four keys in the order they are validated, digested and
// printed. They are a subset of profileKnobs and are named here so the refusals and the
// documentation cannot drift from the set.
var selectionKnobs = []string{audioLanguagesKey, subtitleLanguagesKey, keepCommentaryKey, remuxOnlyKey}

// SelectionKnobs returns the closed set of stream-selection keys, in that order. It
// returns a copy: the set is closed, and a caller must not be able to open it.
func SelectionKnobs() []string { return append([]string(nil), selectionKnobs...) }

// noLanguages is the canonical text an EMPTY language list renders to, and it is a token
// rather than the empty string for two reasons: `holdfast validate` prints this text, and
// a blank column tells an operator nothing; and the digest is taken over this text, where
// an empty value beside a knob name reads as a knob nobody set rather than a resolved
// "every language". It can never collide with a real list - every accepted code is
// exactly three alphabetic characters and this is four - which is the same guarantee
// store.noInputsRead rests on.
const noLanguages = "none"

// canonicalLanguages is the ONE reading of a configured language list: lowercased,
// trimmed, empties dropped, sorted and de-duplicated.
//
// It is canonical because the same text is BOTH what the matcher compares against and
// what the digest is taken over. Matching is case-insensitive and order-free, so `[ENG,
// jpn]`, `[jpn, eng]` and `[eng, jpn, eng]` decide every file identically - and two
// profiles that behave identically must digest identically, which is only true if one
// function produces the text both readings use.
func canonicalLanguages(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, code := range list {
		c := strings.ToLower(strings.TrimSpace(code))
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// renderLanguages is canonicalLanguages as the single line of text the digest hashes and
// `validate` prints.
func renderLanguages(list []string) string {
	codes := canonicalLanguages(list)
	if len(codes) == 0 {
		return noLanguages
	}
	return strings.Join(codes, ",")
}

// AudioLanguageCodes and SubtitleLanguageCodes are this profile's language lists in their
// canonical form - which is what the selection compares a stream's tag against. An empty
// result means "carry every stream of that type".
func (p Profile) AudioLanguageCodes() []string { return canonicalLanguages(p.AudioLanguages) }

// SubtitleLanguageCodes is the subtitle half of AudioLanguageCodes.
func (p Profile) SubtitleLanguageCodes() []string { return canonicalLanguages(p.SubtitleLanguages) }

// CommentaryKept reports whether container-marked commentary streams are carried forward
// under this root, defaulting to TRUE when unset. Dropping a commentary track is a choice
// and never a side effect of a language filter.
func (p Profile) CommentaryKept() bool { return commentaryKept(p.KeepCommentary) }

// RemuxOnlyEnabled reports whether this root stream-copies the video as well, defaulting
// to FALSE when unset - which is every configuration written before this key existed.
func (p Profile) RemuxOnlyEnabled() bool { return remuxOnly(p.RemuxOnly) }

// The two sentinel readings as free functions, because a top-level value and a resolved
// per-root profile must read them identically or the same YAML would mean two things
// depending on which spelling of an entry it was written under.

func commentaryKept(p *bool) bool { return p == nil || *p }

func remuxOnly(p *bool) bool { return p != nil && *p }

// CommentaryKept reports whether container-marked commentary is carried at the top level,
// defaulting to TRUE when unset.
func (c *Config) CommentaryKept() bool { return commentaryKept(c.KeepCommentary) }

// RemuxOnlyEnabled reports whether the top level stream-copies the video, defaulting to
// FALSE when unset.
func (c *Config) RemuxOnlyEnabled() bool { return remuxOnly(c.RemuxOnly) }

// validateSelection is the per-value refusal for the two language lists, run against the
// top level and again against every root's RESOLVED profile by Profile.validate.
//
// A code is validated by SHAPE and never against a registry. Carrying a copy of ISO 639-2
// would mean refusing the day a container uses a code the copy predates, and the failure
// that actually happens is `english`, `en` or a stray comma - which three alphabetic
// characters catches. The never-a-silent-file guard covers what shape checking cannot.
func (p Profile) validateSelection() error {
	for _, k := range []struct {
		key  string
		list []string
	}{
		{audioLanguagesKey, p.AudioLanguages},
		{subtitleLanguagesKey, p.SubtitleLanguages},
	} {
		for _, code := range k.list {
			if !validLanguageCode(code) {
				return fmt.Errorf("%s carries %q, which is not a language code: each entry must be "+
					"three alphabetic characters (an ISO 639-2 code, e.g. eng, jpn, fre). Codes are "+
					"compared case-insensitively, and a stream with no language tag - or the undefined "+
					"code und - is KEPT whatever this list says", k.key, code)
			}
		}
	}
	return nil
}

// validLanguageCode reports whether s is three alphabetic characters. Whitespace around a
// code is ordinary YAML and is trimmed rather than refused; everything else - a two-letter
// code, an English word, a stray comma, an empty entry - is refused by name.
func validLanguageCode(s string) bool {
	c := strings.TrimSpace(s)
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// checkRemuxEncoderConflict refuses a configuration LAYER that sets `remux_only: true`
// and also names `encoder`. The two are contradictory instructions about the same job:
// one says re-encode nothing, the other says which encoder to re-encode with, and a build
// that silently picked either would be doing something the operator's file does not say.
//
// The exclusion is PER LAYER and not per resolved profile, deliberately. `encoder` has a
// built-in default so every profile RESOLVES one, and refusing a resolved pair would make
// every remux-only configuration unloadable. The conflict is an operator writing both in
// one place, and that is what is refused; an `encoder` merely INHERITED from an outer
// layer is not a conflict and is not refused.
//
// It runs in Load, where the layers are still distinguishable, and it changes nothing: the
// error is returned before a Config is built, so `run`, `serve` and `validate` all refuse
// the same file in the same words with nothing else having happened.
func checkRemuxEncoderConflict(topRemux any, explicitTop map[string]bool, entries []rootEntry, file string) error {
	if explicitTop[remuxOnlyKey] && explicitTop[encoderKey] && truthy(topRemux) {
		return fmt.Errorf("%s is true and %s is set in the same layer (the top level of %s): %s means "+
			"nothing is re-encoded, so naming an encoder beside it is two different instructions about "+
			"the same job. Remove one of them - an %s inherited from the top level by a root that does "+
			"NOT set %s is not a conflict",
			remuxOnlyKey, encoderKey, file, remuxOnlyKey, encoderKey, remuxOnlyKey)
	}
	for i, e := range entries {
		raw, ok := e.override[remuxOnlyKey]
		if !ok || !truthy(raw) {
			continue
		}
		if _, named := e.override[encoderKey]; !named {
			continue
		}
		where := fmt.Sprintf("library_roots[%d]", i)
		if strings.TrimSpace(e.path) != "" {
			where += " (" + e.path + ")"
		}
		return fmt.Errorf("%s is true and %s is set in the same layer (%s in %s): %s means nothing is "+
			"re-encoded, so naming an encoder beside it is two different instructions about the same "+
			"job. Remove one of them - an %s inherited from the top level is not a conflict",
			remuxOnlyKey, encoderKey, where, file, remuxOnlyKey, encoderKey)
	}
	return nil
}

// truthy reads a RAW layered value as a boolean the way the weakly-typed decoder will.
// An environment override arrives as the string "true" and a YAML boolean as a bool, so
// both spellings of a genuine true are seen here; anything the decoder would refuse is
// not treated as true, because the decoder's own refusal is the better error and this
// check must not pre-empt it with a worse one.
func truthy(raw any) bool {
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && b
	default:
		return false
	}
}

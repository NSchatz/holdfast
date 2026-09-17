package config

import (
	"strings"
	"testing"
)

// S0089 - what an operator may write as a resolution rule, and what they are refused.
//
// Every refusal here is a START refusal (cli L7, the clause `secrets` K5 moved there): the
// process reads its configuration once, validates it, and exits non-zero naming the
// offending key. A rule that decided files it was never meant to reach would be a bitrate
// floor, a crf or a savings floor applied to a band nobody chose, on a tool that deletes
// the source of every file it accepts - so the direction is refuse at start, never guess
// and act.

// rulesOf returns the resolved rules of the one root in cfg.
func rulesOf(t *testing.T, cfg string) Rules {
	t.Helper()
	c := loadYAML(t, cfg)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return rootByPath(t, c, "/mnt/tv").Profile.Rules
}

// refusal loads cfg, runs Validate, and returns the refusal - failing the test when there
// is none. Both doors are asked because both are start refusals: a shape is refused while
// the file is being read, and a VALUE is refused against the profile it would produce.
func refusal(t *testing.T, cfg string) string {
	t.Helper()
	c, err := load(t, cfg)
	if err == nil {
		err = c.Validate()
	}
	if err == nil {
		t.Fatalf("the configuration was ACCEPTED:\n%s", cfg)
	}
	return err.Error()
}

// mentions fails unless the message names every one of the things an operator has to be
// told: which key, which root, which rule.
func mentions(t *testing.T, msg string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("the refusal does not mention %q:\n%s", w, msg)
		}
	}
}

// [AC-1] WHEN a library_roots entry carries a `rules` key holding an ordered list, it is
// accepted and the list's WRITTEN ORDER is preserved on the resolved root. A rule entry is
// a mapping carrying an optional `when` plus any subset of the three knobs.
//
// The order is the assertion that matters, because first-match is what makes it semantic:
// a resolver that sorted the list, or that collected the rules into a map, would still
// produce a working configuration and would decide a different file by a different rule.
// The two rules here are deliberately written out of band order so that "as written" and
// "sorted by band" are different answers.
//
// MUTATION: build the list from a map, or sort it by band, and the order assertion reds.
func TestRules_AListIsAcceptedAndKeepsItsWrittenOrder(t *testing.T) {
	rules := rulesOf(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          min_source_height: 2160
        crf: 18
      - when:
          max_source_height: 576
        min_bitrate_kbps: 800
        min_savings_percent: 5
      - crf: 24
`)
	if len(rules) != 3 {
		t.Fatalf("the root resolved %d rule(s), want 3", len(rules))
	}
	if rules[0].CRF == nil || *rules[0].CRF != 18 {
		t.Errorf("rules[0] is not the 2160 band written first: %v", rules[0])
	}
	if rules[1].MinBitrateKbps == nil || *rules[1].MinBitrateKbps != 800 ||
		rules[1].MinSavingsPercent == nil || *rules[1].MinSavingsPercent != 5 {
		t.Errorf("rules[1] did not carry both of the knobs it names: %v", rules[1])
	}
	if rules[1].CRF != nil {
		t.Errorf("rules[1] carries a crf it never named: %v", *rules[1].CRF)
	}
	// The third is the unconditional form: no `when` at all, which matches every file.
	if rules[2].When.Bounded() {
		t.Errorf("rules[2] carries a band, but it was written with no `when`: %v", rules[2].When)
	}
	if rules[2].CRF == nil || *rules[2].CRF != 24 {
		t.Errorf("rules[2] is not the unconditional crf 24: %v", rules[2])
	}
}

// [AC-8] IF a `when` carries a key spelled min_height or max_height, the process refuses to
// start with a message naming min_source_height / max_source_height and stating that a
// `when` bound selects which files a rule applies TO and is not an output ceiling.
//
// It is a NAMED refusal and not an unknown-key one because the confusion is specific and
// predictable: the downscale item takes `max_height` for an output ceiling, and the two
// keys are one word apart. An operator who writes `max_height: 1080` here meant either
// "apply this rule to files up to 1080p" or "write 1080p files", and being told which key
// is which is the difference between a two-minute fix and a library judged by a band its
// operator did not write.
//
// MUTATION: fold these two into the unknown-key arm and the assertions on the two correct
// spellings and on the sentence about ceilings all red.
func TestRules_AWhenBoundSpelledAsAnOutputCeilingIsRefusedByName(t *testing.T) {
	for _, key := range []string{"min_height", "max_height"} {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          `+key+`: 1080
        crf: 24
`)
		mentions(t, msg, key, "min_source_height", "max_source_height", "/mnt/tv", "rules[0]")
		if !strings.Contains(msg, "SOURCE") || !strings.Contains(msg, "ceiling") {
			t.Errorf("the refusal for %q does not say that a bound selects inputs rather than "+
				"shaping outputs:\n%s", key, msg)
		}
	}
}

// [AC-9] IF `rules` is present but is not a list, or a rule entry is not a mapping, the
// process refuses to start naming the library root and the rule's index, and exits
// non-zero (cli L1, and the clause secrets K5 became: configuration is validated at start
// and fails loudly).
//
// The non-zero exit is the CLI's contract and is graded where an exit code exists - at the
// command (see cmd/holdfast). What is graded here is that Load returns a refusal rather
// than a configuration, which is what makes that exit reachable: a build that accepted a
// malformed `rules` and resolved it to none would start, scan, and silently judge every
// file by the root's own thresholds.
//
// MUTATION: treat a non-list `rules` as a single-entry list (the way library_roots treats
// a bare string) and the first arm is accepted.
func TestRules_AMalformedRulesValueIsRefusedNamingTheRootAndTheIndex(t *testing.T) {
	t.Run("not a list", func(t *testing.T) {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      crf: 24
`)
		mentions(t, msg, "rules", "/mnt/tv", "list")
	})
	t.Run("an entry that is not a mapping", func(t *testing.T) {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        crf: 24
      - 1080
`)
		mentions(t, msg, "/mnt/tv", "rules[1]", "mapping")
	})
	t.Run("present with no value", func(t *testing.T) {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
`)
		mentions(t, msg, "rules", "/mnt/tv", "no value")
	})
}

// [AC-10] IF a rule carries a key that is neither `when` nor one of the three
// rule-overridable knobs, the process refuses to start naming the offending key and
// listing exactly the keys a rule may carry, read from ONE closed enumeration.
//
// The single enumeration is the half that outlives this item. A later item adds a knob to
// ruleKnobs and this message names it with no second list to edit, so the refusal an
// operator reads can never be a list of what a rule USED to accept - which is the way this
// class of message goes stale everywhere.
//
// MUTATION: write the accepted keys as a literal in the message and the derivation
// assertion below reds the moment ruleKnobs and that literal disagree.
func TestRules_AnUnknownRuleKeyIsRefusedAgainstTheClosedEnumeration(t *testing.T) {
	msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        encoder: svtav1
`)
	mentions(t, msg, "encoder", "/mnt/tv", "rules[0]", "when")
	// Every knob the enumeration holds is named by the message, read from the enumeration
	// itself rather than from a copy of it.
	for _, knob := range RuleKnobs() {
		if !strings.Contains(msg, knob) {
			t.Errorf("the refusal does not name %q, which a rule MAY carry:\n%s", knob, msg)
		}
	}
	// And a key inside `when` gets the same treatment, for the same reason: a bound nobody
	// reads is a band that silently covers more than the operator wrote.
	inner := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_width: 1024
        crf: 24
`)
	mentions(t, inner, "max_source_width", "min_source_height", "max_source_height", "rules[0]")
}

// [AC-11] IF a `when` carries min_source_height greater than max_source_height, or a bound
// that is not a positive integer, the process refuses to start naming the root, the rule
// index and the offending value(s).
//
// A band that covers nothing is refused rather than accepted as a rule that never fires,
// because "never fires" is indistinguishable from "the rule is working" until an operator
// checks a file. The non-integer arm is the one the weakly-typed decoder would swallow:
// 575.5 would become 575 and "576" would become 576 without a word, and a band whose edge
// the operator did not write selects a different set of files than the one they meant.
//
// MUTATION: read the bounds through the same weakly-typed decoder the knobs go through and
// the fraction, the word and the boolean are all accepted.
func TestRules_ABandThatCoversNothingOrIsNotPixelsIsRefused(t *testing.T) {
	t.Run("min above max", func(t *testing.T) {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          min_source_height: 1080
          max_source_height: 576
        crf: 24
`)
		mentions(t, msg, "/mnt/tv", "rules[0]", "1080", "576")
	})
	for name, value := range map[string]string{
		"a fraction":   "575.5",
		"a word":       "\"sd\"",
		"a boolean":    "true",
		"zero":         "0",
		"negative":     "-1080",
		"an empty key": "",
	} {
		t.Run(name, func(t *testing.T) {
			msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: `+value+`
        crf: 24
`)
			mentions(t, msg, "max_source_height", "rules[0]")
		})
	}
}

// [AC-12] IF a rule names no knob at all, the process refuses to start, stating that such a
// rule would silently shadow every rule after it under first-match.
//
// It is the one refusal here that is about the ORDER rather than about a value. A rule with
// a band and no knob matches, wins, and changes nothing - so every rule after it is
// unreachable for the files it took, and nothing in the operator's file says so. Refusing
// it is what keeps first-match a rule an operator can reason about from the page.
//
// MUTATION: accept a knobless rule and the file below starts, with the second rule silently
// never applying to anything at or below 576.
func TestRules_ARuleNamingNoKnobIsRefusedAsAShadow(t *testing.T) {
	msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
      - when:
          max_source_height: 1080
        crf: 24
`)
	mentions(t, msg, "/mnt/tv", "rules[0]", "shadow")
	// And the same for a rule that carries neither a band nor a knob, which is the same
	// mistake with nothing at all in it.
	bare := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - {}
`)
	mentions(t, bare, "rules[0]")
}

// [AC-13] IF a rule gives a knob a value the top level would refuse, the process refuses to
// start with the SAME per-value message the top level produces, prefixed with the root and
// the rule index.
//
// Same words, and that is the criterion rather than a nicety: two copies of the same
// arithmetic drift, and the day they do, a crf of 99 inside a rule starts a run that the
// identical crf beside it would have refused. The prefix is what an operator needs on top
// - the message alone names a key, and a key appears in as many places as they have roots.
//
// MUTATION: validate a rule against a bare Profile rather than against the profile it
// produces and the top-level message for min_savings_percent stops matching.
func TestRules_ARuleValueIsRefusedInTheTopLevelsOwnWords(t *testing.T) {
	for _, tc := range []struct{ knob, value, want string }{
		{"crf", "99", "crf 99 out of range (0-51)"},
		{"min_savings_percent", "140", "min_savings_percent 140 out of range (0-99)"},
	} {
		msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        `+tc.knob+`: `+tc.value+`
`)
		if !strings.Contains(msg, tc.want) {
			t.Errorf("the refusal for %s does not carry the top level's own message %q:\n%s",
				tc.knob, tc.want, msg)
		}
		mentions(t, msg, "/mnt/tv", "rules[0]")
	}

	// The anti-vacuity arm: the SAME value at the TOP LEVEL produces the SAME sentence, so
	// what is asserted above is the top level's message and not a copy that happens to read
	// like one.
	top := refusal(t, `
library_roots:
  - /mnt/tv
crf: 99
`)
	if !strings.Contains(top, "crf 99 out of range (0-51)") {
		t.Errorf("the top level no longer produces the message the rule refusal is held to:\n%s", top)
	}
}

// [AC-14] WHEN `rules` is present and empty, it is accepted and the root behaves exactly as
// one with no `rules` key, INCLUDING producing the identical profile digest.
//
// The digest is the half with teeth. It is on every terminal row, so a digest that moved
// when an operator wrote `rules: []` would detach every row already written from the
// profile that decided it, for a configuration change that changes nothing.
//
// MUTATION: fold the rules into the digest unconditionally (an empty list rendering as an
// empty string still writes the `rules=` line) and the digests differ.
func TestRules_AnEmptyListIsTheSameAsNoRulesAtAll(t *testing.T) {
	empty := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    crf: 24
    rules: []
`)
	if err := empty.Validate(); err != nil {
		t.Fatalf("an empty rules list was refused: %v", err)
	}
	absent := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    crf: 24
`)
	er := rootByPath(t, empty, "/mnt/tv")
	ar := rootByPath(t, absent, "/mnt/tv")
	if len(er.Profile.Rules) != 0 {
		t.Errorf("an empty list resolved %d rule(s), want none", len(er.Profile.Rules))
	}
	if er.Profile.Digest() != ar.Profile.Digest() {
		t.Errorf("`rules: []` digests %s and no rules key digests %s - writing an empty list "+
			"detached every row already written from the profile that decided it",
			er.Profile.Digest(), ar.Profile.Digest())
	}
}

// [AC-15] IF `rules` appears as a TOP-LEVEL key, the process refuses to start and the
// refusal names the `library_roots` entry form as where rules are written.
//
// `rules` is not a top-level key, so a build that merely rejected it as unknown would be
// telling an operator it was a typo. It is not a typo: it is a rule list written one level
// too high, and the only useful message is the one that shows where it goes.
//
// MUTATION: drop the special case and the generic "unknown config key (typo?)" refusal
// stops naming library_roots and stops showing the entry form.
func TestRules_RulesAtTheTopLevelAreRefusedWithTheEntryFormNamed(t *testing.T) {
	msg := refusal(t, `
library_roots:
  - /mnt/tv
rules:
  - when:
      max_source_height: 576
    crf: 24
`)
	mentions(t, msg, "rules", "library_roots", "path")
	if strings.Contains(msg, "typo?") {
		t.Errorf("a misplaced rule list is reported as a typo, which sends an operator looking "+
			"for a spelling mistake that is not there:\n%s", msg)
	}
}

// [AC-20] WHEN a root carries no `rules`, its profile digest is IDENTICAL to the one this
// build computed for the same resolved knob values before rules existed; and WHEN two roots
// differ only in their rules, their digests differ.
//
// The first half is a GOLDEN VALUE, committed here, and it has to be: there is no earlier
// build left to diff against once the change exists, and a digest that moved would attach
// every terminal row already in the field to a profile that no longer names it.
//
// MUTATION: write the `rules=` line into the digest unconditionally and the golden value
// reds; drop the rules from the digest entirely and the second half reds.
func TestRules_TheDigestIsUnmovedWithoutRulesAndMovesWithThem(t *testing.T) {
	// The golden: a root that overrides nothing, under a configuration that sets nothing,
	// digests exactly this. The value was READ OFF THE BUILD THAT PRECEDED THIS ITEM
	// (a37aefd, `Root.Profile.Digest()` over the same configuration) rather than off this
	// one, which is the only way the assertion means anything - a golden copied from the
	// build under test asserts that the code agrees with itself.
	const goldenDefaultProfile = "270a1ea926171dfe"
	plain := loadYAML(t, `
library_roots:
  - /mnt/tv
`)
	if got := rootByPath(t, plain, "/mnt/tv").Profile.Digest(); got != goldenDefaultProfile {
		t.Errorf("the default profile now digests %s, want the committed %s - every terminal row "+
			"already in the field records the old value and would stop naming the profile that "+
			"decided it", got, goldenDefaultProfile)
	}

	// And two roots that differ ONLY in their rules must not share an identifier.
	c := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    crf: 24
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 800
  - path: /mnt/movies
    crf: 24
`)
	banded := rootByPath(t, c, "/mnt/tv").Profile.Digest()
	plainRoot := rootByPath(t, c, "/mnt/movies").Profile.Digest()
	if banded == plainRoot {
		t.Errorf("a banded root and an identical unbanded one share the digest %s, so a row cannot "+
			"say which of the two decided it", banded)
	}

	// The same holds between two banded roots whose bands differ, which is the case a
	// digest taken over the rule COUNT rather than the rules would miss.
	d := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 800
  - path: /mnt/movies
    rules:
      - when:
          max_source_height: 720
        min_bitrate_kbps: 800
`)
	if a, b := rootByPath(t, d, "/mnt/tv").Profile.Digest(),
		rootByPath(t, d, "/mnt/movies").Profile.Digest(); a == b {
		t.Errorf("two roots whose bands differ share the digest %s", a)
	}
}

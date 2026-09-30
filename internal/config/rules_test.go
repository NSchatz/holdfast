package config

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/encoder"
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
        min_vmaf: 90
`)
	// The fixture's key was `encoder` until S0165 made it a rule knob; it is now a VMAF key,
	// which stays refused (S0165 AC-7), and the message still lists the closed enumeration.
	mentions(t, msg, "min_vmaf", "/mnt/tv", "rules[0]", "when")
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

// ---- S0165: per-rule encoder selection ----
//
// A rule may name the `encoder` its band is written with; every VMAF key stays the root's.
// Each test below names the S0165 criterion it grades.

// s0165GoldenConfigs are the configurations whose digest and recorded rule text AC-2 pins.
// None of them has a rule naming an encoder, and together they cover every rule knob the
// pinned build accepted, a band bounded on each side and on both, an unconditional rule, a
// root overriding the encoder itself, and a root with no rules at all.
var s0165GoldenConfigs = []string{`
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 800
        min_savings_percent: 5
      - when:
          min_source_height: 721
          max_source_height: 1080
        crf: 26
      - when:
          min_source_height: 2160
        crf: 18
        max_height: 1080
      - crf: 24
`, `
library_roots:
  - path: /mnt/tv
    encoder: svtav1
    crf: 30
    rules:
      - when:
          max_source_height: 720
        crf: 32
`, `
library_roots:
  - /mnt/tv
`}

// [S0165 AC-2] WHEN no rule names an encoder, each root's profile digest and its recorded
// rule-list decision input text (Rules.Canonical, the InputRules value) are the ones the
// pinned build b7c26ca produces for the identical configuration.
//
// The goldens were READ OFF b7c26ca (and, identically, off this goal's start bf36b9c), by
// running the same three configurations through `Root.Profile.Digest()` and
// `Profile.Rules.Canonical()` on that tree - never off the build under test, which would
// only prove the code agrees with itself. A move here detaches every terminal row already in
// the field from the profile that decided it and re-opens every banded row once.
//
// MUTATION: render the encoder into a rule's canonical text unconditionally (an absent one
// as `encoder=`) and every banded golden reds; add encoder to the digest's rules line by any
// other route and the digests red.
func TestS0165_AC2_AnEncoderFreeRuleListDigestsAndRecordsAsThePinnedBuild(t *testing.T) {
	golden := []struct{ digest, canonical string }{
		{"58bf2544119bc088", "[max_source_height=576,min_bitrate_kbps=800,min_savings_percent=5];" +
			"[min_source_height=721,max_source_height=1080,crf=26];" +
			"[min_source_height=2160,crf=18,max_height=1080];[crf=24]"},
		{"b6100ba709f45fec", "[max_source_height=720,crf=32]"},
		{"270a1ea926171dfe", ""},
	}
	for i, y := range s0165GoldenConfigs {
		c := loadYAML(t, y)
		if err := c.Validate(); err != nil {
			t.Fatalf("golden configuration %d was refused: %v", i, err)
		}
		p := rootByPath(t, c, "/mnt/tv").Profile
		if got := p.Digest(); got != golden[i].digest {
			t.Errorf("configuration %d digests %s, want b7c26ca's %s", i, got, golden[i].digest)
		}
		if got := p.Rules.Canonical(); got != golden[i].canonical {
			t.Errorf("configuration %d records rules=%q, want b7c26ca's %q", i, got, golden[i].canonical)
		}
	}
}

// [S0165 AC-1, config half] A rule naming `encoder` and no other knob is a valid rule, and
// the profile it resolves carries that encoder for the band while a height outside the band
// keeps the root's. The engine half (the argv) is in internal/engine/rules_test.go.
//
// MUTATION: leave `encoder` out of applyTo and the in-band assertion reds; drop it from
// ruleKnobs and the rule is refused as naming no knob.
func TestS0165_AC1_ARuleNamingOnlyAnEncoderIsValidAndResolvesForItsBand(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          max_source_height: 576
        encoder: svtav1
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("a rule naming only an encoder was refused: %v", err)
	}
	p := rootByPath(t, c, "/mnt/tv").Profile
	if got := p.WithRules(480).Encoder; got != "svtav1" {
		t.Errorf("a 480-line source resolves encoder %q, want the rule's svtav1", got)
	}
	if got := p.WithRules(1080).Encoder; got != "cpu" {
		t.Errorf("a 1080-line source resolves encoder %q, want the root's cpu", got)
	}
	if got := p.Rules[0].Knobs(); len(got) != 1 || got[0] != "encoder" {
		t.Errorf("the rule names knobs %v, want [encoder]", got)
	}
	// And the settings a job gets carry it: TranscodeIn is what the engine reads.
	if ts := c.TranscodeIn(p.WithRules(480), "/mnt/tv/ep.mkv"); ts.Encoder != "svtav1" {
		t.Errorf("the job settings carry encoder %q, want svtav1", ts.Encoder)
	}
}

// [S0165 AC-7] IF a rule carries any VMAF key, the configuration is refused naming the root,
// the rule index and the key. The floors are the root's: a rule may change which encoder
// writes a band, never the floor that band is judged against. (The exit code and the
// untouched library are graded at the command, in cmd/holdfast/preflight_test.go.)
//
// MUTATION: admit any of the six into ruleKnobs and its arm is accepted.
func TestS0165_AC7_EveryVmafKeyInARuleIsRefusedByName(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"min_vmaf", "80"},
		{"vmaf_min_pool", "40"},
		{"vmaf_min_chroma", "20"},
		{"vmaf_enable", "false"},
		{"vmaf_subsample", "2"},
		{"vmaf_model", "vmaf_v0.6.1"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        crf: 30
      - when:
          max_source_height: 1080
        encoder: svtav1
        `+tc.key+`: `+tc.value+`
`)
			mentions(t, msg, "/mnt/tv", "rules[1]", tc.key, "floor")
		})
	}
}

// [S0165 AC-8] IF a rule's `encoder` is empty, absent-valued, not text, or not a key or
// ffmpeg-codec alias this build ships, the configuration is refused naming the root, the
// rule index and the value, listing the known encoders; an alias is accepted and behaves
// exactly as its key.
//
// The refusal is validateEncoderKey's - the function an encode profile's encoder is judged
// by - so a rule accepts what an encode profile accepts and nothing else.
//
// MUTATION: judge the rule's encoder with Profile.validate (which reads "" as inherit) and
// the empty arms are accepted; skip the check and the unknown arm is accepted.
func TestS0165_AC8_ARuleEncoderIsJudgedAsAnEncodeProfilesIs(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"unknown":       {"hevc_turbo", `"hevc_turbo"`},
		"empty":         {`""`, `""`},
		"no value":      {"", `""`},
		"not text":      {"5", "5"},
		"a list":        {"[cpu]", "cpu"},
		"top-level-ish": {"auto_detect_everything", `"auto_detect_everything"`},
	} {
		t.Run(name, func(t *testing.T) {
			msg := refusal(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        crf: 30
      - when:
          max_source_height: 720
        encoder: `+tc.value+`
`)
			mentions(t, msg, "/mnt/tv", "rules[1]", "encoder", tc.want, "known")
			for _, k := range []string{"cpu", "svtav1", "nvenc"} {
				if !strings.Contains(msg, k) {
					t.Errorf("the refusal does not list the known encoder %q:\n%s", k, msg)
				}
			}
		})
	}

	// The alias arm: hevc_nvenc and nvenc resolve to one Spec, as they do everywhere else.
	for _, alias := range [][2]string{{"hevc_nvenc", "nvenc"}, {"libsvtav1", "svtav1"}} {
		c := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        encoder: `+alias[0]+`
`)
		if err := c.Validate(); err != nil {
			t.Fatalf("the alias %s was refused: %v", alias[0], err)
		}
		ts := c.TranscodeIn(rootByPath(t, c, "/mnt/tv").Profile.WithRules(480), "/mnt/tv/a.mkv")
		got, ok := encoderLookup(ts.Encoder)
		want, _ := encoderLookup(alias[1])
		if !ok || got != want {
			t.Errorf("the alias %s resolves to %+v (ok=%v), want the Spec of %s: %+v", alias[0], got, ok, alias[1], want)
		}
	}
}

// [S0165 AC-10] IF a rule names an encoder under a root whose RESOLVED remux_only is true,
// the configuration is refused naming the root, the rule index, `encoder` and `remux_only`:
// the rule's encoder could never run. Resolved, so a remux_only inherited from the top level
// counts as much as one the root writes; and a root that does not remux accepts the rule.
//
// MUTATION: check the root's WRITTEN remux_only rather than the resolved one and the
// inherited arm is accepted; drop the check and both refusal arms are accepted.
func TestS0165_AC10_ARuleEncoderUnderARemuxOnlyRootIsRefused(t *testing.T) {
	for name, cfg := range map[string]string{
		"written on the root": `
library_roots:
  - path: /mnt/tv
    remux_only: true
    rules:
      - when:
          max_source_height: 576
        encoder: svtav1
`,
		"inherited from the top level": `
remux_only: true
library_roots:
  - path: /mnt/tv
    rules:
      - when:
          max_source_height: 576
        crf: 30
      - encoder: svtav1
`,
	} {
		t.Run(name, func(t *testing.T) {
			msg := refusal(t, cfg)
			mentions(t, msg, "/mnt/tv", "rules[", "encoder", "remux_only")
		})
	}
	c := loadYAML(t, `
remux_only: true
library_roots:
  - path: /mnt/tv
    remux_only: false
    rules:
      - when:
          max_source_height: 576
        encoder: svtav1
`)
	if err := c.Validate(); err != nil {
		t.Errorf("a rule encoder under a root that resolves remux_only false was refused: %v", err)
	}
}

// [S0165 AC-14] Two roots that differ only in the encoder one of their rules names, or in
// whether a rule names one, carry different profile digests.
//
// MUTATION: leave `encoder` out of the rule's canonical text and all three digests coincide.
func TestS0165_AC14_ARuleEncoderMovesTheDigest(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - path: /mnt/a
    rules:
      - when:
          max_source_height: 576
        crf: 30
  - path: /mnt/b
    rules:
      - when:
          max_source_height: 576
        crf: 30
        encoder: svtav1
  - path: /mnt/c
    rules:
      - when:
          max_source_height: 576
        crf: 30
        encoder: cpu
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	a := rootByPath(t, c, "/mnt/a").Profile.Digest()
	b := rootByPath(t, c, "/mnt/b").Profile.Digest()
	d := rootByPath(t, c, "/mnt/c").Profile.Digest()
	if a == b || a == d || b == d {
		t.Errorf("digests a=%s b=%s c=%s: roots differing only in a rule's encoder share one", a, b, d)
	}
	if got := rootByPath(t, c, "/mnt/b").Profile.Rules.Canonical(); got != "[max_source_height=576,crf=30,encoder=svtav1]" {
		t.Errorf("the rule renders %q, want the encoder after the numeric knobs", got)
	}
}

// [S0165 AC-16] IF a ceiling in force for a band writes that band's output at a height the
// first-match resolution puts under a different rule (or under none) AND the two resolved
// encoders target different codecs, the configuration is refused naming the root, both
// bands, the ceiling and both codecs.
//
// The output height is read off downscale.Resolve, the computation the encode's scale
// filter is built from (verdict F4), and the edge arms sit exactly on it: an output at 1080
// is decided by a band whose lower bound is 1080 and not by one whose lower bound is 1081, so
// a check that took the output as the source's height, or as the ceiling plus or minus one,
// reds one of them.
//
// MUTATION: skip bandCrossing and every refused arm is accepted; compare rule indexes only
// (not codecs) and the same-codec arm is refused; take the output height as the source's
// own and the edge arms swap.
func TestS0165_AC16_ACeilingThatWritesIntoAnotherCodecsBandIsRefused(t *testing.T) {
	refused := map[string]struct {
		cfg  string
		want []string
	}{
		"the spec's fixture: an av1 band capped into the root's hevc": {`
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          min_source_height: 1081
        max_height: 1080
        encoder: svtav1
`, []string{"/mnt/tv", "rules[0]", "root's own profile", "max_height 1080", "av1", "hevc"}},
		"the root's own ceiling writes into an av1 band": {`
library_roots:
  - path: /mnt/tv
    encoder: cpu
    max_height: 720
    rules:
      - when:
          max_source_height: 720
        encoder: svtav1
`, []string{"/mnt/tv", "root's own profile", "rules[0]", "max_height 720", "av1", "hevc"}},
		"edge: the output lands one line below the next band": {`
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          min_source_height: 1082
        max_height: 1080
        encoder: svtav1
      - when:
          min_source_height: 1081
          max_source_height: 1081
        encoder: svtav1
`, []string{"rules[0]", "root's own profile", "max_height 1080", "av1", "hevc"}},
		"a rule-to-rule crossing names both rules": {`
library_roots:
  - path: /mnt/tv
    encoder: svtav1
    rules:
      - when:
          min_source_height: 1500
        max_height: 1080
      - when:
          min_source_height: 721
          max_source_height: 1499
        encoder: cpu
`, []string{"rules[0]", "rules[1]", "max_height 1080", "av1", "hevc"}},
	}
	for name, tc := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			mentions(t, refusal(t, tc.cfg), tc.want...)
		})
	}

	accepted := map[string]string{
		"the spec's fixture under a root whose 1080 output resolves to av1": `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          min_source_height: 1081
        max_height: 1080
        encoder: svtav1
      - when:
          min_source_height: 721
        encoder: svtav1
`,
		"edge: the output lands exactly on the next band's lower bound": `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          min_source_height: 1082
        max_height: 1080
        encoder: svtav1
      - when:
          min_source_height: 1080
          max_source_height: 1081
        encoder: svtav1
`,
		"a crossing into a band of the same codec": `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          min_source_height: 1081
        max_height: 1080
        crf: 26
`,
		"a band whose sources never exceed its ceiling": `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          max_source_height: 1080
        max_height: 1080
        encoder: svtav1
`,
		"the pinned golden configuration, ceiling and all": s0165GoldenConfigs[0],
	}
	for name, cfg := range accepted {
		t.Run("accepted: "+name, func(t *testing.T) {
			c, err := load(t, cfg)
			if err == nil {
				err = c.Validate()
			}
			if err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}
}

// [S0165 AC-16, the helper] bandName and targetCodecOf are the two renderings the refusal is
// built from; an unresolvable key is never taken to agree with a different one.
func TestS0165_AC16_AnUnresolvableEncoderNeverAgreesWithAnother(t *testing.T) {
	if targetCodecOf("cpu") != "hevc" || targetCodecOf("svtav1") != "av1" {
		t.Errorf("targetCodecOf(cpu)=%q targetCodecOf(svtav1)=%q", targetCodecOf("cpu"), targetCodecOf("svtav1"))
	}
	if targetCodecOf("x-one") == targetCodecOf("x-two") {
		t.Error("two unresolvable encoder keys were taken to target one codec")
	}
	if !strings.Contains(targetCodecOf("x-one"), "x-one") {
		t.Errorf("an unresolvable key's codec does not name it: %q", targetCodecOf("x-one"))
	}
	if bandName(-1) == bandName(0) || bandName(2) != "rules[2]" {
		t.Errorf("bandName(-1)=%q bandName(0)=%q bandName(2)=%q", bandName(-1), bandName(0), bandName(2))
	}
}

// encoderLookup is encoder.Lookup, named here so the AC-8 alias arm compares Specs.
var encoderLookup = encoder.Lookup

// [S0165 AC-9, config half] RuleEncoders is every encoder a rule names, deduplicated, in
// configuration order, each with the FIRST root and rule index naming it - the list the
// startup preflight walks. A rule naming no encoder contributes nothing.
//
// MUTATION: skip the dedupe and svtav1 appears twice; record the last naming rule and the
// index reds.
func TestS0165_AC9_RuleEncodersIsEveryEncoderARuleNames(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - path: /mnt/a
    rules:
      - when:
          max_source_height: 576
        crf: 30
      - when:
          max_source_height: 720
        encoder: svtav1
  - path: /mnt/b
    rules:
      - encoder: svtav1
  - path: /mnt/c
    rules:
      - when:
          max_source_height: 576
        encoder: cpu
`)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := c.RuleEncoders()
	want := []RuleEncoder{{Key: "svtav1", Root: "/mnt/a", Rule: 1}, {Key: "cpu", Root: "/mnt/c", Rule: 0}}
	if len(got) != len(want) {
		t.Fatalf("RuleEncoders = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RuleEncoders[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if none := loadYAML(t, s0165GoldenConfigs[0]).RuleEncoders(); len(none) != 0 {
		t.Errorf("a rule list naming no encoder yields %+v, want nothing", none)
	}
}

package config

import (
	"fmt"
	"strings"
	"testing"
)

// Queue PRIORITY as configuration (docs/design/queue-order.md#priority): where it may be
// written, the range it is held to, how one file's priority is chosen, and the property the
// whole key rests on - it decides nothing a terminal row records, so it moves no digest, no
// rule text and no encode setting.

// priorityLibrary is the configuration the resolution cases read: a root with a priority, a
// root without one, rules with and without priorities (one banded, one priority-only), and
// encode profiles with and without one.
const priorityLibrary = `
library_roots:
  - path: /mnt/films
    priority: 5
    rules:
      - when: {max_source_height: 576}
        crf: 26
        priority: 50
      - when: {min_source_height: 2000}
        crf: 20
      - when: {min_source_height: 1000}
        priority: -7
  - path: /mnt/tv
encode_profiles:
  - name: anime
    match: "**/anime/**"
    crf: 24
    priority: 20
  - name: plain
    match: "*.avi"
    crf: 23
`

// TestPriority_IsAcceptedOnARootARuleAndAnEncodeProfile reads all three spellings back off
// a loaded configuration, including a rule that names ONLY a priority.
func TestPriority_IsAcceptedOnARootARuleAndAnEncodeProfile(t *testing.T) {
	c := loadYAML(t, priorityLibrary)
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	films := rootByPath(t, c, "/mnt/films")
	if films.Priority == nil || *films.Priority != 5 {
		t.Errorf("root priority = %v, want 5", films.Priority)
	}
	if tv := rootByPath(t, c, "/mnt/tv"); tv.Priority != nil {
		t.Errorf("a root naming no priority carries %d", *tv.Priority)
	}
	rules := films.Profile.Rules
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	if rules[0].Priority == nil || *rules[0].Priority != 50 {
		t.Errorf("rules[0] priority = %v, want 50", rules[0].Priority)
	}
	if rules[1].Priority != nil {
		t.Errorf("rules[1] names no priority and carries %d", *rules[1].Priority)
	}
	if rules[2].Priority == nil || *rules[2].Priority != -7 || len(rules[2].Knobs()) != 0 {
		t.Errorf("rules[2] = %+v, want a priority-only rule at -7", rules[2])
	}
	if p := c.EncodeProfiles[0].Priority; p == nil || *p != 20 {
		t.Errorf("encode_profiles[0] priority = %v, want 20", p)
	}
	if p := c.EncodeProfiles[1].Priority; p != nil {
		t.Errorf("encode_profiles[1] names no priority and carries %d", *p)
	}
	if !c.PriorityConfigured() {
		t.Error("PriorityConfigured() is false for a configuration that names three")
	}

	// `validate` prints a rule's priority on the rule's own line, after its knobs.
	if got, want := rules[0].String(), "source height any to 576: crf=26 priority=50"; got != want {
		t.Errorf("rules[0].String() = %q, want %q", got, want)
	}
	if got, want := rules[2].String(), "source height 1000 to any: priority=-7"; got != want {
		t.Errorf("rules[2].String() = %q, want %q", got, want)
	}
}

// TestPriority_AFileTakesItsRuleThenItsEncodeProfileThenItsRoot grades the precedence, and
// that FIRST MATCH still decides everything: a matching rule that names no priority does not
// hand the question to a later rule, and neither does a matching encode profile.
func TestPriority_AFileTakesItsRuleThenItsEncodeProfileThenItsRoot(t *testing.T) {
	c := loadYAML(t, priorityLibrary)
	films := rootByPath(t, c, "/mnt/films")
	tv := rootByPath(t, c, "/mnt/tv")
	for _, tc := range []struct {
		name   string
		root   Root
		path   string
		height int
		want   int
	}{
		{"the matching rule's, over the encode profile's and the root's", films, "/mnt/films/anime/a.mkv", 480, 50},
		{"a priority-only rule's", films, "/mnt/films/x.mkv", 1080, -7},
		{"a matching rule naming none falls to the encode profile, not to a later rule", films,
			"/mnt/films/anime/b.mkv", 2160, 20},
		{"no matching rule and no encode profile priority: the root's", films, "/mnt/films/c.avi", 720, 5},
		{"no rule, no encode profile: the root's", films, "/mnt/films/d.mkv", 720, 5},
		{"a root naming none: the encode profile's", tv, "/mnt/tv/anime/e.mkv", 1080, 20},
		{"the first matching encode profile names none: 0, not a later profile's", tv, "/mnt/tv/f.avi", 1080, 0},
		{"nothing names one: 0", tv, "/mnt/tv/g.mkv", 1080, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.PriorityOf(tc.root, tc.path, tc.height); got != tc.want {
				t.Errorf("PriorityOf(%s, height %d) = %d, want %d", tc.path, tc.height, got, tc.want)
			}
		})
	}

	// The second matching encode profile names a priority; the first names none, so the
	// first decides and the file is 0 - first match wins for priority as for every setting.
	c2 := loadYAML(t, `
library_roots:
  - /mnt/tv
encode_profiles:
  - name: first
    match: "*.mkv"
    crf: 23
  - name: second
    match: "*.mkv"
    priority: 9
`)
	if got := c2.PriorityOf(rootByPath(t, c2, "/mnt/tv"), "/mnt/tv/a.mkv", 0); got != 0 {
		t.Errorf("a file whose first matching encode profile names no priority got %d, want 0", got)
	}
	if ep, ok := c2.EncodeProfileFor("/mnt/tv/a.mkv"); !ok || ep.Name != "first" {
		t.Errorf("EncodeProfileFor = %v %v, want the first matching profile", ep.Name, ok)
	}
	if _, ok := c2.EncodeProfileFor("/mnt/tv/a.avi"); ok {
		t.Error("EncodeProfileFor matched a file no profile selects")
	}
}

// TestPriority_TheSourceHeightIsNeededOnlyForABandedRulesPriority grades when the ordering
// has to probe a height for a priority: only where a rule names one and some rule bands.
func TestPriority_TheSourceHeightIsNeededOnlyForABandedRulesPriority(t *testing.T) {
	five := 5
	bound := 576
	for _, tc := range []struct {
		name string
		root Root
		want bool
	}{
		{"no rules", Root{Priority: &five}, false},
		{"banded rules naming no priority", Root{Profile: Profile{Rules: Rules{
			{When: Band{MaxSourceHeight: &bound}, CRF: &five}}}}, false},
		{"an unbanded rule naming a priority", Root{Profile: Profile{Rules: Rules{
			{CRF: &five, Priority: &five}}}}, false},
		{"a banded rule naming a priority", Root{Profile: Profile{Rules: Rules{
			{When: Band{MaxSourceHeight: &bound}, Priority: &five}}}}, true},
		{"an unbanded priority after a banded rule", Root{Profile: Profile{Rules: Rules{
			{When: Band{MaxSourceHeight: &bound}, CRF: &five}, {Priority: &five}}}}, true},
	} {
		if got := tc.root.PriorityNeedsSourceHeight(); got != tc.want {
			t.Errorf("%s: PriorityNeedsSourceHeight() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPriority_ReopensNoRowAndMovesNoDigest is the property priority rests on: the same
// configuration with and without priorities resolves to the SAME profile digest, the same
// rule canonical text and the same encode settings for every file. The engine's half (the
// decision inputs a terminal row records) is TestQueueOrder_PriorityIsInNoDecisionInput.
func TestPriority_ReopensNoRowAndMovesNoDigest(t *testing.T) {
	without := loadYAML(t, strings.NewReplacer(
		"    priority: 5\n", "",
		"        priority: 50\n", "",
		"    priority: 20\n", "",
		"      - when: {min_source_height: 1000}\n        priority: -7\n", "      - when: {min_source_height: 1000}\n        crf: 19\n",
	).Replace(priorityLibrary))
	with := loadYAML(t, strings.Replace(priorityLibrary,
		"      - when: {min_source_height: 1000}\n        priority: -7\n",
		"      - when: {min_source_height: 1000}\n        crf: 19\n        priority: -7\n", 1))
	if without.PriorityConfigured() {
		t.Fatal("the stripped configuration still names a priority")
	}
	for _, clean := range []string{"/mnt/films", "/mnt/tv"} {
		a, b := rootByPath(t, without, clean), rootByPath(t, with, clean)
		if a.Profile.Digest() != b.Profile.Digest() {
			t.Errorf("%s: digest %s without priorities, %s with: a priority must move no digest",
				clean, a.Profile.Digest(), b.Profile.Digest())
		}
		if a.Profile.Rules.Canonical() != b.Profile.Rules.Canonical() {
			t.Errorf("%s: rule text %q without priorities, %q with", clean,
				a.Profile.Rules.Canonical(), b.Profile.Rules.Canonical())
		}
		for _, f := range []string{"/anime/a.mkv", "/b.avi", "/c.mkv"} {
			for _, h := range []int{480, 1080, 2160} {
				ta := without.TranscodeIn(a.Profile.WithRules(h), clean+f)
				tb := with.TranscodeIn(b.Profile.WithRules(h), clean+f)
				if ta != tb {
					t.Errorf("%s%s at %d: settings %+v without priorities, %+v with", clean, f, h, ta, tb)
				}
			}
		}
	}
}

// TestPriority_OutOfRangeOrNotAWholeNumberIsRefusedNamingTheKey holds all three places to
// one range and one reading of a number, each refusal naming the key and where it was.
func TestPriority_OutOfRangeOrNotAWholeNumberIsRefusedNamingTheKey(t *testing.T) {
	places := map[string]func(v string) string{
		"root": func(v string) string {
			return "library_roots:\n  - path: /mnt/films\n    priority: " + v + "\n"
		},
		"rule": func(v string) string {
			return "library_roots:\n  - path: /mnt/films\n    rules:\n      - crf: 20\n        priority: " + v + "\n"
		},
		"encode profile": func(v string) string {
			return "library_roots:\n  - /mnt/films\nencode_profiles:\n  - name: p\n    match: \"*.mkv\"\n    priority: " + v + "\n"
		},
	}
	where := map[string]string{"root": "library_roots[0]", "rule": "rules[0]", "encode profile": "encode_profiles[0]"}
	for place, yaml := range places {
		for _, v := range []string{"1001", "-1001", "2.5", "high", "true", "1e9", ""} {
			t.Run(fmt.Sprintf("%s/%q", place, v), func(t *testing.T) {
				msg := refusal(t, yaml(v))
				mentions(t, msg, "priority", where[place])
			})
		}
		for _, v := range []string{"1000", "-1000", "0", "\"12\""} {
			t.Run(fmt.Sprintf("%s/accepts %s", place, v), func(t *testing.T) {
				c, err := load(t, yaml(v))
				if err == nil {
					err = c.Validate()
				}
				if err != nil {
					t.Errorf("priority %s was refused: %v", v, err)
				}
			})
		}
	}

	// A Config assembled in Go meets the same range through Validate.
	big := 5000
	for name, c := range map[string]Config{
		"root": {Roots: []Root{{Path: "/mnt/a", Clean: "/mnt/a", Priority: &big}}},
		"rule": {Roots: []Root{{Path: "/mnt/a", Clean: "/mnt/a",
			Profile: Profile{Rules: Rules{{CRF: &big, Priority: &big}}}}}},
		"encode profile": {EncodeProfiles: []EncodeProfile{{Name: "p", Priority: &big}}},
	} {
		if err := c.validatePriorities(); err == nil || !strings.Contains(err.Error(), "priority 5000 out of range") {
			t.Errorf("%s: validatePriorities() = %v, want the range refusal", name, err)
		}
	}
	if err := (&Config{}).validatePriorities(); err != nil {
		t.Errorf("a Config naming no priority was refused: %v", err)
	}
}

// TestPriority_IsRefusedByNameAtTheTopLevel: a top-level `priority` (the file or
// HOLDFAST_PRIORITY) selects nothing, so it is refused saying where the key belongs rather
// than as an unknown key.
func TestPriority_IsRefusedByNameAtTheTopLevel(t *testing.T) {
	_, err := load(t, "library_roots:\n  - /mnt/films\npriority: 5\n")
	if err == nil {
		t.Fatal("a top-level priority was accepted")
	}
	mentions(t, err.Error(), "priority", "TOP-LEVEL", "library_roots entry", "encode_profiles")

	t.Setenv("HOLDFAST_PRIORITY", "5")
	_, err = load(t, "library_roots:\n  - /mnt/films\n")
	if err == nil {
		t.Fatal("HOLDFAST_PRIORITY was accepted")
	}
	mentions(t, err.Error(), "HOLDFAST_PRIORITY", "TOP-LEVEL")
}

// TestPriority_ARuleNamingNothingAtAllIsStillRefused: allowing a priority-only rule did not
// open the door to a rule naming nothing, which still shadows without doing anything.
func TestPriority_ARuleNamingNothingAtAllIsStillRefused(t *testing.T) {
	msg := refusal(t, "library_roots:\n  - path: /mnt/tv\n    rules:\n      - when: {max_source_height: 576}\n")
	mentions(t, msg, "rules[0]", "shadow", "priority")
	// And an unknown key in a rule names priority among what a rule may carry.
	msg = refusal(t, "library_roots:\n  - path: /mnt/tv\n    rules:\n      - priorty: 3\n")
	mentions(t, msg, "priorty", "priority")
	// And an unknown key in a root entry names it too.
	msg = refusal(t, "library_roots:\n  - path: /mnt/tv\n    priorty: 3\n")
	mentions(t, msg, "priorty", "priority")
	// And in an encode profile.
	msg = refusal(t, "library_roots:\n  - /mnt/tv\nencode_profiles:\n  - name: p\n    priorty: 3\n")
	mentions(t, msg, "priorty", "priority")
}

// TestPriority_AConfigurationNamingNoneReportsNone keeps every earlier configuration's queue
// exactly what it was: nothing anywhere names a priority, so nothing is ordered by one.
func TestPriority_AConfigurationNamingNoneReportsNone(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - path: /mnt/films
    rules:
      - when: {max_source_height: 576}
        crf: 26
encode_profiles:
  - name: p
    match: "*.mkv"
    crf: 23
`)
	if c.PriorityConfigured() {
		t.Error("PriorityConfigured() is true for a configuration that names none")
	}
	if rootByPath(t, c, "/mnt/films").PriorityNeedsSourceHeight() {
		t.Error("a root whose banded rules name no priority reports that its priority needs a height")
	}
	if got := c.PriorityOf(rootByPath(t, c, "/mnt/films"), "/mnt/films/a.mkv", 480); got != 0 {
		t.Errorf("PriorityOf = %d with no priority anywhere, want 0", got)
	}
	if (&Config{}).PriorityConfigured() {
		t.Error("an empty Config reports a priority")
	}
}

// TestPriority_AnyOnePlaceNamingOneIsEnough: PriorityConfigured is true when exactly one of
// the three places names a priority - each on its own - since any one of them is enough to
// change the order a scan offers files in.
func TestPriority_AnyOnePlaceNamingOneIsEnough(t *testing.T) {
	for name, yaml := range map[string]string{
		"a root":            "library_roots:\n  - path: /mnt/a\n    priority: 1\n",
		"a rule":            "library_roots:\n  - path: /mnt/a\n    rules:\n      - crf: 20\n        priority: 1\n",
		"a second rule":     "library_roots:\n  - path: /mnt/a\n    rules:\n      - {when: {max_source_height: 576}, crf: 20}\n      - priority: 1\n",
		"a second root":     "library_roots:\n  - /mnt/a\n  - path: /mnt/b\n    priority: -1\n",
		"an encode profile": "library_roots:\n  - /mnt/a\nencode_profiles:\n  - name: p\n    crf: 20\n  - name: q\n    priority: 0\n",
	} {
		if c := loadYAML(t, yaml); !c.PriorityConfigured() {
			t.Errorf("%s names a priority and PriorityConfigured() is false", name)
		}
	}
}

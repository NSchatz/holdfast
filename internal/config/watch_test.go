package config

import (
	"strings"
	"testing"
)

// The per-root filesystem watch, at the configuration surface. Two questions are asked
// here and nothing else: does the opt-in reach exactly the roots that wrote it (AC-1), and
// does adding it leave every root's profile digest where it was (AC-9).

// TestWatch_OptInIsPerRootAndDefaultsOff grades AC-1: an entry that carries the opt-in is
// watched, an entry that omits it is not, and every configuration written before this
// feature existed carries neither key anywhere and so starts no watch at all.
func TestWatch_OptInIsPerRootAndDefaultsOff(t *testing.T) {
	// A flat list: the spelling every configuration written before profiles existed uses.
	flat := loadYAML(t, "library_roots:\n  - /mnt/media\n  - /mnt/tv\n")
	for _, r := range flat.RootProfiles() {
		if r.Watch.Enabled {
			t.Errorf("library root %s is watched, and nothing in the configuration asked for one", r.Clean)
		}
	}

	// Two entries, one of which opted in. The other must be untouched by it.
	c := loadYAML(t, `
library_roots:
  - path: /mnt/media
    watch: true
  - path: /mnt/tv
    crf: 20
`)
	media := rootByPath(t, c, "/mnt/media")
	if !media.Watch.Enabled {
		t.Errorf("/mnt/media carried %q and is not watched", watchKey)
	}
	if media.Watch.SettleSec != DefaultWatchSettleSec {
		t.Errorf("/mnt/media settled after %ds, want the shipped default of %ds",
			media.Watch.SettleSec, DefaultWatchSettleSec)
	}
	if got := media.Watch.Settle().Seconds(); got != float64(DefaultWatchSettleSec) {
		t.Errorf("Settle() = %gs, want %ds", got, DefaultWatchSettleSec)
	}
	if tv := rootByPath(t, c, "/mnt/tv"); tv.Watch.Enabled {
		t.Error("/mnt/tv omitted the opt-in and is watched anyway")
	}

	// The settle period is configurable, per root, beside the opt-in that gives it meaning.
	settled := rootByPath(t, loadYAML(t, `
library_roots:
  - path: /mnt/media
    watch: true
    watch_settle_sec: 5
`), "/mnt/media")
	if !settled.Watch.Enabled || settled.Watch.SettleSec != 5 {
		t.Errorf("resolved watch = %+v, want it enabled with a 5s settle", settled.Watch)
	}

	// An explicit false is off, and reads exactly as an absent key does.
	if off := rootByPath(t, loadYAML(t, "library_roots:\n  - path: /mnt/media\n    watch: false\n"), "/mnt/media"); off.Watch.Enabled {
		t.Error("watch: false resolved to a watched root")
	}

	// Every spelling that is not a per-root opt-in is a REFUSAL, in the same breath the
	// rest of an entry's keys are refused in: a watch an operator believes is running and
	// is not is the failure this feature cannot have.
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{
			name: "at the top level",
			yaml: "watch: true\nlibrary_roots:\n  - /mnt/media\n",
			want: "INSIDE a library_roots entry",
		},
		{
			name: "the settle period at the top level",
			yaml: "watch_settle_sec: 30\nlibrary_roots:\n  - /mnt/media\n",
			want: "INSIDE a library_roots entry",
		},
		{
			name: "a settle period with no watch to serve",
			yaml: "library_roots:\n  - path: /mnt/media\n    watch_settle_sec: 30\n",
			want: "would be read by nothing",
		},
		{
			name: "a non-boolean opt-in",
			yaml: "library_roots:\n  - path: /mnt/media\n    watch: maybe\n",
			want: "non-boolean",
		},
		{
			name: "a fractional settle period",
			yaml: "library_roots:\n  - path: /mnt/media\n    watch: true\n    watch_settle_sec: 2.5\n",
			want: "whole number of seconds",
		},
		{
			name: "a negative settle period",
			yaml: "library_roots:\n  - path: /mnt/media\n    watch: true\n    watch_settle_sec: -1\n",
			want: "negative",
		},
		{
			name: "an opt-in with no value",
			yaml: "library_roots:\n  - path: /mnt/media\n    watch:\n",
			want: "with no value",
		},
	} {
		_, err := load(t, tc.yaml)
		if err == nil {
			t.Errorf("%s: loaded without a refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal is %q, which does not say %q", tc.name, err, tc.want)
		}
	}

	// And the environment is a top-level layer, so it is the same misplacement.
	t.Setenv("HOLDFAST_WATCH", "true")
	if _, err := load(t, "library_roots:\n  - /mnt/media\n"); err == nil {
		t.Error("HOLDFAST_WATCH=true loaded without a refusal")
	} else if !strings.Contains(err.Error(), "INSIDE a library_roots entry") {
		t.Errorf("HOLDFAST_WATCH refusal is %q, which does not say where the key goes", err)
	}
}

// TestWatchIsNotAProfileKnobAndDoesNotMoveTheDigest grades AC-9. profileKnobs is ONE list
// with two readers - the acceptance check that refuses any other key inside an entry, and
// Digest, which is what a terminal row records the configuration it was decided under by -
// so a watch key added to it would satisfy AC-1 by re-attributing every row in the ledger.
// The watch is accepted without joining the digested set, exactly as `path` and the path
// filters are.
func TestWatchIsNotAProfileKnobAndDoesNotMoveTheDigest(t *testing.T) {
	for _, key := range WatchKeys() {
		if isProfileKnob(key) {
			t.Errorf("isProfileKnob(%q) = true: a watch decides how a file is DISCOVERED, never what is "+
				"done to it, so digesting it would re-attribute every terminal row under this root", key)
		}
		for _, knob := range ProfileKnobs() {
			if knob == key {
				t.Errorf("%q is in the digested knob set", key)
			}
		}
	}

	// The same root, the same knobs, the watch turned on beside them: one digest.
	before := loadYAML(t, `
library_roots:
  - path: /mnt/media
    crf: 20
    vmaf_min_pool: 45
`)
	after := loadYAML(t, `
library_roots:
  - path: /mnt/media
    crf: 20
    vmaf_min_pool: 45
    watch: true
    watch_settle_sec: 5
`)
	was := rootByPath(t, before, "/mnt/media").Profile.Digest()
	now := rootByPath(t, after, "/mnt/media").Profile.Digest()
	if was != now {
		t.Errorf("the profile digest moved from %s to %s when the watch was turned on: every terminal row "+
			"decided under this root would be re-offered because an operator enabled a discovery accelerator",
			was, now)
	}

	// And the resolved knobs `holdfast validate` prints are the same list in the same
	// order, so nothing the watch adds reaches what a row is attributed by.
	wasKnobs := rootByPath(t, before, "/mnt/media").Effective()
	nowKnobs := rootByPath(t, after, "/mnt/media").Effective()
	if len(wasKnobs) != len(nowKnobs) {
		t.Fatalf("the resolved knob set grew from %d to %d entries", len(wasKnobs), len(nowKnobs))
	}
	for i := range wasKnobs {
		if wasKnobs[i] != nowKnobs[i] {
			t.Errorf("resolved knob %d moved from %+v to %+v", i, wasKnobs[i], nowKnobs[i])
		}
	}

	// An entry key that is neither a knob, a filter, a rule list nor a watch key remains a
	// startup refusal: accepting the watch widened the accepted set by exactly two keys.
	if _, err := load(t, "library_roots:\n  - path: /mnt/media\n    watchh: true\n"); err == nil {
		t.Error("an unknown entry key loaded without a refusal")
	}
}

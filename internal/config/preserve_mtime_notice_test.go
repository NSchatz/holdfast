package config

// S0178, the statement half - the effective preserve_mtime choice, said out loud.
//
// The default was KEPT at true by the owner's decision, so nothing here moves it (the
// tests in preserve_mtime_test.go still pin it). What this adds is that the choice is no
// longer silent: Notices() carries exactly one line naming the key, its effective value,
// whether that value is the default or was written, and what the value hides or moves.

import (
	"strings"
	"testing"
)

// preserveMtimeNotices is every notice naming the key, for the reason readTokenNotices
// gives: "exactly one" is a claim about THIS notice, not about the list's length.
func preserveMtimeNotices(c *Config) []string {
	var out []string
	for _, n := range c.Notices() {
		if strings.Contains(n, preserveMtimeKey) {
			out = append(out, n)
		}
	}
	return out
}

// TestNotices_S0178_AC5_StatesTheEffectivePreserveMtimeChoice: every configuration gets
// exactly one line, and the line is true of that configuration in all three things it
// says - the value, where the value came from, and the consequence.
func TestNotices_S0178_AC5_StatesTheEffectivePreserveMtimeChoice(t *testing.T) {
	const (
		whenTrue  = "modification time alone"
		whenFalse = "recently added"
		isDefault = "the default: neither"
		isSet     = "set explicitly"
	)
	yes, no := true, false
	cases := []struct {
		name     string
		body     string // appended to a minimal file; "" leaves the key absent
		env      string // HOLDFAST_PRESERVE_MTIME, "" leaves it unset
		built    *Config
		value    bool
		explicit bool
	}{
		{name: "the key is absent", value: true},
		{name: "the file says true", body: "preserve_mtime: true", value: true, explicit: true},
		{name: "the file says false", body: "preserve_mtime: false", value: false, explicit: true},
		{name: "the environment says false, the file nothing", env: "false", value: false, explicit: true},
		{name: "the environment says true, the file nothing", env: "true", value: true, explicit: true},
		{name: "the environment overrides the file", body: "preserve_mtime: true", env: "false", value: false, explicit: true},
		{name: "a Config built in Go, key unset", built: &Config{LibraryRoots: []string{"/mnt/media"}}, value: true},
		{name: "a Config built in Go, set true", built: &Config{PreserveMtime: &yes}, value: true, explicit: true},
		{name: "a Config built in Go, set false", built: &Config{PreserveMtime: &no}, value: false, explicit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.built
			if c == nil {
				if tc.env != "" {
					t.Setenv("HOLDFAST_PRESERVE_MTIME", tc.env)
				}
				var err error
				if c, err = Load(writeConfig(t, tc.body)); err != nil {
					t.Fatalf("Load: %v", err)
				}
			}
			if c.PreserveMtimeEnabled() != tc.value {
				t.Fatalf("the fixture resolved preserve_mtime to %v, want %v - it proves nothing about the notice",
					c.PreserveMtimeEnabled(), tc.value)
			}
			if c.PreserveMtimeExplicit() != tc.explicit {
				t.Errorf("PreserveMtimeExplicit() = %v, want %v", c.PreserveMtimeExplicit(), tc.explicit)
			}
			got := preserveMtimeNotices(c)
			if len(got) != 1 {
				t.Fatalf("Notices() carried %d line(s) naming preserve_mtime, want exactly 1:\n%v", len(got), got)
			}
			line, low := got[0], strings.ToLower(got[0])
			if strings.ContainsAny(line, "\n\r") {
				t.Errorf("the statement spans more than one line:\n%q", line)
			}

			// The effective value, stated as the value and not merely implied.
			want, other := "preserve_mtime is true ", "preserve_mtime is false "
			say, unsay := whenTrue, whenFalse
			if !tc.value {
				want, other = other, want
				say, unsay = unsay, say
			}
			if !strings.HasPrefix(line, want) {
				t.Errorf("the line does not open with %q:\n%s", want, line)
			}
			if strings.Contains(line, other) {
				t.Errorf("the line also claims %q:\n%s", other, line)
			}
			// The consequence of THIS value, and not the other one's.
			if !strings.Contains(low, say) {
				t.Errorf("the line never says %q:\n%s", say, line)
			}
			if strings.Contains(low, unsay) {
				t.Errorf("the line for %v states the other setting's consequence (%q):\n%s", tc.value, unsay, line)
			}
			// Where the value came from, and what the default is.
			src, notSrc := isDefault, isSet
			if tc.explicit {
				src, notSrc = notSrc, src
			}
			if !strings.Contains(line, src) {
				t.Errorf("the line does not say %q:\n%s", src, line)
			}
			if strings.Contains(line, notSrc) {
				t.Errorf("the line says %q of a value that is not:\n%s", notSrc, line)
			}
			if !tc.explicit && !strings.Contains(line, "is true (the default") {
				t.Errorf("a defaulted value is not stated as the default:\n%s", line)
			}
			if tc.explicit && !strings.Contains(line, "the default is true") {
				t.Errorf("an explicit value's line does not say what the default is:\n%s", line)
			}
			if !strings.Contains(line, "HOLDFAST_PRESERVE_MTIME") {
				t.Errorf("the line does not name the environment override:\n%s", line)
			}
			// A notice, never a warning: neither value weakens a gate.
			for _, w := range c.Warnings() {
				if strings.Contains(w, preserveMtimeKey) {
					t.Errorf("preserve_mtime produced a WARNING, which is reserved for a weakened gate:\n%s", w)
				}
			}
		})
	}
}

// TestLoad_S0178_AC6_ANonBooleanPreserveMtimeIsRefusedNamingTheKey: the weakly typed
// decoder would read `3` as true and a key written with no value as the default, so the
// raw value is refused before it can be coerced - in the file and in the environment.
func TestLoad_S0178_AC6_ANonBooleanPreserveMtimeIsRefusedNamingTheKey(t *testing.T) {
	refused := func(t *testing.T, body, env string) {
		t.Helper()
		if env != "" {
			t.Setenv("HOLDFAST_PRESERVE_MTIME", env)
		}
		c, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatalf("Load accepted it and resolved preserve_mtime to %v", c.PreserveMtimeEnabled())
		}
		if !strings.Contains(err.Error(), preserveMtimeKey) {
			t.Errorf("the refusal does not name %s: %v", preserveMtimeKey, err)
		}
	}
	for _, v := range []string{"banana", "3", "0.5", "", "[true]", "{a: true}", `"yes please"`} {
		t.Run("file="+v, func(t *testing.T) { refused(t, "preserve_mtime: "+v, "") })
	}
	for _, v := range []string{"banana", "3", "maybe", " false "} {
		t.Run("env="+v, func(t *testing.T) { refused(t, "", v) })
		t.Run("env="+v+" over a valid file", func(t *testing.T) { refused(t, "preserve_mtime: true", v) })
	}

	// The control: every genuine boolean spelling still loads, to the value it names.
	for body, want := range map[string]bool{
		"preserve_mtime: true": true, "preserve_mtime: false": false,
		`preserve_mtime: "true"`: true, `preserve_mtime: "false"`: false,
	} {
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Errorf("%s was refused: %v", body, err)
			continue
		}
		if c.PreserveMtimeEnabled() != want {
			t.Errorf("%s resolved to %v", body, c.PreserveMtimeEnabled())
		}
	}
	for env, want := range map[string]bool{"true": true, "false": false, "TRUE": true, "0": false, "1": true} {
		t.Run("env accepts "+env, func(t *testing.T) {
			t.Setenv("HOLDFAST_PRESERVE_MTIME", env)
			c, err := Load(writeConfig(t, ""))
			if err != nil {
				t.Fatalf("HOLDFAST_PRESERVE_MTIME=%q was refused: %v", env, err)
			}
			if c.PreserveMtimeEnabled() != want {
				t.Errorf("HOLDFAST_PRESERVE_MTIME=%q resolved to %v, want %v", env, c.PreserveMtimeEnabled(), want)
			}
		})
	}
}

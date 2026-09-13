package config

// S0085 - the preserve_mtime key.
//
// Criterion: WHEN a configuration names no preserve_mtime, THE SYSTEM SHALL load the value
// TRUE; WHEN a configuration names it, the key SHALL be accepted rather than rejected as an
// unknown key, and its value SHALL be overridable by the environment layer the same way
// every other key is.
//
// The default is the conductor ruling of 2026-09-11, and it is the whole point of the key
// existing at all: every neighbouring tool (cp -p, rsync -a, mv) preserves a file's
// modification time across a replacement, so preserving it is the least-surprising
// behaviour and RESETTING it is the side effect. An operator who wants the mtime to say
// when the bytes were written sets preserve_mtime: false.

import "testing"

func TestPreserveMtime_AnAbsentKeyResolvesToTrue(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.PreserveMtimeEnabled() {
		t.Error("a configuration that never names preserve_mtime resolved to OFF - the default keeps causing " +
			"the harm the key exists to stop: a library-wide pass resets every file's age, and every " +
			"date-based sort in the media server sees the whole library arriving at once")
	}
}

// The default layer is the single source of defaults, so this is where "ships ON" is
// actually decided. It also pins the key as KNOWN: an unknown key is refused as a typo, so
// a key absent from that set is one an operator cannot set at all.
func TestPreserveMtime_TheShippedDefaultLayerPreservesIt(t *testing.T) {
	v, ok := defaultLayer()[preserveMtimeKey]
	if !ok {
		t.Fatalf("%s is not in the default layer, so an absent key has no defined value", preserveMtimeKey)
	}
	if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("the shipped default for %s is %#v, want the bool true", preserveMtimeKey, v)
	}
	if !knownKeys[preserveMtimeKey] {
		t.Errorf("%s is not a known key, so setting it would be rejected as a typo", preserveMtimeKey)
	}
}

func TestPreserveMtime_AnExplicitFalseIsAcceptedAndTurnsItOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, preserveMtimeKey+": false"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.PreserveMtimeEnabled() {
		t.Errorf("an explicit %s: false still preserved the modification time", preserveMtimeKey)
	}
}

func TestPreserveMtime_AnExplicitTrueIsAccepted(t *testing.T) {
	cfg, err := Load(writeConfig(t, preserveMtimeKey+": true"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.PreserveMtimeEnabled() {
		t.Errorf("an explicit %s: true did not preserve the modification time", preserveMtimeKey)
	}
}

func TestPreserveMtime_TheEnvironmentOverridesTheFile(t *testing.T) {
	// HOLDFAST_* beats the YAML file for every other key; a key that ignored the
	// environment would be the one an operator could not set the usual way.
	t.Setenv("HOLDFAST_PRESERVE_MTIME", "false")
	cfg, err := Load(writeConfig(t, preserveMtimeKey+": true"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PreserveMtimeEnabled() {
		t.Error("HOLDFAST_PRESERVE_MTIME=false did not override preserve_mtime: true in the file")
	}

	// And in the other direction, so the override is not one-way: an environment that
	// turns it back ON must beat a file that turned it off.
	t.Setenv("HOLDFAST_PRESERVE_MTIME", "true")
	cfg, err = Load(writeConfig(t, preserveMtimeKey+": false"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.PreserveMtimeEnabled() {
		t.Error("HOLDFAST_PRESERVE_MTIME=true did not override preserve_mtime: false in the file")
	}
}

// A Config built in Go rather than loaded from a file - which is what every engine test
// and every caller that constructs one directly gets - must read as the SHIPPED default
// and not as the struct zero. A plain bool field would read false there, which would make
// the in-process default the opposite of the documented one.
func TestPreserveMtime_AConfigBuiltWithoutLoadStillPreservesIt(t *testing.T) {
	var c Config
	if !c.PreserveMtimeEnabled() {
		t.Error("a Config that names no preserve_mtime reads as OFF in process, while the shipped default is ON")
	}
}

package config

import (
	"strings"
	"testing"
	"time"
)

// tautulliKeys is a configured Tautulli: an address and an api key reference. Synthetic: the
// host is a test name and the reference names a file no test reads.
const tautulliKeys = "tautulli_url: http://tautulli.test:8181\ntautulli_api_key: file:/run/secrets/tautulli-api-key\n"

// TestPlayback_OffByDefault (I5): a configuration that says nothing collects no evidence, and
// the two tuning keys read as their defaults.
func TestPlayback_OffByDefault(t *testing.T) {
	cfg, err := loadHealthYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlaybackEnabled() {
		t.Error("a configuration that writes no playback_source collects playback evidence")
	}
	if cfg.PlaybackPollMinutes != DefaultPlaybackPollMinutes || cfg.PlaybackPollInterval() != 15*time.Minute {
		t.Errorf("default poll: %d minutes (%v), want 15", cfg.PlaybackPollMinutes, cfg.PlaybackPollInterval())
	}
	if cfg.PlaybackPlayedFraction != 0.9 || cfg.EffectivePlaybackPlayedFraction() != 0.9 {
		t.Errorf("default fraction: %g (effective %g), want 0.9", cfg.PlaybackPlayedFraction, cfg.EffectivePlaybackPlayedFraction())
	}
	// Even with Tautulli configured for the streaming pause, nothing is polled until the source
	// is written.
	cfg, err = loadHealthYAML(t, tautulliKeys)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlaybackEnabled() {
		t.Error("configuring the streaming pause turned playback evidence on")
	}
}

func TestPlayback_KeysLoadFromTheFileAndTheEnvironment(t *testing.T) {
	cfg, err := loadHealthYAML(t, tautulliKeys+"playback_source: tautulli\nplayback_poll_minutes: 30\nplayback_played_fraction: 0.8\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PlaybackEnabled() || cfg.PlaybackPollInterval() != 30*time.Minute || cfg.EffectivePlaybackPlayedFraction() != 0.8 {
		t.Errorf("loaded: enabled %v, poll %v, fraction %g", cfg.PlaybackEnabled(), cfg.PlaybackPollInterval(),
			cfg.EffectivePlaybackPlayedFraction())
	}
	t.Setenv("HOLDFAST_PLAYBACK_SOURCE", "tautulli")
	t.Setenv("HOLDFAST_PLAYBACK_POLL_MINUTES", "5")
	cfg, err = loadHealthYAML(t, tautulliKeys)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PlaybackEnabled() || cfg.PlaybackPollInterval() != 5*time.Minute {
		t.Errorf("from the environment: enabled %v, poll %v", cfg.PlaybackEnabled(), cfg.PlaybackPollInterval())
	}
}

// TestPlayback_RefusalsNameTheKey: every refusal names the key and says what is accepted; each
// bound is accepted at its edge.
func TestPlayback_RefusalsNameTheKey(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       []string
	}{
		{"no tautulli at all", "playback_source: tautulli\n",
			[]string{"playback_source: tautulli", "needs tautulli_url and tautulli_api_key"}},
		{"no api key", "tautulli_url: http://tautulli.test:8181\nplayback_source: tautulli\n",
			[]string{"needs tautulli_url and tautulli_api_key"}},
		{"no address", "tautulli_api_key: file:/run/secrets/k\nplayback_source: tautulli\n",
			[]string{"needs tautulli_url and tautulli_api_key"}},
		{"plex", tautulliKeys + "playback_source: plex\n",
			[]string{"plex is not an evidence source", "cannot see a stall", "playback_source: tautulli"}},
		{"unknown source", tautulliKeys + "playback_source: jellyfin\n",
			[]string{"playback_source must be tautulli"}},
		{"poll below range", tautulliKeys + "playback_poll_minutes: -1\n",
			[]string{"playback_poll_minutes -1 out of range (1-1440; 0 means the default of 15)"}},
		{"poll above range", tautulliKeys + "playback_poll_minutes: 1441\n",
			[]string{"playback_poll_minutes 1441 out of range"}},
		{"fraction below range", tautulliKeys + "playback_played_fraction: 0.49\n",
			[]string{"playback_played_fraction 0.49 out of range (0.5-1; 0 means the default of 0.9)"}},
		{"fraction above range", tautulliKeys + "playback_played_fraction: 1.01\n",
			[]string{"playback_played_fraction 1.01 out of range"}},
		{"fraction not a number", tautulliKeys + "playback_played_fraction: .nan\n",
			[]string{"playback_played_fraction NaN out of range"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadHealthYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("accepted:\n%s", tc.yaml)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not contain %q", err, w)
				}
			}
		})
	}
	for _, ok := range []string{
		"playback_poll_minutes: 1\n", "playback_poll_minutes: 1440\n", "playback_poll_minutes: 0\n",
		"playback_played_fraction: 0.5\n", "playback_played_fraction: 1\n", "playback_played_fraction: 0\n",
	} {
		if _, err := loadHealthYAML(t, tautulliKeys+"playback_source: tautulli\n"+ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

// TestPlayback_ZeroMeansTheDefault: a Config assembled by hand carries zeros, which read as the
// shipped defaults rather than as a poll every instant or a fraction nothing reaches.
func TestPlayback_ZeroMeansTheDefault(t *testing.T) {
	var c Config
	if c.PlaybackPollInterval() != DefaultPlaybackPollMinutes*time.Minute {
		t.Errorf("zero poll minutes: %v", c.PlaybackPollInterval())
	}
	if c.EffectivePlaybackPlayedFraction() != DefaultPlaybackPlayedFraction {
		t.Errorf("zero fraction: %g", c.EffectivePlaybackPlayedFraction())
	}
	c.PlaybackPollMinutes, c.PlaybackPlayedFraction = 1, 0.5
	if c.PlaybackPollInterval() != time.Minute || c.EffectivePlaybackPlayedFraction() != 0.5 {
		t.Errorf("written values: %v, %g", c.PlaybackPollInterval(), c.EffectivePlaybackPlayedFraction())
	}
	c.PlaybackSource = "  "
	if c.PlaybackEnabled() {
		t.Error("a blank source enabled playback evidence")
	}
}

// TestTautulliPause_OnUnlessWrittenOff: the streaming pause stays what it always was unless
// tautulli_pause: false is written, in the file or the environment; a value that is not a boolean
// refuses to start, naming the key; and a Config built in Go with nothing set pauses.
func TestTautulliPause_OnUnlessWrittenOff(t *testing.T) {
	cfg, err := loadHealthYAML(t, tautulliKeys)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TautulliPauseEnabled() {
		t.Error("a configuration that does not write tautulli_pause does not pause")
	}
	cfg, err = loadHealthYAML(t, tautulliKeys+"tautulli_pause: false\nplayback_source: tautulli\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TautulliPauseEnabled() || !cfg.PlaybackEnabled() {
		t.Errorf("tautulli_pause: false: pause %v, evidence %v", cfg.TautulliPauseEnabled(), cfg.PlaybackEnabled())
	}
	if cfg, err := loadHealthYAML(t, tautulliKeys+"tautulli_pause: true\n"); err != nil || !cfg.TautulliPauseEnabled() {
		t.Errorf("tautulli_pause: true: %v", err)
	}
	if _, err := loadHealthYAML(t, tautulliKeys+"tautulli_pause: sometimes\n"); err == nil ||
		!strings.Contains(err.Error(), "tautulli_pause") {
		t.Errorf("a value that is not a boolean: %v", err)
	}
	t.Setenv("HOLDFAST_TAUTULLI_PAUSE", "false")
	if cfg, err := loadHealthYAML(t, tautulliKeys); err != nil || cfg.TautulliPauseEnabled() {
		t.Errorf("HOLDFAST_TAUTULLI_PAUSE=false: %v", err)
	}
	var c Config
	if !c.TautulliPauseEnabled() {
		t.Error("a Config built in Go does not pause")
	}
	off := false
	c.TautulliPause = &off
	if c.TautulliPauseEnabled() {
		t.Error("false did not turn the pause off")
	}
}

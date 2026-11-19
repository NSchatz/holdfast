package config

import (
	"fmt"
	"strings"
	"time"
)

// The playback evidence keys (docs/design/playback-evidence.md#playback-evidence): where
// holdfast reads the sessions played on the files it swapped, how often, and the fraction of a
// file a session must reach to count as played through. All of it is off until
// playback_source is written.
const (
	playbackSourceKey   = "playback_source"
	playbackPollKey     = "playback_poll_minutes"
	playbackFractionKey = "playback_played_fraction"
)

// PlaybackSourceTautulli is the one evidence source this build reads: Tautulli's session
// history, through the tautulli_url and tautulli_api_key the streaming pause already uses.
const PlaybackSourceTautulli = "tautulli"

// DefaultPlaybackPollMinutes is how often the evidence is polled when playback_poll_minutes is
// not written. A stall surfaces over days, so a quarter of an hour is far finer than the signal
// needs and costs Tautulli one small history page per poll.
const DefaultPlaybackPollMinutes = 15

// MaxPlaybackPollMinutes bounds the interval at one day. A longer one is not a poll anybody
// means, and the history overlap each poll re-reads is two days.
const MaxPlaybackPollMinutes = 24 * 60

// DefaultPlaybackPlayedFraction is the fraction of a file a session must reach to be played
// through when playback_played_fraction is not written: 0.9, the mark Plex and Tautulli
// themselves treat as watched by default.
const DefaultPlaybackPlayedFraction = 0.9

// MinPlaybackPlayedFraction is the lowest fraction accepted. A session that stopped half way
// through is not a playback anybody would call complete.
const MinPlaybackPlayedFraction = 0.5

// PlaybackEnabled reports whether this configuration collects playback evidence.
func (c *Config) PlaybackEnabled() bool { return strings.TrimSpace(c.PlaybackSource) != "" }

// PlaybackPollInterval is the time between two evidence polls: the configured minutes, with 0
// meaning the default exactly as it does for health_sweep_workers.
func (c *Config) PlaybackPollInterval() time.Duration {
	m := c.PlaybackPollMinutes
	if m < 1 {
		m = DefaultPlaybackPollMinutes
	}
	return time.Duration(m) * time.Minute
}

// EffectivePlaybackPlayedFraction is the played-through fraction: the configured value, with 0
// meaning the default.
func (c *Config) EffectivePlaybackPlayedFraction() float64 {
	if c.PlaybackPlayedFraction == 0 {
		return DefaultPlaybackPlayedFraction
	}
	return c.PlaybackPlayedFraction
}

// validatePlayback refuses a source this build does not read, a source whose own keys are not
// configured, and a value outside each key's range, naming the key.
func (c *Config) validatePlayback() error {
	switch src := strings.TrimSpace(c.PlaybackSource); src {
	case "":
	case PlaybackSourceTautulli:
		if strings.TrimSpace(c.TautulliURL) == "" || strings.TrimSpace(c.TautulliAPIKey) == "" {
			return fmt.Errorf("%s: %s reads Tautulli's session history, so it needs tautulli_url and "+
				"tautulli_api_key (a reference, file:<path> or cmd:<argv>) both set", playbackSourceKey, src)
		}
	case "plex":
		return fmt.Errorf("%s: plex is not an evidence source. Plex's own history records only the views "+
			"that reached its watched mark, with no offset for a session that stopped early, so it cannot see "+
			"a stall. Set %s: %s, which reads Tautulli's session history", playbackSourceKey,
			playbackSourceKey, PlaybackSourceTautulli)
	default:
		return fmt.Errorf("%s must be %s, or unset for no playback evidence: the configured value is not "+
			"one this build reads", playbackSourceKey, PlaybackSourceTautulli)
	}
	if c.PlaybackPollMinutes < 0 || c.PlaybackPollMinutes > MaxPlaybackPollMinutes {
		return fmt.Errorf("%s %d out of range (1-%d; 0 means the default of %d)", playbackPollKey,
			c.PlaybackPollMinutes, MaxPlaybackPollMinutes, DefaultPlaybackPollMinutes)
	}
	// Written as "not inside the range" rather than "below or above it", so a NaN, which
	// compares false both ways, is refused rather than accepted.
	if f := c.PlaybackPlayedFraction; f != 0 && !(f >= MinPlaybackPlayedFraction && f <= 1) {
		return fmt.Errorf("%s %g out of range (%g-1; 0 means the default of %g)", playbackFractionKey,
			f, MinPlaybackPlayedFraction, DefaultPlaybackPlayedFraction)
	}
	return nil
}

package main

import (
	"log/slog"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/playback"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/secret"
)

// tautulliClients builds the one Tautulli client from tautulli_url and the resolved
// tautulli_api_key, and says which of its two readers gets it. The client is nil when Tautulli is
// not configured. pause is the client the scheduler pauses new work on while anything streams:
// the client itself under tautulli_pause: true (the default, what every configuration did before
// the key existed), and nil under tautulli_pause: false, so playback evidence can read Tautulli's
// history without reclamation stopping every evening someone watches.
func tautulliClients(cfg *config.Config, secrets *secret.Set) (client, pause *schedule.Tautulli) {
	client = schedule.NewTautulli(cfg.TautulliURL, cfg.SecretRef("tautulli_api_key"), secrets.Get("tautulli_api_key"))
	if client != nil && cfg.TautulliPauseEnabled() {
		pause = client
	}
	return client, pause
}

// newPlaybackPoller builds the daemon's playback evidence poller, or nil when the configuration
// collects none (playback_source unset, the default) - docs/design/playback-evidence.md.
//
// The source is the Tautulli client the streaming pause already holds, so the api key is the one
// resolved at start and every failure leaves through that client's sanitizer. Server paths are
// mapped back through plex_path_map, because Tautulli names a file as Plex sees it. The poller
// reads through the reporting door and writes only its own three tables; nothing in the encode
// pipeline reads what it records, and nothing is restored on the strength of it.
func newPlaybackPoller(cfg *config.Config, tautulli *schedule.Tautulli, reads playback.Reader, writes playback.Writer,
	log *slog.Logger) *playback.Poller {
	if !cfg.PlaybackEnabled() {
		return nil
	}
	if tautulli == nil {
		// Validate refuses playback_source: tautulli without both Tautulli keys, so this is a
		// key reference that resolved to nothing. The streaming pause is off for the same reason.
		log.Warn("playback evidence is configured but the Tautulli api key resolved to an empty value, so no " +
			"history is read; check what tautulli_api_key refers to")
		return nil
	}
	src := &playback.Tautulli{API: tautulli.API}
	return playback.NewPoller(src, reads, writes, cfg.PlexPathMap, playback.RulesFor(cfg), cfg.PlaybackPollInterval(), log)
}

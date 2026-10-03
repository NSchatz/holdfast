package main

import (
	"log/slog"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/mediaclient"
	"github.com/NSchatz/holdfast/internal/secret"
)

// The media-server clients' three intervals, as `run` and `serve` use them. Production never
// changes one; a test shortens them so a drain or a held swap is graded without waiting the
// shipped interval out.
var (
	mediaDrainBound   = mediaclient.DrainBound
	mediaHoldCacheTTL = mediaclient.HoldCacheTTL
	mediaHoldPoll     = engine.DefaultPlayHoldPoll
)

// attachMediaClients wires the configured media-server clients to eng and returns the
// function that drains them, which `run` calls when its pass is over and `serve` calls at
// shutdown, after the engine has stopped (docs/design/media-clients.md#media-clients).
//
// With no target configured it changes NOTHING: eng keeps the Observer it had (or none), no
// play hold is set, no record is written, and the drain is a no-op. With one or more it
//
//   - adds the post-swap rescan to eng's Observer, beside whatever was observing already,
//     and starts its one sending goroutine;
//   - where Plex is one of them, sets eng's play hold;
//   - states once which targets are on. The record carries names and booleans and no
//     address or credential.
//
// The drain never changes an exit code: it waits at most mediaDrainBound and reports.
func attachMediaClients(eng *engine.Engine, cfg *config.Config, secrets *secret.Set, log *slog.Logger) (drain func()) {
	targets := mediaclient.TargetsFor(cfg, secrets.Get)
	hook := mediaclient.NewHook(targets, log)
	if hook == nil {
		return func() {}
	}
	if prev := eng.Observer; prev != nil {
		eng.Observer = fanout([]engine.Observer{prev, hook.Observe})
	} else {
		eng.Observer = hook.Observe
	}
	hook.Start()

	on := map[string]bool{}
	for _, t := range targets {
		on[t.Name()] = true
		if plex, ok := t.(*mediaclient.Plex); ok {
			eng.PlayHold = mediaclient.NewPlayHold(plex, mediaHoldCacheTTL, log).Held
			eng.PlayHoldPoll = mediaHoldPoll
		}
	}
	log.Info("media-server clients: after a swap each enabled target is asked once to rescan the "+
		"file's directory; a file being played in Plex is held until it stops",
		"radarr", on["radarr"], "sonarr", on["sonarr"], "plex", on["plex"], "plex_play_hold", on["plex"],
		"request_timeout", mediaclient.RequestTimeout.String(), "drain_bound", mediaDrainBound.String())
	return func() { hook.Drain(mediaDrainBound) }
}

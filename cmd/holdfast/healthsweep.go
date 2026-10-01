package main

import (
	"context"
	"log/slog"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/health"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/store"
)

// newHealthSweep builds the daemon's health sweep, or nil when the configuration schedules
// none (health_sweep_interval_hours: 0, the default).
//
// The sweep is LOWER in importance than an encode and is wired to say so: before every
// decode it starts it asks the same two things the encode workers ask between files - the
// operator's pause, then the host-fair scheduler (run window, load cap, streaming pause) -
// and it starts nothing while either says no. A decode already running finishes. It runs
// health_sweep_workers decodes at once (one by default) at the lowest CPU priority.
func newHealthSweep(cfg *config.Config, eng *engine.Engine, st store.HealthLedger, paused func() bool,
	sched *schedule.Scheduler, reporters []health.Reporter, log *slog.Logger) *health.Sweeper {
	if !cfg.HealthSweepEnabled() {
		return nil
	}
	sw := health.New(cfg.HealthSweepInterval(), cfg.EffectiveHealthSweepWorkers(), st,
		eng.HealthSources, health.FFmpeg{Bin: eng.Probe.FFmpeg}, log)
	sw.MayRun = func(ctx context.Context) (bool, string) {
		if paused() {
			return false, "paused (POST /api/resume)"
		}
		return sched.MayRunThrottled(ctx)
	}
	sw.Reporters = reporters
	return sw
}

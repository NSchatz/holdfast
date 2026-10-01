package config

import (
	"fmt"
	"time"
)

// The library health sweep's two keys (docs/design/health-sweep.md#health-sweep).
const (
	healthSweepIntervalKey = "health_sweep_interval_hours"
	healthSweepWorkersKey  = "health_sweep_workers"
)

// MaxHealthSweepIntervalHours bounds the interval at ten years. Nothing longer is a
// schedule anybody means, and the bound keeps the interval far inside a time.Duration.
const MaxHealthSweepIntervalHours = 10 * 8766

// MaxHealthSweepWorkers bounds how many sweep decodes may run at once. The sweep is the
// least important work the daemon does, and a value past this is a typo, not a plan.
const MaxHealthSweepWorkers = 16

// HealthSweepEnabled reports whether this configuration schedules a sweep.
func (c *Config) HealthSweepEnabled() bool { return c.HealthSweepIntervalHours > 0 }

// HealthSweepInterval is the time from the end of one sweep to the start of the next; 0
// when no sweep is scheduled.
func (c *Config) HealthSweepInterval() time.Duration {
	if !c.HealthSweepEnabled() {
		return 0
	}
	return time.Duration(c.HealthSweepIntervalHours) * time.Hour
}

// EffectiveHealthSweepWorkers is the decode concurrency the sweep uses: the configured
// value, with 0 meaning the default of 1 exactly as it does for workers.
func (c *Config) EffectiveHealthSweepWorkers() int {
	if c.HealthSweepWorkers < 1 {
		return 1
	}
	return c.HealthSweepWorkers
}

// validateHealthSweep refuses a value outside each key's range, naming the key, the value
// and what is accepted.
func (c *Config) validateHealthSweep() error {
	if c.HealthSweepIntervalHours < 0 || c.HealthSweepIntervalHours > MaxHealthSweepIntervalHours {
		return fmt.Errorf("%s %d must be between 0 (no health sweep, the default) and %d hours",
			healthSweepIntervalKey, c.HealthSweepIntervalHours, MaxHealthSweepIntervalHours)
	}
	if c.HealthSweepWorkers < 0 || c.HealthSweepWorkers > MaxHealthSweepWorkers {
		return fmt.Errorf("%s %d out of range (0-%d; 0 means the default of 1)",
			healthSweepWorkersKey, c.HealthSweepWorkers, MaxHealthSweepWorkers)
	}
	return nil
}

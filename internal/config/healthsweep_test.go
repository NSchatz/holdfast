package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadHealthYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("library_roots: ["+filepath.Join(dir, "lib")+"]\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// TestHealthSweep_OffByDefault (I5): a configuration that says nothing schedules no sweep.
func TestHealthSweep_OffByDefault(t *testing.T) {
	cfg, err := loadHealthYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthSweepEnabled() || cfg.HealthSweepInterval() != 0 || cfg.HealthSweepIntervalHours != 0 ||
		cfg.HealthSweepWorkers != 1 || cfg.EffectiveHealthSweepWorkers() != 1 {
		t.Errorf("defaults: interval %d (%v, enabled %v), workers %d (effective %d)", cfg.HealthSweepIntervalHours,
			cfg.HealthSweepInterval(), cfg.HealthSweepEnabled(), cfg.HealthSweepWorkers, cfg.EffectiveHealthSweepWorkers())
	}
}

func TestHealthSweep_KeysLoadFromTheFileAndTheEnvironment(t *testing.T) {
	cfg, err := loadHealthYAML(t, "health_sweep_interval_hours: 168\nhealth_sweep_workers: 2\n")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HealthSweepEnabled() || cfg.HealthSweepInterval() != 168*time.Hour || cfg.EffectiveHealthSweepWorkers() != 2 {
		t.Errorf("loaded interval %v workers %d", cfg.HealthSweepInterval(), cfg.EffectiveHealthSweepWorkers())
	}
	t.Setenv("HOLDFAST_HEALTH_SWEEP_INTERVAL_HOURS", "24")
	cfg, err = loadHealthYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthSweepInterval() != 24*time.Hour {
		t.Errorf("from the environment: %v", cfg.HealthSweepInterval())
	}
}

// TestHealthSweep_OutOfRangeRefusesNamingTheKey: each refusal names the key, the value and
// the accepted range; each bound is accepted at its edge.
func TestHealthSweep_OutOfRangeRefusesNamingTheKey(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want string
	}{
		{"health_sweep_interval_hours: -1\n", "health_sweep_interval_hours -1 must be between 0 (no health sweep, the default) and 87660 hours"},
		{"health_sweep_interval_hours: 87661\n", "health_sweep_interval_hours 87661 must be between"},
		{"health_sweep_workers: -1\n", "health_sweep_workers -1 out of range (0-16; 0 means the default of 1)"},
		{"health_sweep_workers: 17\n", "health_sweep_workers 17 out of range"},
	} {
		_, err := loadHealthYAML(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err %v, want one containing %q", tc.yaml, err, tc.want)
		}
	}
	for _, ok := range []string{"health_sweep_interval_hours: 87660\n", "health_sweep_interval_hours: 0\n",
		"health_sweep_workers: 16\n", "health_sweep_workers: 0\n"} {
		if _, err := loadHealthYAML(t, ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	c := Config{HealthSweepWorkers: 0}
	if c.EffectiveHealthSweepWorkers() != 1 {
		t.Errorf("0 workers is effectively %d", c.EffectiveHealthSweepWorkers())
	}
}

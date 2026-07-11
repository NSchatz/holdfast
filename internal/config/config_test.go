package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	tests := []struct {
		name    string
		cfg     Config
		wantErr string // substring; "" means expect success
	}{
		{
			name: "valid single root",
			cfg:  Config{LibraryRoots: []string{"/mnt/media"}},
		},
		{
			name: "valid multiple roots and log level",
			cfg:  Config{LibraryRoots: []string{"/mnt/tv", "/mnt/movies"}, LogLevel: "debug"},
		},
		{
			name:    "empty roots refused",
			cfg:     Config{LibraryRoots: nil},
			wantErr: "library_roots is empty",
		},
		{
			name:    "empty root string refused",
			cfg:     Config{LibraryRoots: []string{""}},
			wantErr: "is empty",
		},
		{
			name:    "relative root refused",
			cfg:     Config{LibraryRoots: []string{"media"}},
			wantErr: "must be an absolute path",
		},
		{
			name:    "filesystem root refused",
			cfg:     Config{LibraryRoots: []string{"/"}},
			wantErr: "filesystem root",
		},
		{
			name:    "filesystem root via dotdot refused",
			cfg:     Config{LibraryRoots: []string{"/mnt/.."}},
			wantErr: "filesystem root",
		},
		{
			name:    "home directory refused",
			cfg:     Config{LibraryRoots: []string{home}},
			wantErr: "home directory",
		},
		{
			name:    "duplicate roots refused",
			cfg:     Config{LibraryRoots: []string{"/mnt/media", "/mnt/media"}},
			wantErr: "duplicate",
		},
		{
			name:    "bad log level refused",
			cfg:     Config{LibraryRoots: []string{"/mnt/media"}, LogLevel: "loud"},
			wantErr: "log_level",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty path", func(t *testing.T) {
		if _, err := Load(""); err == nil {
			t.Fatal("Load(\"\") = nil error, want ErrNoConfig")
		}
	})

	t.Run("valid yaml round-trips and validates", func(t *testing.T) {
		p := filepath.Join(dir, "ok.yaml")
		writeFile(t, p, "library_roots:\n  - /mnt/media\nlog_level: info\ndry_run: true\n")
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(c.LibraryRoots) != 1 || c.LibraryRoots[0] != "/mnt/media" {
			t.Fatalf("LibraryRoots = %v", c.LibraryRoots)
		}
		if !c.DryRun {
			t.Fatal("DryRun = false, want true")
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("unknown key rejected", func(t *testing.T) {
		p := filepath.Join(dir, "typo.yaml")
		writeFile(t, p, "library_roots:\n  - /mnt/media\nlibrery_roots: oops\n")
		if _, err := Load(p); err == nil {
			t.Fatal("Load with unknown key = nil, want error")
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil {
			t.Fatal("Load(missing) = nil, want error")
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestApplyDefaults(t *testing.T) {
	c := Config{LibraryRoots: []string{"/mnt/media"}}
	c.ApplyDefaults()
	if c.Encoder != "cpu" || c.CRF != 22 || c.Preset != "slow" || c.ContainerExt != "mkv" {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.MinBitrateKbps != 2500 || c.MaxFailures != 3 || c.DurationToleranceSec != 1 {
		t.Errorf("numeric defaults not applied: %+v", c)
	}
	if len(c.VideoExts) == 0 {
		t.Error("video_exts default not applied")
	}
	// Explicit values are preserved.
	c2 := Config{LibraryRoots: []string{"/mnt"}, CRF: 18, Encoder: "cpu", MinBitrateKbps: 5000}
	c2.ApplyDefaults()
	if c2.CRF != 18 || c2.MinBitrateKbps != 5000 {
		t.Errorf("explicit values overwritten: %+v", c2)
	}
}

func TestHardlinkSkipDefault(t *testing.T) {
	var c Config // nil pointer
	if !c.HardlinkSkip() {
		t.Error("HardlinkSkip() = false for nil, want true (safe default)")
	}
	f := false
	c.SkipHardlinked = &f
	if c.HardlinkSkip() {
		t.Error("HardlinkSkip() = true when explicitly false")
	}
}

func TestValidateEngineKnobs(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unknown encoder", func(c *Config) { c.Encoder = "nvenc" }, "not supported"},
		{"crf too high", func(c *Config) { c.CRF = 52 }, "crf"},
		{"crf negative", func(c *Config) { c.CRF = -1 }, "crf"},
		{"savings out of range", func(c *Config) { c.MinSavingsPercent = 100 }, "min_savings_percent"},
		{"negative max_failures", func(c *Config) { c.MaxFailures = -1 }, "max_failures"},
		{"container ext with dot", func(c *Config) { c.ContainerExt = ".mkv" }, "container_ext"},
		{"valid cpu config", func(c *Config) { c.Encoder = "cpu"; c.CRF = 20 }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{LibraryRoots: []string{"/mnt/media"}}
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

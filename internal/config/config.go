// Package config loads and validates the transcode YAML configuration.
//
// This is the genesis (TRANSCODE-0) config surface: a minimal, strictly-validated
// struct sufficient to prove the config-as-code contract and to guarantee the tool
// refuses to run against a dangerous or unspecified library root. The full schema —
// koanf-backed, env-overridable, with a CI schema self-test — arrives in TRANSCODE-2
// (see operations/roadmaps/transcode.md §8). Fields are deliberately few here; the
// invariant that matters now is Validate(): a delete-capable tool must never start
// pointed at "/" or a home directory.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the declarative, YAML-authored configuration for the transcoder.
//
// TRANSCODE-1 adds the engine knobs below with sensible defaults (ApplyDefaults).
// The koanf-backed schema with env/flag overrides and a CI schema self-test is
// TRANSCODE-2 — until then these are a minimal, defaulted surface. Because a plain
// zero value cannot be distinguished from "absent" in yaml.v3, a zero for an
// int/string knob is treated as "use the default" (documented limitation resolved
// in TRANSCODE-2); construct a Config directly in tests to set an explicit zero.
type Config struct {
	// LibraryRoots are the directory trees the tool scans and re-encodes files
	// under. It is the ONLY place the tool ever mutates the filesystem, so it is
	// validated strictly (see Validate).
	LibraryRoots []string `yaml:"library_roots"`

	// LogLevel controls verbosity: debug|info|warn|error (default info).
	LogLevel string `yaml:"log_level"`

	// DryRun, when true, makes the tool report intended actions without changing
	// any file.
	DryRun bool `yaml:"dry_run"`

	// --- engine knobs (TRANSCODE-1) ---

	// VideoExts is the set of file extensions scanned (case-insensitive).
	VideoExts []string `yaml:"video_exts"`
	// Encoder selects the encode path. TRANSCODE-1 supports "cpu" (libx265);
	// hardware encoders + SVT-AV1 arrive in TRANSCODE-6.
	Encoder string `yaml:"encoder"`
	// CRF is the libx265 constant-rate-factor (lower = bigger/better).
	CRF int `yaml:"crf"`
	// Preset is the libx265 preset (slower = smaller).
	Preset string `yaml:"preset"`
	// PixelFormat is the output pixel format (10-bit for compression + no banding).
	PixelFormat string `yaml:"pixel_format"`
	// ContainerExt is the output container extension. TRANSCODE-1 uses a fixed
	// default; source-container matching is TRANSCODE-3.
	ContainerExt string `yaml:"container_ext"`
	// MinBitrateKbps skips sources below this (re-encoding them only bloats). 0
	// disables the skip (but see the zero-vs-absent note above for YAML).
	MinBitrateKbps int `yaml:"min_bitrate_kbps"`
	// MinSavingsPercent requires output <= input*(1-this/100); 0 = strictly smaller.
	MinSavingsPercent int `yaml:"min_savings_percent"`
	// DurationToleranceSec is the max |out-in| duration drift accepted.
	DurationToleranceSec float64 `yaml:"duration_tolerance_sec"`
	// MaxFailures retries a failing file this many times before parking it.
	MaxFailures int `yaml:"max_failures"`
	// SkipHardlinked skips files with >1 hard link (an active seed/dup). A nil
	// pointer means the default (true); use HardlinkSkip() to read it.
	SkipHardlinked *bool `yaml:"skip_hardlinked"`
	// StateDir holds the ledger + heartbeat (relative paths are resolved by callers).
	StateDir string `yaml:"state_dir"`
}

// HardlinkSkip reports whether hard-linked sources are skipped, defaulting to true
// when unset (nil). Skipping them is the safe default — replacing a hard-linked
// seed via rename would break the link and reclaim nothing.
func (c *Config) HardlinkSkip() bool { return c.SkipHardlinked == nil || *c.SkipHardlinked }

// ApplyDefaults fills unset engine knobs with their built-in defaults. It is
// called by the production load path (not by tests, which set explicit values).
func (c *Config) ApplyDefaults() {
	if len(c.VideoExts) == 0 {
		c.VideoExts = []string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "flv"}
	}
	if c.Encoder == "" {
		c.Encoder = "cpu"
	}
	if c.CRF == 0 {
		c.CRF = 22
	}
	if c.Preset == "" {
		c.Preset = "slow"
	}
	if c.PixelFormat == "" {
		c.PixelFormat = "yuv420p10le"
	}
	if c.ContainerExt == "" {
		c.ContainerExt = "mkv"
	}
	if c.MinBitrateKbps == 0 {
		c.MinBitrateKbps = 2500
	}
	if c.DurationToleranceSec == 0 {
		c.DurationToleranceSec = 1
	}
	if c.MaxFailures == 0 {
		c.MaxFailures = 3
	}
	if c.StateDir == "" {
		c.StateDir = "state"
	}
}

// ErrNoConfig is returned by Load when the path is empty.
var ErrNoConfig = errors.New("no config path provided")

// Load reads and parses a YAML config file. It does NOT validate — callers run
// Validate() explicitly so `validate` and `run` share one code path. Unknown
// keys are rejected (KnownFields) so a typo becomes a loud error, never a silent
// default — the fail-safe posture the whole tool is built on.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, ErrNoConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &c, nil
}

// Validate refuses a configuration that could cause harm or is under-specified.
// The rules are conservative on purpose: the tool is delete-capable, so an
// ambiguous or dangerous root is a hard error, never a guessed default.
func (c *Config) Validate() error {
	if len(c.LibraryRoots) == 0 {
		return errors.New("library_roots is empty: refusing to run with nothing to scan")
	}

	home, _ := os.UserHomeDir()
	seen := make(map[string]struct{}, len(c.LibraryRoots))
	for i, root := range c.LibraryRoots {
		if root == "" {
			return fmt.Errorf("library_roots[%d] is empty", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("library_roots[%d] %q must be an absolute path", i, root)
		}
		clean := filepath.Clean(root)
		if clean == "/" {
			return fmt.Errorf("library_roots[%d] resolves to %q: refusing to operate on the filesystem root", i, clean)
		}
		if home != "" && clean == filepath.Clean(home) {
			return fmt.Errorf("library_roots[%d] resolves to the home directory %q: refusing", i, clean)
		}
		if _, dup := seen[clean]; dup {
			return fmt.Errorf("library_roots[%d] %q is a duplicate", i, clean)
		}
		seen[clean] = struct{}{}
	}

	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("log_level %q is not one of debug|info|warn|error", c.LogLevel)
	}

	// Engine knobs (validated against their effective, post-ApplyDefaults values).
	switch c.Encoder {
	case "", "cpu":
		// ok (TRANSCODE-1 supports cpu; more encoders in TRANSCODE-6)
	default:
		return fmt.Errorf("encoder %q is not supported (TRANSCODE-1 supports: cpu)", c.Encoder)
	}
	if c.CRF < 0 || c.CRF > 51 {
		return fmt.Errorf("crf %d out of range (0-51)", c.CRF)
	}
	if c.MinSavingsPercent < 0 || c.MinSavingsPercent >= 100 {
		return fmt.Errorf("min_savings_percent %d out of range (0-99)", c.MinSavingsPercent)
	}
	if c.MaxFailures < 0 {
		return fmt.Errorf("max_failures %d must be >= 0", c.MaxFailures)
	}
	if c.DurationToleranceSec < 0 {
		return fmt.Errorf("duration_tolerance_sec %g must be >= 0", c.DurationToleranceSec)
	}
	if strings.ContainsAny(c.ContainerExt, "./\\") {
		return fmt.Errorf("container_ext %q must be a bare extension (no dot or slash)", c.ContainerExt)
	}
	return nil
}

// Package config loads and validates the transcode configuration.
//
// Load is layered (TRANSCODE-2, koanf): built-in defaults ← the YAML file ← the
// environment (TRANSCODE_*). Loading defaults as their own layer means an explicit
// zero in the file/env OVERRIDES a default while an absent key keeps it — resolving
// the zero-vs-absent ambiguity a plain struct-zero default has. Unknown YAML keys
// are rejected (a typo is a loud error, never a silent default). Validate() is the
// fail-safe backstop: a delete-capable tool must never start pointed at "/", a
// home directory, or a symlink that resolves to either.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	koanfenv "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// envPrefix is the prefix for environment overrides: TRANSCODE_CRF=20 sets crf.
const envPrefix = "TRANSCODE_"

// knownKeys are the top-level keys accepted in the YAML file. Any other key is a
// typo and is rejected — fail-safe: never a silent default.
var knownKeys = map[string]bool{
	"library_roots": true, "log_level": true, "dry_run": true,
	"video_exts": true, "encoder": true, "crf": true, "preset": true,
	"pixel_format": true, "container_ext": true, "min_bitrate_kbps": true,
	"min_savings_percent": true, "duration_tolerance_sec": true,
	"max_failures": true, "skip_hardlinked": true, "state_dir": true,
}

// defaultLayer is the built-in default configuration, loaded as koanf's base layer.
// It is the single source of truth for defaults.
func defaultLayer() map[string]any {
	return map[string]interface{}{
		"log_level":              "info",
		"dry_run":                false,
		"video_exts":             []string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "flv"},
		"encoder":                "cpu",
		"crf":                    22,
		"preset":                 "slow",
		"pixel_format":           "auto",
		"container_ext":          "source",
		"min_bitrate_kbps":       2500,
		"min_savings_percent":    0,
		"duration_tolerance_sec": 1.0,
		"max_failures":           3,
		"skip_hardlinked":        true,
		"state_dir":              "state",
	}
}

// Config is the declarative, YAML-authored configuration for the transcoder. Field
// tags are `yaml` (koanf unmarshals with Tag "yaml"). Load returns a fully-defaulted
// Config via koanf's defaults layer (defaultLayer) — the single source of defaults.
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
	// PixelFormat is the output pixel format. "auto" (default) derives it per
	// source — preserve chroma subsampling, floor bit-depth at 10 (see
	// internal/hdr.DerivePixFmt); an exotic/unrecognized source pix_fmt is SKIPPED,
	// never silently subsampled. Any other value forces that pix_fmt for every
	// source (back-compat with TRANSCODE-1's fixed yuv420p10le behaviour).
	PixelFormat string `yaml:"pixel_format"`
	// ContainerExt is the output container extension. "source"/"auto" (default,
	// sentinels) match the SOURCE file's own extension (in-place transcode, e.g.
	// mp4 -> mp4) so a container whose stream types don't round-trip through a
	// different container (e.g. MP4 mov_text subtitles into MKV) isn't forced to
	// change. Any other value forces that extension for every source (TRANSCODE-1
	// behaviour) — the collision guard still applies whenever the effective
	// extension differs from the source's own.
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

// ContainerMatchesSource reports whether ContainerExt is the "match the source"
// sentinel ("source"/"auto"/"") rather than a forced extension.
func (c *Config) ContainerMatchesSource() bool {
	switch c.ContainerExt {
	case "source", "auto", "":
		return true
	default:
		return false
	}
}

// PixelFormatAuto reports whether PixelFormat is the "derive per source" sentinel
// ("auto"/"") rather than a forced pixel format.
func (c *Config) PixelFormatAuto() bool {
	switch c.PixelFormat {
	case "auto", "":
		return true
	default:
		return false
	}
}

// ErrNoConfig is returned by Load when the path is empty.
var ErrNoConfig = errors.New("no config path provided")

// Load builds the effective config from three layers, later overriding earlier:
// built-in defaults ← the YAML file at path ← the environment (TRANSCODE_*). It
// does NOT validate — callers run Validate() explicitly so `validate` and `run`
// share one code path. Unknown top-level keys in the file are rejected (a typo is a
// loud error, never a silent default). A returned Config is fully defaulted.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, ErrNoConfig
	}

	k := koanf.New(".")
	// 1. defaults layer.
	if err := k.Load(confmap.Provider(defaultLayer(), "."), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}

	// 2. the YAML file — loaded into its own instance first so we can reject unknown
	// keys before merging (koanf itself does not error on unknown keys).
	kf := koanf.New(".")
	if err := kf.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	for _, key := range kf.Keys() {
		top := key
		if i := strings.IndexByte(key, '.'); i >= 0 {
			top = key[:i] // a list/nested key like "library_roots.0" -> "library_roots"
		}
		if !knownKeys[top] {
			return nil, fmt.Errorf("unknown config key %q in %s (typo?)", top, path)
		}
	}
	if err := k.Merge(kf); err != nil {
		return nil, fmt.Errorf("merge config %q: %w", path, err)
	}

	// 3. environment overrides (TRANSCODE_CRF=20 -> crf). Values arrive as strings;
	// WeaklyTypedInput (below) coerces them to the field types.
	err := k.Load(koanfenv.Provider(envPrefix, ".", func(s string) string {
		return strings.ToLower(strings.TrimPrefix(s, envPrefix))
	}), nil)
	if err != nil {
		return nil, fmt.Errorf("load env overrides: %w", err)
	}

	var c Config
	if err := k.UnmarshalWithConf("", &c, koanf.UnmarshalConf{
		Tag: "yaml",
		DecoderConfig: &mapstructure.DecoderConfig{
			WeaklyTypedInput: true,
			Result:           &c,
		},
	}); err != nil {
		return nil, fmt.Errorf("decode config %q: %w", path, err)
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

	// A delete-capable tool must be able to check that no root is the home dir; if
	// HOME can't be determined we refuse rather than silently skip the check.
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return errors.New("cannot determine the home directory (set $HOME) — refusing to validate library roots safely")
	}
	cleanHome := filepath.Clean(home)

	// isDangerous reports whether a cleaned absolute path is one we must never
	// operate on (the filesystem root or the home directory).
	dangerous := func(p string) string {
		switch p {
		case "/":
			return "the filesystem root"
		case cleanHome:
			return "the home directory"
		}
		return ""
	}

	seen := make(map[string]struct{}, len(c.LibraryRoots))
	for i, root := range c.LibraryRoots {
		if root == "" {
			return fmt.Errorf("library_roots[%d] is empty", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("library_roots[%d] %q must be an absolute path", i, root)
		}
		clean := filepath.Clean(root)
		if what := dangerous(clean); what != "" {
			return fmt.Errorf("library_roots[%d] resolves to %s (%q): refusing", i, what, clean)
		}
		// Symlink resolution: filepath.Clean is purely lexical, so a symlinked root
		// pointing at "/" or $HOME would pass the check above. If the path EXISTS,
		// re-check its real target. A not-yet-existent root (EvalSymlinks errors)
		// keeps only the lexical guard — validating before the mount exists is fine.
		if resolved, rerr := filepath.EvalSymlinks(clean); rerr == nil {
			rc := filepath.Clean(resolved)
			if what := dangerous(rc); what != "" {
				return fmt.Errorf("library_roots[%d] %q resolves via symlink to %s (%q): refusing", i, clean, what, rc)
			}
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

	// Engine knobs (validated against their effective values).
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

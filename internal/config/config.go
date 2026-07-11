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
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	koanfenv "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/NSchatz/transcode/internal/encoder"
)

// envPrefix is the prefix for environment overrides: TRANSCODE_CRF=20 sets crf.
const envPrefix = "TRANSCODE_"

// defaultServerAddr is the `serve` bind address default — LOCALHOST, so the control
// surface is never network-exposed by accident. Shared by defaultLayer() and
// EffectiveServerAddr() so the two can't drift.
const defaultServerAddr = "127.0.0.1:8080"

// knownKeys are the top-level keys accepted in the YAML file. Any other key is a
// typo and is rejected — fail-safe: never a silent default.
var knownKeys = map[string]bool{
	"library_roots": true, "log_level": true, "dry_run": true,
	"video_exts": true, "encoder": true, "crf": true, "preset": true,
	"pixel_format": true, "container_ext": true, "min_bitrate_kbps": true,
	"min_savings_percent": true, "duration_tolerance_sec": true,
	"max_failures": true, "skip_hardlinked": true, "state_dir": true,
	"vmaf_enable": true, "min_vmaf": true, "vmaf_min_pool": true,
	"vmaf_subsample": true, "vmaf_model": true, "workers": true,
	"server_addr": true, "server_auth_token": true, "scan_interval_sec": true,
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
		"vmaf_enable":            true,
		"min_vmaf":               95.0,
		"vmaf_min_pool":          0.0,
		"vmaf_subsample":         1,
		"vmaf_model":             "auto",
		"workers":                1,
		"server_addr":            defaultServerAddr,
		"server_auth_token":      "",
		"scan_interval_sec":      0,
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
	// Encoder selects the encode path — one of the internal/encoder registry keys:
	// "cpu" (libx265/hevc, the archival default), "svtav1" (libsvtav1/av1, CPU),
	// or the hardware encoders "nvenc" (hevc_nvenc), "av1_nvenc" (av1_nvenc),
	// "qsv" (hevc_qsv), "vaapi" (hevc_vaapi), "amf" (hevc_amf) — all opt-in,
	// gated behind a runtime capability check (never assumed to work; see
	// internal/encoder.Available and cmd/transcode's cmdRun). The raw ffmpeg -c:v
	// codec name (e.g. "libsvtav1") is also accepted as an alias.
	Encoder string `yaml:"encoder"`
	// CRF is the encoder's quality knob (lower = bigger/better): libx265/libsvtav1
	// constant-rate-factor, or reused as the CQ/global_quality/QP target for the
	// hardware encoders (see internal/engine.buildArgs).
	CRF int `yaml:"crf"`
	// Preset is the encoder's speed/quality preset: a libx265 preset word for
	// "cpu" ("slow" etc.), or mapped to SVT-AV1's numeric 0-13 scale for "svtav1"
	// (see internal/engine.svtav1Preset). Ignored by the hardware encoders.
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
	// StateDir holds the job store (jobs.db) + heartbeat (relative paths are
	// resolved by callers).
	StateDir string `yaml:"state_dir"`

	// --- VMAF perceptual-quality gate (TRANSCODE-4) ---

	// VmafEnable turns on the perceptual VMAF accept/reject gate (default true). When
	// enabled and libvmaf is unavailable, an encode is REJECTED (never accept an
	// unmeasured output).
	VmafEnable *bool `yaml:"vmaf_enable"`
	// MinVmaf is the pooled harmonic-mean VMAF below which an encode is rejected
	// (0-100; default 95 ≈ visually lossless / point of diminishing returns).
	MinVmaf float64 `yaml:"min_vmaf"`
	// VmafMinPool, when > 0, additionally rejects an encode whose worst (sub)sampled
	// frame VMAF (the `min` pool) falls below it — catches a worst-segment collapse.
	// 0 (default) disables the floor, leaving the harmonic-mean the sole VMAF gate.
	VmafMinPool float64 `yaml:"vmaf_min_pool"`
	// VmafSubsample is the frame-sampling interval for VMAF (>=1; 1 = every frame;
	// higher is cheaper but less precise). VMAF is a second full decode, so large
	// libraries may raise this.
	VmafSubsample int `yaml:"vmaf_subsample"`
	// VmafModel selects the libvmaf model: "auto" (default) picks vmaf_4k for output
	// height > 1440 else the HD model; any other value is passed through as the model
	// version/spec.
	VmafModel string `yaml:"vmaf_model"`

	// --- worker pool (TRANSCODE-5) ---

	// Workers is the number of concurrent encode workers RunOneshot fans out to.
	// 0 (absent/default) means 1 — the original sequential behaviour. CPU libx265
	// already saturates available cores for a single encode, so raising this above
	// 1 is an explicit opt-in (e.g. many small/low-resolution files, or a hardware
	// encoder in a later phase). Use EffectiveWorkers() to read the resolved value.
	Workers int `yaml:"workers"`

	// --- server / API+UI (TRANSCODE-7, `transcode serve`) ---

	// ServerAddr is the host:port the `serve` HTTP API + web UI binds to. Default
	// "127.0.0.1:8080" — LOCALHOST by design (fail-safe: the control surface is not
	// exposed to the network unless the operator opts in, then fronts it with a
	// reverse proxy). An empty value is treated as the default by `serve` (never a
	// bare ":8080" all-interfaces bind by accident).
	ServerAddr string `yaml:"server_addr"`
	// ServerAuthToken is the bearer token required on MUTATING endpoints
	// (rescan/pause/resume). Empty (default) DISABLES those endpoints entirely —
	// remote control is off until a token is explicitly set (fail-safe). Read
	// endpoints and the UI never require it. Prefer supplying it via the
	// TRANSCODE_SERVER_AUTH_TOKEN environment variable rather than the YAML file so
	// no secret lands in a committed config.
	ServerAuthToken string `yaml:"server_auth_token"`
	// ScanIntervalSec, when > 0, makes `serve` re-scan the library every N seconds
	// (in addition to an initial scan on startup and manual rescans via the API).
	// 0 (default) = no periodic scan: `serve` scans once on startup and thereafter
	// only when the API is asked to. Host-fair scheduling (run-windows, CPU caps) is
	// TRANSCODE-8; this is the plain interval.
	ScanIntervalSec int `yaml:"scan_interval_sec"`
}

// EffectiveServerAddr returns the bind address `serve` should use, defaulting an
// empty ServerAddr to the localhost default rather than a bare ":8080" (which would
// listen on every interface). This keeps "unset" and "explicitly empty" both safe.
func (c *Config) EffectiveServerAddr() string {
	if strings.TrimSpace(c.ServerAddr) == "" {
		return defaultServerAddr
	}
	return c.ServerAddr
}

// EffectiveWorkers returns the number of workers to run, defaulting 0 (absent) or
// a negative value to 1 — matching the pre-TRANSCODE-5 sequential behaviour.
func (c *Config) EffectiveWorkers() int {
	if c.Workers < 1 {
		return 1
	}
	return c.Workers
}

// VmafGate reports whether the VMAF gate is enabled, defaulting to true when unset.
func (c *Config) VmafGate() bool { return c.VmafEnable == nil || *c.VmafEnable }

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
	if c.Encoder != "" {
		if _, ok := encoder.Lookup(c.Encoder); !ok {
			return fmt.Errorf("encoder %q is not supported (known: %v)", c.Encoder, encoder.Known())
		}
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
	if c.MinVmaf < 0 || c.MinVmaf > 100 {
		return fmt.Errorf("min_vmaf %g out of range (0-100)", c.MinVmaf)
	}
	if c.VmafMinPool < 0 || c.VmafMinPool > 100 {
		return fmt.Errorf("vmaf_min_pool %g out of range (0-100)", c.VmafMinPool)
	}
	if c.VmafSubsample < 0 {
		// 0 means "use the default" (Load's koanf layer sets 1; the VMAF scorer also
		// floors <1 to 1) — consistent with the other zero-defaulted knobs. Only a
		// negative interval is invalid.
		return fmt.Errorf("vmaf_subsample %d must be >= 0", c.VmafSubsample)
	}
	// Fail-safe: an explicitly-enabled VMAF gate with no effective threshold (both
	// min_vmaf and vmaf_min_pool 0) is enabled-but-never-rejecting — a silent no-op on
	// a delete-capable tool. Refuse it. (Checked only when vmaf_enable is EXPLICIT: a
	// nil pointer is the default-on state, and Load always resolves it to true with
	// min_vmaf=95, so a real config never trips this by omission.)
	if c.VmafEnable != nil && *c.VmafEnable && c.MinVmaf == 0 && c.VmafMinPool == 0 {
		return errors.New("vmaf_enable is true but both min_vmaf and vmaf_min_pool are 0 — the VMAF gate would never reject; set min_vmaf (e.g. 95) or disable the gate")
	}
	if c.Workers < 0 || c.Workers > 1024 {
		return fmt.Errorf("workers %d out of range (0-1024; 0 means the default of 1)", c.Workers)
	}

	// Server knobs (TRANSCODE-7). A non-empty bind address must be a valid
	// host:port; an empty one is fine (EffectiveServerAddr defaults it to localhost).
	if strings.TrimSpace(c.ServerAddr) != "" {
		if _, _, err := net.SplitHostPort(c.ServerAddr); err != nil {
			return fmt.Errorf("server_addr %q is not a valid host:port: %w", c.ServerAddr, err)
		}
	}
	if c.ScanIntervalSec < 0 {
		return fmt.Errorf("scan_interval_sec %d must be >= 0 (0 = scan once on startup + on demand)", c.ScanIntervalSec)
	}
	return nil
}

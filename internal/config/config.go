// Package config loads and validates the holdfast configuration.
//
// Load is layered (TRANSCODE-2, koanf): built-in defaults ← the YAML file ← the
// environment (HOLDFAST_*). Loading defaults as their own layer means an explicit
// zero in the file/env OVERRIDES a default while an absent key keeps it — resolving
// the zero-vs-absent ambiguity a plain struct-zero default has. Unknown YAML keys
// are rejected (a typo is a loud error, never a silent default). Validate() is the
// fail-safe backstop: a delete-capable tool must never start pointed at "/", a
// home directory, or a symlink that resolves to either.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	koanfenv "github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/NSchatz/holdfast/internal/schedule"
)

// envPrefix is the prefix for environment overrides: HOLDFAST_CRF=20 sets crf.
const envPrefix = "HOLDFAST_"

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
	preserveMtimeKey:  true,
	"allow_non_local": true, "history_retention_rows": true, "undo_window_hours": true,
	"vmaf_enable": true, "min_vmaf": true, "vmaf_min_pool": true,
	"vmaf_min_chroma": true,
	"vmaf_subsample":  true, "vmaf_model": true, "workers": true,
	"server_addr": true, "server_auth_token": true, "scan_interval_sec": true,
	"metrics_enable": true, "notify_url": true, "run_window": true,
	"max_load": true, "tautulli_url": true, "tautulli_api_key": true,
	"bitrate_kbps": true, "encode_profiles": true,
	"scratch_dir": true, "scratch_min_free_gb": true,
}

// profileKeys are the keys accepted inside one `encode_profiles` entry. The
// unknown-key refusal has to bite INSIDE a profile as well as at the top level:
// `encodr: svtav1` nested in a profile is the same typo with the same consequence
// (a silent fall back to the top-level encoder), and a check that only looked at
// the top-level key would never see it.
var profileKeys = map[string]bool{
	"name": true, "match": true,
	"encoder": true, "crf": true, "preset": true,
	"pixel_format": true, "container_ext": true, "bitrate_kbps": true,
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
		"bitrate_kbps":           0,
		"scratch_dir":            "",
		"scratch_min_free_gb":    50,
		"min_bitrate_kbps":       2500,
		"min_savings_percent":    0,
		"duration_tolerance_sec": 1.0,
		"max_failures":           3,
		"skip_hardlinked":        true,
		preserveMtimeKey:         true,
		"state_dir":              "state",
		"history_retention_rows": 0,
		"undo_window_hours":      0,
		"vmaf_enable":            true,
		"min_vmaf":               95.0,
		"vmaf_min_pool":          60.0,
		"vmaf_min_chroma":        30.0,
		"vmaf_subsample":         1,
		"vmaf_model":             "auto",
		"workers":                1,
		"server_addr":            defaultServerAddr,
		"server_auth_token":      "",
		"scan_interval_sec":      0,
		"metrics_enable":         true,
		"notify_url":             "",
		"run_window":             "",
		"max_load":               0.0,
		"tautulli_url":           "",
		"tautulli_api_key":       "",
	}
}

// Config is the declarative, YAML-authored configuration for the transcoder. Field
// tags are `yaml` (koanf unmarshals with Tag "yaml"). Load returns a fully-defaulted
// Config via koanf's defaults layer (defaultLayer) — the single source of defaults.
type Config struct {
	// LibraryRoots are the directory trees the tool scans and re-encodes files
	// under, AS CONFIGURED. It is the ONLY place the tool ever mutates the
	// filesystem, so it is validated strictly (see Validate).
	//
	// An entry in the file may be a plain path or a mapping carrying that path plus a
	// profile (see profile.go); either way this list carries the paths and Roots
	// carries what each of them resolved to.
	LibraryRoots []string `yaml:"library_roots"`

	// Roots are the library roots with their RESOLVED per-root profiles, in the order
	// they were configured. Load always populates it, one entry per LibraryRoots
	// entry, and it is what Validate, Warnings and the engine read to learn what
	// decides a file under a given root.
	//
	// It is not a config key and is never decoded from one: a profile lives INSIDE a
	// library_roots entry, so there is nothing at the top level for this field to be
	// read from. A Config assembled by hand rather than by Load leaves it nil, and
	// RootProfiles then derives one root per LibraryRoots entry carrying the top-level
	// values - which is exactly the single-policy behaviour this generalizes.
	Roots []Root `yaml:"-"`

	// LogLevel controls verbosity: debug|info|warn|error (default info).
	LogLevel string `yaml:"log_level"`

	// DryRun, when true, makes the tool report intended actions without changing
	// any file.
	DryRun bool `yaml:"dry_run"`

	// --- engine knobs (TRANSCODE-1) ---

	// VideoExts is the set of file extensions scanned. Load normalizes each entry to
	// a lowercase, dot-free token (so ".MKV", "MKV", and "mkv" are equivalent and a
	// natural leading dot is not silently un-matchable); the scan compares
	// case-insensitively against a file's dot-stripped extension.
	VideoExts []string `yaml:"video_exts"`
	// Encoder selects the encode path — one of the internal/encoder registry keys:
	// "cpu" (libx265/hevc, the archival default), "svtav1" (libsvtav1/av1, CPU),
	// or the hardware encoders "nvenc" (hevc_nvenc), "av1_nvenc" (av1_nvenc),
	// "qsv" (hevc_qsv), "vaapi" (hevc_vaapi), "amf" (hevc_amf) — all opt-in,
	// gated behind a runtime capability check (never assumed to work; see
	// internal/encoder.Available and cmd/holdfast's cmdRun). The raw ffmpeg -c:v
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
	// BitrateKbps selects TARGET-BITRATE rate control at this many kbps instead of
	// the quality target. 0 - the DEFAULT, and what an absent key resolves to -
	// keeps CRF/CQ/QP quality-target encoding, which is this tool's archival
	// posture and what every existing config gets unchanged.
	//
	// A positive value replaces the quality knob for the affected jobs: no -crf,
	// -cq, -global_quality or -qp is passed at all, because a rate control and a
	// quality target are two different instructions and passing both leaves which
	// one wins to the encoder. It is announced at startup as a NOTICE (see
	// Notices) rather than a warning: nothing about the no-loss gate changes, and
	// a rejected encode still leaves the source untouched.
	//
	// It is a whole number of kbps. A fraction, a word, a boolean, a list or the
	// key with no value is a startup REFUSAL naming the key and the offending
	// value (see requireWholeKbps), never a silently truncated bitrate; a negative
	// value is refused by Validate.
	BitrateKbps int `yaml:"bitrate_kbps"`
	// EncodeProfiles is an ORDERED list of named profiles, each with a match
	// over the source and overrides for the transcode settings above. The FIRST
	// profile whose match selects a source supplies that job's settings, overlaid
	// on the top-level ones; a later matching profile has no effect on that job,
	// and a setting the matching profile does not override keeps its top-level
	// value. A source no profile matches is transcoded under the top-level
	// settings - never skipped and never failed. Absent (the default) resolves
	// every job to exactly the top-level settings.
	//
	// A profile selects what the ENCODER PRODUCES and nothing else. There is
	// deliberately no per-profile VMAF threshold, undo window, retention or any
	// other safety-gate knob: a profile must never be able to move a gate that
	// decides whether a source is destroyed.
	EncodeProfiles []EncodeProfile `yaml:"encode_profiles"`
	// ScratchDir is the directory the encoder's working file is written to. Empty
	// - the DEFAULT, and what an absent key resolves to - writes it beside the
	// source, which is this tool's original behaviour.
	//
	// It never changes the SWAP. Whatever this is set to, the file the finalizing
	// rename reads is a temp in the SOURCE's own directory: an accepted encode is
	// copied back into that directory, proved to be the bytes the gates accepted,
	// made durable, and only then handed to the existing atomic same-directory
	// rename. Nothing is ever renamed or moved out of here onto a source.
	//
	// It is classified and reported at startup, and a missing, unwritable,
	// short-on-space or library-root-overlapping scratch directory REFUSES the run
	// before anything is encoded (see internal/startup). Storage that is not local
	// does not refuse here and needs no allow_non_local entry: nothing
	// irreversible happens in this directory.
	ScratchDir string `yaml:"scratch_dir"`
	// ScratchMinFreeGB is the free-space floor, in GiB (2^30 bytes), the scratch
	// directory's filesystem must clear at STARTUP. Default 50 - roughly one 4K
	// source and its encode - so the commonest way this feature fails (a cache
	// device with nothing left on it) is a refusal naming the path and the figures
	// rather than a library's worth of encodes dying at the write step. 0 disables
	// the floor.
	//
	// It is a floor and NOT a prediction: startup has no per-file size to check
	// against, so the per-job pre-encode check (which refuses a job whose source is
	// larger than the free space right then) is what backs it, and a filesystem
	// that fills from outside holdfast after a run begins produces an ordinary
	// encode failure, which already leaves the source untouched.
	ScratchMinFreeGB int `yaml:"scratch_min_free_gb"`
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

	// PreserveMtime carries the SOURCE's modification time onto the replacement the
	// swap publishes. A nil pointer means the default (TRUE); use
	// PreserveMtimeEnabled() to read it.
	//
	// It defaults ON because resetting the mtime is a real, visible and unrecoverable
	// change to somebody's media server: "Recently Added" in Plex and Jellyfin, and
	// every date-based sort and smart collection built on it, sees a first pass over a
	// library as the entire library arriving at once. Every neighbouring tool - cp -p,
	// rsync -a, mv - preserves the modification time across a replacement, so
	// preserving it is the least-surprising behaviour and RESETTING it is the side
	// effect. The argument for OFF - that a preserved mtime is a deliberate lie about
	// when the bytes were written - is why the key exists, not why it defaults.
	//
	// Preserving it is SAFE despite probe.Fingerprint being size:mtime. A swap always
	// changes the SIZE (the verify gate refuses an output that is not strictly
	// smaller), so the post-swap fingerprint still moves, the terminal done row still
	// keys under a fresh identity, and a resume still reads the replacement as a new
	// file rather than as the already-processed source. With this key on, the size is
	// the whole of that guarantee - which is why the engine asserts it rather than
	// assuming it.
	PreserveMtime *bool `yaml:"preserve_mtime"`

	// StateDir holds the job store (jobs.db) + heartbeat (relative paths are
	// resolved by callers).
	StateDir string `yaml:"state_dir"`

	// HistoryRetentionRows bounds the ledger: the maximum number of terminal rows
	// (done/skipped/failed) the job store retains. 0 - the DEFAULT, and what an absent
	// key resolves to - disables retention entirely and keeps every row.
	//
	// It ships DISABLED and must stay that way. A prune is the one IRREVERSIBLE act in
	// this package's blast radius: it deletes audit history, and no re-run recreates it.
	// A later scan re-derives the file's CURRENT state instead, so a pruned `done` row
	// for a file still on disk comes back as a `skipped` row carrying the
	// already-at-target-codec guard - a weaker record of the same swap. A default that
	// silently deleted those rows would be this tool's cardinal sin with the ledger
	// instead of with the library.
	//
	// The prune it enables cannot lower the published lifetime reclaimed total (a pruned
	// row's contribution is carried forward durably before the row is removed) and cannot
	// cause a file to be encoded again: a terminal row is what holds that file out of the
	// encoder, so a row is only ever removed when the scan LISTED the directory its file
	// should be in and the file was not there. The ledger can therefore sit above this
	// bound - on a library that is not churning, above it permanently.
	// A negative value, or a value that is not a whole number of rows, is a startup
	// REFUSAL naming the key and the offending value - never a silent default.
	HistoryRetentionRows int `yaml:"history_retention_rows"`

	// UndoWindowHours is how many hours a swapped-out original is kept retrievable
	// (UNDO-6). 0 - the DEFAULT - disables the window, which is the configuration in
	// which a swap is final the microsecond it happens; `validate` and startup both
	// say so out loud.
	//
	// While the window is open the original is held by a SECOND HARD LINK to the same
	// data, so retention costs no additional space at the moment it is taken - but the
	// space the swap reclaimed is NOT returned to the filesystem until the window
	// closes and the link is released. A library-wide first pass therefore holds every
	// original it replaced for this many hours, which is the real cost of the setting:
	// the reclaimed figure and the held figure are reported separately for exactly
	// that reason.
	//
	// A source whose original cannot be retained is SKIPPED rather than swapped: the
	// window's promise is that a swap can be walked back, and a swap that cannot be is
	// not one this tool takes while the operator has asked for the window.
	UndoWindowHours int `yaml:"undo_window_hours"`

	// AllowNonLocal opts specific paths in to running on storage holdfast could
	// not positively identify as local (FILESYSTEM-1). holdfast's no-loss
	// contract is stated for a local filesystem - an atomic same-filesystem
	// rename whose failure means it did not happen, a stat that can see a
	// concurrent rewrite, a SQLite WAL that works at all - and at startup it
	// checks it has one, refusing a run whose library roots, state directory or
	// any filesystem mounted beneath a root is NOT local unless the operator has
	// said so here.
	//
	// It is per PATH and never a global switch: each entry names one path and
	// covers that path alone, so opting the state directory in does not quietly
	// opt a NAS mount inside the library in too. Each entry must name a
	// configured library root, the state directory, or a path spelled beneath a
	// configured library root AS CONFIGURED; anything else is a refusal, never a
	// silently ignored line. It never permits a path holdfast cannot INSPECT:
	// that is a refusal whatever is declared here.
	AllowNonLocal []string `yaml:"allow_non_local"`

	// --- VMAF perceptual-quality gate (TRANSCODE-4) ---

	// VmafEnable turns on the perceptual VMAF accept/reject gate (default true). When
	// enabled and libvmaf is unavailable, an encode is REJECTED (never accept an
	// unmeasured output).
	VmafEnable *bool `yaml:"vmaf_enable"`
	// MinVmaf is the pooled harmonic-mean VMAF below which an encode is rejected
	// (0-100; default 95). It is an AVERAGE, and an average hides local damage:
	// Netflix documents that mean pooling "has the risk of hiding poor quality
	// frames", and the harmonic mean is only a weak correction. Measured against
	// this default, ~1% of frames can collapse to VMAF ~35 and the pooled mean
	// still clears 95 — in a 2-hour film that is over a minute of destroyed video
	// passing the gate. MinVmaf is therefore NOT sufficient on its own; the
	// worst-frame floor below is what bounds local damage. Do not disable it.
	MinVmaf float64 `yaml:"min_vmaf"`
	// VmafMinPool is the worst-frame floor: an encode is rejected when its worst
	// (sub)sampled frame VMAF (libvmaf's `min` pool) falls below it. Default 60.
	//
	// This is the gate that catches a locally-broken encode — a short segment
	// destroyed inside an otherwise-clean file, which every structural check passes
	// (it decodes cleanly and carries the right duration, packets and streams) and
	// which the pooled mean averages away.
	//
	// It is the raw minimum, deliberately, and NOT a low-percentile (1st-pct /
	// worst-5%) statistic. A percentile tolerates a FRACTION of frames — but a
	// segment small enough to sneak past the mean gate is by construction a small
	// fraction of frames, so a percentile floor tolerates exactly the damage the
	// mean already tolerates, and its blind spot GROWS with runtime (1% of a 2-hour
	// film is ~72 seconds). The raw min is the only candidate whose guarantee does
	// not decay with duration.
	//
	// That is measured, not argued: vmaf.TestPoolingStatistic_OnlyRawMinSeesSubOne-
	// PercentDamage builds an encode with 1 of 240 frames destroyed and shows the
	// harmonic mean (~99) and the 1st percentile (~98) are BOTH blind to it while
	// the raw min reads ~43. That test reds if this reasoning ever stops holding.
	//
	// 0 disables the floor, restoring the mean-only gate and its blind spot.
	// `validate` warns when you do. The floor only ever REJECTS (the source is
	// kept), so the failure it can cause is a wasted encode, never a lost original.
	VmafMinPool float64 `yaml:"vmaf_min_pool"`
	// VmafMinChroma is the CHROMA floor, in dB: an encode is rejected when the worst
	// (sub)sampled frame's PSNR over the chroma planes - the worse of Cb and Cr -
	// falls below it. Default 30.
	//
	// It exists because the VMAF model above is LUMA-ONLY and therefore structurally
	// blind to chroma damage, and so is every structural gate: an output whose colour
	// planes have been flattened, shifted or desaturated decodes perfectly, carries
	// the right duration, packets and streams, and scores as well on VMAF as a
	// faithful encode does. Before this floor existed the source was then deleted.
	// Measured on real libvmaf: a 15% desaturation of the chroma planes leaves the
	// pooled harmonic mean at ~99 and the worst frame at ~97 - clear of BOTH luma
	// floors at their shipped defaults - while chroma PSNR falls to ~26 dB from the
	// ~40 dB a faithful encode of the same source records.
	//
	// It is PSNR over Cb and Cr rather than a colour-difference metric because PSNR
	// over those planes is computed over the chroma planes and nothing else, so a
	// value that falls can only mean chroma changed. And it is the raw min over
	// frames for the same reason VmafMinPool is: a mean hides a locally-broken
	// segment.
	//
	// The default of 30 dB sits in a measured gap. Honest encodes at the shipped
	// crf that still clear the luma gate bottom out around 40 dB (10 dB of
	// headroom), while the weakest chroma-only damage that evades the luma gate
	// reads ~26 dB. Rejecting a good encode costs a wasted encode and keeps the
	// source; accepting a bad one deletes an original.
	//
	// 0 disables the floor, leaving chroma damage UNGUARDED. `validate` warns when
	// you do. Range 0-100 (dB); libvmaf caps PSNR well below 100 in practice.
	VmafMinChroma float64 `yaml:"vmaf_min_chroma"`
	// VmafSubsample is the frame-sampling interval for VMAF (>=1; 1 = every frame;
	// higher is cheaper but less precise). VMAF is a second full decode, so large
	// libraries may raise this.
	//
	// It WEAKENS THE WORST-FRAME FLOOR: only sampled frames are measured, so a
	// damaged frame that is never sampled is never seen, and VmafMinPool degrades
	// from a guarantee into a sample. `validate` warns when this is > 1.
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

	// --- server / API+UI (TRANSCODE-7, `holdfast serve`) ---

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
	// HOLDFAST_SERVER_AUTH_TOKEN environment variable rather than the YAML file so
	// no secret lands in a committed config.
	ServerAuthToken string `yaml:"server_auth_token"`
	// ScanIntervalSec, when > 0, makes `serve` re-scan the library every N seconds
	// (in addition to an initial scan on startup and manual rescans via the API).
	// 0 (default) = no periodic scan: `serve` scans once on startup and thereafter
	// only when the API is asked to.
	ScanIntervalSec int `yaml:"scan_interval_sec"`

	// --- observability + host-fair scheduling (TRANSCODE-8, `serve` only) ---

	// MetricsEnable exposes Prometheus metrics at /metrics (default true). Metrics
	// are read-only instrumentation — best-effort, never affecting file handling.
	MetricsEnable bool `yaml:"metrics_enable"`
	// NotifyURL is a shoutrrr service URL (e.g. ntfy/Discord/Gotify) for best-effort
	// notifications — a message per failed file + a per-scan summary. Empty (default)
	// disables notifications. May carry a secret; prefer HOLDFAST_NOTIFY_URL over the
	// YAML file.
	NotifyURL string `yaml:"notify_url"`
	// RunWindow is a daily host-fair window "HH:MM-HH:MM" (local time) during which
	// new work may start; empty (default) = always. Outside it, `serve` stops feeding
	// NEW files (an in-flight encode always finishes) — scheduling only DELAYS work.
	RunWindow string `yaml:"run_window"`
	// MaxLoad is a per-core 1-minute load-average cap; while the host is above it,
	// `serve` stops feeding new files. 0 (default) disables the check.
	MaxLoad float64 `yaml:"max_load"`
	// TautulliURL + TautulliAPIKey enable an optional Plex-aware pause: while Tautulli
	// reports an active stream, `serve` stops feeding new files. Both must be set to
	// enable it (default off). A Tautulli outage fails OPEN (never halts transcoding).
	TautulliURL    string `yaml:"tautulli_url"`
	TautulliAPIKey string `yaml:"tautulli_api_key"`
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

// RetentionEnabled reports whether a bounded ledger is configured. It is false for the
// shipped default (0) and for an absent key, which is what makes "keep every row" the
// behaviour an operator gets without asking for anything.
func (c *Config) RetentionEnabled() bool { return c.HistoryRetentionRows > 0 }

// UndoEnabled reports whether the undo window is open at all. It is the single
// reading of "is 0 the disabled sentinel", so the engine, the CLI and the warning
// cannot disagree about what the default means.
func (c *Config) UndoEnabled() bool { return c.UndoWindowHours > 0 }

// UndoWindow is how long a retained original is kept. Zero when the window is
// disabled, which no caller should reach - UndoEnabled gates them all.
func (c *Config) UndoWindow() time.Duration {
	if !c.UndoEnabled() {
		return 0
	}
	return time.Duration(c.UndoWindowHours) * time.Hour
}

// VmafGate reports whether the VMAF gate is enabled, defaulting to true when unset.
func (c *Config) VmafGate() bool { return vmafGate(c.VmafEnable) }

// HardlinkSkip reports whether hard-linked sources are skipped, defaulting to true
// when unset (nil). Skipping them is the safe default — replacing a hard-linked
// seed via rename would break the link and reclaim nothing.
func (c *Config) HardlinkSkip() bool { return hardlinkSkip(c.SkipHardlinked) }

// preserveMtimeKey is the one place the modification-time key is spelled. knownKeys,
// defaultLayer and the struct tag all read it from here, so a rename cannot leave one of
// them behind.
const preserveMtimeKey = "preserve_mtime"

// PreserveMtimeEnabled reports whether the swap carries the source's modification time
// onto the replacement, defaulting to TRUE when unset (nil). It is the single reading of
// that default, so the engine, the loader and the documentation cannot disagree about what
// an absent key means - and a Config built in Go rather than loaded from a file reads as
// the SHIPPED default rather than as the struct zero.
func (c *Config) PreserveMtimeEnabled() bool { return c.PreserveMtime == nil || *c.PreserveMtime }

// ContainerMatchesSource reports whether ContainerExt is the "match the source"
// sentinel ("source"/"auto"/"") rather than a forced extension.
func (c *Config) ContainerMatchesSource() bool { return containerMatchesSource(c.ContainerExt) }

// PixelFormatAuto reports whether PixelFormat is the "derive per source" sentinel
// ("auto"/"") rather than a forced pixel format.
func (c *Config) PixelFormatAuto() bool { return pixelFormatAuto(c.PixelFormat) }

// The four sentinel readings, as free functions, because a top-level value and a
// resolved per-root profile must read them identically or the same YAML would mean two
// things depending on which spelling of an entry it was written under.

func vmafGate(p *bool) bool { return p == nil || *p }

func hardlinkSkip(p *bool) bool { return p == nil || *p }

func containerMatchesSource(ext string) bool {
	switch ext {
	case "source", "auto", "":
		return true
	default:
		return false
	}
}

func pixelFormatAuto(format string) bool {
	switch format {
	case "auto", "":
		return true
	default:
		return false
	}
}

// TopLevelProfile is the profile a root with no overrides of its own resolves to: the
// top-level value of every overridable knob. It is the middle of the three layers, and
// the whole of what a flat library_roots list has ever meant.
//
// TestProfileKnobSetIsClosedAndSingleSourced proves, field by field, that this copies
// every knob in profileKnobs from the identically-tagged Config field - so a knob added
// to Profile and forgotten here is a red test rather than a root that silently inherits
// a zero.
func (c *Config) TopLevelProfile() Profile {
	return Profile{
		Encoder:           c.Encoder,
		CRF:               c.CRF,
		Preset:            c.Preset,
		PixelFormat:       c.PixelFormat,
		ContainerExt:      c.ContainerExt,
		MinBitrateKbps:    c.MinBitrateKbps,
		MinSavingsPercent: c.MinSavingsPercent,
		SkipHardlinked:    c.SkipHardlinked,
		VmafEnable:        c.VmafEnable,
		MinVmaf:           c.MinVmaf,
		VmafMinPool:       c.VmafMinPool,
		VmafMinChroma:     c.VmafMinChroma,
		VmafSubsample:     c.VmafSubsample,
		VmafModel:         c.VmafModel,
	}
}

// RootProfiles is the resolved roots this configuration decides files with, and it is
// the ONE reading of that - Validate, Warnings, the startup preflights and the engine
// all go through it, so none of them can be looking at a different set of profiles than
// the others.
//
// Load populates Roots, so this returns exactly what the inheritance produced. A Config
// assembled by hand (the engine's own tests, and any caller that builds a struct rather
// than reading a file) carries none, and one root per LibraryRoots entry is derived from
// the top-level values instead: the same profile for every root, which is precisely the
// behaviour of every configuration written before profiles existed.
func (c *Config) RootProfiles() []Root {
	if len(c.Roots) > 0 {
		return c.Roots
	}
	top := c.TopLevelProfile()
	roots := make([]Root, 0, len(c.LibraryRoots))
	for _, r := range c.LibraryRoots {
		roots = append(roots, Root{Path: r, Clean: filepath.Clean(r), Profile: top})
	}
	return roots
}

// RootFor returns the root p lies under, and whether there is one. Nested roots are
// refused at validate time, so at most one root can ever contain a path and the answer
// needs no precedence rule for a reader to check.
func (c *Config) RootFor(p string) (Root, bool) {
	for _, r := range c.RootProfiles() {
		if r.Contains(p) {
			return r, true
		}
	}
	return Root{}, false
}

// ErrNoConfig is returned by Load when the path is empty.
var ErrNoConfig = errors.New("no config path provided")

// Load builds the effective config from three layers, later overriding earlier:
// built-in defaults ← the YAML file at path ← the environment (HOLDFAST_*). It
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
	// explicitTop is the set of top-level keys the file or the environment actually
	// carried. The resolved value alone cannot say whether a knob was CHOSEN at the top
	// level or is simply the built-in default - they are the same value - and `validate`
	// has to print which layer supplied each of a root's knobs.
	explicitTop := make(map[string]bool, len(knownKeys))
	for _, key := range kf.Keys() {
		top := topLevelKey(key)
		if !knownKeys[top] {
			return nil, fmt.Errorf("unknown config key %q in %s (typo?)", top, path)
		}
		explicitTop[top] = true
	}
	// The unknown-key refusal, one level down. kf.Keys() flattens a list of maps to
	// `encode_profiles.0.encodr`, and the loop above deliberately only looks at
	// the token before the first dot - so a typo INSIDE a profile passed the check
	// that exists to catch typos. The raw list is walked here instead, before
	// anything is merged or decoded.
	if err := checkProfileKeys(kf.Get("encode_profiles"), path); err != nil {
		return nil, err
	}
	if err := k.Merge(kf); err != nil {
		return nil, fmt.Errorf("merge config %q: %w", path, err)
	}

	// 3. environment overrides (HOLDFAST_CRF=20 -> crf). Values arrive as strings;
	// WeaklyTypedInput (below) coerces them to the field types. Loaded into its own
	// instance first, for the same reason the file is: what it CARRIED has to be
	// readable, not merely what it left behind.
	ke := koanf.New(".")
	err := ke.Load(koanfenv.Provider(envPrefix, ".", func(s string) string {
		return strings.ToLower(strings.TrimPrefix(s, envPrefix))
	}), nil)
	if err != nil {
		return nil, fmt.Errorf("load env overrides: %w", err)
	}
	for _, key := range ke.Keys() {
		explicitTop[topLevelKey(key)] = true
	}
	if err := k.Merge(ke); err != nil {
		return nil, fmt.Errorf("merge env overrides: %w", err)
	}

	// history_retention_rows is a COUNT OF ROWS, and the decoder below is deliberately
	// weakly typed - it would turn `3.7` into 3 and `"12"` into 12 without a word. A
	// retention that silently rounds is a retention the operator did not write, on the
	// one knob whose effect is an irreversible delete of audit history, so the raw value
	// is checked BEFORE the decoder can coerce it. Same discipline as the unknown-key
	// rejection above: loud, never a silent default. (A NEGATIVE whole number decodes
	// faithfully and is refused by Validate, with the rest of the range checks.)
	if err := requireWholeRows(k.Get(retentionKey), retentionKey, path); err != nil {
		return nil, err
	}

	// The per-root profiles, resolved once, here. library_roots is the one key whose
	// value is heterogeneous - a list of paths, or of mappings carrying a path plus a
	// profile, or of both - so it is parsed and resolved BEFORE the struct decode, and
	// the key is then replaced by the plain list of paths the rest of this build has
	// always read. Nothing downstream of here has to know an entry could have been a
	// mapping.
	entries, err := parseRootEntries(k.Get("library_roots"), path)
	if err != nil {
		return nil, err
	}
	roots, err := resolveRoots(k, entries, explicitTop, path)
	if err != nil {
		return nil, err
	}
	k.Delete("library_roots")
	if len(roots) > 0 {
		paths := make([]string, 0, len(roots))
		for _, r := range roots {
			paths = append(paths, r.Path)
		}
		if err := k.Set("library_roots", paths); err != nil {
			return nil, fmt.Errorf("collecting the library roots of %q: %w", path, err)
		}
	}

	// bitrate_kbps is a WHOLE NUMBER OF KBPS and scratch_min_free_gb a whole number
	// of gibibytes, and the decoder below would turn `8000.5` into 8000, `"8000"`
	// into 8000 and `true` into 1 without a word - three bitrates the operator did
	// not write, on the knob that decides what the encoder aims at. Same discipline
	// as the retention check above and the unknown-key rejection: loud, never a
	// silent default. (A NEGATIVE whole number decodes faithfully and is refused by
	// Validate, with the rest of the range checks.)
	if err := requireWholeKbps(k.Get(bitrateKey), bitrateKey, "kbps", path); err != nil {
		return nil, err
	}
	if err := requireWholeKbps(k.Get(scratchFloorKey), scratchFloorKey, "gibibytes", path); err != nil {
		return nil, err
	}
	for i, raw := range profileMaps(kf.Get("encode_profiles")) {
		if v, ok := raw["bitrate_kbps"]; ok {
			key := fmt.Sprintf("encode_profiles[%d].bitrate_kbps", i)
			if err := requireWholeKbps(v, key, "kbps", path); err != nil {
				return nil, err
			}
		}
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

	// video_exts is user-authored and the scan compares it against dot-free,
	// lowercase, filepath.Ext-derived tokens. Normalize it here so the natural
	// forms — ".MKV", "Mp4", " mkv " — all match. Without this a leading dot is
	// SILENTLY un-matchable: ".mkv" in the YAML would make the scan find nothing and
	// report no error, the worst kind of config-edge failure for a stranger pointing
	// the tool at their library. Case was already normalized at match time; the dot
	// and surrounding whitespace were not.
	c.VideoExts = normalizeExts(c.VideoExts)
	c.Roots = roots

	return &c, nil
}

// topLevelKey reduces a koanf key to the top-level config key it belongs to: a
// list/nested key like "library_roots.0" is still the "library_roots" key.
func topLevelKey(key string) string {
	if i := strings.IndexByte(key, '.'); i >= 0 {
		return key[:i]
	}
	return key
}

// retentionKey is the one place the ledger-retention key is spelled. knownKeys,
// defaultLayer and the whole-number refusal all read it from here, so a rename cannot
// leave one of the three behind.
const retentionKey = "history_retention_rows"

// bitrateKey and scratchFloorKey are the same discipline for the two whole-number
// keys this phase adds.
const (
	bitrateKey      = "bitrate_kbps"
	scratchFloorKey = "scratch_min_free_gb"
)

// profileMaps returns the raw `encode_profiles` entries as they were AUTHORED,
// before the weakly-typed decoder has seen them. Anything that is not a list of
// maps yields nothing here and is reported by the decoder instead, which is the
// right division: this function exists to look at the values inside a well-shaped
// list, not to re-implement the decoder's own type errors.
func profileMaps(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		out = append(out, m)
	}
	return out
}

// checkProfileKeys refuses a key inside a profile that the schema does not define.
// It is the unknown-key rejection at the one level the top-level loop cannot see,
// and it fails the same way: naming the profile, the key and the file.
func checkProfileKeys(raw any, path string) error {
	for i, m := range profileMaps(raw) {
		for key := range m {
			if !profileKeys[key] {
				return fmt.Errorf("unknown config key %q in encode_profiles[%d] in %s (typo?): "+
					"a profile accepts name, match, encoder, crf, preset, pixel_format, container_ext, bitrate_kbps",
					key, i, path)
			}
		}
	}
	return nil
}

// requireWholeKbps refuses a value for a whole-number key that is not one, naming
// the key, the unit and the offending value. It runs against the RAW layered value
// for the reason requireWholeRows does: the decoder is WeaklyTypedInput and would
// truncate, coerce or invent a number rather than report one.
func requireWholeKbps(raw any, key, unit, path string) error {
	if isWholeNumber(raw) {
		return nil
	}
	return fmt.Errorf("%s must be a whole number of %s: %#v in %s is not one", key, unit, raw, path)
}

// requireWholeRows refuses a value for a row-count key that is not a whole number of
// rows, naming the key and the offending value. It runs against the RAW layered value
// (defaults <- file <- env) rather than the decoded struct field, because the decoder is
// WeaklyTypedInput: it would truncate 3.7 to 3, read "12" as 12 and read `true` as 1,
// each of which is a configuration the operator did not write.
//
// An env override arrives as a string and a YAML integer as an int, so both spellings of
// a genuine whole number are accepted; anything else - a fraction, a word, a boolean, a
// list, or a key present with no value at all - is a refusal.
func requireWholeRows(raw any, key, path string) error {
	if isWholeNumber(raw) {
		return nil
	}
	return fmt.Errorf("%s must be a whole number of rows (0 disables retention and keeps every terminal row): "+
		"%#v in %s is not one", key, raw, path)
}

// isWholeNumber reports whether a RAW layered value is a genuine whole number. An
// env override arrives as a string and a YAML integer as an int, so both spellings
// of a genuine whole number are accepted; anything else - a fraction, a word, a
// boolean, a list, or a key present with no value at all - is not one.
func isWholeNumber(raw any) bool {
	switch v := raw.(type) {
	case int, int32, int64, uint, uint32, uint64:
		return true
	case float32:
		return float64(v) == math.Trunc(float64(v)) && !math.IsInf(float64(v), 0)
	case float64:
		return v == math.Trunc(v) && !math.IsInf(v, 0) && !math.IsNaN(v)
	case string:
		_, err := strconv.Atoi(strings.TrimSpace(v))
		return err == nil
	}
	return false
}

// normalizeExts lowercases each video extension and strips a leading dot and any
// surrounding whitespace, so ".MKV", "MKV", and "mkv" are one token. An entry that
// normalizes to empty (a stray "" or a bare ".") is dropped rather than kept as a
// token that can never match a real file's extension. It never errors — an
// unusable entry is simply removed.
func normalizeExts(exts []string) []string {
	if exts == nil {
		return nil
	}
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		e = strings.TrimSpace(strings.TrimPrefix(e, "."))
		if e == "" {
			continue
		}
		out = append(out, e)
	}
	return out
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

	// cleaned and resolved carry each root's two spellings, in configuration order, for
	// the nesting refusal below. They are collected in this loop rather than recomputed
	// there so a root is cleaned and symlink-resolved exactly once.
	cleaned := make([]string, len(c.LibraryRoots))
	resolved := make([]string, len(c.LibraryRoots))
	seen := make(map[string]struct{}, len(c.LibraryRoots))
	for i, root := range c.LibraryRoots {
		if root == "" {
			return fmt.Errorf("library_roots[%d] is empty", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("library_roots[%d] %q must be an absolute path", i, root)
		}
		clean := filepath.Clean(root)
		cleaned[i] = clean
		if what := dangerous(clean); what != "" {
			return fmt.Errorf("library_roots[%d] resolves to %s (%q): refusing", i, what, clean)
		}
		// Symlink resolution: filepath.Clean is purely lexical, so a symlinked root
		// pointing at "/" or $HOME would pass the check above. If the path EXISTS,
		// re-check its real target. A not-yet-existent root (EvalSymlinks errors)
		// keeps only the lexical guard — validating before the mount exists is fine.
		if r, rerr := filepath.EvalSymlinks(clean); rerr == nil {
			rc := filepath.Clean(r)
			resolved[i] = rc
			if what := dangerous(rc); what != "" {
				return fmt.Errorf("library_roots[%d] %q resolves via symlink to %s (%q): refusing", i, clean, what, rc)
			}
		}
		if _, dup := seen[clean]; dup {
			return fmt.Errorf("library_roots[%d] %q is a duplicate", i, clean)
		}
		seen[clean] = struct{}{}
	}

	// NESTED ROOTS ARE REFUSED, not resolved.
	//
	// Every knob that decides a file is now per root, so a file under two roots would
	// have two answers to "what CRF, what bitrate floor, what VMAF floor" - and the
	// wrong answer here ends in the deletion of an original that no re-run undoes. A
	// longest-prefix rule would settle it, and nobody reviewing a configuration could
	// see which root won. Refusing makes the resolution unambiguous BY CONSTRUCTION.
	//
	// Both spellings are compared. Lexically, on path boundaries, so /a/b nests under
	// /a and /ab does not; and again on the symlink-resolved paths where both targets
	// exist, because a root that is a link INTO another root is the same overlap wearing
	// a different name. It runs alongside the duplicate, absolute-path, dangerous-path
	// and symlink checks above, never instead of them.
	for i := range c.LibraryRoots {
		for j := range c.LibraryRoots {
			if i == j {
				continue
			}
			if underRoot(cleaned[i], cleaned[j]) {
				return nestedRootsError(i, cleaned[i], j, cleaned[j], "")
			}
			if resolved[i] != "" && resolved[j] != "" && underRoot(resolved[i], resolved[j]) {
				return nestedRootsError(i, cleaned[i], j, cleaned[j],
					fmt.Sprintf(" (%q resolves to %q and %q resolves to %q)",
						cleaned[i], resolved[i], cleaned[j], resolved[j]))
			}
		}
	}

	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("log_level %q is not one of debug|info|warn|error", c.LogLevel)
	}

	// Engine knobs (validated against their effective values). The overridable ones are
	// checked HERE against the top level and AGAIN below against every root's resolved
	// profile - both, deliberately. A top-level value every root happens to override is
	// still a value the operator wrote and still a refusal; and a value only one root
	// carries is refused naming that root.
	if err := c.TopLevelProfile().validate(); err != nil {
		return err
	}
	// The knobs that are NOT per-root, checked once, here. A library root's profile may
	// not carry any of these - the target bitrate, the encode profiles or the scratch
	// location - so there is no per-root pass for them to be checked in.
	//
	// A negative bitrate has no reading: it is neither "use the quality target" (0)
	// nor a rate. Refused BY NAME rather than clamped, on a knob whose whole job is
	// to say what the encoder aims at.
	if c.BitrateKbps < 0 {
		return fmt.Errorf("bitrate_kbps %d must be >= 0 (0 keeps the crf/quality target; a positive value is a target bitrate in kbps)", c.BitrateKbps)
	}
	if err := c.validateProfiles(); err != nil {
		return err
	}
	if err := c.validateScratch(); err != nil {
		return err
	}
	if c.MaxFailures < 0 {
		return fmt.Errorf("max_failures %d must be >= 0", c.MaxFailures)
	}
	// A negative retention has no reading: it is neither "keep everything" (0) nor a
	// bound. Refuse it here, before the store is opened, rather than guessing which the
	// operator meant on the one knob that deletes audit history.
	if c.HistoryRetentionRows < 0 {
		return fmt.Errorf("%s %d must be >= 0 (0 disables retention and keeps every terminal row; "+
			"a positive value is the maximum number of terminal rows the ledger retains)",
			retentionKey, c.HistoryRetentionRows)
	}
	if c.DurationToleranceSec < 0 {
		return fmt.Errorf("duration_tolerance_sec %g must be >= 0", c.DurationToleranceSec)
	}
	// The undo window (UNDO-6). A negative retention is not a shorter window, it is a
	// window that has already closed for every original it would hold - so it would
	// retain a link and release it on the same pass, paying the cost of the feature
	// and delivering none of it. Refused BY NAME rather than clamped to 0, which would
	// silently turn a typo into "swaps are final".
	if c.UndoWindowHours < 0 {
		return fmt.Errorf("undo_window_hours %d must be >= 0 (0 disables the undo window; a swap is then final)", c.UndoWindowHours)
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

	// Host-fair scheduling knobs (TRANSCODE-8).
	if _, err := schedule.ParseWindow(c.RunWindow); err != nil {
		return err
	}
	if c.MaxLoad < 0 {
		return fmt.Errorf("max_load %g must be >= 0 (0 disables the CPU-load cap)", c.MaxLoad)
	}

	// Every per-value refusal above, re-run against what each root ACTUALLY resolved to,
	// naming the root. Without this a profile could carry a crf of 99, an unknown
	// encoder or a chroma floor of 140 and start, because the top-level values it
	// overrode were all fine - and the value a file is decided by is this one, not the
	// one at the top of the file.
	for _, r := range c.RootProfiles() {
		if err := r.Profile.validate(); err != nil {
			return fmt.Errorf("library root %s: %w", r.Clean, err)
		}
	}
	return nil
}

// errVmafGateNeverRejects is the refusal for an explicitly-enabled VMAF gate with every
// floor at 0. Shared between the top-level check and the per-profile one so a root that
// zeroes all three is refused in the same words as a file that does.
var errVmafGateNeverRejects = errors.New("vmaf_enable is true but min_vmaf, vmaf_min_pool and vmaf_min_chroma are all 0 - the VMAF gate would never reject; set min_vmaf (e.g. 95) or disable the gate")

// nestedRootsError is the refusal for two roots where one contains the other. It names
// BOTH, with their indexes, because the fix is to remove or move one of them and an
// operator cannot do that from a message that names only the inner.
func nestedRootsError(i int, outer string, j int, inner, via string) error {
	return fmt.Errorf("library_roots[%d] %q is nested inside library_roots[%d] %q%s: refusing. "+
		"Each root carries its own profile - its own encoder, crf, bitrate floor and VMAF floors - so a "+
		"file under both has two answers to what may be done to it, and the wrong answer deletes an "+
		"original. Configure one root covering the tree, or two that do not overlap",
		j, inner, i, outer, via)
}

// validateScratch applies the rules the CONFIGURATION can decide about the scratch
// directory. Everything that needs the filesystem - does it exist, is it a
// directory, can this process write in it, is there room, does it overlap a library
// root - is the startup check's, taken once over the whole set of paths this run
// would act on (internal/startup), so there is exactly one authority for each
// question rather than two that can disagree.
func (c *Config) validateScratch() error {
	if c.ScratchMinFreeGB < 0 {
		return fmt.Errorf("scratch_min_free_gb %d must be >= 0 (0 disables the free-space floor)", c.ScratchMinFreeGB)
	}
	dir := strings.TrimSpace(c.ScratchDir)
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("scratch_dir %q must be an absolute path", c.ScratchDir)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return errors.New("cannot determine the home directory (set $HOME) — refusing to validate scratch_dir safely")
	}
	clean := filepath.Clean(dir)
	// holdfast SWEEPS this directory on every start, so the same rule the library
	// roots get applies: never point a sweeping, delete-capable tool at "/" or at a
	// home directory, lexically or through a symlink.
	for _, p := range []string{clean, resolvedOrSelf(clean)} {
		switch p {
		case "/":
			return fmt.Errorf("scratch_dir resolves to the filesystem root (%q): refusing", p)
		case filepath.Clean(home):
			return fmt.Errorf("scratch_dir resolves to the home directory (%q): refusing", p)
		}
	}
	return nil
}

// resolvedOrSelf resolves symbolic links where it can and falls back to the path
// itself where it cannot (a directory that does not exist yet is the startup
// check's refusal to report, not this one's).
func resolvedOrSelf(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return p
}

// Notices reports things this configuration MEANS that an operator must be told at
// startup, whether or not they chose them. They are not Warnings: a warning says a
// safety gate has been weakened from what this tool ships, and a shipped default can
// never be that. Both are announced, and they are separate lists so that neither has
// to lie about what it is - a default that produced a warning would train an operator
// to skip warnings, and a real weakened gate is what they would skip next.
//
// The undo window is the case this exists for (UNDO-6). It is OFF by default, and off
// means the swap - the only irreversible act this tool performs - cannot be walked
// back. That is not a weakened gate, it is the tool's behaviour, and it is precisely
// the behaviour somebody deleting originals should hear stated before the first one
// goes.
//
// `validate` prints these as `note:`; `run` and `serve` log them at startup. They are
// logged at WARN rather than INFO, not because a notice is a warning, but because
// `log_level: warn` is a legal setting and a startup statement nobody can hear at a
// level they may legitimately choose has not been made. The list stays separate from
// Warnings regardless, which is where the distinction actually lives.
func (c *Config) Notices() []string {
	var n []string
	if !c.UndoEnabled() {
		n = append(n, "undo_window_hours is 0 - THE UNDO WINDOW IS DISABLED, so a swap is FINAL: "+
			"the rename that replaces a source destroys it, nothing retains the original, and a bad "+
			"encode that cleared every gate cannot be walked back. This is the default. Set "+
			"undo_window_hours (e.g. 24) to keep each replaced original retrievable with "+
			"`holdfast restore <path>` for that long: it costs no extra space at the moment it is "+
			"taken (a second hard link to the same data), but the space a swap reclaimed is not "+
			"returned to the filesystem until the window closes.")
	}
	// A target bitrate is not a weakened gate - every no-loss check still runs and a
	// rejected encode still leaves the source untouched - but it does mean the
	// quality knob an operator can still see in their config file is not what the
	// encoder is being told to aim at, which is exactly the kind of thing a config
	// file lets you believe for months.
	if c.bitrateInEffect() {
		n = append(n, "bitrate_kbps is set - the CRF/QUALITY TARGET IS NOT IN USE for the affected jobs: "+
			"those encodes run under a target-bitrate rate control at the configured kbps, and no crf, cq, "+
			"global_quality or qp value is passed to the encoder at all. Every no-loss gate is unchanged, and an "+
			"encode that misses one is still rejected with the source untouched. Set bitrate_kbps to 0 (the default) "+
			"to go back to the quality target.")
	}
	if strings.TrimSpace(c.ScratchDir) != "" {
		n = append(n, "scratch_dir is set - the encoder writes its working file to "+strings.TrimSpace(c.ScratchDir)+
			" and the accepted result is COPIED BACK into a temp beside the source before the swap. The swap itself is "+
			"unchanged: it is still an atomic rename within the source's own directory. The scratch device therefore pays "+
			"a full write-plus-read cycle per transcode, at video-file sizes.")
	}
	return n
}

// Warnings reports configurations that are VALID but weaken a safety gate — the
// things a delete-capable tool must say out loud rather than absorb silently. They
// are not errors: each is a legitimate choice, and refusing to start would be
// wrong. Staying quiet about them would also be wrong, because the config still
// LOOKS like it has a worst-frame floor when it no longer meaningfully does.
//
// `validate` prints these, and `run`/`serve` log them at startup.
//
// A WARNING IS ALWAYS A WEAKENED GATE. Something that is merely worth stating about a
// configuration - including a shipped default worth stating - belongs in Notices,
// because a default configuration that warns is how an operator learns to skip
// warnings, and the next one will be a real gate they have turned off.
// A warning is emitted ONCE PER AFFECTED ROOT and NAMES that root, because the gates are
// now per root: one process may run over a film library with every floor in place and a
// grainy-anime library with the worst-frame floor turned off, and an unattributed
// "vmaf_min_pool is 0" would leave an operator unable to tell which of their libraries
// had lost it. A root whose resolved profile did not weaken a gate contributes no
// warning about it.
func (c *Config) Warnings() []string {
	roots := c.RootProfiles()
	if len(roots) == 0 {
		// No roots to attribute a weakened gate to (a Config assembled by hand; Validate
		// refuses to RUN one). The top-level profile is still what would decide files, so
		// it is still reported - just with nothing to name.
		return c.TopLevelProfile().warnings("")
	}
	var w []string
	for _, r := range roots {
		w = append(w, r.Profile.warnings(r.Clean)...)
	}
	return w
}

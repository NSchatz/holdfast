// Command transcode is a config-as-code, data-safe, self-hosted media transcoder —
// an open-source Tdarr replacement. It reclaims disk by re-encoding bloated video
// to a smaller modern codec and NEVER destroys a source until a replacement is
// provably faithful.
//
// `run` performs a single oneshot scan of the configured library roots (the
// TRANSCODE-1 data-safety core: skip guards → same-dir temp encode → verify → atomic
// swap → delete). The persistent queue + worker pool (TRANSCODE-5), colour/HDR
// (TRANSCODE-3), VMAF (TRANSCODE-4), and the API/UI (TRANSCODE-7) build on it — see
// operations/roadmaps/transcode.md in the umbrella.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/engine"
	"github.com/NSchatz/transcode/internal/ledger"
	"github.com/NSchatz/transcode/internal/logging"
	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/version"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `transcode — config-as-code, data-safe media transcoder (open-source Tdarr replacement)

Usage:
  transcode <command> [flags]

Commands:
  run        Load config and run one transcode scan over the library roots
  validate   Load and validate a config file, then exit
  version    Print version and exit

Run "transcode <command> -h" for command flags.
`

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "transcode: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// loadConfig parses --config and returns a validated Config, or a nonzero exit
// code written to stderr. Shared by run and validate so both enforce identically.
func loadConfig(fs *flag.FlagSet, args []string, stderr io.Writer) (*config.Config, int) {
	path := fs.String("config", "", "path to the YAML config file (required)")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		// -h/--help: flag already printed usage; exit cleanly (success), and the
		// caller must not proceed to use the nil config (it checks cfg == nil).
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}
		return nil, 2
	}
	if *path == "" {
		fmt.Fprintln(stderr, "transcode: --config is required")
		return nil, 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "transcode: %v\n", err)
		return nil, 1
	}
	// Load already returns a fully-defaulted config (koanf defaults layer) and
	// distinguishes an explicit zero (e.g. crf: 0) from an absent key.
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "transcode: invalid config: %v\n", err)
		return nil, 1
	}
	return cfg, 0
}

func cmdValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	fmt.Fprintf(stdout, "config OK: %d library root(s)\n", len(cfg.LibraryRoots))
	return 0
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	log := logging.New(cfg.LogLevel)

	ffmpeg := envOr("TRANSCODE_FFMPEG", "ffmpeg")
	ffprobe := envOr("TRANSCODE_FFPROBE", "ffprobe")
	// Fail loud if the tools are missing — never a false green / silent no-op.
	for _, bin := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(stderr, "transcode: required binary %q not found: %v\n", bin, err)
			return 1
		}
	}

	log.Info("transcode starting",
		"version", version.Version,
		"library_roots", cfg.LibraryRoots,
		"encoder", cfg.Encoder, "crf", cfg.CRF, "preset", cfg.Preset,
		"dry_run", cfg.DryRun,
	)

	prober := probe.New(ffmpeg, ffprobe)
	enc := engine.FFmpegEncoder{FFmpeg: ffmpeg, Cfg: *cfg, Probe: prober}
	led := ledger.New(filepath.Join(cfg.StateDir, "processed.ledger"))
	eng := engine.New(*cfg, prober, enc, led, log)

	// SIGINT/SIGTERM cancels the context: the in-flight ffmpeg is killed, the temp
	// discarded, and the source left untouched (the swap is the only mutation and
	// only runs after a full verify, which an interrupted encode never reaches).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := eng.RunOneshot(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			log.Warn("interrupted — stopped safely; in-flight temp discarded, source untouched")
			return 0
		}
		fmt.Fprintf(stderr, "transcode: %v\n", err)
		return 1
	}
	log.Info("scan complete")
	return 0
}

// envOr returns the environment value for key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

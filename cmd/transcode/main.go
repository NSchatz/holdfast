// Command transcode is a config-as-code, data-safe, self-hosted media transcoder —
// an open-source Tdarr replacement. It reclaims disk by re-encoding bloated video
// to a smaller modern codec and NEVER destroys a source until a replacement is
// provably faithful.
//
// This is the genesis scaffold (TRANSCODE-0): it wires the CLI, config loading,
// logging, and version stamping. The transcode engine (verify-then-swap-then-delete,
// the queue, the API/UI) lands in later phases — see the roadmap at
// operations/roadmaps/transcode.md in the umbrella. Until then, `run` deliberately
// refuses to touch any file: a delete-capable tool must not do anything surprising
// on first run.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/logging"
	"github.com/NSchatz/transcode/internal/version"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `transcode — config-as-code, data-safe media transcoder (open-source Tdarr replacement)

Usage:
  transcode <command> [flags]

Commands:
  run        Load config and start the transcoder (engine arrives in TRANSCODE-1)
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
	log.Info("transcode starting",
		"version", version.Version,
		"library_roots", cfg.LibraryRoots,
		"dry_run", cfg.DryRun,
	)
	// Fail-safe: the transcode engine is not implemented yet (TRANSCODE-1). We
	// have proven the config is valid and safe, but we will NOT scan or mutate any
	// file until the verify-then-swap-then-delete engine exists. Exit cleanly with
	// a clear message rather than pretending to work.
	log.Warn("transcode engine not yet implemented — no files will be touched",
		"next", "TRANSCODE-1 (data-safety core port)")
	fmt.Fprintln(stdout, "transcode: config validated; engine not yet implemented (TRANSCODE-1). No files were touched.")
	return 0
}

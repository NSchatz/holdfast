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
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/encoder"
	"github.com/NSchatz/transcode/internal/engine"
	"github.com/NSchatz/transcode/internal/logging"
	"github.com/NSchatz/transcode/internal/metrics"
	"github.com/NSchatz/transcode/internal/notify"
	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/schedule"
	"github.com/NSchatz/transcode/internal/server"
	"github.com/NSchatz/transcode/internal/store"
	"github.com/NSchatz/transcode/internal/version"
	"github.com/NSchatz/transcode/internal/webui"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `transcode — config-as-code, data-safe media transcoder (open-source Tdarr replacement)

Usage:
  transcode <command> [flags]

Commands:
  run        Load config and run one transcode scan over the library roots
  serve      Run the HTTP API + web UI (scan on demand / on an interval)
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
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
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

// buildEngine performs the setup shared by `run` and `serve`: locate ffmpeg/
// ffprobe (fail loud if missing), confirm the configured encoder actually works on
// this host (never a silent cpu fallback), open the job store, and construct the
// engine. It returns the engine, the store (the caller MUST Close it), and a
// nonzero exit code on failure (with a message already written to stderr).
func buildEngine(cfg *config.Config, log *slog.Logger, stderr io.Writer) (*engine.Engine, store.Store, int) {
	ffmpeg := envOr("TRANSCODE_FFMPEG", "ffmpeg")
	ffprobe := envOr("TRANSCODE_FFPROBE", "ffprobe")
	// Fail loud if the tools are missing — never a false green / silent no-op.
	for _, bin := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(stderr, "transcode: required binary %q not found: %v\n", bin, err)
			return nil, nil, 1
		}
	}

	// Capability check: Validate only confirms the encoder KEY is known — it has no
	// ffmpeg to actually test with. Here, with ffmpeg in hand, confirm the
	// configured encoder actually WORKS in this build/on this host before doing
	// anything else. This is a hard, loud failure — NEVER a silent fallback to cpu.
	// A hardware encoder (nvenc/qsv/vaapi/amf) with no matching device, or an
	// ffmpeg build missing a codec, must stop before any work rather than let every
	// file either fail one-by-one or (worse, for some hardware encoders) appear to
	// "succeed" while writing nothing. cfg.Encoder is always a valid registry key
	// here (Load defaults it to "cpu"; Validate rejects an unknown/empty encoder).
	if _, err := encoder.RequireAvailable(context.Background(), ffmpeg, ffprobe, cfg.Encoder); err != nil {
		fmt.Fprintf(stderr, "transcode: %v\n", err)
		return nil, nil, 1
	}

	prober := probe.New(ffmpeg, ffprobe)
	enc := engine.FFmpegEncoder{FFmpeg: ffmpeg, Cfg: *cfg, Probe: prober}
	// Belt: an explicit empty state_dir must not silently write the job DB into the
	// process CWD (Load defaults it to "state"; this covers `state_dir: ""`).
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = "state"
	}
	st, err := store.Open(filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		fmt.Fprintf(stderr, "transcode: opening job store: %v\n", err)
		return nil, nil, 1
	}
	return engine.New(*cfg, prober, enc, st, log), st, 0
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
		"encoder", cfg.Encoder, "crf", cfg.CRF, "preset", cfg.Preset,
		"dry_run", cfg.DryRun,
	)

	eng, st, code := buildEngine(cfg, log, stderr)
	if code != 0 {
		return code
	}
	defer st.Close()

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

// cmdServe runs the HTTP API + embedded web UI (TRANSCODE-7). It builds the same
// engine as `run`, wires it to a Controller (scan/pause) and an SSE Hub (live
// state), then serves until SIGINT/SIGTERM. The API is a read-and-control surface:
// it can start a scan and pause new-file feeding, but nothing here ever touches a
// media file — the data-safety invariant is entirely in the engine.
func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	log := logging.New(cfg.LogLevel)

	// One context for the whole daemon: SIGINT/SIGTERM cancels it, which stops the
	// scan loop, releases SSE streams, and cancels any in-flight encode (ffmpeg
	// killed, temp discarded, source intact).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServer(ctx, cfg, log, stderr)
}

// runServer is the serve core, parameterized on ctx so it is testable without
// signals: it serves until ctx is cancelled, then drains gracefully. cmdServe wraps
// it with a signal-bound context.
func runServer(ctx context.Context, cfg *config.Config, log *slog.Logger, stderr io.Writer) int {
	eng, st, code := buildEngine(cfg, log, stderr)
	if code != 0 {
		return code
	}
	defer st.Close()

	ctrl := server.NewController(ctx, eng.RunOneshot, log)
	hub := server.NewHub(st, ctrl, log)
	ctrl.SetOnChange(hub.Trigger) // a pause/scan-state flip broadcasts to SSE clients

	// Observability + host-fair scheduling (TRANSCODE-8), all optional and additive.
	// The engine's single Observer fans out to every consumer; each consumer is
	// non-blocking, so none can stall an encode.
	observers := []engine.Observer{hub.Observe}

	var metricsHandler http.Handler
	if cfg.MetricsEnable {
		mx := metrics.New(st)
		observers = append(observers, mx.Observe)
		metricsHandler = mx.Handler()
	}

	notifier := notify.New(cfg.NotifyURL, log)
	if notifier.Enabled() {
		observers = append(observers, notifier.Observe)
		ctrl.SetScanHooks(notifier.ScanStarted, notifier.ScanFinished)
	}
	eng.Observer = fanout(observers) // live job-state → SSE + metrics + notifications

	// Host-fair scheduler: run-window + CPU-load cap + optional Tautulli pause. It
	// only ever DELAYS work. The engine consults it (throttled) between files; Rescan
	// consults it before starting a scan.
	window, _ := schedule.ParseWindow(cfg.RunWindow) // already validated
	sched := schedule.New(window, cfg.MaxLoad, schedule.NewTautulli(cfg.TautulliURL, cfg.TautulliAPIKey), log)
	ctrl.SetGate(func() (bool, string) { return sched.MayRun(ctx) })
	eng.Paused = func() bool {
		if ctrl.Paused() {
			return true
		}
		ok, _ := sched.MayRunThrottled(ctx)
		return !ok
	}

	srv := server.New(ctx, *cfg, st, ctrl, hub, webui.Handler(), metricsHandler, log)
	var bg sync.WaitGroup
	bg.Add(3)
	go func() { defer bg.Done(); hub.Run(ctx) }()
	go func() { defer bg.Done(); notifier.Run(ctx) }()
	go func() { defer bg.Done(); srv.StartScanLoop(ctx, cfg.ScanIntervalSec) }() // initial scan + optional interval

	addr := cfg.EffectiveServerAddr()
	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Info("serve listening",
			"addr", addr,
			"control_enabled", cfg.ServerAuthToken != "",
			"scan_interval_sec", cfg.ScanIntervalSec,
			"metrics", cfg.MetricsEnable,
			"notify", notifier.Enabled(),
			"run_window", window.String(),
			"max_load", cfg.MaxLoad,
			"tautulli", cfg.TautulliURL != "" && cfg.TautulliAPIKey != "",
			"version", version.Version,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down (signal) — draining connections")
	case err := <-errCh:
		fmt.Fprintf(stderr, "transcode: server error: %v\n", err)
		return 1
	}
	// Graceful shutdown: stop accepting, let handlers return (SSE streams already
	// released via the cancelled base ctx). ctx is already cancelled, so use a
	// fresh, bounded context for the drain.
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shCtx); err != nil {
		log.Warn("graceful shutdown timed out; forcing close", "err", err)
		_ = httpSrv.Close()
	}
	// Join background goroutines before the deferred st.Close(): ctx is already
	// cancelled, so the scan loop + hub have stopped and any in-flight scan is
	// unwinding — wait for the scan goroutine to finish issuing store calls so the
	// store handle is never closed out from under it.
	ctrl.Wait()
	bg.Wait()
	return 0
}

// fanout composes several observers into one engine.Observer, calling each in
// order on every event. Every observer is contractually non-blocking, so the
// fan-out is too — it never stalls an engine worker.
func fanout(obs []engine.Observer) engine.Observer {
	return func(ev engine.Event) {
		for _, o := range obs {
			o(ev)
		}
	}
}

// envOr returns the environment value for key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

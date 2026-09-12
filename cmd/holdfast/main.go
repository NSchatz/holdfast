// Command holdfast is a config-as-code, data-safe, self-hosted media transcoder —
// an open-source Tdarr replacement. It reclaims disk by re-encoding bloated video
// to a smaller modern codec and NEVER destroys a source until a replacement is
// provably faithful.
//
// `run` performs a single oneshot scan of the configured library roots (the
// TRANSCODE-1 data-safety core: skip guards → same-dir temp encode → verify → atomic
// swap → delete). The persistent queue + worker pool (TRANSCODE-5), colour/HDR
// (TRANSCODE-3), VMAF (TRANSCODE-4), and the API/UI (TRANSCODE-7) build on it — see
// operations/roadmaps/holdfast.md in the umbrella.
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

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/metrics"
	"github.com/NSchatz/holdfast/internal/notify"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
	"github.com/NSchatz/holdfast/internal/vmaf"
	"github.com/NSchatz/holdfast/internal/webui"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `holdfast — config-as-code, data-safe media transcoder (open-source Tdarr replacement)

Usage:
  holdfast <command> [flags]

Commands:
  run        Load config and run one transcode scan over the library roots
  serve      Run the HTTP API + web UI (scan on demand / on an interval)
  resolve    Report and resolve a job whose swap outcome could not be established
  restore    List what the undo window is holding, or put one original back
  requeue    Offer a file the engine has already answered back to the pipeline
  export     Write every terminal ledger row to newline-delimited JSON (stdout, or --out)
  validate   Load and validate a config file, then exit
  version    Print version and exit

Run "holdfast <command> -h" for command flags.

  holdfast restore --config config.yaml            # what is retained, and for how long
  holdfast restore --config config.yaml <path>     # put that original back
  holdfast requeue --config config.yaml <path>     # re-open that file's terminal row
  holdfast requeue --config config.yaml --failed   # re-open every row parked at max_failures
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
	case "resolve":
		return cmdResolve(args[1:], stdout, stderr)
	case "restore":
		return cmdRestore(args[1:], stdout, stderr)
	case "requeue":
		return cmdRequeue(args[1:], stdout, stderr)
	case "export":
		return cmdExport(args[1:], stdout, stderr)
	case "validate":
		return cmdValidate(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "holdfast: unknown command %q\n\n%s", args[0], usage)
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
		fmt.Fprintln(stderr, "holdfast: --config is required")
		return nil, 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: %v\n", err)
		return nil, 1
	}
	// Load already returns a fully-defaulted config (koanf defaults layer) and
	// distinguishes an explicit zero (e.g. crf: 0) from an absent key.
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "holdfast: invalid config: %v\n", err)
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
	printResolvedProfiles(stdout, cfg)
	// What this configuration MEANS, before what it has weakened. A disabled undo
	// window is the shipped default and not a weakened gate, but it is the setting in
	// which a swap is final - so it is stated here rather than silently absorbed.
	for _, n := range cfg.Notices() {
		fmt.Fprintf(stdout, "note: %s\n", n)
	}
	reportLedgerAgainstConfig(cfg, stdout)
	// Valid, but a safety gate is weakened — say so. These are not errors (each is a
	// legitimate choice), but a config that has quietly lost its worst-frame floor
	// must not look identical to one that still has it.
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	return 0
}

// reportLedgerAgainstConfig prints what the ledger says about the configuration it was
// decided under: how many terminal rows a scan would now re-open, and why.
//
// `validate` is the one command that answers "what will this configuration DO", and
// since a terminal row is only terminal for the configuration it was taken under, the
// answer is incomplete without it. It is a real widening of what `validate` touches, so
// two properties are load-bearing and are asserted by their own cases:
//
//   - it opens the ledger READ-ONLY, because a validate that migrated an operator's
//     store as a side effect of describing it would make that file unopenable by the
//     daemon still running against it (see store.OpenReadOnly);
//   - a state directory with no ledger in it is not a failure and is not created. A
//     fresh install has nothing to say here and `validate` must still pass, so this
//     reports the absence and returns;
//   - a ledger the PREVIOUS build wrote still yields both figures. That is the
//     population they matter most for - every row in it records nothing, so the first
//     scan after an upgrade re-opens the whole terminal set - and reading it needs no
//     migration, because a schema without the column is a schema under which no row can
//     have recorded anything (see store.SurveyLedgerDecisionInputs).
//
// Nothing here can fail the command. `validate` validates a CONFIGURATION; a ledger that
// could not be read is reported as unreadable beside a config that is still valid.
func reportLedgerAgainstConfig(cfg *config.Config, stdout io.Writer) {
	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stdout, "ledger: none at %s yet, so there is nothing to re-open\n", dbPath)
		return
	}
	survey, err := store.SurveyLedgerDecisionInputs(context.Background(), dbPath,
		engine.DecisionInputsFor(*cfg))
	if err != nil {
		fmt.Fprintf(stdout, "ledger: %s could not be read, so what it was decided under cannot be "+
			"reported here: %v\n", dbPath, err)
		return
	}
	for _, line := range decisionInputsLines(survey) {
		fmt.Fprintf(stdout, "ledger: %s\n", line)
	}
}

// printResolvedProfiles prints, for each configured root, the effective value of every
// overridable knob and WHICH LAYER supplied it.
//
// It prints what the inheritance PRODUCED, never the file as written, and that is the
// whole point of it. A knob may be a built-in default, a top-level choice or that root's
// own profile, and the resolved value is identical in all three cases - so reading the
// YAML back cannot tell an operator what a root will actually do to their files. Only
// this can, and it matters most on the knobs whose wrong value ends in a deleted
// original.
//
// The digest is printed beside the root because it is what a terminal row records: an
// operator holding a job's `profile_digest` can run `validate` and see which of their
// roots decided it, even after they have edited the others.
func printResolvedProfiles(w io.Writer, cfg *config.Config) {
	for _, r := range cfg.RootProfiles() {
		fmt.Fprintf(w, "\nlibrary root %s (profile %s)", r.Clean, r.Profile.Digest())
		if r.Path != r.Clean {
			fmt.Fprintf(w, " [configured as %s]", r.Path)
		}
		fmt.Fprintln(w)
		for _, k := range r.Effective() {
			fmt.Fprintf(w, "  %-20s %-24s from %s\n", k.Knob, k.Value, k.Layer)
		}
	}
}

// cmdRestore is the operator's half of the undo window (UNDO-6): with no argument it
// lists what is retained and how long each has left; with a path it puts that original
// back.
//
// It is deliberately a LOCAL command and not an HTTP endpoint. Restoring overwrites a
// library file with older bytes, which is a mutation, and a mutating endpoint opens an
// authorization question the read-and-control API does not currently answer. It is
// also deliberately cheap: it loads the same config and opens the same state directory
// as `run`/`serve` and stops there - no ffmpeg lookup, no encoder capability check, no
// library walk - because none of those bear on moving one file back into place.
func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	rest := fs.Args()
	if len(rest) > 1 {
		fmt.Fprintf(stderr, "holdfast: restore takes at most one path (got %d)\n", len(rest))
		return 2
	}

	log := logging.New(cfg.LogLevel)
	st, err := store.Open(filepath.Join(stateDirPath(cfg), "jobs.db"))
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: opening job store: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	undo := engine.NewUndoWindow(*cfg, st, log)
	if len(rest) == 0 {
		return listRetained(context.Background(), undo, cfg, stdout, stderr)
	}
	return restoreOne(context.Background(), undo, rest[0], stdout, stderr)
}

// listRetained prints what the undo window is holding. Each line states the space that
// original is HOLDING and how long is left on it, because those are the two facts an
// operator is deciding between: whether to put a file back, and whether to wait for
// the space instead.
func listRetained(ctx context.Context, undo *engine.UndoWindow, cfg *config.Config, stdout, stderr io.Writer) int {
	rows, err := undo.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: reading the retained originals: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		if !cfg.UndoEnabled() {
			fmt.Fprintln(stdout, "nothing is retained: undo_window_hours is 0, so the undo window is disabled and every swap is final.")
			return 0
		}
		fmt.Fprintf(stdout, "nothing is retained: no swap has happened inside the current %dh undo window.\n", cfg.UndoWindowHours)
		return 0
	}
	var held int64
	for _, r := range rows {
		held += r.SourceBytes
	}
	fmt.Fprintf(stdout, "%d retained original(s), holding %d byte(s) that the undo window has not yet returned:\n", len(rows), held)
	now := time.Now().Unix()
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %s  %d bytes  %s  (retained %s)\n",
			r.SourcePath, r.SourceBytes, remainingWindow(r.ExpiresAt, now), r.RetainedPath)
	}
	fmt.Fprintln(stdout, "restore one with: holdfast restore --config <file> <path>")
	return 0
}

// remainingWindow renders how long a retention has left. An expired one says so rather
// than printing a negative duration: it is not gone yet (the release runs at the start
// of the next scan), and the honest report of that state is its own sentence.
func remainingWindow(expiresAt, now int64) string {
	left := time.Duration(expiresAt-now) * time.Second
	if left <= 0 {
		return "window closed - released on the next scan"
	}
	return left.Round(time.Minute).String() + " left"
}

// restoreOne puts one original back, or refuses and says why. Every refusal exits
// NONZERO and has mutated nothing: the whole value of a restore is that an operator
// can trust what it did, and a command that half-restored while reporting a failure
// would be worse than one that never existed.
func restoreOne(ctx context.Context, undo *engine.UndoWindow, path string, stdout, stderr io.Writer) int {
	res, err := undo.Restore(ctx, absoluteLedgerPath(path))
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: cannot restore %s: %v\n", path, err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s (%d bytes) from %s\n", res.Record.SourcePath, res.Record.SourceBytes, res.Record.RetainedPath)
	if res.Removed != "" {
		fmt.Fprintf(stdout, "removed the encode it replaced: %s\n", res.Removed)
	}
	fmt.Fprintf(stdout, "the ledger records the restore at %d; the file will not be re-encoded until it changes.\n", res.RestoredAt)
	return 0
}

// buildEngine performs the setup shared by `run` and `serve`: locate ffmpeg/
// ffprobe (fail loud if missing), confirm the configured encoder actually works on
// this host (never a silent cpu fallback), open the job store, and construct the
// engine. It returns the engine, the store (the caller MUST Close it), and a
// nonzero exit code on failure (with a message already written to stderr).
func buildEngine(cfg *config.Config, log *slog.Logger, stderr io.Writer) (*engine.Engine, store.Store, int) {
	// FILESYSTEM-1, and it goes FIRST. holdfast's no-loss contract is stated for
	// a local filesystem and this is where the tool checks it has one: it
	// classifies every path this run would act on, reports each, and takes ONE
	// start-or-refuse decision over the whole set - before any encode, before
	// the job store is opened, and before anything is created in or under the
	// state directory, so a refused run leaves no jobs.db, journal or sidecar
	// behind on storage it just refused.
	res, code := startupCheck(cfg, log, stderr)
	if code != 0 {
		return nil, nil, code
	}

	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobe := envOr("HOLDFAST_FFPROBE", "ffprobe")
	// Fail loud if the tools are missing — never a false green / silent no-op.
	for _, bin := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(stderr, "holdfast: required binary %q not found: %v\n", bin, err)
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
	// "succeed" while writing nothing. Each profile's encoder is always a valid
	// registry key here (Load defaults it to "cpu"; Validate rejects an unknown one).
	//
	// EVERY distinct encoder any root resolved to is checked, not just the top-level
	// one: a root that says `encoder: nvenc` on a host with no NVIDIA device must stop
	// the run here, exactly as a top-level nvenc does. Distinct, because the check runs
	// a real encode and several roots usually share one encoder.
	for _, e := range distinctBy(cfg, func(p config.Profile) string { return p.Encoder }) {
		if _, err := encoder.RequireAvailable(context.Background(), ffmpeg, ffprobe, e.key); err != nil {
			fmt.Fprintf(stderr, "holdfast: %s: %v\n", e.where, err)
			return nil, nil, 1
		}
	}

	// VMAF model preflight (GATE-4), in the same band and for the same reason as the
	// encoder check above: a capability that will not work must stop the run HERE, not
	// after a library's worth of encoding. Until this existed the gate only checked
	// that the libvmaf FILTER exists, which says nothing about whether the build ships
	// the MODEL the filter was asked for - and holdfast's own `auto` selects
	// vmaf_4k_v0.6.1 above 1440 lines, a spec the FFmpeg filter documentation does not
	// even enumerate. A typo in vmaf_model had the same shape: hours of encoding, then
	// a rejection per file, because an unmeasurable encode is (correctly) never
	// accepted.
	//
	// It runs ONLY when the gate is enabled. With vmaf_enable: false nothing will ever
	// ask libvmaf for a model, so refusing the run over one would be refusing a
	// configuration that cannot fail - and logConfigWarnings has already said, loudly,
	// that there is no perceptual gate at all.
	//
	// Placement is load-bearing and asserted by a test: BEFORE store.Open, so a
	// refused run leaves no jobs.db behind, and long before anything is encoded or
	// swapped.
	//
	// Per resolved profile, for the same reason the encoder check is: `vmaf_model` is a
	// per-root knob, so a typo in one root's model is hours of encoding followed by a
	// rejection per file under that root - and a root whose gate is OFF asks libvmaf for
	// nothing, so refusing the run over its model would be refusing a configuration that
	// cannot fail.
	for _, m := range distinctBy(cfg, func(p config.Profile) string {
		if !p.VmafGate() {
			return ""
		}
		return p.VmafModel
	}) {
		if m.key == "" {
			continue
		}
		if err := vmaf.RequireModel(context.Background(), ffmpeg, m.key); err != nil {
			fmt.Fprintf(stderr, "holdfast: %s: %v\n", m.where, err)
			return nil, nil, 1
		}
	}

	prober := probe.New(ffmpeg, ffprobe)
	enc := engine.FFmpegEncoder{FFmpeg: ffmpeg, Cfg: *cfg, Probe: prober}
	// Belt: an explicit empty state_dir must not silently write the job DB into the
	// process CWD (Load defaults it to "state"; this covers `state_dir: ""`). The
	// defaulting lives in ONE function so `export` reads the database `run` wrote.
	st, err := store.Open(filepath.Join(effectiveStateDir(cfg), "jobs.db"))
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: opening job store: %v\n", err)
		return nil, nil, 1
	}
	// What the ledger was decided under, BEFORE anything re-opens: a scan offers every
	// row whose recorded decision inputs have moved - and every row that records none -
	// back to the guards, and an operator meeting a burst of activity they did not ask
	// for is owed the reason in front of it rather than in a log line per file.
	//
	// A failure here is reported and survived. It is an announcement about work the scan
	// is about to do; the scan does that work whether or not it could be counted first,
	// and refusing to start over an unreadable count would turn a reporting nicety into
	// an outage.
	if survey, err := decisionInputsReport(context.Background(), st, cfg); err != nil {
		log.Warn("could not report what the ledger was decided under (the scan is unaffected)", "err", err)
	} else {
		log.Info("ledger against this configuration",
			"rows_taken_under_a_moved_configuration", survey.Moved,
			"rows_recording_no_decision_inputs", survey.NotRecorded,
			"rows_this_scan_reopens", survey.Reopening(),
			"rows_still_matching", survey.Matching)
		for _, line := range decisionInputsLines(survey) {
			log.Info(line)
		}
	}

	eng := engine.New(*cfg, prober, enc, st, log)
	// The startup walk's coverage BOUNDS the run: this scan enumerates sources
	// from exactly the directories that walk traversed successfully, so a
	// subtree it declined, could not read or failed to traverse yields no file
	// and no swap can happen under it. Its listings come across with it, so the
	// first scan reads the entries that walk already read rather than paying for
	// the same directories twice more.
	eng.SetCoverage(res.Coverage, res.Entries)
	return eng, st, 0
}

// profileUse is one distinct value a capability preflight has to check, and the roots
// that asked for it - so a refusal names the library an operator has to go and edit
// rather than only the value that failed.
type profileUse struct {
	key   string
	where string
}

// distinctBy collects the distinct values key returns across every resolved root, in
// configuration order, each carrying the roots that produced it.
//
// Distinct, because a preflight is expensive - the encoder check runs a real encode -
// and a library with twelve roots on one encoder must not pay for twelve of them.
func distinctBy(cfg *config.Config, key func(config.Profile) string) []profileUse {
	var out []profileUse
	at := map[string]int{}
	for _, r := range cfg.RootProfiles() {
		k := key(r.Profile)
		if i, seen := at[k]; seen {
			out[i].where += ", " + r.Clean
			continue
		}
		at[k] = len(out)
		out = append(out, profileUse{key: k, where: "library root " + r.Clean})
	}
	return out
}

// stateDirPath is the absolute path the configuration interpretation produces
// for the state directory - never the configured string, which may be the
// shipped RELATIVE default. It is what store.Open will use, and it is therefore
// what the startup check classifies and what a printed declaration spells.
func stateDirPath(cfg *config.Config) string {
	dir := effectiveStateDir(cfg)
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return abs
}

// startupPlatform builds the view of the host the startup check reads.
// Production reads the real host; a test substitutes the two seams it needs (the
// filesystem-type lookup and the mount layout), because the gate this repo is
// tested on has neither a network mount nor a second real filesystem.
var startupPlatform = func() startup.Platform { return startup.System(nil, nil) }

// startupCheck runs the whole-run start-or-refuse decision and reports it. On a
// refusal it writes the operator-facing account to stderr - every cause it
// established, each with the exact declaration that would permit it or, where no
// declaration could, the remedy - and returns a nonzero exit code.
func startupCheck(cfg *config.Config, log *slog.Logger, stderr io.Writer) (startup.Result, int) {
	res := startup.Run(startup.Check{
		Roots:        cfg.LibraryRoots,
		StateDir:     stateDirPath(cfg),
		Declarations: cfg.AllowNonLocal,
		IsMediaFile:  func(base string) bool { return engine.IsSourceName(base, cfg.VideoExts) },
		Platform:     startupPlatform(),
	})
	res.Log(log)
	if !res.Start {
		res.WriteRefusal(stderr)
		return res, 1
	}
	return res, 0
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	log := logging.New(cfg.LogLevel)

	log.Info("holdfast starting",
		"version", version.Version,
		"library_roots", cfg.LibraryRoots,
		"encoder", cfg.Encoder, "crf", cfg.CRF, "preset", cfg.Preset,
		"dry_run", cfg.DryRun,
	)
	logResolvedProfiles(cfg, log)
	logConfigWarnings(cfg, log)

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
		fmt.Fprintf(stderr, "holdfast: %v\n", err)
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
	logResolvedProfiles(cfg, log)
	logConfigWarnings(cfg, log)

	// One context for the whole daemon: SIGINT/SIGTERM cancels it, which stops the
	// scan loop, releases SSE streams, and cancels any in-flight encode (ffmpeg
	// killed, temp discarded, source intact).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServer(ctx, cfg, log, stderr)
}

// logConfigWarnings announces, at daemon startup, what this configuration means and
// what it has weakened. A tool that deletes originals should say out loud when the
// gate protecting them has been narrowed — silence here is how a weakened config
// becomes invisible.
//
// Notices come first, and they are logged at WARN even though they are not warnings.
// The two lists stay separate (`validate` prints them as `note:` against `warning:`,
// and Notices exists precisely so a shipped default is never dressed as a weakened
// gate), but a LOG LEVEL is not a severity classification, it is how loud something
// has to be to survive the operator turning the volume down. `log_level: warn` is a
// legal setting, and at INFO this announcement vanished there entirely: the daemon
// started, swapped a file and said nothing about the swap being final. A statement
// that is inaudible at a level an operator may legitimately choose has not been made.
//
// WARN puts it on exactly the footing of the safety warnings below, which is the
// right one: at `log_level: error` both go quiet, and `docs/undo.md` says so.
// logResolvedProfiles states, at startup, what each root actually resolved to. The
// "holdfast starting" line beside it carries the TOP-LEVEL values, and with per-library
// profiles those are no longer what any particular file is judged by - so a run that
// only announced them would report a crf and an encoder that may govern no root at all.
// The digest is the one a terminal row records, so a log and a ledger row can be matched
// up afterwards.
func logResolvedProfiles(cfg *config.Config, log *slog.Logger) {
	for _, r := range cfg.RootProfiles() {
		log.Info("library root resolved",
			"library_root", r.Clean, "profile_digest", r.Profile.Digest(),
			"encoder", r.Profile.Encoder, "crf", r.Profile.CRF, "preset", r.Profile.Preset,
			"min_bitrate_kbps", r.Profile.MinBitrateKbps,
			"vmaf_enable", r.Profile.VmafGate(), "min_vmaf", r.Profile.MinVmaf,
			"vmaf_min_pool", r.Profile.VmafMinPool, "vmaf_min_chroma", r.Profile.VmafMinChroma)
	}
}

func logConfigWarnings(cfg *config.Config, log *slog.Logger) {
	for _, n := range cfg.Notices() {
		log.Warn(n)
	}
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}
}

// runServer is the serve core, parameterized on ctx so it is testable without
// signals: it serves until ctx is cancelled, then drains gracefully. cmdServe wraps
// it with a signal-bound context.
func runServer(ctx context.Context, cfg *config.Config, log *slog.Logger, stderr io.Writer) int {
	// THE source-URL refusal site (LICENSE-3), and it is deliberately the first
	// statement in the function. Every listener this program can create is created
	// below, and both branches that can serve the root path - the embedded dashboard
	// and the API-only page - are behind it, so a build whose Corresponding Source
	// URL is unusable serves neither.
	//
	// It sits AHEAD of buildEngine, which is where the whole-run start-or-refuse
	// decision over the filesystem lives (FILESYSTEM-1). Same shape, same exit
	// contract - refuse the whole run, say which value was rejected, exit non-zero -
	// and deliberately not folded into that decision, which is about the paths this
	// run would act on and is taken by `run` too. `run` starts no listener that
	// serves the root path, so an unusable source URL is none of its business.
	// Ordering it first means the refusal costs no ffmpeg probe, no store open and
	// no filesystem walk: nothing happens at all.
	//
	// A refusal, never a fall back to upstream. A fork whose override is malformed
	// and silently fell back would tell its users that upstream is the source of a
	// binary it is not.
	offer, err := sourceoffer.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing to serve: %v\n", err)
		return 1
	}

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

	srv := server.New(ctx, *cfg, st, ctrl, hub, webui.HandlerFor(offer), metricsHandler, log)
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
		fmt.Fprintf(stderr, "holdfast: server error: %v\n", err)
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

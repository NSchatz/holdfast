// Command holdfast is a config-as-code, data-safe, self-hosted media transcoder —
// an open-source Tdarr replacement. It reclaims disk by re-encoding bloated video
// to a smaller modern codec and NEVER destroys a source until a replacement is
// provably faithful.
//
// `run` performs a single oneshot scan of the configured library roots (the
// TRANSCODE-1 data-safety core: skip guards → same-dir temp encode → verify → atomic
// swap → delete). The persistent queue + worker pool (TRANSCODE-5), colour/HDR
// (TRANSCODE-3), VMAF (TRANSCODE-4), and the HTTP API (TRANSCODE-7) build on it. The
// plan of record is the program brief, .claude/goals/2026-09-holdfast.md, and no longer
// a roadmap in the umbrella - decided by the owner (T2, T8).
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/health"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/metrics"
	"github.com/NSchatz/holdfast/internal/notify"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/schedule"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `holdfast — config-as-code, data-safe media transcoder (open-source Tdarr replacement)

Usage:
  holdfast <command> [flags]

Commands:
  run        Load config and run one transcode scan over the library roots
  serve      Run the HTTP API (scan on demand / on an interval)
  worker     Lease encodes from a serve at worker_server and upload the outputs (a node)
  analyze    Census the library roots: file counts, bytes and distributions (reads only)
  plan       Report what this configuration would do to the library and what it would save
  resolve    Report and resolve a job whose swap outcome could not be established
  restore    List what the undo window is holding, or put one original back
  requeue    Offer a file the engine has already answered back to the pipeline
  export     Write every terminal ledger row to newline-delimited JSON (stdout, or --out)
  validate   Load and validate a config file, then exit
  version    Print version and exit

Run "holdfast <command> -h" for command flags.

  holdfast analyze --config config.yaml            # what is in the library, without touching it
  holdfast analyze --config config.yaml --health   # and which of it does not decode
  holdfast plan --config config.yaml               # what a run would do, and what it would save
  holdfast plan --config config.yaml --json        # the same plan as one JSON document
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
	case "worker":
		return cmdWorker(args[1:], stdout, stderr)
	case "analyze":
		return cmdAnalyze(args[1:], stdout, stderr)
	case "plan":
		return cmdPlan(args[1:], stdout, stderr)
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
	// The configured working location, checked here for the same reason `run` and
	// `serve` check it before their first encode: a missing, unwritable, overlapping
	// or short-of-space scratch directory refuses those runs, and an operator asking
	// `validate` whether their configuration will start is owed that answer rather
	// than a "config OK" the next `run` contradicts.
	//
	// Only the SCRATCH half of the decision runs. `validate` is deliberately cheap -
	// it loads the configuration and stops, with no ffmpeg lookup, no capability
	// check and no library walk - and none of the scratch questions needs one.
	if scratchCode := validateScratch(cfg, stderr); scratchCode != 0 {
		return scratchCode
	}

	fmt.Fprintf(stdout, "config OK: %d library root(s)\n", len(cfg.LibraryRoots))
	// The order the workers will be fed in, stated wherever the key is stated. An operator
	// watching a queue cannot tell a configured order from an accident of the traversal by
	// looking at what is running, so `validate` says which one is in force BEFORE they point
	// it at a library - and it prints the RESOLVED value, so an absent key reads as the
	// default rather than as nothing at all.
	fmt.Fprintf(stdout, "queue order: %s - the order in which this configuration offers files "+
		"to its workers (one of %s)\n", cfg.EffectiveQueueOrder(), config.QueueOrderList())
	printPriorities(stdout, cfg)
	// And how many workers there are, beside it and for the same reason: `auto` is resolved
	// from a quota the operator cannot see from the file, so the count, the quota it came
	// from and where that quota came from are printed before the first file goes.
	fmt.Fprintln(stdout, workersLine(cfg.WorkerPlan()))
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
	// A configured filter that can match nothing is the same kind of statement and is
	// printed the same way: the configuration is valid, the run is not refused, and what
	// the operator believes is protecting a directory is not protecting one.
	for _, u := range cfg.UnreachablePatterns() {
		fmt.Fprintf(stdout, "warning: %s\n", u)
	}
	return 0
}

// workersLine is `validate`'s statement of the worker pool: the resolved count always, and
// for `auto` the quota it was divided from, where that quota came from (the cgroup's cpu.max
// or the CPU count), the divisor, and - where the quota could not be used - why.
func workersLine(p config.WorkerPlan) string {
	if !p.Auto {
		return fmt.Sprintf("workers: %d - the number of files this configuration encodes at once "+
			"(workers: %s)", p.Workers, p.Setting)
	}
	q := strconv.FormatFloat(p.Quota, 'f', -1, 64)
	line := fmt.Sprintf("workers: %d - auto: floor(Q %s / cores_per_worker %d), where Q is %s CPU(s) "+
		"from %s (%d CPU(s) available to this process)", p.Workers, q, p.CoresPerWorker, q,
		p.QuotaSource, p.CPUs)
	switch {
	case p.Err != nil:
		line += "; the cgroup cpu.max could not be used, so Q is the cpu count: " + p.Err.Error()
	case p.Absent:
		line += "; no cgroup CPU quota applies"
	case p.Unlimited:
		line += "; the cgroup names no CPU ceiling"
	}
	return line
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
		engine.DecisionInputsPerPath(*cfg))
	if err != nil {
		fmt.Fprintf(stdout, "ledger: %s could not be read, so what it was decided under cannot be "+
			"reported here: %v\n", dbPath, err)
		return
	}
	for _, line := range decisionInputsLines(survey) {
		fmt.Fprintf(stdout, "ledger: %s\n", line)
	}
}

// logLedgerAgainstConfig is the daemon's half of the same report `validate` prints: what
// the ledger was decided under, logged before the scan re-opens anything.
//
// Every condition it can meet is DEGRADED-AND-CONTINUING, so every one of them is a `warn`
// and none is an `error`. An error means a human must act, and there is nothing for one to
// act on here: the scan does its work whether or not it could be counted first, and a run
// that logged at error over a count would train its reader to ignore the level.
//
// The two warns are different states and are kept apart:
//
//   - the ledger could not be READ at all. The dependency is named (the file), what was
//     tried is named (reading what its terminal rows were decided under), and what happens
//     next is stated (the scan runs unaffected), because a stack trace alone leaves a reader
//     to guess whether the run is still safe.
//   - some rows lie under NO CONFIGURED LIBRARY ROOT. Those files are never enumerated, so
//     they are never re-opened whatever they record, and the count above is an upper bound.
//     The example path is carried so an operator can see which tree it is - usually a root
//     that was renamed or removed - rather than being told a number about files they cannot
//     find.
func logLedgerAgainstConfig(log *slog.Logger, dbPath string, survey store.DecisionInputsSurvey, err error) {
	if err != nil {
		log.Warn("the ledger could not be read, so what it was decided under is not reported",
			"ledger", dbPath,
			"tried", "counting the terminal rows the next scan will re-open",
			"next", "the scan runs unaffected",
			"err", err)
		return
	}
	log.Info("ledger against this configuration",
		"rows_taken_under_a_moved_configuration", survey.Moved,
		"rows_recording_no_decision_inputs", survey.NotRecorded,
		"rows_this_scan_reopens", survey.Reopening(),
		"rows_still_matching", survey.Matching,
		"rows_on_banded_roots_with_no_source_height", survey.NoSourceHeight,
		"rows_under_no_configured_library_root", survey.Unrooted)
	for _, line := range decisionInputsLines(survey) {
		log.Info(line)
	}
	if survey.Unrooted > 0 {
		log.Warn("some terminal rows lie under no configured library root, so the scan never reaches them",
			"ledger", dbPath,
			"rows", survey.Unrooted,
			"example", survey.UnrootedExample,
			"tried", "resolving each row's path to a configured library root",
			"next", "the scan runs unaffected and never enumerates those paths")
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
		printPathFilters(w, r)
		if r.Priority != nil {
			fmt.Fprintf(w, "  %-20s %-24d from this root's entry (orders its files only)\n", "priority", *r.Priority)
		}
		printRules(w, r)
	}
}

// printPriorities states, beside the queue order, that a priority is in force and how a
// file's is chosen, and lists the encode profiles that name one (a root's is printed in its
// block and a rule's on its rule line). With no priority written anywhere it prints nothing,
// so the output of every configuration written before the key existed is unchanged.
func printPriorities(w io.Writer, cfg *config.Config) {
	if !cfg.PriorityConfigured() {
		return
	}
	fmt.Fprintln(w, "priority: files are offered highest priority first, then in the queue order, then by "+
		"path; a file's priority is its matching rule's, else its matching encode profile's, else its "+
		"root's, else 0. It orders files and decides nothing about them")
	for i, p := range cfg.EncodeProfiles {
		if p.Priority != nil {
			fmt.Fprintf(w, "priority: encode_profiles[%d] (%s) priority %d\n", i, p.Name, *p.Priority)
		}
	}
}

// printRules prints the resolution rules in force for one root: every rule, IN LIST ORDER,
// with the band it applies to and the knobs it overrides.
//
// The order is the whole reason this exists. First match wins and nothing merges, so a rule
// is shadowed by any earlier rule whose band contains its own - which is a configuration an
// operator can write, cannot see in their file at a glance, and would otherwise discover
// only as a threshold that never applied. Printing the list in the order the resolver reads
// it is what makes that visible without running the library.
//
// A root with NO rules prints nothing at all, exactly as it did before rules existed: an
// absent list is not a state worth a line, and the shipped configuration carries none.
func printRules(w io.Writer, r config.Root) {
	if len(r.Profile.Rules) == 0 {
		return
	}
	fmt.Fprintf(w, "  %-20s %d rule(s), first match wins, no merging\n", "rules", len(r.Profile.Rules))
	for i, rule := range r.Profile.Rules {
		fmt.Fprintf(w, "  %-20s   [%d] %s\n", "", i, rule)
	}
}

// printPathFilters prints the path filters in force for one root: every pattern, how
// many of them there are, and which layer supplied that list.
//
// The count is a count OF PATTERNS and never of matching files. `validate` describes a
// CONFIGURATION - it opens no directory and stats no path - so a count of files would be
// a number it cannot produce without becoming a different command, and a wrong one would
// be read as "this is how much of my library is protected".
//
// The patterns are printed beneath the knobs rather than beside them because they are not
// knobs: a knob decides what happens to a file and a filter decides whether a file is
// looked at, and the digest above covers the first and not the second.
func printPathFilters(w io.Writer, r config.Root) {
	for _, key := range config.FilterKeys() {
		patterns := r.Filters.Patterns(key)
		count := fmt.Sprintf("%d pattern(s)", len(patterns))
		if len(patterns) == 0 {
			count = "none"
		}
		fmt.Fprintf(w, "  %-20s %-24s from %s\n", key, count, r.Filters.LayerOf(key))
		for _, p := range patterns {
			fmt.Fprintf(w, "  %-20s   %s\n", "", p)
		}
	}
}

// validateScratch reports the scratch directory's start-or-refuse causes, in exactly
// the account `run` and `serve` print, and returns a nonzero exit code when it would
// refuse. With no scratch_dir configured it checks nothing at all and returns 0, so a
// configuration that predates this item is unchanged.
func validateScratch(cfg *config.Config, stderr io.Writer) int {
	if strings.TrimSpace(cfg.ScratchDir) == "" {
		return 0
	}
	res := startup.RunScratchOnly(startup.Check{
		Roots:            cfg.LibraryRoots,
		StateDir:         stateDirPath(cfg),
		ScratchDir:       cfg.ScratchDir,
		ScratchMinFreeGB: cfg.ScratchMinFreeGB,
		Platform:         startupPlatform(),
	})
	if res.Start {
		return 0
	}
	res.WriteRefusal(stderr)
	return 1
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
func buildEngine(cfg *config.Config, log *slog.Logger, stderr io.Writer, scope classifyScope) (*engine.Engine, store.Store, int) {
	// FILESYSTEM-1, and it goes FIRST. holdfast's no-loss contract is stated for
	// a local filesystem and this is where the tool checks it has one: it
	// classifies every path this run would act on, reports each, and takes ONE
	// start-or-refuse decision over the whole set - before any encode, before
	// the job store is opened, and before anything is created in or under the
	// state directory, so a refused run leaves no jobs.db, journal or sidecar
	// behind on storage it just refused.
	res, code := startupCheck(cfg, log, stderr, scope)
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
	// "succeed" while writing nothing. Every key here is a valid registry key (Load
	// defaults the top level to "cpu"; Validate rejects an unknown or empty encoder at
	// the top level, inside a library profile and inside an encode profile alike).
	//
	// EVERY distinct encoder any root resolved to is checked, not just the top-level
	// one: a root that says `encoder: nvenc` on a host with no NVIDIA device must stop
	// the run here, exactly as a top-level nvenc does. Distinct, because the check runs
	// a real encode and several roots usually share one encoder.
	//
	// The check encodes through the job's own derivation and command line, on the render
	// node this host assigned to the encoder (docs/design/hardware.md#detection), so the
	// devices are found first and stated once.
	devices := discoverDevices(log)
	checks := encoderChecks{cfg: cfg, ffmpeg: ffmpeg, ffprobe: ffprobe, devices: devices, log: log}
	for _, e := range distinctBy(cfg, func(p config.Profile) string { return p.Encoder + "\x00" + p.HWFallbackMode() }) {
		key, fallback, _ := strings.Cut(e.key, "\x00")
		if err := checks.require(context.Background(), key, fallback, e.where); err != nil {
			fmt.Fprintf(stderr, "holdfast: %s: %v\n", e.where, err)
			return nil, nil, 1
		}
	}
	// And every encoder an ENCODE PROFILE can override a root's with, for the same
	// reason: `encoder: svtav1` inside one is reached by every file its pattern selects,
	// so a preflight blind to it would deliver the fail-early guarantee for some of an
	// operator's library and not for the rest. The account names the profile that asked,
	// because "nvenc is unavailable" sends an operator to a configuration whose top-level
	// encoder is cpu. An encode profile carries no hw_fallback: the root a file lives under
	// decides it, so the profile's hardware may be missing only where EVERY root falls back
	// to software.
	profileFallback := config.HWFallbackSoftware
	for _, r := range cfg.RootProfiles() {
		if r.Profile.HWFallbackMode() != config.HWFallbackSoftware {
			profileFallback = config.HWFallbackSkip
		}
	}
	for _, e := range cfg.EncodeProfileEncoders() {
		where := "encode_profiles (" + e.Profile + ")"
		if err := checks.require(context.Background(), e.Key, profileFallback, where); err != nil {
			fmt.Fprintf(stderr, "holdfast: %s: %v\n", where, err)
			return nil, nil, 1
		}
	}
	// And every encoder a resolution RULE names (S0165): a rule's encoder is reached by every
	// file its band admits, so it is checked here, before the job store opens, exactly as the
	// two walks above are. The account names the root and the rule index, because the root's
	// own encoder and every encode profile's may well be available.
	//
	// A rule's encoder runs under its root's hw_fallback. The walk below names each encoder
	// once, at the first rule asking for it, so it is held to software only where EVERY root
	// whose rules name it falls back to software; one root that skips keeps the start-time
	// refusal for all of them.
	ruleFallback := map[string]string{}
	for _, r := range cfg.RootProfiles() {
		for _, rule := range r.Profile.Rules {
			if rule.Encoder == nil {
				continue
			}
			if f, seen := ruleFallback[*rule.Encoder]; !seen || f == config.HWFallbackSoftware {
				ruleFallback[*rule.Encoder] = r.Profile.HWFallbackMode()
			}
		}
	}
	for _, e := range cfg.RuleEncoders() {
		where := fmt.Sprintf("library root %s: rules[%d]", e.Root, e.Rule)
		if err := checks.require(context.Background(), e.Key, ruleFallback[e.Key], where); err != nil {
			fmt.Fprintf(stderr, "holdfast: %s: %v\n", where, err)
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

	// The gate's CHAIN, in the same band and for the same reason as the model above. The
	// model preflight proves libvmaf loads what it was asked to load; it says nothing
	// about the rest of the graph the gate composes around it - the per-side conversion
	// to the named comparison format, the scaler that carries that conversion out, and
	// the deinterlace a configured root adds to it. A build missing one of those cannot
	// assemble the chain for any file that needs it,
	// and the two things it could do instead are both forbidden: compare in whatever
	// format libavfilter negotiates, which is a measurement nobody named or recorded, or
	// score nothing at all. So the run stops here, before the first encode.
	if extras, gated := gateChainFilters(cfg); gated {
		if err := vmaf.RequireChain(context.Background(), ffmpeg, extras...); err != nil {
			fmt.Fprintf(stderr, "holdfast: %v\n", err)
			return nil, nil, 1
		}
	}

	// The parallelism every libx265 encode of this run is told to use, derived ONCE, here,
	// and stated once: the x265_cpus key where it names a figure, otherwise the CPU quota
	// of this process's own cgroup, otherwise nothing and libx265's own defaults. It is
	// derived here rather than in engine.New because `plan` and `analyze` build an engine
	// too, and neither encodes a file for a parallelism line to be about.
	x265 := engine.DeriveX265(cfg.X265CPUs, envOr(engine.CgroupRootEnv, ""))
	x265.Announce(log)
	// The resident-memory bound every encode of this run is held to, from the same cgroup
	// mount, derived once and stated once: the limit and the threshold where one is set, and
	// why none is otherwise, in which case every encode runs unwatched.
	memory := engine.DeriveMemoryWatch(envOr(engine.CgroupRootEnv, ""))
	memory.Announce(log)
	// How many workers this run has and, under `auto`, what they were sized from: resolved
	// once when the configuration loaded, stated once here, where `run` and `serve` both
	// come through - with the quota record first where one is owed.
	cfg.WorkerPlan().Announce(log)

	prober := probe.New(ffmpeg, ffprobe)
	enc := engine.FFmpegEncoder{FFmpeg: ffmpeg, Cfg: *cfg, Probe: prober, X265: x265.Parallelism,
		Memory: memory.Bound, Devices: devices}
	// Belt: an explicit empty state_dir must not silently write the job DB into the
	// process CWD (Load defaults it to "state"; this covers `state_dir: ""`). The
	// defaulting lives in ONE function so `export` reads the database `run` wrote.
	st, err := store.Open(filepath.Join(effectiveStateDir(cfg), "jobs.db"))
	if err != nil {
		// A step rolled back for moving rows it never declared is recorded before the
		// message: the daemon is not starting, and the counts are the finding.
		reportMigrationRefusal(log, err)
		fmt.Fprintf(stderr, "holdfast: opening job store: %v\n", err)
		return nil, nil, 1
	}
	// What this open DID to the ledger's shape, step by step, with every table's row count
	// on either side of each step. It is emitted before anything reads the ledger, because
	// it is the one moment the shape under that evidence moves.
	reportMigrations(log, st.MigrationReport())
	// What the ledger was decided under, BEFORE anything re-opens: a scan offers every
	// row whose recorded decision inputs have moved - and every row that records none -
	// back to the guards, and an operator meeting a burst of activity they did not ask
	// for is owed the reason in front of it rather than in a log line per file.
	//
	// A failure here is reported and survived. It is an announcement about work the scan
	// is about to do; the scan does that work whether or not it could be counted first,
	// and refusing to start over an unreadable count would turn a reporting nicety into
	// an outage.
	survey, surveyErr := decisionInputsReport(context.Background(), st, cfg)
	logLedgerAgainstConfig(log, filepath.Join(effectiveStateDir(cfg), "jobs.db"), survey, surveyErr)

	eng := engine.New(*cfg, prober, enc, st, log)
	eng.Devices = devices
	eng.Hardware = checks.hardware
	// The two dynamic-HDR tools, named as ffmpeg is: an environment override, else the
	// binary's own name on PATH. Neither is required to start: a job that needs a missing one
	// skips under dynamic-hdr-tool-missing (docs/design/dynamic-hdr.md#tools).
	eng.DoviTool = envOr(dynhdr.EnvDoviTool, dynhdr.DefaultDoviTool)
	eng.HDR10PlusTool = envOr(dynhdr.EnvHDR10PlusTool, dynhdr.DefaultHDR10PlusTool)
	// The startup walk's coverage BOUNDS the run: this scan enumerates sources
	// from exactly the directories that walk traversed successfully, so a
	// subtree it declined, could not read or failed to traverse yields no file
	// and no swap can happen under it. Its listings come across with it, so the
	// first scan reads the entries that walk already read rather than paying for
	// the same directories twice more.
	eng.SetCoverage(res.Coverage, res.Entries)
	// Every temp this engine writes beside a source carries a record of the process that
	// owns it, under the same state directory as the job store, classified by the same
	// filesystem-type lookup the startup check used. It is what lets a bounded run remove a
	// temp a killed run left, and what keeps every sweep off a live run's in-flight file.
	eng.TrackTempOwners(filepath.Join(stateDirPath(cfg), engine.TempOwnersDirName), startupPlatform().FSType)
	return eng, st, 0
}

// discoverDevices finds the render nodes this process can see and assigns VAAPI's and QSV's,
// and states the result once: each node with its vendor and whether it opens, and the node
// each encoder got or why it got none. A listing that fails is stated and read as no nodes -
// VAAPI and QSV then have no node, and their probe refuses them if the configuration names
// them; nothing else reads a node.
func discoverDevices(log *slog.Logger) hwdevice.Assignment {
	nodes, err := hwdevice.Discover("/")
	if err != nil {
		log.Warn("hardware: the render nodes could not be listed; VAAPI and QSV get none", "err", err)
	}
	for _, n := range nodes {
		if n.OpenErr != nil {
			log.Info("hardware: render node", "node", n.Path, "vendor", n.Vendor, "usable", false, "why", n.OpenErr.Error())
			continue
		}
		log.Info("hardware: render node", "node", n.Path, "vendor", n.Vendor, "usable", n.Usable())
	}
	devices := hwdevice.Assign(nodes)
	log.Info("hardware: render nodes assigned", "vaapi", orNone(devices.VAAPI, devices.Why["vaapi"]),
		"qsv", orNone(devices.QSV, devices.Why["qsv"]))
	return devices
}

func orNone(node, why string) string {
	if node != "" {
		return node
	}
	return "none (" + why + ")"
}

// requireEncoder is the startup check of one configured encoder: a real encode at 8 and at 10
// bits through the job's own derivation and command line (engine.ProbeEncode), on the render
// node this host assigned. An encoder that needs a node and got none says why beside the
// probe's own reason, because "Device creation failed" does not tell an operator that the
// container was never given /dev/dri, or was given it without the group that may open it.
func requireEncoder(ctx context.Context, cfg *config.Config, ffmpeg, ffprobe, key string, devices hwdevice.Assignment) error {
	_, err := probeEncoder(ctx, cfg, ffmpeg, ffprobe, key, devices)
	return err
}

func probeEncoder(ctx context.Context, cfg *config.Config, ffmpeg, ffprobe, key string, devices hwdevice.Assignment) (encoder.Capability, error) {
	probeWith := engine.ProbeEncode(*cfg, ffmpeg, probe.New(ffmpeg, ffprobe), devices)
	_, c, err := encoder.RequireAvailable(ctx, ffmpeg, ffprobe, key, probeWith)
	if err == nil {
		return c, nil
	}
	if spec, ok := encoder.Lookup(key); ok {
		// The node reasons are keyed by the API that opens the node, so the H.264 and AV1
		// encoders of VAAPI and QSV name the same reason their HEVC sibling does.
		if why := devices.Why[spec.API]; why != "" {
			return c, fmt.Errorf("%w (render node: %s)", err, why)
		}
	}
	return c, err
}

// encoderChecks is the startup check of every encoder the configuration can reach, and what
// it established about the hardware ones: each is probed once (require caches it), and the
// run's engine reads the result (engine.Hardware) to resolve `encoder: auto` and hw_fallback
// per job (docs/design/hardware.md#fallback).
type encoderChecks struct {
	cfg             *config.Config
	ffmpeg, ffprobe string
	devices         hwdevice.Assignment
	log             *slog.Logger

	hardware engine.Hardware
	errs     map[string]error
}

// probe runs key's startup probe once and remembers what it found.
func (c *encoderChecks) probe(ctx context.Context, key string) (encoder.Capability, error) {
	if c.errs == nil {
		c.errs = map[string]error{}
		c.hardware = engine.Hardware{}
	}
	spec, known := encoder.Lookup(key)
	if known {
		key = spec.Key
		if got, done := c.hardware[key]; done {
			return got, c.errs[key]
		}
	}
	got, err := probeEncoder(ctx, c.cfg, c.ffmpeg, c.ffprobe, key, c.devices)
	if known && spec.Hardware {
		c.hardware[key], c.errs[key] = got, err
		c.log.Info("hardware: encoder probed", "encoder", key, "8bit", got.EightBit, "10bit", got.TenBit,
			"why", got.Reason)
	}
	return got, err
}

// require decides whether the run may start with key configured at where under fallback:
//
//   - a software encoder must work, as it always had to;
//   - `amf` in the container image is refused whatever the fallback, with its reason;
//   - a hardware encoder that does not work refuses the start under skip (it would skip every
//     file it was asked to encode, so the run says so now rather than per file), and under
//     software is stated and left to the software encoder of its codec, which must work;
//   - `auto` under skip needs one usable hardware encoder; under software, cpu must work.
func (c *encoderChecks) require(ctx context.Context, key, fallback, where string) error {
	software := fallback == config.HWFallbackSoftware
	if key == encoder.Auto {
		var usable []string
		for _, k := range encoder.AutoOrder {
			if got, _ := c.probe(ctx, k); got.Usable() {
				usable = append(usable, k)
			}
		}
		if len(usable) > 0 {
			c.log.Info("hardware: encoder: auto chooses per job among", "where", where, "usable", strings.Join(usable, ","),
				"hw_fallback", fallback)
			if !software {
				return nil
			}
		}
		if !software {
			return fmt.Errorf("encoder: auto found no usable hardware encoder on this host (%s) and hw_fallback "+
				"is skip, so every file would be skipped; set hw_fallback: software to encode them with cpu "+
				"(docs/design/hardware.md#fallback)", strings.Join(encoder.AutoOrder, ", "))
		}
		_, err := c.probe(ctx, "cpu")
		return err
	}
	got, err := c.probe(ctx, key)
	spec, known := encoder.Lookup(key)
	if err == nil || !known || !spec.Hardware {
		return err
	}
	if encoder.RefusedInImage(spec) != "" {
		return err
	}
	if !software {
		return fmt.Errorf("%w; hw_fallback is skip - set hw_fallback: software to encode this root's files "+
			"with %s instead (docs/design/hardware.md#fallback)", err, encoder.SoftwareFallback(spec).Key)
	}
	c.log.Warn("hardware: encoder unavailable; hw_fallback is software, so its jobs are encoded in software",
		"where", where, "encoder", spec.Key, "fallback", encoder.SoftwareFallback(spec).Key, "why", got.Reason)
	_, err = c.probe(ctx, encoder.SoftwareFallback(spec).Key)
	return err
}

// gateChainFilters names the filters THIS configuration's scoring chains can compose on
// top of the ones every scoring graph needs, and says whether any root gates at all.
//
// The always-needed set belongs to the vmaf package and is not repeated here; it already
// carries `scale`, which performs every format conversion and is also what a root with an
// output-height ceiling scales the distorted output back up with. What is added is what
// the configuration asked for: a root that deinterlaces reproduces that filter on the
// reference before the comparison. It is not checked on a configuration that did not ask
// for it, because refusing a build over a filter no root will ever compose would refuse a
// build this run works on.
//
// gated is false when no root has the perceptual gate enabled. Nothing then asks libvmaf
// for anything, so refusing the run over the chain would be refusing a configuration that
// cannot fail - and logConfigWarnings has already said, loudly, that there is no
// perceptual gate at all.
func gateChainFilters(cfg *config.Config) (extras []string, gated bool) {
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		extras = append(extras, name)
	}
	for _, r := range cfg.RootProfiles() {
		if !r.Profile.VmafGate() {
			continue
		}
		gated = true
		if f, ok := deinterlace.Lookup(r.Profile.Deinterlace); ok {
			add(f.Name)
		}
	}
	return extras, gated
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

// startupDecision is the ONE construction of the start-or-refuse check, and every
// command that takes it comes through here: `run` and `serve` to obey it, `analyze` to
// report it and read the Coverage set it produced. One construction, because a second
// caller assembling its own Check is a second answer waiting to diverge from the
// decision the mutating path takes.
func startupDecision(cfg *config.Config) startup.Result {
	return startupDecisionScoped(cfg, classifyScope{})
}

// startupDecisionScoped is that one construction under a narrowing (AC-9). An empty scope
// is the whole-library decision every command has always taken; a `--file` run may narrow
// the classification to the root it will act under, which leaves the TARGET PATH's own
// decision untouched - its root, the state directory, the scratch directory and the
// declaration test are all weighed exactly as before - and declines to traverse roots this
// run provably cannot reach a file under. The roots it left out come back in the Result so
// the caller can name them, because a decision taken over part of the configuration is one
// its reader has to be told the shape of.
func startupDecisionScoped(cfg *config.Config, scope classifyScope) startup.Result {
	only := []string(nil)
	if scope.scoped() {
		only = []string{scope.Root}
	}
	return startup.Run(startup.Check{
		Roots:        cfg.LibraryRoots,
		ClassifyOnly: only,
		StateDir:     stateDirPath(cfg),
		Declarations: cfg.AllowNonLocal,
		IsMediaFile:  func(base string) bool { return engine.IsSourceName(base, cfg.VideoExts) },
		// The path filters' exclude half, per root and from the path alone: a directory
		// one reaches is inspected and never listed, so every scan pass, the orphaned-temp
		// sweep and the watch - all bounded by this walk's coverage - never list it either
		// (S0168). It is the root the scan assigns a path to whose patterns decide.
		Excluded: cfg.DirectoryExcluded(),
		// The configured working location, checked in the same decision and before
		// anything is encoded: a scratch directory that is missing, is not a
		// directory, is unwritable, is short of the floor or overlaps a library
		// root refuses the run here rather than failing every file mid-encode.
		ScratchDir:       cfg.ScratchDir,
		ScratchMinFreeGB: cfg.ScratchMinFreeGB,
		Platform:         startupPlatform(),
	})
}

// startupCheck runs the whole-run start-or-refuse decision and reports it. On a
// refusal it writes the operator-facing account to stderr - every cause it
// established, each with the exact declaration that would permit it or, where no
// declaration could, the remedy - and returns a nonzero exit code.
func startupCheck(cfg *config.Config, log *slog.Logger, stderr io.Writer, scope classifyScope) (startup.Result, int) {
	res := startupDecisionScoped(cfg, scope)
	res.Log(log)
	// A narrowed classification says so, names the target it was narrowed for and names
	// every configured root it did not classify (AC-9). It is `info`: nothing here asks a
	// human to act, and it is the operator's own `--file` that asked for it - but it is
	// never silent, because the narrowing is the one thing a bounded run does that an
	// unbounded run's refusal would have caught.
	if scope.scoped() {
		log.Info("the start-or-refuse classification was scoped to this run's target file: the library "+
			"roots this run cannot reach a file under were not classified, so a condition under one of "+
			"them did not refuse this run",
			"classification", "scoped-to-target",
			"file", scope.File,
			"classified_library_root", scope.Root,
			"library_roots_not_classified", res.Unclassified)
	}
	if !res.Start {
		res.WriteRefusal(stderr)
		return res, exitError
	}
	return res, exitOK
}

// resolveSecrets turns every configured secret reference into a value, ONCE, at start
// (secrets K1, K5). It is the single resolution site: `run` and `serve` both come through
// here before any scan, encode or swap work, so a reference the resolver cannot produce is
// a startup refusal naming the key and the reference rather than a failure hours in.
//
// The refusal is written to stderr as the resolver's own error, which by construction
// carries the key, the reference and the resolver's exit status, and never a candidate
// value or a byte the resolver printed.
func resolveSecrets(ctx context.Context, cfg *config.Config, stderr io.Writer) (*secret.Set, int) {
	refs, err := cfg.SecretRefs()
	if err != nil {
		// Unreachable through loadConfig (Validate already refused a literal), and kept
		// because a future caller that skips Validate must not silently resolve nothing.
		fmt.Fprintf(stderr, "holdfast: invalid config: %v\n", err)
		return nil, 1
	}
	set, err := secret.Resolve(ctx, refs)
	if err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing to start: %v\n", err)
		return nil, 1
	}
	return set, 0
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	// The help text is answered before anything is parsed, so it reaches STDOUT whole -
	// every flag, a runnable example per shape and the exit-code table - rather than the
	// flag package's own defaults on stderr (cli L2, L5).
	if wantsHelp(args) {
		fmt.Fprint(stdout, runUsage())
		return exitOK
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(stderr, runUsage()) }
	bounds := addBoundFlags(fs)
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	log := logging.New(cfg.LogLevel)

	// The bound, and the refusals it adds, BEFORE anything is built: a `--file` path this
	// configuration would never act on refuses here, with nothing probed, encoded, mutated
	// or created under the state directory (AC-2, AC-3, AC-6, AC-7).
	bound, scope, code := resolveBound(cfg, bounds, stderr)
	if code != exitOK {
		return code
	}
	// `--queue-order`, applied to this run's in-memory configuration only, and only AFTER
	// loadConfig has refused an invalid configured queue_order - see run_queue_order.go.
	queueOrderSource, code := resolveQueueOrder(cfg, bounds, stderr)
	if code != exitOK {
		return code
	}

	// Every configured reference is proved resolvable BEFORE the library is walked or a
	// single frame is encoded, even though a oneshot run consumes none of the three
	// itself: a configuration error an operator discovers after a four-hour pass is a
	// configuration error that was reported too late (secrets K5). The only values `run`
	// hands on are the media-server credentials, each to its one client (attachMediaClients).
	secrets, code := resolveSecrets(context.Background(), cfg, stderr)
	if code != 0 {
		return code
	}

	log.Info("holdfast starting",
		"version", version.Version,
		"library_roots", cfg.LibraryRoots,
		"encoder", cfg.Encoder, "crf", cfg.CRF, "preset", cfg.Preset,
		"dry_run", cfg.DryRun,
		"queue_order", cfg.EffectiveQueueOrder(),
		"queue_order_source", queueOrderSource,
	)
	logResolvedProfiles(cfg, log)
	logConfigWarnings(cfg, log)

	eng, st, code := buildEngine(cfg, log, stderr, scope)
	if code != exitOK {
		return code
	}
	defer st.Close()

	// SIGINT/SIGTERM cancels the context: the in-flight ffmpeg is killed, the temp
	// discarded, and the source left untouched (the swap is the only mutation and
	// only runs after a full verify, which an interrupted encode never reaches).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// How far each encode in flight has got, on the stderr this command was handed, for as
	// long as the pass runs (S0173). It starts after the signal context exists so that an
	// interrupt silences it at once.
	stopProgress := startRunProgress(ctx, eng, stderr)
	// The media-server clients, beside the reporter and only where one is configured: the
	// post-swap rescan observes what the pass commits, and the Plex play hold stands at the
	// door of each job and in front of each swap. The drain runs once the pass is over,
	// interrupted or not, for at most its bound, and never changes the exit code.
	drainMediaClients := attachMediaClients(eng, cfg, secrets, log)
	defer drainMediaClients()

	// One call for both shapes: an unbounded Bound is the whole-library pass this command
	// has always run, so there is no second route into the engine to keep in step.
	err := eng.RunBounded(ctx, bound)
	// The reporter has stopped, and finished writing, before anything else is said: no progress
	// line follows the interrupt notice, the error or the command's return.
	stopProgress()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Warn("interrupted — stopped safely; in-flight temp discarded, source untouched")
			return exitOK
		}
		fmt.Fprintf(stderr, "holdfast: %v\n", err)
		return exitError
	}
	log.Info("scan complete")
	return exitOK
}

// cmdServe runs the HTTP API (TRANSCODE-7). It builds the same
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
	logPathFilters(cfg, log)
}

// logPathFilters states, at daemon startup, which paths each root's filters keep this
// run away from, and reports every configured pattern that can match nothing.
//
// The report is a WARN and not an error: the run continues, and there is no human action
// this daemon is waiting on. It is not an info line either - a pattern that covers
// nothing means the operator's configuration is not doing what its author believes, and
// on a tool that deletes sources that is a degraded state, which is what warn means here.
// It names the pattern, the key and the roots it was weighed against, because none of the
// three is recoverable from the others.
func logPathFilters(cfg *config.Config, log *slog.Logger) {
	for _, r := range cfg.RootProfiles() {
		if !r.Filters.InForce() {
			continue
		}
		log.Info("library root path filters in force: only the files these allow are offered to the pipeline",
			"library_root", r.Clean,
			"exclude_paths", r.Filters.Exclude,
			"exclude_paths_from", string(r.Filters.LayerOf("exclude_paths")),
			"include_paths", r.Filters.Include,
			"include_paths_from", string(r.Filters.LayerOf("include_paths")))
	}
	for _, u := range cfg.UnreachablePatterns() {
		log.Warn("a configured path filter COVERS NOTHING: it is an absolute pattern anchored outside "+
			"every library root it applies to, so nothing this run can reach will ever match it. The run "+
			"continues; if this was meant to protect a directory, it is not protecting one",
			"key", u.Key, "pattern", u.Pattern, "library_roots", u.Roots)
	}
}

// runServer is the serve core, parameterized on ctx so it is testable without
// signals: it serves until ctx is cancelled, then drains gracefully. cmdServe wraps
// it with a signal-bound context.
func runServer(ctx context.Context, cfg *config.Config, log *slog.Logger, stderr io.Writer) int {
	// THE source-URL refusal site (LICENSE-3), and it is deliberately the first
	// statement in the function. Every listener this program can create is created
	// below, and the one handler that serves the root path - the plain-text API page -
	// is behind it, so a build whose Corresponding Source URL is unusable serves no
	// page at all.
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
	// The resolved offer is not held here: the root handler resolves it again at
	// construction, through this same accept test, so there is one accept test and no
	// value passed along that a second site could disagree about.
	if _, err := sourceoffer.Resolve(); err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing to serve: %v\n", err)
		return 1
	}

	// THE SECRETS, resolved ONCE, here, before the store is opened and before any scan,
	// encode or swap work (secrets K1, K5). Each resolved value goes to exactly one
	// consumer below and nowhere else: not into cfg, not into a log line, not into the
	// environment any ffmpeg child inherits.
	secrets, code := resolveSecrets(ctx, cfg, stderr)
	if code != 0 {
		return code
	}
	// The intake's credential must not be a server token by another route (the reference
	// check is config.Validate's; this is the resolved half). Refused before anything opens.
	if err := checkWebhookTokenDistinct(secrets); err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing to start: %v\n", err)
		return 1
	}
	// The node credential likewise: a worker holds it, and it may only lease and upload.
	if err := checkNodeTokenDistinct(secrets); err != nil {
		fmt.Fprintf(stderr, "holdfast: refusing to start: %v\n", err)
		return 1
	}

	// The daemon serves the WHOLE library - it scans on an interval and takes submissions
	// for any configured root - so its classification is never narrowed.
	eng, st, code := buildEngine(cfg, log, stderr, classifyScope{})
	if code != 0 {
		return code
	}
	defer st.Close()

	// The REPORTING DOOR: every read that publishes a frame or answers a read endpoint
	// goes through a handle the database itself refuses every write on, opened beside
	// the engine's write handle and never instead of it. The hub receives it, and the
	// server answers its read endpoints from the hub's handle, so the stream and the
	// polled endpoints cannot end up on different doors.
	reads := reportingDoor(filepath.Join(effectiveStateDir(cfg), "jobs.db"), st, log)
	if reads != st {
		defer func() { _ = reads.Close() }()
	}

	ctrl := server.NewController(ctx, eng.RunOneshot, log)
	hub := server.NewHub(reads, ctrl, log)
	ctrl.SetOnChange(hub.Trigger) // a pause/scan-state flip broadcasts to SSE clients

	// Observability + host-fair scheduling (TRANSCODE-8), all optional and additive.
	// The engine's single Observer fans out to every consumer; each consumer is
	// non-blocking, so none can stall an encode.
	observers := []engine.Observer{hub.Observe}

	var metricsHandler http.Handler
	var mx *metrics.Metrics
	if cfg.MetricsEnable {
		mx = metrics.New(st, log)
		observers = append(observers, mx.Observe)
		metricsHandler = mx.Handler()
	}

	notifier := notify.New(cfg.SecretRef("notify_url"), secrets.Get("notify_url"), log)
	if notifier.Enabled() {
		observers = append(observers, notifier.Observe)
		ctrl.SetScanHooks(notifier.ScanStarted, notifier.ScanFinished)
	}
	eng.Observer = fanout(observers) // live job-state → SSE + metrics + notifications
	// The media-server clients, beside those and only where one is configured: the post-swap
	// rescan joins the fan-out, and the Plex play hold stands at the door of each job and in
	// front of each swap. They are drained at shutdown, after the engine has stopped.
	drainMediaClients := attachMediaClients(eng, cfg, secrets, log)

	// Host-fair scheduler: run-window + CPU-load cap + optional Tautulli pause. It
	// only ever DELAYS work. The engine consults it (throttled) between files; Rescan
	// consults it before starting a scan.
	window, _ := schedule.ParseWindow(cfg.RunWindow) // already validated
	tautulli := schedule.NewTautulli(cfg.TautulliURL,
		cfg.SecretRef("tautulli_api_key"), secrets.Get("tautulli_api_key"))
	sched := schedule.New(window, cfg.MaxLoad, tautulli, log)
	ctrl.SetGate(func() (bool, string) { return sched.MayRun(ctx) })
	eng.Paused = func() bool {
		if ctrl.Paused() {
			return true
		}
		ok, _ := sched.MayRunThrottled(ctx)
		return !ok
	}

	// The targeted-scan queue (S0093), beside the controller rather than instead of it.
	// An accepted path enters the SAME pipeline entry point a scan's worker uses, so
	// every guard, the claim, the decision-input re-opening rule and the swap discipline
	// apply to it unchanged; this is a queue and a worker pool, and it decides nothing.
	subs := eng.NewSubmissions(0, 0)

	// The per-root filesystem watch (S0091), beside the scan loop below rather than
	// instead of it: it is an OPT-IN accelerator for discovery and never the mechanism.
	// Events are lossy by construction, so the startup scan and the interval scan run
	// exactly as they do with no watch configured, and a root that asked for a watch and
	// cannot have one says so once, at startup, and is served by that scan alone.
	//
	// It is built AFTER buildEngine on purpose: the watch registers exactly the
	// directories the startup walk traversed successfully, which buildEngine has bound to
	// the engine by here, so a subtree that walk declined is one the watch touches in no
	// way at all.
	watches := eng.NewWatches()

	srv := server.New(ctx, *cfg, secrets.Get("server_auth_token"), secrets.Get("server_read_token"),
		st, ctrl, hub, metricsHandler, log)
	srv.SetSubmissions(subs)
	// The webhook intake's one credential, handed to its one consumer. Unset, both intake
	// endpoints answer 403.
	srv.SetWebhookToken(secrets.Get(config.WebhookTokenKey))
	// The worker nodes' one credential, handed to its one consumer. Unset, the lease
	// endpoints answer 403; set, they answer 503 until a lease hub is wired behind them.
	srv.SetNodeToken(secrets.Get(config.NodeTokenKey))

	// The library health sweep (docs/design/health-sweep.md), OFF unless
	// health_sweep_interval_hours is set. It reads the engine's own enumeration and asks the
	// same pause and scheduler the encode workers do before every decode it starts; it
	// writes only its own ledger rows and never touches a library file.
	var reporters []health.Reporter
	if mx != nil {
		reporters = append(reporters, mx)
	}
	if notifier.Enabled() {
		reporters = append(reporters, notifier)
	}
	sweep := newHealthSweep(cfg, eng, st, ctrl.Paused, sched, reporters, log)
	if sweep != nil {
		srv.SetHealth(sweep)
	}

	// WORKER NODES, only where node_token is set (docs/design/nodes.md#restart). The leases a
	// previous process left live are recovered and taken back - or abandoned - here, before
	// the listener accepts and before the first scan can offer a file to a node; the hub
	// grants nothing until that is done.
	joinNodes := func() {}
	if cfg.NodesEnabled() {
		join, err := startNodes(ctx, cfg, eng, st, srv, log)
		if err != nil {
			fmt.Fprintf(stderr, "holdfast: refusing to start: %v\n", err)
			return 1
		}
		joinNodes = join
	}

	var bg sync.WaitGroup
	bg.Add(5)
	go func() { defer bg.Done(); hub.Run(ctx) }()
	go func() { defer bg.Done(); notifier.Run(ctx) }()
	go func() { defer bg.Done(); subs.Run(ctx) }()                               // drains POST /api/scan
	go func() { defer bg.Done(); watches.Run(ctx) }()                            // opt-in per-root filesystem watch
	go func() { defer bg.Done(); srv.StartScanLoop(ctx, cfg.ScanIntervalSec) }() // initial scan + optional interval
	if sweep != nil {
		bg.Add(1)
		go func() { defer bg.Done(); sweep.Run(ctx) }() // the report-only health sweep
	}

	addr := cfg.EffectiveServerAddr()
	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		listening := []any{
			"addr", addr,
			"control_enabled", !secrets.Get("server_auth_token").Empty(),
			"read_gated", !secrets.Get("server_read_token").Empty(),
			"webhook_enabled", !secrets.Get(config.WebhookTokenKey).Empty(),
			"scan_interval_sec", cfg.ScanIntervalSec,
			"health_sweep_interval_hours", cfg.HealthSweepIntervalHours,
			"queue_order", cfg.EffectiveQueueOrder(),
			"metrics", cfg.MetricsEnable,
			"notify", notifier.Enabled(),
			"run_window", window.String(),
			"max_load", cfg.MaxLoad,
			"tautulli", tautulli != nil,
			"version", version.Version,
		}
		if cfg.NodesEnabled() {
			// Said only where nodes are on, so an existing configuration's record is the
			// one it always was.
			listening = append(listening, "nodes_enabled", true)
		}
		log.Info("serve listening", listening...)
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
	// unwinding - wait for the scan goroutine AND any in-flight targeted submission to
	// finish issuing store calls so the store handle is never closed out from under
	// them. srv.Wait joins both; submissions still queued are dropped unprocessed and
	// unrecorded, because nothing looked at them. bg.Wait then joins the watch, whose
	// descriptors the cancelled ctx has already released and whose own workers finish any
	// file they were inside for the same reason - a settled path still in its queue is
	// dropped, and the next interval scan finds the file exactly as it finds one no event
	// ever named.
	srv.Wait()
	bg.Wait()
	// The lease sweep and every job that took a recovered lease back have ended with ctx.
	joinNodes()
	// Every swap this daemon will commit has been committed and observed: keep attempting
	// the rescan requests still pending for at most the drain bound, then exit regardless.
	drainMediaClients()
	return 0
}

// reportingDoor opens the handle every reporting read goes through: a READ-ONLY door
// onto the ledger, beside the engine's write handle rather than instead of it.
//
// WHY A SECOND HANDLE AT ALL. store.Open sets SetMaxOpenConns(1), and that single
// connection is what actually prevents "database is locked" under concurrent workers -
// there is never a second connection to contend with. What it also means is that every
// reporting read sits in the same queue as the engine's writes: a snapshot rebuild,
// /api/summary and /api/history each went in front of the next Claim/Advance/Finish.
// Open's DSN already sets journal_mode(WAL), so a second READER is a concurrency WAL
// permits rather than a new lock risk, and the write door's single connection is
// untouched by this - which is the property the no-loss contract rests on.
//
// WHAT IT BUYS, AND WHAT IT DOES NOT. Freedom from the WRITE connection, and not
// concurrency among the reads: store's own read door caps itself at one connection too,
// and it is left that way on purpose. That cap is shared with `export` and `validate`,
// nothing here measured a second read connection as worth taking, and the reads this
// serializes are reads of each other rather than of an encode's next transition.
//
// A DOOR THAT WILL NOT OPEN IS NOT FATAL. Reporting is not what this daemon is for. It
// degrades to the write handle, records which door it tried and what it is doing
// instead, and serves - a ledger one schema version behind, an unreadable state
// directory or a file that has just been replaced must cost an operator their figures
// at worst, never their encodes.
func reportingDoor(dbPath string, write store.Store, log *slog.Logger) store.Store {
	reads, err := store.OpenReadOnly(dbPath)
	if err != nil {
		log.Warn("the ledger's read-only door could not be opened: reporting reads degrade to the write handle",
			"dependency", "the job ledger's read-only door",
			"ledger", dbPath,
			"attempted", "store.OpenReadOnly (mode=ro with query_only, at this build's schema version)",
			"next", "serving every reporting read through the engine's write handle, where they queue with its writes",
			"err", err)
		return write
	}
	return reads
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

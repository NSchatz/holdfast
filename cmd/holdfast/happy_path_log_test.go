package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// THE HAPPY PATH THIS FILE GRADES, NAMED RATHER THAN IMPLIED.
//
// HP-RUN is ONE invocation of `holdfast run` - the oneshot scan - in a REAL child
// process, against a configuration whose single library root holds exactly one
// synthetic H.264 source built here by the pinned ffmpeg at a bitrate above the
// shipped min_bitrate_kbps floor. Every path the configuration names (the config
// file, the library root, the state directory) is created by this test, and the
// config path is passed to the command explicitly with --config. The child's
// environment is built here rather than inherited: holdfast layers HOLDFAST_* over
// the config file, so an ambient HOLDFAST_LIBRARY_ROOTS or HOLDFAST_STATE_DIR would
// otherwise aim all of that at a directory this test never made.
//
// The stages HP-RUN traverses, in order: config load and validate; secret
// resolution; the startup filesystem classification; the ffmpeg/ffprobe lookup;
// the encoder capability preflight; the VMAF model preflight; the job store open;
// the library enumeration; the skip guards; the real encode; the verify gates
// (codec, duration and packet parity, strictly-smaller, stream-count parity,
// decode integrity and VMAF); the atomic swap; the deletion of the source; and the
// terminal ledger row.
//
// It runs on the SHIPPED DEFAULTS. The configuration written here sets the library
// root and the state directory and nothing else, so log_level is info,
// vmaf_enable is true, min_bitrate_kbps is 2500 and undo_window_hours is 0. No
// gate the defaults enable is switched off: the fixture is built to CLEAR the
// bitrate floor rather than the floor lowered to admit the fixture, because a run
// that switched a gate off would measure a path no operator takes.
//
// THE CRITERION THIS FILE GRADES IS AC-2 (testing T1): the capture of HP-RUN
// carries zero records at `error` level. AC-1 (REACHED-TRANSCODE), AC-11 (every
// configured path created here), AC-12 (the per-level tally) and the failure
// branches AC-4 through AC-8 are the machinery that makes AC-2 mean something,
// and each of those branches is defeated on purpose by
// `make happy-path-log-selftest`.
//
// This is destructive by design and safe by construction: HP-RUN performs a real
// encode, a real atomic swap and the REAL DELETION of the file it transcoded,
// because a grader that stopped before the swap would never reach the code most
// able to misreport. Everything it destroys is fixture media this test
// synthesised under t.TempDir() for this run.

// The three failure vocabularies, fixed in TEXT because `go test` spends one exit
// status on all of them: a reader has to be able to tell "the grader could not
// run" from "it ran and found error records" from "it ran and the process emitted
// nothing" by reading the message.
const (
	// hpCouldNotRun is the could-not-run vocabulary (AC-6 for an absent tool or
	// capability, AC-8 for a capture that could not be created, read back or closed).
	hpCouldNotRun = "HAPPY-PATH LOG GRADER COULD NOT RUN"
	// hpEmittedNothing is AC-4: the run was quiet, which is never a pass.
	hpEmittedNothing = "HP-RUN RAN AND THE PROCESS EMITTED NOTHING"
	// hpUnreadableLine is AC-5: a captured line that is not a levelled record.
	hpUnreadableLine = "HP-RUN RAN AND EMITTED A LINE THAT IS NOT A LEVELLED RECORD"
	// hpNotReached is AC-7: the happy path was not reached, so AC-2 decides nothing.
	hpNotReached = "HP-RUN RAN AND DID NOT REACH THE HAPPY PATH"
	// hpFoundErrors is AC-2: it ran, it arrived, and it logged at error.
	hpFoundErrors = "HP-RUN RAN AND EMITTED ERROR RECORD(S)"
)

// hpEnvPrefix is internal/config's envPrefix: the layer that overrides the file.
// Load builds its configuration from defaults, then the YAML file, then HOLDFAST_*,
// and `library_roots` and `state_dir` are both keys it accepts there - so a variable
// exported by the shell that started the gate can replace the paths this grader
// created, and HP-RUN's encode, atomic swap and deletion are all real.
const hpEnvPrefix = "HOLDFAST_"

// hpChildEnvNames are the only HOLDFAST_* variables permitted to reach HP-RUN: the
// marker that routes the re-executed test binary into the CLI, and the two tool paths
// HP-RUN looks up. hpChildEnv sets exactly these and hpRequireConfiguredPaths refuses
// to spawn the child if its environment carries any other.
var hpChildEnvNames = []string{subprocessEnv, "HOLDFAST_FFMPEG", "HOLDFAST_FFPROBE"}

// hpRecord is one parsed line of the capture.
type hpRecord struct {
	raw   string
	level string // as written by log/slog: DEBUG, INFO, WARN, ERROR (or an offset form)
	msg   string
}

// isError reports whether this record is an ERROR RECORD: level `error` in any case.
func (r hpRecord) isError() bool { return strings.EqualFold(r.level, "ERROR") }

// TestHappyPathRunEmitsNoErrorRecords grades AC-2 (observability O3): the whole log
// stream of one completed HP-RUN carries no record at `error` level.
//
// The other criteria it carries: AC-1 (REACHED-TRANSCODE by all three observables,
// naming the one that did not hold), AC-11 (every configured path created here,
// --config passed explicitly), AC-12 (the per-level tally, and no failure on warn,
// info or debug), and the failure branches AC-4 through AC-8.
func TestHappyPathRunEmitsNoErrorRecords(t *testing.T) {
	// AC-6. Every tool and capability HP-RUN needs is required HERE, loudly, before
	// anything else: a t.Skip exits zero, and a grader that skipped for want of
	// ffmpeg would be a false green on the exact gate this test adds.
	ffmpegBin, ffprobeBin := hpRequireBinaries(t)

	// AC-11. Those two tool paths are the only HOLDFAST_* variables this grader takes
	// from the environment it was started in, and it re-sets them on the child
	// explicitly below. Every other one is removed here, before a fixture exists and
	// long before the child does, so that nothing an operator exported can be layered
	// over the configuration written below.
	removed := hpIsolateFromAmbientConfig(t)

	dir := t.TempDir()
	cfgPath, libRoot, stateDir, src := hpFixture(t, dir, ffmpegBin)
	wrote := hpSize(t, src)
	hpRequireCapabilities(t, cfgPath, ffmpegBin, ffprobeBin)

	// AC-11. The child's environment is built here and PROVED before the child exists:
	// the three variables this grader sets are the only HOLDFAST_* in it, the three
	// paths the configuration names were all created above under dir, and the
	// configuration resolves to those paths and no others. The proof is taken here
	// rather than after the run because the swap and the deletion are real: a red that
	// arrives once the child has finished undoes neither.
	childEnv := hpChildEnv(ffmpegBin, ffprobeBin)
	hpRequireConfiguredPaths(t, dir, cfgPath, libRoot, stateDir, childEnv, removed)

	// AC-8. The capture is everything the PROCESS writes to its own standard error
	// for the duration of HP-RUN, taken from the process's stream rather than from a
	// buffer handed to an internal entry point: holdfast's logger is built over
	// os.Stderr, so a capture of anything else would prove the absence of an error
	// line somewhere no operator looks.
	capturePath := filepath.Join(dir, "hp-run-stderr.log")
	capture, err := os.Create(capturePath)
	if err != nil {
		t.Fatalf("%s: could not CREATE the capture file at %s: %v", hpCouldNotRun, capturePath, err)
	}

	// AC-11. The command gets the config path explicitly - `run` refuses without
	// --config and has no default config location - and that file, its single
	// library_roots entry and its state_dir are the three paths created above. The
	// environment is the slice hpRequireConfiguredPaths just proved clean, so what was
	// checked is what the child runs under.
	cmd := exec.Command(os.Args[0], "run", "--config", cfgPath)
	cmd.Env = childEnv
	cmd.Stdout = io.Discard
	cmd.Stderr = capture
	runErr := cmd.Run()

	if err := capture.Close(); err != nil {
		t.Fatalf("%s: could not CLOSE the capture file at %s: %v", hpCouldNotRun, capturePath, err)
	}
	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("%s: could not READ BACK the capture file at %s: %v", hpCouldNotRun, capturePath, err)
	}

	// AC-7, first half. A nonzero exit means the run did not get there, and the
	// error-record assertion below must not be reported as satisfied.
	if code, ok := hpExitCode(runErr); !ok {
		t.Fatalf("%s: the child process could not be run at all: %v\ncapture:\n%s",
			hpCouldNotRun, runErr, raw)
	} else if code != 0 {
		t.Fatalf("%s: `holdfast run --config %s` exited %d. AC-2 is UNDECIDED, not satisfied.\ncapture:\n%s",
			hpNotReached, cfgPath, code, raw)
	}

	// AC-5. Every captured line is read as a levelled record or the grader fails
	// quoting it. Skipping it, or counting it as not-an-error, is how this grader
	// would go quietly vacuous the day holdfast moves to JSON.
	records, bad := hpParse(string(raw))
	if len(bad) > 0 {
		t.Fatalf("%s: %d of %d captured line(s) could not be read as `time=... level=... msg=...`. "+
			"First offender, verbatim:\n\t%s\nAll offenders:\n%s",
			hpUnreadableLine, len(bad), len(bad)+len(records), bad[0], hpQuoteAll(bad))
	}

	// AC-12. The per-level tally, reported on every run: a pass that printed no tally
	// cannot be told from a pass that captured nothing. `error` is printed even when
	// it is zero, because zero is the answer this grader exists to establish, and
	// `warn`, `info` and `debug` are counted and NEVER failed on - the undo-window
	// notice below is logged at `warn` by design on a default configuration.
	// It goes to the process's stdout rather than through t.Log so that the defeat
	// runner, which grades this line, reads the grader's own output rather than the
	// testing package's; `go test` surfaces either on -v or on a failure, and
	// `make happy-path-log-selftest` passes -v.
	fmt.Fprintf(os.Stdout, "HP-RUN per-level tally: %s (%d record(s) captured from %s)\n",
		hpTally(records), len(records), capturePath)

	// AC-4. A run that was quiet is never a pass.
	if len(records) == 0 {
		t.Fatalf("%s: the capture at %s holds no record at ANY level, so there was nothing to "+
			"assert over. This is a FAILURE, not a clean run.", hpEmittedNothing, capturePath)
	}

	// AC-1 and AC-7, second half. REACHED-TRANSCODE is all three observables, and a
	// run that exited early satisfies none of them.
	if missed := hpReachedTranscode(t, ffmpegBin, ffprobeBin, src, stateDir, wrote); len(missed) > 0 {
		t.Fatalf("%s: REACHED-TRANSCODE does not hold. The observable(s) that did not hold:\n%s\n"+
			"AC-2 is UNDECIDED, not satisfied.\nlibrary root: %s\ncapture:\n%s",
			hpNotReached, strings.Join(missed, "\n"), libRoot, raw)
	}

	// AC-2. The assertion this file exists for.
	var offenders []hpRecord
	for _, r := range records {
		if r.isError() {
			offenders = append(offenders, r)
		}
	}
	if len(offenders) > 0 {
		var b strings.Builder
		for _, r := range offenders {
			fmt.Fprintf(&b, "\tlevel=%s msg=%q\n\t\tverbatim: %s\n", r.level, r.msg, r.raw)
		}
		t.Fatalf("%s: a completed happy-path run logged %d record(s) at `error`. "+
			"`error` means a human must act, and a process that logs it for a condition it "+
			"handled itself trains its reader to ignore it (observability O3). Relevel each of "+
			"these, or make the condition one a human really must act on:\n%s",
			hpFoundErrors, len(offenders), b.String())
	}
}

// ---- HP-RUN's preconditions -------------------------------------------------

// hpRequireBinaries is AC-6's first half: the two executables HP-RUN looks up,
// required rather than probed-and-skipped.
func hpRequireBinaries(t *testing.T) (ffmpegBin, ffprobeBin string) {
	t.Helper()
	ffmpegBin, ffprobeBin = envOr("HOLDFAST_FFMPEG", "ffmpeg"), envOr("HOLDFAST_FFPROBE", "ffprobe")
	for _, bin := range []string{ffmpegBin, ffprobeBin} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s: %q is not on PATH, and HP-RUN needs it. Install the pinned ffmpeg "+
				"(scripts/install-ffmpeg.sh) or set HOLDFAST_FFMPEG/HOLDFAST_FFPROBE. This grader "+
				"NEVER skips: a skipping test exits zero, which is the false green it exists to "+
				"prevent. Lookup error: %v", hpCouldNotRun, bin, err)
		}
	}
	return ffmpegBin, ffprobeBin
}

// hpIsolateFromAmbientConfig removes every HOLDFAST_* variable from THIS process's
// environment, restores each when the test ends, and returns the names it removed.
//
// It is AC-11's first half. holdfast layers HOLDFAST_* over the config file, so a
// variable exported by the shell that ran the gate would replace the library root or
// the state directory this grader creates, and HP-RUN would aim a REAL encode, a REAL
// atomic swap and the REAL deletion of a source at a directory nothing here made. The
// removal also makes the configuration this process reads back - the capability
// preflight and the path check below, both of which call config.Load - the one the
// child will run under rather than a different resolution of the same file.
func hpIsolateFromAmbientConfig(t *testing.T) []string {
	t.Helper()
	var removed []string
	for _, name := range hpAmbientConfigVars() {
		value := os.Getenv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("%s: %s is set in this process's environment and could not be removed: %v. "+
				"holdfast layers HOLDFAST_* over the config file, so HP-RUN cannot be aimed at the "+
				"paths this grader created while it is set.", hpCouldNotRun, name, err)
		}
		t.Cleanup(func() { _ = os.Setenv(name, value) })
		removed = append(removed, name)
	}
	return removed
}

// hpAmbientConfigVars names the HOLDFAST_* variables this process's environment
// currently carries, sorted.
func hpAmbientConfigVars() []string {
	var names []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, hpEnvPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// hpChildEnv is the environment HP-RUN's child process gets: this process's
// environment, which hpIsolateFromAmbientConfig has emptied of every HOLDFAST_*
// variable, plus exactly the three in hpChildEnvNames. It is returned rather than
// assigned so that hpRequireConfiguredPaths can check the very slice the child is
// handed.
func hpChildEnv(ffmpegBin, ffprobeBin string) []string {
	return append(os.Environ(),
		subprocessEnv+"=1",
		"HOLDFAST_FFMPEG="+ffmpegBin,
		"HOLDFAST_FFPROBE="+ffprobeBin,
	)
}

// hpRequireConfiguredPaths is AC-11's second half: HP-RUN is not spawned at all unless
// the configuration it would run under is the one this test wrote. Three facts, all
// established BEFORE the child exists, because a red that arrives after a swap and a
// deletion undoes neither:
//
//  1. the environment the child will run under carries no HOLDFAST_* variable beyond
//     the three this grader sets, so no ambient value can be layered over the file;
//  2. the config file, the library root and the state directory all lie inside the
//     directory created for this run, so each is a path this test made;
//  3. the configuration that file resolves to names that single library root and that
//     state directory, and nothing else.
//
// The configuration is read through internal/config.Load, the call `run` itself makes.
func hpRequireConfiguredPaths(t *testing.T, dir, cfgPath, libRoot, stateDir string, childEnv, removed []string) {
	t.Helper()
	visible := hpAmbientConfigVars()
	refused := "REFUSED BEFORE the child was spawned: nothing was encoded, swapped or deleted."

	allowed := make(map[string]bool, len(hpChildEnvNames))
	for _, name := range hpChildEnvNames {
		allowed[name] = true
	}
	var stray []string
	for _, kv := range childEnv {
		if name, value, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, hpEnvPrefix) && !allowed[name] {
			stray = append(stray, name+"="+value)
		}
	}
	if len(stray) > 0 {
		t.Fatalf("%s: the environment built for HP-RUN's child carries %d HOLDFAST_* variable(s) "+
			"beyond the %s this grader sets: %s. holdfast layers HOLDFAST_* OVER the config file, so "+
			"one of those can replace the library root or the state directory created here and aim a "+
			"REAL encode, a REAL atomic swap and a REAL deletion at a directory this test never made. "+
			"%s", hpCouldNotRun, len(stray), strings.Join(hpChildEnvNames, ", "),
			strings.Join(stray, " "), refused)
	}

	inside := filepath.Clean(dir) + string(filepath.Separator)
	for _, p := range []struct{ what, path string }{
		{"the config file", cfgPath}, {"the library root", libRoot}, {"the state directory", stateDir},
	} {
		if !strings.HasPrefix(filepath.Clean(p.path), inside) {
			t.Fatalf("%s: %s HP-RUN would use is %s, which is not inside %s - the directory created "+
				"for this run. AC-11 requires every path the invocation names to be one this grader "+
				"created, because HP-RUN encodes, swaps and DELETES for real. %s",
				hpCouldNotRun, p.what, p.path, dir, refused)
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("%s: the configuration this test wrote at %s does not load: %v",
			hpCouldNotRun, cfgPath, err)
	}
	roots := make([]string, 0, len(cfg.LibraryRoots))
	for _, r := range cfg.LibraryRoots {
		roots = append(roots, filepath.Clean(r))
	}
	if len(roots) != 1 || roots[0] != filepath.Clean(libRoot) {
		t.Fatalf("%s: the configuration HP-RUN would run under names library_roots %v, not the single "+
			"root this test created at %s. HOLDFAST_* visible to this process: %s. %s",
			hpCouldNotRun, roots, libRoot, hpNamesOrNone(visible), refused)
	}
	if got, want := filepath.Clean(cfg.StateDir), filepath.Clean(stateDir); got != want {
		t.Fatalf("%s: the configuration HP-RUN would run under names state_dir %s, not the directory "+
			"this test created at %s. HOLDFAST_* visible to this process: %s. %s",
			hpCouldNotRun, got, want, hpNamesOrNone(visible), refused)
	}

	// Reported for the same reason the per-level tally is: a run that was isolated and
	// a run that had nothing to isolate look identical otherwise, and the defeat runner
	// grades this line.
	fmt.Fprintf(os.Stdout, "HP-RUN environment: %d ambient HOLDFAST_* variable(s) removed (%s); the "+
		"child gets %s and nothing else; library_roots=%v and state_dir=%s, both created under %s\n",
		len(removed), hpNamesOrNone(removed), strings.Join(hpChildEnvNames, ", "),
		roots, filepath.Clean(cfg.StateDir), dir)
}

// hpNamesOrNone renders a variable-name list, saying so when it is empty rather than
// printing nothing where a reader cannot tell an empty list from a missing one.
func hpNamesOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// hpRequireCapabilities is AC-6's second half: the encoder and the VMAF model HP-RUN
// will preflight, required through the SAME functions buildEngine requires them with
// and read off the SAME configuration file HP-RUN is given, so an absence that would
// refuse HP-RUN refuses the grader here in the could-not-run vocabulary instead of
// surfacing as an unexplained red inside the run.
func hpRequireCapabilities(t *testing.T, cfgPath, ffmpegBin, ffprobeBin string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("%s: the configuration this test wrote at %s does not load: %v",
			hpCouldNotRun, cfgPath, err)
	}
	ctx := context.Background()
	for _, r := range cfg.RootProfiles() {
		if _, err := encoder.RequireAvailable(ctx, ffmpegBin, ffprobeBin, r.Profile.Encoder); err != nil {
			t.Fatalf("%s: the shipped default encoder %q does not work with this ffmpeg, and "+
				"HP-RUN encodes for real with it: %v", hpCouldNotRun, r.Profile.Encoder, err)
		}
		if !r.Profile.VmafGate() {
			t.Fatalf("%s: the VMAF gate is OFF for %s. It is ON by default and HP-RUN is required "+
				"to run on the shipped defaults, so this grader would be measuring a path no "+
				"operator takes.", hpCouldNotRun, r.Clean)
		}
		if err := vmaf.RequireModel(ctx, ffmpegBin, r.Profile.VmafModel); err != nil {
			t.Fatalf("%s: the VMAF gate is ON by default and HP-RUN measures with it, but the "+
				"model %q does not resolve in this ffmpeg build: %v",
				hpCouldNotRun, r.Profile.VmafModel, err)
		}
	}
}

// hpFixture builds the library HP-RUN scans and the configuration that names it.
// Every path it returns is created here, under dir, for this run alone (AC-11).
//
// The source is 8 Mbps, comfortably above the shipped 2500 kbps floor, so the
// bitrate guard offers it to the pipeline without the floor being touched.
func hpFixture(t *testing.T, dir, ffmpegBin string) (cfgPath, libRoot, stateDir, src string) {
	t.Helper()
	libRoot = filepath.Join(dir, "library")
	stateDir = filepath.Join(dir, "state")
	for _, d := range []string{libRoot, stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("%s: could not create %s: %v", hpCouldNotRun, d, err)
		}
	}
	src = filepath.Join(libRoot, "fixture.mkv")
	out, err := exec.Command(ffmpegBin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast",
		// CBR, not a target: libx264 undershoots a plain -b:v on synthetic content
		// by an order of magnitude, and a fixture that lands under the floor is
		// skipped rather than transcoded. nal-hrd=cbr makes the source genuinely
		// carry the bitrate a bloated H.264 library file carries, which is what the
		// guard is there to let through.
		"-b:v", "8M", "-maxrate", "8M", "-bufsize", "8M", "-x264-params", "nal-hrd=cbr",
		"-pix_fmt", "yuv420p", "--", src,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: the pinned ffmpeg could not build the H.264 fixture: %v\n%s",
			hpCouldNotRun, err, out)
	}
	// The whole configuration. Two keys, both naming paths created above; everything
	// else is the shipped default (D1).
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + libRoot + "\nstate_dir: " + stateDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("%s: could not write the config at %s: %v", hpCouldNotRun, cfgPath, err)
	}
	return cfgPath, libRoot, stateDir, src
}

func hpSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: could not measure %s: %v", hpCouldNotRun, path, err)
	}
	return fi.Size()
}

// ---- REACHED-TRANSCODE ------------------------------------------------------

// hpReachedTranscode returns the observables that did NOT hold, so a failure names
// the one that broke rather than restating that something did (AC-1).
//
// All three, and they are observables rather than a restatement of the run: the
// file at the source path decodes as hevc, it is strictly smaller than the bytes
// this test wrote there, and the ledger carries exactly one terminal `done` row for
// it. A run that exited early satisfies none of the three.
func hpReachedTranscode(t *testing.T, ffmpegBin, ffprobeBin, src, stateDir string, wrote int64) []string {
	t.Helper()
	var missed []string

	codec, answered := probe.New(ffmpegBin, ffprobeBin).VideoCodecAnswered(context.Background(), src)
	switch {
	case !answered:
		missed = append(missed, "\t- the file at the source path "+src+
			" could not be probed at all, so it does not decode as hevc")
	case codec != "hevc":
		missed = append(missed, "\t- the file at the source path "+src+" decodes as "+
			strconv.Quote(codec)+", not \"hevc\": no swap put a transcoded replacement there")
	}

	if fi, err := os.Stat(src); err != nil {
		missed = append(missed, "\t- the source path "+src+" cannot be measured: "+err.Error())
	} else if fi.Size() >= wrote {
		missed = append(missed, fmt.Sprintf("\t- the file at the source path is %d byte(s), "+
			"not strictly smaller than the %d byte(s) this test wrote there", fi.Size(), wrote))
	}

	done, err := hpTerminalDoneRows(stateDir, src)
	switch {
	case err != nil:
		missed = append(missed, "\t- the ledger under "+stateDir+" could not be read: "+err.Error())
	case done != 1:
		missed = append(missed, fmt.Sprintf("\t- the ledger under %s carries %d terminal `done` "+
			"row(s) for the source path, want exactly 1", stateDir, done))
	}
	return missed
}

// hpTerminalDoneRows counts the terminal `done` rows the ledger holds for path.
func hpTerminalDoneRows(stateDir, path string) (int, error) {
	st, err := store.Open(filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.List(context.Background(), []store.Status{store.Done}, 1000)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.Path == path {
			n++
		}
	}
	return n, nil
}

// ---- the capture ------------------------------------------------------------

// hpParse reads the capture as log/slog TEXT records - `time=... level=... msg=...`
// key-value lines, which is what internal/logging builds over the process's stderr.
// It returns the records it read and, separately, every line it could NOT read, so
// the caller can fail on the second set rather than silently treating an unreadable
// line as not-an-error (AC-5).
func hpParse(capture string) (records []hpRecord, unreadable []string) {
	for _, line := range strings.Split(capture, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		rec, ok := hpParseLine(line)
		if !ok {
			unreadable = append(unreadable, line)
			continue
		}
		records = append(records, rec)
	}
	return records, unreadable
}

// hpParseLine reads one slog TEXT record. The handler writes time, level and msg
// first and in that order, so the shape is checked rather than the line searched:
// a search for "level=" anywhere would read a level out of a message that quoted
// one, which is the fragile matcher AC-5 exists to keep honest.
func hpParseLine(line string) (hpRecord, bool) {
	rest, ok := strings.CutPrefix(line, "time=")
	if !ok {
		return hpRecord{}, false
	}
	if _, rest, ok = strings.Cut(rest, " "); !ok {
		return hpRecord{}, false
	}
	rest, ok = strings.CutPrefix(rest, "level=")
	if !ok {
		return hpRecord{}, false
	}
	level, rest, ok := strings.Cut(rest, " ")
	if !ok || level == "" {
		return hpRecord{}, false
	}
	rest, ok = strings.CutPrefix(rest, "msg=")
	if !ok {
		return hpRecord{}, false
	}
	msg := rest
	if strings.HasPrefix(rest, `"`) {
		quoted, _ := strconv.QuotedPrefix(rest)
		if quoted == "" {
			return hpRecord{}, false
		}
		unquoted, err := strconv.Unquote(quoted)
		if err != nil {
			return hpRecord{}, false
		}
		msg = unquoted
	} else if head, _, cut := strings.Cut(rest, " "); cut {
		msg = head
	}
	return hpRecord{raw: line, level: level, msg: msg}, true
}

// hpTally renders the per-level count of what was captured (AC-12). Every level is
// reported, `error` included and reported as zero when it is zero, because the
// count is the observable that tells a real pass from a run that captured nothing.
func hpTally(records []hpRecord) string {
	counts := map[string]int{}
	for _, r := range records {
		counts[strings.ToUpper(r.level)]++
	}
	var levels []string
	for l := range counts {
		levels = append(levels, l)
	}
	sort.Strings(levels)
	// The four levels this fleet defines, always printed, then anything else the
	// handler emitted (a level offset, say) after them.
	var parts []string
	for _, l := range []string{"DEBUG", "INFO", "WARN", "ERROR"} {
		parts = append(parts, fmt.Sprintf("%s=%d", strings.ToLower(l), counts[l]))
		delete(counts, l)
	}
	for _, l := range levels {
		if n, still := counts[l]; still {
			parts = append(parts, fmt.Sprintf("%s=%d", strings.ToLower(l), n))
		}
	}
	return strings.Join(parts, " ")
}

func hpQuoteAll(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "\t%s\n", l)
	}
	return b.String()
}

// hpExitCode reports the child's exit status. ok is false when the process could
// not be run at all, which is a could-not-run rather than a nonzero answer.
func hpExitCode(err error) (code int, ok bool) {
	if err == nil {
		return 0, true
	}
	var ee *exec.ExitError
	if !errorsAs(err, &ee) {
		return 0, false
	}
	return ee.ExitCode(), true
}

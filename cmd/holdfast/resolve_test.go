package main

// `holdfast resolve` - the operator's way out of the parked state.
//
// A parked job holds two files out of the library until a human decides what happened.
// That is the right fail-safe and it would be an unforgivable one to ship without an
// exit, so these tests are about the exit working under the conditions an operator
// actually meets: a replacement somebody already deleted by hand, a path the process
// cannot inspect, an id that names nothing, a job store that has gone read-only, and a
// licensed removal that does not succeed.
//
// Nothing here is mocked below the store: the store is the real SQLite one, the files are
// real files, and every assertion about a file is made by looking at the file.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// parkedFixture is a real store under a real state dir, holding one parked job whose two
// files exist on disk.
type parkedFixture struct {
	dir      string
	cfg      string
	source   string
	replaced string
	st       *store.SQLite
}

func newParkedFixture(t *testing.T) *parkedFixture {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(lib, "movie.mkv")
	replaced := filepath.Join(lib, "movie.__holdfast-replacement__.mkv")
	if err := os.WriteFile(source, []byte("the source holdfast started from"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replaced, []byte("the replacement that passed every gate"), 0o644); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	st, err := store.Open(filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + stateDir + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &parkedFixture{dir: dir, cfg: cfg, source: source, replaced: replaced, st: st}
}

func (f *parkedFixture) park(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	if ok, err := f.st.Claim(ctx, f.source, "31:1700000000", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := f.st.RecordSwapIncident(ctx, store.SwapIncident{
		SourcePath:        f.source,
		SourceFingerprint: "31:1700000000",
		ReplacementPath:   f.replaced,
		SourceAttrs:       "31:1700000000",
		ReplacementAttrs:  "37:1700000001",
		ObservedAttrs:     "31:1700000000",
		Outcome:           store.Indeterminate,
		SwapError:         "swap failed: simulated - the storage is non-local (nfs)",
		StorageClass:      "non-local",
		StorageType:       "nfs",
	}); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	parked, err := f.st.ParkedIncidents(ctx)
	if err != nil || len(parked) != 1 {
		t.Fatalf("ParkedIncidents: %d (err %v)", len(parked), err)
	}
	// The command opens its own handle on the same database file, so this one must not
	// be holding a write lock when it does. SQLite/WAL allows both; closing would break
	// the later assertions, so nothing more is needed here.
	return parked[0].ID
}

func (f *parkedFixture) incident(t *testing.T, id int64) store.SwapIncident {
	t.Helper()
	in, ok, err := f.st.IncidentByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("IncidentByID(%d): ok=%v err=%v", id, ok, err)
	}
	return in
}

// resolveCmd runs the subcommand exactly as main would, and returns its streams.
func resolveCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cmdResolve(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestResolve_ReportsBothPathsAndWhatIsAtEachRightNow is the report half of the action.
// It is unconditional: it comes before any instruction, names both recorded paths, and
// shows the attributes each file currently has beside the ones recorded before the swap.
func TestResolve_ReportsBothPathsAndWhatIsAtEachRightNow(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)

	code, out, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id))
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, errOut)
	}
	for _, want := range []string{f.source, f.replaced, "31:1700000000", "37:1700000001", "no determination given"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q:\n%s", want, out)
		}
	}
	// A report changes nothing.
	if !f.incident(t, id).Parked() {
		t.Error("reporting resolved the job")
	}
	if !exists(f.source) || !exists(f.replaced) {
		t.Error("reporting removed a file")
	}

	// And the listing names every parked job with both of its files.
	code, list, errOut := resolveCmd(t, "--config", f.cfg)
	if code != 0 {
		t.Fatalf("list code = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(list, f.source) || !strings.Contains(list, f.replaced) {
		t.Errorf("the listing does not name both files:\n%s", list)
	}
}

// TestResolve_AnAbsentRecordedFileIsReportedAndTheJobIsStillResolvable is AC15f's Given:
// the retained replacement was deleted by hand (or moved by a media manager) before the
// operator got to it. Observing a file is never a precondition of leaving the parked
// state - refusing here would trap the job in exactly the state this command exists to
// leave.
func TestResolve_AnAbsentRecordedFileIsReportedAndTheJobIsStillResolvable(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)
	if err := os.Remove(f.replaced); err != nil { // deleted by hand
		t.Fatal(err)
	}

	code, out, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id))
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("the missing file was not reported absent:\n%s", out)
	}
	if !strings.Contains(out, "31:1700000000") {
		t.Errorf("the source path's current attributes were not reported:\n%s", out)
	}

	// ... and the instruction is still accepted.
	code, out, errOut = resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "source-is-intact", "--replacement", "delete")
	if code != 0 {
		t.Fatalf("resolving with an absent file: code = %d, stderr: %s", code, errOut)
	}
	in := f.incident(t, id)
	if in.Parked() {
		t.Fatal("the job is still parked after an explicit determination")
	}
	// "there was no file there to dispose of" is the honest disposition, not "deleted".
	if in.DispositionReplacement != store.Absent {
		t.Errorf("replacement disposition = %q, want %q", in.DispositionReplacement, store.Absent)
	}
	if in.DispositionSource != store.KeptInPlace {
		t.Errorf("source disposition = %q, want %q", in.DispositionSource, store.KeptInPlace)
	}
	if !strings.Contains(in.ObservedAtResolutionReplacement, "ABSENT") {
		t.Errorf("what was observed at the replacement path was not recorded: %q", in.ObservedAtResolutionReplacement)
	}
	if !strings.Contains(out, string(store.Absent)) {
		t.Errorf("the operator was not told the recorded disposition:\n%s", out)
	}
}

// TestResolve_AnUninspectablePathIsNamedAsThatAndNotAsAbsent. "Somebody deleted it" and
// "holdfast cannot look at it" call for opposite responses, so they are never collapsed.
func TestResolve_AnUninspectablePathIsNamedAsThatAndNotAsAbsent(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)
	// Remove the search bit on the containing directory: the file is there, and the
	// process cannot stat it.
	lib := filepath.Dir(f.replaced)
	if err := os.Chmod(lib, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lib, 0o755) })
	if _, err := os.Stat(f.replaced); err == nil {
		t.Skip("this process can stat through a directory with no search bit (running as root?)")
	}

	code, out, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id))
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "UNINSPECTABLE") {
		t.Errorf("the unreadable path was not reported as uninspectable:\n%s", out)
	}
	if strings.Contains(out, "ABSENT") {
		t.Errorf("an uninspectable path was reported as absent:\n%s", out)
	}
	// Still resolvable: the instruction stands, and because holdfast could not LOOK, it
	// does not overrule the operator with "absent".
	code, _, errOut = resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "source-is-intact", "--replacement", "retain")
	if code != 0 {
		t.Fatalf("resolving an uninspectable path: code = %d, stderr: %s", code, errOut)
	}
	if in := f.incident(t, id); in.DispositionReplacement != store.RetainedExcluded {
		t.Errorf("replacement disposition = %q, want %q", in.DispositionReplacement, store.RetainedExcluded)
	}
}

// TestResolve_AnInvocationAgainstAJobThatIsNotParkedChangesNothing is AC15g, and the
// report is DISTINCT from the absent-file case: one is "there is nothing to resolve",
// the other is "there is something to resolve and one of its files has gone".
func TestResolve_AnInvocationAgainstAJobThatIsNotParkedChangesNothing(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)

	// (1) no such job.
	code, _, errOut := resolveCmd(t, "--config", f.cfg, "--id", "999")
	if code == 0 {
		t.Error("an invocation against no job at all reported success")
	}
	if !strings.Contains(errOut, "not a parked job") || !strings.Contains(errOut, "nothing was changed") {
		t.Errorf("stderr does not report it distinctly:\n%s", errOut)
	}
	if strings.Contains(errOut, "ABSENT") {
		t.Errorf("the not-parked case was reported as the absent-file case:\n%s", errOut)
	}

	// (2) a job that is no longer parked.
	if code, _, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "swap-was-applied", "--replacement", "retain"); code != 0 {
		t.Fatalf("first resolution: code = %d, stderr %s", code, errOut)
	}
	before := f.incident(t, id)
	srcBytes := readFile(t, f.source)
	replBytes := readFile(t, f.replaced)

	code, _, errOut = resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "source-is-intact", "--replacement", "delete")
	if code == 0 {
		t.Error("a second resolution of the same job was accepted")
	}
	if !strings.Contains(errOut, "already resolved") {
		t.Errorf("stderr does not say the job is already resolved:\n%s", errOut)
	}
	after := f.incident(t, id)
	if after.Resolution != before.Resolution || after.DispositionReplacement != before.DispositionReplacement {
		t.Errorf("the record was modified: %+v -> %+v", before, after)
	}
	if !bytes.Equal(readFile(t, f.source), srcBytes) || !bytes.Equal(readFile(t, f.replaced), replBytes) {
		t.Error("a refused invocation modified a file")
	}
}

// TestResolve_ARemovalIsOrderedAfterTheRecordIsDurable is AC15k's ordering, from the
// outside: with the store unwritable, the file the instruction licensed for deletion is
// STILL THERE afterwards, the job is STILL PARKED, the failure is reported with both
// paths, the determination and both dispositions - and re-invoking once the store is
// writable resolves it. A store failure costs a repeated instruction and never an
// unrecorded deletion.
func TestResolve_ARemovalIsOrderedAfterTheRecordIsDurable(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)

	// Make the job store unwritable the way an operator's does go: the database file
	// (and its directory) lose write permission.
	stateDir := filepath.Join(f.dir, "state")
	if err := os.Chmod(filepath.Join(stateDir, "jobs.db"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		_ = os.Chmod(stateDir, 0o755)
		_ = os.Chmod(filepath.Join(stateDir, "jobs.db"), 0o644)
	}
	t.Cleanup(restore)
	if canWrite(filepath.Join(stateDir, "jobs.db")) {
		t.Skip("this process can write a read-only file (running as root?)")
	}

	code, _, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "swap-was-applied", "--replacement", "delete")
	if code == 0 {
		t.Fatal("an unpersistable resolution reported success")
	}
	if !exists(f.replaced) {
		t.Fatal("the replacement was DELETED although the resolution authorising it could not be recorded")
	}
	// Reported with everything an operator needs to repeat the instruction.
	for _, want := range []string{"STILL PARKED", f.source, f.replaced, "swap-was-applied", string(store.KeptInPlace), string(store.Deleted)} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the failure report does not carry %q:\n%s", want, errOut)
		}
	}
	if !f.incident(t, id).Parked() {
		t.Fatal("the job left the parked state without a durable record")
	}

	// Once the store is writable, the same instruction resolves it - and only then is
	// the removal ordered.
	restore()
	code, _, errOut = resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "swap-was-applied", "--replacement", "delete")
	if code != 0 {
		t.Fatalf("re-invocation: code = %d, stderr: %s", code, errOut)
	}
	in := f.incident(t, id)
	if in.Parked() {
		t.Error("the job is still parked after a durable resolution")
	}
	if in.DispositionReplacement != store.Deleted {
		t.Errorf("replacement disposition = %q, want %q", in.DispositionReplacement, store.Deleted)
	}
	if exists(f.replaced) {
		t.Error("the licensed removal did not happen once the record was durable")
	}
	if !exists(f.source) {
		t.Error("the source path was removed - only the replacement was licensed")
	}
}

// TestResolve_ALicensedRemovalThatFailsCorrectsTheRecordAndKeepsThePathExcluded is the
// unhappy path of the only licensed removal in holdfast. The record is made durable
// first (AC15k), so if the unlink then fails the record would be claiming a deletion that
// did not happen and the exclusion keyed to that disposition would have been released -
// with the file still sitting in the library. The disposition is therefore CORRECTED to
// retained-excluded, the surviving file stays out of enumeration, and the failure is
// reported. The determination itself stands: it is recorded, durable and correct - only
// the disposal failed.
func TestResolve_ALicensedRemovalThatFailsCorrectsTheRecordAndKeepsThePathExcluded(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)

	orig := removeFile
	removeFile = func(string) error { return errors.New("unlink: read-only file system") }
	t.Cleanup(func() { removeFile = orig })

	code, _, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
		"--determination", "swap-was-applied", "--replacement", "delete")
	if code == 0 {
		t.Error("a failed removal reported success")
	}
	if !strings.Contains(errOut, "REMOVING the replacement failed") {
		t.Errorf("the failure was not reported:\n%s", errOut)
	}
	if !strings.Contains(errOut, f.replaced) {
		t.Errorf("the failure report does not name the file that is still there:\n%s", errOut)
	}
	if !exists(f.replaced) {
		t.Fatal("the file is gone - the fixture did not exercise a FAILED removal")
	}

	in := f.incident(t, id)
	if in.DispositionReplacement != store.RetainedExcluded {
		t.Fatalf("disposition = %q, want it corrected to %q - a record must not claim a deletion that did not happen",
			in.DispositionReplacement, store.RetainedExcluded)
	}
	if in.RemovalError == "" {
		t.Error("the removal failure was not recorded beside the corrected disposition")
	}
	if in.Resolution != store.SwapWasApplied {
		t.Errorf("the operator's determination was changed to %q", in.Resolution)
	}
	if in.Parked() {
		t.Error("the job was returned to the parked state - the determination is recorded and correct")
	}
	// The whole point of the correction: the surviving file is held out of enumeration.
	excluded, err := f.st.ExcludedReplacementPaths(context.Background())
	if err != nil {
		t.Fatalf("ExcludedReplacementPaths: %v", err)
	}
	if len(excluded) != 1 || excluded[0] != f.replaced {
		t.Errorf("the surviving file is not excluded from enumeration: %v", excluded)
	}
}

// TestResolve_ARefusedInstructionTouchesNothing covers the input the operator can get
// wrong: a determination that is not one of the two, and a missing disposition. A
// resolution that leaves either path without a disposition is not accepted.
func TestResolve_ARefusedInstructionTouchesNothing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"a determination that is not one of the two", []string{"--determination", "probably-fine", "--replacement", "retain"}, "is not a determination"},
		{"no disposition for the replacement", []string{"--determination", "source-is-intact"}, "--replacement is required"},
		{"a disposition that is not one of the two", []string{"--determination", "source-is-intact", "--replacement", "shred"}, "not a replacement disposition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newParkedFixture(t)
			id := f.park(t)
			args := append([]string{"--config", f.cfg, "--id", itoa(id)}, tc.args...)
			code, _, errOut := resolveCmd(t, args...)
			if code == 0 {
				t.Fatal("a malformed instruction was accepted")
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want it to name %q", errOut, tc.want)
			}
			if !f.incident(t, id).Parked() {
				t.Error("a refused instruction left the parked state")
			}
			if !exists(f.source) || !exists(f.replaced) {
				t.Error("a refused instruction removed a file")
			}
		})
	}
}

// TestResolve_TheReplacementIsNeverKeptInPlace. "kept-in-place" means later runs treat
// that path normally, and handing a gate-passed encode back to enumeration as if it were
// somebody's source is how a library acquires a permanent duplicate. The command offers
// no way to ask for it, and the store refuses it even if one were added.
func TestResolve_TheReplacementIsNeverKeptInPlace(t *testing.T) {
	f := newParkedFixture(t)
	id := f.park(t)
	for _, det := range []string{"swap-was-applied", "source-is-intact"} {
		t.Run(det, func(t *testing.T) {
			if _, _, errOut := resolveCmd(t, "--config", f.cfg, "--id", itoa(id),
				"--determination", det, "--replacement", "kept-in-place"); !strings.Contains(errOut, "not a replacement disposition") {
				t.Errorf("the command accepted kept-in-place for the replacement:\n%s", errOut)
			}
		})
	}
	err := f.st.ResolveIncident(context.Background(), id, store.Resolution{
		Determination:          store.SwapWasApplied,
		DispositionSource:      store.KeptInPlace,
		DispositionReplacement: store.KeptInPlace,
	})
	if err == nil {
		t.Fatal("the store accepted kept-in-place for a recorded replacement path")
	}
}

// --- helpers ------------------------------------------------------------------

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

func canWrite(p string) bool {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

package main

// The operator's half of the undo window (UNDO-6): `holdfast restore`.
//
// Everything here is graded through the REAL command - dispatch, the real config
// load, the real store, real files - because a restore is the one operation this tool
// has that overwrites a library file with older bytes. Its refusals are therefore
// worth as much as its successes, and each of them is asserted to have changed
// NOTHING: the fixture tree is hashed before and after and compared file by file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// subprocessEnv makes the test binary re-exec itself as the holdfast CLI; see
// TestMain in startup_test.go.
const subprocessEnv = "HOLDFAST_TEST_SUBPROCESS"

// ---- helpers ----------------------------------------------------------------

// undoLibrary is preflightLibrary with the undo window open and the perceptual gate
// off. The VMAF gate is a second full decode and is proven by its own suite; what is
// under test here is what happens to the ORIGINAL after a swap, so these fixtures buy
// nothing by re-measuring one.
func undoLibrary(t *testing.T, hours int, extra string) (cfgPath, lib, state, src string) {
	t.Helper()
	return preflightLibrary(t, fmt.Sprintf("vmaf_enable: false\nundo_window_hours: %d\n%s", hours, extra))
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// treeHashes is every regular file under root, by path, hashed. It is how a refusal
// is proved to have mutated NOTHING - not the target, not the retained original, not
// anything else that happened to be in the library.
func treeHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out[path] = sha256File(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := treeHashes(t, root)
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s was REMOVED by an operation that reported a refusal", path)
			continue
		}
		if got != want {
			t.Errorf("%s changed under an operation that reported a refusal", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was CREATED by an operation that reported a refusal", path)
		}
	}
}

// cli runs one command in-process and returns its exit code and streams.
func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := dispatch(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// cliProcess runs one command in a REAL child process, which is what makes a claim
// about surviving a restart mean anything: the child shares nothing with this test
// but the state directory on disk.
func cliProcess(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			t.Fatalf("running %v: %v\n%s", args, err, out)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

// errorsAs is errors.As, kept local so the import list of this file stays about what
// it is testing.
func errorsAs(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// openStore opens the state directory's ledger for assertions.
func openStore(t *testing.T, state string) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func onlyRetention(t *testing.T, st *store.SQLite) store.Retained {
	t.Helper()
	rows, err := st.ListRetained(context.Background())
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d retention(s), want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// ---- AC2: a restore returns the source, and the ledger records it ------------

// TestRestore_ReturnsTheSourceByteForByteAndRecordsIt is UNDO-6's second criterion.
//
// Both halves matter and they fail independently: an operator needs the bytes back,
// and they need the ledger to stop claiming the swap that has just been undone still
// stands. A ledger left reporting `done` for a file that is now the original again is
// not merely stale - it is what the next scan reads, and it would re-encode the file
// somebody just rescued.
func TestRestore_ReturnsTheSourceByteForByteAndRecordsIt(t *testing.T) {
	cfgPath, _, state, src := undoLibrary(t, 24, "")
	before := sha256File(t, src)

	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	if got := sha256File(t, src); got == before {
		t.Fatal("the swap did not happen, so there is nothing to restore - the fixture proves nothing")
	}

	st := openStore(t, state)
	r := onlyRetention(t, st)
	st.Close()

	code, out, errOut := cli(t, "restore", "--config", cfgPath, src)
	if code != 0 {
		t.Fatalf("restore exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, src) {
		t.Errorf("the restore report does not name the file it restored: %s", out)
	}

	// The bytes are back, exactly.
	if got := sha256File(t, src); got != before {
		t.Errorf("the restored file is not the pre-swap source:\n  got  %s\n  want %s", got, before)
	}

	st2 := openStore(t, state)
	rec, ok, err := st2.GetRetained(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the ledger has no record of the retention that was just restored")
	}
	if rec.RestoredAt == nil {
		t.Fatal("the ledger does not record that the restore happened")
	}
	if age := time.Since(time.Unix(*rec.RestoredAt, 0)); age < 0 || age > time.Hour {
		t.Errorf("the recorded restore time %d is not when the restore happened", *rec.RestoredAt)
	}

	// A ledger read for that path reports the RESTORE, not the swap's done outcome.
	rows, err := st2.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var forPath []store.Job
	for _, j := range rows {
		if j.Path == src {
			forPath = append(forPath, j)
		}
	}
	if len(forPath) != 1 {
		t.Fatalf("the ledger holds %d row(s) for %s, want exactly 1: %+v", len(forPath), src, forPath)
	}
	if forPath[0].Status == store.Done {
		t.Errorf("the ledger still reports the swap as done for a file that has been restored: %+v", forPath[0])
	}
	if forPath[0].Outcome.Reason != engine.SkipRestoredOriginal {
		t.Errorf("the ledger row reads %q, want %q so a reader can see the file was restored",
			forPath[0].Outcome.Reason, engine.SkipRestoredOriginal)
	}
	if r.RetainedPath == "" || fileExists(r.RetainedPath) {
		t.Errorf("the retained original is still at %s after being moved back into place", r.RetainedPath)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestRestore_ARestoredFileIsNotImmediatelyReEncoded is the other half of recording
// the restore, and it is the one an operator would find out about the hard way: the
// scan that follows a restore must not swap the file again, through the very gates
// that passed the encode they rejected.
func TestRestore_ARestoredFileIsNotImmediatelyReEncoded(t *testing.T) {
	cfgPath, _, _, src := undoLibrary(t, 24, "")
	before := sha256File(t, src)

	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	if code, _, errOut := cli(t, "restore", "--config", cfgPath, src); code != 0 {
		t.Fatalf("restore exited %d: %s", code, errOut)
	}
	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("second run exited %d: %s", code, errOut)
	}
	if got := sha256File(t, src); got != before {
		t.Error("the scan after a restore swapped the file again - the restore was undone by the tool itself")
	}
}

// ---- AC10: nothing retained, or already released -----------------------------

// TestRestore_RefusesWhenThereIsNothingRetained is UNDO-6's tenth criterion, in both
// of its shapes: a path this tool never swapped, and one whose window has closed and
// whose retention has been released. Each must name the path, exit non-zero, and
// leave every file in the library exactly as it was.
func TestRestore_RefusesWhenThereIsNothingRetained(t *testing.T) {
	t.Run("never retained", func(t *testing.T) {
		cfgPath, lib, _, _ := undoLibrary(t, 24, "")
		other := filepath.Join(lib, "never-touched.mkv")
		if err := os.WriteFile(other, []byte("some bytes nobody swapped"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := treeHashes(t, lib)

		code, _, errOut := cli(t, "restore", "--config", cfgPath, other)
		if code == 0 {
			t.Fatal("restoring a path with nothing retained exited 0")
		}
		if !strings.Contains(errOut, other) {
			t.Errorf("the refusal does not name the path: %s", errOut)
		}
		assertTreeUnchanged(t, lib, before)
	})

	t.Run("already released", func(t *testing.T) {
		cfgPath, lib, state, src := undoLibrary(t, 1, "")
		if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
			t.Fatalf("run exited %d: %s", code, errOut)
		}
		st := openStore(t, state)
		r := onlyRetention(t, st)
		// Age the retention past its window through the store's own API - the same row
		// an hour of wall clock would produce, without an hour of wall clock.
		expired := r
		expired.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		if err := st.Retain(context.Background(), expired); err != nil {
			t.Fatal(err)
		}
		st.Close()

		// The next scan closes the window.
		if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
			t.Fatalf("second run exited %d: %s", code, errOut)
		}
		if fileExists(r.RetainedPath) {
			t.Fatal("the release did not happen, so this case is not about a released retention")
		}
		before := treeHashes(t, lib)

		code, _, errOut := cli(t, "restore", "--config", cfgPath, src)
		if code == 0 {
			t.Fatal("restoring a released retention exited 0")
		}
		if !strings.Contains(errOut, src) {
			t.Errorf("the refusal does not name the path: %s", errOut)
		}
		assertTreeUnchanged(t, lib, before)
	})
}

// TestRestore_RefusesWhenTheRetainedOriginalIsGone is UNDO-6's thirteenth criterion at
// the command surface: the record promised a restore, the retained original is not
// there, and the honest answer is to say so, drop the record and change nothing.
func TestRestore_RefusesWhenTheRetainedOriginalIsGone(t *testing.T) {
	cfgPath, lib, state, src := undoLibrary(t, 24, "")
	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	st := openStore(t, state)
	r := onlyRetention(t, st)
	st.Close()

	if err := os.Remove(r.RetainedPath); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, lib)

	code, _, errOut := cli(t, "restore", "--config", cfgPath, src)
	if code == 0 {
		t.Fatal("restoring a record whose retained original is gone exited 0")
	}
	if !strings.Contains(errOut, r.RetainedPath) {
		t.Errorf("the refusal does not name the missing retained original: %s", errOut)
	}
	assertTreeUnchanged(t, lib, before)

	st2 := openStore(t, state)
	if rows, err := st2.ListRetained(context.Background()); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Errorf("the unrestorable record was not dropped: %+v", rows)
	}
}

// ---- AC11: the file at the target has moved since the swap -------------------

// TestRestore_RefusesWhenTheSwappedFileHasMoved is UNDO-6's eleventh criterion, and it
// is the failure mode a restore has that costs an operator data. If something else
// wrote the file since the swap - a re-download, an *arr upgrade, a human - putting
// the original back would silently destroy that content, exactly as a swap over a
// rewritten source would. Both fingerprints are named, because "it changed" without
// saying from what to what is not something anyone can act on.
func TestRestore_RefusesWhenTheSwappedFileHasMoved(t *testing.T) {
	cfgPath, lib, state, src := undoLibrary(t, 24, "")
	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	st := openStore(t, state)
	r := onlyRetention(t, st)
	st.Close()

	newer := []byte("content that arrived after the swap and must not be destroyed")
	if err := os.WriteFile(src, newer, 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeHashes(t, lib)

	code, _, errOut := cli(t, "restore", "--config", cfgPath, src)
	if code == 0 {
		t.Fatal("restoring over content holdfast did not write exited 0")
	}
	if !strings.Contains(errOut, r.SwappedFingerprint) {
		t.Errorf("the refusal does not name the fingerprint holdfast swapped in (%s): %s", r.SwappedFingerprint, errOut)
	}
	if !strings.Contains(errOut, fingerprintOf(t, src)) {
		t.Errorf("the refusal does not name the fingerprint the file carries now: %s", errOut)
	}
	if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, newer) {
		t.Error("the newer content was overwritten by a restore that reported a refusal")
	}
	assertTreeUnchanged(t, lib, before)
}

// fingerprintOf is the size:mtime identity the refusal above names, computed the same
// way the tool does.
func fingerprintOf(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().Unix())
}

// ---- AC12: it survives a restart ---------------------------------------------

// TestRestore_SurvivesARestart is UNDO-6's twelfth criterion. A retention that only
// worked inside the process that took it would be worthless: the operator who needs
// it is, by construction, the one who came back later.
//
// Every step here is a SEPARATE OS PROCESS. They share nothing but the state
// directory, so the second one knows what it knows because it read the ledger off
// disk, not because this test told it.
func TestRestore_SurvivesARestart(t *testing.T) {
	t.Run("restore", func(t *testing.T) {
		cfgPath, _, state, src := undoLibrary(t, 24, "")
		before := sha256File(t, src)

		if code, out := cliProcess(t, "run", "--config", cfgPath); code != 0 {
			t.Fatalf("the first process exited %d:\n%s", code, out)
		}
		if sha256File(t, src) == before {
			t.Fatal("the first process did not swap, so there is nothing to restore across the restart")
		}

		// A second process, listing what a first process retained.
		code, out := cliProcess(t, "restore", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("listing exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, src) {
			t.Errorf("a second process does not see the original the first one retained:\n%s", out)
		}

		// A third process, restoring it.
		code, out = cliProcess(t, "restore", "--config", cfgPath, src)
		if code != 0 {
			t.Fatalf("restoring from a second process exited %d:\n%s", code, out)
		}
		if got := sha256File(t, src); got != before {
			t.Errorf("the file restored by a later process is not the pre-swap source:\n  got  %s\n  want %s", got, before)
		}
		_ = state
	})

	t.Run("release", func(t *testing.T) {
		cfgPath, _, state, src := undoLibrary(t, 1, "")
		if code, out := cliProcess(t, "run", "--config", cfgPath); code != 0 {
			t.Fatalf("the first process exited %d:\n%s", code, out)
		}
		st := openStore(t, state)
		r := onlyRetention(t, st)
		expired := r
		expired.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		if err := st.Retain(context.Background(), expired); err != nil {
			t.Fatal(err)
		}
		st.Close()

		// A second process, releasing what a first process retained.
		if code, out := cliProcess(t, "run", "--config", cfgPath); code != 0 {
			t.Fatalf("the second process exited %d:\n%s", code, out)
		}
		if fileExists(r.RetainedPath) {
			t.Error("a later process did not release the expired retention a first one took")
		}
		st2 := openStore(t, state)
		if rows, err := st2.ListRetained(context.Background()); err != nil {
			t.Fatal(err)
		} else if len(rows) != 0 {
			t.Errorf("a later process did not drop the released record: %+v", rows)
		}
		if !fileExists(src) {
			t.Error("the release removed the library file rather than the retained original")
		}
	})
}

// ---- the listing -------------------------------------------------------------

// TestRestore_ListsWhatIsRetainedAndHowLongItHasLeft covers the no-argument form: the
// two facts an operator is deciding between are what a file would cost to keep and
// how long they have to decide.
func TestRestore_ListsWhatIsRetainedAndHowLongItHasLeft(t *testing.T) {
	cfgPath, _, state, src := undoLibrary(t, 24, "")

	// Before any swap there is nothing to list, and saying so is not an error.
	code, out, errOut := cli(t, "restore", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("listing an empty window exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "nothing is retained") {
		t.Errorf("the empty listing does not say so: %s", out)
	}

	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
	st := openStore(t, state)
	r := onlyRetention(t, st)
	st.Close()

	code, out, errOut = cli(t, "restore", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("listing exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, src) {
		t.Errorf("the listing does not name the retained original's path: %s", out)
	}
	if !strings.Contains(out, fmt.Sprint(r.SourceBytes)) {
		t.Errorf("the listing does not report the %d bytes it is holding: %s", r.SourceBytes, out)
	}
	if !strings.Contains(out, "left") {
		t.Errorf("the listing does not say how long the window has left: %s", out)
	}
}

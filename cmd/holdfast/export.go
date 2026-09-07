package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast export` - taking the record somewhere else (LEDGER-5).
//
// The ledger is the evidence an operator audits after this tool has deleted their
// originals, and until now the only way to read it was through a capped HTTP response or
// by opening the SQLite file by hand. This writes every terminal row out as
// newline-delimited JSON, one object per line, in exactly the shape /api/history publishes
// for a row - which is a promise kept by CALLING that projection (server.HistoryRowJSON)
// rather than by restating it here.
//
// It is a local, operator-run READ of the operator's own store: no listener, no port, no
// network surface of any kind, and it never writes to the store it reads. That last clause
// is enforced rather than promised - store.OpenReadOnly opens the file `mode=ro`, so SQLite
// itself refuses every write, and it does NOT migrate. The daemon's door (store.Open)
// migrates unconditionally, which for a READER would mean that exporting from a ledger an
// older holdfast wrote silently upgraded that ledger in place - whereupon store.migrate
// refuses the file to the very binary still running against it. The one command whose job
// is to preserve the record must not be the command that costs an operator their daemon.
//
// A ledger this build's schema does not match is therefore a REFUSAL naming the store path,
// in both directions (criterion 13), not a repair: newer was already refused, and older is
// refused now. Migrating stays a deliberate act - `holdfast run` or `holdfast serve`.
//
// Absence survives the trip. Every unmeasured field is an explicit JSON `null`, never a 0,
// because 0 is legal for all of them and a VMAF of 0.0 is a destroyed frame rather than a
// missing measurement. An export that flattened those to zeros would be inventing evidence
// about swaps nobody measured, in the one file whose entire job is to be evidence.

func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "write the export to this path instead of stdout (refuses to overwrite an existing file)")
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}

	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")

	// The store must ALREADY EXIST. store.Open would happily create one, and an export
	// that silently created an empty database and then reported an empty ledger would
	// tell an operator who mistyped state_dir that they had transcoded nothing - the
	// most misleading possible answer from a tool whose job is to be evidence. A read
	// that finds nothing to read says so and exits non-zero.
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "holdfast: cannot read the job store %q: %v\n", dbPath, err)
		return 1
	}
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		// Covers every way the store refuses to be read, including a schema version that
		// is not this build's - ahead of it (a database from the future, whose columns a
		// narrower SELECT cannot all see) or behind it (a ledger an earlier holdfast
		// wrote, which this command refuses to migrate on its way past). The path is
		// named because "opening the job store failed" without it is unactionable.
		fmt.Fprintf(stderr, "holdfast: cannot open the job store %q: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if *out == "" {
		if err := writeExport(context.Background(), st, stdout); err != nil {
			fmt.Fprintf(stderr, "holdfast: exporting the ledger: %v\n", err)
			return 1
		}
		return 0
	}
	if err := exportToFile(context.Background(), st, *out); err != nil {
		fmt.Fprintf(stderr, "holdfast: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "exported the ledger to %s\n", *out)
	return 0
}

// effectiveStateDir is the directory the store lives in, defaulting an explicitly empty
// state_dir the same way buildEngine does so `export` and `run` cannot disagree about
// which database they are talking about.
func effectiveStateDir(cfg *config.Config) string {
	if cfg.StateDir == "" {
		return "state"
	}
	return cfg.StateDir
}

// exportToFile writes the export to path, and it is the whole of the "leave nothing
// behind" contract.
//
// REFUSE TO CLOBBER, first and before anything is read: an export is a record, and
// silently overwriting the last one - which may be the only copy of rows a prune has since
// removed - is a data loss this subcommand exists to prevent, not to cause. os.Lstat, so
// a symlink at the destination counts as existing rather than being followed to whatever
// it points at.
//
// Then temp-then-rename, in the destination's OWN directory so the rename is same
// filesystem and atomic: a failure at any point removes the temp and leaves NO partial
// export, and a reader never sees a half-written file under the name they asked for. It is
// the same discipline the engine's swap and the dashboard generator both use.
func exportToFile(ctx context.Context, st store.Store, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("refusing to overwrite the existing export destination %q "+
			"(an export is a record; move or remove it, or name another path)", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot use the export destination %q: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".holdfast-export-*")
	if err != nil {
		return fmt.Errorf("cannot create the export destination %q: %w", path, err)
	}
	tmpName := tmp.Name()
	// Every failure below unlinks the temp. The deferred remove is a no-op after a
	// successful rename (the name no longer exists), which is why it is unconditional.
	defer func() { _ = os.Remove(tmpName) }()

	if err := writeExport(ctx, st, tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write the export %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write the export %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write the export %q: %w", path, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("cannot write the export %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot create the export destination %q: %w", path, err)
	}
	return nil
}

// writeExport streams every terminal row to w as newline-delimited JSON.
//
// It STREAMS - one row off the cursor at a time, never a slice of the whole table -
// because a ledger that has outgrown a capped API response has also outgrown memory, and
// this subcommand exists precisely for the library that got that big.
//
// A ledger with no terminal row writes NOTHING and returns nil. That is the honest empty
// export: zero lines, exit zero, distinguishable from any failure, which exits non-zero
// with a message naming what failed.
func writeExport(ctx context.Context, st store.Store, w io.Writer) error {
	bw := bufio.NewWriter(w)
	err := st.EachTerminal(ctx, func(j store.Job) error {
		line, err := server.HistoryRowJSON(j)
		if err != nil {
			return fmt.Errorf("encoding the row for %q: %w", j.Path, err)
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	})
	if err != nil {
		return err
	}
	return bw.Flush()
}

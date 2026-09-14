package main

// Refuter artifact for S0103-holdfast-plan-report, impl gate ordinal 2, finding F5.
//
// AC-11: "WHEN the build contains the `analyze` command and both it and `plan` run over the
// same library with the same configuration, THE SYSTEM SHALL report an identical covered
// file set and an identical per file skip reason from both."
//
// Ordinal 1's F1 was the SYMBOLIC LINK case and this loop reconciled it. The census still
// carries its own membership rule for every OTHER irregular entry: an entry that is not a
// regular file and not a symbolic link is withheld as irregular, whatever its name. The
// enumeration `plan` and the daemon share decides membership by NAME, so a unix socket
// carrying a configured video extension is enumerated, covered and decided by BOTH of them,
// and is in no figure of `analyze`.

import (
	"path/filepath"
	"syscall"
	"testing"
)

// mknodSocket puts a unix socket INODE under a configured video extension. mknod(2) lets an
// unprivileged caller create a socket or a FIFO, and a socket is used rather than a FIFO
// because opening a FIFO with no writer blocks.
func mknodSocket(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mknod(path, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Skipf("this filesystem will not hold a unix socket inode: %v", err)
	}
}

func TestRegress0103F5_PlanAndAnalyzeDisagreeOnAnIrregularNonLinkSource(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")
	mknodSocket(t, filepath.Join(lib, "stream.mkv"))

	c := analyzeJSON(t, cfgPath)
	p := planJSON(t, cfgPath)

	if p.Total.Covered.Files != c.Total.Sources.Files {
		t.Fatalf("AC-11: plan covers %d file(s), analyze counts %d source(s) over the same library "+
			"with the same configuration - the two covered sets are not identical",
			p.Total.Covered.Files, c.Total.Sources.Files)
	}
	if p.Total.Covered.Bytes != c.Total.Sources.Bytes {
		t.Fatalf("AC-11: plan covers %d byte(s), analyze counts %d",
			p.Total.Covered.Bytes, c.Total.Sources.Bytes)
	}
}

// TestRegress0103F5b_TheDaemonPassCoversIt establishes which of the two is out of step, so
// the remedy is not a guess: plan covering the entry is AC-10 holding - the daemon's own
// pass enumerates it and records a terminal row for it - and the census is the copy that
// disagrees. It is graded exactly as AC-10's own named test grades the equality.
func TestRegress0103F5b_TheDaemonPassCoversIt(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")
	sock := filepath.Join(lib, "stream.mkv")
	mknodSocket(t, sock)

	decided := daemonPassPaths(t, cfgPath)
	if decided[sock] == "" {
		t.Fatalf("the daemon pass recorded no row for %q, so plan must not cover it either: %v",
			sock, decided)
	}
	t.Logf("the daemon pass records %q for %q, and analyze counts it as no source at all",
		decided[sock], sock)
}

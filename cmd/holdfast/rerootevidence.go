package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The evidence that --to is the tree --from was: a MOVE of one filesystem, not a copy of it
// and not another tree laid out alike.
//
// Presence alone cannot say that. A copy of a few recorded files under --to, with the old
// tree still in place, matches every size and mtime it holds; carried on that evidence, a
// retained original the copy left out is "gone" at the new path, the next restore drops its
// record as unrestorable under a configured root, and the original under --from is orphaned.
// So every recorded file is read at BOTH paths, and the old path is the deciding witness:
//
//   - present at both: the same file on two mounts of one filesystem (an old mount point left
//     in place, which a real host can have) has ONE identity, the same device and inode at
//     both paths, and that is the strongest evidence there is. Two identities is a copy, and
//     refuses.
//   - present only at --from: a partial copy, or the wrong --to. It refuses: carrying the
//     row would point it at nothing while the file it describes is still where it was.
//   - present only at --to: the old mount is gone, and the file is judged by what the ledger
//     recorded about it - a size:mtime for a media file, a size for a retained original.
//   - present at neither: deleted since its row was written, the ordinary state of a ledger.
//
// Two shares then decide, over what remains: the MATCH RATIO, matched over matched plus
// replaced, which rules out an unrelated tree (it matches almost nothing, the same tree has
// replaced only a few); and the COVERAGE, matched over every recorded file checked, gone
// included, which rules out a sparse tree that shares a few files and lacks the rest.

// rerootMinMatch is --min-match's default: the share of the recorded files found under --to
// that must still be the recorded file. A live library replaces files under the same name
// (an episode upgraded in place) between the move and the reroot, so the rule is DOMINANCE
// rather than unanimity: the question is whether --to is the same filesystem as --from, and
// an unrelated tree matches almost nothing.
const rerootMinMatch = 0.95

// rerootMinMatched is the floor under the ratio: at least this many recorded files must
// match. One is enough, because a match is not a coincidence - the same relative path, the
// same size to the byte and the same modification second - and the ratio is what rules out
// the tree that matches almost nothing. A floor any higher would refuse a small library's
// honest move for want of files rather than for want of evidence.
const rerootMinMatched = 1

// rerootMinCoverage is --min-coverage's default. A library loses files between a row being
// written and a move - deleted, renamed by an *arr - but not most of them; a tree holding
// fewer than half the recorded files is not shown to be the same library.
const rerootMinCoverage = 0.5

// rerootEvidence is what the files at the two paths say about the plan.
type rerootEvidence struct {
	// Checked is every recorded file read; Matched those found as recorded, SameFile the
	// part of Matched proven by one identity at both paths; Absent those carried as gone.
	Checked, Matched, SameFile, Absent int
	// Replaced are media files at --to only that match no fingerprint recorded for their
	// path: replaced since under the same name, and carried as recorded.
	Replaced []rerootFinding
	// RetainedMismatched are retained originals not the recorded size, not a regular file,
	// or unreadable. Any one refuses: a restore renames that file over a library file.
	RetainedMismatched []rerootFinding
	// Copies are files at both paths with two identities, or whose identity could not be
	// read at one of them. LeftBehind are files at --from only. Any one refuses.
	Copies, LeftBehind []rerootFinding

	// FromSeen is whether --from is a directory this process can see, and Witnesses how many
	// recorded files are there. The identity check ran only where both hold: an old mount
	// point left empty is a directory that proves nothing.
	FromSeen  bool
	Witnesses int
	// MissingRetained are live retained originals absent under --to while the identity check
	// could not run, so nothing shows whether the original is gone or only out of sight. They
	// refuse unless --carry-missing-retained acknowledges them; CarriedMissingRetained counts
	// the ones it carried (they are in Absent too).
	MissingRetained        []rerootFinding
	CarriedMissingRetained int
}

// identityChecked reports whether the old tree was there to compare against.
func (r rerootEvidence) identityChecked() bool { return r.FromSeen && r.Witnesses > 0 }

// rerootFinding is one file that is not what the ledger recorded, or not where it should be.
type rerootFinding struct {
	Path, Recorded, Now string
}

// ratio is matched over matched plus replaced, 0 when nothing was found.
func (r rerootEvidence) ratio() float64 {
	return share(r.Matched, r.Matched+len(r.Replaced))
}

// coverage is matched over every recorded file checked, 0 when none was.
func (r rerootEvidence) coverage() float64 { return share(r.Matched, r.Checked) }

func share(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// notThere reports whether a stat error says the path does not exist.
func notThere(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// checkRerootEvidence reads every recorded file at its old path and its new one.
//
// Presence and identity are read with Lstat, of the ENTRY at each path: one file on two mounts
// is one entry, the same device and inode at both paths, while a --to tree of symbolic links
// into a still-present --from is a different entry from the file it points at, and refuses as
// a copy. Where only --to holds the file its attributes are read with Stat, FOLLOWING a link
// as probe.Fingerprint does, through probe.AttributesOf, the one derivation every recorded
// fingerprint came from.
func checkRerootEvidence(evidence []store.RerootEvidence, from string, carryMissingRetained bool) rerootEvidence {
	var res rerootEvidence
	if fi, err := os.Stat(from); err == nil && fi.IsDir() {
		res.FromSeen = true
	}
	type seen struct {
		atNew, atOld   os.FileInfo
		errNew, errOld error
	}
	reads := make([]seen, len(evidence))
	for i, e := range evidence {
		r := &reads[i]
		r.atNew, r.errNew = os.Lstat(e.Path)
		r.atOld, r.errOld = os.Lstat(e.Was)
		if r.errOld == nil {
			res.Witnesses++
		}
	}

	for i, e := range evidence {
		res.Checked++
		r := reads[i]
		recorded := recordedOf(e)
		newGone, oldGone := r.errNew != nil && notThere(r.errNew), r.errOld != nil && notThere(r.errOld)
		finding := func(now string) rerootFinding { return rerootFinding{Path: e.Path, Recorded: recorded, Now: now} }

		switch {
		case r.errOld != nil && !oldGone:
			res.Copies = append(res.Copies, finding(fmt.Sprintf("unreadable at %s (%v), so whether it is the same "+
				"file cannot be established", e.Was, r.errOld)))
		case newGone && !oldGone:
			res.LeftBehind = append(res.LeftBehind, finding("not there, and still at "+e.Was))
		case newGone && e.Retained && !res.identityChecked():
			// Gone from --to, and --from could not be looked at: the original may be out of
			// sight rather than gone, and the restore that meets this record after the move
			// drops it as unrestorable. So it is carried only on an acknowledgement.
			if !carryMissingRetained {
				res.MissingRetained = append(res.MissingRetained, finding("not there, and "+e.Was+" could not be checked"))
				continue
			}
			res.CarriedMissingRetained++
			res.Absent++
		case newGone:
			res.Absent++
		case r.errNew != nil:
			res.notAsRecorded(e, finding(fmt.Sprintf("unreadable (%v)", r.errNew)))
		case !oldGone:
			switch {
			case !os.SameFile(r.atOld, r.atNew):
				res.Copies = append(res.Copies, finding("a different file from the one still at "+e.Was))
			case e.Retained && (!r.atNew.Mode().IsRegular() || r.atNew.Size() != e.RetainedBytes):
				res.RetainedMismatched = append(res.RetainedMismatched, finding(retainedNow(r.atNew)))
			default:
				// One entry on two mounts: whatever has happened to the file since, this is the tree.
				res.Matched++
				res.SameFile++
			}
		case e.Retained:
			if r.atNew.Mode().IsRegular() && r.atNew.Size() == e.RetainedBytes {
				res.Matched++
			} else {
				res.RetainedMismatched = append(res.RetainedMismatched, finding(retainedNow(r.atNew)))
			}
		default:
			atNew, err := os.Stat(e.Path)
			if err != nil {
				res.notAsRecorded(e, finding(fmt.Sprintf("unreadable (%v)", err)))
				continue
			}
			now := probe.AttributesOf(atNew).String()
			if anyEquals(e.Fingerprints, now) {
				res.Matched++
			} else {
				res.Replaced = append(res.Replaced, finding(now))
			}
		}
	}
	return res
}

// notAsRecorded files a file that could not be read as recorded: a retained original refuses,
// a media file counts as replaced.
func (r *rerootEvidence) notAsRecorded(e store.RerootEvidence, f rerootFinding) {
	if e.Retained {
		r.RetainedMismatched = append(r.RetainedMismatched, f)
		return
	}
	r.Replaced = append(r.Replaced, f)
}

func retainedNow(fi os.FileInfo) string {
	if !fi.Mode().IsRegular() {
		return "not a regular file (" + fi.Mode().Type().String() + ")"
	}
	return fmt.Sprintf("%d bytes", fi.Size())
}

func recordedOf(e store.RerootEvidence) string {
	if e.Retained {
		return fmt.Sprintf("a retained original of %d bytes", e.RetainedBytes)
	}
	var s string
	for i, fp := range e.Fingerprints {
		if i > 0 {
			s += " or "
		}
		s += fp
	}
	return s
}

func anyEquals(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// refusal is why the evidence does not carry the plan, with the refusal's findings to name,
// and "" when it does.
func (r rerootEvidence) refusal(from, to string, minMatch, minCoverage float64) (string, string, []rerootFinding) {
	switch {
	case r.Checked == 0:
		return fmt.Sprintf("the ledger records no file under %s whose size and modification time could be checked "+
			"under %s, so nothing shows %s holds the same files", from, to, to), "", nil
	case len(r.Copies) > 0 || len(r.LeftBehind) > 0:
		why := fmt.Sprintf("%s is still live and does not agree with %s: %d recorded file(s) are a different file "+
			"at each path, and %d are still under %s but not under %s (named above). This is a copy or a partial "+
			"one, or the wrong --to, not a move: carrying the rows would leave each file they describe where it "+
			"is, and a retained original's record pointing at nothing", from, to, len(r.Copies), len(r.LeftBehind),
			from, to)
		return why, "not carried", append(append([]rerootFinding{}, r.Copies...), r.LeftBehind...)
	case len(r.RetainedMismatched) > 0:
		return fmt.Sprintf("%d retained original(s) under %s are not the size of the original their retention "+
				"recorded (named above). A restore renames that file over a library file, so a retention is carried "+
				"only where its file is the one recorded or is gone", len(r.RetainedMismatched), to),
			"retained original not the one recorded", r.RetainedMismatched
	case r.Matched < rerootMinMatched:
		return fmt.Sprintf("none of the %d file(s) the ledger recorded under %s exists under %s as the file it "+
			"recorded (%d found replaced, %d gone), so nothing shows %s holds the same files",
			r.Checked, from, to, len(r.Replaced), r.Absent, to), "", nil
	case r.coverage() < minCoverage:
		return fmt.Sprintf("only %d of the %d file(s) the ledger recorded under %s are under %s as recorded (a "+
			"coverage of %.3f, under the --min-coverage threshold of %g), so %s is not shown to be the same "+
			"library: a tree that shares a few files and lacks the rest is not the one that moved",
			r.Matched, r.Checked, from, to, r.coverage(), minCoverage, to), "", nil
	case r.ratio() < minMatch:
		return fmt.Sprintf("only %d of the %d recorded file(s) found under %s match what the ledger recorded "+
			"(a match ratio of %.3f, under the --min-match threshold of %g), so %s is not shown to be the same "+
			"files as %s: an unrelated tree matches almost nothing, and the same tree replaces only a few",
			r.Matched, r.Matched+len(r.Replaced), to, r.ratio(), minMatch, to, from), "", nil
	case len(r.MissingRetained) > 0:
		return fmt.Sprintf("%d retained original(s) the undo window still holds are not under %s (named above), "+
			"and %s could not be checked, so nothing shows whether each original is gone or only out of sight. "+
			"Carried to the new path, the next restore of one would find nothing there and drop its record. "+
			"Make %s visible to this command (bind it read-only into the container) so the old path can be "+
			"checked, or, if those originals are known to be gone, run again with --carry-missing-retained, which "+
			"carries their records as recorded for the undo window to drop as missing",
			len(r.MissingRetained), to, from, from), "retained original missing", r.MissingRetained
	}
	return "", "", nil
}

func printRerootEvidence(w io.Writer, r rerootEvidence, from, to string, minMatch, minCoverage float64) {
	fmt.Fprintf(w, "evidence: of %d file(s) the ledger recorded, %d are under %s as recorded (%d of them the "+
		"same file at both paths), %d were replaced since (carried as recorded), %d are at neither path "+
		"(deleted since; carried as recorded), %d differ from or are only at the old path, and %d retained "+
		"original(s) are not the recorded size or are missing\n",
		r.Checked, r.Matched, to, r.SameFile, len(r.Replaced), r.Absent, len(r.Copies)+len(r.LeftBehind),
		len(r.RetainedMismatched)+len(r.MissingRetained))
	fmt.Fprintf(w, "evidence: match ratio %d/%d = %.3f, threshold %g (--min-match); coverage %d/%d = %.3f, "+
		"threshold %g (--min-coverage)\n", r.Matched, r.Matched+len(r.Replaced), r.ratio(), minMatch,
		r.Matched, r.Checked, r.coverage(), minCoverage)
	printFindings(w, "replaced since (carried as recorded)", r.Replaced)
	switch {
	case r.identityChecked():
		fmt.Fprintf(w, "evidence: %s can be seen and holds %d of the recorded files, so each file at both "+
			"paths was checked to be one file\n", from, r.Witnesses)
	case r.FromSeen:
		fmt.Fprintf(w, "evidence: %s holds none of the recorded files (an empty mount point?), so the identity "+
			"check was not run\n", from)
	default:
		fmt.Fprintf(w, "evidence: %s cannot be seen from here, so the identity check was not run\n", from)
	}
	if r.CarriedMissingRetained > 0 {
		fmt.Fprintf(w, "evidence: %d retained original(s) not under %s carried as recorded (--carry-missing-retained); "+
			"the undo window drops them as missing\n", r.CarriedMissingRetained, to)
	}
}

// printFindings prints a bounded sample, each with what was recorded and what is there now.
func printFindings(w io.Writer, label string, list []rerootFinding) {
	for i, m := range list {
		if i == rerootSample {
			fmt.Fprintf(w, "  ... and %d more\n", len(list)-rerootSample)
			break
		}
		fmt.Fprintf(w, "%s: %s - the ledger recorded %s, and it is %s\n", label, m.Path, m.Recorded, m.Now)
	}
}

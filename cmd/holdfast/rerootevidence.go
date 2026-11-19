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
	// part of Matched proven by one identity at both paths; Absent those at neither path.
	Checked, Matched, SameFile, Absent int
	// Replaced are media files at --to only that match no fingerprint recorded for their
	// path: replaced since under the same name, and carried as recorded.
	Replaced []rerootFinding
	// RetainedMismatched are retained originals not the recorded size, or unreadable. Any
	// one refuses: a restore renames that file over a library file.
	RetainedMismatched []rerootFinding
	// Copies are files at both paths with two identities, or whose identity could not be
	// read at one of them. LeftBehind are files at --from only. Any one refuses.
	Copies, LeftBehind []rerootFinding
}

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

// checkRerootEvidence reads every recorded file at its old path and its new one. The stat
// FOLLOWS a symbolic link, as probe.Fingerprint does, and the attributes are read through
// probe.AttributesOf, the one derivation every recorded fingerprint came from.
func checkRerootEvidence(evidence []store.RerootEvidence) rerootEvidence {
	var res rerootEvidence
	for _, e := range evidence {
		res.Checked++
		recorded := recordedOf(e)
		atNew, errNew := os.Stat(e.Path)
		atOld, errOld := os.Stat(e.Was)
		newGone, oldGone := errNew != nil && notThere(errNew), errOld != nil && notThere(errOld)

		switch {
		case errOld != nil && !oldGone:
			res.Copies = append(res.Copies, rerootFinding{Path: e.Path, Recorded: recorded,
				Now: fmt.Sprintf("unreadable at %s (%v), so whether it is the same file cannot be established", e.Was, errOld)})
		case newGone && oldGone:
			res.Absent++
		case newGone:
			res.LeftBehind = append(res.LeftBehind, rerootFinding{Path: e.Path, Recorded: recorded,
				Now: "not there, and still at " + e.Was})
		case errNew != nil:
			f := rerootFinding{Path: e.Path, Recorded: recorded, Now: fmt.Sprintf("unreadable (%v)", errNew)}
			if e.Retained {
				res.RetainedMismatched = append(res.RetainedMismatched, f)
			} else {
				res.Replaced = append(res.Replaced, f)
			}
		case !oldGone && !os.SameFile(atOld, atNew):
			res.Copies = append(res.Copies, rerootFinding{Path: e.Path, Recorded: recorded,
				Now: "a different file from the one still at " + e.Was})
		case e.Retained && atNew.Size() != e.RetainedBytes:
			res.RetainedMismatched = append(res.RetainedMismatched, rerootFinding{Path: e.Path, Recorded: recorded,
				Now: fmt.Sprintf("%d bytes", atNew.Size())})
		case !oldGone:
			// One file on two mounts: whatever has happened to it since, this is the tree.
			res.Matched++
			res.SameFile++
		case e.Retained:
			res.Matched++
		default:
			now := probe.AttributesOf(atNew).String()
			if anyEquals(e.Fingerprints, now) {
				res.Matched++
			} else {
				res.Replaced = append(res.Replaced, rerootFinding{Path: e.Path, Recorded: recorded, Now: now})
			}
		}
	}
	return res
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
	}
	return "", "", nil
}

func printRerootEvidence(w io.Writer, r rerootEvidence, to string, minMatch, minCoverage float64) {
	fmt.Fprintf(w, "evidence: of %d file(s) the ledger recorded, %d are under %s as recorded (%d of them the "+
		"same file at both paths), %d were replaced since (carried as recorded), %d are at neither path "+
		"(deleted since; carried as recorded), %d differ from or are only at the old path, and %d retained "+
		"original(s) are not the recorded size\n",
		r.Checked, r.Matched, to, r.SameFile, len(r.Replaced), r.Absent, len(r.Copies)+len(r.LeftBehind),
		len(r.RetainedMismatched))
	fmt.Fprintf(w, "evidence: match ratio %d/%d = %.3f, threshold %g (--min-match); coverage %d/%d = %.3f, "+
		"threshold %g (--min-coverage)\n", r.Matched, r.Matched+len(r.Replaced), r.ratio(), minMatch,
		r.Matched, r.Checked, r.coverage(), minCoverage)
	printFindings(w, "replaced since (carried as recorded)", r.Replaced)
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

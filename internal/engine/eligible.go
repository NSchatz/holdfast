package engine

// The ONE decision about whether a path is a file this pipeline may act on.
//
// Enumeration and a targeted submission ask the same question from two different
// starting points. A scan already knows where it is looking - it walks the configured
// roots, so containment is a property of the walk - and decides each ENTRY from its name
// alone, which is what bounds its cost by the directory tree rather than by the library.
// A submission is handed a bare string by a caller on the network and knows nothing: the
// path may climb out of every root, may be a symbolic link into somewhere else entirely,
// may be a directory, may not exist.
//
// So the decision is written in two halves that compose, rather than as two decisions:
//
//   - Name is the half enumeration reads. It is exactly what IsSourceName has always
//     decided, and IsSourceName now delegates to it, so there is ONE implementation of
//     "a configured video extension, and not one of holdfast's own working files".
//   - Judge is the whole question, for a path arriving from outside. It RESOLVES the path
//     first and answers every later rule against the resolved form, then finishes by
//     asking Name. A rule added to Name therefore reaches both routes by construction.
//
// That is the property AC4a of S0093 asks for and the reason this file exists: a video
// extension added, a working-file name form added or a library root added must not need a
// second edit to a parallel copy of the same rules.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
)

// The rule tokens, a CLOSED vocabulary. They are the answer to "which rule refused this
// path", which is what a caller keys off and what an operator greps for, so they are
// treated exactly as the skip-guard tokens are: add freely, but renaming one changes what
// a report means.
const (
	// RuleNotAbsolute is a path that is not anchored at all. This tool only ever mutates
	// the filesystem under a library root, so a path resolved against somebody's working
	// directory is refused for the same reason config.Validate refuses a relative root.
	RuleNotAbsolute = "path-not-absolute"

	// RuleUnsupportedCharacters is a path carrying a literal tab or newline. Declined is
	// where that rule lives; refusing it at the door is what stops a submission being
	// accepted and then silently doing nothing.
	RuleUnsupportedCharacters = "unsupported-path-characters"

	// RuleUnresolvable is a path whose real form could not be established - most often a
	// directory on the way to it that this process may not read. It is deliberately NOT
	// the same answer as "does not exist": an unreadable directory is a deployment fault
	// an operator can fix, and answering it as an absence would send them looking for a
	// missing file.
	RuleUnresolvable = "path-unresolvable"

	// RuleNotARegularFile covers both "nothing is there" and "something is there that is
	// not a file": a directory, a device, a socket, a fifo. Each names what was seen.
	RuleNotARegularFile = "not-a-regular-file"

	// RuleOutsideRoots is THE rule this whole file exists to answer honestly. Everything
	// downstream of the pipeline's door is licensed to rewrite the file it was handed, so
	// a path that does not resolve at or beneath a configured library root must never
	// reach it.
	RuleOutsideRoots = "outside-library-roots"

	// RuleRetentionArea is a path inside a per-directory retention area. Those bytes are
	// what an operator was given a window to recover; handing them to the encoder would
	// destroy the thing the window is holding.
	RuleRetentionArea = "retention-area"

	// RuleWorkingFile is one of holdfast's own working files - a work-in-progress temp, a
	// retained original, a retained replacement. Each is a file holdfast wrote and none is
	// ever anybody's source, whatever extension it carries.
	RuleWorkingFile = "holdfast-working-file"

	// RuleNotAVideoFile is a path whose extension is not one of the configured
	// video_exts, which is the same test the scan's enumeration applies.
	RuleNotAVideoFile = "not-a-video-extension"

	// RuleFilteredPath is a path the configured exclude_paths/include_paths filters
	// keep out of this run. A scan reaches the same answer by not offering the file;
	// this is that answer for a path arriving from outside, because a filter is the
	// operator's statement about which paths this tool may touch and everything past
	// this door is licensed to rewrite the file it was handed.
	RuleFilteredPath = "excluded-by-path-filter"
)

// Declined is the whole of what this pipeline refuses OUTRIGHT: a path it will not claim,
// probe, encode or record anything about, whatever is at the other end of it. It answers
// with the rule token and the detail an operator reads, or false.
//
// It is ONE function and every door asks it - ProcessFile before it claims, the read-only
// plan pass before it counts, and Judge before it admits a submission - because a refusal
// the daemon takes and a report does not is exactly how a plan comes to publish a file as
// one a run would transcode when no run ever will. A refusal added HERE reaches all three
// with no second edit.
func Declined(p string) (rule, detail string, yes bool) {
	if strings.ContainsAny(p, "\t\n") {
		return RuleUnsupportedCharacters, "the path carries a literal tab or newline; such a path " +
			"is not processed, and a job row keyed on it would be legal SQL and worth nothing", true
	}
	return "", "", false
}

// DeclinedPath is BOTH questions ProcessFile asks of an ENUMERATED path before it claims
// anything: Declined above, and whether there is a file at the other end that a run would
// act on at all. It reads the filesystem and writes nothing.
//
// The stat FOLLOWS the link, exactly as ProcessFile's own does, so a dangling symbolic link
// and a directory by either spelling both answer yes here - and each is a path the daemon
// returns from having claimed nothing, probed nothing and recorded nothing. A report about
// what a run would do must not count one, which is why the read-only plan pass and the
// census ask this rather than a rule of their own.
func DeclinedPath(p string) (rule, detail string, yes bool) {
	if rule, detail, yes := Declined(p); yes {
		return rule, detail, true
	}
	fi, err := os.Stat(p)
	return DeclinedByAttributes(p, fi, err)
}

// DeclinedByAttributes is the half of DeclinedPath that needs an attribute read, answered
// from a read the caller already took. err is the failure of that read itself.
//
// It is split out so a caller that stats the file anyway asks this question off ITS OWN
// read rather than paying for a second one, and so the rule stays in ONE place while it
// does: a refusal added here still reaches every door, exactly as DeclinedPath's doc
// promises. The read must FOLLOW the link, as DeclinedPath's own does.
func DeclinedByAttributes(p string, fi os.FileInfo, err error) (rule, detail string, yes bool) {
	if err != nil {
		return RuleNotARegularFile, fmt.Sprintf("%s is not a file this run can act on: %v", p, err), true
	}
	if fi.IsDir() {
		return RuleNotARegularFile, fmt.Sprintf("%s is a directory, not a regular file", p), true
	}
	return "", "", false
}

// Ineligible is the ONE rule a path broke, and what was seen. Rule is the token; Detail
// is for the human reading the report, and carries the resolved path whenever resolution
// moved it, because "the path you sent is not the path that was judged" is the single
// most confusing thing about a refusal here.
type Ineligible struct {
	Rule   string
	Detail string
}

// Error makes an Ineligible usable as an error where a caller wants one. The rule token
// leads, so a log line is greppable by the same token a response body carries.
func (i *Ineligible) Error() string { return i.Rule + ": " + i.Detail }

// Eligibility is the decision, bound to one configuration: the library roots a path must
// resolve beneath and the video extensions a name must carry.
type Eligibility struct {
	roots     []config.Root
	videoExts []string
}

// NewEligibility builds the decision from a whole configuration. It resolves the roots
// once, the way New does, so the answer is taken against the same root list the engine
// judges files by.
func NewEligibility(cfg config.Config) Eligibility {
	return Eligibility{roots: cfg.RootProfiles(), videoExts: cfg.VideoExts}
}

// Eligibility is THIS engine's decision, over the roots it resolved at construction. It
// is the one a targeted submission must be judged by: any other would be a second copy.
func (e *Engine) Eligibility() Eligibility {
	return Eligibility{roots: e.roots, videoExts: e.Cfg.VideoExts}
}

// Name is the half the enumeration reads: given a file's BASENAME, is it one a scan would
// enumerate as a source? nil means yes. It opens nothing, which is what lets the scan and
// the startup walk decide an entry from a listing.
//
// The order is deliberate. A working file is reported as a working file even though it
// also carries a video extension, because that is the informative answer: a retained
// original IS a .mkv, and telling an operator their .mkv is not a .mkv would be a lie.
func (el Eligibility) Name(base string) *Ineligible {
	switch {
	case isTempName(base):
		return &Ineligible{Rule: RuleWorkingFile, Detail: fmt.Sprintf(
			"%q is a work-in-progress temp holdfast wrote, never a source", base)}
	case isUndoName(base):
		return &Ineligible{Rule: RuleWorkingFile, Detail: fmt.Sprintf(
			"%q is an original the undo window is holding, never a source", base)}
	case IsRetainedReplacementName(base):
		return &Ineligible{Rule: RuleWorkingFile, Detail: fmt.Sprintf(
			"%q is a replacement holdfast retained, never a source", base)}
	case !matchesVideoExt(base, el.videoExts):
		return &Ineligible{Rule: RuleNotAVideoFile, Detail: fmt.Sprintf(
			"%q carries no configured video extension (video_exts: %s)",
			base, strings.Join(el.videoExts, ", "))}
	}
	return nil
}

// IsSourceName reports whether a file BASENAME is one a scan would enumerate as a source:
// it carries one of the configured video extensions and is not one of this tool's own
// working files. Three things are excluded and each is a file holdfast itself wrote, none
// of which is ever anybody's source even though each is itself a *.mkv (or whatever the
// source was):
//
//   - a work-in-progress temp;
//   - an original the undo window is holding (UNDO-6). A retention area whose files were
//     enumerated would hand the encoder the very bytes the undo window is holding,
//     re-encode them, and swap the result over them - destroying the thing an operator
//     was given a window to recover;
//   - a replacement this tool RETAINED (FILESYSTEM-1's record-free hold-back: a file
//     holdfast wrote is never anybody's source, whether or not a record of it survived,
//     and where the store could not be written none did).
//
// It is the ONE definition of "a media file this run would enumerate", shared by the
// scan, by the startup walk and - through Eligibility.Judge - by a targeted submission.
// It delegates rather than repeating the rules: a caller that only needs a yes/no keeps
// the cheapest possible call site, and there is still one implementation to edit.
func IsSourceName(base string, exts []string) bool {
	return Eligibility{videoExts: exts}.Name(base) == nil
}

// IsRetentionDir reports whether a directory IS a per-directory retention area. It is the
// question the enumeration asks of each directory it is about to list, spelled once here
// so the constant has a single reader on that path.
func IsRetentionDir(dir string) bool { return filepath.Base(dir) == UndoDirName }

// InRetentionArea reports whether a path lies inside a retention area at any depth. It is
// the question a SUBMISSION has to ask, because a submission arrives with a whole path
// rather than walking into one directory at a time: the enumeration reaches the same
// answer by declining to descend, and this reaches it by looking at where the path is.
func InRetentionArea(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
		if seg == UndoDirName {
			return true
		}
	}
	return false
}

// Judge applies the WHOLE decision to a path submitted from outside, and is the only
// route by which such a path may reach the pipeline. It returns the RESOLVED path - the
// one every rule below was answered against, and therefore the one that may be processed
// - or the single rule that refused it.
//
// RESOLUTION COMES FIRST, and that ordering is the whole safety property. config.Root's
// own Contains is a LEXICAL check over filepath.Clean: it answers "/lib/../etc/passwd" as
// being under "/lib", and it cannot see that "/lib/film.mkv" is a symbolic link whose
// target is somewhere else entirely. Both are paths a caller on the network can send, and
// everything past the pipeline's door is licensed to rewrite the file it was handed. So
// the path is resolved to its real form and the root check is answered against THAT.
//
// The root check then runs before the file is even stat'd, so the refusal an out-of-root
// path gets names the rule that matters rather than an incidental property of whatever is
// at the other end.
//
// Nothing here is a gate in its own right. Every rule is one the enumeration already
// applies, asked of a path instead of a listing entry; the guards, the claim and the swap
// discipline are all still ahead, on ProcessFile.
func (el Eligibility) Judge(p string) (string, *Ineligible) {
	if rule, detail, yes := Declined(p); yes {
		return "", &Ineligible{Rule: rule, Detail: fmt.Sprintf("%q: %s", p, detail)}
	}
	if !filepath.IsAbs(p) {
		return "", &Ineligible{Rule: RuleNotAbsolute, Detail: fmt.Sprintf(
			"%q is not an absolute path; this tool only ever acts under a library root, so a "+
				"submitted path is never resolved against a working directory", p)}
	}

	resolved, bad := resolve(p)
	if bad != nil {
		return "", bad
	}

	root, rooted := el.rootFor(resolved)
	if !rooted {
		return "", &Ineligible{Rule: RuleOutsideRoots, Detail: fmt.Sprintf(
			"%s does not lie at or beneath any configured library root (%s)",
			spelling(p, resolved), el.rootList())}
	}
	// The path filters, asked of the RESOLVED path for the same reason the root check
	// is: a submission may name an excluded tree through a link out of an included one,
	// and the filter has to be answered against the path that would actually be
	// rewritten. A scan reaches this answer by never offering the file.
	if !root.Offers(resolved) {
		return "", &Ineligible{Rule: RuleFilteredPath, Detail: fmt.Sprintf(
			"%s is kept out of this run by the path filters in force for library root %s "+
				"(exclude_paths: %s; include_paths: %s)",
			spelling(p, resolved), root.Clean,
			patternList(root.Filters.Exclude), patternList(root.Filters.Include))}
	}
	if InRetentionArea(resolved) {
		return "", &Ineligible{Rule: RuleRetentionArea, Detail: fmt.Sprintf(
			"%s is inside a %s retention area, which holds originals the undo window is keeping",
			spelling(p, resolved), UndoDirName)}
	}
	if err := regularFile(resolved); err != nil {
		return "", err
	}
	if bad := el.Name(filepath.Base(resolved)); bad != nil {
		if resolved != p {
			bad.Detail = fmt.Sprintf("%s resolves to %s, and %s", p, resolved, bad.Detail)
		}
		return "", bad
	}
	return resolved, nil
}

// rootFor returns the configured root a RESOLVED path lies at or beneath, and whether
// there is one. Nested roots are refused by config.Validate, so at most one root can
// contain a path and this needs no precedence rule.
//
// It compares against each root's cleaned spelling and nothing else, deliberately. That
// is the same spelling Engine.rootFor matches on when it decides which profile judges the
// file, so a path accepted here is a path rootFor attributes to the SAME root - and a
// submission cannot end up encoded at one root's crf, or weighed against another root's
// filters, while it was admitted under a third root's name. Where a configured root is
// itself a symbolic link the answer is a refusal, which is the conservative direction and
// is what a scan reaches too: a walk does not follow the link it is rooted at.
func (el Eligibility) rootFor(resolved string) (config.Root, bool) {
	for _, r := range el.roots {
		if r.Contains(resolved) {
			return r, true
		}
	}
	return config.Root{}, false
}

// patternList renders a filter list for a refusal message, stating an empty list as
// such: "none" and an empty pair of brackets read very differently to somebody working
// out which of their two keys refused a path.
func patternList(patterns []string) string {
	if len(patterns) == 0 {
		return "none"
	}
	return strings.Join(patterns, ", ")
}

// rootList renders the configured roots for a refusal message. An empty list is stated as
// such: "no library root is configured" is a different fault from "not under one of
// these", and an operator meeting an empty parenthesis would have to guess which.
func (el Eligibility) rootList() string {
	if len(el.roots) == 0 {
		return "no library root is configured"
	}
	out := make([]string, 0, len(el.roots))
	for _, r := range el.roots {
		out = append(out, r.Clean)
	}
	return "library_roots: " + strings.Join(out, ", ")
}

// spelling renders a path for a message, naming the resolved form beside the submitted
// one whenever resolution moved it. That is the evidence for a `..` climb and for a
// symbolic link out of the roots alike: the refusal says what the path really was, so
// nobody has to reproduce the resolution by hand to understand it.
func spelling(submitted, resolved string) string {
	if submitted == resolved {
		return submitted
	}
	return submitted + " (resolves to " + resolved + ")"
}

// resolve establishes a path's real form, and DISTINGUISHES the ways that can fail,
// because the criteria distinguish them: a file that is not there and a directory this
// process may not read are different reports to an operator and different fixes.
func resolve(p string) (string, *Ineligible) {
	real, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(real), nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", &Ineligible{Rule: RuleNotARegularFile, Detail: fmt.Sprintf(
			"%s does not exist (or is a symbolic link with nothing at the other end)", p)}
	case errors.Is(err, fs.ErrPermission):
		return "", &Ineligible{Rule: RuleUnresolvable, Detail: fmt.Sprintf(
			"%s could not be resolved: permission denied reading a directory on the way to it", p)}
	default:
		return "", &Ineligible{Rule: RuleUnresolvable, Detail: fmt.Sprintf(
			"%s could not be resolved: %v", p, err)}
	}
}

// regularFile refuses anything that is not a plain file, naming what it is instead. The
// path handed in is already resolved, so there is no link left to follow and Lstat and
// Stat answer identically.
func regularFile(resolved string) *Ineligible {
	fi, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Ineligible{Rule: RuleNotARegularFile, Detail: resolved + " does not exist"}
		}
		return &Ineligible{Rule: RuleUnresolvable, Detail: fmt.Sprintf(
			"%s could not be inspected: %v", resolved, err)}
	}
	if fi.Mode().IsRegular() {
		return nil
	}
	return &Ineligible{Rule: RuleNotARegularFile, Detail: fmt.Sprintf(
		"%s is %s, not a regular file", resolved, kindOf(fi.Mode()))}
}

// kindOf names what a non-regular file IS, in the words an operator would use. A refusal
// that said only "not a regular file" would leave them to work out which of half a dozen
// things they had pointed at.
func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m&fs.ModeDevice != 0:
		return "a device"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	default:
		return "not a regular file"
	}
}

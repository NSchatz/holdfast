package startup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// Kind says what a checked path IS. Every checked path is one of these three,
// and the whole set is known before any encode begins: nothing is discovered
// later in the run.
type Kind string

const (
	KindLibraryRoot Kind = "library-root"
	KindStateDir    Kind = "state-dir"
	KindMount       Kind = "mount"
	// KindScratchDir is the configured working location (scratch_dir), when one
	// is configured. It is a checked path like the others - it is inspected,
	// classified and reported before anything is encoded - but the not-local row
	// does NOT apply to it: see applyDeclarations and NoticeScratchNotLocal.
	KindScratchDir Kind = "scratch-dir"
)

// Record is one classification record: one per DISTINCT checked path, so a path
// configured twice is reported once, and every one of them is emitted before any
// encode begins, before any file under a library root is created, renamed or
// removed, and before anything is created in or under the state directory.
type Record struct {
	Kind Kind
	// Path is the path as configured, or - for a mount the walk found - as the
	// walk reached it, which is the spelling a declaration must use.
	Path string
	// Resolved is the resolved form of Path, empty when it could not be
	// established. A checked path whose own resolved form is unestablished is
	// one no declaration can be compared against, so it is never classified
	// local: see resolveInto.
	Resolved string
	// Class is the storage classification of this path, or Unclassified where
	// there is no storage to classify - which is the one case of a configured
	// library root that does not exist. A missing root is reported through
	// Missing and never under the word `undetermined`.
	Class Class
	// Type names the filesystem matched, on a local or non-local record.
	Type string
	// Reason says why an undetermined record is undetermined, so a report never
	// names a filesystem it did not detect.
	Reason string
	// Missing marks a configured library root that does not exist. It is
	// distinct from permission denial and from an undetermined type, and such a
	// record is never classified local.
	Missing bool
	// Denied marks a path the process may not inspect. No opt-in lifts it.
	Denied bool
	// Covered marks a not-local checked path that an opt-in covers: the run
	// proceeds there with a REDUCED no-loss guarantee, and says so.
	Covered bool
}

// NoticeKind is one of the things startup reports that never decide whether the
// run proceeds. Every one of them is a report, a classification or a constraint;
// none is a sixth row of the decision.
type NoticeKind string

const (
	// NoticeLinkLeavesRoots: a symbolic link whose resolved target is beneath no
	// configured library root. Not descended, and no source under it is
	// enumerated, encoded or swapped.
	NoticeLinkLeavesRoots NoticeKind = "symlink-leaves-the-library-roots"
	// NoticeRegionWalked: this path exposes a directory of storage the walk
	// already entered earlier in this run. Not descended, and nothing is
	// enumerated from THIS spelling - the sources are enumerated exactly once,
	// under the spelling the walk reached first.
	NoticeRegionWalked NoticeKind = "storage-already-walked-under-another-path"
	// NoticeUnreadable: the process may not list or enter this directory.
	NoticeUnreadable NoticeKind = "directory-could-not-be-read"
	// NoticeListingFailed: listing or entering this directory failed for some
	// other reason (an I/O error, or another program moving it mid-walk).
	NoticeListingFailed NoticeKind = "directory-could-not-be-traversed"
	// NoticeUnresolvable: a path whose resolved form could not be established.
	NoticeUnresolvable NoticeKind = "path-could-not-be-resolved"
	// NoticeEmptyRoot: a library root that is present, local and holds no media
	// file. Not an error, and not a reason to refuse.
	NoticeEmptyRoot NoticeKind = "library-root-present-and-empty"
	// NoticeUnnecessary: a well-formed declaration that covers nothing, because
	// the path it names is local, is not a checked path at all, or is a
	// configured library root that does not exist. Reported rather than passed
	// over in silence, which is what keeps a typo beneath a root - and a stale
	// line naming a root that has since vanished - visible. The detail always
	// says WHICH of those it is, so the report never invites the operator to
	// delete a line when the real remedy is elsewhere.
	NoticeUnnecessary NoticeKind = "declaration-unnecessary"
	// NoticeReducedGuarantee: a checked path that is not local and that an
	// opt-in covers. The run proceeds there and the no-loss guarantee is reduced.
	NoticeReducedGuarantee NoticeKind = "reduced-guarantee"
	// NoticeScratchNotLocal: the configured scratch directory is on storage this
	// build could not positively identify as local. The run STARTS, no
	// declaration is required and none can be written for it, and the no-loss
	// guarantee is not reduced - nothing irreversible happens in that directory
	// and the swap still runs beside the source on storage the checks above have
	// already adjudicated. It is reported because "the encode is running over
	// NFS" explains a throughput complaint an operator would otherwise chase for
	// a week.
	NoticeScratchNotLocal NoticeKind = "scratch-dir-storage-is-not-local"
)

// Notice is one report that does not decide the run.
type Notice struct {
	Kind   NoticeKind
	Path   string
	Detail string
}

// CauseKind is a ground for refusing the run.
type CauseKind string

const (
	CauseMalformed   CauseKind = "malformed-declaration"
	CauseMissingRoot CauseKind = "library-root-does-not-exist"
	CauseDenied      CauseKind = "permission-denied"
	CauseUnlistable  CauseKind = "library-root-could-not-be-listed"
	CauseNotLocal    CauseKind = "storage-is-not-local"

	// The five grounds on which a configured scratch directory refuses the run.
	// They are their own row (scratchRow) rather than being folded into the rows
	// above, because the questions are different ones: a scratch directory is
	// holdfast's working area, and whether it exists, is a directory, is writable,
	// has room and is clear of the library are five things no library root is ever
	// asked. Storage that is not local is deliberately NOT among them (D6).
	CauseScratchMissing    CauseKind = "scratch-dir-does-not-exist"
	CauseScratchNotDir     CauseKind = "scratch-dir-is-not-a-directory"
	CauseScratchDenied     CauseKind = "scratch-dir-could-not-be-inspected"
	CauseScratchUnwritable CauseKind = "scratch-dir-is-not-writable"
	CauseScratchLowSpace   CauseKind = "scratch-dir-free-space-is-below-the-floor"
	CauseScratchOverlaps   CauseKind = "scratch-dir-overlaps-a-library-root"
)

// The rows of the ordered decision that can REFUSE, and the row that starts. They
// are named rather than spelled as literals because the count appears in the
// operator-facing refusal ("decided at check N of M") and a row appended without
// moving that number would print a lie.
const (
	rowMalformed  = 1
	rowMissing    = 2
	rowUninspect  = 3
	rowNotLocal   = 4
	rowScratch    = 5
	rowStart      = 6
	refusalRows   = rowStart - 1
	gibibyteBytes = 1 << 30
)

// Cause is one established ground for refusing. The decision stops at the first
// ROW that applies, but the report is not narrowed to it: every cause already
// established is named, so an operator fixes everything in one pass instead of
// paying the startup traversal once per problem.
type Cause struct {
	Kind CauseKind
	Row  int
	Path string
	// Detail is the cause AS OBSERVED - the classification detected, or the
	// exact failure a listing returned.
	Detail string
	// Declaration is the exact declaration that would permit the run, empty
	// where no declaration could lift the refusal.
	Declaration string
	// Remedy is what to do instead, set exactly where Declaration is empty.
	Remedy string
}

// Result is everything one startup check established.
type Result struct {
	// Records is one classification record per distinct checked path.
	Records []Record
	// Notices are the reports that never decide the run.
	Notices []Notice
	// Causes are every established ground for refusal, whichever row decided.
	Causes []Cause
	// Row is the row of the ordered decision that decided: 1 a malformed
	// declaration, 2 a missing library root, 3 a path that cannot be inspected
	// or a root that cannot be listed, 4 storage that is not local and
	// uncovered, 5 a configured scratch directory this run cannot use, 6 the run
	// starts.
	Row int
	// Start is true exactly when Row is rowStart.
	Start bool
	// Coverage is every directory the walk traversed SUCCESSFULLY, in walk
	// order. It BOUNDS the run: a source may be enumerated only from one of
	// these directories, so a subtree that was declined, could not be read or
	// failed to traverse contributes nothing to this run.
	//
	// It is NEVER nil - Run guarantees a non-nil slice even where the walk
	// traversed nothing at all. That is load-bearing rather than tidy: the
	// engine reads a nil Coverage as "no startup check ran, walk the roots
	// recursively", so a walk that covered nothing handing back a nil slice
	// would silently UN-BOUND the scan at exactly the moment the bound matters
	// most. An empty non-nil slice says what it means: nothing may be
	// enumerated in this run.
	Coverage []string
	// Entries is what each of those listings RETURNED, keyed by the path
	// Coverage names the directory by, in name order and unfiltered: the walk
	// already read every entry, so the scan that follows can decide which of
	// them are sources and which are this tool's own temp files without listing
	// the directory a second time.
	//
	// A directory is here exactly when it is in Coverage. One whose listing
	// failed, one the walk declined and one it never reached are absent from
	// both, and a directory that listed EMPTY carries an empty slice rather than
	// no key at all - the two say different things, and only the first is
	// evidence that holdfast looked.
	//
	// A consumer that finds no key for a directory must list it for itself: this
	// is what the walk SAW, never a licence to conclude anything about a
	// directory nobody listed.
	Entries map[string][]Entry
	// LocalSet is the complete set of filesystem types this build classifies
	// local.
	LocalSet []string
	// ScratchProbed records whether the scratch directory's writability probe
	// actually ran - which is the only moment this check creates anything at all,
	// and then only a zero-length file it removes again. It is on the Result so
	// the operator-facing refusal can say so rather than print an unqualified
	// "nothing was created" that would be a shade less than true.
	ScratchProbed bool
}

// Check is one startup check: the configuration it reads and the platform it
// reads it through.
type Check struct {
	// Roots are the configured library roots, in the order the configuration
	// declares them. The order is part of the contract: it fixes which spelling
	// the walk reaches storage by when more than one root exposes it, so two
	// starts over an unchanged layout decide identically.
	Roots []string
	// StateDir is the state directory as the absolute path the configuration
	// interpretation produces, never the configured string.
	StateDir string
	// Declarations are the operator's opt-ins, as declared.
	Declarations []string
	// IsMediaFile reports whether a file BASENAME is one this run would
	// enumerate as a source. Deciding it needs no read of any file, which is why
	// the walk's cost is bounded by the directory tree and not by the library.
	IsMediaFile func(base string) bool
	// ScratchDir is the configured working location, or "" when the encoder
	// writes beside the source. It is checked, classified and reported, and it
	// is never walked: it is not a library and nothing is enumerated from it.
	ScratchDir string
	// ScratchMinFreeGB is the free-space floor, in GiB, the scratch directory's
	// filesystem must clear. 0 disables the floor.
	ScratchMinFreeGB int
	// Platform is the substitutable view of the host.
	Platform Platform
}

// Run takes the whole check and returns everything it established, including the
// single start-or-refuse decision. It creates nothing, removes nothing, renames
// nothing, and opens no media file.
func Run(c Check) Result {
	r := &checkRun{
		c: c,
		// Coverage starts non-nil and stays non-nil: see Result.Coverage. A
		// walk that traverses nothing must hand back an EMPTY bound, never the
		// absent bound that means "walk everything".
		res:         Result{LocalSet: LocalTypes(), Coverage: []string{}, Entries: map[string][]Entry{}},
		byResolved:  map[string]int{},
		byPath:      map[string]int{},
		entered:     map[Region]regionEntry{},
		coveredSet:  map[string]bool{},
		regionMedia: map[Region]bool{},
		regionKids:  map[Region][]Region{},
		declined:    map[string]bool{},
	}
	r.run()
	return r.res
}

type checkRun struct {
	c   Check
	res Result

	roots         []string // cleaned, in configuration order
	resolvedRoots []string // resolved form of each root, empty where unestablished
	rootsResolved bool
	// mountInfoNoted keeps the "mount information could not be read" report to
	// one line rather than one per directory.
	mountInfoNoted bool

	byResolved map[string]int // resolved form -> index into res.Records
	byPath     map[string]int // path -> index into res.Records

	// walk state
	entered     map[Region]regionEntry // region -> what the walk observed entering it
	coveredSet  map[string]bool
	regionMedia map[Region]bool     // this region's own directory holds a media file
	regionKids  map[Region][]Region // regions of the subdirectories of a region
	declined    map[string]bool     // configured roots the region rule declined
}

func (r *checkRun) run() {
	for _, root := range r.c.Roots {
		r.roots = append(r.roots, cleanPath(root))
	}

	// (1) The declarations, first and lexically. The check is syntactic: it is
	// decided from the declared text and the configured root text alone, so it
	// touches no storage, cannot fail, and does not depend on anything the walk
	// finds.
	wellFormed := r.checkDeclarations()

	// (2) The roots and the state directory, classified before anything is
	// walked and long before anything is opened.
	rootInfo := make([]*Info, len(r.roots))
	for i, root := range r.roots {
		rootInfo[i] = r.inspectRoot(root)
	}
	r.inspectStateDir()

	// (3) The walk, root by root in configuration order.
	for i, root := range r.roots {
		if rootInfo[i] == nil {
			continue // missing, or not inspectable: already a cause, and nothing to walk
		}
		r.walkRoot(root, *rootInfo[i])
	}

	// (4) Present-and-empty is a property of the walk, and needs no read of any
	// file: a root that was listed and holds no media file anywhere beneath it.
	r.reportEmptyRoots(rootInfo)

	// (5) Coverage of the checked paths by the declarations, which is the
	// not-local row's input.
	r.applyDeclarations(wellFormed)

	// (6) The configured working location, if there is one. It runs AFTER the
	// roots are known, because two of its five questions are about them: whether
	// it overlaps one, and the fact that a scratch directory is never walked or
	// enumerated the way a root is.
	r.checkScratch()

	// (7) The one decision.
	r.decide()
}

// checkDeclarations applies the syntactic malformedness test and returns the
// declarations that passed it, in declaration order. A declaration is well
// formed when it names a configured library root, the state directory, or a path
// LEXICALLY beneath a configured library root - spelled beneath one of the roots
// AS CONFIGURED. Existence is no part of the test.
func (r *checkRun) checkDeclarations() []string {
	var ok []string
	for _, d := range r.c.Declarations {
		text := strings.TrimSpace(d)
		if text == "" || !filepath.IsAbs(text) {
			r.refuse(Cause{Kind: CauseMalformed, Row: rowMalformed, Path: d,
				Detail: "a declaration must be an absolute path",
				Remedy: r.wellFormedHint()})
			continue
		}
		clean := cleanPath(text)
		if clean == cleanPath(r.c.StateDir) {
			ok = append(ok, text)
			continue
		}
		beneath := false
		for _, root := range r.roots {
			if clean == root || lexicallyBeneath(clean, root) {
				beneath = true
				break
			}
		}
		if !beneath {
			r.refuse(Cause{Kind: CauseMalformed, Row: rowMalformed, Path: d,
				Detail: "names neither a configured library root, nor the state directory, nor a path beneath a configured library root",
				Remedy: r.wellFormedHint()})
			continue
		}
		ok = append(ok, text)
	}
	return ok
}

func (r *checkRun) wellFormedHint() string {
	return fmt.Sprintf("a well-formed declaration names a path spelled beneath one of the configured library roots %v as configured, or the state directory %s",
		r.roots, r.c.StateDir)
}

// inspectRoot establishes a configured library root and classifies it, returning
// its Info when the walk may go on to descend it.
func (r *checkRun) inspectRoot(root string) *Info {
	info, err := r.c.Platform.Inspect(root)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// A root that does not exist is MISSING, distinctly from permission
			// denial and from an undetermined type, and is never local. It
			// carries NO classification at all rather than `undetermined`: the
			// lookup did not fail to determine a type, there is no storage there
			// to have one, and the two read identically to an operator if both
			// print the same word. Row 2 refuses it, and no declaration can
			// cover a path with no storage (see applyDeclarations).
			r.addRecord(Record{Kind: KindLibraryRoot, Path: root, Resolved: r.resolvedOrEmpty(root),
				Class: Unclassified, Missing: true, Reason: "the path does not exist"})
			r.refuse(Cause{Kind: CauseMissingRoot, Row: rowMissing, Path: root,
				Detail: "the configured library root does not exist",
				Remedy: "create it, mount it, or point library_roots at storage that exists"})
		case errors.Is(err, fs.ErrPermission):
			// The platform will not say whether it exists BECAUSE the process
			// may not inspect it or its parent. That is permission denial and
			// never a missing path, and no declaration lifts it.
			r.addRecord(Record{Kind: KindLibraryRoot, Path: root, Class: Undetermined, Denied: true,
				Reason: "the process is not permitted to inspect this path"})
			r.refuse(deniedCause(root))
		default:
			// Inspection failed for some other reason, so the type is
			// undetermined and row 4 governs. The walk never reached it, so it
			// is reported as one this run did not traverse and no source is
			// taken from it.
			r.addRecord(Record{Kind: KindLibraryRoot, Path: root, Resolved: r.resolvedOrEmpty(root),
				Class: Undetermined, Reason: fmt.Sprintf("the path could not be inspected: %v", err)})
			r.notice(NoticeListingFailed, root, fmt.Sprintf("not traversed: %v", err))
		}
		return nil
	}
	rec := r.classifyPath(KindLibraryRoot, root)
	if rec.Denied {
		return nil
	}
	return &info
}

// inspectStateDir classifies the state directory. Where it does not yet exist it
// is classified by THE STORAGE IT WOULD BE CREATED ON - the longest existing
// prefix of its resolved form - and is never created in order to classify it. Its
// non-existence makes it neither a missing path nor undetermined: a fresh
// install starts.
func (r *checkRun) inspectStateDir() {
	dir := cleanPath(r.c.StateDir)
	_, err := r.c.Platform.Inspect(dir)
	switch {
	case err == nil:
		r.classifyPath(KindStateDir, dir)
	case errors.Is(err, fs.ErrPermission):
		r.addRecord(Record{Kind: KindStateDir, Path: dir, Class: Undetermined, Denied: true,
			Reason: "the process is not permitted to inspect this path"})
		r.refuse(deniedCause(dir))
	case errors.Is(err, fs.ErrNotExist):
		// Classify the storage it WOULD be created on. The absent components
		// contribute nothing, and nothing is created here.
		anchor, aerr := r.longestExisting(dir)
		if aerr != nil {
			if errors.Is(aerr, fs.ErrPermission) {
				r.addRecord(Record{Kind: KindStateDir, Path: dir, Class: Undetermined, Denied: true,
					Reason: "the process is not permitted to inspect the nearest existing ancestor of this path"})
				r.refuse(deniedCause(dir))
				return
			}
			r.addRecord(Record{Kind: KindStateDir, Path: dir, Class: Undetermined,
				Reason: fmt.Sprintf("the nearest existing ancestor could not be inspected: %v", aerr)})
			return
		}
		cl := r.classifyStorageOf(anchor)
		rec := Record{Kind: KindStateDir, Path: dir,
			Class: cl.Class, Type: cl.Type, Reason: cl.Reason, Denied: cl.Denied}
		r.resolveInto(&rec)
		r.addRecord(rec)
		if rec.Denied {
			r.refuse(deniedCause(dir))
		}
	default:
		r.addRecord(Record{Kind: KindStateDir, Path: dir, Resolved: r.resolvedOrEmpty(dir),
			Class: Undetermined, Reason: fmt.Sprintf("the path could not be inspected: %v", err)})
	}
}

// classifyPath classifies an existing checked path and records it.
func (r *checkRun) classifyPath(kind Kind, path string) Record {
	rec := r.newRecord(kind, path)
	if rec.Denied {
		r.refuse(deniedCause(path))
	}
	r.addRecord(rec)
	return rec
}

// newRecord builds the classification record for one EXISTING checked path: its
// storage classification and its resolved form together, because the two are not
// independent. It refuses nothing itself; the caller decides.
func (r *checkRun) newRecord(kind Kind, path string) Record {
	cl := r.classifyStorageOf(path)
	rec := Record{Kind: kind, Path: path,
		Class: cl.Class, Type: cl.Type, Reason: cl.Reason, Denied: cl.Denied}
	r.resolveInto(&rec)
	return rec
}

// resolveInto establishes a checked path's resolved form and applies the rule
// that governs a failure to establish it.
//
// A checked path whose OWN resolved form cannot be established is one no
// declaration can be compared against - the comparison that decides coverage is
// in resolved form on both sides - so the run must be refused rather than
// started: at the permission row where the cause is that the process may not
// inspect the path or its parent, and OTHERWISE by classifying it
// `undetermined`, so that the not-local row refuses.
//
// The type lookup answering is not a reprieve. Only a POSITIVE identification
// counts as local, and "this path is on ext4" is not one when holdfast cannot
// say which path it is: the whole coverage question - is this the path the
// operator declared? is this the same path as that record? - is unanswerable,
// and starting anyway is the fail-open direction in a check whose entire thesis
// is that it does not take one.
func (r *checkRun) resolveInto(rec *Record) {
	got, err := r.resolvedForm(rec.Path)
	if err == nil {
		rec.Resolved = got
		return
	}
	rec.Resolved = ""
	rec.Type = ""
	rec.Class = Undetermined
	if errors.Is(err, fs.ErrPermission) {
		rec.Denied = true
		rec.Reason = "the resolved form of this path could not be established: the process is not permitted to inspect it or its parent"
		return
	}
	rec.Reason = fmt.Sprintf("the resolved form of this path could not be established: %v", err)
}

func (r *checkRun) classifyStorageOf(path string) classification {
	t, err := r.c.Platform.FSType(path)
	return classify(t, err)
}

func deniedCause(path string) Cause {
	return Cause{Kind: CauseDenied, Row: rowUninspect, Path: path,
		Detail: "the process could not inspect this path",
		Remedy: "grant the process permission to read it, or point the setting at storage holdfast can inspect"}
}

// addRecord adds a classification record, deduplicated in RESOLVED FORM: a path
// configured twice, or reached by two spellings that resolve to one path, is
// reported once, under the first spelling that reached it.
func (r *checkRun) addRecord(rec Record) {
	if rec.Resolved != "" {
		if _, dup := r.byResolved[rec.Resolved]; dup {
			return
		}
	} else if _, dup := r.byPath[rec.Path]; dup {
		return
	}
	r.res.Records = append(r.res.Records, rec)
	i := len(r.res.Records) - 1
	if rec.Resolved != "" {
		r.byResolved[rec.Resolved] = i
	}
	r.byPath[rec.Path] = i
}

func (r *checkRun) notice(k NoticeKind, path, detail string) {
	r.res.Notices = append(r.res.Notices, Notice{Kind: k, Path: path, Detail: detail})
}

func (r *checkRun) refuse(c Cause) { r.res.Causes = append(r.res.Causes, c) }

// resolvedForm implements Definitions/Resolved form, and implements it GENERALLY:
// where the tail does not exist, the resolved form is that of the LONGEST PREFIX
// which does exist with the absent components re-attached unchanged. Every site
// that compares two paths reads this one, on both sides.
func (r *checkRun) resolvedForm(p string) (string, error) {
	cur := cleanPath(p)
	rest := ""
	for {
		got, err := r.c.Platform.Resolve(cur)
		if err == nil {
			if rest == "" {
				return cleanPath(got), nil
			}
			return cleanPath(filepath.Join(got, rest)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// Failed or denied: the resolved form is UNESTABLISHED, which is not
			// the same as absent.
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func (r *checkRun) resolvedOrEmpty(p string) string {
	got, err := r.resolvedForm(p)
	if err != nil {
		return ""
	}
	return got
}

// longestExisting returns the longest existing prefix of p's resolved form: the
// storage a not-yet-existing path would be created on.
func (r *checkRun) longestExisting(p string) (string, error) {
	cur := cleanPath(p)
	for {
		if _, err := r.c.Platform.Inspect(cur); err == nil {
			return cur, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur, nil
		}
		cur = parent
	}
}

// applyDeclarations decides, in resolved form on BOTH sides, which checked paths
// each well-formed declaration covers, and reports the declarations that cover
// nothing rather than passing over them in silence.
func (r *checkRun) applyDeclarations(decls []string) {
	for _, d := range decls {
		res, err := r.resolvedForm(d)
		if err != nil {
			// A declaration whose resolved form cannot be established covers NO
			// checked path, and says so rather than being read as covering one.
			r.notice(NoticeUnresolvable, d, fmt.Sprintf("this declaration could not be resolved and therefore covers nothing: %v", err))
			continue
		}
		matched, denied, missing, useful := false, false, false, false
		for i := range r.res.Records {
			rec := &r.res.Records[i]
			if rec.Resolved == "" || rec.Resolved != res {
				continue
			}
			matched = true
			if rec.Denied {
				denied = true
				continue
			}
			if rec.Missing {
				// A configured library root that does not exist has no storage,
				// so there is nothing here for a declaration to permit a run on.
				// Marking it covered would consume a stale opt-in as USEFUL and
				// report it nowhere, which is exactly what keeping a declaration
				// that covers nothing visible exists to prevent.
				missing = true
				continue
			}
			if !rec.Class.IsLocal() {
				rec.Covered = true
				useful = true
			}
		}
		if useful {
			continue
		}
		// A declaration that covered nothing is reported, but the REASON has to
		// be the true one. A checked path whose resolved form could not be
		// established is not a path where the walk "found no mount": no
		// declaration can be compared against it (AC10a) and none can lift it,
		// so calling it unnecessary would tell the operator to delete the line
		// when the remedy is to grant permission or move the setting.
		unresolvable := false
		if !matched {
			if rec, ok := r.unresolvableRecord(cleanPath(d)); ok {
				unresolvable, denied = true, rec.Denied
			}
		}
		switch {
		case denied:
			r.notice(NoticeUnresolvable, d, "this declaration covers nothing: holdfast cannot inspect that path, and no declaration permits a run on a path it cannot inspect")
		case unresolvable:
			r.notice(NoticeUnresolvable, d, "this declaration covers nothing: the resolved form of that checked path could not be established, so no declaration can be compared against it")
		case missing:
			r.notice(NoticeUnnecessary, d, "this declaration covers nothing: the configured library root it names does not exist, so there is no storage there to classify or to permit")
		case matched:
			r.notice(NoticeUnnecessary, d, "this path is on storage classified local")
		default:
			r.notice(NoticeUnnecessary, d, "the startup walk found no distinct mounted filesystem at this path")
		}
	}

	for i := range r.res.Records {
		rec := &r.res.Records[i]
		if rec.Missing || rec.Class.IsLocal() {
			continue
		}
		// The scratch directory is exempt from the not-local row, and it is the
		// only checked path that is. The declaration exists because holdfast's
		// no-loss contract needs local rename semantics WHERE THE IRREVERSIBLE ACT
		// HAPPENS, and no irreversible act happens in the working area: the
		// encode's working file is disposable by construction, and the swap still
		// runs beside the source on storage the roots' own check adjudicated.
		// Refusing here would be a gate that protects nothing while training an
		// operator to add declarations - and it could not even be lifted, since a
		// path outside every library root is not one a well-formed declaration may
		// name. It is REPORTED instead (checkScratch), because what it explains is
		// real.
		if rec.Kind == KindScratchDir {
			continue
		}
		if rec.Covered {
			r.notice(NoticeReducedGuarantee, rec.Path,
				fmt.Sprintf("classified %s: an opt-in covers it, and holdfast's no-loss guarantee is REDUCED here", rec.describeClass()))
			continue
		}
		if rec.Denied {
			continue // permission denial is row 3's, and no declaration lifts it
		}
		cause := Cause{Kind: CauseNotLocal, Row: rowNotLocal, Path: rec.Path, Detail: rec.describeClass()}
		if rec.Resolved == "" {
			cause.Remedy = "point the setting at storage holdfast can resolve, or repair the storage"
		} else {
			cause.Declaration = rec.Path
		}
		r.refuse(cause)
	}
}

// checkScratch establishes the configured working location, in a fixed order, and
// refuses the run at row 5 for any of the five things that make it unusable:
// it does not exist, it is not a directory, it cannot be inspected, it overlaps a
// library root, it is already below the configured free-space floor, or this
// process cannot create and remove a file in it.
//
// THE ORDER IS PART OF THE CONTRACT, because one of these questions writes.
// Existence, kind, resolution, overlap and free space are all READS, and each of
// them returns immediately on a refusal - so a run refused for any of those
// reasons has created nothing, anywhere, which is what a startup refusal owes. The
// writability probe is asked LAST and only when every read has passed; when it
// fails, the creation itself is what failed, so nothing was created then either.
// The one case in which a probe file exists at all is the case where the scratch
// directory is fine, and the probe removes it. WriteRefusal states that outright
// rather than leaving an operator to infer it.
//
// Storage that is NOT LOCAL is deliberately not on the list. It is classified,
// recorded and reported like every other checked path, and the run starts.
func (r *checkRun) checkScratch() {
	dir := strings.TrimSpace(r.c.ScratchDir)
	if dir == "" {
		return
	}
	clean := cleanPath(dir)

	info, err := r.c.Platform.Inspect(clean)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.addRecord(Record{Kind: KindScratchDir, Path: clean, Resolved: r.resolvedOrEmpty(clean),
			Class: Unclassified, Missing: true, Reason: "the path does not exist"})
		r.refuse(Cause{Kind: CauseScratchMissing, Row: rowScratch, Path: clean,
			Detail: "the configured scratch_dir does not exist",
			Remedy: "create it, mount it, point scratch_dir at a directory that exists, or unset scratch_dir to encode beside the source"})
		return
	case errors.Is(err, fs.ErrPermission):
		r.addRecord(Record{Kind: KindScratchDir, Path: clean, Class: Undetermined, Denied: true,
			Reason: "the process is not permitted to inspect this path"})
		r.refuse(Cause{Kind: CauseScratchDenied, Row: rowScratch, Path: clean,
			Detail: "the process could not inspect the configured scratch_dir",
			Remedy: "grant the process permission to read it, point scratch_dir at storage holdfast can inspect, or unset scratch_dir"})
		return
	case err != nil:
		r.addRecord(Record{Kind: KindScratchDir, Path: clean, Resolved: r.resolvedOrEmpty(clean),
			Class: Undetermined, Reason: fmt.Sprintf("the path could not be inspected: %v", err)})
		r.refuse(Cause{Kind: CauseScratchDenied, Row: rowScratch, Path: clean,
			Detail: fmt.Sprintf("the configured scratch_dir could not be inspected: %v", err),
			Remedy: "repair the storage, point scratch_dir at storage holdfast can inspect, or unset scratch_dir"})
		return
	case !info.IsDir:
		r.addRecord(Record{Kind: KindScratchDir, Path: clean, Resolved: r.resolvedOrEmpty(clean),
			Class: Unclassified, Reason: "the path exists but is not a directory"})
		r.refuse(Cause{Kind: CauseScratchNotDir, Row: rowScratch, Path: clean,
			Detail: "the configured scratch_dir exists but is not a directory",
			Remedy: "point scratch_dir at a directory, or unset scratch_dir to encode beside the source"})
		return
	}

	rec := r.newRecord(KindScratchDir, clean)
	r.addRecord(rec)
	if rec.Denied {
		r.refuse(Cause{Kind: CauseScratchDenied, Row: rowScratch, Path: clean,
			Detail: rec.Reason,
			Remedy: "grant the process permission to read it, point scratch_dir at storage holdfast can inspect, or unset scratch_dir"})
		return
	}

	// Overlap with a library root, compared in RESOLVED FORM on both sides, as
	// every other path comparison in this package is - a symbolic link into the
	// library is the whole reason a lexical comparison is not enough here.
	//
	// A working area inside the tree the walk classifies and the scan enumerates
	// delivers none of the four things a scratch location is for (it is the same
	// storage), and it puts holdfast's own working files where its own sweep,
	// hold-backs and collision guards have to reason about them twice.
	if r.refuseScratchOverlap(rec) {
		return
	}

	if floor := r.c.ScratchMinFreeGB; floor > 0 {
		free, ferr := r.c.Platform.FreeBytes(clean)
		if ferr != nil {
			r.refuse(Cause{Kind: CauseScratchDenied, Row: rowScratch, Path: clean,
				Detail: fmt.Sprintf("the free space on the filesystem holding the configured scratch_dir could not be established: %v", ferr),
				Remedy: "repair the storage, point scratch_dir at storage holdfast can inspect, or unset scratch_dir"})
			return
		}
		want := uint64(floor) * gibibyteBytes
		if free < want {
			r.refuse(Cause{Kind: CauseScratchLowSpace, Row: rowScratch, Path: clean,
				Detail: fmt.Sprintf("%d byte(s) free, and scratch_min_free_gb requires at least %d GiB (%d byte(s))",
					free, floor, want),
				Remedy: "free space on that filesystem, point scratch_dir at a larger device, lower scratch_min_free_gb " +
					"(0 disables the floor), or unset scratch_dir to encode beside the source"})
			return
		}
	}

	// LAST, and the only thing this package writes. See the doc comment.
	r.res.ScratchProbed = true
	if werr := r.c.Platform.ProbeWritable(clean); werr != nil {
		r.refuse(Cause{Kind: CauseScratchUnwritable, Row: rowScratch, Path: clean,
			Detail: fmt.Sprintf("this process cannot create and remove a file in the configured scratch_dir: %v", werr),
			Remedy: "grant the process write permission there (holdfast writes every encode's working file into it), " +
				"point scratch_dir at a writable directory, or unset scratch_dir to encode beside the source"})
		return
	}

	if !rec.Class.IsLocal() {
		r.notice(NoticeScratchNotLocal, clean, fmt.Sprintf(
			"classified %s. The run STARTS: nothing irreversible happens in the working area - the encode's working "+
				"file is disposable, and the swap still happens beside the source on storage the checks above adjudicated - "+
				"so no %s declaration is required here, and none can be written for a path outside every library root. "+
				"It is reported because it is the answer to why encoding is slow.",
			rec.describeClass(), ConfigKey))
	}
}

// refuseScratchOverlap refuses a scratch directory that is a library root, is
// beneath one, or has one beneath it, and reports whether it did. Both directions
// are refused because both are the same mistake: the working area and the library
// must not be the same tree.
func (r *checkRun) refuseScratchOverlap(rec Record) bool {
	scratch := rec.Resolved
	if scratch == "" {
		scratch = rec.Path
	}
	for i, root := range r.roots {
		resolved := ""
		if r.rootsResolved && i < len(r.resolvedRoots) {
			resolved = r.resolvedRoots[i]
		}
		if resolved == "" {
			resolved = r.resolvedOrEmpty(root)
		}
		if resolved == "" {
			resolved = root
		}
		var how string
		switch {
		case scratch == resolved:
			how = "is the configured library root"
		case lexicallyBeneath(scratch, resolved):
			how = "is beneath the configured library root"
		case lexicallyBeneath(resolved, scratch):
			how = "contains the configured library root"
		default:
			continue
		}
		r.refuse(Cause{Kind: CauseScratchOverlaps, Row: rowScratch, Path: rec.Path,
			Detail: fmt.Sprintf("the configured scratch_dir (resolved %s) %s %s (resolved %s), "+
				"so holdfast's working area would sit inside the tree it scans - on the same storage, "+
				"delivering none of what a scratch location is for", scratch, how, root, resolved),
			Remedy: "point scratch_dir at a directory outside every library root, or unset scratch_dir to encode beside the source"})
		return true
	}
	return false
}

// unresolvableRecord finds a checked path whose RESOLVED FORM could not be
// established, by the text it was recorded under. Such a path is the one case a
// declaration cannot be compared against in resolved form (AC10a), so the
// comparison falls back to the spelling - only to report honestly WHY the
// declaration covers nothing, never to make it cover something.
func (r *checkRun) unresolvableRecord(clean string) (Record, bool) {
	for _, rec := range r.res.Records {
		if rec.Resolved == "" && cleanPath(rec.Path) == clean {
			return rec, true
		}
	}
	return Record{}, false
}

func (rec Record) describeClass() string {
	switch {
	case rec.Missing:
		// Never "undetermined": there is no storage here to have a type.
		return "missing (the path does not exist)"
	case rec.Class == NonLocal:
		return fmt.Sprintf("non-local (%s)", rec.Type)
	case rec.Class == Local:
		return fmt.Sprintf("local (%s)", rec.Type)
	default:
		return fmt.Sprintf("undetermined (%s)", rec.Reason)
	}
}

// decide takes the ONE start-or-refuse decision, over the whole set of checked
// paths and the whole set of declarations together, in this order, stopping at
// the first row that applies to anything in it. The order is TOTAL and the list
// is CLOSED: nothing else in this package decides whether the run proceeds.
func (r *checkRun) decide() {
	sort.SliceStable(r.res.Causes, func(i, j int) bool { return r.res.Causes[i].Row < r.res.Causes[j].Row })
	for row := 1; row <= refusalRows; row++ {
		for _, c := range r.res.Causes {
			if c.Row == row {
				r.res.Row = row
				r.res.Start = false
				return
			}
		}
	}
	r.res.Row = rowStart
	r.res.Start = true
}

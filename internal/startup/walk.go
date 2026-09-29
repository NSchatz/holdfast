package startup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
)

// NoticeMountInfoUnavailable reports that the mount table could not be read, so
// the walk fell back to the mounted-filesystem identity alone. It changes what
// can be told apart (a bind mount of a directory of the SAME filesystem stops
// being a distinct record) and it changes nothing about coverage, termination or
// safety: such a directory is on the same filesystem as its parent, so it
// carries the same classification.
const NoticeMountInfoUnavailable NoticeKind = "mount-information-unavailable"

// walkRoot walks one configured library root. The region rule holds ACROSS the
// roots and a configured root is subject to it like any other directory: a root
// whose storage an earlier root's walk already entered is NOT entered again. It
// is reported as one this run did not descend, it still carries its own
// classification record, it is never reported present and empty, and declining
// it refuses nothing - declining a region is not a failure to list it. Coverage
// is unharmed and nothing is enumerated twice: every source under such a root
// was reachable through the root that WAS walked.
//
// THAT LAST SENTENCE IS THE CONDITION, not a decoration, and it is what tells
// this rule apart from AC4b's. A region is marked ON ENTRY and never on a
// listing that completes (AC2b2), so a region can be "already entered" and yet
// have been traversed nowhere: the walk met this storage, tried to list it, and
// the listing failed. Then nothing under this root was reachable through the
// root that was walked, AC2b3's whole rationale is false, and what holds instead
// is the antecedent of AC4b - a configured library root that exists, can be
// inspected, and CANNOT BE LISTED OR ENTERED, for ANY reason. So the run is
// refused at AC10b row 3 reporting the cause the walk OBSERVED, exactly as it
// would be had this root been the first spelling to reach that storage. Without
// it AC4b would be reachable or not by declaration order alone: the same two
// roots over the same layout in the other order refuse.
//
// A region PRUNED earlier in the walk was never entered, so none of the above
// applies to it either. A root exposing that storage is a spelling no exclude
// pattern reaches - a configured root is never pruned itself - so it is declined,
// refusing nothing, and is told which excluded spelling reached it first (S0168
// AC-7): walking it would offer the very files the filter keeps out under their
// other name.
func (r *checkRun) walkRoot(root string, info Info) {
	prev, entered := r.entered[info.Region]
	if !entered {
		r.descend(root, info, true)
		return
	}
	if prev.pruned {
		r.declined[root] = true
		r.notice(NoticeRegionExcluded, root, prev.describeExcluded("this configured library root"))
		return
	}
	if prev.err != nil {
		// The report says what the walk saw, and never that sources under this
		// root "are enumerated exactly once, from there": none is enumerated
		// from anywhere (AC2f1 reports the path WITH THE REASON).
		r.notice(prev.noticeKind(), root, prev.describeAsRoot(root))
		r.refuse(unlistableCause(root, prev.err, prev.denied))
		return
	}
	r.declined[root] = true
	r.notice(NoticeRegionWalked, root, fmt.Sprintf(
		"this configured library root exposes storage this run already walked under %s; every source under it is enumerated exactly once, from there", prev.path))
}

// regionEntry is what the walk observed when it ENTERED one region: the path it
// entered by, and the failure - if any - the listing there returned. A region is
// marked on entry and never on a listing that completes (AC2b2), so "entered" is
// not "traversed successfully", and this is what tells the two apart wherever a
// later exposure of that region is declined.
//
// A region can also be marked without being entered at all: pruned records that
// the walk met this storage first under a spelling an exclude pattern reaches and
// did not list it, so a later spelling of it is declined rather than descended.
type regionEntry struct {
	path   string
	err    error
	denied bool
	pruned bool
}

func (e regionEntry) noticeKind() NoticeKind {
	if e.denied {
		return NoticeUnreadable
	}
	return NoticeListingFailed
}

// describeExcluded is the report for a later spelling of pruned storage. It names
// the excluded spelling that reached it first and never says the sources are
// enumerated from there, because they are enumerated from nowhere.
func (e regionEntry) describeExcluded(what string) string {
	return fmt.Sprintf("not descended: %s exposes storage this run reached first under %s, which an "+
		"exclude_paths pattern excludes, so no source is enumerated from it under either spelling", what, e.path)
}

func (e regionEntry) describeAsRoot(root string) string {
	if e.path == root {
		return fmt.Sprintf("not traversed: this configured library root could not be listed: %v", e.err)
	}
	return fmt.Sprintf("not traversed: this configured library root exposes storage this run entered under %s, where the listing failed: %v", e.path, e.err)
}

// unlistableCause is AC4b's refusal: a configured library root that exists and
// can be inspected but cannot be listed or entered, FOR ANY CAUSE, refuses the
// run at row 3, names that root, reports the cause it observed, and names no
// declaration as a remedy. Permission denial and a listing that FAILED are
// distinct causes there, and both are distinct from a network classification, an
// undetermined type and a missing path. A root holdfast cannot list is one whose
// whole subtree is unknown to it: nothing may be enumerated from it, so starting
// would look to the operator exactly like an empty library. One level down this
// is not a refusal - the run still starts - and this rule is deliberately the
// ROOT alone.
func unlistableCause(root string, err error, denied bool) Cause {
	if denied {
		return Cause{Kind: CauseUnlistable, Row: 3, Path: root,
			Detail: fmt.Sprintf("the configured library root could not be listed: permission denied (%v)", err),
			Remedy: "grant the process permission to list it, or point library_roots at storage holdfast can list"}
	}
	return Cause{Kind: CauseUnlistable, Row: 3, Path: root,
		Detail: fmt.Sprintf("the configured library root could not be listed: %v", err),
		Remedy: "repair the storage, or point library_roots at storage holdfast can list"}
}

// descend enters one directory and walks it.
//
// TERMINATION, over every link and mount layout and not links alone: the walk
// enters each REGION at most once and the regions beneath the roots are finite,
// so it cannot descend for ever. The same rule cuts a symbolic-link cycle and an
// unbounded bind chain (mount --bind /srv/media /srv/media/loop makes
// /srv/media/loop/loop/... an infinite chain of DISTINCT resolved directories
// that link resolution does not collapse, every region of which is already
// walked). The region is marked ON ENTRY and never on a listing that completes,
// so a directory whose listing fails partway has still had its region entered
// and no failure can cost termination.
func (r *checkRun) descend(dir string, info Info, isRoot bool) {
	r.entered[info.Region] = regionEntry{path: dir}

	entries, err := r.c.Platform.ReadDir(dir)
	if err != nil {
		// A listing that fails PARTWAY is not a successful traversal: the
		// directory is reported, nothing is enumerated from it, and whatever it
		// did return is not trusted. The region STAYS entered - that is what
		// keeps the walk terminating - but the failure is recorded against it,
		// so a later exposure of the same storage is told the truth about what
		// happened here, and a configured ROOT exposing it is refused (AC4b)
		// rather than declined.
		denied := errors.Is(err, fs.ErrPermission)
		r.entered[info.Region] = regionEntry{path: dir, err: err, denied: denied}
		kind := NoticeListingFailed
		if denied {
			kind = NoticeUnreadable
		}
		r.notice(kind, dir, err.Error())
		if isRoot {
			r.refuse(unlistableCause(dir, err, denied))
		}
		return
	}
	r.coveredSet[dir] = true
	r.res.Coverage = append(r.res.Coverage, dir)

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	// The listing is kept, not merely consumed: this walk is the only pass over
	// the tree a scan gets for free, and what it read here is what the scan would
	// otherwise pay a second and a third listing for. It is the SAME slice the
	// loop below annotates - a link or a subdirectory records what following it
	// reached - so keeping it costs the walk no second copy of anything.
	r.res.Entries[dir] = entries
	for i := range entries {
		e := entries[i]
		child := filepath.Join(dir, e.Name)

		if e.IsLink {
			// A symbolic link is met only because this is a DIRECTORY
			// TRAVERSAL: a startup that read the mount table alone would never
			// see one. Descend it only where its resolved target is
			// RESOLVED-beneath a configured library root - the resolved test
			// applied to the root as well as to the target, so under a
			// symlinked root an interior link onto another directory of that
			// same library is descended.
			target, terr := r.resolvedForm(child)
			if terr != nil {
				r.notice(NoticeUnresolvable, child, fmt.Sprintf(
					"not descended: the resolved target could not be established: %v", terr))
				continue
			}
			if !r.resolvedWithinRoots(target) {
				// The rule is about a link the walk would DESCEND THROUGH, and
				// its bar is on sources UNDER a declined link. A symbolic link
				// that resolves to a FILE has nothing under it and is descended
				// into by nobody: it is an ordinary entry of a directory this
				// walk DID traverse successfully, and the scan enumerates it
				// from there by name like any other. Reporting it as one this
				// run "does not descend into" would make startup's report and
				// the scan contradict each other about the same path - the
				// operator is told the file is out of bounds and a job row
				// appears for it - so the report is raised only for a link the
				// walk would otherwise have entered.
				ti, terr := r.c.Platform.Inspect(child)
				if terr == nil && !ti.IsDir {
					if r.isMedia(e.Name) {
						r.regionMedia[info.Region] = true
					}
					continue
				}
				if terr == nil {
					entries[i].ResolvesToDir = true
				}
				r.notice(NoticeLinkLeavesRoots, child, fmt.Sprintf(
					"not descended: the resolved target %s is beneath no configured library root", target))
				continue
			}
		}

		if !e.IsDir && !e.IsLink {
			if r.isMedia(e.Name) {
				r.regionMedia[info.Region] = true
			}
			continue
		}

		ci, cerr := r.c.Platform.Inspect(child)
		if cerr != nil {
			// A directory the walk cannot even inspect is not a checked path -
			// the walk found no mounted filesystem there, because it could not
			// look. It is reported and nothing is taken from it, which is what
			// keeps it safe: a path no source is enumerated from is a path no
			// swap can happen under.
			//
			// One an exclude pattern reaches would not have been listed either,
			// so it is reported as what it is - excluded - with the failure
			// beside it, and never as the unreadable directory an operator's
			// filter already keeps out of the run (S0168).
			if r.excluded(child) {
				r.notice(NoticeExcluded, child, fmt.Sprintf(
					"not listed: an exclude_paths pattern reaches it, and it could not be inspected either: %v", cerr))
				continue
			}
			kind := NoticeListingFailed
			if errors.Is(cerr, fs.ErrPermission) {
				kind = NoticeUnreadable
			}
			r.notice(kind, child, fmt.Sprintf("not descended: %v", cerr))
			continue
		}
		if !ci.IsDir {
			if r.isMedia(e.Name) {
				r.regionMedia[info.Region] = true
			}
			continue
		}
		entries[i].ResolvesToDir = true
		r.considerDirectory(child, ci, info)
	}
}

// considerDirectory classifies a directory the walk met, records it when it is a
// distinct mount point, and descends it unless its region was already walked.
func (r *checkRun) considerDirectory(child string, ci, parent Info) {
	if r.isMountPoint(child, ci, parent) {
		// Distinctness is by resolved MOUNT POINT, never by backing device: one
		// filesystem mounted at two paths beneath a root gets a record for EACH
		// mount point, and each is a checked path an opt-in must cover in its
		// own right. A directory on the same mounted filesystem as its parent
		// gets no record of its own. The record is emitted whether or not the
		// walk goes on to descend it, so a mount whose region is already walked,
		// and a mount that cannot be read, both still carry their own record.
		rec := r.newRecord(KindMount, child)
		r.addRecord(rec)
		if rec.Denied {
			// A mount the walk FOUND is a checked path in its own right, so a
			// checked path that exists and cannot be INSPECTED refuses the run
			// here exactly as a root or the state directory would - whatever was
			// declared, and with no declaration named as a remedy. This is not
			// the unreadable case: being unable to LIST a directory beneath a
			// root refuses nothing, while being unable to establish its type at
			// all is a path holdfast cannot inspect.
			r.refuse(deniedCause(child))
		}
	}

	// The prune is decided AFTER the mount record and before anything else, so a
	// pruned mount point keeps its record and every refusal that record carries
	// (S0168 AC-6), while nothing beneath it - a mount point included - is ever
	// met. It is not one of this region's kids either: a root whose only media
	// lies beneath pruned directories holds none this run would enumerate.
	if r.excluded(child) {
		r.prune(child, ci.Region)
		return
	}

	r.regionKids[parent.Region] = append(r.regionKids[parent.Region], ci.Region)

	if prev, ok := r.entered[ci.Region]; ok {
		if prev.pruned {
			// Pruned storage met again under a spelling no exclude pattern
			// reaches: declined, never descended (S0168 AC-7). Descending it would
			// enumerate, under this name, exactly the files the excluded name
			// keeps out of the run.
			r.notice(NoticeRegionExcluded, child, prev.describeExcluded("this path"))
			return
		}
		// Never enter a region entered earlier in THIS run, and skip exactly the
		// already-walked PART: a mount exposing a region the walk covered only
		// in part is descended and only the walked part is skipped, so nothing
		// is silently lost and nothing is enumerated twice. One level down this
		// refuses nothing either way (AC2f1: no such cause is a sixth row) - but
		// the REASON reported has to be the one the walk observed, so a region
		// that was entered and could not be listed does not get told it is
		// covered from somewhere else.
		if prev.err != nil {
			r.notice(NoticeRegionWalked, child, fmt.Sprintf(
				"not descended: this path exposes storage this run already entered under %s, where the listing failed (%v); no source is enumerated from it under either spelling",
				prev.path, prev.err))
			return
		}
		r.notice(NoticeRegionWalked, child, fmt.Sprintf(
			"not descended: this path exposes storage this run already walked under %s; its sources are enumerated exactly once, from there", prev.path))
		return
	}
	r.descend(child, ci, false)
}

// excluded reports whether the path filters exclude dir whole (Check.Excluded).
//
// A configured library root is never excluded, wherever the walk meets it (S0168
// AC-5): a root is listed, classified and walked whatever a pattern says about it,
// and that includes a root an EARLIER root's pattern reaches - two nested roots,
// which Validate refuses. The walk then meets it inside the outer root and walks it
// there, and the outer root's patterns prune what lies beneath it.
func (r *checkRun) excluded(dir string) bool {
	return r.c.Excluded != nil && !r.isConfiguredRoot(dir) && r.c.Excluded(dir)
}

// isConfiguredRoot reports whether dir is one of the configured library roots, as
// cleaned.
func (r *checkRun) isConfiguredRoot(dir string) bool {
	clean := cleanPath(dir)
	for _, root := range r.roots {
		if root == clean {
			return true
		}
	}
	return false
}

// prune records a directory the walk does not list because an exclude pattern
// reaches it. Its region is marked - unless an earlier spelling already entered
// it, whose record stands - so the same storage met later under a spelling no
// pattern reaches is declined rather than walked (AC-7). Nothing is added to the
// coverage or to the carried listings, which is what keeps every scan pass, the
// orphaned-temp sweep and the watch out of it: all three are bounded by them.
func (r *checkRun) prune(dir string, reg Region) {
	if _, seen := r.entered[reg]; !seen {
		r.entered[reg] = regionEntry{path: dir, pruned: true}
	}
	r.notice(NoticeExcluded, dir, "not listed: an exclude_paths pattern in force for its library root "+
		"reaches it, so nothing beneath it is enumerated, swept or watched, and the ledger retention "+
		"pass draws no conclusion about a file beneath it")
}

// isMountPoint reports whether child is the root of a mounted filesystem.
//
// Two independent limbs, deliberately: the mounted-filesystem identity changing
// between a directory and its parent (which needs no mount information at all
// and catches every real filesystem boundary), and the mount table (the only
// thing that can see a BIND mount, which shares its device number with the
// filesystem it exposes). When the mount table cannot be read the first limb
// still answers, and the classification of anything the second limb would have
// added is by construction identical to its parent's - same mounted filesystem,
// same storage - so absent mount information moves what can be told apart and
// never what the decision is made of.
func (r *checkRun) isMountPoint(child string, ci, parent Info) bool {
	if ci.DevID != parent.DevID {
		return true
	}
	mounted, err := r.c.Platform.MountPoint(child)
	if err != nil {
		if !r.mountInfoNoted {
			r.mountInfoNoted = true
			r.notice(NoticeMountInfoUnavailable, child, err.Error())
		}
		return false
	}
	return mounted
}

// reportEmptyRoots reports every configured library root that is present, local
// and holds no media file this run would enumerate. That report is made AT
// STARTUP, because the walk visits the tree anyway and deciding a file is not one
// this run would enumerate needs no read of it.
//
// An empty library, an unlistable one and an undescended one are three distinct
// reports and this emits only the first: a root the region rule declined is
// reported as undescended and never as empty, and a root that could not be
// listed has already refused the run.
func (r *checkRun) reportEmptyRoots(rootInfo []*Info) {
	for i, root := range r.roots {
		if rootInfo[i] == nil || r.declined[root] || !r.coveredSet[root] {
			continue
		}
		idx, ok := r.recordIndex(root)
		if !ok || !r.res.Records[idx].Class.IsLocal() {
			continue
		}
		if r.regionHasMedia(rootInfo[i].Region) {
			continue
		}
		r.notice(NoticeEmptyRoot, root, "present, and holds no media file this run would enumerate")
	}
}

// regionHasMedia reports whether a media file lies anywhere at or beneath a
// region, following into the regions the walk declined to re-enter as well as
// the ones it descended - so a root whose library is reached through another
// spelling is not called empty. The visited set makes it terminate over a bind
// loop just as the walk does.
func (r *checkRun) regionHasMedia(reg Region) bool {
	seen := map[Region]bool{}
	var walk func(Region) bool
	walk = func(x Region) bool {
		if seen[x] {
			return false
		}
		seen[x] = true
		if r.regionMedia[x] {
			return true
		}
		for _, kid := range r.regionKids[x] {
			if walk(kid) {
				return true
			}
		}
		return false
	}
	return walk(reg)
}

func (r *checkRun) recordIndex(path string) (int, bool) {
	if res, err := r.resolvedForm(path); err == nil {
		if i, ok := r.byResolved[res]; ok {
			return i, true
		}
	}
	i, ok := r.byPath[path]
	return i, ok
}

func (r *checkRun) isMedia(base string) bool {
	return r.c.IsMediaFile != nil && r.c.IsMediaFile(base)
}

// resolvedWithinRoots reports whether an already-resolved target is at or
// beneath the RESOLVED form of some configured library root.
func (r *checkRun) resolvedWithinRoots(target string) bool {
	if !r.rootsResolved {
		r.rootsResolved = true
		for _, root := range r.roots {
			r.resolvedRoots = append(r.resolvedRoots, r.resolvedOrEmpty(root))
		}
	}
	for _, rr := range r.resolvedRoots {
		if rr != "" && withinResolved(target, rr) {
			return true
		}
	}
	return false
}

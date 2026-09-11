package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast requeue` is the operator's lever for the rows a configuration change cannot
// reason about.
//
// Re-opening at scan time answers "the value this verdict was computed from has moved".
// It cannot answer the rest. A row parked at max_failures records an ERROR TEXT, not a
// decision the configuration determined, so no key an operator edits tells holdfast
// whether a new value would change it. A file skipped by a guard whose inputs have NOT
// moved is a verdict the operator may simply want taken again - after replacing the
// source, after upgrading ffmpeg, after any of the things the ledger cannot see. This is
// the door for both, and like `restore` it is LOCAL BY DESIGN: it changes what the engine
// will do to a media file, and a mutating endpoint opens an authorization question the
// read-and-control API does not answer (ratified operator decision).
//
// It re-opens by the SAME mechanism a configuration change does - clearing what the row
// recorded, so the next Claim reads a verdict it cannot re-derive - rather than by
// deleting the row. Deleting would take a done row's contribution to the lifetime
// reclaimed total with it, and a failed row's attempt accounting with it.
//
// That mechanism is what re-opens a done or skipped row. A row parked at max_failures is
// held by its attempt count and by nothing else, so re-opening one means clearing the
// count - and which SELECTOR reached it does not change what holds it. A requeue that
// cleared the count only under --failed would tell an operator who named their one parked
// file that it was back in the pipeline, and the next scan would refuse it exactly as
// before: the silent no-op this whole feature exists to end, rebuilt inside the lever
// built to end it.

// SkipGuards is the closed vocabulary `--guard` accepts, in the order `requeue` lists
// them when it refuses one it does not recognise.
//
// The two mutable guards are deliberately absent. Both are cleared and re-derived on
// every pass already, so there is nothing for a requeue to re-open: the file is offered
// to the pipeline by the next scan whatever anybody does here.
//
// SkipRestoredOriginal IS here, and that is not an oversight: an operator naming it must
// learn that the rows are protected rather than that the token does not exist. Requeue
// refuses to touch them and says so.
var SkipGuards = []string{
	SkipAlreadyTargetCodec,
	SkipLowBitrate,
	SkipInterlaced,
	SkipDolbyVision,
	SkipHDR10Plus,
	SkipIncompleteHDRMetadata,
	SkipExoticPixelFormat,
	SkipTargetExists,
	SkipSymlink,
	SkipRestoredOriginal,
}

// KnownGuard reports whether token is one this build recognises.
func KnownGuard(token string) bool {
	for _, g := range SkipGuards {
		if g == token {
			return true
		}
	}
	return false
}

// RequeueSelector names the rows one requeue is about. Exactly one of the three is set;
// none is refused (ErrNoSelector), because a requeue with no selector would be a request
// to re-open the whole ledger and nobody types that by accident, and more than one is
// refused (ErrTooManySelectors), because acting on one of them would hand an operator a
// different set from the one they typed without telling them.
type RequeueSelector struct {
	// Path is one library path. Every terminal row at it is re-opened - there is
	// normally one, but a path the library has held more than one version of carries a
	// row per fingerprint and an operator naming the path means the file, not a
	// fingerprint they have no way of knowing.
	Path string
	// Guard is a skip token. Every `skipped` row carrying it is re-opened.
	Guard string
	// Failed selects every row parked at max_failures.
	Failed bool
}

// Empty reports whether nothing was selected.
func (s RequeueSelector) Empty() bool { return s.Path == "" && s.Guard == "" && !s.Failed }

// Given names each selector that was set, in the order the usage lists them. A requeue
// carries exactly one; this is what the refusal prints when it carries more.
func (s RequeueSelector) Given() []string {
	var given []string
	if s.Path != "" {
		given = append(given, "a path")
	}
	if s.Guard != "" {
		given = append(given, "--guard "+s.Guard)
	}
	if s.Failed {
		given = append(given, "--failed")
	}
	return given
}

// describe names what a requeue looked for, for the refusal that has to say so.
func (s RequeueSelector) describe() string {
	switch {
	case s.Path != "":
		return "the path " + s.Path
	case s.Guard != "":
		return "rows skipped by the guard " + s.Guard
	default:
		return "rows parked at max_failures"
	}
}

// ProtectedRow is one row a requeue matched and REFUSED to touch, with the reason it is
// refused. It is reported rather than silently dropped: an operator who asked for a set
// and got a smaller one is owed the difference, and each of these three is a file whose
// current state is exactly what a re-encode must not be allowed to overwrite.
type ProtectedRow struct {
	Path string
	Why  string
}

// RequeueResult is what one requeue did.
type RequeueResult struct {
	// Reopened is the paths whose rows were re-opened, sorted.
	Reopened []string
	// Protected is every matched row that was left exactly as it was.
	Protected []ProtectedRow
	// AttemptsCleared is how many of the re-opened rows were parked on their attempt
	// count, so re-opening them handed the file back its retries. It is reported
	// because that is a different act from clearing what a row recorded about the
	// configuration: the next scan will ENCODE those files if the guards let it.
	AttemptsCleared int
}

// reopening is one row a requeue is about to re-open, and how.
type reopening struct {
	path, fingerprint string
	clearFailures     bool
}

// ErrNoSelector is `requeue` with no path, no --guard and no --failed.
var ErrNoSelector = errors.New("requeue needs a path, --guard <token> or --failed: " +
	"re-opening the whole ledger is not something this command will do on an empty selector")

// ErrTooManySelectors is a requeue carrying more than one of path, --guard and --failed.
// Each names a different set, and the ledger has no reading of two of them together: a
// command that picked one would act on a set the operator did not ask for and report that
// as success, which is the same silence this whole feature exists to end.
type ErrTooManySelectors struct{ Given []string }

func (e ErrTooManySelectors) Error() string {
	return fmt.Sprintf("requeue takes ONE selector and got %d (%s): each names a different set of "+
		"rows, so run the command once per set rather than have it guess which one you meant",
		len(e.Given), strings.Join(e.Given, ", "))
}

// ErrUnknownGuard is a --guard token this build does not recognise. It is distinct from
// "matched nothing" on purpose: a typo and an empty set are different problems, and
// reporting a typo as zero matches sends an operator looking at their library.
type ErrUnknownGuard struct{ Token string }

func (e ErrUnknownGuard) Error() string {
	return fmt.Sprintf("%q is not a guard this build recognises. The guards are: %s",
		e.Token, strings.Join(SkipGuards, ", "))
}

// ErrNothingReopened is a requeue that re-opened no row, whether because nothing matched
// or because everything that matched is protected. Either way the command has NOT
// succeeded, and it exits non-zero: a requeue that reported success against an empty set
// would leave an operator believing a file is back in the pipeline when it is not.
type ErrNothingReopened struct {
	Looked    string
	Protected int
}

func (e ErrNothingReopened) Error() string {
	if e.Protected > 0 {
		return fmt.Sprintf("nothing was re-opened: every row matching %s is one holdfast never re-opens "+
			"(%d of them, named above)", e.Looked, e.Protected)
	}
	return fmt.Sprintf("nothing matched %s, so nothing was re-opened and no row was changed", e.Looked)
}

// Requeue re-opens the rows sel names, and reports what it did and what it refused.
//
// The match set is collected BEFORE anything is written, for two reasons. A requeue that
// matched nothing must leave every row untouched, which it cannot promise while it is
// already writing; and the store serialises every access through one connection, so
// writing inside the walk of a streaming read would deadlock against it.
func Requeue(ctx context.Context, st store.Store, sel RequeueSelector, maxFailures int) (RequeueResult, error) {
	var res RequeueResult
	if sel.Empty() {
		return res, ErrNoSelector
	}
	if given := sel.Given(); len(given) > 1 {
		return res, ErrTooManySelectors{Given: given}
	}
	if sel.Guard != "" && !KnownGuard(sel.Guard) {
		return res, ErrUnknownGuard{Token: sel.Guard}
	}

	rows, err := st.List(ctx, requeueStatuses(sel), 0)
	if err != nil {
		return res, fmt.Errorf("reading the ledger: %w", err)
	}
	var matched []reopening
	for _, j := range rows {
		if !selects(sel, j, maxFailures) {
			continue
		}
		if why := protects(j); why != "" {
			res.Protected = append(res.Protected, ProtectedRow{Path: j.Path, Why: why})
			continue
		}
		matched = append(matched, reopening{
			path:        j.Path,
			fingerprint: j.Fingerprint,
			// What holds a row out of the pipeline is a property of THE ROW, so what
			// re-opens it is too. A failed row is held by its attempt count and by
			// nothing else (see store.Claim): clearing what it recorded about the
			// configuration would change nothing an operator could observe, so a
			// command that stopped there would report a file back in the pipeline that
			// the next scan refuses exactly as before. Reading it off the selector
			// instead made that true of every route to a failed row but --failed.
			clearFailures: j.Status == store.Failed,
		})
	}

	for _, m := range matched {
		changed, err := st.Reopen(ctx, m.path, m.fingerprint, m.clearFailures)
		if err != nil {
			return res, fmt.Errorf("re-opening %s: %w", m.path, err)
		}
		if changed {
			res.Reopened = append(res.Reopened, m.path)
			if m.clearFailures {
				res.AttemptsCleared++
			}
		}
	}
	sort.Strings(res.Reopened)
	sort.Slice(res.Protected, func(i, j int) bool { return res.Protected[i].Path < res.Protected[j].Path })

	if len(res.Reopened) == 0 {
		return res, ErrNothingReopened{Looked: sel.describe(), Protected: len(res.Protected)}
	}
	return res, nil
}

// requeueStatuses narrows the read to the statuses the selector could possibly match.
//
// A PATH names a file, so it reaches every terminal status - including the two that are
// never re-opened, which is the whole reason it has to see them: an operator naming a
// parked path is owed "this one is parked and here is why", not "nothing matched".
// A GUARD token only ever sits on a skipped row and --failed only on a failed one, so
// neither can reach a parked row at all.
//
// It reads through List rather than EachTerminal for the same reason: EachTerminal is the
// export's walk and carries the three ordinary terminal statuses only. A requeue is an
// operator command run by hand, not something on the scan's path, so holding the matching
// rows is a cost it can pay where the scan could not.
func requeueStatuses(sel RequeueSelector) []store.Status {
	switch {
	case sel.Guard != "":
		return []store.Status{store.Skipped}
	case sel.Failed:
		return []store.Status{store.Failed}
	default:
		return []store.Status{
			store.Done, store.Skipped, store.Failed, store.WouldTranscode,
			store.Indeterminate, store.AppliedDespiteError,
		}
	}
}

// selects reports whether one row is in the set sel names.
func selects(sel RequeueSelector, j store.Job, maxFailures int) bool {
	switch {
	case sel.Path != "":
		return j.Path == sel.Path
	case sel.Guard != "":
		return j.Status == store.Skipped && j.Outcome.Reason == sel.Guard
	default:
		// Parked at max_failures, which is the same arithmetic Claim refuses on - the
		// count is the only thing holding these rows, so the count is what decides
		// whether one of them is what --failed is for.
		return j.Status == store.Failed && maxFailures > 0 && j.FailCount >= maxFailures
	}
}

// protects reports why a matched row is one holdfast never re-opens, or "".
//
// These are the same three the store refuses in Reopen's own WHERE clause. Both checks
// are wanted: this one is what lets the command SAY which rows it left alone and why,
// and that one is what makes the protection a property of the write rather than of the
// caller that happened to make it.
func protects(j store.Job) string {
	switch {
	case j.Status == store.Indeterminate:
		return "parked: the swap's outcome could not be established, so re-encoding this path " +
			"would overwrite contents nobody can describe. `holdfast resolve` is the way out"
	case j.Status == store.AppliedDespiteError:
		return "the swap took effect despite reporting an error, so the file at this path IS the " +
			"replacement and this row describes an attempt that is over"
	case j.Status == store.Skipped && j.Outcome.Reason == SkipRestoredOriginal:
		return "an operator restored this original through the undo window: re-opening it would " +
			"feed their rescued bytes back to the gates that passed the encode they rejected"
	default:
		return ""
	}
}

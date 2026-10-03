package server

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/diskfree"
	"github.com/NSchatz/holdfast/internal/store"
)

// The per-root sizing figures of GET /api/summary (S0169): per configured library root,
// how many bytes a dry run recorded as candidates, what a run over them is projected to
// save, what the undo window is still holding there, and how much room the filesystem
// has. They are what "how big is this job, and will it fit" is answered from, so an
// operator sizes a run from the API and not from a copy of jobs.db.
//
// EVERYTHING HERE IS A READ. Nothing in this file writes a jobs row, and nothing touches a
// retention or a file: a retained original is a second hard link and the only undo path a
// swap leaves, so the held figure is computed from the ledger's record of it and the
// retained name is never opened, stat-ed, renamed or removed.
//
// NULL IS NOT ZERO (docs/design/ledger-totals.md#null-is-not-zero). Each of the three
// reads behind these figures - the ledger, the retention table, the filesystem - can fail
// on its own, and a failure blanks only the figures that read fills: they go out as JSON
// null beside every figure that was read, and never as 0.

// rootTotalsDTO is one configured library root's figures on the wire. Every figure is a
// POINTER and deliberately not omitempty: null says "could not be read", and a true zero
// (no candidate, nothing retained) is 0.
//
// The field names avoid the status word on purpose and say "candidate": a candidate is a
// row a dry run recorded as one it would encode.
type rootTotalsDTO struct {
	// Root is the root's cleaned path (config.Root.Clean), the spelling a job row records
	// as its library_root.
	Root string `json:"root"`
	// CandidateFiles counts the root's candidate rows that recorded a source size, and
	// CandidateBytes is the sum of those sizes. CandidateExcluded counts the candidate
	// rows that recorded none: excluded and counted, never summed as zero.
	CandidateFiles    *int64 `json:"candidate_files"`
	CandidateExcluded *int64 `json:"candidate_excluded"`
	CandidateBytes    *int64 `json:"candidate_bytes"`
	// ProjectionBasisFiles counts the root's done rows that recorded both sizes, and
	// ProjectedSavingsBytes is floor(candidate_bytes x S / B) over them, S the sum of
	// source minus output and B the sum of source. It is null where there is no basis:
	// a projection from nothing would be an invention.
	ProjectionBasisFiles  *int64 `json:"projection_basis_files"`
	ProjectedSavingsBytes *int64 `json:"projected_savings_bytes"`
	// BytesHeldByUndoWindow is the held figure restricted to retentions whose SOURCE path
	// lies under this root.
	BytesHeldByUndoWindow *int64 `json:"bytes_held_by_undo_window"`
	// FreeBytes is the bytes available to this process on the filesystem holding the
	// root. It is per FILESYSTEM: two roots on one filesystem report the same figure, and
	// it is never summed.
	FreeBytes *int64 `json:"free_bytes"`
}

// unattributedTotalsDTO carries the rows and retentions attributed to NO configured root:
// a row that recorded no library root, and a root since removed from the configuration.
// It has no projection and no free space, because it has neither a basis nor a filesystem.
type unattributedTotalsDTO struct {
	CandidateFiles        *int64 `json:"candidate_files"`
	CandidateExcluded     *int64 `json:"candidate_excluded"`
	CandidateBytes        *int64 `json:"candidate_bytes"`
	BytesHeldByUndoWindow *int64 `json:"bytes_held_by_undo_window"`
}

// rootLedgerCache holds the per-root ledger read between refreshes.
//
// The read is whole-ledger work, so it is bounded exactly as the aggregates are: at most
// one read per ledgerFigureInterval however often the summary is polled. It is its own
// cache, refreshed only when a summary asks, because the SSE snapshot does not carry the
// per-root block and a frame must not pay for a figure it does not ship.
//
// A refresh that FAILS leaves nothing to serve for that interval: the figures go out as
// null until a later refresh reads them. The wire shape carries no age for these figures,
// so an older value republished after a failed read would be a value stated as current.
type rootLedgerCache struct {
	mu          sync.Mutex
	refreshedAt time.Time
	refreshed   bool
	totals      []store.RootTotal
	ok          bool
}

// rootLedger returns the per-root ledger figures and whether they could be read,
// refreshing them at most once per ledgerFigureInterval. The interval is measured from the
// ATTEMPT, so a ledger that cannot be read is retried once per interval, not per request.
func (h *Hub) rootLedger(ctx context.Context) ([]store.RootTotal, bool) {
	now := h.now()
	c := &h.rootFigures
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refreshed && now.Sub(c.refreshedAt) < ledgerFigureInterval {
		return c.totals, c.ok
	}
	c.refreshedAt, c.refreshed = now, true
	// The read outlives the request that started it: its answer is cached for every caller
	// of the interval, so one client hanging up mid-read must not blank the figures for all.
	totals, err := h.store.RootTotals(context.WithoutCancel(ctx))
	if err != nil {
		h.log.Warn("the job store could not be read for the per-root candidate and projection "+
			"figures, so they are reported unavailable (null) until a later refresh reads them; "+
			"the rest of the summary still ships",
			"dependency", "job store", "read", "RootTotals", "err", err)
		c.totals, c.ok = nil, false
		return nil, false
	}
	c.totals, c.ok = totals, true
	return totals, true
}

// freeSpaceTimeout bounds one root's free-space read. statfs(2) on a hung network mount
// does not return, and the summary must: past the bound the root's figure is null.
const freeSpaceTimeout = 2 * time.Second

var (
	errFreeSpaceTimeout = errors.New("the free-space read did not return within its bound")
	errFreeSpaceRange   = errors.New("the free-space figure does not fit the wire's integer")
)

// freeCall is one free-space read in flight: every request that asks about the same root
// while it runs waits on this one rather than starting its own.
type freeCall struct {
	started time.Time
	done    chan struct{}
	bytes   uint64
	err     error
}

// freeSpace answers "how much room is there on the filesystem holding this root", bounded.
//
// The answer is internal/diskfree's and no second statfs: that package exists so two parts
// of holdfast cannot disagree about room, and the engine's pre-encode check reads the same
// one.
//
// A read that hangs cannot be cancelled, so it is not repeated: while one is in flight for
// a root, later requests join it, and each waits only for what is left of the bound
// measured from when that read STARTED. A mount that stays hung therefore costs one blocked
// read in total, not one per poll, and every request after the first is answered at once.
//
// The zero value is ready to use and reads diskfree.Bytes under freeSpaceTimeout; read and
// timeout are the seam a test stands in for the filesystem through.
type freeSpace struct {
	read    func(path string) (uint64, error)
	timeout time.Duration

	mu       sync.Mutex
	inflight map[string]*freeCall
}

// bytes returns the free bytes on the filesystem holding root, or why there is no figure.
func (f *freeSpace) bytes(ctx context.Context, root string) (int64, error) {
	timeout := f.timeout
	if timeout <= 0 {
		timeout = freeSpaceTimeout
	}
	call := f.join(root)
	wait := time.NewTimer(timeout - time.Since(call.started))
	defer wait.Stop()
	select {
	case <-call.done:
	case <-wait.C:
		return 0, errFreeSpaceTimeout
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if call.err != nil {
		return 0, call.err
	}
	if call.bytes > math.MaxInt64 {
		return 0, errFreeSpaceRange
	}
	return int64(call.bytes), nil
}

// join returns the read in flight for root, starting one when there is none.
func (f *freeSpace) join(root string) *freeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if call, ok := f.inflight[root]; ok {
		return call
	}
	read := f.read
	if read == nil {
		read = diskfree.Bytes
	}
	if f.inflight == nil {
		f.inflight = make(map[string]*freeCall)
	}
	call := &freeCall{started: time.Now(), done: make(chan struct{})}
	f.inflight[root] = call
	go func() {
		call.bytes, call.err = read(root)
		f.mu.Lock()
		delete(f.inflight, root)
		f.mu.Unlock()
		close(call.done)
	}()
	return call
}

// freeByRoot reads every root's free space, all at once, so the response is bounded by one
// timeout however many roots hang. A root whose read fails or does not return is nil, and
// says so in one warn record naming the root; every other root keeps its figure.
func (s *Server) freeByRoot(ctx context.Context, roots []config.Root) []*int64 {
	out := make([]*int64, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			free, err := s.free.bytes(ctx, root.Clean)
			if err != nil {
				s.log.Warn("the free space of a library root's filesystem could not be read, so "+
					"that root's free_bytes is reported unavailable (null); every other figure is unaffected",
					"dependency", "filesystem", "read", "free space (statfs)", "root", root.Clean, "err", err)
				return
			}
			out[i] = &free
		}()
	}
	wg.Wait()
	return out
}

// projectedSavings is floor(candidate x saved / basis): the candidate bytes scaled by the
// BYTES-WEIGHTED saving the root's own completed encodes achieved. It is not the mean of
// per-row ratios, which over-weights small files and mis-states bytes.
//
// The product is taken in arbitrary precision, because two figures of a pebibyte each
// already exceed an int64 multiplied together. nil is "no projection": a basis that sums
// to nothing gives nothing to divide by, and a quotient the wire's integer cannot hold is
// not reported as a different number.
func projectedSavings(candidate, saved, basis int64) *int64 {
	if basis <= 0 {
		return nil
	}
	q := new(big.Int).Mul(big.NewInt(candidate), big.NewInt(saved))
	// Div is Euclidean division, which for a positive divisor is the floor.
	q.Div(q, big.NewInt(basis))
	if !q.IsInt64() {
		return nil
	}
	v := q.Int64()
	return &v
}

// zero is a fresh pointer to 0: a figure that was READ and is nothing.
func zero() *int64 { return new(int64) }

// rootSizing builds the summary's three additions: the top-level held figure, the per-root
// block in configuration order, and the unattributed totals.
//
// The three reads are independent, and each failure is carried by exactly the figures that
// read fills. The top-level held figure is the SUM of the same per-retention read the
// per-root split is made from, over the predicate the gauge sums over, so the parts always
// add up to the whole and the whole is what the gauge reports.
func (s *Server) rootSizing(ctx context.Context) (held *int64, roots []rootTotalsDTO, unattributed unattributedTotalsDTO) {
	profiles := s.cfg.RootProfiles()
	roots = make([]rootTotalsDTO, len(profiles))
	// A root is found by its cleaned path, the spelling a row records. Nested roots are
	// refused at validate, so a path names at most one; the first entry wins a repeat. A
	// row that recorded no root carries "", which is no root's cleaned path.
	index := make(map[string]int, len(profiles))
	for i, p := range profiles {
		roots[i].Root = p.Clean
		if _, seen := index[p.Clean]; !seen {
			index[p.Clean] = i
		}
	}

	if ledger, ok := s.hub.rootLedger(ctx); ok {
		for i := range roots {
			roots[i].CandidateFiles, roots[i].CandidateExcluded, roots[i].CandidateBytes = zero(), zero(), zero()
			roots[i].ProjectionBasisFiles = zero()
		}
		unattributed.CandidateFiles, unattributed.CandidateExcluded, unattributed.CandidateBytes = zero(), zero(), zero()
		for _, t := range ledger {
			i, configured := index[t.LibraryRoot]
			if !configured {
				*unattributed.CandidateFiles += t.CandidateFiles
				*unattributed.CandidateExcluded += t.CandidateExcluded
				*unattributed.CandidateBytes += t.CandidateBytes
				continue
			}
			r := &roots[i]
			*r.CandidateFiles += t.CandidateFiles
			*r.CandidateExcluded += t.CandidateExcluded
			*r.CandidateBytes += t.CandidateBytes
			*r.ProjectionBasisFiles += t.BasisFiles
			if t.BasisFiles > 0 {
				r.ProjectedSavingsBytes = projectedSavings(t.CandidateBytes, t.BasisSavedBytes, t.BasisSourceBytes)
			}
		}
	}

	if sources, err := s.reads().HeldBySource(ctx); err != nil {
		s.log.Warn("the job store could not be read for the bytes held by the undo window, so "+
			"bytes_held_by_undo_window is reported unavailable (null) at the top level, for every "+
			"root and for the unattributed total; every other figure is unaffected",
			"dependency", "job store", "read", "HeldBySource", "figure", "bytes_held_by_undo_window", "err", err)
	} else {
		held = zero()
		unattributed.BytesHeldByUndoWindow = zero()
		for i := range roots {
			roots[i].BytesHeldByUndoWindow = zero()
		}
		for _, src := range sources {
			*held += src.SourceBytes
			into := unattributed.BytesHeldByUndoWindow
			if root, ok := s.cfg.RootFor(src.SourcePath); ok {
				into = roots[index[root.Clean]].BytesHeldByUndoWindow
			}
			*into += src.SourceBytes
		}
	}

	for i, free := range s.freeByRoot(ctx, profiles) {
		roots[i].FreeBytes = free
	}
	return held, roots, unattributed
}

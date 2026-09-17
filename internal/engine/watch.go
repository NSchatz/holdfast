package engine

// The per-root filesystem watch (S0091): an OPT-IN accelerator for DISCOVERY, and nothing
// else.
//
// holdfast finds a new file by enumerating the whole library, on startup or on the
// scan_interval_sec tick. The steady state of a media library is "one episode arrived", and
// answering that with a full traversal is why the interval ships at 0 and why in practice a
// new file waits for a restart. A watch turns the arrival into an event.
//
// What it is NOT is the mechanism. Events are lossy by construction - the platform's queue
// overflows, a restart misses everything, a file moved in by a rename the watch never saw
// is simply there - so the periodic scan remains the source of truth and this only ever
// makes it earlier. Every offer goes through Engine.ProcessFile, the exported door every
// scan-found file already enters by, so every skip guard, every gate and the Store.Claim
// mutual-exclusion guard reach a watch-found file BY CONSTRUCTION rather than by a second
// copy of them living here.
//
// Two things it owns that the scan does not:
//
//   - THE SETTLE DELAY. A scan meets a file that has been sitting there; a watch meets one
//     the moment a download client created it. Probing a half-written 40 GB remux reads a
//     duration and a packet count that are not the file's, and every gate downstream is
//     weighed against those numbers. So a file is offered only once its SIZE has held still
//     for the configured period.
//   - THE FALLBACK DECISION. A watch either exists for a root or it does not, and the one
//     unacceptable outcome is a root nobody is watching that is reported as watched. Every
//     way that can happen is decided once, at startup, and recorded at warn with the
//     dependency that failed, what was tried, and the interval scan named as what covers
//     the root now.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/fsclass"
)

// DefaultWatchDescriptors bounds how many directories one process may watch at once,
// across every watched root.
//
// fsnotify is not recursive: a descriptor is one per DIRECTORY and the count grows with the
// tree, while the host's own limit (fs.inotify.max_user_watches on Linux) differs per
// distro and per available memory. Reaching it reports itself as "no space left on device"
// or "too many open files" on an Add that would otherwise have quietly succeeded for half a
// library. An explicit bound of our own means the degradation is ANNOUNCED at a number this
// build chose rather than met at whatever number the host happens to carry.
const DefaultWatchDescriptors = 8192

// DefaultWatchOfferQueue is how many settled paths may wait for the pool. A full queue is a
// dropped offer and is recorded as one: the file is still in the library and the next
// interval scan finds it, which is exactly the relationship this feature has to the scan.
const DefaultWatchOfferQueue = 1024

// watchSettleTick is how often the settle decision is taken. It is a stat per pending file
// per tick and nothing else - no directory is listed and no file is opened - and the
// pending set is bounded by what actually arrived since the last tick.
const watchSettleTick = time.Second

// Watches is the filesystem watch for one engine: the per-root start-or-fall-back decision,
// the settle tracker, and a small pool that offers settled paths to ProcessFile.
type Watches struct {
	eng *Engine
	// log carries the component, bound once here rather than retyped per call, so every
	// line this file emits can be joined to the watch rather than to the engine at large.
	log *slog.Logger

	// --- the two seams -------------------------------------------------------------
	//
	// A test cannot make a kernel lack a backend, overflow a queue, or take sixty seconds
	// to settle a file. It stands in for the EVENT SOURCE and the CLOCK; the watch itself,
	// and everything from the offer onwards, is the real thing.

	// newBackend obtains the event source. Production wires the fsnotify-backed one.
	newBackend func() (watchBackend, error)
	// now is the clock the settle period is measured against.
	now func() time.Time
	// mechanism is the name of the event mechanism this build carries for this platform,
	// and "" where it carries none. It is only ever REPORTED once a backend has actually
	// been obtained.
	mechanism string

	// maxDescriptors is the bound above, held per instance so a test can drive exhaustion
	// without opening eight thousand descriptors.
	maxDescriptors int

	backend watchBackend

	mu sync.Mutex
	// pending is the settle tracker: one entry per path an event named that this run has
	// not yet offered.
	pending map[string]*settling
	// watched is how many descriptors each WATCHED root holds, keyed by its cleaned path.
	// A root that fell back is absent, which is what Watching reads.
	watched map[string]int

	// ch carries settled, eligible paths to the pool. It is bounded for the reason the
	// targeted-submission queue is: a queue that grew without limit would be a queue that
	// falls arbitrarily far behind the library it is meant to be accelerating.
	ch      chan string
	workers int

	wg   sync.WaitGroup
	once sync.Once
}

// settling is one path waiting for its size to hold still.
type settling struct {
	// settle is the period this path's root configured.
	settle time.Duration
	// size is the last size observed, and known says whether one has been.
	size  int64
	known bool
	// stableSince is when the size last CHANGED, which is what the period is measured
	// from. A watch that measured from the event would offer a file that has been growing
	// steadily for the whole period.
	stableSince time.Time
}

// NewWatches builds the watch for this engine. It starts nothing: Run takes the decision
// and drives it, exactly as the submission queue's Run does.
func (e *Engine) NewWatches() *Watches {
	return &Watches{
		eng:            e,
		log:            e.Log.With("component", "watch"),
		newBackend:     newFSNotifyBackend,
		now:            time.Now,
		mechanism:      thisMechanism(),
		maxDescriptors: DefaultWatchDescriptors,
		pending:        map[string]*settling{},
		watched:        map[string]int{},
		ch:             make(chan string, DefaultWatchOfferQueue),
		workers:        e.Cfg.EffectiveWorkers(),
	}
}

// Watching reports whether root ended up with a watch over it. It is the question the
// fallback decision answers, and it is deliberately not derivable from the configuration: a
// root that asked for a watch and could not have one is configured for one and is not
// watched.
func (w *Watches) Watching(root string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watched[filepath.Clean(root)] > 0
}

// Descriptors is how many watch descriptors this process holds, across every watched root.
func (w *Watches) Descriptors() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, held := range w.watched {
		n += held
	}
	return n
}

// Run takes the start-or-fall-back decision, then drives the watch until ctx is cancelled.
// It is the serve command's driver and runs in the caller's goroutine, exactly as the hub's
// and the submission queue's do.
//
// A cancelled ctx tears every descriptor down - the same shutdown path the scan loop
// honours - and stops the pool taking new work. A file already inside ProcessFile finishes
// through that function's own cancellation discipline: the in-flight ffmpeg is killed, its
// temp is discarded and the source is untouched.
func (w *Watches) Run(ctx context.Context) {
	w.once.Do(func() {
		// The PARKED report is owed once per process, and a daemon whose whole point is
		// that it need not scan may otherwise never publish one.
		w.eng.EnsureHoldBacks(ctx)
		w.start(ctx)
		if w.Descriptors() == 0 {
			// Nothing is watched: no configuration asked for one, or every root that did
			// fell back and has said so. Starting a pool and a loop over no event source
			// would be a daemon holding goroutines open to do nothing.
			//
			// A backend obtained for a root that then fell back - the platform refused the
			// very first directory, say - is released HERE, because the loop that would
			// otherwise have released it is the one not starting.
			if err := w.Close(); err != nil {
				w.log.Warn("releasing the watcher obtained for a root that then fell back failed; no root is "+
					"watched and the interval scan covers every one of them",
					"dependency", "github.com/fsnotify/fsnotify", "err", err,
					"next", "the interval scan (scan_interval_sec) serves every root; nothing else changes")
			}
			return
		}
		for i := 0; i < w.workers; i++ {
			w.wg.Add(1)
			go w.drain(ctx, "watch-w"+strconv.Itoa(i))
		}
		w.wg.Add(1)
		go w.loop(ctx)
	})
	w.wg.Wait()
}

// Wait blocks until the watch has stopped and any file being processed has finished. Call
// it during shutdown, after the base ctx is cancelled and BEFORE the store handle is
// closed, so a worker can never issue a store call against a closed handle. It is the same
// contract Controller.Wait and Submissions.Wait carry.
func (w *Watches) Wait() { w.wg.Wait() }

// Close releases every descriptor this watch holds. It is safe to call twice and safe to
// call on a watch that never obtained a backend.
func (w *Watches) Close() error {
	w.mu.Lock()
	b := w.backend
	w.backend = nil
	w.watched = map[string]int{}
	w.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Close()
}

// start takes the decision for every root that opted in, and records it. It is called once,
// by Run, and returns having either watched a root or said why it did not.
func (w *Watches) start(ctx context.Context) {
	roots := w.optedIn()
	if len(roots) == 0 {
		return
	}
	for _, r := range roots {
		dirs, bad := w.covered(r)
		if bad != nil {
			w.fellBack(r, bad)
			continue
		}
		if bad := w.classify(r); bad != nil {
			w.fellBack(r, bad)
			continue
		}
		backend, bad := w.source()
		if bad != nil {
			w.fellBack(r, bad)
			continue
		}
		w.watch(ctx, r, dirs, backend)
	}
}

// optedIn is the roots this configuration asked for a watch over, in configuration order.
func (w *Watches) optedIn() []config.Root {
	var out []config.Root
	for _, r := range w.eng.Cfg.RootProfiles() {
		if r.Watch.Enabled {
			out = append(out, r)
		}
	}
	return out
}

// unwatchable is one established reason a root cannot be watched: which dependency failed,
// what was tried, and - for the record an operator reads - nothing else, because the next
// action is always the same one.
type unwatchable struct {
	dependency string
	attempted  string
}

// covered is the directories this watch may register under a root: exactly the ones the
// startup walk traversed successfully, which is the same set the scan enumerates from.
//
// A directory the walk declined is one this feature touches in no way at all, and a run
// with no coverage set at all is one whose bound was never established - so the watch
// declines rather than walking the tree itself and discovering directories no scan would
// look in.
func (w *Watches) covered(r config.Root) ([]string, *unwatchable) {
	if w.eng.Coverage == nil {
		return nil, &unwatchable{
			dependency: "the startup walk's coverage set (FILESYSTEM-1)",
			attempted:  "asked for the directories the startup walk traversed successfully, and this run established none",
		}
	}
	var dirs []string
	for _, dir := range w.eng.Coverage {
		// The retention area holds originals the undo window is keeping. The scan does
		// not list it and the watch does not register it: an event from in there could
		// only ever be about bytes an operator was given a window to recover.
		if IsRetentionDir(dir) {
			continue
		}
		if r.Contains(dir) {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return nil, &unwatchable{
			dependency: "the startup walk's coverage set (FILESYSTEM-1)",
			attempted:  "asked for the covered directories under this root, and the walk traversed none of it",
		}
	}
	return dirs, nil
}

// classify answers the FILESYSTEM-1 question about a root's storage, through the same
// lookup the swap and the source guards use. Only a POSITIVE local identification counts:
// a network filesystem provides no event support at all (the NFS and SMB protocols carry
// no file notifications), and storage this build cannot identify is not evidence that it
// does.
func (w *Watches) classify(r config.Root) *unwatchable {
	cls := fsclass.Of(w.eng.fsLookup, r.Clean)
	if cls.IsLocal() {
		return nil
	}
	detail := cls.String()
	if cls.Reason != "" {
		detail += ": " + cls.Reason
	}
	return &unwatchable{
		dependency: "FILESYSTEM-1, the storage classification of this root",
		attempted:  "classified the storage under this root and needed a positive local identification, and got " + detail,
	}
}

// source obtains the event source, once, and reports the two ways there is none: a platform
// this build carries no backend for, and a platform that refused one.
//
// The mechanism is checked FIRST and the constructor is not called at all where there is no
// name for what it would return. A watcher this build could not name would be a watch
// running under no mechanism anybody could report, which is the false report AC-2 forbids.
func (w *Watches) source() (watchBackend, *unwatchable) {
	w.mu.Lock()
	if w.backend != nil {
		defer w.mu.Unlock()
		return w.backend, nil
	}
	w.mu.Unlock()

	if w.mechanism == "" {
		return nil, &unwatchable{
			dependency: "github.com/fsnotify/fsnotify",
			attempted: "asked for the filesystem-event backend this build carries for this platform, " +
				"and this build carries none for it",
		}
	}
	b, err := w.newBackend()
	if err != nil {
		return nil, &unwatchable{
			dependency: "github.com/fsnotify/fsnotify",
			attempted:  fmt.Sprintf("asked the platform for a %s watcher and it refused: %v", w.mechanism, err),
		}
	}
	w.mu.Lock()
	w.backend = b
	w.mu.Unlock()
	return b, nil
}

// watch registers this root's covered directories, bounded, and records what it got.
//
// The bound and the host's own limit are the same outcome and are reported the same way:
// this root is no longer FULLY watched, and the interval scan covers it. The watch is kept
// - a watch over part of a tree still accelerates that part - but the record says how much
// of the tree it holds, so nothing anywhere reports a watch over the whole of a tree it
// sees part of.
func (w *Watches) watch(ctx context.Context, r config.Root, dirs []string, backend watchBackend) {
	held := 0
	var partial *unwatchable
	for _, dir := range dirs {
		if ctx.Err() != nil {
			break
		}
		if w.Descriptors()+held >= w.maxDescriptors {
			partial = &unwatchable{
				dependency: "the watch-descriptor bound this build holds",
				attempted: fmt.Sprintf("registered %d of this root's %d covered directories and reached the "+
					"bound of %d descriptors", held, len(dirs), w.maxDescriptors),
			}
			break
		}
		if err := backend.Add(dir); err != nil {
			partial = &unwatchable{
				dependency: "the host's own watch limit",
				attempted: fmt.Sprintf("registered %d of this root's %d covered directories and the platform "+
					"refused the next one (%s): %v", held, len(dirs), dir, err),
			}
			break
		}
		held++
	}
	if held == 0 {
		if partial == nil {
			partial = &unwatchable{
				dependency: "the startup walk's coverage set (FILESYSTEM-1)",
				attempted:  "had no covered directory under this root to register",
			}
		}
		w.fellBack(r, partial)
		return
	}
	w.mu.Lock()
	w.watched[r.Clean] = held
	w.mu.Unlock()

	// The record AC-2 asks for: the mechanism actually obtained, once per root, beside the
	// descriptor count it is holding and the settle period its files will be offered after.
	w.log.Info("watch started for this library root; the periodic scan still covers it in full",
		"library_root", r.Clean, "mechanism", w.mechanism,
		"watch_descriptors", held, "directories_covered", len(dirs),
		"settle_sec", r.Watch.SettleSec)
	if partial != nil {
		w.degraded(r, partial, held, len(dirs))
	}
	if r.Watch.SettleSec == 0 {
		w.log.Warn("this root's settle period is 0, so a file is offered the moment an event names it: a file "+
			"a download client is still writing will be probed mid-write, and every gate downstream is weighed "+
			"against a duration and a packet count that are not the finished file's",
			"library_root", r.Clean, "settle_sec", 0)
	}
}

// fellBack records that a root asked for a watch and has none. It NAMES no mechanism,
// deliberately: this root obtained none, and a record carrying one would be the false
// report the whole feature is written against.
func (w *Watches) fellBack(r config.Root, why *unwatchable) {
	w.log.Warn("this library root cannot be watched; falling back to the interval scan, which covers it in full",
		"library_root", r.Clean,
		"dependency", why.dependency,
		"attempted", why.attempted,
		"next", "the interval scan (scan_interval_sec) serves this root alone; nothing else changes")
}

// degraded records a root that IS watched but not over the whole of its tree. It is a warn
// for the reason O3 gives: the process continued in a degraded state, and an operator who
// believed the watch covered the library would otherwise never learn that it does not.
func (w *Watches) degraded(r config.Root, why *unwatchable, held, covered int) {
	w.log.Warn("this library root is NO LONGER FULLY WATCHED: the watch covers part of its tree and the "+
		"interval scan covers the rest, which is every directory it holds no descriptor for",
		"library_root", r.Clean,
		"dependency", why.dependency,
		"attempted", why.attempted,
		"watch_descriptors", held, "directories_covered", covered,
		"next", "the interval scan (scan_interval_sec) covers this root in full; the watch accelerates the part it holds")
}

// loop is the watch: events in, settle decisions on the tick, until ctx is cancelled.
func (w *Watches) loop(ctx context.Context) {
	defer w.wg.Done()
	defer func() {
		if err := w.Close(); err != nil {
			w.log.Warn("releasing the watch descriptors failed; the process is shutting down and the "+
				"kernel releases them with it", "err", err)
		}
	}()

	w.mu.Lock()
	backend := w.backend
	w.mu.Unlock()
	if backend == nil {
		return
	}
	events := backend.Events()
	errs := backend.Errors()

	ticker := time.NewTicker(watchSettleTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			w.observe(ev)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			w.reportSourceError(err)
		case <-ticker.C:
			w.settleOnce()
		}
	}
}

// observe takes one event. It decides from the PATH and the event alone - nothing is opened
// and nothing is stat'd here - because an event storm is one write's worth of work per
// event and the settle pass is where the filesystem is asked anything.
func (w *Watches) observe(ev watchEvent) {
	path := filepath.Clean(ev.Path)
	if ev.Gone {
		// A path that was removed or renamed away before it settled is DROPPED, here,
		// without a probe and without a record at error: a create-then-delete is routine,
		// and an operator trained to ignore this log is an operator who ignores the next
		// one too.
		w.mu.Lock()
		_, waiting := w.pending[path]
		delete(w.pending, path)
		w.mu.Unlock()
		if waiting {
			w.log.Debug("dropped a watched path that went away before it settled; nothing was offered for it",
				"file", path)
		}
		return
	}
	// The same name rule the enumeration applies, so holdfast's own working files - the
	// temp an encode is writing, a retained original, a retained replacement - produce no
	// offer however much they are written to.
	if bad := w.eng.Eligibility().Name(filepath.Base(path)); bad != nil {
		return
	}
	root, ok := w.eng.rootFor(path)
	if !ok || !w.Watching(root.Clean) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, waiting := w.pending[path]; waiting {
		// Already waiting. The SIZE is what restarts the period, never the event: a file
		// being written produces events continuously, and resetting on each of them would
		// make the period unreachable for exactly the file it exists for.
		return
	}
	w.pending[path] = &settling{settle: root.Watch.Settle(), stableSince: w.now()}
}

// settleOnce takes the settle decision for every pending path: a stat, and one of three
// answers.
//
//   - Gone or unreadable: dropped, with no offer and no record at error (AC-8).
//   - A size that moved: the period restarts from now. This is the whole delay - a file
//     that has been growing for an hour has never been stable for a minute.
//   - A size that has held still for the configured period: offered, once, and forgotten.
func (w *Watches) settleOnce() {
	now := w.now()
	type ready struct{ path string }
	var offers []ready
	var vanished []string

	w.mu.Lock()
	for path, s := range w.pending {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			delete(w.pending, path)
			vanished = append(vanished, path)
			continue
		}
		if !s.known || fi.Size() != s.size {
			s.size = fi.Size()
			s.known = true
			s.stableSince = now
			continue
		}
		if now.Sub(s.stableSince) < s.settle {
			continue
		}
		delete(w.pending, path)
		offers = append(offers, ready{path: path})
	}
	w.mu.Unlock()

	for _, p := range vanished {
		w.log.Debug("dropped a watched path that went away or became unreadable before it settled; "+
			"nothing was offered for it", "file", p)
	}
	for _, o := range offers {
		w.offer(o.path)
	}
}

// offer hands ONE settled path to the pool, through the same eligibility decision a
// targeted submission is judged by - which resolves the path before judging it, so a
// symbolic link out of the roots and a path inside a retention area are refused here rather
// than met at the pipeline's door.
func (w *Watches) offer(path string) {
	resolved, bad := w.eng.Eligibility().Judge(path)
	if bad != nil {
		w.log.Debug("not offering a watched path", "file", path, "rule", bad.Rule, "detail", bad.Detail)
		return
	}
	select {
	case w.ch <- resolved:
	default:
		w.log.Warn("the watch's offer queue is full, so this settled path was DROPPED rather than waited on; "+
			"the file is still in the library and the next interval scan finds it",
			"file", resolved, "queue_capacity", cap(w.ch))
	}
}

// drain is one worker. The ctx check before processing is what makes a cancelled watch
// drop its backlog rather than work through it: a receive that wins the race with the
// cancellation still hands back a path, and starting an encode at that point would be
// starting work the shutdown has already decided not to do.
func (w *Watches) drain(ctx context.Context, worker string) {
	defer w.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case p := <-w.ch:
			if ctx.Err() != nil {
				return
			}
			w.process(ctx, worker, p)
		}
	}
}

// process hands ONE settled path to the pipeline, through the same exported door a scan's
// worker uses. It decides nothing about the file - the skip guards, the gates, the claim
// and the swap discipline are all on the other side of that door, which is precisely why
// the watch reaches them without holding a second copy of any of them.
func (w *Watches) process(ctx context.Context, worker, path string) {
	if err := w.eng.ProcessFile(ctx, worker, path); err != nil {
		w.log.Warn("a watched file ended with an error", "file", path, "err", err)
	}
}

// reportSourceError states what the event source itself reported. The platform's queue
// overflowing is the case: events were dropped, and what covers the gap is the same thing
// that covers a restart and a rename nobody saw - the periodic scan.
func (w *Watches) reportSourceError(err error) {
	w.log.Warn("the event source reported an error, so events may have been missed; the periodic scan "+
		"covers every watched root in full and is what finds anything the watch did not",
		"dependency", "github.com/fsnotify/fsnotify", "err", err,
		"next", "the interval scan (scan_interval_sec) reconciles every root")
}

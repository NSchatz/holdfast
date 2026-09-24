package engine

import (
	"cmp"
	"slices"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// The declared QUEUE ORDER (S0095): which candidate a scan offers first.
//
// It decides SEQUENCE and nothing else. Membership is settled before anything here runs -
// the coverage bound, IsSourceName, the path filters and offered() have all already spoken -
// so every path this file receives is offered, and the only question is when.
//
// TWO SHAPES, and the difference between them is the whole design.
//
// `path`, the default, is the enumeration's own hand-out order (docs/enumeration.md). The
// sequence IS the traversal, so this file is not in the way at all: nothing is held per
// candidate, no metadata is read, and the first file reaches a worker while the library is
// still being listed. An install that predates this key is byte-for-byte the build it was.
//
// Every other order needs a KEY the traversal does not supply, so it cannot be produced
// without seeing every candidate first. What is held while that happens is a (key, path)
// pair per candidate and nothing else - not the listing, not an os.FileInfo, not the
// fingerprint - which is the shape S0099's streamed enumeration left room for and the bound
// docs/enumeration.md records the measurement against.
//
// THE KEY IS READ ONCE PER CANDIDATE, through the same seam every other pre-claim attribute
// read goes through (Engine.stat), so what a pass costs is a number a case can COUNT rather
// than a claim about a profile. `path` reads nothing at all.

// candidate is one file waiting to be offered under a keyed order: its ordering key and its
// path, and the one bit that says whether the key could be read at all.
//
// It holds no more than that ON PURPOSE. The fingerprint, the size AND the modification time
// together, or the os.FileInfo the key came out of would each be convenient later and each
// would multiply what a library-sized queue weighs, so the read is taken, reduced to one
// int64 and dropped.
type candidate struct {
	// key is what the configured order sorts on: the size in bytes, or the modification
	// time in whole Unix seconds. Meaningless when unread is true.
	key int64
	// path is the file, exactly as the enumeration produced it.
	path string
	// unread marks a candidate whose key could not be read. It sorts after every candidate
	// whose key WAS read, rather than being dropped: this decides sequence, and a file
	// silently removed from a queue is a file that is never processed (AC-10).
	unread bool
}

// enumerateOrdered is the enumeration as the scan and the read-only plan pass both drive it,
// with the configured queue order applied. It is the ONE place that order is imposed, so a
// plan cannot predict a sequence the scan will not work in.
//
// Under `path` it is exactly enumerateStream and adds nothing - not a wrapper that happens to
// preserve the order, the same call - because the property that matters there is that
// NOTHING is added: no buffering, no per-candidate read, no delay before the first file
// reaches a worker.
//
// Under a keyed order the enumeration runs to completion into a slice of (key, path) pairs,
// the slice is sorted, and the pairs are then offered in that order. The sink's own stop
// signal is honoured in both phases, so a pause or a cancellation during the listing stops
// the listing and one during the feed stops the feed.
func (e *Engine) enumerateOrdered(pass *listings, to sink) map[string]bool {
	order := e.Cfg.EffectiveQueueOrder()
	if order == config.QueueOrderPath {
		return e.enumerateStream(pass, to)
	}

	var queue []candidate
	observed := e.enumerateStream(pass, sink{
		offer: func(p string) bool {
			queue = append(queue, e.candidateFor(order, p))
			return true
		},
		// Asked before each directory is listed, exactly as it is when the enumeration
		// feeds the workers directly: a scan paused while it is collecting keys stops
		// collecting, and the directories it never reached are never reported as observed.
		stopped: to.stopped,
		// A temp is told of as it is listed, in either order: it is never queued.
		temp: to.temp,
	})

	sortQueue(queue, order)
	for _, c := range queue {
		if to.halted() || !to.offer(c.path) {
			break
		}
	}
	return observed
}

// candidateFor reads one candidate's ordering key. The read is ONE stat-family call through
// the engine's own seam, and it is the only metadata this ordering ever reads.
//
// A key that cannot be read is not a reason to drop the file or to fail the pass. The file
// vanished between the listing and here, or this process may not look at it - both are
// conditions a scan must survive - so the candidate is marked unread, ordered after every
// candidate whose key WAS read, and still offered (AC-10). The fail-safe direction on a
// question about SEQUENCE is to answer it late rather than to answer it wrongly.
func (e *Engine) candidateFor(order, path string) candidate {
	fi, err := e.stat(path)
	if err != nil {
		e.reportUnreadableOrderingKey(order, path, err)
		return candidate{path: path, unread: true}
	}
	at := probe.AttributesOf(fi)
	switch order {
	case config.QueueOrderLargest, config.QueueOrderSmallest:
		return candidate{key: at.SizeBytes, path: path}
	default:
		return candidate{key: at.MTimeUnix, path: path}
	}
}

// reportUnreadableOrderingKey states a candidate this pass could not order: WHICH file, WHAT
// was tried and WHAT HAPPENS NEXT (observability O4).
//
// It is `warn` and not `error` (observability O3): the pass continues, the file is still
// offered, and no human has to act for this scan to finish. What it costs is stated rather
// than implied - the file is offered late instead of in the operator's chosen order - because
// "holdfast processed my library largest-first except for these four" is otherwise invisible.
func (e *Engine) reportUnreadableOrderingKey(order, path string, err error) {
	e.Log.Warn("a candidate's ordering key could not be read, so this scan cannot place it in the "+
		"configured order",
		"file", path,
		"queue_order", order,
		"operation", "read the file's size and modification time to order the queue",
		"err", errText(err),
		"next", "the file is still offered to a worker this pass, after every candidate whose key "+
			"was read and in path order among the others that could not be read")
}

// sortQueue puts the candidates into the configured order, in place.
//
// THE ORDER IS TOTAL (AC-4). Two candidates with the same key break on the full path
// ascending, and a path appears exactly once in one pass, so there is no pair this
// comparison leaves unordered - which is what makes two scans over an unchanged library
// offer the same files in the same sequence under every value of the key.
//
// Candidates whose key could not be read come last WHATEVER the order, and are ordered among
// themselves by path. Placing them by a key nobody read would be inventing one.
func sortQueue(queue []candidate, order string) {
	descending := order == config.QueueOrderLargest || order == config.QueueOrderNewest
	slices.SortFunc(queue, func(a, b candidate) int {
		if a.unread != b.unread {
			if a.unread {
				return 1
			}
			return -1
		}
		if !a.unread && a.key != b.key {
			if descending {
				return cmp.Compare(b.key, a.key)
			}
			return cmp.Compare(a.key, b.key)
		}
		return cmp.Compare(a.path, b.path)
	})
}

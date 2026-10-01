package engine

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/queuekey"
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

// candidate is one file waiting to be offered under a keyed order: its ordering key, its
// queue priority and its path, and the one bit that says whether the key could be read.
//
// It holds no more than that ON PURPOSE. The fingerprint, the size AND the modification time
// together, the probe snapshot or the os.FileInfo the key came out of would each be
// convenient later and each would multiply what a library-sized queue weighs, so the read is
// taken, reduced to one int64 (and the priority to one int) and dropped.
type candidate struct {
	// key is what the configured order sorts on: the size in bytes, the modification time
	// in whole Unix seconds, the estimated bytes saved per hour of work, or - under `path`
	// with a priority configured - the candidate's position in the traversal. Meaningless
	// when unread is true.
	key int64
	// priority is the file's queue priority (config.PriorityOf): higher is offered first,
	// ahead of the key. 0 where no priority is configured, which is every configuration
	// written before the key existed - so it orders nothing there.
	priority int
	// path is the file, exactly as the enumeration produced it.
	path string
	// unread marks a candidate whose key or priority could not be read. It sorts after
	// every candidate whose key WAS read, rather than being dropped: this decides sequence,
	// and a file silently removed from a queue is a file that is never processed (AC-10).
	unread bool
}

// enumerateOrdered is the enumeration as the scan and the read-only plan pass both drive it,
// with the configured queue order and any configured priority applied. It is the ONE place
// that order is imposed, so a plan cannot predict a sequence the scan will not work in.
//
// Under `path` with no priority written anywhere it is exactly enumerateStream and adds
// nothing - not a wrapper that happens to preserve the order, the same call - because the
// property that matters there is that NOTHING is added: no buffering, no per-candidate read,
// no delay before the first file reaches a worker.
//
// Otherwise the enumeration runs to completion into a slice of candidates, the slice is
// sorted - priority descending, then the order's key, then the full path - and the paths
// are then offered in that order. Under `path` with a priority the key is the traversal
// position, so files of equal priority keep the traversal's own sequence. The sink's own
// stop signal is honoured in both phases, so a pause or a cancellation during the listing
// stops the listing and one during the feed stops the feed - and one observed while keys are
// being PROBED stops the probing and offers nothing at all this pass (S0164 AC-13).
func (e *Engine) enumerateOrdered(pass *listings, to sink) map[string]bool {
	order := e.Cfg.EffectiveQueueOrder()
	prioritised := e.Cfg.PriorityConfigured()
	if order == config.QueueOrderPath && !prioritised {
		return e.enumerateStream(pass, to)
	}
	ctx := to.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var queue []candidate
	keys := &keyReading{order: order, log: e.Log}
	halted := false
	observed := e.enumerateStream(pass, sink{
		offer: func(p string) bool {
			c, ok := e.candidateFor(ctx, order, prioritised, p, int64(len(queue)), to, keys)
			if !ok {
				halted = true
				return false
			}
			queue = append(queue, c)
			return true
		},
		// Asked before each directory is listed, exactly as it is when the enumeration
		// feeds the workers directly: a scan paused while it is collecting keys stops
		// collecting, and the directories it never reached are never reported as observed.
		stopped: to.stopped,
		// A temp is told of as it is listed, in either order: it is never queued.
		temp: to.temp,
		// So is a source the filters or a hold-back kept out: neither is a candidate, and
		// what a keyed order sorts is only ever the set the enumeration offered.
		excluded: to.excluded,
		held:     to.held,
	})
	if halted {
		// A stop observed between two probes: the queue is partial, and a partial queue
		// sorted and fed would offer files in an order the configuration did not ask for.
		// The next pass reads every key again.
		return observed
	}
	keys.finished()

	sortQueue(queue, order)
	for _, c := range queue {
		if to.halted() || !to.offer(c.path) {
			break
		}
	}
	return observed
}

// sourceFacts is what one ordering probe established about a candidate: whether the probe
// found a video stream at all, its source facts for the savings estimate, and whether its
// dimensions were established (the height is what a banded rule's priority needs).
type sourceFacts struct {
	video  bool
	dims   bool
	source queuekey.Source
}

// orderFacts takes the ONE probe the ordering reads a candidate's source facts through:
// the seam where a case substitutes it, and otherwise the engine's own prober. withBitrate
// is false where only the height is wanted, so the bit_rate container fallback (a second
// ffprobe on a file whose stream carries none) is not paid for a figure nothing reads.
func (e *Engine) orderFacts(ctx context.Context, path string, withBitrate bool) sourceFacts {
	if e.orderFactsFn != nil {
		return e.orderFactsFn(ctx, path, withBitrate)
	}
	vp := e.Probe.VideoProps(ctx, path)
	f := sourceFacts{video: vp.Codec() != ""}
	if !f.video {
		return f
	}
	w, h, ok := vp.Dimensions()
	f.dims = ok
	f.source.Width, f.source.Height = w, h
	if d, ok := vp.DurationSec(); ok {
		f.source.DurationSec = d
	}
	if withBitrate {
		f.source.VideoKbps = vp.BitrateKbps()
	}
	return f
}

// candidateFor reads one candidate's ordering key and priority. ok is false only when the
// sink asked to stop before a probe this candidate needed: no probe starts after a stop is
// observed (S0164 AC-13).
//
// Each candidate costs AT MOST ONE read of each kind: one stat under `largest`, `smallest`,
// `newest` and `oldest` (and none under `path`), and at most one probe - its source facts
// under `savings_per_hour`, which also carry the height a banded rule's priority needs, or
// the height alone under any other order where such a priority is configured.
//
// A key that cannot be read is not a reason to drop the file or to fail the pass. The file
// vanished between the listing and here, this process may not look at it, or the probe
// established no bitrate, dimensions or duration - all conditions a scan must survive - so
// the candidate is marked unread, ordered after every candidate whose key WAS read, and
// still offered (AC-10). The fail-safe direction on a question about SEQUENCE is to answer it
// late rather than to answer it wrongly.
func (e *Engine) candidateFor(ctx context.Context, order string, prioritised bool, path string,
	seq int64, to sink, keys *keyReading) (candidate, bool) {
	c := candidate{path: path}
	root, _ := e.rootFor(path)
	needHeight := prioritised && root.PriorityNeedsSourceHeight()
	savings := order == config.QueueOrderSavingsPerHour

	var facts sourceFacts
	if savings || needHeight {
		if to.halted() {
			return candidate{}, false
		}
		facts = e.orderFacts(ctx, path, savings)
		if to.probed != nil {
			to.probed()
		}
	}

	switch order {
	case config.QueueOrderPath:
		c.key = seq
	case config.QueueOrderSavingsPerHour:
		key, err := e.savingsKey(root, path, facts)
		keys.read(err == nil)
		if err != nil {
			e.reportUnreadableOrderingKey(order, path, opSavings, err)
			return candidate{path: path, unread: true}, true
		}
		c.key = key
	default:
		fi, err := e.stat(path)
		if err != nil {
			e.reportUnreadableOrderingKey(order, path, opStat, err)
			return candidate{path: path, unread: true}, true
		}
		at := probe.AttributesOf(fi)
		switch order {
		case config.QueueOrderLargest, config.QueueOrderSmallest:
			c.key = at.SizeBytes
		default:
			c.key = at.MTimeUnix
		}
	}

	if prioritised {
		height := 0
		if needHeight {
			if !facts.video || !facts.dims {
				e.reportUnreadableOrderingKey(order, path, opPriorityHeight, errNoHeight)
				return candidate{path: path, unread: true}, true
			}
			height = facts.source.Height
		}
		c.priority = e.Cfg.PriorityOf(root, path, height)
	}
	return c, true
}

// The operations an unreadable-key record names (observability O4): what was tried.
const (
	opStat           = "read the file's size and modification time to order the queue"
	opSavings        = "probe the source's video bitrate, dimensions and duration to estimate its savings per hour"
	opPriorityHeight = "probe the source height that decides which resolution rule, and so which priority, applies"
)

// errNoHeight is the reason a banded rule's priority could not be resolved.
var errNoHeight = errors.New("the probe established no source height")

// errNoVideo is the reason a savings key could not be read for a file the probe found no
// video stream in (or could not read at all).
var errNoVideo = errors.New("the probe found no video stream")

// savingsKey is one candidate's `savings_per_hour` key, from the facts its one probe read
// and the configuration that would decide it: the root's profile with the first matching
// rule laid over it (the height is in the facts), the job's encode settings for its
// bitrate target, any `max_height` ceiling for the output picture, and `remux_only`.
func (e *Engine) savingsKey(root config.Root, path string, f sourceFacts) (int64, error) {
	if !f.video {
		return 0, errNoVideo
	}
	if !f.dims {
		return 0, errors.New("the probe established no picture dimensions")
	}
	prof := root.Profile.WithRules(f.source.Height)
	ts := e.Cfg.TranscodeIn(prof, path)
	target := queuekey.Target{BitrateKbps: ts.BitrateKbps, RemuxOnly: prof.RemuxOnlyEnabled()}
	if scale := prof.DownscaleFor(f.source.Width, f.source.Height); scale.Enabled() {
		target.Width, target.Height = scale.Width, scale.Height
	}
	est, err := queuekey.Default.Estimate(f.source, target)
	if err != nil {
		return 0, err
	}
	return est.BytesPerHour, nil
}

// keyReading is the progress of one pass's `savings_per_hour` key reading: it probes every
// candidate before the first is offered, which on a large library is minutes, so it says so
// at least once per keyProgressEvery candidates and once on finishing (S0164 AC-14). Under
// every other order it records nothing.
type keyReading struct {
	order          string
	log            *slog.Logger
	readN, unreadN int
}

// keyProgressEvery is how many candidates pass between two progress records.
const keyProgressEvery = 1000

// read counts one candidate whose key was (ok) or was not read.
func (k *keyReading) read(ok bool) {
	if ok {
		k.readN++
	} else {
		k.unreadN++
	}
	if n := k.readN + k.unreadN; n%keyProgressEvery == 0 {
		k.log.Info("reading savings_per_hour ordering keys: every candidate is probed before the first is offered",
			"queue_order", k.order, "candidates_read", n, "keys_read", k.readN, "keys_unread", k.unreadN)
	}
}

// finished states, before the first candidate is offered, how many keys were read and how
// many could not be.
func (k *keyReading) finished() {
	if k.order != config.QueueOrderSavingsPerHour {
		return
	}
	k.log.Info("savings_per_hour ordering keys read; offering candidates in that order",
		"queue_order", k.order, "keys_read", k.readN, "keys_unread", k.unreadN,
		"next", "candidates whose key could not be read are offered after every other, in path order")
}

// reportUnreadableOrderingKey states a candidate this pass could not order: WHICH file, WHAT
// was tried and WHAT HAPPENS NEXT (observability O4).
//
// It is `warn` and not `error` (observability O3): the pass continues, the file is still
// offered, and no human has to act for this scan to finish. What it costs is stated rather
// than implied - the file is offered late instead of in the operator's chosen order - because
// "holdfast processed my library largest-first except for these four" is otherwise invisible.
func (e *Engine) reportUnreadableOrderingKey(order, path, operation string, err error) {
	e.Log.Warn("a candidate's ordering key could not be read, so this scan cannot place it in the "+
		"configured order",
		"file", path,
		"queue_order", order,
		"operation", operation,
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
	descending := order == config.QueueOrderLargest || order == config.QueueOrderNewest ||
		order == config.QueueOrderSavingsPerHour
	slices.SortFunc(queue, func(a, b candidate) int {
		if a.unread != b.unread {
			if a.unread {
				return 1
			}
			return -1
		}
		// Priority first, higher first. Every candidate carries 0 where none is configured,
		// so this compares equal and the order below is exactly what it was.
		if !a.unread && a.priority != b.priority {
			return cmp.Compare(b.priority, a.priority)
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

package config

import (
	"fmt"
	"strings"
)

// The order a scan offers its candidate files to the workers in.
//
// It decides SEQUENCE and never membership: every file the enumeration would have offered is
// still offered, and the coverage bound, the path filters and the record-based hold-backs
// decide that set exactly as they did before this key existed.
//
// `path` is the default and is what this tool has always done - the enumeration's own
// hand-out order, directory by directory in entry-name order (docs/enumeration.md). It is
// the one value that needs no key: the sequence IS the traversal, so nothing is held per
// candidate and the first file reaches a worker while the library is still being listed.
// Every other value needs one key read per candidate, so it holds that key and the path
// until the listing is finished.
const (
	// QueueOrderPath is the enumeration's own hand-out order, and the default.
	QueueOrderPath = "path"
	// QueueOrderLargest offers the biggest source first: the most reclaimed bytes per hour
	// of CPU, which is the figure this tool exists to move.
	QueueOrderLargest = "largest"
	// QueueOrderSmallest offers the smallest source first: the most complete verdicts per
	// hour, which is what validating a new configuration wants.
	QueueOrderSmallest = "smallest"
	// QueueOrderNewest offers the most recently modified source first.
	QueueOrderNewest = "newest"
	// QueueOrderOldest offers the least recently modified source first.
	QueueOrderOldest = "oldest"
)

// queueOrderKey is the configuration key, spelled once so the refusal, the defaults layer
// and the known-key set cannot drift apart.
const queueOrderKey = "queue_order"

// QueueOrders is the closed set of accepted values, in the order every refusal names them.
// A value outside it refuses to START (cli L7): a scan that began under an unreadable order
// would have to guess one, and a guess is how a library gets processed in a sequence nobody
// asked for.
var QueueOrders = []string{
	QueueOrderPath, QueueOrderLargest, QueueOrderSmallest, QueueOrderNewest, QueueOrderOldest,
}

// QueueOrderList renders the accepted set for a message, so a refusal and the shipped
// example configuration name the same five values in the same order.
func QueueOrderList() string { return strings.Join(QueueOrders, "|") }

// ValidQueueOrder reports whether v is one of the five accepted values. The empty string is
// NOT one of them: the defaults layer fills an ABSENT key, so an empty value in a loaded
// configuration is one the operator wrote, and a written key that means nothing is a typo
// rather than a request for the default.
func ValidQueueOrder(v string) bool {
	for _, o := range QueueOrders {
		if v == o {
			return true
		}
	}
	return false
}

// EffectiveQueueOrder is the resolved order: the configured value, or `path` where there is
// none. A Config assembled by hand rather than by Load carries no value, and resolving it
// here is what makes such a Config behave exactly as one whose file says nothing - which is
// every install that predates this key.
func (c *Config) EffectiveQueueOrder() string {
	if c.QueueOrder == "" {
		return QueueOrderPath
	}
	return c.QueueOrder
}

// QueueOrderNeedsKey reports whether the resolved order has to read a per-candidate ordering
// key. It is false for `path` alone, which is the whole of why that order costs no metadata
// inspection and holds nothing per candidate.
func (c *Config) QueueOrderNeedsKey() bool { return c.EffectiveQueueOrder() != QueueOrderPath }

// renderQueueOrder is how a raw configured value is named back to the operator. A key
// written with no value at all arrives as nil, and "" is what the operator wrote as far as
// this key is concerned: nothing.
func renderQueueOrder(raw any) string {
	if raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	return fmt.Sprint(raw)
}

// queueOrderRefusal names BOTH the value that was rejected and the five that are accepted.
// An operator who has just been refused needs the spelling they should have written, and a
// message naming only one of the two makes them go and find the other.
func queueOrderRefusal(v string) error {
	return fmt.Errorf("%s %q is not one of %s: it decides the ORDER in which a scan offers "+
		"files to its workers, never which files are offered. Omit the key for the default %q",
		queueOrderKey, v, QueueOrderList(), QueueOrderPath)
}

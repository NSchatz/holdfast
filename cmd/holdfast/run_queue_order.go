package main

// `run --queue-order <order>` (S0174): the configured queue order, overridden for one run.
//
// run_bounded.go says the bounded flags are not configuration, and this one is the
// deliberate exception the spec rules on: queue order decides SEQUENCE and never
// membership, no terminal row, profile digest or decision input records it, so a run under
// an overridden order decides every file exactly as the configured one would - it only
// reaches them in a different order. What keeps it auditable is that the startup record
// names the order in force AND where it came from (queueOrderSourceCLI or
// queueOrderSourceConfig), so an overridden run is never mistaken for a configured one.
//
// WHERE THE OVERRIDE IS APPLIED, and on what value. It is applied in cmdRun after
// loadConfig has returned, which is after config.Load refused an invalid queue_order the
// file or HOLDFAST_QUEUE_ORDER carried and after Validate refused it again. So a broken
// configured value still refuses the run with exit 1 naming the key, whatever the flag
// says (AC-15): the override never reaches the value those checks read. What it replaces
// is the in-memory Config's QueueOrder, and nothing else - the file is never written
// (AC-12). Precedence is therefore flag, then HOLDFAST_QUEUE_ORDER, then the file, then
// the default, the last three being config.Load's own layering.

import (
	"fmt"
	"io"

	"github.com/NSchatz/holdfast/internal/config"
)

// Where the queue order in force came from, as the startup record names it.
const (
	// queueOrderSourceCLI is `--queue-order` on this invocation.
	queueOrderSourceCLI = "cli"
	// queueOrderSourceConfig is the configuration: the file, HOLDFAST_QUEUE_ORDER, or the
	// default where neither says anything.
	queueOrderSourceConfig = "config"
)

// resolveQueueOrder applies a typed `--queue-order` to cfg for this run, or refuses the
// invocation. It returns where the order in force came from and the exit code: exitOK to
// carry on, exitUsage for a value the queue_order key does not accept.
//
// The accepted set is config.QueueOrders, read here and never restated, so a value the key
// learns is a value this flag accepts with no edit to this file. The comparison is exact:
// `Smallest` and `" "` are not orders, exactly as they are not in the file.
func resolveQueueOrder(cfg *config.Config, f boundFlags, stderr io.Writer) (string, int) {
	if !f.typed(flagQueueOrder) {
		return queueOrderSourceConfig, exitOK
	}
	v := *f.queueOrder
	if !config.ValidQueueOrder(v) {
		fmt.Fprintf(stderr, "holdfast: --%s: %q is not one of %s\n", flagQueueOrder, v, config.QueueOrderList())
		return queueOrderSourceConfig, exitUsage
	}
	cfg.QueueOrder = v
	return queueOrderSourceCLI, exitOK
}

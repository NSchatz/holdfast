package server

import (
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// A job row's `priority` (docs/design/queue-order.md#priority).
//
// The queue orders candidates by a priority the CONFIGURATION names, and until now nothing
// on the read surface said what a file's priority was, so an operator watching a queue
// could not tell why one file was offered ahead of another. The rows now say.
//
// It is a display of the configuration and nothing more:
//
//   - it is computed at projection time from the path and the source height the row already
//     records. Reading it opens no file and runs no probe;
//   - it is written to no row, enters no decision input and no digest, and orders nothing.
//     The queue's own order is the engine's, taken from the same Config.PriorityOf;
//   - nothing sets it. There is no route that takes a priority, and there is not to be one:
//     a priority is declared in the configuration, in git, and an API that moved a file
//     ahead of the declared order was declined by the owner.

// PriorityResolver answers one row's queue priority: nil where it is not known.
// sourceHeight is the height the row records, nil where it records none.
type PriorityResolver func(path string, sourceHeight *int) *int

// SetPriority wires the resolver the row projections read `priority` from. Set it once,
// before serving, the way the server's own hooks are set. With none wired every row's
// `priority` is null.
func (h *Hub) SetPriority(resolve PriorityResolver) { h.priority = resolve }

// ConfigPriority is the resolver over one configuration: what Config.PriorityOf answers for
// the file, and nil in the two cases where that would be a guess.
//
// A path under NO configured root has no root to take a priority from, and no rule list to
// be decided by. The engine decides such a file under the top-level profile, but a row for
// one is a row this configuration no longer describes - a root that was removed - and
// reporting 0 for it would read as "this file is queued at the default priority".
//
// A root whose rules band on the source's height and name a priority gives a file the
// priority of the band its height falls in. A row that records no height (a pending row
// nothing has probed) cannot be placed in a band, and PriorityOf with a height of 0 would
// answer for a band the file may not be in. That is null too.
func ConfigPriority(cfg config.Config) PriorityResolver {
	return func(path string, sourceHeight *int) *int {
		root, ok := cfg.RootFor(path)
		if !ok {
			return nil
		}
		height := 0
		if root.PriorityNeedsSourceHeight() {
			if sourceHeight == nil || *sourceHeight < 1 {
				return nil
			}
			height = *sourceHeight
		}
		p := cfg.PriorityOf(root, path, height)
		return &p
	}
}

// rowDTOs projects job rows onto the wire with each row's priority filled in. Every
// projection this server serves goes through it - the queue, the history, the stream and
// the ledger search - so no two of them can disagree about a file's priority.
func (h *Hub) rowDTOs(jobs []store.Job) []jobDTO {
	dtos := toDTOs(jobs)
	if h.priority == nil {
		return dtos
	}
	for i := range dtos {
		dtos[i].Priority = h.priority(jobs[i].Path, jobs[i].Outcome.SourceHeight)
	}
	return dtos
}

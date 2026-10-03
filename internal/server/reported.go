package server

import "github.com/NSchatz/holdfast/internal/store"

// SetLiveEngine says whether the process serving this hub runs with `dry_run: false`.
//
// Under a live engine the per-status counts on GET /api/summary and on the SSE snapshot
// report a `would-transcode` row inside `pending` (store.ReportedCounts): it is a row a
// run will claim from the start, and a dry run's conclusion sitting beside real states
// told an operator nothing about what the engine would do. It changes what is REPORTED
// and nothing else - no row is rewritten, and a claimed file still passes every guard.
//
// It is a setter and not a constructor argument so that NewHub, and every caller that
// never calls this, reports the counts exactly as the ledger holds them. `holdfast serve`
// is the one caller that opts in, from its loaded configuration. Set it before serving.
func (h *Hub) SetLiveEngine(live bool) { h.live.Store(live) }

// reportedCounts projects the ledger's per-status counts onto the wire under the one
// shared vocabulary rule. The summary endpoint and the snapshot both go through here, so a
// client that polls sees the keys a client that subscribes sees.
func (h *Hub) reportedCounts(sum map[store.Status]int) map[string]int {
	reported := store.ReportedCounts(sum, h.live.Load())
	counts := make(map[string]int, len(reported))
	for st, n := range reported {
		counts[string(st)] = n
	}
	return counts
}

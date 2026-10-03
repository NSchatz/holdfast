package store

// ReportedCounts is the ONE rule for the per-status vocabulary the queue-depth surfaces
// report: the `summary` map of GET /api/summary, the same map on the SSE snapshot and the
// `holdfast_queue_depth{state}` gauge. All three call it, so none of them can report a
// state another does not.
//
// live says the process serving the surface runs with `dry_run: false`. A
// `would-transcode` row is the one re-claimable terminal state: under a live engine it is
// work a run will claim from the start, exactly as a `pending` row is, and it is counted
// inside `pending` with no `would-transcode` key reported. Under a dry run, and for every
// caller that does not say (live false), the counts go out as the ledger holds them.
//
// It is a rule about what is REPORTED and nothing else. sum is never modified, no row is
// read or written, and the stored status of every such row stays `would-transcode`: a live
// run that claims one still passes the file through every skip guard before any encode.
//
// No key is fabricated: with no `would-transcode` row there is nothing to fold, and a
// ledger with no pending work reports no `pending` key, as the ledger read itself does.
func ReportedCounts(sum map[Status]int, live bool) map[Status]int {
	out := make(map[Status]int, len(sum))
	for st, n := range sum {
		if live && st == WouldTranscode {
			if n > 0 {
				out[Pending] += n
			}
			continue
		}
		out[st] += n
	}
	return out
}

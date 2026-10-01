package engine

// HealthSources hands every source a scan would offer to offer, in the enumeration's own
// order, for the library health sweep (docs/design/health-sweep.md#health-sweep). It stops
// when stop returns true or offer returns false.
//
// It is the SAME traversal a scan streams - the coverage set, the path filters, the
// retention area, the work-in-progress names and the record-based hold-backs all apply -
// so a sweep reads exactly the set the pipeline would be offered and nothing else. It
// lists every directory itself and never takes the startup walk's carried listings, which
// belong to the first scan.
func (e *Engine) HealthSources(stop func() bool, offer func(path string) bool) {
	e.enumerateStream(newListings(nil), sink{offer: offer, stopped: stop})
}

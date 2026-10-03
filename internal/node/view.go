package node

import (
	"sort"
	"time"
)

// The hub's in-memory view of its nodes, for reporting (GET /api/nodes).
//
// It is a READ of what the hub already holds - the polls waiting for work, the leases it
// knows to be live, the cool-offs in force - and of one thing it remembers only for this:
// the mode and the encoders each node's most recent poll stated. Nothing here is on any
// grant's path. The view is taken under the hub's own mutex, which is never held across
// I/O, so reading it cannot wait on a ledger write or an upload, and it changes nothing.

// maxSeenNodes bounds how many node names the hub remembers a last poll for. A deployment
// has a handful of nodes; the bound is here so a holder of the node token cannot grow the
// server's memory by polling under name after name. The node seen longest ago is forgotten
// first.
const maxSeenNodes = 256

// seenNode is what a node's most recent poll stated.
type seenNode struct {
	mode     string
	encoders []string
	at       time.Time
}

// sawLocked remembers what a node's poll stated. h.mu is held.
func (h *Hub) sawLocked(node, mode string, encoders []string) {
	if _, known := h.seen[node]; !known && len(h.seen) >= maxSeenNodes {
		h.forgetOldestLocked()
	}
	h.seen[node] = seenNode{mode: mode, encoders: append([]string(nil), encoders...), at: h.o.Now()}
}

// forgetOldestLocked drops the node seen longest ago, the lesser name among equals so the
// choice does not depend on map order. h.mu is held.
func (h *Hub) forgetOldestLocked() {
	oldest, found := "", false
	for name, s := range h.seen {
		o := h.seen[oldest]
		if !found || s.at.Before(o.at) || (s.at.Equal(o.at) && name < oldest) {
			oldest, found = name, true
		}
	}
	delete(h.seen, oldest)
}

// NodeView is one node as the hub knows it right now.
type NodeView struct {
	// Node is the name the node polls under.
	Node string
	// Mode is the mode its most recent poll asked for work in (ModeMapped or ModeHTTP),
	// and "" where the hub has seen no poll from it: a node known only by a lease a
	// restart recovered, for example.
	Mode string
	// Encoders is what that poll reported, nil where the hub has seen none.
	Encoders []string
	// Waiting reports whether a poll of the node's is open: queued for work, or reserved
	// for a job that has not been granted yet.
	Waiting bool
	// CoolingUntil is the instant until which the node is offered nothing, and the zero
	// time when it is not cooling off.
	CoolingUntil time.Time
	// LiveLeases counts the leases in a live state the hub holds for the node.
	LiveLeases int
}

// View is every node the hub knows of, by name ascending: every node whose poll it
// remembers or holds, every node with a live lease, and every node cooling off.
func (h *Hub) View() []NodeView {
	h.mu.Lock()
	defer h.mu.Unlock()
	nodes := map[string]*NodeView{}
	at := func(name string) *NodeView {
		v := nodes[name]
		if v == nil {
			v = &NodeView{Node: name}
			nodes[name] = v
		}
		return v
	}
	for name, s := range h.seen {
		v := at(name)
		v.Mode, v.Encoders = s.mode, append([]string{}, s.encoders...)
	}
	for _, p := range h.polls {
		at(p.node).Waiting = true
	}
	for t := range h.tickets {
		// A reserved poll whose long-poll ran out is gone: its node was told there is no
		// work, and the ticket is only waiting to be spent.
		if t.p.state == pollReserved {
			at(t.p.node).Waiting = true
		}
	}
	for _, name := range h.live {
		at(name).LiveLeases++
	}
	for name := range h.cooling {
		if c := h.coolingLocked(name); c != nil {
			at(name).CoolingUntil = c.until
		}
	}
	out := make([]NodeView, 0, len(nodes))
	for _, v := range nodes {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

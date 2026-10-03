package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/store"
)

// GET /api/nodes: the worker nodes this server knows of and the leases it has granted them.
//
// It is a READ, in the read group, behind the same gate as /api/queue: open unless
// `server_read_token` is set, then the read token or the control token. The node token does
// NOT open it - a worker's credential can lease and upload and can read nothing - and it
// does not sit under the lease prefix, where that credential is the gate.
//
// What it serves is bounded by what it is for: which nodes there are, what each is doing,
// and how each lease stood. It never serves a lease id, a working path, a digest or a
// token. A lease id is the capability a heartbeat, an upload and a completion are
// authorised by; the working path is where the server will take an upload; neither is
// something a reader of this page has any use for, and the first is one a reader must not
// have. The DTOs below simply have no field for them.
//
// It grants, ends, adopts and re-opens nothing: the ledger is read through the reporting
// door, which the database refuses every write on, and the hub is asked only for a copy of
// what it holds.

// NodesReadPath is the path the read is served at. It is `/api/nodes` and not under
// NodePathPrefix (`/api/node/v1`): that prefix is the lease protocol's, gated on the node
// token alone.
const NodesReadPath = "/api/nodes"

// nodeLeasesLimit caps the leases one response lists. The total beside them is over the
// whole lease ledger.
const nodeLeasesLimit = 200

// nodeDTO is one node on the wire. `mode` and `encoders` are what its most recent poll
// stated and are null where this process has seen no poll from it - after a restart, a node
// is known by its lease before it polls again. `cooling_until` is null while the node is
// not cooling off. None of the three is ever "" or 0 standing in for "not known".
type nodeDTO struct {
	Node         string   `json:"node"`
	Mode         *string  `json:"mode"`
	Encoders     []string `json:"encoders"`
	Waiting      bool     `json:"waiting"`
	CoolingUntil *int64   `json:"cooling_until"`
	LeasesActive int      `json:"leases_active"`
}

// nodeLeaseDTO is one lease on the wire. `ended_at` is null until the lease is terminal,
// `reason` null on a lease that did not fail or expire, and `output_bytes` null until an
// upload is admitted - an output of zero bytes is never admitted, so the ledger's zero only
// ever means "not recorded" and is never served as a size.
type nodeLeaseDTO struct {
	Node        string  `json:"node"`
	Path        string  `json:"path"`
	State       string  `json:"state"`
	Epoch       int64   `json:"epoch"`
	GrantedAt   int64   `json:"granted_at"`
	UpdatedAt   int64   `json:"updated_at"`
	ExpiresAt   int64   `json:"expires_at"`
	EndedAt     *int64  `json:"ended_at"`
	Reason      *string `json:"reason"`
	SourceBytes int64   `json:"source_bytes"`
	OutputBytes *int64  `json:"output_bytes"`
}

// nodesResponse is the body of GET /api/nodes. Both arrays are always arrays.
type nodesResponse struct {
	// Enabled says whether this server takes worker nodes at all: whether a lease hub is
	// wired. False is a server whose lease endpoints answer 503.
	Enabled     bool           `json:"enabled"`
	Now         int64          `json:"now"`
	Nodes       []nodeDTO      `json:"nodes"`
	Leases      []nodeLeaseDTO `json:"leases"`
	LeasesTotal rowTotalDTO    `json:"leases_total"`
}

// noNodesCoverage is the set the total covers on a server that takes no nodes: none, and it
// says so rather than naming a ledger nothing read.
const noNodesCoverage = "no lease: this server takes no worker nodes"

// errNoLeaseLedger is a reporting handle that holds no lease ledger. Not reachable in the
// daemon, whose reporting door is the SQLite ledger itself.
var errNoLeaseLedger = errors.New("the reporting handle holds no lease ledger")

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	out := nodesResponse{Now: time.Now().Unix(), Nodes: []nodeDTO{}, Leases: []nodeLeaseDTO{}}
	if s.nodes == nil {
		zero, age := int64(0), int64(0)
		out.LeasesTotal = rowTotalDTO{Available: true, Covers: noNodesCoverage, Cap: nodeLeasesLimit,
			AgeSeconds: &age, Count: &zero}
		writeJSON(w, http.StatusOK, out)
		return
	}
	ledger, ok := s.reads().(store.LeaseLedger)
	if !ok {
		s.fail(w, "nodes", errNoLeaseLedger)
		return
	}
	leases, err := ledger.RecentLeases(r.Context(), nodeLeasesLimit)
	if err != nil {
		s.fail(w, "nodes", err)
		return
	}
	out.Enabled = true
	out.Leases = leaseDTOs(leases)
	out.Nodes = nodeDTOs(s.nodes.View(), leases)
	out.LeasesTotal = leasesTotalOf(s, ledger.CountLeases(r.Context()))
	writeJSON(w, http.StatusOK, out)
}

// leasesTotalOf projects the lease total. One that could not be read is STATED as
// unavailable beside the leases that still ship, never reported as a count of zero.
func leasesTotalOf(s *Server, total store.RowTotal) rowTotalDTO {
	out := rowTotalDTO{Available: total.Err == nil, Covers: total.Coverage.Set, Cap: nodeLeasesLimit}
	if total.Err != nil {
		s.log.Warn("lease total unavailable (the leases still ship)", "err", total.Err)
		out.Unavailable = aggregateUnavailable
		return out
	}
	n, age := total.Count, int64(0)
	out.Count, out.AgeSeconds = &n, &age
	return out
}

// leaseDTOs projects the listed leases, in the order the ledger returned them.
func leaseDTOs(leases []store.Lease) []nodeLeaseDTO {
	out := make([]nodeLeaseDTO, 0, len(leases))
	for _, l := range leases {
		d := nodeLeaseDTO{
			Node: l.Node, Path: l.Path, State: string(l.State), Epoch: l.Epoch,
			GrantedAt: l.GrantedAt.Unix(), UpdatedAt: l.UpdatedAt.Unix(), ExpiresAt: l.ExpiresAt.Unix(),
			Reason: leaseReason(l), SourceBytes: l.SourceSize,
		}
		if !l.EndedAt.IsZero() {
			at := l.EndedAt.Unix()
			d.EndedAt = &at
		}
		if l.OutputBytes > 0 {
			n := l.OutputBytes
			d.OutputBytes = &n
		}
		out = append(out, d)
	}
	return out
}

// leaseReason is the reason word a lease row carries, or nil where it carries none.
//
// A failed lease's reason is the word its NODE stated, held to 1 to 64 characters from a-z,
// 0-9 and `_` when it was taken. A lease id is 32 characters from that same alphabet, so a
// node could state its own lease's id as its reason; such a row is served the hub's own
// word for a node-stated failure instead, and this response carries no lease id whatever a
// node wrote.
func leaseReason(l store.Lease) *string {
	reason := l.Reason
	if reason == "" {
		return nil
	}
	if reason == l.ID {
		reason = string(node.ReasonNodeFailed)
	}
	return &reason
}

// nodeDTOs is one entry per node the hub knows of or a listed lease names, by name
// ascending. A node only a lease names is one this process has seen nothing of: no mode, no
// encoders, no open poll, no cool-off and no live lease.
func nodeDTOs(view []node.NodeView, leases []store.Lease) []nodeDTO {
	out := make([]nodeDTO, 0, len(view))
	known := make(map[string]bool, len(view))
	for _, v := range view {
		known[v.Node] = true
		d := nodeDTO{Node: v.Node, Mode: nullableText(v.Mode), Encoders: v.Encoders,
			Waiting: v.Waiting, LeasesActive: v.LiveLeases}
		if !v.CoolingUntil.IsZero() {
			until := v.CoolingUntil.Unix()
			d.CoolingUntil = &until
		}
		out = append(out, d)
	}
	for _, l := range leases {
		if !known[l.Node] {
			known[l.Node] = true
			out = append(out, nodeDTO{Node: l.Node})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

package config

import (
	"fmt"
	"strings"
	"time"
)

// The worker-node keys (docs/design/nodes.md#leases). Two groups: what the SERVER that owns
// the library reads, and what a `holdfast worker` reads. Nodes are off until node_token is
// written: with it unset the server grants no lease and /api/node/v1 answers 403.
const (
	nodeTokenKey            = "node_token"
	nodeLeaseTTLKey         = "node_lease_ttl_sec"
	nodeMaxLeasesKey        = "node_max_leases"
	nodeMaxLeasesPerNodeKey = "node_max_leases_per_node"
	nodeMaxTransfersKey     = "node_max_transfers"
	nodeGateSlotsKey        = "node_gate_slots"

	workerServerKey  = "worker_server"
	workerNameKey    = "worker_name"
	workerSlotsKey   = "worker_slots"
	workerPathMapKey = "worker_path_map"
	workerWorkDirKey = "worker_work_dir"

	workerModeKey         = "worker_mode"
	workerInsecureHTTPKey = "worker_insecure_http"
	workerTLSCAKey        = "worker_tls_ca"

	serverTLSCertKey = "server_tls_cert"
	serverTLSKeyKey  = "server_tls_key"
)

// The two ways a worker reaches a leased source (docs/design/nodes.md#http-mode).
const (
	// WorkerModeMapped reads the source through the worker's own mount of the library.
	WorkerModeMapped = "mapped"
	// WorkerModeHTTP downloads the source from the server on the lease.
	WorkerModeHTTP = "http"
)

// ServerTLSKeyKey is the TLS private key's key, for the caller that hands the resolved
// value to the listener, and WorkerTLSCAKey the key a refusal about the worker's extra
// trust names.
const (
	ServerTLSKeyKey  = serverTLSKeyKey
	ServerTLSCertKey = serverTLSCertKey
	WorkerTLSCAKey   = workerTLSCAKey
)

// NodeTokenKey is the node credential's key, for the callers that hand the resolved value
// to the server and to a worker.
const NodeTokenKey = nodeTokenKey

// The shipped defaults of the server-side knobs. Every one is ASSUMED: nobody has measured
// a node deployment, and each stays marked so until a report replaces it
// (docs/design/nodes.md).
const (
	// DefaultNodeLeaseTTLSec is how long a lease lives without a heartbeat. ASSUMED.
	DefaultNodeLeaseTTLSec = 60
	// DefaultNodeMaxLeases caps the live leases across every node. ASSUMED.
	DefaultNodeMaxLeases = 4
	// DefaultNodeMaxLeasesPerNode caps the live leases one node holds. ASSUMED.
	DefaultNodeMaxLeasesPerNode = 1
	// DefaultNodeMaxTransfers caps the uploads in flight across every node. ASSUMED.
	DefaultNodeMaxTransfers = 2
	// DefaultNodeGateSlots is how many node outputs the server gates at once. ASSUMED.
	DefaultNodeGateSlots = 1
	// DefaultWorkerSlots is how many encodes one worker runs at once.
	DefaultWorkerSlots = 1
)

// The accepted ranges. A lease shorter than MinNodeLeaseTTLSec leaves its heartbeat
// (a quarter of it) under a second, and one longer than a day is not a liveness bound.
const (
	MinNodeLeaseTTLSec = 4
	MaxNodeLeaseTTLSec = 86400
	// MaxNodeCount bounds each of the four counts and worker_slots. A value past it is a
	// typo, not a plan.
	MaxNodeCount = 256
)

// nodeHeartbeatDivisor is the fraction of the TTL a node heartbeats at: every TTL/4.
const nodeHeartbeatDivisor = 4

// MaxNodeNameLen bounds a node's name.
const MaxNodeNameLen = 64

// NodesEnabled reports whether a node credential reference is configured, which is what
// turns the lease endpoints on.
func (c *Config) NodesEnabled() bool { return strings.TrimSpace(c.NodeToken) != "" }

// orDefault is v, and def where v is zero: a Config built without Load carries zeros, and
// a zero written in the file means the default, as it does for workers.
func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// NodeLeaseTTL is how long a lease lives without a heartbeat.
func (c *Config) NodeLeaseTTL() time.Duration {
	return time.Duration(orDefault(c.NodeLeaseTTLSec, DefaultNodeLeaseTTLSec)) * time.Second
}

// NodeHeartbeat is how often a node renews its lease: a quarter of the TTL.
func (c *Config) NodeHeartbeat() time.Duration { return c.NodeLeaseTTL() / nodeHeartbeatDivisor }

// EffectiveNodeMaxLeases is the cap on live leases across every node.
func (c *Config) EffectiveNodeMaxLeases() int {
	return orDefault(c.NodeMaxLeases, DefaultNodeMaxLeases)
}

// EffectiveNodeMaxLeasesPerNode is the cap on live leases one node holds.
func (c *Config) EffectiveNodeMaxLeasesPerNode() int {
	return orDefault(c.NodeMaxLeasesPerNode, DefaultNodeMaxLeasesPerNode)
}

// EffectiveNodeMaxTransfers is the cap on uploads in flight across every node.
func (c *Config) EffectiveNodeMaxTransfers() int {
	return orDefault(c.NodeMaxTransfers, DefaultNodeMaxTransfers)
}

// EffectiveNodeGateSlots is how many node outputs the server gates at once.
func (c *Config) EffectiveNodeGateSlots() int {
	return orDefault(c.NodeGateSlots, DefaultNodeGateSlots)
}

// EffectiveWorkerSlots is how many encodes this worker runs at once.
func (c *Config) EffectiveWorkerSlots() int { return orDefault(c.WorkerSlots, DefaultWorkerSlots) }

// EffectiveWorkerMode is the worker's mode: mapped where the key is unset, as a Config built
// without Load leaves it.
func (c *Config) EffectiveWorkerMode() string {
	if c.WorkerMode == "" {
		return WorkerModeMapped
	}
	return c.WorkerMode
}

// TLSEnabled reports whether `serve` listens with TLS: both server_tls_cert and
// server_tls_key are written. Validate has refused one without the other.
func (c *Config) TLSEnabled() bool {
	return strings.TrimSpace(c.ServerTLSCert) != "" && strings.TrimSpace(c.ServerTLSKey) != ""
}

// validateTransport refuses a transport key outside its accepted values, naming the key
// (docs/design/nodes.md#transport). It runs after the literal-credential refusal, so a
// pasted server_tls_key is reported as one and never as a missing certificate.
func (c *Config) validateTransport() error {
	cert, key := strings.TrimSpace(c.ServerTLSCert), strings.TrimSpace(c.ServerTLSKey)
	switch {
	case cert != "" && key == "":
		return fmt.Errorf("%s is set and %s is not: built-in TLS needs both, the certificate chain as a "+
			"path and its private key by reference (%s: file:/run/secrets/holdfast-tls-key). Set %s, or "+
			"remove %s to listen without TLS", serverTLSCertKey, serverTLSKeyKey, serverTLSKeyKey,
			serverTLSKeyKey, serverTLSCertKey)
	case key != "" && cert == "":
		return fmt.Errorf("%s is set and %s is not: built-in TLS needs both, the private key by reference "+
			"and the PEM certificate chain it belongs to as a path. Set %s, or remove %s to listen without TLS",
			serverTLSKeyKey, serverTLSCertKey, serverTLSCertKey, serverTLSKeyKey)
	case cert != "" && !strings.HasPrefix(cert, "/"):
		return fmt.Errorf("%s %q must be an absolute path (it starts with /) to a PEM certificate chain",
			serverTLSCertKey, cert)
	}
	if ca := strings.TrimSpace(c.WorkerTLSCA); ca != "" && !strings.HasPrefix(ca, "/") {
		return fmt.Errorf("%s %q must be an absolute path (it starts with /) to a PEM bundle of the "+
			"certificates the worker trusts beside the system roots", workerTLSCAKey, ca)
	}
	switch c.WorkerMode {
	case "", WorkerModeMapped:
	case WorkerModeHTTP:
		if len(c.WorkerPathMap) > 0 {
			return fmt.Errorf("%s is %s and %s is set: a worker in http mode downloads each source from "+
				"its server and maps no path, so the two contradict each other. Remove %s, or set %s: %s",
				workerModeKey, WorkerModeHTTP, workerPathMapKey, workerPathMapKey, workerModeKey, WorkerModeMapped)
		}
	default:
		return fmt.Errorf("%s %q is not one of %s|%s", workerModeKey, c.WorkerMode, WorkerModeMapped, WorkerModeHTTP)
	}
	return nil
}

// ValidNodeName reports whether name is one a node may give: 1 to MaxNodeNameLen
// characters from letters, digits, `.`, `_` and `-`. The name reaches the lease ledger and
// every log record about the node, so nothing that could be read as a path, a separator or
// a control character is one.
func ValidNodeName(name string) bool {
	if name == "" || len(name) > MaxNodeNameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// validateNodes refuses a node or worker key outside its accepted values, naming the key.
// It runs after the literal-credential refusal, so a pasted node_token is reported as one.
func (c *Config) validateNodes() error {
	// node_token must be a secret of its own. The reference is compared as written and
	// nothing is resolved; two references that reach one value by different routes are
	// refused at start, where the resolved values are.
	if ref := strings.TrimSpace(c.NodeToken); ref != "" {
		for _, other := range []struct{ key, value string }{
			{"server_auth_token", c.ServerAuthToken},
			{"server_read_token", c.ServerReadToken},
			{webhookTokenKey, c.WebhookToken},
		} {
			if strings.TrimSpace(other.value) == ref {
				return fmt.Errorf("%s and %s are the same reference: the node credential is the one a "+
					"worker node holds and it may only lease a job and upload its output, so it must be a "+
					"secret of its own. Point %s at a different secret", nodeTokenKey, other.key, nodeTokenKey)
			}
		}
	}
	// A zero is the key left unset on a Config built without Load, and means the default
	// exactly as it does for workers; every other value outside the range refuses.
	if ttl := c.NodeLeaseTTLSec; ttl != 0 && (ttl < MinNodeLeaseTTLSec || ttl > MaxNodeLeaseTTLSec) {
		return fmt.Errorf("%s %d must be between %d and %d seconds (0 means the default of %d; a node "+
			"heartbeats every quarter of it)", nodeLeaseTTLKey, ttl, MinNodeLeaseTTLSec, MaxNodeLeaseTTLSec,
			DefaultNodeLeaseTTLSec)
	}
	for _, n := range []struct {
		key   string
		value int
		def   int
	}{
		{nodeMaxLeasesKey, c.NodeMaxLeases, DefaultNodeMaxLeases},
		{nodeMaxLeasesPerNodeKey, c.NodeMaxLeasesPerNode, DefaultNodeMaxLeasesPerNode},
		{nodeMaxTransfersKey, c.NodeMaxTransfers, DefaultNodeMaxTransfers},
		{nodeGateSlotsKey, c.NodeGateSlots, DefaultNodeGateSlots},
		{workerSlotsKey, c.WorkerSlots, DefaultWorkerSlots},
	} {
		if n.value < 0 || n.value > MaxNodeCount {
			return fmt.Errorf("%s %d out of range (0-%d; 0 means the default of %d)", n.key, n.value, MaxNodeCount, n.def)
		}
	}

	if server := strings.TrimSpace(c.WorkerServer); server != "" {
		if err := checkTargetURL(workerServerKey, server); err != nil {
			return err
		}
	}
	if c.WorkerName != "" && !ValidNodeName(c.WorkerName) {
		return fmt.Errorf("%s must be 1 to %d characters from letters, digits, '.', '_' and '-': the "+
			"configured value is not", workerNameKey, MaxNodeNameLen)
	}
	if c.WorkerWorkDir != "" && !strings.HasPrefix(c.WorkerWorkDir, "/") {
		return fmt.Errorf("%s %q must be an absolute path (it starts with /)", workerWorkDirKey, c.WorkerWorkDir)
	}
	return c.WorkerPathMap.validate(workerPathMapKey)
}

// MapMatched is Map, and also reports whether an entry's From covered the path. A caller
// that must not take an uncovered path for a mapped one asks it; Map itself passes such a
// path through unchanged.
func (m PathMap) MapMatched(holdfastPath string) (targetPath string, matched bool) {
	return m.translate(holdfastPath, func(e PathMapEntry) (string, string) { return e.From, e.To })
}

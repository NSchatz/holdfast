package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/secret"
)

// TestSecretKeys_NodeTokenIsSecretBearingAndRefusesALiteral is the goal's credential line
// for the worker nodes: `node_token` is in the closed list of secret-bearing keys, a literal
// value is refused naming the key - in the YAML file and in HOLDFAST_NODE_TOKEN - with no
// part of the value in the refusal, a `file:` reference is accepted and resolves to the
// file's contents, and the resolved value renders as `<redacted>` wherever it is printed.
func TestSecretKeys_NodeTokenIsSecretBearingAndRefusesALiteral(t *testing.T) {
	const key = "node_token"
	const literal = "LITERAL-NODE-CREDENTIAL-MUST-NEVER-BE-ECHOED"

	if NodeTokenKey != key {
		t.Fatalf("NodeTokenKey = %q, want %q", NodeTokenKey, key)
	}
	listed := 0
	for _, k := range SecretBearingKeys {
		if k == key {
			listed++
		}
	}
	if listed != 1 {
		t.Fatalf("%s appears %d times in SecretBearingKeys, want exactly once: %v", key, listed, SecretBearingKeys)
	}

	refuses := func(t *testing.T, where string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("a literal %s %s was ACCEPTED", key, where)
		}
		var lit *secret.ErrLiteral
		if !errorsAsLiteral(err, &lit) {
			t.Errorf("a literal %s %s was refused, but not as a literal credential: %v", key, where, err)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal %s does not name %s: %v", where, key, err)
		}
		if strings.Contains(err.Error(), literal) {
			t.Errorf("the refusal %s echoes the literal value: %v", where, err)
		}
	}

	t.Run("a literal in the YAML file is refused", func(t *testing.T) {
		_, err := loadValidated(t, mediaBase+key+": "+literal+"\n")
		refuses(t, "in the YAML file", err)
	})

	t.Run("a literal in the environment is refused", func(t *testing.T) {
		t.Setenv("HOLDFAST_NODE_TOKEN", literal)
		_, err := loadValidated(t, mediaBase)
		refuses(t, "in HOLDFAST_NODE_TOKEN", err)
	})

	t.Run("absent is off", func(t *testing.T) {
		c, err := loadValidated(t, mediaBase)
		if err != nil {
			t.Fatalf("a configuration with no %s was refused: %v", key, err)
		}
		if c.NodeToken != "" || c.NodesEnabled() {
			t.Errorf("%s defaults to %q (enabled=%v), want empty and off", key, c.NodeToken, c.NodesEnabled())
		}
		if c.SecretRef(key).Configured() {
			t.Errorf("an absent %s parsed to a configured reference", key)
		}
	})

	t.Run("whitespace alone is off", func(t *testing.T) {
		c := Config{NodeToken: "  \t"}
		if c.NodesEnabled() {
			t.Error("NodesEnabled is true for a node_token holding only whitespace")
		}
	})

	t.Run("a file reference resolves and renders redacted", func(t *testing.T) {
		const plaintext = "resolved-node-credential"
		secretFile := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(secretFile, []byte(plaintext+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := loadValidated(t, mediaBase+key+": file:"+secretFile+"\n")
		if err != nil {
			t.Fatalf("a file: reference in %s was refused: %v", key, err)
		}
		if !c.NodesEnabled() {
			t.Errorf("NodesEnabled is false with %s set", key)
		}
		refs, err := c.SecretRefs()
		if err != nil {
			t.Fatalf("SecretRefs: %v", err)
		}
		if len(refs) != len(SecretBearingKeys) {
			t.Fatalf("SecretRefs returned %d references for %d keys", len(refs), len(SecretBearingKeys))
		}
		// The reference SecretRefs reports under this key is this key's own value: a raw
		// list one entry out of step with SecretBearingKeys would resolve another key's
		// secret here.
		if got := c.SecretRef(key).String(); got != "file:"+secretFile {
			t.Errorf("SecretRef(%s) = %q, want the reference as written", key, got)
		}
		set, err := secret.Resolve(context.Background(), refs)
		if err != nil {
			t.Fatalf("resolving %s: %v", key, err)
		}
		v := set.Get(key)
		if got := v.Expose(); got != plaintext {
			t.Errorf("%s resolved to %q, want the file's contents", key, got)
		}
		asJSON, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshalling the value: %v", err)
		}
		var fromJSON string
		if err := json.Unmarshal(asJSON, &fromJSON); err != nil {
			t.Fatalf("the value did not marshal to a JSON string: %s", asJSON)
		}
		for name, rendered := range map[string]string{
			"%v": fmt.Sprintf("%v", v), "%s": fmt.Sprintf("%s", v), "%+v": fmt.Sprintf("%+v", v),
			"%#v": fmt.Sprintf("%#v", v), "JSON": fromJSON,
		} {
			if strings.Contains(rendered, plaintext) {
				t.Errorf("the resolved %s rendered through %s carries the plaintext: %s", key, name, rendered)
			}
			if !strings.Contains(rendered, "<redacted>") {
				t.Errorf("the resolved %s rendered through %s is %q, want <redacted>", key, name, rendered)
			}
		}
	})
}

// TestNodeToken_MayNotShareAReferenceWithAnotherServerToken: the node credential is the one
// a worker holds, so a reference shared with the control, read or webhook token refuses,
// naming both keys.
func TestNodeToken_MayNotShareAReferenceWithAnotherServerToken(t *testing.T) {
	const shared = "file:/run/secrets/shared"
	for _, other := range []string{"server_auth_token", "server_read_token", "webhook_token"} {
		t.Run(other, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+other+": "+shared+"\nnode_token: \" "+shared+" \"\n")
			if err == nil {
				t.Fatalf("node_token sharing %s's reference was ACCEPTED", other)
			}
			if !strings.Contains(err.Error(), "node_token") || !strings.Contains(err.Error(), other) {
				t.Errorf("the refusal must name node_token and %s: %v", other, err)
			}
		})
	}
	c, err := loadValidated(t, mediaBase+"server_auth_token: file:/run/secrets/control\n"+
		"server_read_token: file:/run/secrets/read\nwebhook_token: file:/run/secrets/webhook\n"+
		"node_token: file:/run/secrets/node\n")
	if err != nil {
		t.Fatalf("four distinct references were refused: %v", err)
	}
	if !c.NodesEnabled() {
		t.Error("NodesEnabled is false with node_token set")
	}
	// The other three unset are not "the same reference" as anything.
	if _, err := loadValidated(t, mediaBase+"node_token: file:/run/secrets/node\n"); err != nil {
		t.Fatalf("a node_token with no other token was refused: %v", err)
	}
}

// TestNodeKeys_DefaultsAreTheDocumentedOnes: a configuration that says nothing about nodes
// loads the shipped (ASSUMED) defaults, and the accessors answer them.
func TestNodeKeys_DefaultsAreTheDocumentedOnes(t *testing.T) {
	c, err := loadValidated(t, mediaBase)
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeLeaseTTLSec != 60 || c.NodeMaxLeases != 4 || c.NodeMaxLeasesPerNode != 1 ||
		c.NodeMaxTransfers != 2 || c.NodeGateSlots != 1 || c.WorkerSlots != 1 {
		t.Errorf("loaded defaults: ttl=%d leases=%d per-node=%d transfers=%d gate=%d slots=%d, want 60, 4, 1, 2, 1, 1",
			c.NodeLeaseTTLSec, c.NodeMaxLeases, c.NodeMaxLeasesPerNode, c.NodeMaxTransfers, c.NodeGateSlots, c.WorkerSlots)
	}
	if c.WorkerServer != "" || c.WorkerName != "" || c.WorkerWorkDir != "" || len(c.WorkerPathMap) != 0 {
		t.Errorf("the worker keys default to %q %q %q %v, want all empty",
			c.WorkerServer, c.WorkerName, c.WorkerWorkDir, c.WorkerPathMap)
	}
	// A Config built without Load carries zeros, and each accessor answers the default.
	var zero Config
	if zero.NodeLeaseTTL() != 60*time.Second || zero.NodeHeartbeat() != 15*time.Second {
		t.Errorf("a zero Config: ttl %v heartbeat %v, want 60s and 15s", zero.NodeLeaseTTL(), zero.NodeHeartbeat())
	}
	if zero.EffectiveNodeMaxLeases() != 4 || zero.EffectiveNodeMaxLeasesPerNode() != 1 ||
		zero.EffectiveNodeMaxTransfers() != 2 || zero.EffectiveNodeGateSlots() != 1 || zero.EffectiveWorkerSlots() != 1 {
		t.Error("a zero Config's node accessors do not answer the defaults 4, 1, 2, 1, 1")
	}
	if err := zero.validateNodes(); err != nil {
		t.Errorf("a zero Config's node keys were refused: %v", err)
	}
	set := Config{NodeLeaseTTLSec: 120, NodeMaxLeases: 9, NodeMaxLeasesPerNode: 3, NodeMaxTransfers: 5,
		NodeGateSlots: 2, WorkerSlots: 7}
	if set.NodeLeaseTTL() != 2*time.Minute || set.NodeHeartbeat() != 30*time.Second {
		t.Errorf("ttl %v heartbeat %v, want 2m and 30s (a quarter of the TTL)", set.NodeLeaseTTL(), set.NodeHeartbeat())
	}
	if set.EffectiveNodeMaxLeases() != 9 || set.EffectiveNodeMaxLeasesPerNode() != 3 ||
		set.EffectiveNodeMaxTransfers() != 5 || set.EffectiveNodeGateSlots() != 2 || set.EffectiveWorkerSlots() != 7 {
		t.Error("the node accessors do not answer the configured values 9, 3, 5, 2, 7")
	}
}

// TestNodeKeys_AValueOutOfRangeRefusesNamingTheKey, with both edges of every range.
func TestNodeKeys_AValueOutOfRangeRefusesNamingTheKey(t *testing.T) {
	counts := []string{"node_max_leases", "node_max_leases_per_node", "node_max_transfers", "node_gate_slots", "worker_slots"}
	type tc struct {
		key   string
		value string
		ok    bool
	}
	cases := []tc{
		{"node_lease_ttl_sec", "0", true}, {"node_lease_ttl_sec", "4", true}, {"node_lease_ttl_sec", "86400", true},
		{"node_lease_ttl_sec", "3", false}, {"node_lease_ttl_sec", "1", false},
		{"node_lease_ttl_sec", "86401", false}, {"node_lease_ttl_sec", "-1", false},
	}
	for _, k := range counts {
		cases = append(cases, tc{k, "0", true}, tc{k, "1", true}, tc{k, "256", true}, tc{k, "257", false}, tc{k, "-1", false})
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+c.key+": "+c.value+"\n")
			if c.ok {
				if err != nil {
					t.Fatalf("%s: %s was refused: %v", c.key, c.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: %s was ACCEPTED", c.key, c.value)
			}
			if !strings.Contains(err.Error(), c.key+" "+c.value) {
				t.Errorf("the refusal must name %s and the value %s: %v", c.key, c.value, err)
			}
		})
	}
	// Each key is refused under its OWN name: a refusal that named a neighbour would send
	// an operator to the wrong line.
	for i, k := range counts {
		t.Run("named alone/"+k, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+k+": 999\n")
			if err == nil {
				t.Fatal("999 was accepted")
			}
			for j, other := range counts {
				// node_max_leases is a prefix of node_max_leases_per_node, so the check is
				// on the key followed by the value.
				if j != i && strings.Contains(err.Error(), other+" 999") {
					t.Errorf("the refusal for %s names %s: %v", k, other, err)
				}
			}
		})
	}
	t.Run("the environment form is read", func(t *testing.T) {
		t.Setenv("HOLDFAST_NODE_MAX_TRANSFERS", "7")
		c, err := loadValidated(t, mediaBase)
		if err != nil || c.NodeMaxTransfers != 7 {
			t.Fatalf("HOLDFAST_NODE_MAX_TRANSFERS=7 loaded %v (err %v)", c, err)
		}
	})
}

// TestWorkerKeys_AreValidatedNamingTheKey: the worker's address, name, working directory
// and path map.
func TestWorkerKeys_AreValidatedNamingTheKey(t *testing.T) {
	good := mediaBase + "worker_server: https://holdfast.example.test:8443/base\nworker_name: node-a.1_x\n" +
		"worker_slots: 2\nworker_work_dir: /var/lib/holdfast-worker\n" +
		"worker_path_map:\n  - {from: /mnt/media, to: /data/media}\n  - {from: /mnt/media/tv, to: /data/shows}\n"
	c, err := loadValidated(t, good)
	if err != nil {
		t.Fatalf("a complete worker configuration was refused: %v", err)
	}
	if c.WorkerServer != "https://holdfast.example.test:8443/base" || c.WorkerName != "node-a.1_x" ||
		c.WorkerSlots != 2 || c.WorkerWorkDir != "/var/lib/holdfast-worker" {
		t.Errorf("the worker keys loaded as %q %q %d %q", c.WorkerServer, c.WorkerName, c.WorkerSlots, c.WorkerWorkDir)
	}
	want := PathMap{{From: "/mnt/media", To: "/data/media"}, {From: "/mnt/media/tv", To: "/data/shows"}}
	if len(c.WorkerPathMap) != 2 || c.WorkerPathMap[0] != want[0] || c.WorkerPathMap[1] != want[1] {
		t.Errorf("worker_path_map loaded as %v, want %v in the order written", c.WorkerPathMap, want)
	}

	for _, tc := range []struct{ name, yaml, names string }{
		{"an address with no scheme", "worker_server: holdfast.example.test:8080\n", "worker_server"},
		{"an address with userinfo", "worker_server: http://user:pw@holdfast.example.test\n", "worker_server"},
		{"an address with a query", "worker_server: http://holdfast.example.test/?token=x\n", "worker_server"},
		{"a name with a slash", "worker_name: node/a\n", "worker_name"},
		{"a name with a space", "worker_name: \"node a\"\n", "worker_name"},
		{"a name 65 characters long", "worker_name: " + strings.Repeat("n", 65) + "\n", "worker_name"},
		{"a relative working directory", "worker_work_dir: work\n", "worker_work_dir"},
		{"a relative path-map source", "worker_path_map:\n  - {from: media, to: /data}\n", "worker_path_map[0].from"},
		{"a relative path-map target", "worker_path_map:\n  - {from: /media, to: data}\n", "worker_path_map[0].to"},
		{"a path map repeating a source", "worker_path_map:\n  - {from: /a, to: /b}\n  - {from: /a, to: /c}\n", "worker_path_map[1].from"},
		{"a path map with an unknown key", "worker_path_map:\n  - {from: /a, too: /b}\n", "worker_path_map[0]"},
		{"a path map that is not a list", "worker_path_map: /a:/b\n", "worker_path_map"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+tc.yaml)
			if err == nil {
				t.Fatalf("%s was ACCEPTED", tc.name)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal must name %s: %v", tc.names, err)
			}
		})
	}
	// A name of exactly the bound is accepted.
	if _, err := loadValidated(t, mediaBase+"worker_name: "+strings.Repeat("n", 64)+"\n"); err != nil {
		t.Errorf("a 64-character worker_name was refused: %v", err)
	}
	// An address with surrounding whitespace is the address.
	if _, err := loadValidated(t, mediaBase+"worker_server: \" http://127.0.0.1:8080 \"\n"); err != nil {
		t.Errorf("a worker_server with surrounding whitespace was refused: %v", err)
	}

	t.Run("worker_path_map has no environment form", func(t *testing.T) {
		t.Setenv("HOLDFAST_WORKER_PATH_MAP", "/a:/b")
		_, err := loadValidated(t, mediaBase)
		if err == nil {
			t.Fatal("HOLDFAST_WORKER_PATH_MAP was accepted")
		}
		if !strings.Contains(err.Error(), "HOLDFAST_WORKER_PATH_MAP") || !strings.Contains(err.Error(), "no environment form") {
			t.Errorf("the refusal must name the variable and say it has no environment form: %v", err)
		}
	})
}

func TestValidNodeName(t *testing.T) {
	for name, want := range map[string]bool{
		"node-a": true, "Node_1.x": true, "a": true, "0": true, "z": true, "Z": true, "A": true, "9": true,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
		"": false, "node a": false, "node/a": false, "node\n": false, "nöde": false, "a:b": false,
		"`": false, "{": false, "@": false, "[": false, "/": false, ":": false,
	} {
		if got := ValidNodeName(name); got != want {
			t.Errorf("ValidNodeName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestPathMap_MapMatchedSaysWhetherAnEntryCoveredThePath: Map passes an uncovered path
// through unchanged, and MapMatched is what tells the two apart.
func TestPathMap_MapMatchedSaysWhetherAnEntryCoveredThePath(t *testing.T) {
	m := PathMap{{From: "/mnt/media", To: "/data/media"}, {From: "/mnt/media/tv", To: "/data/shows"}}
	for _, tc := range []struct {
		in, want string
		matched  bool
	}{
		{"/mnt/media/film/a.mkv", "/data/media/film/a.mkv", true},
		{"/mnt/media/tv/s/e.mkv", "/data/shows/s/e.mkv", true},
		{"/mnt/media", "/data/media", true},
		{"/mnt/media 2/a.mkv", "/mnt/media 2/a.mkv", false},
		{"/elsewhere/a.mkv", "/elsewhere/a.mkv", false},
	} {
		got, matched := m.MapMatched(tc.in)
		if got != tc.want || matched != tc.matched {
			t.Errorf("MapMatched(%q) = %q, %v; want %q, %v", tc.in, got, matched, tc.want, tc.matched)
		}
		if plain := m.Map(tc.in); plain != got {
			t.Errorf("Map(%q) = %q and MapMatched answered %q", tc.in, plain, got)
		}
	}
	if got, matched := (PathMap{}).MapMatched("/a/b"); got != "/a/b" || matched {
		t.Errorf("an empty map answered %q, %v; want the path unchanged and unmatched", got, matched)
	}
}

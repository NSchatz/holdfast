package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
)

// TestServe_RefusesANodeTokenThatResolvesToAnotherServerToken: two DIFFERENT references
// that reach one secret pass config.Validate, which compares references. `serve` refuses
// them once resolved, naming both keys and printing no value, before a listener exists and
// before the state directory is created.
func TestServe_RefusesANodeTokenThatResolvesToAnotherServerToken(t *testing.T) {
	const shared = "shared-secret-value-7c2d"
	for _, otherKey := range []string{"server_auth_token", "server_read_token", "webhook_token"} {
		t.Run(otherKey, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "media")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(name, value string) string {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte(value+"\n"), 0o400); err != nil {
					t.Fatal(err)
				}
				return p
			}
			// The key under test shares the node credential's value; the other two do not.
			values := map[string]string{"server_auth_token": "distinct-value-1", "server_read_token": "distinct-value-2",
				"webhook_token": "distinct-value-3"}
			values[otherKey] = shared
			body := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") +
				"\nserver_addr: 127.0.0.1:0" +
				"\nserver_auth_token: file:" + write("control", values["server_auth_token"]) +
				"\nserver_read_token: file:" + write("read", values["server_read_token"]) +
				"\nwebhook_token: file:" + write("webhook", values["webhook_token"]) +
				"\nnode_token: file:" + write("node", shared) + "\n"
			cfgPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("the configuration must pass Validate for this case to mean anything: %v", err)
			}
			var stderr bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			code := runServer(ctx, cfg, discardLog(), &stderr)
			if ctx.Err() != nil {
				t.Fatal("serve STARTED with a node_token that resolves to another server token's value")
			}
			if code == 0 {
				t.Fatalf("serve exited 0: %s", stderr.String())
			}
			msg := stderr.String()
			if !strings.Contains(msg, "refusing to start") || !strings.Contains(msg, "node_token") ||
				!strings.Contains(msg, otherKey) {
				t.Errorf("the refusal must name node_token and %s: %s", otherKey, msg)
			}
			if strings.Contains(msg, shared) || strings.Contains(msg, "distinct-value") {
				t.Errorf("the refusal prints a credential: %s", msg)
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
				t.Error("the refused start created the state directory")
			}
		})
	}
}

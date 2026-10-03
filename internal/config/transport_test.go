package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/secret"
)

// keyBlock is the PEM block type of a private key. It is spelled in two halves so that no
// file of this repository carries the marker a secret scanner reads as a committed key: the
// fixtures below hold no key, only the shape of one.
const keyBlock = "PRIVATE" + " KEY"

// The transport keys (docs/design/nodes.md#transport): server_tls_cert, server_tls_key,
// worker_mode, worker_insecure_http and worker_tls_ca.

// TestSecretKeys_ServerTLSKeyIsSecretBearingAndRefusesALiteral: the TLS private key is
// reached by reference. It is in the closed list exactly once, a literal - a pasted PEM
// block, in the file or in HOLDFAST_SERVER_TLS_KEY - refuses naming the key with no part of
// the value in the refusal, and a file: reference resolves to the file's contents, whole,
// and renders redacted. server_tls_cert is a path to public certificates and is not in the
// list.
func TestSecretKeys_ServerTLSKeyIsSecretBearingAndRefusesALiteral(t *testing.T) {
	const key = "server_tls_key"
	const literal = "MC4CAQAwBQYDK2VwBCIEIE-LITERAL-KEY-MATERIAL-MUST-NEVER-BE-ECHOED"
	if ServerTLSKeyKey != key || ServerTLSCertKey != "server_tls_cert" || WorkerTLSCAKey != "worker_tls_ca" {
		t.Fatalf("the exported key names are %q %q %q", ServerTLSKeyKey, ServerTLSCertKey, WorkerTLSCAKey)
	}
	listed := map[string]int{}
	for _, k := range SecretBearingKeys {
		listed[k]++
	}
	if listed[key] != 1 {
		t.Fatalf("%s appears %d times in SecretBearingKeys, want exactly once: %v", key, listed[key], SecretBearingKeys)
	}
	for _, public := range []string{"server_tls_cert", "worker_tls_ca", "worker_insecure_http", "worker_mode"} {
		if listed[public] != 0 {
			t.Errorf("%s is in SecretBearingKeys; it is a plain value, not a credential", public)
		}
	}
	if len(SecretBearingKeys) != 10 {
		t.Errorf("SecretBearingKeys holds %d keys, want the 10 this build has: %v", len(SecretBearingKeys), SecretBearingKeys)
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
		if strings.Contains(err.Error(), literal) || strings.Contains(err.Error(), "LITERAL-KEY-MATERIAL") {
			t.Errorf("the refusal %s echoes the literal value: %v", where, err)
		}
	}
	t.Run("a literal in the YAML file is refused, with or without a certificate beside it", func(t *testing.T) {
		_, err := loadValidated(t, mediaBase+key+": "+literal+"\n")
		refuses(t, "in the YAML file", err)
		_, err = loadValidated(t, mediaBase+"server_tls_cert: /etc/holdfast/tls.pem\n"+key+": "+literal+"\n")
		refuses(t, "in the YAML file beside server_tls_cert", err)
	})
	t.Run("a pasted PEM block is refused", func(t *testing.T) {
		_, err := loadValidated(t, mediaBase+"server_tls_cert: /etc/holdfast/tls.pem\n"+key+": |\n  -----BEGIN "+keyBlock+"-----\n  "+
			literal+"\n  -----END "+keyBlock+"-----\n")
		refuses(t, "as a PEM block", err)
	})
	t.Run("a literal in the environment is refused", func(t *testing.T) {
		t.Setenv("HOLDFAST_SERVER_TLS_KEY", literal)
		_, err := loadValidated(t, mediaBase)
		refuses(t, "in HOLDFAST_SERVER_TLS_KEY", err)
	})
	t.Run("a file reference resolves to the whole key and renders redacted", func(t *testing.T) {
		const pemKey = "-----BEGIN " + keyBlock + "-----\nline-one-of-the-key\nline-two-of-the-key\n-----END " + keyBlock + "-----"
		keyFile := filepath.Join(t.TempDir(), "tls-key.pem")
		if err := os.WriteFile(keyFile, []byte(pemKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := loadValidated(t, mediaBase+"server_tls_cert: /etc/holdfast/tls.pem\n"+key+": file:"+keyFile+"\n")
		if err != nil {
			t.Fatalf("a file: reference in %s was refused: %v", key, err)
		}
		if !c.TLSEnabled() {
			t.Error("TLSEnabled is false with both keys set")
		}
		refs, err := c.SecretRefs()
		if err != nil || len(refs) != len(SecretBearingKeys) {
			t.Fatalf("SecretRefs returned %d references for %d keys (%v)", len(refs), len(SecretBearingKeys), err)
		}
		if got := c.SecretRef(key).String(); got != "file:"+keyFile {
			t.Errorf("SecretRef(%s) = %q, want the reference as written", key, got)
		}
		set, err := secret.Resolve(context.Background(), refs)
		if err != nil {
			t.Fatalf("resolving %s: %v", key, err)
		}
		v := set.Get(key)
		if v.Expose() != pemKey {
			t.Errorf("%s resolved to %d bytes, want the file's %d: a multi-line key must arrive whole", key, len(v.Expose()), len(pemKey))
		}
		var asJSON string
		if raw, err := json.Marshal(v); err != nil || json.Unmarshal(raw, &asJSON) != nil {
			t.Fatalf("marshalling the value: %v", err)
		}
		for form, rendered := range map[string]string{"%v": fmt.Sprintf("%v", v), "%s": fmt.Sprintf("%s", v),
			"%+v": fmt.Sprintf("%+v", v), "%#v": fmt.Sprintf("%#v", v), "json": asJSON} {
			if strings.Contains(rendered, "line-one-of-the-key") || !strings.Contains(rendered, secret.Redacted) {
				t.Errorf("the resolved key rendered through %s as %q, want %s", form, rendered, secret.Redacted)
			}
		}
		// Every other key's reference is still its own.
		if set.Get("node_token").Expose() != "" || set.Get("server_auth_token").Expose() != "" {
			t.Error("resolving server_tls_key produced a value under another key")
		}
	})
	t.Run("the environment form of both keys is read", func(t *testing.T) {
		t.Setenv("HOLDFAST_SERVER_TLS_CERT", "/etc/holdfast/tls.pem")
		t.Setenv("HOLDFAST_SERVER_TLS_KEY", "file:/run/secrets/tls-key")
		c, err := loadValidated(t, mediaBase)
		if err != nil || c.ServerTLSCert != "/etc/holdfast/tls.pem" || c.ServerTLSKey != "file:/run/secrets/tls-key" || !c.TLSEnabled() {
			t.Fatalf("HOLDFAST_SERVER_TLS_CERT and _KEY loaded %q %q (err %v)", c.ServerTLSCert, c.ServerTLSKey, err)
		}
	})
}

// TestServerTLS_BothOrNeither: one key without the other refuses naming the one that is
// missing; neither is TLS off; a relative certificate path refuses.
func TestServerTLS_BothOrNeither(t *testing.T) {
	c, err := loadValidated(t, mediaBase)
	if err != nil || c.TLSEnabled() || c.ServerTLSCert != "" || c.ServerTLSKey != "" {
		t.Fatalf("a configuration with neither key: TLS enabled %v, cert %q, key %q, err %v", c.TLSEnabled(), c.ServerTLSCert, c.ServerTLSKey, err)
	}
	for _, tc := range []struct {
		name, yaml string
		// missing is the key the refusal says is not set.
		missing, set string
	}{
		{"a certificate with no key", "server_tls_cert: /etc/holdfast/tls.pem\n", "server_tls_key is not", "server_tls_cert is set"},
		{"a key with no certificate", "server_tls_key: file:/run/secrets/tls-key\n", "server_tls_cert is not", "server_tls_key is set"},
		{"a certificate with a whitespace key", "server_tls_cert: /etc/holdfast/tls.pem\nserver_tls_key: \"  \"\n", "server_tls_key is not", "server_tls_cert is set"},
		{"a key with a whitespace certificate", "server_tls_key: file:/run/secrets/tls-key\nserver_tls_cert: \"  \"\n", "server_tls_cert is not", "server_tls_key is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadValidated(t, mediaBase+tc.yaml)
			if err == nil {
				t.Fatal("half a TLS pair was ACCEPTED")
			}
			if !strings.Contains(err.Error(), tc.missing) || !strings.Contains(err.Error(), tc.set) {
				t.Errorf("the refusal does not say %q and %q: %v", tc.set, tc.missing, err)
			}
			if strings.Contains(err.Error(), "/run/secrets/tls-key") {
				t.Errorf("the refusal quotes the key's reference, which it has no need to: %v", err)
			}
			if c != nil && c.TLSEnabled() {
				t.Error("TLSEnabled is true for half a pair")
			}
		})
	}
	_, err = loadValidated(t, mediaBase+"server_tls_cert: tls.pem\nserver_tls_key: file:/run/secrets/tls-key\n")
	if err == nil || !strings.Contains(err.Error(), "server_tls_cert") || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("a relative server_tls_cert = %v, want a refusal naming the key and an absolute path", err)
	}
	if _, err := loadValidated(t, mediaBase+"server_tls_cert: /etc/holdfast/tls.pem\nserver_tls_key: cmd:/usr/bin/pass show tls\n"); err != nil {
		t.Errorf("a cmd: reference was refused: %v", err)
	}
}

// TestWorkerTransportKeys_ModeInsecureAndCA: the defaults reproduce a worker of the build
// before these keys; each key refuses a value outside its own, naming itself; the
// environment forms are read.
func TestWorkerTransportKeys_ModeInsecureAndCA(t *testing.T) {
	c, err := loadValidated(t, mediaBase)
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerMode != "mapped" || c.EffectiveWorkerMode() != WorkerModeMapped || c.WorkerInsecureHTTP || c.WorkerTLSCA != "" {
		t.Errorf("the defaults are mode %q insecure %v ca %q, want mapped, false and empty", c.WorkerMode, c.WorkerInsecureHTTP, c.WorkerTLSCA)
	}
	var zero Config
	if zero.EffectiveWorkerMode() != "mapped" || zero.TLSEnabled() {
		t.Error("a zero Config is not a mapped worker with TLS off")
	}
	if err := zero.validateTransport(); err != nil {
		t.Errorf("a zero Config's transport keys were refused: %v", err)
	}

	c, err = loadValidated(t, mediaBase+"worker_mode: mapped\nworker_insecure_http: true\nworker_tls_ca: /etc/holdfast/ca.pem\n"+
		"worker_path_map:\n  - {from: /mnt/media, to: /data/media}\n")
	if err != nil || c.WorkerMode != "mapped" || !c.WorkerInsecureHTTP || c.WorkerTLSCA != "/etc/holdfast/ca.pem" {
		t.Fatalf("a mapped worker with the transport keys loaded %+v, %v", c, err)
	}
	for _, tc := range []struct {
		name, yaml string
		names      []string
	}{
		{"an unknown mode", "worker_mode: streaming\n", []string{"worker_mode", "streaming", "mapped|http"}},
		{"a mode in another case", "worker_mode: HTTP\n", []string{"worker_mode", "mapped|http"}},
		{"a relative bundle", "worker_tls_ca: ca.pem\n", []string{"worker_tls_ca", "absolute path"}},
		{"http mode with a path map", "worker_mode: http\nworker_path_map:\n  - {from: /mnt/media, to: /data/media}\n",
			[]string{"worker_mode is http", "worker_path_map is set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+tc.yaml)
			if err == nil {
				t.Fatal("it was ACCEPTED")
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
	t.Run("the environment forms are read", func(t *testing.T) {
		t.Setenv("HOLDFAST_WORKER_MODE", "http")
		t.Setenv("HOLDFAST_WORKER_INSECURE_HTTP", "true")
		t.Setenv("HOLDFAST_WORKER_TLS_CA", "/etc/holdfast/ca.pem")
		c, err := load(t, "state_dir: /var/lib/holdfast\n")
		if err != nil {
			t.Fatal(err)
		}
		if c.WorkerMode != "http" || !c.WorkerInsecureHTTP || c.WorkerTLSCA != "/etc/holdfast/ca.pem" {
			t.Errorf("the environment loaded mode %q insecure %v ca %q", c.WorkerMode, c.WorkerInsecureHTTP, c.WorkerTLSCA)
		}
		if err := c.ValidateWorker(); err != nil {
			t.Errorf("an http worker configured from the environment was refused: %v", err)
		}
	})
}

// TestValidateWorker_AnHTTPWorkerNamesNoLibraryRoot: a worker in http mode reads no
// library, so its configuration needs no library root - no fake one - and one that names a
// root, or a path map, beside `worker_mode: http` is refused by name rather than read as
// something it cannot mean. Every other rule still applies, and every other command still
// refuses a file with no library root.
func TestValidateWorker_AnHTTPWorkerNamesNoLibraryRoot(t *testing.T) {
	httpWorker := "worker_mode: http\nworker_server: https://holdfast.example.test\nnode_token: file:/run/secrets/node\n"
	c, err := load(t, httpWorker)
	if err != nil {
		t.Fatalf("an http worker's configuration with no library_roots did not load: %v", err)
	}
	if len(c.LibraryRoots) != 0 || c.EffectiveWorkerMode() != WorkerModeHTTP {
		t.Fatalf("it loaded roots %v and mode %q", c.LibraryRoots, c.WorkerMode)
	}
	if err := c.ValidateWorker(); err != nil {
		t.Errorf("ValidateWorker refused an http worker with no library root: %v", err)
	}
	// Validate itself is what it was: `run` and `serve` refuse this file.
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "library_roots is empty") {
		t.Errorf("Validate on a file with no library root = %v, want the empty-roots refusal", err)
	}

	c, err = load(t, mediaBase+httpWorker)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateWorker(); err == nil || !strings.Contains(err.Error(), "worker_mode is http") || !strings.Contains(err.Error(), "library_roots is set") {
		t.Errorf("an http worker naming a library root = %v, want a refusal naming worker_mode and library_roots", err)
	}

	// Every rule that is not about a root still bites in http mode.
	for name, yaml := range map[string]string{
		"a literal credential": "worker_mode: http\nnode_token: a-pasted-credential\n",
		"a path map":           httpWorker + "worker_path_map:\n  - {from: /mnt/media, to: /data/media}\n",
		"a bad log level":      httpWorker + "log_level: chatty\n",
		"a bad worker name":    httpWorker + "worker_name: \"node a\"\n",
	} {
		c, err := load(t, yaml)
		if err != nil {
			continue // refused at load is refused
		}
		if err := c.ValidateWorker(); err == nil {
			t.Errorf("%s was ACCEPTED in an http worker's configuration", name)
		}
	}

	// A mapped worker is held to Validate, whole: it names its own mount as a library root.
	c, err = load(t, "worker_server: https://holdfast.example.test\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateWorker(); err == nil || !strings.Contains(err.Error(), "library_roots is empty") {
		t.Errorf("a mapped worker with no library root = %v, want the empty-roots refusal", err)
	}
	c, err = load(t, mediaBase+"worker_server: https://holdfast.example.test\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateWorker(); err != nil {
		t.Errorf("a mapped worker naming its mount was refused: %v", err)
	}
}

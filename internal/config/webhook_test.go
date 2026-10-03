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

// TestSecretKeys_WebhookTokenIsSecretBearingAndRefusesALiteral is the goal's credential line
// for the webhook intake: `webhook_token` is in the closed list of secret-bearing keys, a
// literal value is refused naming the key - in the YAML file and in HOLDFAST_WEBHOOK_TOKEN -
// with no part of the value in the refusal, a `file:` reference is accepted and resolves to
// the file's contents, and the resolved value renders as `<redacted>` wherever it is printed.
func TestSecretKeys_WebhookTokenIsSecretBearingAndRefusesALiteral(t *testing.T) {
	const key = "webhook_token"
	const literal = "LITERAL-WEBHOOK-CREDENTIAL-MUST-NEVER-BE-ECHOED"

	if WebhookTokenKey != key {
		t.Fatalf("WebhookTokenKey = %q, want %q", WebhookTokenKey, key)
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
		t.Setenv("HOLDFAST_WEBHOOK_TOKEN", literal)
		_, err := loadValidated(t, mediaBase)
		refuses(t, "in HOLDFAST_WEBHOOK_TOKEN", err)
	})

	t.Run("absent is off", func(t *testing.T) {
		c, err := loadValidated(t, mediaBase)
		if err != nil {
			t.Fatalf("a configuration with no %s was refused: %v", key, err)
		}
		if c.WebhookToken != "" || c.WebhookEnabled() {
			t.Errorf("%s defaults to %q (enabled=%v), want empty and off", key, c.WebhookToken, c.WebhookEnabled())
		}
		if c.SecretRef(key).Configured() {
			t.Errorf("an absent %s parsed to a configured reference", key)
		}
	})

	t.Run("a file reference resolves and renders redacted", func(t *testing.T) {
		const plaintext = "resolved-webhook-credential"
		secretFile := filepath.Join(t.TempDir(), "credential")
		if err := os.WriteFile(secretFile, []byte(plaintext+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := loadValidated(t, mediaBase+key+": file:"+secretFile+"\n")
		if err != nil {
			t.Fatalf("a file: reference in %s was refused: %v", key, err)
		}
		if !c.WebhookEnabled() {
			t.Errorf("WebhookEnabled is false with %s set", key)
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
		// encoding/json escapes the angle brackets, so the JSON form is read back to the
		// string a consumer of it would see.
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

// TestWebhookToken_MayNotShareAReferenceWithAServerToken: the intake credential is the one an
// arr holds, so a configuration that points it at the control token's or the read token's
// secret is refused naming both keys. A reference of its own is accepted beside both.
func TestWebhookToken_MayNotShareAReferenceWithAServerToken(t *testing.T) {
	const shared = "file:/run/secrets/shared"
	for _, other := range []string{"server_auth_token", "server_read_token"} {
		t.Run(other, func(t *testing.T) {
			_, err := loadValidated(t, mediaBase+other+": "+shared+"\nwebhook_token: \" "+shared+" \"\n")
			if err == nil {
				t.Fatalf("webhook_token sharing %s's reference was ACCEPTED", other)
			}
			if !strings.Contains(err.Error(), "webhook_token") || !strings.Contains(err.Error(), other) {
				t.Errorf("the refusal must name webhook_token and %s: %v", other, err)
			}
		})
	}
	c, err := loadValidated(t, mediaBase+"server_auth_token: file:/run/secrets/control\n"+
		"server_read_token: file:/run/secrets/read\nwebhook_token: file:/run/secrets/webhook\n")
	if err != nil {
		t.Fatalf("three distinct references were refused: %v", err)
	}
	if !c.WebhookEnabled() {
		t.Error("WebhookEnabled is false with webhook_token set")
	}
	// Neither server token configured: an unset key is not "the same reference" as another
	// unset key, and a webhook_token on its own is accepted.
	if _, err := loadValidated(t, mediaBase+"webhook_token: file:/run/secrets/webhook\n"); err != nil {
		t.Fatalf("a webhook_token with no server token was refused: %v", err)
	}
	if _, err := loadValidated(t, mediaBase); err != nil {
		t.Fatalf("a configuration with none of the three was refused: %v", err)
	}
}

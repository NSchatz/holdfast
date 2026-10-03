package config

import (
	"fmt"
	"strings"
)

// webhookTokenKey is the credential the Sonarr and Radarr webhook intake accepts
// (docs/design/media-clients.md#webhook-intake). It is reached by reference and is off until
// it is written.
const webhookTokenKey = "webhook_token"

// WebhookTokenKey is that key's name, for the one caller that hands the resolved value to
// the server.
const WebhookTokenKey = webhookTokenKey

// WebhookEnabled reports whether a webhook credential reference is configured, which is what
// turns the two intake endpoints on.
func (c *Config) WebhookEnabled() bool { return strings.TrimSpace(c.WebhookToken) != "" }

// validateWebhookToken refuses a webhook_token written as the SAME reference as one of the
// two server tokens. The intake credential is the one an arr holds, and it may only queue a
// file; a reference shared with server_auth_token or server_read_token would hand the arr a
// credential that also pauses holdfast or reads every library path. It compares the
// references as written and resolves nothing, so two references that reach one value by
// different routes are not caught here.
func (c *Config) validateWebhookToken() error {
	ref := strings.TrimSpace(c.WebhookToken)
	if ref == "" {
		return nil
	}
	for _, other := range []struct{ key, value string }{
		{"server_auth_token", c.ServerAuthToken},
		{"server_read_token", c.ServerReadToken},
	} {
		if strings.TrimSpace(other.value) == ref {
			return fmt.Errorf("%s and %s are the same reference: the webhook credential is the one "+
				"Sonarr or Radarr holds and it may only queue a file, so it must be a secret of its own. "+
				"Point %s at a different secret", webhookTokenKey, other.key, webhookTokenKey)
		}
	}
	return nil
}

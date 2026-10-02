package main

import (
	"crypto/subtle"
	"fmt"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// checkWebhookTokenDistinct refuses a webhook credential whose RESOLVED value is the value
// of the control token or the read token. config.Validate already refuses the same
// REFERENCE written twice; this is the half only a resolved set can answer, two different
// references that reach one secret. The webhook credential is the one an arr holds, and it
// may only queue a file: were it also the control token, the arr would hold a credential
// that pauses holdfast and withholds paths, whatever the intake itself accepts.
//
// The comparison is constant time, an unset token is never "the same" as anything, and the
// refusal names the two keys and no value.
func checkWebhookTokenDistinct(secrets *secret.Set) error {
	hook := secrets.Get(config.WebhookTokenKey)
	if hook.Empty() {
		return nil
	}
	for _, key := range []string{"server_auth_token", "server_read_token"} {
		other := secrets.Get(key)
		if other.Empty() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hook.Expose()), []byte(other.Expose())) == 1 {
			return fmt.Errorf("%s and %s resolve to the same value: the webhook credential is the one "+
				"Sonarr or Radarr holds and it may only queue a file, so it must be a secret of its own. "+
				"Point %s at a different secret", config.WebhookTokenKey, key, config.WebhookTokenKey)
		}
	}
	return nil
}

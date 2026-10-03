package main

import (
	"crypto/subtle"
	"fmt"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// checkNodeTokenDistinct refuses a node credential whose RESOLVED value is the value of the
// control token, the read token or the webhook token. config.Validate already refuses the
// same REFERENCE written twice; this is the half only a resolved set can answer, two
// different references that reach one secret. The node credential is the one a worker node
// holds, and it may only lease a job and upload its output: were it also the control token,
// every worker would hold a credential that pauses holdfast and withholds paths.
//
// The comparison is constant time, an unset token is never "the same" as anything, and the
// refusal names the two keys and no value.
func checkNodeTokenDistinct(secrets *secret.Set) error {
	tok := secrets.Get(config.NodeTokenKey)
	if tok.Empty() {
		return nil
	}
	for _, key := range []string{"server_auth_token", "server_read_token", config.WebhookTokenKey} {
		other := secrets.Get(key)
		if other.Empty() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(tok.Expose()), []byte(other.Expose())) == 1 {
			return fmt.Errorf("%s and %s resolve to the same value: the node credential is the one a "+
				"worker node holds and it may only lease a job and upload its output, so it must be a "+
				"secret of its own. Point %s at a different secret", config.NodeTokenKey, key, config.NodeTokenKey)
		}
	}
	return nil
}

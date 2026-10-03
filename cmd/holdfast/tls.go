package main

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/secret"
)

// Built-in TLS for `serve` (docs/design/nodes.md#transport).
//
// server_tls_cert is a plain path to a PEM certificate chain, and server_tls_key a secret
// reference to its PEM private key. With both set the listener speaks TLS; with neither it
// is the listener it always was. There is no client certificate and no user login: the
// credentials are the bearer tokens they were, and TLS is what keeps them and the media off
// the wire in the clear.

// TLSPairError is a server_tls_cert and server_tls_key pair `serve` will not listen with.
// Key names the configuration key at fault. Neither field ever holds a byte of the private
// key, and the error wraps nothing that could: crypto/tls's own error is classified here and
// then dropped.
type TLSPairError struct {
	Key string
	Why string
}

func (e *TLSPairError) Error() string { return e.Key + ": " + e.Why }

// serverTLS builds the listener's TLS configuration, or nil with no error when built-in TLS
// is off. The resolved key is exposed exactly once, to tls.X509KeyPair, which parses "a
// public/private key pair from a pair of PEM encoded data" (`go doc crypto/tls X509KeyPair`,
// Go 1.25.14; https://pkg.go.dev/crypto/tls#X509KeyPair, read 2026-10-03).
//
// The minimum version is TLS 1.2. It is crypto/tls's own default for a server - "By default,
// TLS 1.2 is currently used as the minimum" (`go doc crypto/tls Config.MinVersion`, same
// toolchain) - and it is written out so the floor does not move with a toolchain.
func serverTLS(cfg *config.Config, key secret.Value) (*tls.Config, error) {
	if !cfg.TLSEnabled() {
		return nil, nil
	}
	certPath := strings.TrimSpace(cfg.ServerTLSCert)
	chain, err := os.ReadFile(certPath)
	if err != nil {
		// err names the path and the errno, and a certificate chain is public.
		return nil, &TLSPairError{Key: config.ServerTLSCertKey, Why: "the certificate chain could not be read: " + err.Error()}
	}
	if !holdsPEM(chain, "CERTIFICATE") {
		return nil, &TLSPairError{Key: config.ServerTLSCertKey, Why: certPath + " holds no PEM certificate"}
	}
	if key.Empty() {
		return nil, &TLSPairError{Key: config.ServerTLSKeyKey, Why: "the reference resolved to no value"}
	}
	pair, err := tls.X509KeyPair(chain, []byte(key.Expose()))
	if err != nil {
		return nil, &TLSPairError{Key: config.ServerTLSKeyKey, Why: fmt.Sprintf("the value the reference resolves to "+
			"is not a PEM private key that belongs to the certificate in %s (%s). Nothing of it is printed",
			config.ServerTLSCertKey, certPath)}
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// holdsPEM reports whether data carries at least one PEM block of that type.
func holdsPEM(data []byte, blockType string) bool {
	for rest := bytes.TrimSpace(data); len(rest) > 0; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return false
		}
		if block.Type == blockType {
			return true
		}
	}
	return false
}

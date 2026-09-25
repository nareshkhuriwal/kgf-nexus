// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// ClientMaterial is hot-reloaded client TLS material for a client that is not
// gRPC -- today, the NATS connection in internal/bus.
//
// It is the same reloader DialOption uses, so it inherits everything docs/TLS.md
// §5.0 promises: validation before a swap, debounce of non-atomic writers, the
// expiry metrics, and one poller per distinct file set per process. A NATS client
// configured with the very files the gRPC client dials with shares that
// reloader rather than starting a second one.
//
// The accessors are shaped for callbacks that run on every (re)connect, such as
// nats.ClientTLSConfig: each call returns the material current at that moment,
// so a rotated certificate or CA bundle reaches the next connection without a
// restart, and an established connection keeps what it handshook with.
type ClientMaterial struct {
	r *reloader
}

// LoadClientMaterial loads (synchronously, so a bad file fails startup) and then
// watches a client key pair and/or CA bundle. Any of the three may be empty, but
// the certificate and key are both-or-neither, for the same reason as in
// DialOption: half a pair is a mistake, never a request for less security.
func LoadClientMaterial(certFile, keyFile, caFile string) (*ClientMaterial, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("client TLS is half-configured: certificate=%q key=%q -- both are required to present a client certificate",
			certFile, keyFile)
	}
	if certFile == "" && caFile == "" {
		return nil, errors.New("transport: LoadClientMaterial needs a key pair, a CA bundle, or both")
	}
	r, err := getReloader(roleClient, fileSet{cert: certFile, key: keyFile, ca: caFile}, false)
	if err != nil {
		return nil, err
	}
	return &ClientMaterial{r: r}, nil
}

// HasCertificate reports whether a client key pair is configured.
func (m *ClientMaterial) HasCertificate() bool { return m.r.files.cert != "" }

// HasCA reports whether a CA bundle is configured (otherwise the host's trust
// store verifies the server).
func (m *ClientMaterial) HasCA() bool { return m.r.files.ca != "" }

// Certificate returns the client key pair currently in force.
func (m *ClientMaterial) Certificate() (tls.Certificate, error) {
	s := m.r.current()
	if s == nil || s.leaf == nil {
		return tls.Certificate{}, errors.New("transport: no client certificate is configured")
	}
	return *s.leaf, nil
}

// RootCAs returns the CA pool currently in force, or nil (the host's trust
// store) when no bundle is configured.
func (m *ClientMaterial) RootCAs() (*x509.CertPool, error) {
	s := m.r.current()
	if s == nil {
		return nil, errors.New("transport: TLS material is not loaded")
	}
	return s.pool, nil
}

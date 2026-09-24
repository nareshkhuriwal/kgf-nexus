// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"crypto/tls"
	"net"

	"google.golang.org/grpc/credentials"
)

// reloadingClientCreds are gRPC client transport credentials that build a fresh
// tls.Config from the reloader's current snapshot on EVERY handshake.
//
// Why a wrapper rather than one long-lived credentials.NewTLS(cfg): a client's
// RootCAs has no per-handshake callback in crypto/tls. The only ways to rotate
// the trust bundle of a running client are to disable Go's verification and
// re-implement it in VerifyPeerCertificate (easy to get subtly wrong -- hostname
// checks, EKU, intermediates -- and a mistake there silently accepts anything),
// or to hand each handshake a config built from the current bundle. This does
// the second, and still delegates the handshake itself to grpc-go's own TLS
// credentials, so ALPN enforcement and AuthInfo stay exactly what grpc expects.
//
// The client certificate is supplied through GetClientCertificate rather than
// Certificates: with Certificates, crypto/tls silently sends NO certificate when
// ours does not match the CAs the server advertised, and the failure surfaces as
// the server's "bad certificate" instead of anything that names the mismatch.
type reloadingClientCreds struct {
	r          *reloader
	serverName string
}

var _ credentials.TransportCredentials = (*reloadingClientCreds)(nil)

func (c *reloadingClientCreds) config() *tls.Config {
	s := c.r.current()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// nil selects the host's trust store: see DialOption for when that is
		// intended.
		RootCAs:    s.pool,
		ServerName: c.serverName,
	}
	if leaf := s.leaf; leaf != nil {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return leaf, nil
		}
	}
	return cfg
}

func (c *reloadingClientCreds) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return credentials.NewTLS(c.config()).ClientHandshake(ctx, authority, raw)
}

func (c *reloadingClientCreds) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errNotClient
}

func (c *reloadingClientCreds) Info() credentials.ProtocolInfo {
	// Mirrors credentials.NewTLS's Info, including ServerName: grpc-go uses it as
	// the :authority when set, which is what makes EnvServerName behave the same
	// under reload as it did before.
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.2", ServerName: c.serverName}
}

func (c *reloadingClientCreds) Clone() credentials.TransportCredentials {
	cp := *c
	return &cp
}

// OverrideServerName is deprecated in grpc-go but still part of the interface.
func (c *reloadingClientCreds) OverrideServerName(name string) error {
	c.serverName = name
	return nil
}

// Package transport resolves the TLS posture of inter-service gRPC from the
// environment, so that enabling encryption is a deployment decision rather than a
// code change in eight places.
//
// THE FAILURE THIS PACKAGE PREVENTS.
//
// Every control-plane hop was plaintext, and each of the eight dial sites spelled
// that out for itself with insecure.NewCredentials(). Two consequences followed
// from the duplication, and both are worse than the plaintext itself:
//
//   - Turning TLS on meant editing eight files in five services, so it was never
//     a thing an operator could do -- only a thing a developer could ship. A
//     security posture that requires a release is a posture nobody adopts.
//   - There was no way to ASK whether the posture was right. A deployment that
//     intended mTLS and got plaintext looked identical to one that intended
//     plaintext: healthy services, successful RPCs, lease tokens and session
//     tokens crossing the network in the clear. Nothing fails, nothing logs, and
//     the mistake is discovered by a packet capture or not at all.
//
// So the resolution lives in one place, defaults to exactly today's plaintext
// behaviour when nothing is configured, and can be made fatal (see RequireTLS) by
// a deployment that must not ship unencrypted.
//
// This is transport encryption and peer identity only. It says nothing about
// authorization -- see docs/TLS.md.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Environment variable names, following internal/config's convention of naming
// every accepted spelling in one place so an operator has a single list to read
// and a typo is visible next to the name it was meant to be.
//
// Unlike the DSN there is no history of six services disagreeing here, because
// nothing read these before this package existed -- so one spelling each, and a
// shared NEXUS_TLS_ prefix that groups them in an `env | sort`.
const (
	// EnvServerCert and EnvServerKey are the identity a service SERVES with.
	// Both or neither: a cert with no key cannot be loaded, and treating that as
	// "fall back to plaintext" would turn a half-finished config change into a
	// silent downgrade.
	EnvServerCert = "NEXUS_TLS_CERT_FILE"
	EnvServerKey  = "NEXUS_TLS_KEY_FILE"

	// EnvClientCert and EnvClientKey are the identity a service DIALS with, and
	// are what makes a connection mutually authenticated. Separate from the
	// serving pair because a service is both a server and a client of its peers,
	// and a deployment may legitimately issue it one leaf for each role.
	EnvClientCert = "NEXUS_TLS_CLIENT_CERT_FILE"
	EnvClientKey  = "NEXUS_TLS_CLIENT_KEY_FILE"

	// EnvCACert is the bundle used to verify the OTHER end, in both directions: a
	// client verifies the server's chain against it, and a server verifies a
	// presented client certificate against it. One knob because in a
	// single-issuer deployment -- which is every deployment this platform is
	// small enough to have -- they are the same bundle, and two knobs would
	// invite setting one and forgetting the other.
	EnvCACert = "NEXUS_TLS_CA_FILE"

	// EnvServerRequireClientCert turns on mTLS enforcement at the server.
	//
	// Separate from having a CA configured, because verifying a client
	// certificate WHEN OFFERED is not the same as requiring one, and only the
	// second actually excludes an unauthenticated peer. Defaulting it on would
	// break the ordinary case of a TLS-terminating server that still has
	// plaintext-era clients mid-rollout, so the operator opts in once every
	// client carries a cert.
	EnvServerRequireClientCert = "NEXUS_TLS_REQUIRE_CLIENT_CERT"

	// EnvServerName overrides the hostname a client verifies the server
	// certificate against.
	//
	// This exists because PeerTarget rewrites a peer address to dns:///host:port
	// and gRPC then verifies against `host` -- which in compose is a service name
	// like "scheduler" that a certificate issued for a real DNS name will not
	// contain. Without an override, a correctly issued cert is rejected with
	// "x509: certificate is valid for nexus.internal, not scheduler", which reads
	// like a broken certificate and is actually a naming mismatch.
	EnvServerName = "NEXUS_TLS_SERVER_NAME"
)

// requireTLSEnvNames are the spellings that make plaintext fatal, mirroring
// dsnRequiredEnvNames in internal/config/dsn.go -- same two-spelling rationale,
// and deliberately the same NEXUS_ENV so that one variable arms every
// production-only guard the platform has.
var requireTLSEnvNames = []string{
	"NEXUS_REQUIRE_TLS",
	"NEXUS_ENV",
}

// ServerOptions returns the gRPC server options that enable TLS, or an empty
// slice when no serving certificate is configured.
//
// Empty rather than an error for the unconfigured case: that IS the dev path, and
// it must stay byte-for-byte the behaviour of a bare grpc.NewServer. The options
// are returned as a slice so a caller appends them to config.PeerServerOptions()
// rather than choosing between the two -- the connection-age rotation those
// options carry is load-bearing regardless of the transport, and a caller forced
// to pick one would silently lose it.
//
// A configured-but-unloadable certificate is an error, never a downgrade. An
// unreadable key file is a deployment mistake, and falling back to plaintext
// would hide it behind a working service.
func ServerOptions() ([]grpc.ServerOption, error) {
	certFile, keyFile := os.Getenv(EnvServerCert), os.Getenv(EnvServerKey)
	if certFile == "" && keyFile == "" {
		if RequireTLS() {
			return nil, fmt.Errorf(
				"serving plaintext gRPC, and %s requires TLS: set %s and %s (and %s to verify peers)",
				strings.Join(requireTLSEnvNames, " or "), EnvServerCert, EnvServerKey, EnvCACert)
		}
		return nil, nil
	}
	// Half a pair is always a mistake, and the message says which half is missing
	// because "failed to load key pair" sends an operator reading the file they
	// did set.
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("TLS is half-configured: %s=%q and %s=%q -- both are required to serve TLS",
			EnvServerCert, certFile, EnvServerKey, keyFile)
	}

	leaf, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load serving key pair from %s and %s: %w", certFile, keyFile, err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{leaf},
		// Pinned rather than left to the default because the default floor moves
		// with the Go release, and a control plane's floor should be a decision
		// someone made rather than a property of the toolchain that built it.
		MinVersion: tls.VersionTLS12,
	}

	requireClient := boolEnv(EnvServerRequireClientCert)
	caFile := os.Getenv(EnvCACert)
	if requireClient {
		if caFile == "" {
			// Requiring a client cert with nothing to verify it against would
			// accept ANY certificate, including a self-signed one an attacker
			// minted -- strictly worse than plaintext, because it looks like mTLS
			// in a config review.
			return nil, fmt.Errorf("%s is set but %s is not: a client certificate cannot be verified without a CA bundle, and accepting an unverified one is weaker than the plaintext it replaces",
				EnvServerRequireClientCert, EnvCACert)
		}
		pool, err := certPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return []grpc.ServerOption{grpc.Creds(credentials.NewTLS(cfg))}, nil
}

// DialOption returns the transport credentials a control-plane client should dial
// a peer with: TLS verified against the configured CA, mTLS when a client
// certificate is configured, and insecure.NewCredentials() when neither is.
//
// One option rather than a slice, because exactly one transport-credentials
// option is meaningful per connection and returning a slice would suggest
// otherwise. It is meant to be appended to config.PeerDialOptions() -- those
// govern reconnect behaviour and are needed under TLS just as much as under
// plaintext.
func DialOption() (grpc.DialOption, error) {
	caFile := os.Getenv(EnvCACert)
	certFile, keyFile := os.Getenv(EnvClientCert), os.Getenv(EnvClientKey)

	if caFile == "" && certFile == "" && keyFile == "" {
		if RequireTLS() {
			return nil, fmt.Errorf(
				"dialing a peer in plaintext, and %s requires TLS: set %s (and %s plus %s for mutual TLS)",
				strings.Join(requireTLSEnvNames, " or "), EnvCACert, EnvClientCert, EnvClientKey)
		}
		return grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pool, err := certPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	// No CA but a client cert means "verify the server against the host's trust
	// store, and identify myself" -- coherent for a peer behind a
	// publicly-issued certificate, so it is allowed rather than rejected. Leaving
	// RootCAs nil is what selects the system pool.

	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("client TLS is half-configured: %s=%q and %s=%q -- both are required for mutual TLS",
				EnvClientCert, certFile, EnvClientKey, keyFile)
		}
		leaf, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client key pair from %s and %s: %w", certFile, keyFile, err)
		}
		cfg.Certificates = []tls.Certificate{leaf}
	}

	// See EnvServerName: the name gRPC derives from a dns:///service:port target
	// is a compose service name, which a real certificate will not carry.
	if name := os.Getenv(EnvServerName); name != "" {
		cfg.ServerName = name
	}

	return grpc.WithTransportCredentials(credentials.NewTLS(cfg)), nil
}

// RequireTLS reports whether the deployment has demanded encrypted inter-service
// gRPC, in the same shape as config.DSNRequired.
//
// NEXUS_ENV counts only when it names a production-like environment, because
// NEXUS_ENV=dev must keep the plaintext path working -- the entire point of this
// package is that the dev path is unchanged.
func RequireTLS() bool {
	if v := os.Getenv("NEXUS_REQUIRE_TLS"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off":
			return false
		default:
			return true
		}
	}
	// NEXUS_ENV deliberately does NOT arm this, unlike config.DSNRequired().
	//
	// The two guards look symmetrical and are not. A missing DSN is a
	// configuration error with no working outcome -- the service cannot serve
	// without a database, so failing at startup costs nothing. Missing TLS has a
	// working outcome today: every existing deployment runs plaintext, and ADR-0011
	// still records mTLS as a known unpaid cost. Arming this from NEXUS_ENV would
	// mean that wiring TLS support into the services BREAKS every deployment that
	// had set NEXUS_ENV=production for the DSN guard, at the moment of wiring and
	// for a reason nothing in their config changed to cause.
	//
	// So requiring TLS stays an explicit, separate decision. An operator turns it
	// on once certs are actually distributed, which is the only point at which the
	// requirement can be satisfied.
	return false
}

// certPool reads a PEM bundle into a pool that trusts only what the file
// contains.
//
// Deliberately not seeded from the system pool: the point of pinning an internal
// CA is that a certificate signed by any of the ~150 public roots a host happens
// to trust is NOT acceptable for a control-plane peer, and starting from
// x509.SystemCertPool would quietly restore all of them.
func certPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %s (%s): %w", path, EnvCACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// AppendCertsFromPEM reports only "nothing usable found", so the message
		// has to supply the likely cause: this is what pointing the variable at a
		// DER file, a key, or a truncated download looks like.
		return nil, fmt.Errorf("CA bundle %s (%s) contains no PEM certificates; a DER-encoded or truncated file looks exactly like this", path, EnvCACert)
	}
	return pool, nil
}

// boolEnv reads an on/off switch with the same word list DSNRequired accepts, so
// that NEXUS_TLS_REQUIRE_CLIENT_CERT=false means what an operator who has already
// written NEXUS_REQUIRE_DSN=false expects it to.
func boolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// tlsEnvNames is every variable this package reads. Listed once so clearEnv
// cannot drift out of date and leave one name inherited from the developer's
// shell, which would make an assertion about the unconfigured path pass or fail
// for a reason unrelated to the test.
var tlsEnvNames = []string{
	EnvServerCert, EnvServerKey,
	EnvClientCert, EnvClientKey,
	EnvCACert, EnvServerRequireClientCert, EnvServerName,
	"NEXUS_REQUIRE_TLS", "NEXUS_ENV",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range tlsEnvNames {
		t.Setenv(name, "")
	}
}

// serve stands up a real gRPC server carrying the health service on an ephemeral
// loopback port and returns its address.
//
// The health service is used because it ships with grpc-go: asserting on a real
// handshake requires a real RPC, and inventing a proto for it would add a
// generated-code dependency to a test about transport credentials.
func serve(t *testing.T, opts []grpc.ServerOption) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// check dials addr with the given option and performs one health Check, returning
// the RPC error. A nil error means the transport handshake completed AND a call
// crossed it -- gRPC connects lazily, so a successful dial alone proves nothing.
//
// Deliberately WITHOUT grpc.WaitForReady: gRPC treats a refused handshake as a
// transient failure and retries it forever, so a wait-for-ready call reports a
// rejection as a five-second deadline instead of as the TLS error it is. Failing
// fast makes the negative tests both quick and specific about what went wrong.
func check(t *testing.T, addr string, opt grpc.DialOption) error {
	t.Helper()

	conn, err := grpc.NewClient(addr, opt)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect explicitly and wait for the first state change, so the call below
	// happens after the handshake has been attempted rather than racing it and
	// failing with a bare "connection is idle".
	conn.Connect()
	for {
		s := conn.GetState()
		if s == connectivity.Ready || s == connectivity.TransientFailure {
			break
		}
		if !conn.WaitForStateChange(ctx, s) {
			return ctx.Err()
		}
	}

	_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

// TestUnconfiguredStaysPlaintext is the compatibility guarantee of the whole
// package: a deployment that sets nothing must behave exactly as it did before
// this package existed. If this fails, adopting the package breaks every dev
// environment and compose file at once.
func TestUnconfiguredStaysPlaintext(t *testing.T) {
	clearEnv(t)

	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions with nothing configured returned an error: %v", err)
	}
	if len(sopts) != 0 {
		t.Fatalf("ServerOptions returned %d options with nothing configured; the plaintext dev path must add none", len(sopts))
	}

	dopt, err := DialOption()
	if err != nil {
		t.Fatalf("DialOption with nothing configured returned an error: %v", err)
	}

	addr := serve(t, sopts)
	if err := check(t, addr, dopt); err != nil {
		t.Fatalf("plaintext health check failed: %v", err)
	}
}

// TestServerTLSAcceptsAClientTrustingTheCA covers the base case an operator
// enables first: encrypt the hop, verify the server, do not yet require client
// certs.
func TestServerTLSAcceptsAClientTrustingTheCA(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	srvCert, srvKey := ca.issue(t, "scheduler", []string{"scheduler"}, false)

	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions: %v", err)
	}
	if len(sopts) == 0 {
		t.Fatal("ServerOptions returned nothing with a key pair configured; the server would serve plaintext")
	}
	addr := serve(t, sopts)

	// The server's variables are cleared for the client half so that a dial
	// option accidentally reading the SERVING pair cannot make this pass.
	t.Setenv(EnvServerCert, "")
	t.Setenv(EnvServerKey, "")
	t.Setenv(EnvCACert, ca.caFile)
	dopt, err := DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	if err := check(t, addr, dopt); err != nil {
		t.Fatalf("TLS health check failed: %v", err)
	}
}

// TestClientTrustingTheWrongCAIsRejected is the assertion that the CA bundle is
// actually load-bearing. A pool built from the system roots, or an empty pool
// treated as "trust anything", would let this pass -- and a deployment would
// believe it had pinned an issuer when it had not.
func TestClientTrustingTheWrongCAIsRejected(t *testing.T) {
	clearEnv(t)
	real := newTestCA(t, "nexus")
	other := newTestCA(t, "attacker")
	srvCert, srvKey := real.issue(t, "scheduler", []string{"scheduler"}, false)

	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions: %v", err)
	}
	addr := serve(t, sopts)

	t.Setenv(EnvServerCert, "")
	t.Setenv(EnvServerKey, "")
	t.Setenv(EnvCACert, other.caFile)
	dopt, err := DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	if err := check(t, addr, dopt); err == nil {
		t.Fatal("health check SUCCEEDED against a server signed by an untrusted CA; the CA bundle is not being enforced")
	}
}

// TestMTLSRequiresAClientCertificate is the difference between "verify a client
// cert if one is offered" and "require one". Only the second excludes an
// unauthenticated peer, and it is the reason NEXUS_TLS_REQUIRE_CLIENT_CERT exists
// separately from having a CA configured.
func TestMTLSRequiresAClientCertificate(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	srvCert, srvKey := ca.issue(t, "scheduler", []string{"scheduler"}, false)
	cliCert, cliKey := ca.issue(t, "api-gateway", nil, true)

	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	t.Setenv(EnvCACert, ca.caFile)
	t.Setenv(EnvServerRequireClientCert, "1")
	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions: %v", err)
	}
	addr := serve(t, sopts)

	t.Setenv(EnvServerCert, "")
	t.Setenv(EnvServerKey, "")
	t.Setenv(EnvServerRequireClientCert, "")

	// A client that trusts the CA but presents no identity of its own must be
	// refused; this is the whole point of mTLS.
	t.Run("no client cert is refused", func(t *testing.T) {
		dopt, err := DialOption()
		if err != nil {
			t.Fatalf("DialOption: %v", err)
		}
		if err := check(t, addr, dopt); err == nil {
			t.Fatal("health check SUCCEEDED without a client certificate while the server required one")
		}
	})

	t.Run("client cert from the CA is accepted", func(t *testing.T) {
		t.Setenv(EnvClientCert, cliCert)
		t.Setenv(EnvClientKey, cliKey)
		dopt, err := DialOption()
		if err != nil {
			t.Fatalf("DialOption: %v", err)
		}
		if err := check(t, addr, dopt); err != nil {
			t.Fatalf("mTLS health check with a valid client certificate failed: %v", err)
		}
	})

	// A leaf from a DIFFERENT issuer proves the server verifies the chain rather
	// than merely observing that some certificate was presented.
	t.Run("client cert from another CA is refused", func(t *testing.T) {
		other := newTestCA(t, "attacker")
		badCert, badKey := other.issue(t, "impostor", nil, true)
		t.Setenv(EnvClientCert, badCert)
		t.Setenv(EnvClientKey, badKey)
		dopt, err := DialOption()
		if err != nil {
			t.Fatalf("DialOption: %v", err)
		}
		if err := check(t, addr, dopt); err == nil {
			t.Fatal("health check SUCCEEDED with a client certificate from an untrusted CA")
		}
	})
}

// TestServerNameOverrideFixesTheComposeNameMismatch covers the documented reason
// EnvServerName exists: PeerTarget yields dns:///host:port, so gRPC verifies
// against a compose service name that a real certificate does not carry.
func TestServerNameOverrideFixesTheComposeNameMismatch(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	// Issued for a name the client will NOT be dialing.
	srvCert, srvKey := ca.issue(t, "nexus.internal", []string{"nexus.internal"}, false)

	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions: %v", err)
	}
	addr := serve(t, sopts)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	// "localhost" is in neither the DNS names nor the IP addresses of the leaf, so
	// the handshake must fail on the name before the override is applied.
	target := "localhost:" + port

	t.Setenv(EnvServerCert, "")
	t.Setenv(EnvServerKey, "")
	t.Setenv(EnvCACert, ca.caFile)

	dopt, err := DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	if err := check(t, target, dopt); err == nil {
		t.Fatal("handshake SUCCEEDED against a certificate issued for a different name; nothing is verifying the hostname")
	}

	t.Setenv(EnvServerName, "nexus.internal")
	dopt, err = DialOption()
	if err != nil {
		t.Fatalf("DialOption with a server name override: %v", err)
	}
	if err := check(t, target, dopt); err != nil {
		t.Fatalf("handshake with %s=nexus.internal failed: %v", EnvServerName, err)
	}
}

// TestRequireTLSRefusesPlaintext is the production guard. Without it, a
// deployment that intended TLS and shipped without certs is indistinguishable
// from one that intended plaintext: every service is healthy and every bearer
// token crosses the network in the clear.
func TestRequireTLSRefusesPlaintext(t *testing.T) {
	for _, name := range []string{"NEXUS_REQUIRE_TLS"} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(name, "1")

			if !RequireTLS() {
				t.Fatalf("RequireTLS false with %s set; the guard is not armed", name)
			}
			if _, err := ServerOptions(); err == nil {
				t.Error("ServerOptions returned plaintext options while TLS was required")
			} else if !strings.Contains(err.Error(), EnvServerCert) {
				t.Errorf("error does not name %s, so an operator cannot tell what to set: %v", EnvServerCert, err)
			}
			if _, err := DialOption(); err == nil {
				t.Error("DialOption returned insecure credentials while TLS was required")
			} else if !strings.Contains(err.Error(), EnvCACert) {
				t.Errorf("error does not name %s: %v", EnvCACert, err)
			}
		})
	}
}

// TestRequireTLSIsSatisfiedByConfiguredCerts guards the other direction: the
// guard must not be a hard block on startup once certs ARE present, or arming it
// would mean no deployment can ever boot.
func TestRequireTLSIsSatisfiedByConfiguredCerts(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	srvCert, srvKey := ca.issue(t, "scheduler", []string{"scheduler"}, false)

	t.Setenv("NEXUS_ENV", "production")
	t.Setenv(EnvServerCert, srvCert)
	t.Setenv(EnvServerKey, srvKey)
	t.Setenv(EnvCACert, ca.caFile)

	sopts, err := ServerOptions()
	if err != nil {
		t.Fatalf("ServerOptions with certs configured under NEXUS_ENV=production: %v", err)
	}
	if len(sopts) == 0 {
		t.Fatal("ServerOptions returned nothing but no error; that is the silent plaintext the guard exists to prevent")
	}
	if _, err := DialOption(); err != nil {
		t.Fatalf("DialOption with a CA configured under NEXUS_ENV=production: %v", err)
	}
}

// TestNexusEnvDevKeepsPlaintextWorking pins the carve-out that makes the guard
// adoptable. NEXUS_ENV is already set in dev compose files for other reasons, and
// if any value armed the guard, adding this package would break every laptop.
func TestNexusEnvDevKeepsPlaintextWorking(t *testing.T) {
	for _, v := range []string{"dev", "development", "local", "test"} {
		clearEnv(t)
		t.Setenv("NEXUS_ENV", v)
		if RequireTLS() {
			t.Errorf("RequireTLS true with NEXUS_ENV=%s; the dev path must stay plaintext", v)
		}
	}
	// And the explicit override must beat a production-looking NEXUS_ENV, for the
	// staging or CI case that deliberately runs without certs.
	clearEnv(t)
	t.Setenv("NEXUS_ENV", "production")
	t.Setenv("NEXUS_REQUIRE_TLS", "0")
	if RequireTLS() {
		t.Error("NEXUS_REQUIRE_TLS=0 did not override NEXUS_ENV=production")
	}
}

// TestHalfConfiguredIsAnError covers the partial edit. Treating a cert with no key
// as "fall back to plaintext" would turn an interrupted config change into a
// silent downgrade, which is the exact failure this package exists to make
// impossible.
func TestHalfConfiguredIsAnError(t *testing.T) {
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "scheduler", []string{"scheduler"}, false)

	t.Run("server cert without key", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvServerCert, cert)
		if _, err := ServerOptions(); err == nil {
			t.Error("ServerOptions accepted a certificate with no key")
		}
	})
	t.Run("server key without cert", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvServerKey, key)
		if _, err := ServerOptions(); err == nil {
			t.Error("ServerOptions accepted a key with no certificate")
		}
	})
	t.Run("client cert without key", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvClientCert, cert)
		if _, err := DialOption(); err == nil {
			t.Error("DialOption accepted a client certificate with no key")
		}
	})
}

// TestRequireClientCertWithoutCAIsRefused guards a configuration that is strictly
// worse than the plaintext it replaces: requiring a client certificate with
// nothing to verify it against accepts any self-signed leaf, while still reading
// as mTLS in a config review.
func TestRequireClientCertWithoutCAIsRefused(t *testing.T) {
	clearEnv(t)
	ca := newTestCA(t, "nexus")
	cert, key := ca.issue(t, "scheduler", []string{"scheduler"}, false)

	t.Setenv(EnvServerCert, cert)
	t.Setenv(EnvServerKey, key)
	t.Setenv(EnvServerRequireClientCert, "true")
	if _, err := ServerOptions(); err == nil {
		t.Fatal("ServerOptions required a client certificate with no CA bundle to verify it against")
	}
}

// TestUnreadableFilesAreErrorsNotDowngrades pins the no-silent-fallback rule for
// the paths an operator is most likely to mistype.
func TestUnreadableFilesAreErrorsNotDowngrades(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing key pair", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvServerCert, filepath.Join(dir, "absent.pem"))
		t.Setenv(EnvServerKey, filepath.Join(dir, "absent-key.pem"))
		if _, err := ServerOptions(); err == nil {
			t.Error("ServerOptions silently fell back to plaintext for a missing certificate file")
		}
	})

	t.Run("missing CA bundle", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvCACert, filepath.Join(dir, "absent-ca.pem"))
		if _, err := DialOption(); err == nil {
			t.Error("DialOption silently fell back to insecure credentials for a missing CA bundle")
		}
	})

	// A DER file, a private key, or a truncated download all look like this, and
	// each would otherwise produce an empty pool that trusts nothing and fails at
	// handshake time instead of at startup.
	t.Run("CA bundle with no PEM", func(t *testing.T) {
		clearEnv(t)
		junk := filepath.Join(dir, "junk-ca.pem")
		if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv(EnvCACert, junk)
		if _, err := DialOption(); err == nil {
			t.Error("DialOption accepted a CA bundle containing no certificates")
		}
	})
}

// TestNexusEnvDoesNotArmTheTLSGuard pins a deliberate asymmetry with
// config.DSNRequired(), which NEXUS_ENV DOES arm.
//
// The two guards look symmetrical and are not. A missing DSN has no working
// outcome -- the service cannot serve without a database. Missing TLS has one:
// every existing deployment runs plaintext today. If NEXUS_ENV armed this, then
// merely WIRING TLS support into the services would break every deployment that
// had set NEXUS_ENV=production for the DSN guard, at the moment of wiring and for
// a reason nothing in their configuration changed to cause. Requiring TLS has to
// be its own decision, made once certs exist to satisfy it.
func TestNexusEnvDoesNotArmTheTLSGuard(t *testing.T) {
	for _, env := range []string{"production", "prod", "staging"} {
		clearEnv(t)
		t.Setenv("NEXUS_ENV", env)
		if RequireTLS() {
			t.Errorf("NEXUS_ENV=%s armed the TLS guard; wiring TLS in would then break "+
				"every deployment already setting it for the DSN guard", env)
		}
		// And plaintext must still be what an unconfigured deployment gets.
		if opts, err := ServerOptions(); err != nil || opts != nil {
			t.Errorf("NEXUS_ENV=%s: ServerOptions = (%v, %v), want (nil, nil)", env, opts, err)
		}
	}
}

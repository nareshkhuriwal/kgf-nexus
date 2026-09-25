// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package bustest gives the NATS authorization integration tests (in
// internal/bus, the event-service relay and the agent) one shared description of
// the broker under test: where it is, which role identities exist and how to
// dial it as each of them.
//
// The broker is started by tests/nats/run-integration.sh with a configuration
// rendered by deploy/ -- once from deploy/nats/nexus-nats-auth.sh (the compose
// path) and once from the Helm chart with TLS on -- so a passing test says
// something about the configurations that ship, not about a hand-written one.
package bustest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
)

// Environment the runner sets. Unset URL means "not under the runner": tests
// skip with the standard marker, which the runner turns into a failure (F85).
const (
	EnvURL        = "NEXUS_TEST_NATS_AUTHZ_URL"
	EnvDir        = "NEXUS_TEST_NATS_AUTHZ_DIR"     // <role>/user.nk per role
	EnvTenants    = "NEXUS_TEST_NATS_AUTHZ_TENANTS" // space-separated; at least two
	EnvCA         = "NEXUS_TEST_NATS_AUTHZ_CA"      // set: the broker requires TLS
	EnvClientCert = "NEXUS_TEST_NATS_AUTHZ_CLIENT_CERT"
	EnvClientKey  = "NEXUS_TEST_NATS_AUTHZ_CLIENT_KEY"
	EnvVariant    = "NEXUS_TEST_NATS_AUTHZ_VARIANT" // label for logs: compose | helm
)

// Broker is the authorization-enabled broker under test.
type Broker struct {
	URL     string
	Dir     string
	Tenants []string
	TLS     bus.TLSConfig
	Variant string
}

// Authz describes the broker, or skips the test when the runner did not start one.
func Authz(t *testing.T) *Broker {
	t.Helper()
	u := os.Getenv(EnvURL)
	if u == "" {
		t.Skip("skipping integration test: " + EnvURL + " is not set (use tests/nats/run-integration.sh)")
	}
	b := &Broker{
		URL:     u,
		Dir:     os.Getenv(EnvDir),
		Tenants: strings.Fields(os.Getenv(EnvTenants)),
		Variant: os.Getenv(EnvVariant),
	}
	if b.Dir == "" || len(b.Tenants) < 2 {
		t.Fatalf("%s and at least two tenants in %s are required", EnvDir, EnvTenants)
	}
	if ca := os.Getenv(EnvCA); ca != "" {
		b.TLS = bus.TLSConfig{Enabled: true, CAFile: ca,
			CertFile: os.Getenv(EnvClientCert), KeyFile: os.Getenv(EnvClientKey)}
	}
	t.Logf("NATS authorization broker: variant=%s url=%s tls=%v tenants=%v", b.Variant, b.URL, b.TLS.Enabled, b.Tenants)
	return b
}

// Seed is the seed file for a role (event-service, monitor, agent-<ns>).
func (b *Broker) Seed(role string) string { return filepath.Join(b.Dir, role, "user.nk") }

// Config is how the production code would be configured for role. name is the
// connection name, which also fixes the reply-inbox prefix the permissions
// grant (bus.InboxPrefixFor), so pass the name the real process uses.
func (b *Broker) Config(role, name string) bus.Config {
	return bus.Config{
		URL:          b.URL,
		NKeySeedFile: b.Seed(role),
		RequireAuth:  true,
		TLS:          b.TLS,
		RequireTLS:   b.TLS.Enabled,
		Name:         name,
		InboxPrefix:  bus.InboxPrefixFor(name),
	}
}

// Connect dials as role through bus.Connect -- the production connect path --
// and waits for the connection to be established. extra options come last, so
// a test can replace the error handler to observe permission violations.
func (b *Broker) Connect(t *testing.T, role, name string, extra ...nats.Option) *nats.Conn {
	t.Helper()
	nc, err := bus.Connect(b.Config(role, name), nil, extra...)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(nc.Close)
	WaitConnected(t, nc, role)
	return nc
}

// WaitConnected fails (never skips: the broker is known to be up) when nc does
// not reach CONNECTED, naming the last error, which is how a rejected
// credential shows up under RetryOnFailedConnect.
func WaitConnected(t *testing.T, nc *nats.Conn, who string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for nc.Status() != nats.CONNECTED {
		if time.Now().After(deadline) {
			t.Fatalf("%s never connected (status %v, last error %v)", who, nc.Status(), nc.LastError())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("%s flush: %v", who, err)
	}
}

// Errors collects asynchronous errors (permission violations arrive this way).
type Errors struct{ C chan error }

// NewErrors returns a collector and the option that installs it.
func NewErrors() (*Errors, nats.Option) {
	e := &Errors{C: make(chan error, 64)}
	return e, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		select {
		case e.C <- err:
		default:
		}
	})
}

// ExpectViolation waits for a permissions violation mentioning subject.
func (e *Errors) ExpectViolation(t *testing.T, what, subject string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-e.C:
			if strings.Contains(strings.ToLower(err.Error()), "permissions violation") && strings.Contains(err.Error(), subject) {
				return
			}
			t.Logf("%s: unrelated async error while waiting: %v", what, err)
		case <-deadline:
			t.Fatalf("%s: the broker did not refuse it (no permissions violation for %q)", what, subject)
		}
	}
}

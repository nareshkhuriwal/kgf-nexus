//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus/bustest"
)

// TestNATSAuthzAgentHintsWithTenantCredentials drives SubscribeDispatchHints --
// the agent's only NATS code -- with a tenant-scoped NKey against the broker
// configuration deploy/ renders. Its own tenant's hints wake it; a worker
// misconfigured to serve ANOTHER tenant's namespace with these credentials is
// never woken, because the broker refuses the subscription.
func TestNATSAuthzAgentHintsWithTenantCredentials(t *testing.T) {
	b := bustest.Authz(t)
	own, other := b.Tenants[0], b.Tenants[1]
	es := b.Connect(t, "event-service", "nexus-event-service")

	var mine, stolen atomic.Int64
	h, err := SubscribeDispatchHints(b.Config("agent-"+own, "nexus-agent"), own, []string{"default", "gpu"},
		func() { mine.Add(1) }, testLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	waitFor(t, "agent hint connection", 10*time.Second, h.Connected)
	_ = h.nc.Flush()

	wrong, err := SubscribeDispatchHints(b.Config("agent-"+own, "nexus-agent"), other, []string{"default"},
		func() { stolen.Add(1) }, testLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wrong.Close)
	waitFor(t, "second agent connection", 10*time.Second, wrong.Connected)
	_ = wrong.nc.Flush()

	for _, q := range []string{"default", "gpu"} {
		if err := es.Publish(bus.DispatchSubject(own, q), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := es.Publish(bus.DispatchSubject(other, "default"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	_ = es.Flush()
	waitFor(t, "own-tenant hints", 5*time.Second, func() bool { return mine.Load() >= 2 })
	time.Sleep(500 * time.Millisecond)
	if n := stolen.Load(); n != 0 {
		t.Fatalf("an agent holding tenant %s's credentials was woken %d times by tenant %s's hints", own, n, other)
	}
}

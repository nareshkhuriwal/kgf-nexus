//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
	"github.com/khuriwalgroup/kgf-nexus/internal/bus/bustest"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	eventservice "github.com/khuriwalgroup/kgf-nexus/services/event-service"
)

// TestNATSAuthzRelayRunsAsEventService runs the REAL relay, as the event-service
// identity, against the broker configuration deploy/ renders: streams are
// ensured, run events are published and ACKED by JetStream (a row is only
// marked published after the stream's ack, which comes back through the
// role's own reply inbox), the SSE waker is woken, a dispatch hint reaches an
// agent holding that tenant's credentials, and the reconcile hint works too.
// Nothing here may count a publish failure.
func TestNATSAuthzRelayRunsAsEventService(t *testing.T) {
	b := bustest.Authz(t)
	pool := testPG(t)
	ns := b.Tenants[0]
	w := newWorldNS(t, pool, ns)

	es := b.Connect(t, "event-service", "nexus-event-service")
	agent := b.Connect(t, "agent-"+ns, "nexus-agent")

	r, m := newRelay(t, pool, es)
	waker := eventservice.NewRunWaker()
	if _, err := waker.Attach(es); err != nil {
		t.Fatal(err)
	}
	woken, stop := waker.Subscribe(w.runID)
	defer stop()

	hints, err := agent.SubscribeSync(bus.DispatchSubject(ns, w.queue))
	if err != nil {
		t.Fatal(err)
	}
	_ = agent.Flush()
	_ = es.Flush()

	w.appendEvents(3)
	if _, err := pool.Exec(w.ctx, `INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
		VALUES ($1::uuid,'n1','t','PENDING','{}')`, w.runID); err != nil {
		t.Fatal(err)
	}
	res, err := store.NewDAGEngineStore(pool).ApplyNodeStateAndCreateAttempts(w.ctx, w.runID,
		[]store.NodeTransition{{NodeID: "n1", From: domain.NodePending, To: domain.NodeReady}},
		[]store.NewAttempt{{RunID: w.runID, NodeID: "n1", Queue: w.queue,
			Resources: &nexusv1.ResourceVector{CpuMillis: 1}, IdempotencyKey: "k-" + w.runID}})
	if err != nil || len(res.AttemptsCreated) != 1 {
		t.Fatalf("create attempt: %v %+v", err, res)
	}

	drain(t, r, w)
	var pending int
	if err := pool.QueryRow(w.ctx, `SELECT count(*)::int FROM outbox WHERE namespace = $1 AND published_at IS NULL`, ns).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	for i := 0; pending > 0 && i < 50; i++ {
		_, _ = r.PublishBatch(w.ctx)
		_ = pool.QueryRow(w.ctx, `SELECT count(*)::int FROM outbox WHERE namespace = $1 AND published_at IS NULL`, ns).Scan(&pending)
	}
	if pending > 0 {
		t.Fatalf("%d outbox rows for %s were never acknowledged by JetStream", pending, ns)
	}

	select {
	case <-woken:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSE waker was not woken by the relayed run event")
	}
	if _, err := hints.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the tenant's agent did not receive the relayed dispatch hint: %v", err)
	}
	if n, err := r.PublishReadyHints(w.ctx); err != nil || n == 0 {
		t.Fatalf("reconcile hints as event-service: n=%d err=%v", n, err)
	}
	if _, err := hints.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the reconcile hint did not reach the agent: %v", err)
	}
	for _, kind := range []string{"row", "broker"} {
		if f := m.Failures.With(metrics.Labels{"kind": kind}).Value(); f != 0 {
			t.Errorf("the relay counted %v %s publish failures under the shipped permissions", f, kind)
		}
	}
}

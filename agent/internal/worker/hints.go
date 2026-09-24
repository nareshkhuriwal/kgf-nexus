// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
)

// HintSubscription listens for dispatch hints on the worker's queues and turns
// each into a Wake. It holds the only NATS state the agent has.
type HintSubscription struct {
	nc   *nats.Conn
	subs []*nats.Subscription
}

// SubscribeDispatchHints connects to the bus and subscribes to
// nexus.task.dispatch.<namespace>.<queue> for every queue the worker serves.
//
// It never makes NATS a dependency of the agent. The connection is made with
// RetryOnFailedConnect, so a broker that is down at startup is simply retried in
// the background while the worker long-polls; the client re-establishes these
// subscriptions on every reconnect. The only error returned is a configuration
// the client rejects outright, and the caller logs it and carries on polling.
//
// Plain core subscriptions, not JetStream consumers, on purpose: a hint is worth
// something only NOW, so there is nothing to replay after a disconnect, and a
// durable consumer per worker would be broker state that outlives the worker's
// session for no benefit. The periodic reconcile hint (event-service) re-covers
// anything missed while disconnected.
func SubscribeDispatchHints(cfg bus.Config, namespace string, queues []string,
	wake func(), log *slog.Logger) (*HintSubscription, error) {
	nc, err := bus.Connect(cfg, log)
	if err != nil {
		return nil, err
	}
	h := &HintSubscription{nc: nc}
	for _, q := range queues {
		subject := bus.DispatchSubject(namespace, q)
		// The handler ignores the payload entirely. Whatever the hint claims --
		// an attempt id, a queue depth -- the claim in Postgres is the only
		// authority, so the safest parse is none.
		sub, err := nc.Subscribe(subject, func(*nats.Msg) { wake() })
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("agent: subscribe %s: %w", subject, err)
		}
		// A slow consumer here would only mean dropped hints, which are
		// harmless; bound the buffer so a hint flood cannot grow agent memory.
		_ = sub.SetPendingLimits(1024, 1<<20)
		h.subs = append(h.subs, sub)
	}
	log.Info("subscribed to dispatch hints; long polling remains the fallback",
		"namespace", namespace, "queues", queues)
	return h, nil
}

// Connected reports whether the hint connection is currently up.
func (h *HintSubscription) Connected() bool {
	return h != nil && h.nc != nil && h.nc.Status() == nats.CONNECTED
}

// Close drops the subscriptions and the connection.
func (h *HintSubscription) Close() {
	if h == nil || h.nc == nil {
		return
	}
	for _, s := range h.subs {
		_ = s.Unsubscribe()
	}
	h.nc.Close()
}

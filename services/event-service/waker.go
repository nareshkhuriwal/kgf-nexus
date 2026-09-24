// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eventservice

import (
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/khuriwalgroup/kgf-nexus/internal/bus"
)

// RunWaker wakes SSE tails the moment the bus says their run changed, instead of
// at their next poll tick.
//
// It is a HINT, like everything on the bus: a woken tail re-reads Postgres by
// after_seq exactly as a ticking one does, so a duplicate wake costs one cheap
// query and a lost wake costs one poll interval. The tail never renders what the
// message said -- the database remains the only thing a client is shown.
//
// One NATS subscription per process fans out to any number of local tails, rather
// than one subscription per SSE client, so a thousand open browser tabs cost the
// broker one interest, not a thousand. It also works across replicas: whichever
// event-service replica relayed the event, every replica receives it.
type RunWaker struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

// NewRunWaker builds an unattached waker. Unattached, it simply never fires, and
// tails fall back to their poll interval.
func NewRunWaker() *RunWaker {
	return &RunWaker{waiters: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe registers interest in one run. The returned channel has capacity one
// and is signalled without blocking, so a burst of events for a busy run
// coalesces into one wake rather than queueing N re-reads behind a slow client.
func (w *RunWaker) Subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	set := w.waiters[runID]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		w.waiters[runID] = set
	}
	set[ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if set := w.waiters[runID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(w.waiters, runID)
			}
		}
	}
}

// Notify wakes every tail of runID.
func (w *RunWaker) Notify(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.waiters[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Attach subscribes the waker to every run subject on nc. The subscription
// survives reconnects (the client re-establishes it), so an outage only means
// tails poll until the broker returns.
func (w *RunWaker) Attach(nc *nats.Conn) (*nats.Subscription, error) {
	sub, err := nc.Subscribe(bus.AllRunsSubject, func(m *nats.Msg) {
		if runID, ok := bus.RunIDFromSubject(m.Subject); ok {
			w.Notify(runID)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("event-service: subscribe %s: %w", bus.AllRunsSubject, err)
	}
	return sub, nil
}

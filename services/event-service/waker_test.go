// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eventservice

import (
	"testing"
	"time"
)

func woke(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// TestRunWakerTargetsOnlyTheRun: a wake for one run must not make every open tail
// re-query the database.
func TestRunWakerTargetsOnlyTheRun(t *testing.T) {
	w := NewRunWaker()
	a, cancelA := w.Subscribe("run-a")
	defer cancelA()
	b, cancelB := w.Subscribe("run-b")
	defer cancelB()

	w.Notify("run-a")
	if !woke(a) {
		t.Error("the run's own tail was not woken")
	}
	if woke(b) {
		t.Error("another run's tail was woken")
	}
}

// TestRunWakerCoalescesBursts: a busy run publishing 1000 events wakes a slow
// client once, not 1000 times, and Notify never blocks the NATS callback.
func TestRunWakerCoalescesBursts(t *testing.T) {
	w := NewRunWaker()
	ch, cancel := w.Subscribe("run")
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			w.Notify("run")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on a tail that was not reading")
	}
	if !woke(ch) {
		t.Fatal("no wake at all")
	}
	if woke(ch) {
		t.Error("a burst queued more than one wake")
	}
}

func TestRunWakerUnsubscribeCleansUp(t *testing.T) {
	w := NewRunWaker()
	_, cancel := w.Subscribe("run")
	cancel()
	w.Notify("run") // must not panic on a closed-out set
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waiters) != 0 {
		t.Errorf("waiters leaked: %v", w.waiters)
	}
}

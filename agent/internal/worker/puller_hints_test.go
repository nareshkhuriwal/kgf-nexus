// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// gatedPollClient records polls and can hold one open, so a test can deliver
// hints WHILE a poll is in flight -- the interleaving that decides whether hints
// can ever cancel a claim or pile up.
type gatedPollClient struct {
	polls     atomic.Int64
	pollAgain int32
	err       error

	mu        sync.Mutex
	gate      chan struct{} // when non-nil, the NEXT poll blocks on it
	cancelled atomic.Bool   // a held poll saw its ctx cancelled
	started   chan struct{}
}

func (c *gatedPollClient) PollTask(ctx context.Context, _ *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error) {
	c.polls.Add(1)
	c.mu.Lock()
	gate := c.gate
	c.gate = nil
	started := c.started
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			c.cancelled.Store(true)
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return &nexusv1.PollTaskResponse{PollAgainAfterMs: c.pollAgain}, nil
}

func waitPolls(t *testing.T, c *gatedPollClient, n int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for c.polls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d polls, wanted %d within %v", c.polls.Load(), n, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startPuller(t *testing.T, c *gatedPollClient) (*Puller, func()) {
	t.Helper()
	reg := NewRegistry(vec(4000, 4<<30, 0))
	p := NewPuller("w-hint", []string{"default"}, c, reg, newFakeLeases(), nil, testLog())
	p.LongPollWait = 10 * time.Millisecond
	p.ErrorBackoff = 10 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	return p, func() { cancel(); <-done }
}

// TestWakeCutsThePauseBetweenPolls is the whole point of a hint: a worker idling
// between polls polls again at once instead of waiting out the server's
// poll_again_after_ms.
//
// The control half matters as much as the assertion (F89): without a Wake, NO
// second poll may happen in the same window, or the test would pass for a puller
// that simply polls fast and ignores hints entirely.
func TestWakeCutsThePauseBetweenPolls(t *testing.T) {
	c := &gatedPollClient{pollAgain: 10_000} // a 10s pause between polls
	p, stop := startPuller(t, c)
	defer stop()

	waitPolls(t, c, 1, time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := c.polls.Load(); n != 1 {
		t.Fatalf("control failed: %d polls without any hint; the pause is not being honoured, "+
			"so this test could not tell a hint from a fast loop", n)
	}

	p.Wake()
	waitPolls(t, c, 2, 500*time.Millisecond)
	if got := p.HintsReceived(); got != 1 {
		t.Errorf("HintsReceived = %d, want 1", got)
	}
}

// TestDuplicateAndReorderedHintsCoalesce: a flood of hints -- duplicates of one
// message, stale hints for work already claimed, hints arriving in any order --
// collapses into at most ONE extra poll. The claim in Postgres decides what (if
// anything) the worker gets, so the only cost a hint may ever impose is a poll,
// and a flood must not multiply it.
func TestDuplicateAndReorderedHintsCoalesce(t *testing.T) {
	c := &gatedPollClient{pollAgain: 10_000, gate: make(chan struct{}), started: make(chan struct{}, 1)}
	gate := c.gate
	p, stop := startPuller(t, c)
	defer stop()

	<-c.started // the first poll is now held open
	for i := 0; i < 100; i++ {
		p.Wake()
	}
	close(gate)

	// One poll held + at most one coalesced follow-up; then the 10s pause.
	waitPolls(t, c, 2, time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := c.polls.Load(); n != 2 {
		t.Errorf("100 hints during one poll produced %d polls in total, want exactly 2 "+
			"(the held poll plus one coalesced re-poll)", n)
	}
	if got := p.HintsReceived(); got != 100 {
		t.Errorf("HintsReceived = %d, want 100", got)
	}
}

// TestHintNeverCancelsAPollInFlight: cancelling a PollTask whose claim had
// already committed would strand a leased assignment until the lease expired and
// the attempt went LOST. A hint must wait for the poll to finish.
func TestHintNeverCancelsAPollInFlight(t *testing.T) {
	c := &gatedPollClient{pollAgain: 10_000, gate: make(chan struct{}), started: make(chan struct{}, 1)}
	gate := c.gate
	p, stop := startPuller(t, c)
	defer stop()

	<-c.started
	p.Wake()
	time.Sleep(100 * time.Millisecond)
	if c.cancelled.Load() {
		t.Fatal("a hint cancelled the poll in flight")
	}
	if n := c.polls.Load(); n != 1 {
		t.Fatalf("a hint started a second, concurrent poll (%d polls)", n)
	}
	close(gate)
	waitPolls(t, c, 2, time.Second)
}

// TestHintDoesNotShortenErrorBackoff: while the control plane is failing, hints
// must not turn the fleet's backoff into a retry storm against it.
func TestHintDoesNotShortenErrorBackoff(t *testing.T) {
	c := &gatedPollClient{err: errors.New("control plane unavailable")}
	p, stop := startPuller(t, c)
	defer stop()

	waitPolls(t, c, 1, time.Second)
	for i := 0; i < 10; i++ {
		p.Wake()
	}
	time.Sleep(300 * time.Millisecond)
	if n := c.polls.Load(); n != 1 {
		t.Errorf("hints cut the 10s error backoff: %d polls", n)
	}
}

// TestPullerWorksWithNoHintSource: the bus is optional. A puller nobody ever
// wakes still polls on its own schedule.
func TestPullerWorksWithNoHintSource(t *testing.T) {
	c := &gatedPollClient{pollAgain: 20}
	_, stop := startPuller(t, c)
	defer stop()
	waitPolls(t, c, 3, time.Second)
}

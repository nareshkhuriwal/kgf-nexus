// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"testing"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// factsRM is fakeRM plus the optional placement-fact extensions.
type factsRM struct {
	fakeRM
	cached      []string
	drainReason string
}

func (f *factsRM) RecordHeartbeatFacts(_ context.Context, _ string, cached []string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	f.cached = cached
	return nil
}

func (f *factsRM) DrainWorkerForReason(_ context.Context, _ string, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drainReason = reason
	return nil
}

// An agent that received a spot interruption notice says so on its heartbeat;
// the gateway marks it DRAINING (so the push path stops too) and answers with
// the DRAIN directive in the same beat (ADR-0030 §2).
func TestHeartbeatEvictionDrainReasonDrainsTheWorker(t *testing.T) {
	rm := &factsRM{fakeRM: fakeRM{exists: true, state: "ACTIVE"}}
	s := testServer(newLeaseDB(), rm, &fakeSched{})
	resp, err := s.Heartbeat(workerCtx(), &nexusv1.HeartbeatRequest{WorkerId: "w1", DrainReason: "eviction:aws"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.drainReason != "eviction:aws" {
		t.Fatalf("worker not drained, reason %q", rm.drainReason)
	}
	var drain bool
	for _, d := range resp.GetDirectives() {
		drain = drain || d.GetKind() == nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN
	}
	if !drain {
		t.Fatal("no DRAIN directive after the agent reported an eviction")
	}
}

func TestHeartbeatForwardsCachedArtifactsOnlyWhenReported(t *testing.T) {
	rm := &factsRM{fakeRM: fakeRM{exists: true, state: "ACTIVE"}}
	s := testServer(newLeaseDB(), rm, &fakeSched{})
	if _, err := s.Heartbeat(workerCtx(), &nexusv1.HeartbeatRequest{WorkerId: "w1",
		CachedArtifacts: []string{"sha256:a"}, CachedArtifactsReported: true}); err != nil {
		t.Fatal(err)
	}
	if len(rm.cached) != 1 || rm.cached[0] != "sha256:a" {
		t.Fatalf("cached artifacts not forwarded: %v", rm.cached)
	}
	// An old agent reports nothing; the stored set must not be touched.
	rm.cached = []string{"untouched"}
	if _, err := s.Heartbeat(workerCtx(), &nexusv1.HeartbeatRequest{WorkerId: "w1"}); err != nil {
		t.Fatal(err)
	}
	if len(rm.cached) != 1 || rm.cached[0] != "untouched" {
		t.Fatalf("a heartbeat without the field overwrote the cached set: %v", rm.cached)
	}
	if rm.drainReason != "" {
		t.Fatal("a plain heartbeat drained the worker")
	}
}

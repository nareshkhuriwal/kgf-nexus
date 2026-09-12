// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEventServiceHTTPAddrFixesTheGRPCPort covers the config mismatch that made every
// publish fail with "connection refused".
//
// The fleet hands every peer address as host:9090 because that is the gRPC
// convention, but event-service serves HTTP and binds 8080 only. Nothing listened
// on the configured port.
func TestEventServiceHTTPAddrFixesTheGRPCPort(t *testing.T) {
	cases := map[string]string{
		"event-service:9090": "event-service:8080",
		// A deliberate port is respected: an operator who moved the service must
		// not have their choice silently rewritten.
		"event-service:8081": "event-service:8081",
		"event-service:8080": "event-service:8080",
		// No port: fall back to what the service actually listens on.
		"event-service": "event-service:8080",
		// Unset stays unset, so the "address is unset" path still reports itself
		// rather than dialing ":8080" on the local host.
		"": "",
	}
	for in, want := range cases {
		if got := eventServiceHTTPAddr(in); got != want {
			t.Errorf("eventServiceHTTPAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEventClientPublishesToTheOwner is the regression test for the stub: Publish
// used to return "event-service gRPC is not wired yet" and never sent anything, so
// no RunStateChanged event was ever recorded for any run and the audit trail that
// run_events exists to provide was empty.
func TestEventClientPublishesToTheOwner(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"seq":7}`))
	}))
	defer srv.Close()

	node := "step1"
	c := NewEventClient(strings.TrimPrefix(srv.URL, "http://"), quietLogger())
	if err := c.Publish(context.Background(), "run-123", &node, nil,
		"RunStateChanged", map[string]any{"to": "PAUSED"}, "workflow-manager"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/v1/runs/run-123/events"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotBody["kind"] != "RunStateChanged" {
		t.Errorf("kind = %v, want RunStateChanged", gotBody["kind"])
	}
	// Actor is required by the owner and is what makes the row an audit record
	// rather than a bare state log.
	if gotBody["actor"] != "workflow-manager" {
		t.Errorf("actor = %v, want workflow-manager", gotBody["actor"])
	}
	if gotBody["node_id"] != "step1" {
		t.Errorf("node_id = %v, want step1", gotBody["node_id"])
	}
	// seq must NOT be sent: it is allocated from workflow_runs.event_seq by the
	// owner, and a client-chosen value would race (CONTRACT.md, EVENT SEQ).
	if _, present := gotBody["seq"]; present {
		t.Error("body carried seq; only the owner may allocate it")
	}
}

// TestEventClientReportsTheOwnersRejection keeps a 4xx from being reported as an
// outage: the body carries the reason, and retrying a malformed event never helps.
func TestEventClientReportsTheOwnersRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"kind and actor are required"}`))
	}))
	defer srv.Close()

	c := NewEventClient(strings.TrimPrefix(srv.URL, "http://"), quietLogger())
	err := c.Publish(context.Background(), "run-1", nil, nil, "", nil, "")
	if err == nil {
		t.Fatal("a rejected publish returned no error")
	}
	if !strings.Contains(err.Error(), "kind and actor are required") {
		t.Errorf("error %q does not carry the owner's reason", err)
	}
}

// TestEventClientUnconfiguredIsExplicit keeps an unset address from looking like a
// successful publish, which would silently drop the whole audit trail.
func TestEventClientUnconfiguredIsExplicit(t *testing.T) {
	c := NewEventClient("", quietLogger())
	err := c.Publish(context.Background(), "run-1", nil, nil, "RunCreated", nil, "wfm")
	if err == nil {
		t.Fatal("an unconfigured client reported success")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q does not say the peer is unconfigured", err)
	}
}

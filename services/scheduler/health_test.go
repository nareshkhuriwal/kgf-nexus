// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestHealthzDoesNotDependOnTheDatabase pins the most important property of the
// liveness probe. /healthz must stay OK during a database outage: if it failed,
// the orchestrator would kill every scheduler replica, restarting them fixes
// nothing, and the fleet would be empty the moment Postgres returned.
//
// The store is nil here, which is the strongest possible statement of the
// property — the handler cannot be touching it.
func TestHealthzDoesNotDependOnTheDatabase(t *testing.T) {
	var ready atomic.Bool
	ready.Store(false) // even while draining, liveness stays OK
	mux := healthMux(nil, nil, &ready)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 regardless of DB or drain state", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

// TestReadyzFailsWhileDraining covers the shutdown ordering: readiness must fail
// before the loops stop, so the load balancer stops routing to a replica that is
// on its way out.
func TestReadyzFailsWhileDraining(t *testing.T) {
	var ready atomic.Bool
	ready.Store(false)
	mux := healthMux(nil, nil, &ready)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining = %d, want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/readyz body is not JSON: %v", err)
	}
	if body["status"] != "draining" {
		t.Errorf("status = %q, want draining", body["status"])
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]string{
		"debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR",
		// An unrecognised level must not silence the service; info is the safe
		// fallback.
		"": "INFO", "nonsense": "INFO",
	}
	for in, want := range tests {
		if got := parseLevel(in).String(); got != want {
			t.Errorf("parseLevel(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("NEXUS_TEST_ENVOR", "set-value")
	if got := envOr("NEXUS_TEST_ENVOR", "fallback"); got != "set-value" {
		t.Errorf("envOr with a set var = %q, want the var's value", got)
	}
	if got := envOr("NEXUS_TEST_ENVOR_MISSING", "fallback"); got != "fallback" {
		t.Errorf("envOr with an unset var = %q, want the fallback", got)
	}
	// An empty env var must fall back rather than configure an empty DSN or
	// listen address, which would fail confusingly at startup.
	t.Setenv("NEXUS_TEST_ENVOR_EMPTY", "")
	if got := envOr("NEXUS_TEST_ENVOR_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("envOr with an empty var = %q, want the fallback", got)
	}
}

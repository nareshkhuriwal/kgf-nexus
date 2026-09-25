// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/worker"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
)

// TestReadyzFollowsRegistration: /readyz is 503 until the agent holds a
// registered session and again after it loses it, while /healthz stays 200
// throughout -- a control plane that is not up yet must make the agent
// not-ready, never restart it.
func TestReadyzFollowsRegistration(t *testing.T) {
	ready := &worker.Readiness{}
	mux := opsMux(metrics.NewServiceRegistry("agent", "test"), ready)
	get := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if c := get("/readyz"); c != http.StatusServiceUnavailable {
		t.Errorf("/readyz before registration = %d, want 503", c)
	}
	if c := get("/healthz"); c != http.StatusOK {
		t.Errorf("/healthz before registration = %d, want 200", c)
	}
	ready.Set(true)
	if c := get("/readyz"); c != http.StatusOK {
		t.Errorf("/readyz while registered = %d, want 200", c)
	}
	ready.Set(false)
	if c := get("/readyz"); c != http.StatusServiceUnavailable {
		t.Errorf("/readyz after the session ended = %d, want 503", c)
	}
}

// A capacity setting the agent cannot read exactly must be refused, never
// truncated or defaulted. The float-notation case is the one that actually
// happened: a Helm-rendered byte count became "1.7179869184e+10", the old
// parser read it as 1, and the agent advertised 1 byte of memory.
func TestParseEnvIntIsStrict(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{"", 42, false},
		{"17179869184", 17179869184, false},
		{" 8000 ", 8000, false},
		{"1.7179869184e+10", 0, true},
		{"5.36870912e+10", 0, true},
		{"16Gi", 0, true},
		{"12abc", 0, true},
		{"1.5", 0, true},
	} {
		got, err := parseEnvInt("NEXUS_MEMORY_BYTES", tc.raw, 42)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseEnvInt(%q): err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseEnvInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

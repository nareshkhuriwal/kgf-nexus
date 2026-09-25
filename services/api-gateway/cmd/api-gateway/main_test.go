// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/health"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

func clearRateLimitEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NEXUS_RATE_LIMIT_RPS", "NEXUS_RATE_LIMIT_BURST", "NEXUS_RATE_LIMIT_MAX_KEYS",
		"NEXUS_RATE_LIMIT_IP_RPS", "NEXUS_RATE_LIMIT_IP_BURST",
		"NEXUS_RATE_LIMIT_AUTH_FAIL_RPS", "NEXUS_RATE_LIMIT_AUTH_FAIL_BURST",
	} {
		t.Setenv(k, "")
	}
}

// Unset stays off: rate limiting is opt-in, and a deployment that never set
// NEXUS_RATE_LIMIT_RPS must see no 429s after upgrading — even if it set one of
// the newer knobs on its own.
func TestRateLimitsFromEnvIsOptIn(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_IP_RPS", "10")
	if _, ok := rateLimitsFromEnv(); ok {
		t.Fatal("rate limiting enabled without NEXUS_RATE_LIMIT_RPS")
	}
}

// The two original knobs keep their meaning: the per-credential allowance.
func TestRateLimitsFromEnvKeepsTheOriginalKnobs(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_RPS", "20")
	t.Setenv("NEXUS_RATE_LIMIT_BURST", "40")
	got, ok := rateLimitsFromEnv()
	if !ok {
		t.Fatal("not enabled")
	}
	want := rest.GatewayRateLimitsFor(rest.RateLimitConfig{RequestsPerSecond: 20, Burst: 40})
	if got != want {
		t.Fatalf("got %+v, want the derived defaults %+v", got, want)
	}
}

func TestRateLimitsFromEnvOverrides(t *testing.T) {
	clearRateLimitEnv(t)
	t.Setenv("NEXUS_RATE_LIMIT_RPS", "20")
	t.Setenv("NEXUS_RATE_LIMIT_MAX_KEYS", "5000")
	t.Setenv("NEXUS_RATE_LIMIT_IP_RPS", "300")
	t.Setenv("NEXUS_RATE_LIMIT_AUTH_FAIL_BURST", "25")
	got, _ := rateLimitsFromEnv()

	// A rate override without a burst takes burst = rate, the rule the original
	// NEXUS_RATE_LIMIT_BURST follows.
	if got.PerAddress.RequestsPerSecond != 300 || got.PerAddress.Burst != 0 {
		t.Errorf("per-address = %+v, want 300/s with burst defaulting to the rate", got.PerAddress)
	}
	// A burst override alone keeps the default rate.
	if got.AuthFailures.Burst != 25 || got.AuthFailures.RequestsPerSecond != rest.DefaultAuthFailureLimit.RequestsPerSecond {
		t.Errorf("auth failures = %+v, want the default rate with burst 25", got.AuthFailures)
	}
	if got.PerCredential.MaxEntries != 5000 || got.AuthFailures.MaxEntries != 5000 {
		t.Errorf("MaxEntries not applied to every limiter: %+v", got)
	}
}

func testHealth() *health.Handler {
	return health.New(map[string]health.Check{"postgres": func(context.Context) error { return nil }})
}

// TestPublicHandlerDoesNotServeMetrics: the REST port is the internet-facing
// ingress; /metrics must answer there with nothing.
func TestPublicHandlerDoesNotServeMetrics(t *testing.T) {
	reg := metrics.NewServiceRegistry("api-gateway", "test")
	h := publicHandler(http.NewServeMux(), testHealth(), metrics.NewHTTPMetrics(reg))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics on the public listener = %d, want 404", rec.Code)
	}
}

// TestOpsListenerServesMetricsAndProbes: the dedicated listener carries
// /metrics and the probes, so neither a scraper nor a kubelet needs the public
// port.
func TestOpsListenerServesMetricsAndProbes(t *testing.T) {
	reg := metrics.NewServiceRegistry("api-gateway", "test")
	metrics.NewGRPCServerMetrics(reg)
	mux := metrics.OpsMux(reg, testHealth().Routes)

	fams := metricstest.Scrape(t, mux, "/metrics")
	metricstest.RequireFamilies(t, fams, "go_goroutines", metrics.BuildInfoName)
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s on the ops listener = %d, want 200", p, rec.Code)
		}
	}
}

// TestPublicHandlerRouteTemplates pins the composition the route label depends
// on: the API mux mounted directly under the outer mux, so the middleware reads
// the API route's PATTERN -- never the raw path with its ids, and never the
// outer "/v1/" catch-all.
func TestPublicHandlerRouteTemplates(t *testing.T) {
	reg := metrics.NewRegistry()
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/namespaces/{ns}/runs/{id}", func(w http.ResponseWriter, _ *http.Request) {})
	api.HandleFunc("POST /v1/namespaces/{ns}/runs/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	h := publicHandler(api, testHealth(), metrics.NewHTTPMetrics(reg))

	for _, id := range []string{"0b7e", "5c1a", "9f00"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/namespaces/team-a/runs/"+id, nil))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/namespaces/team-b/runs/"+id+"/cancel", nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/namespaces/team-a/nothing-here", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	cases := []struct {
		labels map[string]string
		want   float64
	}{
		{map[string]string{"method": "GET", "route": "/v1/namespaces/{ns}/runs/{id}", "code": "200"}, 3},
		{map[string]string{"method": "POST", "route": "/v1/namespaces/{ns}/runs/{id}/cancel", "code": "202"}, 3},
		{map[string]string{"route": metrics.UnmatchedRoute, "code": "404"}, 1},
		{map[string]string{"route": "/readyz"}, 1},
	}
	for _, c := range cases {
		if s := metricstest.Find(fams, metrics.HTTPRequestsTotal, c.labels); s == nil || s.GetCounter().GetValue() != c.want {
			t.Errorf("requests%v = %v, want %v", c.labels, s, c.want)
		}
	}
	for _, s := range fams[metrics.HTTPRequestsTotal].GetMetric() {
		for _, lp := range s.GetLabel() {
			if lp.GetName() == "route" && lp.GetValue() == "/v1/" {
				t.Error("a request was labelled with the outer catch-all \"/v1/\": the inner pattern was lost")
			}
		}
	}
}

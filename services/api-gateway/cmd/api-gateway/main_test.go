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
)

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

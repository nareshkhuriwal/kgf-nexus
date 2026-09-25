// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
)

// TestHTTPMiddlewareLabelsByRouteTemplate is the cardinality guarantee: a
// thousand distinct run ids produce ONE series, labelled by the pattern, and
// an unmatched path never becomes a label.
func TestHTTPMiddlewareLabelsByRouteTemplate(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewHTTPMetrics(reg)

	// The gateway's shape: an API mux mounted under an outer mux.
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/namespaces/{ns}/runs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	api.HandleFunc("POST /v1/namespaces/{ns}/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	outer := http.NewServeMux()
	outer.Handle("/v1/", api)
	outer.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	h := m.Middleware(outer)

	for i := 0; i < 50; i++ {
		serve(h, http.MethodGet, "/v1/namespaces/prod/runs/"+string(rune('a'+i%26))+"-run")
	}
	serve(h, http.MethodPost, "/v1/namespaces/prod/runs")
	serve(h, http.MethodGet, "/v1/does/not/exist/123")
	serve(h, http.MethodGet, "/also-missing/456")
	serve(h, "BREW", "/v1/namespaces/prod/runs/x")
	serve(h, http.MethodGet, "/healthz")

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	metricstest.RequireFamilies(t, fams, metrics.HTTPRequestsTotal, metrics.HTTPRequestDuration,
		metrics.HTTPInFlight, metrics.HTTPResponseSize)

	get := metricstest.Find(fams, metrics.HTTPRequestsTotal,
		map[string]string{"method": "GET", "route": "/v1/namespaces/{ns}/runs/{id}", "code": "200"})
	if get == nil || get.GetCounter().GetValue() != 50 {
		t.Errorf("GET run by template = %v, want 50 on ONE series", get)
	}
	if post := metricstest.Find(fams, metrics.HTTPRequestsTotal,
		map[string]string{"method": "POST", "route": "/v1/namespaces/{ns}/runs", "code": "201"}); post == nil {
		t.Error("POST create run not labelled by its template with status 201")
	}
	if un := metricstest.Find(fams, metrics.HTTPRequestsTotal,
		map[string]string{"route": metrics.UnmatchedRoute, "code": "404"}); un == nil || un.GetCounter().GetValue() != 2 {
		t.Errorf("unmatched paths = %v, want 2 collapsed into route=%q", un, metrics.UnmatchedRoute)
	}
	if other := metricstest.Find(fams, metrics.HTTPRequestsTotal, map[string]string{"method": "OTHER"}); other == nil {
		t.Error("a non-standard method was not bounded to OTHER")
	}
	if hz := metricstest.Find(fams, metrics.HTTPRequestsTotal, map[string]string{"route": "/healthz"}); hz == nil {
		t.Error("outer-mux routes are not labelled")
	}

	// No label value anywhere may contain a concrete id.
	for _, s := range fams[metrics.HTTPRequestsTotal].GetMetric() {
		for _, lp := range s.GetLabel() {
			if lp.GetName() == "route" && (strings.Contains(lp.GetValue(), "prod") || strings.Contains(lp.GetValue(), "-run")) {
				t.Errorf("raw path leaked into the route label: %q", lp.GetValue())
			}
		}
	}
	if n := len(fams[metrics.HTTPRequestsTotal].GetMetric()); n > 6 {
		t.Errorf("%d request series for 55 requests; route templating is not bounding cardinality", n)
	}
	if v := fams[metrics.HTTPInFlight].GetMetric()[0].GetGauge().GetValue(); v != 0 {
		t.Errorf("in-flight = %v after all requests finished, want 0", v)
	}
}

func TestRouteLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":                             metrics.UnmatchedRoute,
		"GET /v1/runs/{id}":            "/v1/runs/{id}",
		"/v1/":                         "/v1/",
		"POST  /v1/x":                  "/v1/x",
		"GET example.com/v1/{ns}/runs": "example.com/v1/{ns}/runs",
	} {
		if got := metrics.RouteLabel(in); got != want {
			t.Errorf("RouteLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHTTPMiddlewarePreservesFlusher: the SSE live tail flushes through the
// wrapper; losing http.Flusher would buffer every event stream.
func TestHTTPMiddlewarePreservesFlusher(t *testing.T) {
	m := metrics.NewHTTPMetrics(metrics.NewRegistry())
	var flushed bool
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped ResponseWriter is not an http.Flusher")
		}
		f.Flush()
		flushed = true
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController cannot reach the underlying writer: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !flushed || !rec.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}
}

func TestGRPCInterceptorsRecordCodes(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewGRPCServerMetrics(reg)
	unary := m.UnaryInterceptor()

	info := &grpc.UnaryServerInfo{FullMethod: "/nexus.v1.WorkerService/PollTask"}
	_, _ = unary(context.Background(), nil, info, func(context.Context, any) (any, error) { return "ok", nil })
	_, _ = unary(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, status.Error(codes.Unauthenticated, "no")
	})
	_, _ = unary(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, errors.New("plain error")
	})
	stream := m.StreamInterceptor()
	_ = stream(nil, nil, &grpc.StreamServerInfo{FullMethod: "/nexus.v1.WorkerService/StreamLogs", IsClientStream: true},
		func(any, grpc.ServerStream) error { return nil })

	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	metricstest.RequireFamilies(t, fams, metrics.GRPCServerStarted, metrics.GRPCServerHandled, metrics.GRPCServerHandling)
	base := map[string]string{"grpc_type": "unary", "grpc_service": "nexus.v1.WorkerService", "grpc_method": "PollTask"}
	for code, want := range map[string]float64{"OK": 1, "Unauthenticated": 1, "Unknown": 1} {
		l := map[string]string{"grpc_code": code}
		for k, v := range base {
			l[k] = v
		}
		if s := metricstest.Find(fams, metrics.GRPCServerHandled, l); s == nil || s.GetCounter().GetValue() != want {
			t.Errorf("handled{code=%s} = %v, want %v", code, s, want)
		}
	}
	if s := metricstest.Find(fams, metrics.GRPCServerStarted, base); s == nil || s.GetCounter().GetValue() != 3 {
		t.Errorf("started = %v, want 3", s)
	}
	if s := metricstest.Find(fams, metrics.GRPCServerHandled,
		map[string]string{"grpc_type": "client_stream", "grpc_method": "StreamLogs"}); s == nil {
		t.Error("stream RPC not recorded with its stream type")
	}
}

func TestOpsServerDisabledIsNoop(t *testing.T) {
	ops, err := metrics.ListenOps("off", http.NotFoundHandler(), nil)
	if err != nil || ops != nil {
		t.Fatalf("ListenOps(off) = %v, %v; want nil, nil", ops, err)
	}
	if err := ops.Serve(); err != nil {
		t.Errorf("Serve on a disabled listener = %v", err)
	}
	if err := ops.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a disabled listener = %v", err)
	}
}

func TestOpsServerServesMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.NewGauge("nexus_ops_test", "help").Gauge().Set(1)
	ops, err := metrics.ListenOps("127.0.0.1:0", metrics.OpsMux(reg), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ops.Serve() }()
	defer func() { _ = ops.Shutdown(context.Background()) }()

	resp, err := http.Get("http://" + ops.Addr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	fams := metricstest.Parse(t, resp.Body)
	metricstest.RequireFamilies(t, fams, "nexus_ops_test")
}

func serve(h http.Handler, method, path string) {
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

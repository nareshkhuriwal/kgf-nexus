// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTP metric family names. Exported so dashboards, alerts and tests reference
// one spelling.
const (
	HTTPRequestsTotal   = "nexus_http_requests_total"
	HTTPRequestDuration = "nexus_http_request_duration_seconds"
	HTTPInFlight        = "nexus_http_requests_in_flight"
	HTTPResponseSize    = "nexus_http_response_size_bytes"
)

// UnmatchedRoute is the route label for a request no pattern matched (a 404 or
// 405 from the mux). Every unmatched path collapses into it; labelling by the
// raw path would let any client mint a new time series per request.
const UnmatchedRoute = "unmatched"

// HTTPBuckets extend DefaultBuckets to a minute: a REST call behind a slow
// upstream, or a large spec upload, legitimately takes tens of seconds, and a
// histogram whose top bucket is 10s reports every one of those as "+Inf".
var HTTPBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// HTTPMetrics is RED instrumentation for an http.Handler, labelled by ROUTE
// TEMPLATE, never by raw path.
//
// The route comes from http.Request.Pattern -- the net/http ServeMux pattern
// that matched ("GET /v1/namespaces/{ns}/runs/{id}") -- read AFTER the handler
// returns. ServeMux sets Pattern on the request it was handed, so an outer
// middleware observes the innermost mux's match as long as nothing between
// them replaced the *Request (r.WithContext copies it). The gateway mounts its
// API mux directly under the outer mux for exactly that reason.
type HTTPMetrics struct {
	requests *CounterVec
	duration *HistogramVec
	size     *HistogramVec
	inflight *Gauge
}

// NewHTTPMetrics registers the HTTP families on reg.
func NewHTTPMetrics(reg *Registry) *HTTPMetrics {
	return &HTTPMetrics{
		requests: reg.NewCounter(HTTPRequestsTotal,
			"HTTP requests completed, by method, route template and status code.",
			"method", "route", "code"),
		duration: reg.NewHistogram(HTTPRequestDuration,
			"HTTP request latency in seconds, by method, route template and status code. Long-lived streams (SSE) are observed when they end.",
			HTTPBuckets, "method", "route", "code"),
		size: reg.NewHistogram(HTTPResponseSize,
			"HTTP response body size in bytes, by method and route template.",
			[]float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216},
			"method", "route"),
		inflight: reg.NewGauge(HTTPInFlight, "HTTP requests currently being served.").Gauge(),
	}
}

// Middleware wraps next with the RED instruments.
func (m *HTTPMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.inflight.Inc()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			m.inflight.Dec()
			method := normalizeMethod(r.Method)
			route := RouteLabel(r.Pattern)
			code := strconv.Itoa(rw.status)
			if p := recover(); p != nil {
				// Counted as the 500 net/http will turn it into, then re-raised
				// so the server's own panic handling is unchanged.
				code = "500"
				m.observe(method, route, code, start, rw.bytes)
				panic(p)
			}
			m.observe(method, route, code, start, rw.bytes)
		}()
		next.ServeHTTP(rw, r)
	})
}

func (m *HTTPMetrics) observe(method, route, code string, start time.Time, bytes int64) {
	l := Labels{"method": method, "route": route, "code": code}
	m.requests.With(l).Inc()
	m.duration.With(l).Observe(time.Since(start).Seconds())
	m.size.With(Labels{"method": method, "route": route}).Observe(float64(bytes))
}

// RouteLabel turns a ServeMux pattern into the route label: the method prefix
// is dropped (it is the method label's job) and an empty pattern is
// UnmatchedRoute.
func RouteLabel(pattern string) string {
	if pattern == "" {
		return UnmatchedRoute
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimLeft(pattern[i+1:], " ")
	}
	return pattern
}

// normalizeMethod bounds the method label. The method is client-chosen, so an
// arbitrary token would otherwise be a cardinality vector.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// statusWriter records the status and body size.
//
// It forwards Flush -- the SSE live tail depends on it -- and exposes Unwrap so
// http.ResponseController reaches the underlying writer for deadlines and
// hijacking.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	w.wroteHeader = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

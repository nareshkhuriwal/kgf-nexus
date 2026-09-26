// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// gRPC server metric family names. They are the grpc-ecosystem
// (go-grpc-middleware/providers/prometheus) spellings, so the community gRPC
// dashboards and alerts work against Nexus without relabelling.
const (
	GRPCServerStarted  = "grpc_server_started_total"
	GRPCServerHandled  = "grpc_server_handled_total"
	GRPCServerHandling = "grpc_server_handling_seconds"
)

// GRPCBuckets reach past a minute because the worker long poll (PollTask) is
// held open server-side for up to its poll wait by design; with 10s as the top
// bucket every poll would be "+Inf" and the histogram would say nothing.
var GRPCBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// GRPCServerMetrics instruments a gRPC server: RPCs started, RPCs handled by
// status code, and handling latency, by service and method.
//
// Cardinality is bounded by construction: grpc-go only invokes interceptors for
// methods registered on the server (an unknown method is answered Unimplemented
// before any interceptor runs), so grpc_service/grpc_method range over the
// server's own descriptors, and grpc_code over the 17 canonical codes.
type GRPCServerMetrics struct {
	started  *CounterVec
	handled  *CounterVec
	handling *HistogramVec
}

// NewGRPCServerMetrics registers the gRPC server families on reg.
func NewGRPCServerMetrics(reg *Registry) *GRPCServerMetrics {
	return &GRPCServerMetrics{
		started: reg.NewCounter(GRPCServerStarted,
			"RPCs started on the server.", "grpc_type", "grpc_service", "grpc_method"),
		handled: reg.NewCounter(GRPCServerHandled,
			"RPCs completed on the server, regardless of success or failure, by status code.",
			"grpc_type", "grpc_service", "grpc_method", "grpc_code"),
		handling: reg.NewHistogram(GRPCServerHandling,
			"Latency (seconds) of RPCs handled by the server. No code label, matching the grpc-ecosystem histogram; errors are in "+GRPCServerHandled+".",
			GRPCBuckets, "grpc_type", "grpc_service", "grpc_method"),
	}
}

// ServerOptions returns the interceptor options to pass to grpc.NewServer. They
// are Chain* options, so they compose with any interceptors the server already
// has; put them FIRST so an RPC rejected by authentication is still counted.
func (m *GRPCServerMetrics) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		// The OpenTelemetry stats handler rides with the metrics interceptors
		// because every Nexus gRPC server installs these: one call site, and
		// no server can be instrumented for one signal and not the other.
		// It continues the caller's trace from the request metadata
		// (ADR-0026); with tracing off it only propagates.
		tracing.GRPCServerOption(),
		grpc.ChainUnaryInterceptor(m.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(m.StreamInterceptor()),
	}
}

// UnaryInterceptor records one unary RPC.
func (m *GRPCServerMetrics) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		svc, method := splitMethod(info.FullMethod)
		start := time.Now()
		m.started.With(Labels{"grpc_type": "unary", "grpc_service": svc, "grpc_method": method}).Inc()
		resp, err := handler(ctx, req)
		m.done("unary", svc, method, err, start)
		return resp, err
	}
}

// StreamInterceptor records one streaming RPC.
func (m *GRPCServerMetrics) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		svc, method := splitMethod(info.FullMethod)
		typ := streamType(info)
		start := time.Now()
		m.started.With(Labels{"grpc_type": typ, "grpc_service": svc, "grpc_method": method}).Inc()
		err := handler(srv, ss)
		m.done(typ, svc, method, err, start)
		return err
	}
}

func (m *GRPCServerMetrics) done(typ, svc, method string, err error, start time.Time) {
	// status.Code maps a non-status error to Unknown and nil to OK, which is
	// what the client will see.
	code := status.Code(err).String()
	l := Labels{"grpc_type": typ, "grpc_service": svc, "grpc_method": method, "grpc_code": code}
	m.handled.With(l).Inc()
	m.handling.With(Labels{"grpc_type": typ, "grpc_service": svc, "grpc_method": method}).
		Observe(time.Since(start).Seconds())
}

// splitMethod splits "/pkg.Service/Method" into its service and method.
func splitMethod(full string) (service, method string) {
	full = strings.TrimPrefix(full, "/")
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "unknown", full
}

func streamType(info *grpc.StreamServerInfo) string {
	switch {
	case info.IsClientStream && info.IsServerStream:
		return "bidi_stream"
	case info.IsClientStream:
		return "client_stream"
	case info.IsServerStream:
		return "server_stream"
	}
	return "unary"
}

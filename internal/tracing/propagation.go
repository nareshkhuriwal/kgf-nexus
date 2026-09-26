// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/filters"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// Carrier keys. The W3C header names, lower-case, as both the HTTP and the
// map carriers spell them.
const (
	KeyTraceparent = "traceparent"
	KeyTracestate  = "tracestate"
)

// Inject returns ctx's trace context as a W3C carrier map (traceparent and,
// when present, tracestate), or nil when ctx carries no valid span context.
// It is what TaskAssignment.trace_context carries to the agent.
func Inject(ctx context.Context) map[string]string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return nil
	}
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	if len(c) == 0 {
		return nil
	}
	return c
}

// Extract returns ctx with the remote span context in carrier as its parent.
// A nil, empty or malformed carrier returns ctx unchanged: a bad traceparent
// from a peer must never fail the work it rides on.
func Extract(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier(carrier))
}

// Traceparent renders ctx's span context as a W3C traceparent header value,
// or "" when there is none. The one-string form is what the database stores
// (outbox.trace_context, task_attempts.trace_context): tracestate and baggage
// are deliberately NOT persisted across the asynchronous hops, because they
// are request-scoped vendor data whose meaning does not survive a queue.
func Traceparent(ctx context.Context) string {
	return Inject(ctx)[KeyTraceparent]
}

// FromTraceparent returns ctx with tp (a W3C traceparent value) as the remote
// parent; an empty or malformed tp returns ctx unchanged.
func FromTraceparent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	return Extract(ctx, map[string]string{KeyTraceparent: tp})
}

// SpanContextFromTraceparent parses tp; the result is invalid when tp is.
func SpanContextFromTraceparent(tp string) trace.SpanContext {
	return trace.SpanContextFromContext(FromTraceparent(context.Background(), tp))
}

// NullableTraceparent is Traceparent as a SQL parameter: nil (NULL) when the
// context carries no trace, so untraced writes store NULL rather than "".
func NullableTraceparent(ctx context.Context) *string {
	if tp := Traceparent(ctx); tp != "" {
		return &tp
	}
	return nil
}

// ── task processes ───────────────────────────────────────────────────────────

// TaskEnv renders ctx's trace context as the environment of a task process,
// following the OpenTelemetry convention of upper-case TRACEPARENT and
// TRACESTATE variables. The Python SDK picks these up; any OTel-instrumented
// program that honours the convention joins the attempt's trace.
func TaskEnv(ctx context.Context) map[string]string {
	c := Inject(ctx)
	if c == nil {
		return nil
	}
	env := map[string]string{"TRACEPARENT": c[KeyTraceparent]}
	if ts := c[KeyTracestate]; ts != "" {
		env["TRACESTATE"] = ts
	}
	return env
}

// ── NATS ─────────────────────────────────────────────────────────────────────

// natsCarrier adapts nats.Header, which is a map[string][]string exactly like
// http.Header but without its canonicalisation.
type natsCarrier nats.Header

func (c natsCarrier) Get(k string) string {
	if v := c[k]; len(v) > 0 {
		return v[0]
	}
	// Tolerate a canonicalised spelling from another producer.
	for key, v := range c {
		if strings.EqualFold(key, k) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
func (c natsCarrier) Set(k, v string) { c[k] = []string{v} }
func (c natsCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// InjectNATS writes ctx's trace context into a message's headers, creating
// the header map when needed.
func InjectNATS(ctx context.Context, msg *nats.Msg) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return
	}
	if msg.Header == nil {
		msg.Header = nats.Header{}
	}
	Propagator.Inject(ctx, natsCarrier(msg.Header))
}

// ExtractNATS returns ctx with a message's trace context as parent.
func ExtractNATS(ctx context.Context, h nats.Header) context.Context {
	if len(h) == 0 {
		return ctx
	}
	return Propagator.Extract(ctx, natsCarrier(h))
}

// ── gRPC ─────────────────────────────────────────────────────────────────────

// grpcFilter keeps health checks and reflection out of traces: a probe every
// few seconds per pod would be most of the spans and none of the insight.
var grpcFilter = filters.None(filters.ServiceName("grpc.health.v1.Health"),
	filters.ServicePrefix("grpc.reflection."))

// GRPCServerOption traces every RPC a server handles, continuing the caller's
// trace from the request metadata. Installed on every Nexus gRPC server,
// including the gateway's worker listener, through metrics.ServerOptions.
func GRPCServerOption() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(
		otelgrpc.WithFilter(grpcFilter),
		otelgrpc.WithPropagators(Propagator),
	))
}

// GRPCDialOption traces every RPC a client makes and injects the traceparent
// into its metadata. Installed through config.PeerDialOptions (every internal
// hop) and the agent's control-plane dial.
func GRPCDialOption() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler(
		otelgrpc.WithFilter(grpcFilter),
		otelgrpc.WithPropagators(Propagator),
	))
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

// EnvRESTPublicEndpoint names the switch that makes the REST listener treat
// an inbound traceparent as a LINK rather than a parent. The gateway is the
// internet-facing ingress: a client that sends sampled traceparents forces
// the parent-based sampler to keep every one of its requests. Operators who
// do not trust their callers set this to true; the default honours callers,
// which is what lets an SDK-submitted run join the submitter's trace.
const EnvRESTPublicEndpoint = "NEXUS_TRACING_PUBLIC_ENDPOINT"

// HTTPMiddleware opens a server span for every request, continuing the
// caller's trace (or linking to it; see EnvRESTPublicEndpoint).
//
// It is hand-written rather than otelhttp because the span name must be the
// ROUTE TEMPLATE ("POST /v1/namespaces/{ns}/runs"), which net/http's ServeMux
// records on the *Request it routes. otelhttp names the span before routing,
// and it hands the mux a copy of the request, so the pattern would be lost.
// Here the copy we pass down IS the one the mux annotates, and we read its
// Pattern after the handler returns. Anything between this middleware and
// the mux must pass the request through unchanged (the metrics middleware
// does; TestHTTPMiddlewareRouteTemplate pins it).
func HTTPMiddleware(next http.Handler) http.Handler {
	public := strings.EqualFold(os.Getenv(EnvRESTPublicEndpoint), "true")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLScheme(scheme(r)),
				semconv.URLPath(r.URL.Path),
			),
		}
		parent := remote
		if public {
			if sc := trace.SpanContextFromContext(remote); sc.IsValid() {
				opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}), trace.WithNewRoot())
			}
			// Baggage from an untrusted caller is not propagated either.
			parent = r.Context()
		}
		ctx, span := Tracer().Start(parent, "HTTP "+r.Method, opts...)
		defer span.End()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		r2 := r.WithContext(ctx)
		next.ServeHTTP(sw, r2)
		route := r2.Pattern
		if i := strings.IndexByte(route, ' '); i >= 0 {
			route = strings.TrimLeft(route[i+1:], " ")
		}
		if route != "" {
			span.SetName(r.Method + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(sw.status))
		if sw.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.status))
		}
	})
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush keeps SSE streaming working through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ── attribute keys ───────────────────────────────────────────────────────────

// Span attribute keys for Nexus identifiers. IDs are fine as SPAN attributes
// -- a span is one event -- but never as metric labels (ADR-0019).
var (
	AttrNamespace     = attribute.Key("nexus.namespace")
	AttrRunID         = attribute.Key("nexus.run_id")
	AttrNodeID        = attribute.Key("nexus.node_id")
	AttrAttemptID     = attribute.Key("nexus.attempt_id")
	AttrAttemptNumber = attribute.Key("nexus.attempt_number")
	AttrWorkerID      = attribute.Key("nexus.worker_id")
	AttrQueue         = attribute.Key("nexus.queue")
	AttrExecutor      = attribute.Key("nexus.executor")
	AttrOutcome       = attribute.Key("nexus.outcome")
	AttrCount         = attribute.Key("nexus.count")
)

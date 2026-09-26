// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

// inMemory installs a synchronous in-memory tracer provider for one test and
// restores the previous global provider afterwards.
func inMemory(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := Setup(context.Background(), Options{Service: "test", Exporter: exp, Syncer: true,
		Sampler: sdktrace.AlwaysSample(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exp
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestEnabledIsOptIn(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://c:4318/v1/traces"}, true},
		{map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, true},
		{map[string]string{"OTEL_TRACES_EXPORTER": "console"}, true},
		{map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, false},
		{map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_TRACES_EXPORTER": "otlp"}, false},
	}
	for _, c := range cases {
		if got := Enabled(env(c.env)); got != c.want {
			t.Errorf("Enabled(%v) = %v; want %v", c.env, got, c.want)
		}
	}
}

func TestSetupDisabledInstallsOnlyThePropagator(t *testing.T) {
	prev := otel.GetTracerProvider()
	shutdown, err := Setup(context.Background(), Options{Service: "x", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != prev {
		t.Fatal("a disabled Setup replaced the tracer provider")
	}
	if otel.GetTextMapPropagator() == nil {
		t.Fatal("no propagator installed")
	}
}

func TestSetupRejectsBadConfiguration(t *testing.T) {
	for _, e := range []map[string]string{
		{"OTEL_TRACES_EXPORTER": "zipkin"},
		{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"},
		{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_TRACES_SAMPLER": "sometimes"},
		{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_TRACES_SAMPLER_ARG": "2"},
	} {
		if _, err := Setup(context.Background(), Options{Service: "x", Getenv: env(e)}); err == nil {
			t.Errorf("Setup(%v) accepted", e)
		}
	}
}

func TestDefaultSamplerIsParentBasedRatio(t *testing.T) {
	s, err := SamplerFromEnv(env(map[string]string{"OTEL_TRACES_SAMPLER_ARG": "0.25"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Description(); !strings.Contains(d, "ParentBased") || !strings.Contains(d, "0.25") {
		t.Fatalf("sampler = %s", d)
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	inMemory(t)
	ctx, span := Start(context.Background(), "root")
	defer span.End()
	tp := Traceparent(ctx)
	if !strings.HasPrefix(tp, "00-"+span.SpanContext().TraceID().String()+"-"+span.SpanContext().SpanID().String()) {
		t.Fatalf("traceparent %q does not name the span", tp)
	}
	sc := SpanContextFromTraceparent(tp)
	if sc.TraceID() != span.SpanContext().TraceID() || !sc.IsRemote() {
		t.Fatalf("parsed %v", sc)
	}
	if Traceparent(context.Background()) != "" || NullableTraceparent(context.Background()) != nil {
		t.Fatal("an untraced context produced a traceparent")
	}
	if SpanContextFromTraceparent("garbage").IsValid() || FromTraceparent(context.Background(), "00-zz") == nil {
		t.Fatal("malformed traceparent handling")
	}
	env := TaskEnv(ctx)
	if env["TRACEPARENT"] != tp {
		t.Fatalf("TaskEnv = %v", env)
	}
}

func TestNATSCarrier(t *testing.T) {
	inMemory(t)
	ctx, span := Start(context.Background(), "publish")
	defer span.End()
	msg := nats.NewMsg("subj")
	InjectNATS(ctx, msg)
	got := trace.SpanContextFromContext(ExtractNATS(context.Background(), msg.Header))
	if got.TraceID() != span.SpanContext().TraceID() || got.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("extracted %v", got)
	}
	empty := nats.NewMsg("subj")
	InjectNATS(context.Background(), empty)
	if len(empty.Header) != 0 {
		t.Fatal("untraced publish added headers")
	}
}

// TestHTTPMiddlewareRouteTemplate pins the span naming contract: the span is
// named after the ServeMux pattern, it continues an inbound traceparent, and
// the metrics-style pass-through wrapper in between does not hide the pattern.
func TestHTTPMiddlewareRouteTemplate(t *testing.T) {
	exp := inMemory(t)
	mux := http.NewServeMux()
	var inner trace.SpanContext
	mux.HandleFunc("POST /v1/namespaces/{ns}/runs", func(w http.ResponseWriter, r *http.Request) {
		inner = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(http.StatusCreated)
	})
	passThrough := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) })
	h := HTTPMiddleware(passThrough)

	parentCtx, parent := Start(context.Background(), "client")
	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/team-a/runs", nil)
	otel.GetTextMapPropagator().Inject(parentCtx, propagationHeader(req.Header))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	parent.End()

	var server *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			server = &exp.GetSpans()[i]
		}
	}
	if server == nil {
		t.Fatal("no server span")
	}
	if server.Name != "POST /v1/namespaces/{ns}/runs" {
		t.Fatalf("span name %q; want the route template", server.Name)
	}
	if server.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("server span did not continue the inbound traceparent")
	}
	if inner.SpanID() != server.SpanContext.SpanID() {
		t.Fatal("the handler did not see the server span in its context")
	}
}

func TestHTTPMiddlewarePublicEndpointLinks(t *testing.T) {
	t.Setenv(EnvRESTPublicEndpoint, "true")
	exp := inMemory(t)
	h := HTTPMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	parentCtx, parent := Start(context.Background(), "untrusted")
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	otel.GetTextMapPropagator().Inject(parentCtx, propagationHeader(req.Header))
	h.ServeHTTP(httptest.NewRecorder(), req)
	parent.End()
	for _, s := range exp.GetSpans() {
		if s.SpanKind != trace.SpanKindServer {
			continue
		}
		if s.SpanContext.TraceID() == parent.SpanContext().TraceID() {
			t.Fatal("public endpoint joined the caller's trace")
		}
		if len(s.Links) != 1 || s.Links[0].SpanContext.SpanID() != parent.SpanContext().SpanID() {
			t.Fatalf("links = %v", s.Links)
		}
		return
	}
	t.Fatal("no server span")
}

// TestGRPCPropagation: a client call made inside a span arrives at the server
// as a child span in the same trace, and health checks are filtered out.
func TestGRPCPropagation(t *testing.T) {
	exp := inMemory(t)
	call := func() trace.Span {
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer(GRPCServerOption())
		healthpb.RegisterHealthServer(srv, health.NewServer())
		go func() { _ = srv.Serve(lis) }()
		defer srv.Stop()
		conn, err := grpc.NewClient("passthrough:///buf",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()), GRPCDialOption())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, span := Start(context.Background(), "caller")
		if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatal(err)
		}
		span.End()
		return span
	}

	call()
	for _, s := range exp.GetSpans() {
		if strings.Contains(s.Name, "Health") {
			t.Fatalf("health check traced: %s", s.Name)
		}
	}

	// Lift the filter to observe propagation on a service we can run here.
	saved := grpcFilter
	grpcFilter = nil
	defer func() { grpcFilter = saved }()
	exp.Reset()
	caller := call()
	var client, server *tracetest.SpanStub
	spans := exp.GetSpans()
	for i := range spans {
		switch spans[i].SpanKind {
		case trace.SpanKindClient:
			client = &spans[i]
		case trace.SpanKindServer:
			server = &spans[i]
		}
	}
	if client == nil || server == nil {
		t.Fatalf("spans = %v", spans.Snapshots())
	}
	if client.Parent.SpanID() != caller.SpanContext().SpanID() {
		t.Fatal("client span is not a child of the caller")
	}
	if server.Parent.SpanID() != client.SpanContext.SpanID() || server.SpanContext.TraceID() != caller.SpanContext().TraceID() {
		t.Fatal("server span did not continue the client's trace over gRPC metadata")
	}
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	inMemory(t)
	var buf bytes.Buffer
	log := slog.New(LogHandler(slog.NewJSONHandler(&buf, nil))).With("service", "x")
	ctx, span := Start(context.Background(), "op")
	log.InfoContext(ctx, "hello")
	span.End()
	log.Info("no context")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first, second map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first["trace_id"] != span.SpanContext().TraceID().String() || first["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("record = %v", first)
	}
	if _, ok := second["trace_id"]; ok {
		t.Fatal("a record without a context gained a trace_id")
	}
	if l := WithTrace(ctx, log); l == log {
		t.Fatal("WithTrace did not bind the ids")
	}
}

func propagationHeader(h http.Header) propagation.HeaderCarrier { return propagation.HeaderCarrier(h) }

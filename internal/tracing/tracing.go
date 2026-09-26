// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package tracing is Nexus's OpenTelemetry setup and propagation plumbing
// (ADR-0026). Every service and the agent call Setup once at startup; every
// hop -- HTTP, gRPC, NATS, the transactional outbox, the attempt row, the task
// process -- carries a W3C trace context through the helpers here, so one run
// can be followed from the API request that created it to the task that ran
// and the result that ended it.
//
// OFF BY DEFAULT, and configured only through the standard OTEL_* variables so
// an operator's existing collector configuration applies unchanged:
//
//	OTEL_TRACES_EXPORTER          otlp | console | none
//	OTEL_EXPORTER_OTLP_ENDPOINT   (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
//	OTEL_EXPORTER_OTLP_PROTOCOL   grpc | http/protobuf (default)
//	OTEL_EXPORTER_OTLP_HEADERS, _INSECURE, _CERTIFICATE, _TIMEOUT, _COMPRESSION
//	OTEL_TRACES_SAMPLER           default parentbased_traceidratio
//	OTEL_TRACES_SAMPLER_ARG       ratio, default 1.0
//	OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES, OTEL_BSP_*
//	OTEL_SDK_DISABLED=true        forces tracing off
//
// Tracing is ON when OTEL_TRACES_EXPORTER names an exporter or an OTLP
// endpoint variable is set. The OpenTelemetry specification's default
// exporter is "otlp", which would make every binary without configuration
// try to reach localhost:4318 and log export failures forever; Nexus requires
// an explicit opt-in instead.
//
// PROPAGATION IS ALWAYS ON, even with tracing off. The W3C propagator is
// installed unconditionally, so a service that does not export spans still
// forwards the caller's traceparent to the next hop: a partially traced
// deployment (tracing on in the gateway and the agent only, say) still
// produces connected traces rather than fragments.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope of every span Nexus code creates.
const ScopeName = "github.com/khuriwalgroup/kgf-nexus"

// Options configure Setup.
type Options struct {
	// Service is the default service.name (OTEL_SERVICE_NAME overrides it).
	Service string
	// Version becomes service.version.
	Version string
	// Getenv reads the environment; os.Getenv when nil. Injectable for tests.
	Getenv func(string) string
	// Exporter, when set, replaces the configured exporter and forces tracing
	// on. Tests pass an in-memory exporter (tracetest.NewInMemoryExporter).
	Exporter sdktrace.SpanExporter
	// Sampler, when set, replaces the configured sampler (tests).
	Sampler sdktrace.Sampler
	// Syncer exports spans synchronously instead of batching (tests).
	Syncer bool
	// Log receives exporter errors; slog.Default when nil.
	Log *slog.Logger
}

// Propagator is the W3C trace-context + baggage propagator every hop uses.
var Propagator propagation.TextMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{})

var propagatorOnce sync.Once

// InstallPropagator sets the global propagator. Setup calls it; tests and
// tools that never call Setup may call it directly. Idempotent.
func InstallPropagator() {
	propagatorOnce.Do(func() { otel.SetTextMapPropagator(Propagator) })
}

// Enabled reports whether the environment turns tracing on.
func Enabled(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_EXPORTER"))) {
	case "none":
		return false
	case "":
	default:
		// otlp, console -- or a name Setup will reject loudly, not ignore.
		return true
	}
	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Shutdown flushes and stops the tracer provider.
type Shutdown func(context.Context) error

// Setup installs the propagator and, when tracing is enabled, a tracer
// provider exporting through the configured exporter. The returned Shutdown
// flushes buffered spans; call it last during process shutdown, with a
// bounded context. With tracing disabled Setup installs nothing else and the
// global provider stays the no-op one, which costs a context lookup per span.
func Setup(ctx context.Context, o Options) (Shutdown, error) {
	InstallPropagator()
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	noop := func(context.Context) error { return nil }
	if o.Exporter == nil && !Enabled(getenv) {
		return noop, nil
	}

	exp := o.Exporter
	if exp == nil {
		var err error
		exp, err = newExporter(ctx, getenv)
		if err != nil {
			return noop, err
		}
	}
	sampler := o.Sampler
	if sampler == nil {
		var err error
		sampler, err = SamplerFromEnv(getenv)
		if err != nil {
			return noop, err
		}
	}
	res, err := newResource(ctx, o.Service, o.Version)
	if err != nil {
		return noop, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithSampler(sampler), sdktrace.WithResource(res)}
	if o.Syncer {
		opts = append(opts, sdktrace.WithSyncer(exp))
	} else {
		// Batch settings come from OTEL_BSP_* (read by the SDK). The queue is
		// bounded and drops when full: a collector outage costs spans, never
		// memory or latency on the traced path.
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(rateLimitedErrorHandler(log))
	log.Info("OpenTelemetry tracing enabled",
		"exporter", exporterName(getenv), "sampler", samplerDescription(getenv),
		"endpoint", firstNonEmpty(getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), getenv("OTEL_EXPORTER_OTLP_ENDPOINT")))
	return tp.Shutdown, nil
}

func exporterName(getenv func(string) string) string {
	if v := strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_EXPORTER"))); v != "" {
		return v
	}
	return "otlp"
}

func newExporter(ctx context.Context, getenv func(string) string) (sdktrace.SpanExporter, error) {
	switch name := exporterName(getenv); name {
	case "console":
		return stdouttrace.New(stdouttrace.WithWriter(os.Stderr))
	case "otlp":
		// Both exporters read OTEL_EXPORTER_OTLP_{,TRACES_}{ENDPOINT,HEADERS,
		// INSECURE,CERTIFICATE,TIMEOUT,COMPRESSION} themselves, so every
		// standard knob works without a Nexus-specific spelling.
		proto := strings.ToLower(firstNonEmpty(getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"),
			getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
		switch proto {
		case "grpc":
			return otlptracegrpc.New(ctx)
		case "", "http/protobuf":
			return otlptracehttp.New(ctx)
		case "http/json":
			return nil, errors.New("tracing: OTEL_EXPORTER_OTLP_PROTOCOL=http/json is not supported; use grpc or http/protobuf")
		default:
			return nil, fmt.Errorf("tracing: unknown OTEL_EXPORTER_OTLP_PROTOCOL %q", proto)
		}
	default:
		return nil, fmt.Errorf("tracing: unsupported OTEL_TRACES_EXPORTER %q (supported: otlp, console, none)", name)
	}
}

// SamplerFromEnv builds the sampler, defaulting to parent-based ratio.
//
// Parent-based is what keeps a trace whole: the service that starts a trace
// (the gateway, or a scheduler loop) makes the ratio decision once, and every
// downstream service follows the sampled flag in the traceparent instead of
// rolling its own dice and leaving holes.
func SamplerFromEnv(getenv func(string) string) (sdktrace.Sampler, error) {
	ratio := 1.0
	if v := strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER_ARG")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return nil, fmt.Errorf("tracing: OTEL_TRACES_SAMPLER_ARG=%q must be a ratio in [0, 1]", v)
		}
		ratio = f
	}
	switch name := strings.ToLower(strings.TrimSpace(getenv("OTEL_TRACES_SAMPLER"))); name {
	case "", "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio), nil
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	default:
		return nil, fmt.Errorf("tracing: unsupported OTEL_TRACES_SAMPLER %q", name)
	}
}

func samplerDescription(getenv func(string) string) string {
	s := firstNonEmpty(getenv("OTEL_TRACES_SAMPLER"), "parentbased_traceidratio")
	return s + "(" + firstNonEmpty(getenv("OTEL_TRACES_SAMPLER_ARG"), "1.0") + ")"
}

// newResource merges, in increasing precedence: the SDK and host identity,
// the service's own name and version, and OTEL_SERVICE_NAME /
// OTEL_RESOURCE_ATTRIBUTES from the environment.
func newResource(ctx context.Context, service, version string) (*resource.Resource, error) {
	kv := []resource.Option{
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceNamespace("kgf-nexus")),
	}
	if version != "" {
		kv = append(kv, resource.WithAttributes(semconv.ServiceVersion(version)))
	}
	kv = append(kv, resource.WithFromEnv())
	r, err := resource.New(ctx, kv...)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}
	return r, nil
}

// rateLimitedErrorHandler logs exporter errors at most once a minute: a
// collector outage would otherwise print one error per batch, per service,
// for as long as it lasts.
func rateLimitedErrorHandler(log *slog.Logger) otel.ErrorHandler {
	var mu sync.Mutex
	var last time.Time
	var suppressed int
	return otel.ErrorHandlerFunc(func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if !last.IsZero() && time.Since(last) < time.Minute {
			suppressed++
			return
		}
		log.Warn("OpenTelemetry error (spans may be dropped; tracing never blocks the traced work)",
			"error", err, "suppressed_since_last", suppressed)
		last, suppressed = time.Now(), 0
	})
}

// Tracer is the tracer every Nexus span comes from.
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Start starts a span from the Nexus tracer. It is the helper call sites
// use, so span creation reads the same everywhere.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// End records err on span (when non-nil) and ends it.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// SetupOrExit is the one line every service main uses: Setup, exiting with a
// logged error when the OTEL_* configuration is invalid (a typo in an
// exporter or sampler must not silently turn tracing off), and returning the
// flush to defer. The flush is bounded at five seconds so an unreachable
// collector cannot hold up a pod's termination.
func SetupOrExit(ctx context.Context, service, version string, log *slog.Logger) func() {
	shutdown, err := Setup(ctx, Options{Service: service, Version: version, Log: log})
	if err != nil {
		log.Error("invalid OpenTelemetry configuration", "error", err)
		os.Exit(1)
	}
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(sctx); err != nil {
			log.Warn("could not flush traces at shutdown", "error", err)
		}
	}
}

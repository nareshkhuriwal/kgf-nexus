// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"
)

func memoryTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := tracing.Setup(context.Background(), tracing.Options{Service: "scheduler-test",
		Exporter: exp, Syncer: true, Sampler: sdktrace.AlwaysSample(), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()); otel.SetTracerProvider(prev) })
	return exp
}

// TestDispatchSpanJoinsTheRunTrace: the dispatch span is a child of the trace
// stored on the attempt row (the run's), linked to the poll that claimed it,
// and the context handed to the agent names the dispatch span.
func TestDispatchSpanJoinsTheRunTrace(t *testing.T) {
	exp := memoryTracer(t)

	runCtx, runSpan := tracing.Start(context.Background(), "run.materialize")
	stored := tracing.Traceparent(runCtx)
	runSpan.End()

	pollCtx, poll := tracing.Start(context.Background(), "scheduler.claim")
	a := &store.WorkerAssignment{AttemptID: uuid.New(), RunID: uuid.New(), NodeID: "n", AttemptNumber: 1,
		TraceContext: stored}
	carrier := dispatchSpan(pollCtx, time.Now(), a)
	poll.End()

	var dispatch *tracetest.SpanStub
	spans := exp.GetSpans()
	for i := range spans {
		if spans[i].Name == "attempt.dispatch" {
			dispatch = &spans[i]
		}
	}
	if dispatch == nil {
		t.Fatal("no dispatch span")
	}
	if dispatch.SpanContext.TraceID() != runSpan.SpanContext().TraceID() ||
		dispatch.Parent.SpanID() != runSpan.SpanContext().SpanID() {
		t.Fatal("dispatch span is not a child of the attempt's stored trace")
	}
	if len(dispatch.Links) != 1 || dispatch.Links[0].SpanContext.SpanID() != poll.SpanContext().SpanID() {
		t.Fatalf("dispatch span links = %v; want the poll", dispatch.Links)
	}
	got := tracing.SpanContextFromTraceparent(carrier["traceparent"])
	if got.SpanID() != dispatch.SpanContext.SpanID() {
		t.Fatal("the agent is not handed the dispatch span")
	}

	// No stored trace: the dispatch hangs off the poll instead.
	exp.Reset()
	pollCtx, poll = tracing.Start(context.Background(), "scheduler.claim")
	carrier = dispatchSpan(pollCtx, time.Now(), &store.WorkerAssignment{AttemptID: uuid.New(), RunID: uuid.New()})
	poll.End()
	if sc := tracing.SpanContextFromTraceparent(carrier["traceparent"]); sc.TraceID() != poll.SpanContext().TraceID() {
		t.Fatal("an untraced attempt was not dispatched under the poll")
	}
	_ = trace.SpanKindProducer
}

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"
)

// Metrics are the result-store client instruments shared by every user of the
// interface. Labels are the backend, the operation and a closed outcome set --
// never a key, bucket or namespace -- so cardinality is fixed (ADR-0019).
type Metrics struct {
	ops      *metrics.CounterVec
	duration *metrics.HistogramVec
	bytes    *metrics.CounterVec
}

// NewMetrics registers the result-store client metrics on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		ops: reg.NewCounter("nexus_objstore_operations_total",
			"Result-store operations by backend, operation and outcome (ok, not_found, exists, error).",
			"backend", "op", "outcome"),
		duration: reg.NewHistogram("nexus_objstore_operation_duration_seconds",
			"Result-store operation latency; for get, the time to open the object, not to drain it.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
			"backend", "op"),
		bytes: reg.NewCounter("nexus_objstore_bytes_total",
			"Payload bytes written (direction=out) and read (direction=in) through the result store.",
			"backend", "direction"),
	}
}

// Instrument wraps s with a span per operation and, when m is non-nil,
// operation metrics. Spans carry the key (a digest or a log path: per-event
// data, fine on a span) but never object bytes.
func Instrument(s Store, m *Metrics) Store {
	if s == nil {
		return nil
	}
	if _, ok := s.(*instrumented); ok {
		return s
	}
	return &instrumented{s: s, m: m, backend: s.Backend(), bucket: s.Bucket()}
}

type instrumented struct {
	s       Store
	m       *Metrics
	backend string
	bucket  string
}

func (i *instrumented) Backend() string { return i.backend }
func (i *instrumented) Bucket() string  { return i.bucket }

func (i *instrumented) start(ctx context.Context, op, key string, extra ...attribute.KeyValue) (context.Context, trace.Span, time.Time) {
	attrs := append([]attribute.KeyValue{
		attribute.String("nexus.objstore.backend", i.backend),
		attribute.String("nexus.objstore.bucket", i.bucket),
	}, extra...)
	if key != "" {
		attrs = append(attrs, attribute.String("nexus.objstore.key", key))
	}
	ctx, span := tracing.Start(ctx, "objstore."+op,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return ctx, span, time.Now()
}

func (i *instrumented) finish(span trace.Span, op string, start time.Time, err error) {
	outcome := "ok"
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		outcome = "not_found"
	case errors.Is(err, ErrExists):
		outcome = "exists"
	default:
		outcome = "error"
	}
	span.SetAttributes(attribute.String("nexus.objstore.outcome", outcome))
	if outcome == "error" {
		tracing.End(span, err)
	} else {
		span.End()
	}
	if i.m != nil {
		i.m.ops.With(metrics.Labels{"backend": i.backend, "op": op, "outcome": outcome}).Inc()
		i.m.duration.With(metrics.Labels{"backend": i.backend, "op": op}).Observe(time.Since(start).Seconds())
	}
}

func (i *instrumented) addBytes(direction string, n int64) {
	if i.m != nil && n > 0 {
		i.m.bytes.With(metrics.Labels{"backend": i.backend, "direction": direction}).Add(float64(n))
	}
}

func (i *instrumented) Put(ctx context.Context, key string, body io.Reader, size int64, opts PutOptions) error {
	ctx, span, start := i.start(ctx, "put", key, attribute.Int64("nexus.objstore.size_bytes", size),
		attribute.Bool("nexus.objstore.if_absent", opts.IfAbsent))
	cr := &countingReader{r: body}
	err := i.s.Put(ctx, key, cr, size, opts)
	if err == nil {
		i.addBytes("out", cr.n)
	}
	i.finish(span, "put", start, err)
	return err
}

func (i *instrumented) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, span, start := i.start(ctx, "stat", key)
	info, err := i.s.Stat(ctx, key)
	i.finish(span, "stat", start, err)
	return info, err
}

func (i *instrumented) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	ctx, span, start := i.start(ctx, "get", key,
		attribute.Int64("nexus.objstore.offset", offset), attribute.Int64("nexus.objstore.length", length))
	rc, err := i.s.Get(ctx, key, offset, length)
	i.finish(span, "get", start, err)
	if err != nil {
		return nil, err
	}
	return &countingReadCloser{ReadCloser: rc, done: func(n int64) { i.addBytes("in", n) }}, nil
}

func (i *instrumented) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	ctx, span, start := i.start(ctx, "list", "", attribute.String("nexus.objstore.prefix", prefix))
	n := 0
	err := i.s.List(ctx, prefix, func(o ObjectInfo) error { n++; return fn(o) })
	span.SetAttributes(attribute.Int("nexus.objstore.listed", n))
	i.finish(span, "list", start, err)
	return err
}

// ListAfter keeps the wrapped backend's server-side start key: without it the
// decorator would hide AfterLister and every resumed listing would walk the
// prefix from the beginning.
func (i *instrumented) ListAfter(ctx context.Context, prefix, startAfter string, fn func(ObjectInfo) error) error {
	ctx, span, start := i.start(ctx, "list", "", attribute.String("nexus.objstore.prefix", prefix),
		attribute.String("nexus.objstore.start_after", startAfter))
	n := 0
	err := ListAfter(ctx, i.s, prefix, startAfter, func(o ObjectInfo) error { n++; return fn(o) })
	span.SetAttributes(attribute.Int("nexus.objstore.listed", n))
	i.finish(span, "list", start, err)
	return err
}

func (i *instrumented) Delete(ctx context.Context, key string) error {
	ctx, span, start := i.start(ctx, "delete", key)
	err := i.s.Delete(ctx, key)
	i.finish(span, "delete", start, err)
	return err
}

func (i *instrumented) BucketExists(ctx context.Context) (bool, error) {
	ctx, span, start := i.start(ctx, "bucket_exists", "")
	ok, err := i.s.BucketExists(ctx)
	i.finish(span, "bucket_exists", start, err)
	return ok, err
}

func (i *instrumented) CreateBucket(ctx context.Context) error {
	ctx, span, start := i.start(ctx, "create_bucket", "")
	err := i.s.CreateBucket(ctx)
	i.finish(span, "create_bucket", start, err)
	return err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type countingReadCloser struct {
	io.ReadCloser
	n      int64
	done   func(int64)
	closed bool
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error {
	if !c.closed {
		c.closed = true
		c.done(c.n)
	}
	return c.ReadCloser.Close()
}

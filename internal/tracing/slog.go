// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"
)

// LogHandler wraps h so every record logged with a context that carries a
// valid span gains trace_id and span_id (ARCHITECTURE §9: "trace_id on every
// line"). Log aggregation can then jump from a log line to its trace and back.
//
// Only the *Context logging methods (InfoContext, ErrorContext, ...) carry a
// context; a record logged without one is passed through untouched. Loggers
// built with WithTrace carry the ids as fixed attributes instead, which is what
// long-lived per-attempt loggers use.
func LogHandler(h slog.Handler) slog.Handler {
	if _, ok := h.(logHandler); ok {
		return h
	}
	return logHandler{h}
}

type logHandler struct{ slog.Handler }

func (l logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return l.Handler.Handle(ctx, r)
}

func (l logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return logHandler{l.Handler.WithAttrs(as)}
}

func (l logHandler) WithGroup(name string) slog.Handler {
	return logHandler{l.Handler.WithGroup(name)}
}

// WithTrace returns log with ctx's trace_id and span_id bound as attributes,
// or log itself when ctx carries no span.
func WithTrace(ctx context.Context, log *slog.Logger) *slog.Logger {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return log
	}
	return log.With("trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
}

// PgxTracer is the query tracer every pool is built with: one client span per
// query, batch, COPY, prepare and pool acquire, named after the statement's
// operation. Query PARAMETERS are never recorded (they carry tenant data and
// tokens); the statement text is, and in Nexus it is always a constant.
func PgxTracer() pgx.QueryTracer {
	return otelpgx.NewTracer()
}

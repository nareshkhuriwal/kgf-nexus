// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import "context"

// Attribution: the verified KGF context of a request, carried in its Go
// context so every audit writer -- the gateway's helpers and the store
// functions that append audit rows inside their own transactions -- records
// the same KGF organization, scope and principal (ADR-0024 §5, ADR-0025 §2).
//
// Only the gateway puts a context here, and only one this package's Verifier
// accepted: in NEXUS_AUTH_MODE kgf/both the context that AUTHENTICATED the
// request, and in standalone mode (with KGF's keys configured) a context that
// verified alongside a Nexus credential, which then only attributes and never
// authorizes. A token that did not verify is never attached, so a caller
// cannot put words in KGF's mouth.

type ctxKey struct{}
type reqKey struct{}

// With attaches a verified context.
func With(ctx context.Context, c Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// From returns the verified context, if any.
func From(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(Context)
	return c, ok
}

// WithRequestID attaches the request's correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqKey{}, id)
}

// RequestIDFrom returns the correlation id, or "".
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(reqKey{}).(string)
	return s
}

// SanitizeRequestID bounds a caller-supplied id: at most 128 printable ASCII
// characters, else "". It is recorded as a correlation id, never trusted.
func SanitizeRequestID(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import (
	"context"
	"strings"
	"testing"
)

func TestAttributionContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := From(ctx); ok || RequestIDFrom(ctx) != "" {
		t.Fatal("an empty context carries no attribution")
	}
	c := Context{Organization: "org-1", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "user:ana", Role: RoleOperator}
	ctx = WithRequestID(With(ctx, c), "req_1")
	got, ok := From(ctx)
	if !ok || got != c || RequestIDFrom(ctx) != "req_1" {
		t.Fatalf("round trip: %+v %v %q", got, ok, RequestIDFrom(ctx))
	}
}

func TestSanitizeRequestID(t *testing.T) {
	for in, want := range map[string]string{
		"req_abc-1:2.3":          "req_abc-1:2.3",
		"":                       "",
		"has space":              "",
		"tab\there":              "",
		"newline\n":              "",
		"café":                   "",
		strings.Repeat("a", 128): strings.Repeat("a", 128),
		strings.Repeat("a", 129): "",
	} {
		if got := SanitizeRequestID(in); got != want {
			t.Errorf("SanitizeRequestID(%q) = %q, want %q", in, got, want)
		}
	}
}

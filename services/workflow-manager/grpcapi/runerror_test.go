// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestNotFoundPathsAreNotFound: a missing row, and an id Postgres cannot even
// cast (22P02), are NotFound from the owner, which the gateway answers 404.
// Both used to be Internal (500) when they did not carry store.ErrNotFound.
func TestNotFoundPathsAreNotFound(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("resolve spec: %w", store.ErrNotFound),
		fmt.Errorf("get run: %w", &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "x"`}),
	} {
		if got := status.Code(runError("create run", err)); got != codes.NotFound {
			t.Errorf("runError(%v) = %s, want NotFound", err, got)
		}
	}
	if got := status.Code(runError("create run", errors.New("boom"))); got != codes.Internal {
		t.Errorf("an unclassified failure became %s, want Internal", got)
	}
}

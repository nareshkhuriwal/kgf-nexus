// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package store

import (
	"context"
	"github.com/google/uuid"
	"testing"
)

func TestIntegrationEmailLoginTenantResolution(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	a, b := uuid.NewString(), uuid.NewString()
	email := a + "@example.test"
	for _, ns := range []string{a, b} {
		if _, err = tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'test')`, ns); err != nil {
			t.Fatal(err)
		}
	}
	first, err := GatewayCreateUser(ctx, tx, a, email, "Owner", "admin", "test-password-long")
	if err != nil {
		t.Fatal(err)
	}
	u, err := GatewayAuthenticatePassword(ctx, tx, "", email, "test-password-long")
	if err != nil || u.ID != first.ID || u.Namespace != a {
		t.Fatalf("email resolution failed: %v", err)
	}
	if _, err = GatewayAuthenticatePassword(ctx, tx, "", email, "wrong-password"); err != ErrIdentityUnknown {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err = GatewayCreateUser(ctx, tx, b, email, "Other", "admin", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	if _, err = GatewayAuthenticatePassword(ctx, tx, "", email, "test-password-long"); err != ErrIdentityUnknown {
		t.Fatalf("ambiguous email accepted: %v", err)
	}
}

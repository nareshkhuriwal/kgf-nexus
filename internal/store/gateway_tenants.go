// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package store

import (
	"context"
	"time"
)

type TenantRow struct {
	ID           string    `json:"id"`
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	ContactEmail string    `json:"contact_email"`
	CreatedAt    time.Time `json:"created_at"`
}

func GatewayTenant(ctx context.Context, q GWQuerier, namespace string) (TenantRow, error) {
	var t TenantRow
	err := q.QueryRow(ctx, `SELECT id::text,namespace,name,contact_email,created_at FROM tenants WHERE namespace=$1`, namespace).Scan(&t.ID, &t.Namespace, &t.Name, &t.ContactEmail, &t.CreatedAt)
	return t, err
}
func GatewayPlatformOperator(ctx context.Context, q GWQuerier, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var allowed bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_operators p JOIN users u ON u.id=p.user_id WHERE p.user_id=$1 AND u.disabled_at IS NULL AND u.role='admin')`, userID).Scan(&allowed)
	return allowed, err
}

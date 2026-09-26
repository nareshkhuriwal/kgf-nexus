// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// KGF Cloud tenancy mapping (ADR-0024 §3): each KGF (organization,
// subscription, resource group) is one Nexus tenant namespace, provisioned on
// first use. Owned by the api-gateway.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrKGFNamespaceConflict means the derived namespace exists but was not
// provisioned for this KGF scope. It is refused, never adopted.
var ErrKGFNamespaceConflict = errors.New("kgf: namespace exists and belongs to another scope")

// KGFScope is one KGF workload scope.
type KGFScope struct {
	Organization  string `json:"organization"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resource_group"`
}

// normalized lower-cases every part. KGF (like ARM) treats subscription and
// resource-group names case-insensitively, so "Data-RG" and "data-rg" are one
// scope and must be one namespace.
func (s KGFScope) normalized() KGFScope {
	return KGFScope{
		Organization:  strings.ToLower(strings.TrimSpace(s.Organization)),
		Subscription:  strings.ToLower(strings.TrimSpace(s.Subscription)),
		ResourceGroup: strings.ToLower(strings.TrimSpace(s.ResourceGroup)),
	}
}

// Complete reports whether the scope names a workload scope.
func (s KGFScope) Complete() bool {
	n := s.normalized()
	return n.Organization != "" && n.Subscription != "" && n.ResourceGroup != "" &&
		len(n.Organization) <= 128 && len(n.Subscription) <= 128 && len(n.ResourceGroup) <= 128
}

// KGFNamespace derives the Nexus namespace for a KGF scope:
//
//	"kgf-" + hex(sha256("kgf-nexus/v1" 0x00 org 0x00 sub 0x00 rg))[:32]
//
// Deterministic, a valid Nexus identifier, fixed length, and the organization
// is inside the hash, so equal subscription and group names in two
// organizations never share a namespace. The "v1" is the mapping's version:
// changing the derivation must change it and migrate kgf_scopes, never
// silently remap tenants.
func KGFNamespace(s KGFScope) string {
	n := s.normalized()
	sum := sha256.Sum256([]byte("kgf-nexus/v1\x00" + n.Organization + "\x00" + n.Subscription + "\x00" + n.ResourceGroup))
	return "kgf-" + hex.EncodeToString(sum[:])[:32]
}

// GatewayProvisionKGFNamespace ensures the tenant and kgf_scopes rows for a
// scope exist and returns the namespace. Idempotent and safe under concurrent
// first requests: both inserts are ON CONFLICT DO NOTHING, and the stored
// triple is read back and compared, so a namespace is never claimed for two
// scopes.
func GatewayProvisionKGFNamespace(ctx context.Context, db GWDB, s KGFScope) (string, error) {
	if !s.Complete() {
		return "", errors.New("kgf: organization, subscription and resource group are required")
	}
	n := s.normalized()
	ns := KGFNamespace(n)
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin kgf provisioning: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	name := fmt.Sprintf("KGF %s / %s / %s", n.Organization, n.Subscription, n.ResourceGroup)
	if len(name) > 160 {
		name = name[:160]
	}
	var createdTenant bool
	err = tx.QueryRow(ctx, `INSERT INTO tenants (namespace, name) VALUES ($1, $2)
		ON CONFLICT (namespace) DO NOTHING RETURNING true`, ns, name).Scan(&createdTenant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("provision kgf tenant: %w", err)
	}
	// The scope row is written only by the transaction that created the
	// tenant. Both commit together, so a KGF-provisioned tenant always has
	// its row; a tenant that exists WITHOUT one was created some other way and
	// is refused below rather than adopted. A concurrent first request that
	// lost the tenant insert waited for the winner's commit (unique-index
	// conflict) and reads the winner's row.
	if createdTenant {
		if _, err := tx.Exec(ctx, `INSERT INTO kgf_scopes (namespace, organization, subscription, resource_group)
			VALUES ($1, $2, $3, $4)`, ns, n.Organization, n.Subscription, n.ResourceGroup); err != nil {
			return "", fmt.Errorf("provision kgf scope: %w", err)
		}
	}
	var got KGFScope
	err = tx.QueryRow(ctx, `SELECT organization, subscription, resource_group FROM kgf_scopes WHERE namespace=$1`, ns).
		Scan(&got.Organization, &got.Subscription, &got.ResourceGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		// A tenant with this name exists and was not provisioned from KGF.
		return "", ErrKGFNamespaceConflict
	}
	if err != nil {
		return "", fmt.Errorf("read kgf scope: %w", err)
	}
	if got != n {
		return "", ErrKGFNamespaceConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit kgf provisioning: %w", err)
	}
	return ns, nil
}

// GatewayKGFScope reads the KGF scope a namespace was provisioned for.
func GatewayKGFScope(ctx context.Context, q GWQuerier, namespace string) (KGFScope, error) {
	var s KGFScope
	err := q.QueryRow(ctx, `SELECT organization, subscription, resource_group FROM kgf_scopes WHERE namespace=$1`, namespace).
		Scan(&s.Organization, &s.Subscription, &s.ResourceGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

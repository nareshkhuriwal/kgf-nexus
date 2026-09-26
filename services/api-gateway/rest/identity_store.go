// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// StoreIdentityAdmin adapts the pgx store to the IdentityAdmin interface.
//
// A thin adapter rather than letting handlers call the store directly, for the
// same reason WorkflowManager and ResourceManagerAPI are interfaces: it keeps the
// HTTP layer testable without Postgres, and it makes the set of identity
// operations the REST surface can perform an explicit, reviewable list rather
// than "whatever the store exports".
type StoreIdentityAdmin struct {
	DB store.GWDB
}

// NewStoreIdentityAdmin builds the adapter over a gateway database handle.
func NewStoreIdentityAdmin(db store.GWDB) StoreIdentityAdmin {
	return StoreIdentityAdmin{DB: db}
}

func (s StoreIdentityAdmin) AuthenticatePassword(ctx context.Context, namespace, email, password string) (store.UserRow, error) {
	return store.GatewayAuthenticatePassword(ctx, s.DB, namespace, email, password)
}

func (s StoreIdentityAdmin) CreateSession(ctx context.Context, userID string, ttl time.Duration, userAgent, clientIP string) (string, store.SessionRow, error) {
	return store.GatewayCreateSession(ctx, s.DB, userID, ttl, userAgent, clientIP)
}

func (s StoreIdentityAdmin) RevokeSession(ctx context.Context, sessionID string) error {
	return store.GatewayRevokeSession(ctx, s.DB, sessionID)
}

func (s StoreIdentityAdmin) RevokeUserSessions(ctx context.Context, userID string) (int64, error) {
	return store.GatewayRevokeUserSessions(ctx, s.DB, userID)
}

func (s StoreIdentityAdmin) CreateUser(ctx context.Context, namespace, email, displayName, role, password string) (store.UserRow, error) {
	return store.GatewayCreateUser(ctx, s.DB, namespace, email, displayName, role, password)
}

func (s StoreIdentityAdmin) ListUsers(ctx context.Context, namespace string, limit int) ([]store.UserRow, error) {
	return store.GatewayListUsers(ctx, s.DB, namespace, limit)
}

func (s StoreIdentityAdmin) GetUser(ctx context.Context, namespace, userID string) (store.UserRow, error) {
	return store.GatewayGetUser(ctx, s.DB, namespace, userID)
}

// SetUserDisabled needs the GWDB rather than a bare querier: disabling a user and
// revoking its sessions must commit together, or a deactivated person keeps access
// until their token expires.
func (s StoreIdentityAdmin) SetUserDisabled(ctx context.Context, namespace, userID string, disabled bool) (store.UserRow, error) {
	return store.GatewaySetUserDisabled(ctx, s.DB, namespace, userID, disabled)
}

func (s StoreIdentityAdmin) SetUserPassword(ctx context.Context, userID, password string) error {
	return store.GatewaySetUserPassword(ctx, s.DB, userID, password)
}

func (s StoreIdentityAdmin) ListUserSessions(ctx context.Context, userID string, limit int) ([]store.SessionRow, error) {
	return store.GatewayListUserSessions(ctx, s.DB, userID, limit)
}

func (s StoreIdentityAdmin) CreateScopedToken(ctx context.Context, namespace, name, role string, scopes []string, expiresAt *time.Time, createdBy *string) (string, store.ScopedTokenRow, error) {
	return store.GatewayCreateScopedToken(ctx, s.DB, namespace, name, role, scopes, expiresAt, createdBy)
}

func (s StoreIdentityAdmin) ListScopedTokens(ctx context.Context, namespace string, limit int) ([]store.ScopedTokenRow, error) {
	return store.GatewayListScopedTokens(ctx, s.DB, namespace, limit)
}

func (s StoreIdentityAdmin) RevokeScopedToken(ctx context.Context, namespace, tokenID string) error {
	return store.GatewayRevokeScopedToken(ctx, s.DB, namespace, tokenID)
}

func (s StoreIdentityAdmin) AppendAudit(ctx context.Context, e store.AuditEntry) error {
	return store.GatewayAppendAudit(ctx, s.DB, e)
}

// ListUsersPage is the keyset-paginated listing (ADR-0024 §6).
func (s StoreIdentityAdmin) ListUsersPage(ctx context.Context, namespace string, limit int, cursor, search string) (store.UsersPage, error) {
	return store.GatewayListUsersPage(ctx, s.DB, namespace, limit, cursor, search)
}

// CreateScopedTokenWith mints a service-account or ceilinged token (ADR-0024 §6).
func (s StoreIdentityAdmin) CreateScopedTokenWith(ctx context.Context, spec store.TokenSpec) (string, store.ScopedTokenRow, error) {
	return store.GatewayCreateScopedTokenWith(ctx, s.DB, spec)
}

func (s StoreIdentityAdmin) ListAudit(ctx context.Context, namespace string, f store.AuditFilter) ([]store.AuditRow, string, error) {
	return store.GatewayListAudit(ctx, s.DB, namespace, f)
}

// WithinTx binds every identity operation fn performs to one transaction (a
// SAVEPOINT when s is itself transactional) and commits only on success.
func (s StoreIdentityAdmin) WithinTx(ctx context.Context, fn func(IdentityAdmin) error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(StoreIdentityAdmin{DB: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBadCursor means a pagination cursor was not one this server issued.
var ErrBadCursor = errors.New("invalid cursor")

// MaxUsersPage bounds one page of the users listing.
const MaxUsersPage = 200

// UsersPage is one keyset page of a namespace's accounts.
type UsersPage struct {
	Users []UserRow
	// NextCursor is empty on the last page.
	NextCursor string
}

// encodeUserCursor packs the last row's sort key. Opaque to clients: it is
// base64 so nobody builds one by hand and so the format can change.
func encodeUserCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeUserCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || len(id) != 36 {
		return time.Time{}, "", ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return t, id, nil
}

// escapeLike makes a user search string literal inside ILIKE.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// GatewayListUsersPage lists accounts newest first with a keyset cursor on
// (created_at, id), optionally filtered server-side by email or display name.
//
// Keyset rather than OFFSET: a page boundary stays stable while accounts are
// created, and page N costs the same as page 1. This replaces the 500-account
// ceiling the role-assignment picker used to hit (ADR-0024 §10).
func GatewayListUsersPage(ctx context.Context, q GWQuerier, namespace string, limit int, cursor, search string) (UsersPage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxUsersPage {
		limit = MaxUsersPage
	}
	var afterTS *time.Time
	var afterID *string
	if cursor != "" {
		t, id, err := decodeUserCursor(cursor)
		if err != nil {
			return UsersPage{}, err
		}
		afterTS, afterID = &t, &id
	}
	pattern := ""
	if s := strings.TrimSpace(search); s != "" {
		if len(s) > 200 {
			s = s[:200]
		}
		pattern = "%" + escapeLike(strings.ToLower(s)) + "%"
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, email_verified_at
		  FROM users
		 WHERE namespace = $1
		   AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
		   AND ($4 = '' OR lower(email) LIKE $4 OR lower(display_name) LIKE $4)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`, namespace, afterTS, afterID, pattern, limit+1)
	if err != nil {
		return UsersPage{}, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := make([]UserRow, 0, limit+1)
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt); err != nil {
			return UsersPage{}, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return UsersPage{}, fmt.Errorf("iterate users: %w", err)
	}
	page := UsersPage{Users: out}
	if len(out) > limit {
		page.Users = out[:limit]
		last := page.Users[limit-1]
		page.NextCursor = encodeUserCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

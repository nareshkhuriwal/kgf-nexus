// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"net/url"
)

// AuditExport opens the namespace's audit export (ADR-0025): NDJSON or CSV,
// streamed. The caller closes the body. query carries format, since, until,
// action, outcome and actor_id.
func (c *Client) AuditExport(ctx context.Context, query url.Values) (io.ReadCloser, error) {
	accept := "application/x-ndjson"
	if query.Get("format") == "csv" {
		accept = "text/csv"
	}
	return c.stream(ctx, c.urlFor("/audit/export", query), accept)
}

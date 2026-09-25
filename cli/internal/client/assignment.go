// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"
)

// GetAssignment fetches an attempt's resolved inputs and TaskSpec for replay
// (ADR-0022). runID "" or "-" finds the attempt in any run of the namespace.
// The response never contains the lease token or a secret value.
func (c *Client) GetAssignment(ctx context.Context, runID, attemptID string) (any, error) {
	if runID == "" {
		runID = "-"
	}
	path := "/runs/" + url.PathEscape(runID) + "/attempts/" + url.PathEscape(attemptID) + "/assignment"
	return c.do(ctx, http.MethodGet, c.urlFor(path, nil), nil, nil)
}

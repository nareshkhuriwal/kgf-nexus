// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package eventservice

import (
	"context"
	"net/http"
	"time"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

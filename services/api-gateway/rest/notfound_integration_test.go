//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

// Not-found paths must answer 404 with a problem body, never 500, against
// REAL Postgres: an id that names nothing, and an id that is not even a UUID
// (which Postgres rejects with 22P02 when it is cast), on every id-addressed
// route. Before the sweep a malformed id reached the client as "internal".

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func TestIntegrationUnknownAndMalformedIDsAre404(t *testing.T) {
	pool := attentionPool(t)
	f := seedTenant(t, pool, "nf", "RUNNING")
	api := New(pool, authn.New(pool), &idOnlyOwner{pool: pool}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithBackfills(fakeBackfills{})
	h := api.Routes()

	routes := []struct{ method, path string }{
		{http.MethodGet, "/runs/%s"},
		{http.MethodGet, "/runs/%s/events"},
		{http.MethodPost, "/runs/%s/cancel"},
		{http.MethodPost, "/runs/%s/pause"},
		{http.MethodPost, "/runs/%s/resume"},
		{http.MethodPost, "/runs/%s/retry"},
		{http.MethodGet, "/specdefs/%s"},
		{http.MethodGet, "/attempts/%s/logs"},
		{http.MethodGet, "/backfills/%s"},
		{http.MethodGet, "/workers/%s"},
		{http.MethodGet, "/workers/%s/quarantines"},
		{http.MethodPost, "/workers/%s/drain"},
		{http.MethodDelete, "/runs/%s"},
	}
	for _, rt := range routes {
		for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
			path := "/v1/namespaces/" + f.ns + strings.Replace(rt.path, "%s", id, 1)
			t.Run(rt.method+" "+path, func(t *testing.T) {
				req := httptest.NewRequest(rt.method, path, strings.NewReader("{}"))
				req.Header.Set("Authorization", "Bearer "+f.key)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status %d, want 404. body=%s", rec.Code, rec.Body)
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" || body["error"] == nil {
					t.Errorf("404 without a problem body: %s", rec.Body)
				}
			})
		}
	}
}

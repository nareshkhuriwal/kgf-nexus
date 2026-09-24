// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

type fakeInvalidator struct{ got []store.MemoInvalidation }

func (f *fakeInvalidator) InvalidateCache(_ context.Context, inv store.MemoInvalidation) (int64, time.Time, error) {
	f.got = append(f.got, inv)
	return int64(len(f.got)), time.Unix(1700000000, 0).UTC(), nil
}

func newCacheAPI(t *testing.T, inv CacheInvalidator) http.Handler {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if inv != nil {
		api = api.WithCacheInvalidator(inv)
	}
	return api.Routes()
}

// Quarantine takes capacity out of the pool, so like drain it needs admin; the
// history is a read.
func TestQuarantineRoutesAreAdminOnly(t *testing.T) {
	h, _, _ := newTestAPI(t)
	for _, path := range []string{
		"/v1/namespaces/team-ml/workers/w1/quarantine",
		"/v1/namespaces/team-ml/workers/w1/release",
	} {
		if rec := post(t, h, path, readerKey, "", `{"reason":"x"}`); rec.Code != http.StatusForbidden {
			t.Errorf("reader POST %s: %d, want 403", path, rec.Code)
		}
		if rec := post(t, h, path, "", "", `{"reason":"x"}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous POST %s: %d, want 401", path, rec.Code)
		}
	}
}

func TestQuarantineValidatesBeforeActing(t *testing.T) {
	h, _, _ := newTestAPI(t)
	// fakeRM does not implement WorkerQuarantiner: the route says so plainly.
	if rec := post(t, h, "/v1/namespaces/team-ml/workers/w1/quarantine", adminKey, "", `{"reason":"x"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("unwired quarantine: %d, want 501", rec.Code)
	}
}

type quarantineRM struct{ fakeRM }

func (quarantineRM) QuarantineWorker(context.Context, string, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (quarantineRM) ReleaseWorker(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

func newRestDBWithAdmin() *restDB {
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	return db
}

func TestQuarantineRejectsBadInput(t *testing.T) {
	db := newRestDBWithAdmin()
	h := New(db, authn.New(db), &fakeWFM{}, quarantineRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()
	cases := map[string]string{
		"missing reason":    `{}`,
		"blank reason":      `{"reason":"   "}`,
		"negative duration": `{"reason":"x","duration_ms":-1}`,
		"over 30 days":      `{"reason":"x","duration_ms":2592000001}`,
		"not json":          `{`,
	}
	for name, body := range cases {
		rec := post(t, h, "/v1/namespaces/team-ml/workers/w1/quarantine", adminKey, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestInvalidateCache(t *testing.T) {
	if rec := post(t, newCacheAPI(t, nil), "/v1/namespaces/team-ml/cache/invalidations", adminKey, "", `{"task_ref":"a"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("unwired: %d, want 501", rec.Code)
	}
	inv := &fakeInvalidator{}
	h := newCacheAPI(t, inv)
	if rec := post(t, h, "/v1/namespaces/team-ml/cache/invalidations", readerKey, "", `{"task_ref":"a"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("reader: %d, want 403", rec.Code)
	}
	for _, bad := range []string{`{}`, `{"task_version":2}`, `{"cache_key":"","reason":"x"}`} {
		if rec := post(t, h, "/v1/namespaces/team-ml/cache/invalidations", adminKey, "", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
	rec := post(t, h, "/v1/namespaces/team-ml/cache/invalidations", adminKey, "",
		`{"task_ref":"etl.clean","task_version":3,"reason":"bad upstream"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("invalidate: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["invalidation_id"] == nil || len(inv.got) != 1 {
		t.Fatalf("resp=%v calls=%d", out, len(inv.got))
	}
	got := inv.got[0]
	if got.Namespace != "team-ml" || got.TaskRef != "etl.clean" || got.TaskVersion == nil || *got.TaskVersion != 3 ||
		got.Actor == "" || got.Reason != "bad upstream" {
		t.Fatalf("forwarded %+v", got)
	}
}

// Every new route must carry a permission mapping; guard() denies unmapped ones,
// so a missing entry would make the route unusable rather than open.
func TestNewRoutesHavePermissions(t *testing.T) {
	for route, want := range map[string]string{
		"POST /v1/namespaces/{ns}/workers/{id}/quarantine": "workers.quarantine",
		"POST /v1/namespaces/{ns}/workers/{id}/release":    "workers.quarantine",
		"GET /v1/namespaces/{ns}/workers/{id}/quarantines": "workers.view",
		"POST /v1/namespaces/{ns}/cache/invalidations":     "workflows.cache.invalidate",
	} {
		perms, ok := routePermissions[route]
		if !ok || len(perms) != 1 || perms[0] != want {
			t.Errorf("%s -> %v, want [%s]", route, perms, want)
		}
		known := false
		for _, c := range authn.Catalog() {
			if c == want {
				known = true
			}
		}
		if !known {
			t.Errorf("%s is not in the permission catalog; Allows() would deny it to everyone", want)
		}
	}
}
